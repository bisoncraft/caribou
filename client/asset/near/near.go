// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/keygen"
	dexnear "decred.org/dcrdex/dex/networks/near"
	"github.com/decred/base58"
	"github.com/decred/dcrd/hdkeychain/v3"
)

const (
	version       = 0
	BipID         = dexnear.BipID
	walletTypeRPC = "rpc"
	keyFileName   = "near-keyfile.json"

	// tipPollInterval is how often to check for new blocks.
	tipPollInterval = 5 * time.Second
)

var (
	WalletInfo = asset.WalletInfo{
		Name:              "NEAR Protocol",
		SupportedVersions: []uint32{version},
		UnitInfo:          dexnear.UnitInfo,
		AvailableWallets: []*asset.WalletDefinition{{
			Seeded:      true,
			Type:        walletTypeRPC,
			Tab:         "Native",
			Description: "NEAR Protocol wallet",
			ConfigOpts: []*asset.ConfigOption{{
				Key:         "rpcprovider",
				DisplayName: "RPC Provider",
				Description: "NEAR RPC endpoint URL",
			}},
		}},
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
}

// Driver implements asset.Driver and asset.Creator.
type Driver struct{}

var _ asset.Driver = (*Driver)(nil)
var _ asset.Creator = (*Driver)(nil)

func (d *Driver) Open(cfg *asset.WalletConfig, logger dex.Logger, network dex.Network) (asset.Wallet, error) {
	return newWallet(cfg, logger, network)
}

func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	return decodeCoinID(coinID)
}

func (d *Driver) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

func (d *Driver) Exists(walletType, dataDir string, settings map[string]string, net dex.Network) (bool, error) {
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

	return saveKeyFile(filepath.Join(params.DataDir, keyFileName), privKey, params.Pass)
}

// NearWallet implements asset.Wallet and asset.Authenticator for the NEAR
// Protocol.
type NearWallet struct {
	log         dex.Logger
	net         dex.Network
	emit        *asset.WalletEmitter
	peersChange func(uint32, error)
	dataDir     string
	settings    map[string]string

	keyMtx    sync.RWMutex
	privKey   ed25519.PrivateKey // nil when locked
	pubKey    ed25519.PublicKey
	accountID string // hex-encoded public key (implicit account)

	rpc *rpcClient

	tipMtx  sync.RWMutex
	tipHash [32]byte
	tip     uint64
	tipTime time.Time

	pendingTxsMtx sync.RWMutex
	pendingTxs    map[string]*asset.WalletTransaction

	nonceMtx sync.Mutex
	nonce    uint64
}

var _ asset.Wallet = (*NearWallet)(nil)
var _ asset.Authenticator = (*NearWallet)(nil)

func newWallet(cfg *asset.WalletConfig, logger dex.Logger, network dex.Network) (*NearWallet, error) {
	keyFile := filepath.Join(cfg.DataDir, keyFileName)
	kf, err := readKeyFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("error loading key file: %w", err)
	}

	pubKey := ed25519.PublicKey(kf.PubKey)
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
		pendingTxs:  make(map[string]*asset.WalletTransaction),
	}, nil
}

// Unlock decrypts the private key using the wallet password.
func (w *NearWallet) Unlock(pw []byte) error {
	keyFile := filepath.Join(w.dataDir, keyFileName)
	kf, err := readKeyFile(keyFile)
	if err != nil {
		return fmt.Errorf("error reading key file: %w", err)
	}

	seed, err := decryptSeed(kf.EncryptedSeed, pw)
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
	endpoint := w.rpcEndpoint()
	rpc, err := newRPCClient(endpoint, "", w.log)
	if err != nil {
		return nil, fmt.Errorf("error creating RPC client: %w", err)
	}
	w.rpc = rpc

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
		Available: dexnear.YoctoToDrops(available),
		Locked:    dexnear.YoctoToDrops(locked),
	}, nil
}

func (w *NearWallet) DepositAddress() (string, error) {
	return w.accountID, nil
}

func (w *NearWallet) OwnsDepositAddress(address string) (bool, error) {
	return strings.EqualFold(address, w.accountID), nil
}

