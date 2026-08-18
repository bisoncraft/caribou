// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	// transactionValidityPeriod is the protocol's
	// transaction_validity_period genesis parameter: a transaction
	// referencing block R can only be included while the chain head is
	// within this many blocks of R. It is 86400 on both mainnet and
	// testnet (per EXPERIMENTAL_genesis_config). A pending transaction the
	// network still does not know this far past its reference block can
	// never be included.
	transactionValidityPeriod = 86400

	// pendingExpiryMargin pads transactionValidityPeriod before a pending
	// transaction is declared dead, absorbing any boundary ambiguity in
	// the inclusion rule. At ~1-second blocks the margin costs about two
	// minutes.
	pendingExpiryMargin = 100

	// refHeightKey is the WalletTransaction.AdditionalData key holding the
	// height of the block a sent transaction referenced — the tip at send
	// time. The pending-transaction poller uses it to prove a
	// still-unknown transaction dead once the validity period has lapsed.
	refHeightKey = "refHeight"
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
	torProxy    string

	keyMtx    sync.RWMutex
	privKey   ed25519.PrivateKey // nil when locked
	pubKey    ed25519.PublicKey
	accountID string // hex-encoded public key (implicit account)

	// ctx is set in Connect and cancelled on shutdown. Interface methods
	// that take no context of their own use it so their RPC calls abort
	// with the wallet instead of running to the client timeout.
	ctx       context.Context
	ctxCancel context.CancelFunc

	// rpc is set on Connect. It is accessed atomically because the
	// trait-exposed fee-estimate methods can be reached (via Core) before
	// or concurrently with Connect. Read it through client().
	rpc  atomic.Pointer[rpcClient]
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

	// tokenContracts maps a registered NEP-141 token contract address on
	// this wallet's network to its asset ID, so the receive scan can
	// recognize an incoming token transfer's target contract.
	tokenContracts map[string]uint32
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

	tokenContracts := make(map[string]uint32, len(Tokens))
	for id, tok := range Tokens {
		if addr, ok := tok.NetContracts[network]; ok {
			tokenContracts[addr] = id
		}
	}

	return &NearWallet{
		log:            logger,
		net:            network,
		emit:           cfg.Emit,
		peersChange:    cfg.PeersChange,
		dataDir:        cfg.DataDir,
		settings:       cfg.Settings,
		torProxy:       cfg.TorProxy,
		pubKey:         pubKey,
		accountID:      accountID,
		txDB:           cfg.NetworkTxDB,
		tokenContracts: tokenContracts,
	}, nil
}

// errNotConnected is returned by client() when the wallet's RPC client has
// not been set (Connect not yet run).
var errNotConnected = errors.New("wallet not connected")

