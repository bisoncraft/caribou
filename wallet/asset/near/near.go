// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/keygen"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/keystore"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/decred/base58"
	"github.com/decred/dcrd/hdkeychain/v3"
)

const (
	version       = 0
	walletTypeRPC = "rpc"
	keyFileName   = "near-keyfile.json"

	// tipPollInterval is how often to check for new blocks.
	tipPollInterval = 5 * time.Second
)

var (
	rpcWalletDefinition = &asset.WalletDefinition{
		Seeded:      true,
		Type:        walletTypeRPC,
		Tab:         "Native",
		Description: "NEAR Protocol wallet",
		ConfigOpts: []*asset.ConfigOption{{
			Key:         "rpcprovider",
			DisplayName: "RPC Providers",
			Description: "Comma-separated list of NEAR RPC endpoint URLs. Leave empty to use the built-in providers.",
		}},
	}

	WalletInfo = asset.WalletInfo{
		Name:              "NEAR Protocol",
		SupportedVersions: []uint32{version},
		UnitInfo:          UnitInfo,
		AvailableWallets:  []*asset.WalletDefinition{rpcWalletDefinition},
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
			Priority:      true,
		},
	}

	seedDerivationPath = []uint32{
		hdkeychain.HardenedKeyStart + 44,  // purpose 44'
		hdkeychain.HardenedKeyStart + 397, // NEAR coin type 397'
		hdkeychain.HardenedKeyStart,       // account 0'
		hdkeychain.HardenedKeyStart,       // 0' (hardened for ed25519)
		hdkeychain.HardenedKeyStart,       // 0' (hardened for ed25519)
	}

	namedAccountRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(\.[a-z0-9][a-z0-9_-]*)*\.(near|testnet)$`)
)

func init() {
	asset.Register(BipID, &Driver{})
	registerToken(usdcTokenID, "Circle's USDC NEP-141 token on NEAR.", util.Mainnet, util.Testnet)
}

// Driver implements asset.Driver and asset.Creator.
type Driver struct{}

var _ asset.Driver = (*Driver)(nil)
var _ asset.Creator = (*Driver)(nil)

func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	return newWallet(cfg, logger, network)
}

func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	return decodeCoinID(coinID)
}

func (d *Driver) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

func (d *Driver) Exists(walletType, dataDir string, settings map[string]string, net util.Network) (bool, error) {
	if walletType != walletTypeRPC {
		return false, fmt.Errorf("unknown wallet type %q", walletType)
	}
	keyFile := filepath.Join(dataDir, keyFileName)
	_, err := os.Stat(keyFile)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (d *Driver) Create(params *asset.CreateWalletParams) error {
	if params.Type != walletTypeRPC {
		return fmt.Errorf("unknown wallet type %q", params.Type)
	}
	if len(params.Seed) == 0 {
		return fmt.Errorf("wallet seed cannot be empty")
	}
	privKey, zero, err := privKeyFromSeed(params.Seed)
	if err != nil {
		return err
	}
	defer zero()

	if err := os.MkdirAll(params.DataDir, 0700); err != nil {
		return fmt.Errorf("error creating data directory: %w", err)
	}

	seed := privKey.Seed()
	defer encode.ClearBytes(seed)
	pubKey := privKey.Public().(ed25519.PublicKey)
	return keystore.Save(filepath.Join(params.DataDir, keyFileName), seed, pubKey, params.Pass)
}

// NearWallet implements asset.Wallet and asset.Authenticator for the NEAR
// Protocol.
type NearWallet struct {
	log         util.Logger
	net         util.Network
	emit        *asset.WalletEmitter
	peersChange func(uint32, error)
	dataDir     string
	settings    map[string]string

	keyMtx    sync.RWMutex
	privKey   ed25519.PrivateKey // nil when locked
	pubKey    ed25519.PublicKey
	accountID string // hex-encoded public key (implicit account)

	rpc  *rpcClient
	txDB txdb.NetworkTxDB

	tipMtx  sync.RWMutex
	tipHash [32]byte
	tip     uint64
	tipTime time.Time

	nonceMtx sync.Mutex
	nonce    uint64

	// gasMtx guards the cached network gas price used for fee estimates.
	gasMtx        sync.Mutex
	gasPrice      *big.Int
	gasPriceStamp time.Time

	// lastScannedHeight is the last block height that was scanned for
	// incoming transactions.
	lastScannedHeight uint64
}

var _ asset.Wallet = (*NearWallet)(nil)
var _ asset.Authenticator = (*NearWallet)(nil)
var _ asset.Withdrawer = (*NearWallet)(nil)
var _ asset.TxFeeEstimator = (*NearWallet)(nil)

func newWallet(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (*NearWallet, error) {
	keyFile := filepath.Join(cfg.DataDir, keyFileName)
	pubKeyB, err := keystore.PubKey(keyFile)
	if err != nil {
		return nil, fmt.Errorf("error loading key file: %w", err)
	}

	pubKey := ed25519.PublicKey(pubKeyB)
	accountID := hex.EncodeToString(pubKey)

	return &NearWallet{
		log:         logger,
		net:         network,
		emit:        cfg.Emit,
		peersChange: cfg.PeersChange,
		dataDir:     cfg.DataDir,
		settings:    cfg.Settings,
		pubKey:      pubKey,
		accountID:   accountID,
		txDB:        cfg.NetworkTxDB,
	}, nil
}

// Unlock decrypts the private key using the wallet password.
func (w *NearWallet) Unlock(pw []byte) error {
	keyFile := filepath.Join(w.dataDir, keyFileName)
	seed, err := keystore.Open(keyFile, pw)
	if err != nil {
		return fmt.Errorf("error decrypting key: %w", err)
	}
	defer encode.ClearBytes(seed)

	privKey := ed25519.NewKeyFromSeed(seed)

	// Verify the decrypted key matches the stored public key.
	derivedPub := privKey.Public().(ed25519.PublicKey)
	if !derivedPub.Equal(w.pubKey) {
		encode.ClearBytes(privKey)
		return fmt.Errorf("decrypted key does not match stored public key")
	}

	w.keyMtx.Lock()
	w.privKey = privKey
	w.keyMtx.Unlock()
	return nil
}

// Lock zeros the private key.
func (w *NearWallet) Lock() error {
	w.keyMtx.Lock()
	defer w.keyMtx.Unlock()
	if w.privKey != nil {
		encode.ClearBytes(w.privKey)
		w.privKey = nil
	}
	return nil
}

// Locked returns true if the private key is not loaded.
func (w *NearWallet) Locked() bool {
	w.keyMtx.RLock()
	defer w.keyMtx.RUnlock()
	return w.privKey == nil
}

func (w *NearWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	rpc, err := newRPCClient(w.rpcEndpoints(), "", w.log)
	if err != nil {
		return nil, fmt.Errorf("error creating RPC client: %w", err)
	}
	w.rpc = rpc

	// The transaction database (w.txDB) is the master tx DB handle supplied by
	// Core via WalletConfig.NetworkTxDB; the wallet does not own its lifecycle.

	// Fetch initial block.
	block, err := w.rpc.getBlock("final")
	if err != nil {
		return nil, fmt.Errorf("error fetching initial block: %w", err)
	}
	w.tipMtx.Lock()
	w.tip = block.Header.Height
	w.tipTime = time.Unix(0, int64(block.Header.Timestamp))
	copy(w.tipHash[:], decodeBlockHash(block.Header.Hash))
	w.tipMtx.Unlock()

	w.lastScannedHeight = block.Header.Height

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.tipPoller(ctx)
	}()

	return &wg, nil
}

func (w *NearWallet) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

func (w *NearWallet) Balance() (*asset.Balance, error) {
	info, err := w.rpc.viewAccount(w.accountID)
	if err != nil {
		// Account may not exist yet (no funds received). Return zero balance.
		if isAccountNotFound(err) {
			return &asset.Balance{}, nil
		}
		return nil, fmt.Errorf("error querying account: %w", err)
	}

	available, ok := parseYoctoNEAR(info.Amount)
	if !ok {
		return nil, fmt.Errorf("error parsing account balance %q", info.Amount)
	}

	locked, ok := parseYoctoNEAR(info.Locked)
	if !ok {
		return nil, fmt.Errorf("error parsing locked balance %q", info.Locked)
	}

	return &asset.Balance{
		Available: YoctoToDrops(available),
		Locked:    YoctoToDrops(locked),
	}, nil
}

func (w *NearWallet) DepositAddress() (string, error) {
	return w.accountID, nil
}

func (w *NearWallet) OwnsDepositAddress(address string) (bool, error) {
	return strings.EqualFold(address, w.accountID), nil
}

// reserveNonce reserves the next access-key nonce for a transaction. Callers
// must hold nonceMtx. It advances past both the network's current view of
// the account's nonce and any nonce already reserved by an earlier call, so
// two sends issued back-to-back don't compute the same nonce while the
// first transaction has yet to be reflected in the final access-key view.
func (w *NearWallet) reserveNonce() (uint64, error) {
	akInfo, err := w.rpc.viewAccessKey(w.accountID, w.pubKeyBase58(), "final")
	if err != nil {
		return 0, fmt.Errorf("error querying access key: %w", err)
	}
	nonce := akInfo.Nonce + 1
	if w.nonce+1 > nonce {
		nonce = w.nonce + 1
	}
	w.nonce = nonce
	return nonce, nil
}

func (w *NearWallet) Send(address string, value, _ uint64) (asset.Coin, error) {
	if !w.ValidateAddress(address) {
		return nil, fmt.Errorf("invalid NEAR address %q", address)
	}
	if value == 0 {
		return nil, fmt.Errorf("cannot send zero value")
	}
	return w.sendYocto(address, DropsToYocto(value))
}

// sendYocto signs and broadcasts a transfer, retrying once with a refreshed
// nonce if the network rejects the first attempt for a stale nonce. On-chain
// execution failures are returned as errors; only transfers that executed
// (or may yet execute, when the broadcast wait times out) are recorded.
func (w *NearWallet) sendYocto(address string, yoctoAmount *big.Int) (asset.Coin, error) {
	w.keyMtx.RLock()
	if w.privKey == nil {
		w.keyMtx.RUnlock()
		return nil, fmt.Errorf("wallet is locked")
	}
	privKey := w.privKey
	w.keyMtx.RUnlock()

	w.nonceMtx.Lock()
	defer w.nonceMtx.Unlock()

	akInfo, err := w.rpc.viewAccessKey(w.accountID, w.pubKeyBase58(), "final")
	if err != nil {
		return nil, err
	}

	// The final-view nonce lags transactions executed in the last block or
	// two, so a local watermark protects rapid sequential sends.
	nonce := akInfo.Nonce + 1
	if w.nonce >= nonce {
		nonce = w.nonce + 1
	}

	result, hash, err := w.broadcastTransfer(privKey, address, yoctoAmount, nonce)
	if err != nil && isInvalidNonce(err) {
		// Lost a nonce race (e.g. the same key in use by another wallet
		// instance). Refresh from the optimistic view and retry once.
		akInfo, akErr := w.rpc.viewAccessKey(w.accountID, w.pubKeyBase58(), "optimistic")
		if akErr != nil {
			return nil, fmt.Errorf("nonce refresh failed after %v: %w", err, akErr)
		}
		if akInfo.Nonce >= nonce {
			nonce = akInfo.Nonce + 1
		} else {
			nonce++
		}
		result, hash, err = w.broadcastTransfer(privKey, address, yoctoAmount, nonce)
	}

	fees := w.estimateFee()

	switch {
	case err == nil:
		if result.Status.Failure != nil {
			// The transaction executed but its actions failed. The nonce was
			// consumed, so advance the watermark before reporting.
			w.nonce = nonce
			return nil, fmt.Errorf("transfer failed on-chain: %s", string(result.Status.Failure))
		}
		if f := result.feeDrops(); f > 0 {
			fees = f
		}
	case isTimeoutError(err):
		// The broadcast was accepted but execution didn't complete within
		// the RPC wait window. Record the transaction as pending; the tip
		// poller resolves its final status.
		w.log.Warnf("Transfer %s execution outcome not yet known: %v", base58.Encode(hash[:]), err)
	default:
		return nil, fmt.Errorf("error broadcasting transaction: %w", err)
	}

	w.nonce = nonce

	value := YoctoToDrops(yoctoAmount)
	txID := base58.Encode(hash[:])
	recipient := address
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        txID,
		Amount:    value,
		Fees:      fees,
		Recipient: &recipient,
	}

	if w.txDB != nil {
		if err := w.txDB.StoreTx(wtx); err != nil {
			w.log.Errorf("Error storing send tx: %v", err)
		}
	}

	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
	}

	return &coin{txHash: hash, value: value}, nil
}

// broadcastTransfer builds, signs, and submits a single transfer transaction
// with the given nonce, waiting for optimistic execution.
func (w *NearWallet) broadcastTransfer(privKey ed25519.PrivateKey, address string, yoctoAmount *big.Int, nonce uint64) (*txResult, [32]byte, error) {
	w.tipMtx.RLock()
	blockHash := w.tipHash
	w.tipMtx.RUnlock()

	tx := &nearTransaction{
		signerID:   w.accountID,
		publicKey:  [32]byte(w.pubKey),
		nonce:      nonce,
		receiverID: address,
		blockHash:  blockHash,
		actions:    []action{transferAction(yoctoAmount)},
	}

	txBytes := serializeTransaction(tx)
	hash := sha256.Sum256(txBytes)
	sig := ed25519.Sign(privKey, hash[:])
	signedTx := serializeSignedTransaction(txBytes, sig)

	result, err := w.rpc.sendTx(encodeSignedTx(signedTx))
	return result, hash, err
}

func (w *NearWallet) ValidateAddress(address string) bool {
	// Implicit account: 64-char hex string (ed25519 pubkey).
	if len(address) == 64 {
		_, err := hex.DecodeString(address)
		return err == nil
	}
	// Named account.
	return namedAccountRE.MatchString(address)
}

func (w *NearWallet) SyncStatus() (*asset.SyncStatus, error) {
	w.tipMtx.RLock()
	tip := w.tip
	tipTime := w.tipTime
	w.tipMtx.RUnlock()

	synced := time.Since(tipTime) < time.Duration(MaxBlockInterval)*time.Second

	return &asset.SyncStatus{
		Synced: synced,
		Blocks: tip,
	}, nil
}

// StandardSendFee returns the estimated fee in drops for a simple transfer,
// derived from the current network gas price. The feeRate argument is
// ignored; NEAR fees are protocol-priced.
func (w *NearWallet) StandardSendFee(_ uint64) uint64 {
	return w.estimateFee()
}

// estimateFee is estimateFeeYocto in drops, rounded up.
func (w *NearWallet) estimateFee() uint64 {
	return YoctoToDropsCeil(w.estimateFeeYocto())
}

// estimateFeeYocto returns the estimated cost of a simple transfer at the
// current (cached) gas price, falling back to DefaultFee when no gas price
// has ever been obtained.
func (w *NearWallet) estimateFeeYocto() *big.Int {
	if gp := w.cachedGasPrice(); gp != nil {
		return new(big.Int).Mul(gp, big.NewInt(transferGas))
	}
	return DropsToYocto(DefaultFee)
}

// gasPriceTTL is how long a fetched gas price is considered fresh. NEAR's
// gas price moves slowly (at most ~1%/block under congestion), so a short
// TTL tracks it closely without an RPC round trip per estimate.
const gasPriceTTL = time.Minute

// cachedGasPrice returns the network gas price, refreshed at most once per
// gasPriceTTL. Returns the last known price if the RPC is unavailable, or
// nil if no price was ever fetched.
func (w *NearWallet) cachedGasPrice() *big.Int {
	w.gasMtx.Lock()
	defer w.gasMtx.Unlock()
	if w.gasPrice != nil && time.Since(w.gasPriceStamp) < gasPriceTTL {
		return w.gasPrice
	}
	gp, err := w.rpc.gasPrice()
	if err != nil {
		w.log.Warnf("Error fetching gas price: %v", err)
		return w.gasPrice
	}
	w.gasPrice = gp
	w.gasPriceStamp = time.Now()
	return gp
}

// withdrawFeeMargin scales the fee reserved when computing a maximum
// spendable amount. Gas prices can rise between estimation and the blocks
// where the transfer's receipts execute; the unspent margin simply remains
// in the account.
const withdrawFeeMargin = 2

// spendableYocto returns the balance available to a transfer's deposit after
// reserving the protocol storage stake for the account's current size and a
// fee allowance with margin.
func (w *NearWallet) spendableYocto() (*big.Int, error) {
	info, err := w.rpc.viewAccount(w.accountID)
	if err != nil {
		if isAccountNotFound(err) {
			return new(big.Int), nil
		}
		return nil, fmt.Errorf("error querying account: %w", err)
	}
	bal, ok := parseYoctoNEAR(info.Amount)
	if !ok {
		return nil, fmt.Errorf("error parsing account balance %q", info.Amount)
	}
	reserve := new(big.Int).Mul(new(big.Int).SetUint64(info.StorageUsed), storageAmountPerByte)
	fee := new(big.Int).Mul(w.estimateFeeYocto(), big.NewInt(withdrawFeeMargin))
	spendable := bal.Sub(bal, reserve)
	spendable.Sub(spendable, fee)
	if spendable.Sign() < 0 {
		spendable.SetUint64(0)
	}
	return spendable, nil
}

// Withdraw sends funds to address with the fee subtracted from value. The
// amount is additionally capped so the account retains its storage stake:
// requesting the full balance withdraws the maximum the protocol allows.
func (w *NearWallet) Withdraw(address string, value, _ uint64) (asset.Coin, error) {
	if !w.ValidateAddress(address) {
		return nil, fmt.Errorf("invalid NEAR address %q", address)
	}
	if value == 0 {
		return nil, fmt.Errorf("cannot withdraw zero value")
	}
	spendable, err := w.spendableYocto()
	if err != nil {
		return nil, err
	}
	amt := DropsToYocto(value)
	amt.Sub(amt, w.estimateFeeYocto())
	if amt.Cmp(spendable) > 0 {
		amt.Set(spendable)
	}
	if amt.Sign() <= 0 {
		return nil, fmt.Errorf("withdraw value %d drops cannot cover the fee and storage reserve", value)
	}
	return w.sendYocto(address, amt)
}

// EstimateSendTxFee returns the estimated fee for a simple transfer,
// verifying that the balance covers the send. The provided feeRate is
// ignored; NEAR fees are protocol-priced.
func (w *NearWallet) EstimateSendTxFee(address string, value, _ uint64, subtract, maxWithdraw bool) (fee uint64, isValidAddress bool, err error) {
	fee = w.estimateFee()
	if value == 0 && !maxWithdraw {
		return 0, false, fmt.Errorf("cannot send zero value")
	}
	spendable, err := w.spendableYocto()
	if err != nil {
		return 0, false, err
	}
	switch {
	case maxWithdraw:
		if spendable.Sign() <= 0 {
			return 0, false, fmt.Errorf("balance cannot cover the fee and storage reserve")
		}
	case subtract:
		amt := DropsToYocto(value)
		amt.Sub(amt, w.estimateFeeYocto())
		if amt.Sign() <= 0 || amt.Cmp(spendable) > 0 {
			return 0, false, fmt.Errorf("insufficient balance to send %d drops fee-inclusive", value)
		}
	default:
		if DropsToYocto(value).Cmp(spendable) > 0 {
			return 0, false, fmt.Errorf("insufficient balance to send %d drops plus %d fee", value, fee)
		}
	}
	return fee, w.ValidateAddress(address), nil
}

// FeeRate satisfies asset.Wallet, returning the standard per-transaction fee
// in the wallet's atomic unit.
func (w *NearWallet) FeeRate() uint64 {
	return w.StandardSendFee(0)
}

func (w *NearWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if w.txDB == nil || req == nil {
		return &txdb.TxHistoryResponse{}, nil
	}
	// nil selects this network's native-asset records, excluding any token
	// wallet's transactions sharing this NetworkTxDB.
	return w.txDB.GetAssetTxs(nil, req)
}

func (w *NearWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	// Check the local DB first.
	if w.txDB != nil {
		wt, err := w.txDB.GetTx(txID)
		if err != nil {
			return nil, err
		}
		if wt != nil {
			return wt, nil
		}
	}

	// Try querying the RPC.
	result, err := w.rpc.txStatus(txID, w.accountID)
	if err != nil {
		return nil, asset.CoinNotFoundError
	}

	txType := txdb.Send
	if result.Transaction.ReceiverID == w.accountID && result.Transaction.SignerID != w.accountID {
		txType = txdb.Receive
	}

	fees := result.feeDrops()
	if fees == 0 {
		fees = w.estimateFee()
	}
	wt := &txdb.WalletTransaction{
		Type:      txType,
		ID:        txID,
		Amount:    result.transferAmount(),
		Fees:      fees,
		Confirmed: result.Status.isSuccess(),
	}

	if txType == txdb.Send {
		recipient := result.Transaction.ReceiverID
		wt.Recipient = &recipient
	}

	if result.Status.isSuccess() {
		wt.BlockNumber, wt.Timestamp = w.resolveBlock(result.TransactionOutcome.BlockHash)
	}

	return wt, nil
}

func (w *NearWallet) PendingTransactions(_ context.Context) []*txdb.WalletTransaction {
	if w.txDB == nil {
		return nil
	}
	pending, err := w.txDB.GetAssetPendingTxs(nil)
	if err != nil {
		w.log.Errorf("Error getting pending txs: %v", err)
		return nil
	}
	return pending
}

// tipPoller polls for new blocks and checks pending transactions.
func (w *NearWallet) tipPoller(ctx context.Context) {
	ticker := time.NewTicker(tipPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			block, err := w.rpc.getBlock("final")
			if err != nil {
				w.log.Errorf("Error fetching block: %v", err)
				continue
			}

			w.tipMtx.Lock()
			newTip := block.Header.Height > w.tip
			if newTip {
				w.tip = block.Header.Height
				w.tipTime = time.Unix(0, int64(block.Header.Timestamp))
				copy(w.tipHash[:], decodeBlockHash(block.Header.Hash))
			}
			w.tipMtx.Unlock()

			if newTip {
				if w.emit != nil {
					w.emit.TipChange(block.Header.Height)
				}
				w.checkPendingTxs()
				w.scanIncomingTxs(block.Header.Height)
			}

		case <-ctx.Done():
			return
		}
	}
}

// checkPendingTxs queries the RPC for pending transactions and updates the
// database when they confirm or fail. Deliberately unfiltered (every asset
// sharing this NetworkTxDB, not just the native one): TokenWallet has no
// poller of its own, so this is the only reconciliation loop for token
// pending txs too. That's safe here (unlike SOL) because this loop never
// re-derives a tx's type/amount from the wallet's own address — it only
// updates BlockNumber/Timestamp/Confirmed/Rejected on the existing record
// looked up by the tx's own ID.
func (w *NearWallet) checkPendingTxs() {
	if w.txDB == nil {
		return
	}
	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		w.log.Errorf("Error getting pending txs: %v", err)
		return
	}
	for _, tx := range pending {
		result, err := w.rpc.txStatus(tx.ID, w.accountID)
		if err != nil {
			w.log.Tracef("Pending tx %s not yet available: %v", tx.ID, err)
			continue
		}
		blockHeight, blockTime := w.resolveBlock(result.TransactionOutcome.BlockHash)
		if result.Status.isSuccess() {
			tx.BlockNumber = blockHeight
			tx.Timestamp = blockTime
			tx.Confirmed = true
			if fees := result.feeDrops(); fees > 0 {
				tx.Fees = fees
			}
			if err := w.txDB.StoreTx(tx); err != nil {
				w.log.Errorf("Error updating confirmed tx %s: %v", tx.ID, err)
				continue
			}
			w.log.Infof("Transaction %s confirmed at block %d", tx.ID, blockHeight)
			if w.emit != nil {
				w.emit.TransactionNote(tx, false)
			}
		} else if result.Status.Failure != nil {
			w.log.Warnf("Transaction %s failed: %s", tx.ID, string(result.Status.Failure))
			// A failed tx is terminal: it is finalized (Confirmed) and did not
			// have its intended effect (Rejected), matching the convention used
			// by the other wallets. Marking it Confirmed removes it from the
			// pending set so it is no longer monitored.
			tx.BlockNumber = blockHeight
			tx.Timestamp = blockTime
			tx.Confirmed = true
			tx.Rejected = true
			if err := w.txDB.StoreTx(tx); err != nil {
				w.log.Errorf("Error updating failed tx %s: %v", tx.ID, err)
			}
		}
	}
}

// resolveBlock looks up the height and timestamp (unix seconds) for a block
// hash. Falls back to the current tip if the lookup fails.
func (w *NearWallet) resolveBlock(blockHash string) (height, tsSec uint64) {
	if blockHash != "" {
		block, err := w.rpc.getBlockByHash(blockHash)
		if err == nil {
			return block.Header.Height, block.Header.Timestamp / 1_000_000_000
		}
		w.log.Warnf("Error resolving block hash %s: %v", blockHash, err)
	}
	w.tipMtx.RLock()
	defer w.tipMtx.RUnlock()
	return w.tip, uint64(w.tipTime.Unix())
}

// maxScanBlocks limits how many blocks we scan in a single poll cycle to
// avoid excessive RPC calls after a long offline period.
const maxScanBlocks = 10

// scanIncomingTxs checks whether our account balance increased since the last
// scan. Only when an increase is detected does it scan the intervening blocks
// for the actual incoming transfer transactions.
func (w *NearWallet) scanIncomingTxs(newTip uint64) {
	if w.txDB == nil {
		return
	}

	startHeight := w.lastScannedHeight + 1
	if newTip < startHeight {
		return
	}

	// Compare account balance at the two block heights. If the balance
	// didn't increase we can skip the expensive block/chunk scanning.
	prevAcct, err := w.rpc.viewAccountAt(w.accountID, w.lastScannedHeight)
	if err != nil {
		// Account may not exist yet at the earlier height.
		if !isAccountNotFound(err) {
			w.log.Warnf("Error querying account at block %d: %v", w.lastScannedHeight, err)
		}
		w.lastScannedHeight = newTip
		return
	}
	curAcct, err := w.rpc.viewAccountAt(w.accountID, newTip)
	if err != nil {
		w.log.Warnf("Error querying account at block %d: %v", newTip, err)
		return
	}

	prevBal, _ := parseYoctoNEAR(prevAcct.Amount)
	curBal, _ := parseYoctoNEAR(curAcct.Amount)
	if prevBal == nil || curBal == nil || curBal.Cmp(prevBal) <= 0 {
		w.lastScannedHeight = newTip
		return
	}

	// Balance increased — scan blocks to find the incoming transfer(s).
	if newTip-startHeight >= maxScanBlocks {
		startHeight = newTip - maxScanBlocks + 1
	}

	for height := startHeight; height <= newTip; height++ {
		block, err := w.rpc.getBlockByHeight(height)
		if err != nil {
			w.log.Warnf("Error fetching block %d for receive scan: %v", height, err)
			break
		}
		blockTime := block.Header.Timestamp / 1_000_000_000
		for _, chunk := range block.Chunks {
			w.scanChunk(chunk.ChunkHash, height, blockTime)
		}
	}

	w.lastScannedHeight = newTip
}

// scanChunk fetches a chunk and stores any incoming transfer transactions.
func (w *NearWallet) scanChunk(chunkHash string, blockHeight, blockTime uint64) {
	chunk, err := w.rpc.getChunk(chunkHash)
	if err != nil {
		w.log.Warnf("Error fetching chunk %s: %v", chunkHash, err)
		return
	}

	for i := range chunk.Transactions {
		ct := &chunk.Transactions[i]
		if ct.ReceiverID != w.accountID || ct.SignerID == w.accountID {
			continue
		}
		var amount uint64
		for _, a := range ct.Actions {
			if a.Transfer != nil {
				yocto, ok := parseYoctoNEAR(a.Transfer.Deposit)
				if ok {
					amount += YoctoToDrops(yocto)
				}
			}
		}
		if amount == 0 {
			continue
		}

		// Check if we already have this tx.
		existing, err := w.txDB.GetTx(ct.Hash)
		if err != nil {
			w.log.Warnf("Error checking for existing tx %s: %v", ct.Hash, err)
			continue
		}
		if existing != nil {
			continue
		}

		sender := ct.SignerID
		wtx := &txdb.WalletTransaction{
			Type:        txdb.Receive,
			ID:          ct.Hash,
			Amount:      amount,
			BlockNumber: blockHeight,
			Timestamp:   blockTime,
			Confirmed:   true,
			Recipient:   &sender,
		}

		if err := w.txDB.StoreTx(wtx); err != nil {
			w.log.Errorf("Error storing receive tx %s: %v", ct.Hash, err)
			continue
		}

		w.log.Infof("Detected incoming transfer %s: %d drops from %s", ct.Hash, amount, ct.SignerID)
		if w.emit != nil {
			w.emit.TransactionNote(wtx, true)
		}
	}
}

// rpcEndpoints returns the RPC endpoints to use: the comma-separated
// "rpcprovider" setting when present, else the network's default provider
// ranking.
func (w *NearWallet) rpcEndpoints() []string {
	if setting, ok := w.settings["rpcprovider"]; ok {
		var eps []string
		for _, u := range strings.Split(setting, ",") {
			if u = strings.TrimSpace(u); u != "" {
				eps = append(eps, u)
			}
		}
		if len(eps) > 0 {
			return eps
		}
	}
	if eps, ok := DefaultRPCEndpoints[w.net]; ok {
		return eps
	}
	return DefaultRPCEndpoints[util.Mainnet]
}

// pubKeyBase58 returns the NEAR-formatted public key string "ed25519:<base58>".
func (w *NearWallet) pubKeyBase58() string {
	return "ed25519:" + base58.Encode(w.pubKey)
}

// privKeyFromSeed derives an ed25519 private key from the wallet seed.
func privKeyFromSeed(seed []byte) (ed25519.PrivateKey, func(), error) {
	extKey, err := keygen.GenDeepChild(seed, seedDerivationPath)
	if err != nil {
		return nil, nil, fmt.Errorf("error deriving key: %w", err)
	}

	privKeyBytes, err := extKey.SerializedPrivKey()
	if err != nil {
		extKey.Zero()
		return nil, nil, fmt.Errorf("error serializing private key: %w", err)
	}

	privKey := ed25519.NewKeyFromSeed(privKeyBytes)

	zero := func() {
		extKey.Zero()
		encode.ClearBytes(privKeyBytes)
	}

	return privKey, zero, nil
}

// decodeBlockHash decodes a base58-encoded NEAR block hash.
func decodeBlockHash(s string) []byte {
	b := base58.Decode(s)
	if len(b) < 32 {
		return make([]byte, 32)
	}
	return b[:32]
}

// isAccountNotFound checks if an RPC error indicates a missing account.
func isAccountNotFound(err error) bool {
	if err == nil {
		return false
	}
	rpcErr, ok := err.(*rpcError)
	if !ok {
		return false
	}
	if rpcErr.Cause != nil && rpcErr.Cause.Name == "UNKNOWN_ACCOUNT" {
		return true
	}
	return strings.Contains(string(rpcErr.Data), "does not exist")
}