func (w *NearWallet) Send(address string, value, _ uint64) (asset.Coin, error) {
	if !w.ValidateAddress(address) {
		return nil, fmt.Errorf("invalid NEAR address %q", address)
	}
	if value == 0 {
		return nil, fmt.Errorf("cannot send zero value")
	}

	w.keyMtx.RLock()
	if w.privKey == nil {
		w.keyMtx.RUnlock()
		return nil, fmt.Errorf("wallet is locked")
	}
	privKey := w.privKey
	w.keyMtx.RUnlock()

	yoctoAmount := dexnear.DropsToYocto(value)

	w.nonceMtx.Lock()
	defer w.nonceMtx.Unlock()

	// Get access key for nonce and block hash.
	akInfo, err := w.rpc.viewAccessKey(w.accountID, w.pubKeyBase58())
	if err != nil {
		return nil, fmt.Errorf("error querying access key: %w", err)
	}

	w.tipMtx.RLock()
	blockHash := w.tipHash
	w.tipMtx.RUnlock()

	nonce := akInfo.Nonce + 1

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
	encoded := encodeSignedTx(signedTx)

	result, err := w.rpc.broadcastTxCommit(encoded)
	if err != nil {
		return nil, fmt.Errorf("error broadcasting transaction: %w", err)
	}

	if !result.Status.isSuccess() {
		return nil, fmt.Errorf("transaction failed: %s", string(result.Status.Failure))
	}

	w.nonce = nonce

	var txHash [32]byte
	copy(txHash[:], hash[:])

	recipient := address
	wtx := &asset.WalletTransaction{
		Type:      asset.Send,
		ID:        hex.EncodeToString(hash[:]),
		Amount:    value,
		Fees:      dexnear.DefaultFee,
		Recipient: &recipient,
		Confirmed: true,
	}

	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
	}

	return &coin{txHash: txHash, value: value}, nil
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

	synced := time.Since(tipTime) < time.Duration(dexnear.MaxBlockInterval)*time.Second

	return &asset.SyncStatus{
		Synced: synced,
		Blocks: tip,
	}, nil
}

func (w *NearWallet) StandardSendFee(_ uint64) uint64 {
	return dexnear.DefaultFee
}

func (w *NearWallet) TxHistory(_ *asset.TxHistoryRequest) (*asset.TxHistoryResponse, error) {
	// TODO: Implement persistent transaction history.
	return &asset.TxHistoryResponse{}, nil
}

func (w *NearWallet) WalletTransaction(ctx context.Context, txID string) (*asset.WalletTransaction, error) {
	w.pendingTxsMtx.RLock()
	if wtx, found := w.pendingTxs[txID]; found {
		w.pendingTxsMtx.RUnlock()
		return wtx, nil
	}
	w.pendingTxsMtx.RUnlock()

	// Try querying the RPC.
	result, err := w.rpc.txStatus(txID, w.accountID)
	if err != nil {
		return nil, asset.CoinNotFoundError
	}

	txType := asset.Send
	return &asset.WalletTransaction{
		Type:      txType,
		ID:        txID,
		Confirmed: result.Status.isSuccess(),
	}, nil
}

func (w *NearWallet) PendingTransactions(_ context.Context) []*asset.WalletTransaction {
	w.pendingTxsMtx.RLock()
	defer w.pendingTxsMtx.RUnlock()
	txs := make([]*asset.WalletTransaction, 0, len(w.pendingTxs))
	for _, tx := range w.pendingTxs {
		txs = append(txs, tx)
	}
	return txs
}

// tipPoller polls for new blocks.
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

			if newTip && w.emit != nil {
				w.emit.TipChange(block.Header.Height)
			}

		case <-ctx.Done():
			return
		}
	}
}

func (w *NearWallet) rpcEndpoint() string {
	if ep, ok := w.settings["rpcprovider"]; ok && ep != "" {
		return ep
	}
	if ep, ok := dexnear.DefaultRPCEndpoints[w.net]; ok {
		return ep
	}
	return dexnear.DefaultRPCEndpoints[dex.Mainnet]
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

// keyFileData is the structure stored on disk.
type keyFileData struct {
	EncryptedSeed dex.Bytes `json:"encryptedSeed"`
	PubKey        dex.Bytes `json:"pubKey"`
}

// saveKeyFile encrypts the ed25519 seed with AES-256-GCM and writes it to disk.
// The password (already a 32-byte blake256 hash from core) is used as the
// AES key directly.
func saveKeyFile(path string, privKey ed25519.PrivateKey, pw []byte) error {
	seed := privKey.Seed()
	defer encode.ClearBytes(seed)

	encrypted, err := encryptSeed(seed, pw)
	if err != nil {
		return fmt.Errorf("error encrypting seed: %w", err)
	}

	pubKey := make([]byte, ed25519.PublicKeySize)
	copy(pubKey, privKey.Public().(ed25519.PublicKey))

	data := keyFileData{
		EncryptedSeed: encrypted,
		PubKey:        pubKey,
	}

	b, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return os.WriteFile(path, b, 0600)
}

// readKeyFile reads and parses the key file without decrypting the seed.
func readKeyFile(path string) (*keyFileData, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data keyFileData
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// encryptSeed encrypts the seed using AES-256-GCM. The password is hashed
// with SHA-256 to produce the 32-byte AES key. The returned ciphertext
// includes the nonce prepended.
func encryptSeed(seed, pw []byte) ([]byte, error) {
	key := sha256.Sum256(pw)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, seed, nil), nil
}

// decryptSeed decrypts the seed from the ciphertext (nonce prepended) using
// the wallet password.
func decryptSeed(ciphertext, pw []byte) ([]byte, error) {
	key := sha256.Sum256(pw)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	return gcm.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
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
	return strings.Contains(rpcErr.Data, "does not exist")
}