// client returns the wallet's RPC client, or errNotConnected if Connect has
// not run. It reads the pointer atomically, so it is safe to call
// concurrently with Connect.
func (w *NearWallet) client() (*rpcClient, error) {
	rpc := w.rpc.Load()
	if rpc == nil {
		return nil, errNotConnected
	}
	return rpc, nil
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

func (w *NearWallet) Connect(ctx context.Context) (_ *sync.WaitGroup, err error) {
	w.ctx, w.ctxCancel = context.WithCancel(ctx)
	defer func() {
		if err != nil {
			w.ctxCancel()
		}
	}()

	rpc, err := newRPCClient(w.rpcEndpoints(), w.torProxy, w.log)
	if err != nil {
		return nil, fmt.Errorf("error creating RPC client: %w", err)
	}
	w.rpc.Store(rpc)

	// The transaction database (w.txDB) is the master tx DB handle supplied by
	// Core via WalletConfig.NetworkTxDB; the wallet does not own its lifecycle.

	// Fetch initial block.
	block, err := rpc.getBlock(w.ctx, "final")
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
	rpc, err := w.client()
	if err != nil {
		return nil, err
	}
	info, err := rpc.viewAccount(w.ctx, w.accountID)
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
func (w *NearWallet) reserveNonce(ctx context.Context, rpc *rpcClient) (uint64, error) {
	akInfo, err := rpc.viewAccessKey(ctx, w.accountID, w.pubKeyBase58(), "final")
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

	rpc, err := w.client()
	if err != nil {
		return nil, err
	}

	result, hash, refHeight, err := w.sendActions(w.ctx, rpc, privKey, address, []action{transferAction(yoctoAmount)})

	var rejected bool
	var failureMsg string
	switch {
	case err == nil:
		if result.Status.Failure != nil {
			// The transaction executed but its actions failed: terminal, and
			// the nonce was consumed. Record it as confirmed-and-rejected so
			// the burnt gas is visible in history, then report the failure.
			rejected = true
			failureMsg = string(result.Status.Failure)
		}
	case errors.Is(err, errBroadcastAmbiguous):
		// The broadcast may or may not have been accepted. Record the
		// transaction as pending; the reserved nonce is not reused. The tip
		// poller resolves the final status. Returning an error here would
		// invite a retry that could double-send.
		w.log.Warnf("Transfer %s broadcast outcome unknown; recording as pending: %v", base58.Encode(hash[:]), err)
	default:
		return nil, fmt.Errorf("error broadcasting transaction: %w", err)
	}

	// Prefer the actual fee burnt; fall back to an estimate only when the
	// execution outcome is not yet known (ambiguous broadcast).
	var fees uint64
	if err == nil {
		fees = result.feeDrops()
	}
	if fees == 0 {
		fees = w.estimateFee()
	}

	value := YoctoToDrops(yoctoAmount)
	txID := base58.Encode(hash[:])
	recipient := address
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        txID,
		Amount:    value,
		Fees:      fees,
		Recipient: &recipient,
		Confirmed: rejected,
		Rejected:  rejected,
		// The reference height lets the pending poller prove the
		// transaction dead if the network never sees it.
		AdditionalData: map[string]string{refHeightKey: strconv.FormatUint(refHeight, 10)},
	}
	if rejected {
		wtx.BlockNumber, wtx.Timestamp = w.resolveBlock(w.ctx, rpc, result.TransactionOutcome.BlockHash)
	}

	if w.txDB != nil {
		if err := w.txDB.StoreTx(wtx); err != nil {
			w.log.Errorf("Error storing send tx: %v", err)
		}
	}

	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
	}

	if rejected {
		return nil, fmt.Errorf("transfer failed on-chain: %s", failureMsg)
	}
	return &coin{txHash: hash, value: value}, nil
}

// sendActions reserves a nonce, then signs and broadcasts a transaction
// with the given receiver and actions, retrying once with a refreshed nonce
// if the network rejects the first attempt for a stale nonce. Both the
// native transfer and token paths route through here so they share the
// retry behavior.
func (w *NearWallet) sendActions(ctx context.Context, rpc *rpcClient, privKey ed25519.PrivateKey, receiverID string, actions []action) (result *txResult, hash [32]byte, refHeight uint64, err error) {
	w.nonceMtx.Lock()
	defer w.nonceMtx.Unlock()

	nonce, err := w.reserveNonce(ctx, rpc)
	if err != nil {
		return nil, hash, 0, err
	}

	result, hash, refHeight, err = w.broadcastActions(ctx, rpc, privKey, receiverID, actions, nonce)
	if err != nil && isInvalidNonce(err) {
		// An authoritative InvalidNonce means this transaction was rejected
		// and did not execute, so a fresh-nonce retry is a new send rather
		// than a duplicate of a possibly-live transaction. (An ambiguous
		// broadcast is never an InvalidNonce error, so it cannot reach here.)
		// The nonce may have been consumed by our own lagging watermark or by
		// the same key in another wallet instance; refresh and retry once.
		akInfo, akErr := rpc.viewAccessKey(ctx, w.accountID, w.pubKeyBase58(), "optimistic")
		if akErr != nil {
			return nil, hash, 0, fmt.Errorf("nonce refresh failed after %v: %w", err, akErr)
		}
		if akInfo.Nonce >= nonce {
			nonce = akInfo.Nonce + 1
		} else {
			nonce++
		}
		w.nonce = nonce
		result, hash, refHeight, err = w.broadcastActions(ctx, rpc, privKey, receiverID, actions, nonce)
	}
	return result, hash, refHeight, err
}

// broadcastActions builds, signs, and submits a single transaction with the
// given nonce, waiting for optimistic execution. refHeight is the height of
// the block the transaction references, after which the transaction's
// validity period is measured.
func (w *NearWallet) broadcastActions(ctx context.Context, rpc *rpcClient, privKey ed25519.PrivateKey, receiverID string, actions []action, nonce uint64) (_ *txResult, hash [32]byte, refHeight uint64, _ error) {
	w.tipMtx.RLock()
	blockHash := w.tipHash
	refHeight = w.tip
	w.tipMtx.RUnlock()

	tx := &nearTransaction{
		signerID:   w.accountID,
		publicKey:  [32]byte(w.pubKey),
		nonce:      nonce,
		receiverID: receiverID,
		blockHash:  blockHash,
		actions:    actions,
	}

	txBytes := serializeTransaction(tx)
	hash = sha256.Sum256(txBytes)
	sig := ed25519.Sign(privKey, hash[:])
	signedTx := serializeSignedTransaction(txBytes, sig)

	result, err := rpc.sendTx(ctx, encodeSignedTx(signedTx))
	return result, hash, refHeight, err
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

// estimateFeeAtGas prices a gas budget at the current (cached) gas price,
// in drops rounded up, falling back to DefaultFee when no gas price has
// ever been obtained. An attached budget is an upper bound on the burn —
// unused gas is refunded — so this overstates; callers use it only as a
// placeholder until the actual burn is known.
func (w *NearWallet) estimateFeeAtGas(gas int64) uint64 {
	if gp := w.cachedGasPrice(); gp != nil {
		return YoctoToDropsCeil(new(big.Int).Mul(gp, big.NewInt(gas)))
	}
	return DefaultFee
}

// gasPriceTTL is how long a fetched gas price is considered fresh. NEAR's
// gas price moves slowly (at most ~1%/block under congestion), so a short
// TTL tracks it closely without an RPC round trip per estimate.
const gasPriceTTL = time.Minute

// cachedGasPrice returns the network gas price, refreshed at most once per
// gasPriceTTL. Returns the last known price if the RPC is unavailable, or
// nil if no price was ever fetched. The RPC call is made without holding
// gasMtx, so estimate callers never serialize behind an in-flight fetch,
// and the stamp is advanced on failure too so a persistent outage backs off
// for gasPriceTTL rather than re-sweeping every endpoint on every estimate.
func (w *NearWallet) cachedGasPrice() *big.Int {
	w.gasMtx.Lock()
	cached, stamp := w.gasPrice, w.gasPriceStamp
	w.gasMtx.Unlock()
	// Back off for gasPriceTTL after any attempt, success or failure, so a
	// persistent outage does not re-sweep every endpoint on every estimate;
	// callers get the last known price (nil → DefaultFee) meanwhile.
	if !stamp.IsZero() && time.Since(stamp) < gasPriceTTL {
		return cached
	}

	rpc, err := w.client()
	if err != nil {
		return cached
	}
	gp, err := rpc.gasPrice(w.ctx)

	w.gasMtx.Lock()
	defer w.gasMtx.Unlock()
	w.gasPriceStamp = time.Now()
	if err != nil {
		w.log.Warnf("Error fetching gas price: %v", err)
		return w.gasPrice // last known (may be nil)
	}
	w.gasPrice = gp
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
	rpc, err := w.client()
	if err != nil {
		return nil, err
	}
	info, err := rpc.viewAccount(w.ctx, w.accountID)
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
		// Mirror Withdraw, which subtracts the fee and caps the amount at the
		// spendable balance. Erroring when value-fee exceeds spendable would
		// reject exactly the max-balance withdrawal Withdraw fulfills.
		amt := DropsToYocto(value)
		amt.Sub(amt, w.estimateFeeYocto())
		if amt.Cmp(spendable) > 0 {
			amt.Set(spendable)
		}
		if amt.Sign() <= 0 {
			return 0, false, fmt.Errorf("value %d drops cannot cover the fee and storage reserve", value)
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
	rpc, err := w.client()
	if err != nil {
		return nil, err
	}
	result, err := rpc.txStatus(ctx, txID, w.accountID)
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
		wt.BlockNumber, wt.Timestamp = w.resolveBlock(ctx, rpc, result.TransactionOutcome.BlockHash)
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

// tipPoller polls for new blocks and checks pending transactions. The RPC
// client is stable for the poller's lifetime — a reconfigure spawns a fresh
// Connect and poller — so it is loaded once here.
func (w *NearWallet) tipPoller(ctx context.Context) {
	ticker := time.NewTicker(tipPollInterval)
	defer ticker.Stop()

	rpc := w.rpc.Load()
	if rpc == nil {
		return
	}

	for {
		select {
		case <-ticker.C:
			block, err := rpc.getBlock(ctx, "final")
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
				w.checkPendingTxs(ctx, rpc, block.Header.Height)
				w.scanIncomingTxs(ctx, rpc, block.Header.Height)
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
func (w *NearWallet) checkPendingTxs(ctx context.Context, rpc *rpcClient, tip uint64) {
	if w.txDB == nil {
		return
	}
	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		w.log.Errorf("Error getting pending txs: %v", err)
		return
	}
	for _, tx := range pending {
		result, err := rpc.txStatus(ctx, tx.ID, w.accountID)
		if err != nil {
			if isUnknownTransaction(err) {
				w.maybeExpirePendingTx(tx, tip)
			} else {
				w.log.Tracef("Pending tx %s not yet available: %v", tx.ID, err)
			}
			continue
		}
		blockHeight, blockTime := w.resolveBlock(ctx, rpc, result.TransactionOutcome.BlockHash)
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

// maybeExpirePendingTx marks a pending transaction rejected once it is
// provably dead: the network has no record of it (only a definitive
// UNKNOWN_TRANSACTION reaches here — transport failures prove nothing) and
// the chain has advanced past the validity period of the block the
// transaction referenced, so it can never be included. Without this, a
// broadcast that was recorded pending on an ambiguous outcome but never
// reached the chain would be reported pending forever, with no signal that
// it is safe to re-send.
func (w *NearWallet) maybeExpirePendingTx(tx *txdb.WalletTransaction, tip uint64) {
	refHeight, err := strconv.ParseUint(tx.AdditionalData[refHeightKey], 10, 64)
	if err != nil {
		return // no reference height recorded; death cannot be proven
	}
	if tip <= refHeight+transactionValidityPeriod+pendingExpiryMargin {
		return // still inside the inclusion window
	}
	w.log.Warnf("Pending transaction %s was never observed by the network and its validity "+
		"period (reference height %d) has lapsed at height %d; marking it rejected", tx.ID, refHeight, tip)
	tx.Confirmed = true
	tx.Rejected = true
	if err := w.txDB.StoreTx(tx); err != nil {
		w.log.Errorf("Error updating dead tx %s: %v", tx.ID, err)
		return
	}
	if w.emit != nil {
		w.emit.TransactionNote(tx, false)
	}
}

// resolveBlock looks up the height and timestamp (unix seconds) for a block
// hash. Falls back to the current tip if the lookup fails.
func (w *NearWallet) resolveBlock(ctx context.Context, rpc *rpcClient, blockHash string) (height, tsSec uint64) {
	if blockHash != "" {
		block, err := rpc.getBlockByHash(ctx, blockHash)
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

// scanIncomingTxs scans blocks since the last checkpoint (bounded to
// maxScanBlocks per call, oldest first) for incoming native and NEP-141
// token transfers. lastScannedHeight only ever advances to a height that
// was actually scanned: on an RPC error mid-scan, or when the backlog
// exceeds maxScanBlocks, the checkpoint stops short so the remaining
// blocks are picked up on a later call instead of being silently skipped.
func (w *NearWallet) scanIncomingTxs(ctx context.Context, rpc *rpcClient, newTip uint64) {
	if w.txDB == nil {
		return
	}

	startHeight := w.lastScannedHeight + 1
	if newTip < startHeight {
		return
	}

	endHeight := newTip
	if endHeight-startHeight >= maxScanBlocks {
		endHeight = startHeight + maxScanBlocks - 1
	}

	scanned := w.lastScannedHeight
	for height := startHeight; height <= endHeight; height++ {
		block, err := rpc.getBlockByHeight(ctx, height)
		if err != nil {
			w.log.Warnf("Error fetching block %d for receive scan: %v", height, err)
			break
		}
		blockTime := block.Header.Timestamp / 1_000_000_000
		for _, chunk := range block.Chunks {
			w.scanChunk(ctx, rpc, chunk.ChunkHash, height, blockTime)
		}
		scanned = height
	}

	w.lastScannedHeight = scanned
}

// scanChunk fetches a chunk and stores any incoming native or NEP-141
// token transfer transactions.
func (w *NearWallet) scanChunk(ctx context.Context, rpc *rpcClient, chunkHash string, blockHeight, blockTime uint64) {
	chunk, err := rpc.getChunk(ctx, chunkHash)
	if err != nil {
		w.log.Warnf("Error fetching chunk %s: %v", chunkHash, err)
		return
	}

	for i := range chunk.Transactions {
		ct := &chunk.Transactions[i]
		if ct.SignerID == w.accountID {
			continue // our own outgoing transaction
		}
		if ct.ReceiverID == w.accountID {
			w.scanNativeReceive(ct, blockHeight, blockTime)
			continue
		}
		if tokenID, ok := w.tokenContracts[ct.ReceiverID]; ok {
			w.scanTokenReceive(ctx, rpc, ct, tokenID, blockHeight, blockTime)
		}
	}
}

// scanNativeReceive stores an incoming native NEAR transfer transaction, if
// not already recorded.
func (w *NearWallet) scanNativeReceive(ct *chunkTransaction, blockHeight, blockTime uint64) {
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
		return
	}

	existing, err := w.txDB.GetTx(ct.Hash)
	if err != nil {
		w.log.Warnf("Error checking for existing tx %s: %v", ct.Hash, err)
		return
	}
	if existing != nil {
		return
	}

	sender := ct.SignerID
	recipient := w.accountID
	wtx := &txdb.WalletTransaction{
		Type:           txdb.Receive,
		ID:             ct.Hash,
		Amount:         amount,
		BlockNumber:    blockHeight,
		Timestamp:      blockTime,
		Confirmed:      true,
		Recipient:      &recipient,
		AdditionalData: map[string]string{"sender": sender},
	}

	if err := w.txDB.StoreTx(wtx); err != nil {
		w.log.Errorf("Error storing receive tx %s: %v", ct.Hash, err)
		return
	}

	w.log.Infof("Detected incoming transfer %s: %d drops from %s", ct.Hash, amount, sender)
	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
	}
}

// ftTransferArgs is the subset of ft_transfer/ft_transfer_call args needed
// to recognize an incoming NEP-141 transfer.
type ftTransferArgs struct {
	ReceiverID string `json:"receiver_id"`
	Amount     string `json:"amount"`
}

// scanTokenReceive checks a transaction addressed to a known NEP-141 token
// contract for an ft_transfer/ft_transfer_call naming this account as the
// receiver, storing a Receive record with TokenID set if found. Unlike a
// native Transfer action, a FunctionCall's receipt can fail independently
// of the transaction's inclusion in the chunk (e.g. the sender was never
// storage-registered, or its token balance was insufficient), so the
// outcome is confirmed with the RPC before crediting it.
func (w *NearWallet) scanTokenReceive(ctx context.Context, rpc *rpcClient, ct *chunkTransaction, tokenID uint32, blockHeight, blockTime uint64) {
	var amount uint64
	for _, a := range ct.Actions {
		if a.FunctionCall == nil {
			continue
		}
		if a.FunctionCall.MethodName != "ft_transfer" && a.FunctionCall.MethodName != "ft_transfer_call" {
			continue
		}
		argsJSON, err := base64.StdEncoding.DecodeString(a.FunctionCall.Args)
		if err != nil {
			continue
		}
		var args ftTransferArgs
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			continue
		}
		if args.ReceiverID != w.accountID {
			continue
		}
		amt, err := strconv.ParseUint(args.Amount, 10, 64)
		if err != nil {
			continue
		}
		amount += amt
	}
	if amount == 0 {
		return
	}

	existing, err := w.txDB.GetTx(ct.Hash)
	if err != nil {
		w.log.Warnf("Error checking for existing tx %s: %v", ct.Hash, err)
		return
	}
	if existing != nil {
		return
	}

	result, err := rpc.txStatus(ctx, ct.Hash, ct.SignerID)
	if err != nil {
		w.log.Warnf("Error checking status of token tx %s: %v", ct.Hash, err)
		return
	}
	if !result.Status.isSuccess() {
		return
	}

	sender := ct.SignerID
	recipient := w.accountID
	wtx := &txdb.WalletTransaction{
		Type:           txdb.Receive,
		ID:             ct.Hash,
		Amount:         amount,
		TokenID:        &tokenID,
		BlockNumber:    blockHeight,
		Timestamp:      blockTime,
		Confirmed:      true,
		Recipient:      &recipient,
		AdditionalData: map[string]string{"sender": sender},
	}

	if err := w.txDB.StoreTx(wtx); err != nil {
		w.log.Errorf("Error storing token receive tx %s: %v", ct.Hash, err)
		return
	}

	w.log.Infof("Detected incoming token transfer %s: %d atoms (token %d) from %s", ct.Hash, amount, tokenID, sender)
	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
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
	// errors.As, not a type assertion, so wrapping between the RPC layer
	// and callers cannot silently turn a missing account into a hard error.
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		return false
	}
	if rpcErr.Cause != nil && rpcErr.Cause.Name == "UNKNOWN_ACCOUNT" {
		return true
	}
	return strings.Contains(string(rpcErr.Data), "does not exist")
}

// isUnknownTransaction reports whether err is the node's definitive
// statement that it has no record of the transaction. Transport failures
// and timeouts do not qualify: they prove nothing about the transaction.
func isUnknownTransaction(err error) bool {
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return rpcErr.Cause != nil && rpcErr.Cause.Name == "UNKNOWN_TRANSACTION"
}
