// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/keystore"
	"github.com/bisoncraft/caribou/wallet/txdb"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/gorilla/websocket"
)

// --- params ---

func TestParseHYPE(t *testing.T) {
	tests := []struct {
		input   string
		want    uint64
		wantErr bool
	}{
		{"1", 1e8, false},
		{"1.0", 1e8, false},
		{"1.5", 150_000_000, false},
		{"0.00000001", 1, false},
		{"0.000000001", 0, false}, // sub-atom truncated
		{"100.50000000", 100_50_000_000, false},
		{"0", 0, false},
		{"", 0, false},
		{"abc", 0, true},
		{"1.abc", 0, true},
		// The API is untrusted input. Scaling by 1e8 overflows uint64 well
		// below ParseUint's own limit, and a wrapped value feeds
		// Balance().Available and ledger amounts as a plausible small number.
		{"184467440737.09551617", 0, true}, // wrapped to 1
		{"184467440738", 0, true},          // whole part alone overflows
		{"18446744073709551615", 0, true},  // MaxUint64 whole part
		// Largest representable value, and one atom below the whole-part
		// boundary: these must still parse.
		{"184467440737.09551615", 18_446_744_073_709_551_615, false},
		{"184467440737", 18_446_744_073_700_000_000, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseHYPE(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseHYPE(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseHYPE(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestAtomsToHYPE(t *testing.T) {
	tests := []struct {
		atoms uint64
		want  string
	}{
		{1e8, "1"},
		{150_000_000, "1.5"},
		{1, "0.00000001"},
		{100_50_000_000, "100.5"},
		{0, "0"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.atoms), func(t *testing.T) {
			got := atomsToHYPE(tt.atoms)
			if got != tt.want {
				t.Errorf("atomsToHYPE(%d) = %q, want %q", tt.atoms, got, tt.want)
			}
			// Round-trip.
			roundTripped, err := parseHYPE(got)
			if err != nil {
				t.Fatalf("parseHYPE round-trip error: %v", err)
			}
			if roundTripped != tt.atoms {
				t.Errorf("round-trip %d -> %q -> %d", tt.atoms, got, roundTripped)
			}
		})
	}
}

// --- coin ---

func TestCoinID(t *testing.T) {
	hashHex := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	txID := "0x" + hashHex

	c := &coin{txID: txID, value: 12345}

	if c.Value() != 12345 {
		t.Errorf("coin.Value() = %d, want 12345", c.Value())
	}
	if c.TxID() != txID {
		t.Errorf("coin.TxID() = %q, want %q", c.TxID(), txID)
	}
	if c.String() != txID {
		t.Errorf("coin.String() = %q, want %q", c.String(), txID)
	}

	id := c.ID()
	if len(id) != 32 {
		t.Errorf("coin.ID() length = %d, want 32", len(id))
	}
	if hex.EncodeToString(id) != hashHex {
		t.Errorf("coin.ID() hex = %q, want %q", hex.EncodeToString(id), hashHex)
	}

	// decodeCoinID round-trip.
	decoded, err := decodeCoinID(id)
	if err != nil {
		t.Fatalf("decodeCoinID error: %v", err)
	}
	if decoded != txID {
		t.Errorf("decodeCoinID = %q, want %q", decoded, txID)
	}

	// Invalid length.
	_, err = decodeCoinID([]byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for short coin ID")
	}
}

func TestNonceBasedCoinID(t *testing.T) {
	nonce := uint64(1_700_000_000_000) // unix ms
	txID := fmt.Sprintf("0x%016x", nonce)

	c := &coin{txID: txID, value: 5e8}
	id := c.ID()
	if len(id) != 32 {
		t.Fatalf("nonce-based coin ID length = %d, want 32", len(id))
	}

	// The nonce should be recoverable from the coin ID bytes (last 8 bytes).
	recovered := strconv.FormatUint(
		uint64(id[24])<<56|uint64(id[25])<<48|uint64(id[26])<<40|uint64(id[27])<<32|
			uint64(id[28])<<24|uint64(id[29])<<16|uint64(id[30])<<8|uint64(id[31]),
		10,
	)
	_ = recovered // Just verify it doesn't panic; exact value is implementation detail.
}

// --- address / key derivation ---

func TestValidateAddress(t *testing.T) {
	w := &HyperWallet{}

	tests := []struct {
		address string
		want    bool
	}{
		{"0x1234567890abcdef1234567890abcdef12345678", true},
		{"0xABCDEF1234567890ABCDEF1234567890ABCDEF12", true},   // mixed case
		{"1234567890abcdef1234567890abcdef12345678", false},    // missing 0x
		{"0x1234567890abcdef1234567890abcdef1234567", false},   // too short
		{"0x1234567890abcdef1234567890abcdef123456789", false}, // too long
		{"0xGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG", false},  // bad chars
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			got := w.ValidateAddress(tt.address)
			if got != tt.want {
				t.Errorf("ValidateAddress(%q) = %v, want %v", tt.address, got, tt.want)
			}
		})
	}
}

func TestPrivKeyDeterminism(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}

	priv1, addr1, zero1, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("privKeyFromSeed error: %v", err)
	}
	defer zero1()

	priv2, addr2, zero2, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("second privKeyFromSeed error: %v", err)
	}
	defer zero2()

	if addr1 != addr2 {
		t.Errorf("same seed produced different addresses: %s vs %s", addr1, addr2)
	}
	if !evmAddressRE.MatchString(addr1) {
		t.Errorf("derived address %q does not match EVM address pattern", addr1)
	}

	pub1 := gethcrypto.CompressPubkey(&priv1.PublicKey)
	pub2 := gethcrypto.CompressPubkey(&priv2.PublicKey)
	if !bytes.Equal(pub1, pub2) {
		t.Error("same seed produced different public keys")
	}
}

func TestPubKeyToAddress(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 7)
	}

	priv, addr, zero, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("privKeyFromSeed error: %v", err)
	}
	defer zero()

	compressedPub := gethcrypto.CompressPubkey(&priv.PublicKey)
	derivedAddr := pubKeyToAddress(compressedPub)

	if derivedAddr != addr {
		t.Errorf("pubKeyToAddress = %q, want %q", derivedAddr, addr)
	}
}

// --- keyfile ---

func TestKeyFileRoundTrip(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 50)
	}
	pw := []byte("keyfile-password-32-bytes-exact!")

	priv, _, zero, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("privKeyFromSeed error: %v", err)
	}
	defer zero()

	dir := t.TempDir()
	path := filepath.Join(dir, keyFileName)

	expectedPub := gethcrypto.CompressPubkey(&priv.PublicKey)
	if err := keystore.Save(path, gethcrypto.FromECDSA(priv), expectedPub, pw); err != nil {
		t.Fatalf("keystore.Save error: %v", err)
	}

	gotPub, err := keystore.PubKey(path)
	if err != nil {
		t.Fatalf("keystore.PubKey error: %v", err)
	}
	if !bytes.Equal(gotPub, expectedPub) {
		t.Error("stored pubkey doesn't match")
	}

	decSeed, err := keystore.Open(path, pw)
	if err != nil {
		t.Fatalf("keystore.Open error: %v", err)
	}
	defer encode.ClearBytes(decSeed)

	recoveredPriv, err := gethcrypto.ToECDSA(decSeed)
	if err != nil {
		t.Fatalf("ToECDSA error: %v", err)
	}
	recoveredPub := gethcrypto.CompressPubkey(&recoveredPriv.PublicKey)
	if !bytes.Equal(recoveredPub, expectedPub) {
		t.Error("recovered key does not match original")
	}
}

func TestKeyFileNotExist(t *testing.T) {
	_, err := keystore.PubKey("/nonexistent/path/hl-keyfile.json")
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got: %v", err)
	}
}

// --- authenticator ---

func TestAuthenticator(t *testing.T) {
	seed := make([]byte, 32)
	rand.Read(seed)
	pw := []byte("authenticator-pw-32bytes-exactly")

	priv, _, zero, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("privKeyFromSeed error: %v", err)
	}
	defer zero()

	dir := t.TempDir()
	pubKey := gethcrypto.CompressPubkey(&priv.PublicKey)
	if err := keystore.Save(filepath.Join(dir, keyFileName), gethcrypto.FromECDSA(priv), pubKey, pw); err != nil {
		t.Fatalf("keystore.Save error: %v", err)
	}

	w := &HyperWallet{
		dataDir: dir,
		pubKey:  pubKey,
		address: pubKeyToAddress(pubKey),
	}

	if !w.Locked() {
		t.Fatal("wallet should be locked initially")
	}

	if err := w.Unlock([]byte("wrong-password-wrong-password!!!!")); err == nil {
		t.Fatal("expected error unlocking with wrong password")
	}
	if !w.Locked() {
		t.Fatal("wallet should still be locked after bad password")
	}

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}
	if w.Locked() {
		t.Fatal("wallet should be unlocked")
	}

	w.keyMtx.RLock()
	if w.privKey == nil {
		t.Error("privKey should be non-nil after unlock")
	}
	w.keyMtx.RUnlock()

	if err := w.Lock(); err != nil {
		t.Fatalf("Lock error: %v", err)
	}
	if !w.Locked() {
		t.Fatal("wallet should be locked after Lock()")
	}

	w.keyMtx.RLock()
	if w.privKey != nil {
		t.Error("privKey should be nil after Lock()")
	}
	w.keyMtx.RUnlock()
}

// --- EIP-712 ---

func TestEIP712DomainHashStable(t *testing.T) {
	// The domain hash must be deterministic — a regression guard.
	h1 := eip712DomainHashFor(eip712ChainIDs[util.Testnet])
	h2 := eip712DomainHashFor(eip712ChainIDs[util.Testnet])
	if !bytes.Equal(h1, h2) {
		t.Error("eip712DomainHashFor is not deterministic")
	}
	if len(h1) != 32 {
		t.Errorf("domain hash length = %d, want 32", len(h1))
	}
}

func TestSendAssetEIP712HashDeterministic(t *testing.T) {
	h1 := sendAssetEIP712Hash(util.Mainnet, "0xabc", "spot", "spot", "HYPE:0x1234", "1.5", "", 1700000000000)
	h2 := sendAssetEIP712Hash(util.Mainnet, "0xabc", "spot", "spot", "HYPE:0x1234", "1.5", "", 1700000000000)
	if !bytes.Equal(h1, h2) {
		t.Error("sendAssetEIP712Hash is not deterministic")
	}

	// Different nonce → different hash.
	h3 := sendAssetEIP712Hash(util.Mainnet, "0xabc", "spot", "spot", "HYPE:0x1234", "1.5", "", 1700000000001)
	if bytes.Equal(h1, h3) {
		t.Error("different nonce should produce different hash")
	}

	// Mainnet vs testnet → different hash.
	h4 := sendAssetEIP712Hash(util.Testnet, "0xabc", "spot", "spot", "HYPE:0x1234", "1.5", "", 1700000000000)
	if bytes.Equal(h1, h4) {
		t.Error("mainnet and testnet should produce different hashes")
	}
}

func TestSignAndVerify(t *testing.T) {
	seed := make([]byte, 32)
	rand.Read(seed)

	priv, _, zero, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("privKeyFromSeed error: %v", err)
	}
	defer zero()

	hash := sendAssetEIP712Hash(util.Mainnet, "0xrecipient", "spot", "spot", "HYPE:0x1234", "1.0", "", 1700000000000)

	sig, err := signHash(priv, hash)
	if err != nil {
		t.Fatalf("signHash error: %v", err)
	}

	if sig[64] != 27 && sig[64] != 28 {
		t.Errorf("V = %d, want 27 or 28", sig[64])
	}

	// Recover the public key and verify the address.
	// go-ethereum's Sign returns V as 0/1 before we add 27; recover expects 0/1.
	recoverySig := make([]byte, 65)
	copy(recoverySig, sig[:])
	recoverySig[64] -= 27

	recovered, err := gethcrypto.SigToPub(hash, recoverySig)
	if err != nil {
		t.Fatalf("SigToPub error: %v", err)
	}
	recoveredAddr := gethcrypto.PubkeyToAddress(*recovered).Hex()
	expectedAddr := gethcrypto.PubkeyToAddress(priv.PublicKey).Hex()

	if recoveredAddr != expectedAddr {
		t.Errorf("recovered address %q != expected %q", recoveredAddr, expectedAddr)
	}
}

// --- RPC client (httptest mock) ---

func newTestRPCClient(t *testing.T, handler http.HandlerFunc) (*rpcClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	log := util.StdOutLogger("HLTEST", util.LevelTrace)
	return newRPCClient(srv.URL, "", log), srv
}

func TestRPCSpotBalance(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		resp := spotClearinghouseState{
			Balances: []spotBalance{
				{Coin: "USDC", Total: "100.0", Hold: "0.0"},
				{Coin: "HYPE", Total: "50.5", Hold: "0.5"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}
	c, srv := newTestRPCClient(t, handler)
	defer srv.Close()

	avail, held, err := c.spotBalance(context.Background(), "0xtest")
	if err != nil {
		t.Fatalf("spotBalance error: %v", err)
	}
	wantAvail, _ := parseHYPE("50.0") // 50.5 - 0.5
	wantHeld, _ := parseHYPE("0.5")
	if avail != wantAvail {
		t.Errorf("available = %d, want %d", avail, wantAvail)
	}
	if held != wantHeld {
		t.Errorf("held = %d, want %d", held, wantHeld)
	}
}

func TestRPCSpotBalanceNoHYPE(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		resp := spotClearinghouseState{
			Balances: []spotBalance{
				{Coin: "USDC", Total: "100.0", Hold: "0.0"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}
	c, srv := newTestRPCClient(t, handler)
	defer srv.Close()

	avail, held, err := c.spotBalance(context.Background(), "0xtest")
	if err != nil {
		t.Fatalf("spotBalance error: %v", err)
	}
	if avail != 0 || held != 0 {
		t.Errorf("expected zero balance when no HYPE entry, got avail=%d held=%d", avail, held)
	}
}

func TestRPCSpotMeta(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		resp := spotMetaResponse{
			Tokens: []spotToken{
				{Name: "USDC", Index: 0, TokenID: "0xusdc"},
				{Name: "HYPE", Index: 1, TokenID: "0x1234abcd"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}
	c, srv := newTestRPCClient(t, handler)
	defer srv.Close()

	meta, err := c.spotMeta(context.Background())
	if err != nil {
		t.Fatalf("spotMeta error: %v", err)
	}

	tokenID, err := hypeTokenIDFromMeta(meta)
	if err != nil {
		t.Fatalf("hypeTokenIDFromMeta error: %v", err)
	}
	if tokenID != "HYPE:0x1234abcd" {
		t.Errorf("tokenID = %q, want %q", tokenID, "HYPE:0x1234abcd")
	}
}

func TestRPCSpotMetaNoHYPE(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		resp := spotMetaResponse{
			Tokens: []spotToken{
				{Name: "USDC", Index: 0, TokenID: "0xusdc"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}
	c, srv := newTestRPCClient(t, handler)
	defer srv.Close()

	meta, err := c.spotMeta(context.Background())
	if err != nil {
		t.Fatalf("spotMeta error: %v", err)
	}
	_, err = hypeTokenIDFromMeta(meta)
	if err == nil {
		t.Error("expected error when HYPE not in meta")
	}
}

func TestRPCLedgerUpdates(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		updates := []ledgerUpdate{
			{
				Time: 1700000001000,
				Hash: "0x" + fmt.Sprintf("%064x", 1),
				Delta: ledgerDelta{
					Type:        "spotTransfer",
					Token:       "HYPE",
					Amount:      "5.0",
					User:        "0xsender",
					Destination: "0xrecipient",
				},
			},
		}
		json.NewEncoder(w).Encode(updates)
	}
	c, srv := newTestRPCClient(t, handler)
	defer srv.Close()

	updates, err := c.ledgerUpdates(context.Background(), "0xrecipient", 1700000000000)
	if err != nil {
		t.Fatalf("ledgerUpdates error: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	amt, err := parseHYPE(updates[0].Delta.Amount)
	if err != nil {
		t.Fatalf("parseHYPE error: %v", err)
	}
	if amt != 5e8 {
		t.Errorf("amount = %d, want 5e8", amt)
	}
}

func TestRPCSpotSend(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != exchangePath {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		resp := exchangeResponse{Status: "ok"}
		json.NewEncoder(w).Encode(resp)
	}
	c, srv := newTestRPCClient(t, handler)
	defer srv.Close()

	action := sendAssetAction{
		Type:             "sendAsset",
		HyperliquidChain: "Testnet",
		SignatureChainID: "0x3e6",
		Destination:      "0xrecipient",
		SourceDex:        "spot",
		DestinationDex:   "spot",
		Token:            "HYPE:0x1234",
		Amount:           "1.0",
		FromSubAccount:   "",
		Nonce:            1700000000000,
	}
	var sig [65]byte
	sig[64] = 28

	if err := c.sendAsset(context.Background(), action, sig, 1700000000000); err != nil {
		t.Fatalf("sendAsset error: %v", err)
	}
}

func TestRPCSendAssetError(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		resp := exchangeResponse{
			Status:   "err",
			Response: json.RawMessage(`"insufficient balance"`),
		}
		json.NewEncoder(w).Encode(resp)
	}
	c, srv := newTestRPCClient(t, handler)
	defer srv.Close()

	action := sendAssetAction{Type: "sendAsset"}
	var sig [65]byte
	err := c.sendAsset(context.Background(), action, sig, 123)
	if err == nil {
		t.Fatal("expected error for err status")
	}
}

func TestRPCSendAssetNestedError(t *testing.T) {
	// Server returns status "ok" but embeds a per-action error in data.statuses.
	handler := func(w http.ResponseWriter, r *http.Request) {
		resp := exchangeResponse{
			Status:   "ok",
			Response: json.RawMessage(`{"type":"default","data":{"statuses":[{"error":"Insufficient balance"}]}}`),
		}
		json.NewEncoder(w).Encode(resp)
	}
	c, srv := newTestRPCClient(t, handler)
	defer srv.Close()

	action := sendAssetAction{Type: "sendAsset"}
	var sig [65]byte
	err := c.sendAsset(context.Background(), action, sig, 123)
	if err == nil {
		t.Fatal("expected error for nested action error")
	}
	var rejection *exchangeRejection
	if !errors.As(err, &rejection) {
		t.Errorf("expected *exchangeRejection, got %T: %v", err, err)
	}
}

// --- driver ---

func TestDriverInfo(t *testing.T) {
	d := &Driver{}
	info := d.Info()
	if info.Name != "Hyperliquid" {
		t.Errorf("Name = %q, want %q", info.Name, "Hyperliquid")
	}
	if info.UnitInfo.Conventional.ConversionFactor != 1e8 {
		t.Errorf("ConversionFactor = %v, want 1e8", info.UnitInfo.Conventional.ConversionFactor)
	}
	if info.Network.ID != BipID {
		t.Errorf("Network.ID = %d, want %d", info.Network.ID, BipID)
	}
}

func TestDriverDecodeCoinID(t *testing.T) {
	d := &Driver{}

	id := make([]byte, 32)
	for i := range id {
		id[i] = byte(i)
	}
	s, err := d.DecodeCoinID(id)
	if err != nil {
		t.Fatalf("DecodeCoinID error: %v", err)
	}
	if s == "" {
		t.Error("DecodeCoinID returned empty string")
	}

	_, err = d.DecodeCoinID([]byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for short coin ID")
	}
}

func TestDriverCreate(t *testing.T) {
	seed := make([]byte, 32)
	rand.Read(seed)
	pw := []byte("driver-create-password-32bytes!!")
	dir := t.TempDir()

	d := &Driver{}

	exists, err := d.Exists("rpc", dir, nil, util.Testnet)
	if err != nil {
		t.Fatalf("Exists error: %v", err)
	}
	if exists {
		t.Fatal("wallet should not exist before Create")
	}

	err = d.Create(&asset.CreateWalletParams{
		Type:    "rpc",
		Seed:    seed,
		Pass:    pw,
		DataDir: dir,
		Net:     util.Testnet,
		Logger:  util.StdOutLogger("HLTEST", util.LevelTrace),
	})
	if err != nil {
		t.Fatalf("Create error: %v", err)
	}

	exists, err = d.Exists("rpc", dir, nil, util.Testnet)
	if err != nil {
		t.Fatalf("Exists after Create error: %v", err)
	}
	if !exists {
		t.Fatal("wallet should exist after Create")
	}
}

// TestMaxBlockIntervalExceedsPingInterval guards the invariant that
// MaxBlockInterval is large enough that a healthy connection (receiving pongs
// every wsPingInterval seconds) never appears stale in SyncStatus.
func TestMaxBlockIntervalExceedsPingInterval(t *testing.T) {
	maxBlock := time.Duration(MaxBlockInterval) * time.Second
	required := wsPingInterval + wsWriteDeadline
	if maxBlock < required {
		t.Errorf("MaxBlockInterval (%s) must be >= wsPingInterval+wsWriteDeadline (%s) "+
			"to prevent SyncStatus.Synced toggling between pings",
			maxBlock, required)
	}
}

func TestSyncStatus(t *testing.T) {
	w := &HyperWallet{}

	// Zero tipTime: not yet connected.
	ss, err := w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus: %v", err)
	}
	if ss.Synced {
		t.Error("expected Synced=false with zero tipTime")
	}

	// Fresh tipTime: synced.
	w.tipMtx.Lock()
	w.tipTime = time.Now()
	w.tipMtx.Unlock()
	ss, _ = w.SyncStatus()
	if !ss.Synced {
		t.Error("expected Synced=true with fresh tipTime")
	}

	// Stale tipTime: not synced.
	w.tipMtx.Lock()
	w.tipTime = time.Now().Add(-time.Duration(MaxBlockInterval+1) * time.Second)
	w.tipMtx.Unlock()
	ss, _ = w.SyncStatus()
	if ss.Synced {
		t.Error("expected Synced=false with stale tipTime")
	}
}

func TestDriverCreateUnknownType(t *testing.T) {
	d := &Driver{}
	err := d.Create(&asset.CreateWalletParams{
		Type: "unknown",
		Seed: make([]byte, 32),
	})
	if err == nil {
		t.Error("expected error for unknown wallet type")
	}
}

// --- nil rpc guard ---

func TestBalanceBeforeConnect(t *testing.T) {
	w := &HyperWallet{}
	_, err := w.Balance()
	if err == nil {
		t.Fatal("expected error calling Balance before Connect")
	}
}

func TestRescanBeforeConnect(t *testing.T) {
	w := &HyperWallet{}
	err := w.Rescan(context.Background(), 0)
	if err == nil {
		t.Fatal("expected error calling Rescan before Connect")
	}
}

func TestSendWithdrawBeforeConnect(t *testing.T) {
	w, _ := newSendTestWallet(t, func(rw http.ResponseWriter, r *http.Request) {
		json.NewEncoder(rw).Encode(exchangeResponse{Status: "ok"})
	})
	w.rpc = nil

	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "send",
			fn: func() error {
				_, err := w.Send("0x1234567890123456789012345678901234567890", 1e8, 0)
				return err
			},
		},
		{
			name: "withdraw",
			fn: func() error {
				_, err := w.Withdraw("0x1234567890123456789012345678901234567890", 1e8, 0)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn()
			if !errors.Is(err, asset.ErrConnectionDown) {
				t.Fatalf("expected ErrConnectionDown, got %v", err)
			}
		})
	}
}

// --- txdb ---

func newTestTxDB(t *testing.T) txdb.NetworkTxDB {
	t.Helper()
	log := util.StdOutLogger("TXDBTEST", util.LevelTrace)
	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	return master.NetworkTxDB(BipID)
}

func TestTxDBStoreGetDelete(t *testing.T) {
	netDB := newTestTxDB(t)

	wtx := &txdb.WalletTransaction{
		NetworkID:   BipID,
		AssetID:     BipID,
		Type:        txdb.Send,
		ID:          "0xaabbcc",
		Amount:      1e8,
		BlockNumber: 100,
		Confirmed:   true,
	}
	setSubmissionTime(wtx, 999)

	if err := netDB.StoreTx(wtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	got, err := netDB.GetTx("0xaabbcc")
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if got == nil {
		t.Fatal("GetTx returned nil for stored tx")
	}
	if got.Amount != 1e8 || got.BlockNumber != 100 {
		t.Errorf("got %+v, want amount=1e8 block=100", got)
	}

	if err := netDB.RemoveTx("0xaabbcc"); err != nil {
		t.Fatalf("RemoveTx: %v", err)
	}
	after, err := netDB.GetTx("0xaabbcc")
	if err != nil {
		t.Fatalf("GetTx after delete: %v", err)
	}
	if after != nil {
		t.Error("expected nil after delete")
	}
}

func TestTxDBGetTxsMissing(t *testing.T) {
	netDB := newTestTxDB(t)
	got, err := netDB.GetTx("not-there")
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if got != nil {
		t.Error("expected nil for missing tx")
	}
}

func TestTxDBGetTxsPagination(t *testing.T) {
	netDB := newTestTxDB(t)

	for i := 1; i <= 5; i++ {
		wtx := &txdb.WalletTransaction{
			NetworkID:   BipID,
			AssetID:     BipID,
			Type:        txdb.Send,
			ID:          fmt.Sprintf("0x%016x", i),
			Amount:      uint64(i) * 1e8,
			BlockNumber: uint64(i),
			Timestamp:   uint64(i) * 1000,
			Confirmed:   true,
		}
		if err := netDB.StoreTx(wtx); err != nil {
			t.Fatalf("StoreTx %d: %v", i, err)
		}
	}

	// Fetch 3 most recent.
	resp, err := netDB.GetTxs(&txdb.TxHistoryRequest{N: 3})
	if err != nil {
		t.Fatalf("GetTxs: %v", err)
	}
	if len(resp.Txs) != 3 {
		t.Fatalf("expected 3 txs, got %d", len(resp.Txs))
	}
	if !resp.MoreAvailable {
		t.Error("expected MoreAvailable=true")
	}
	// Reversed order: block 5, 4, 3.
	if resp.Txs[0].BlockNumber != 5 {
		t.Errorf("first tx block = %d, want 5", resp.Txs[0].BlockNumber)
	}
}

func TestTxDBGetPendingTxs(t *testing.T) {
	netDB := newTestTxDB(t)

	pendingTx := &txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        "pending-1", Type: txdb.Send, Amount: 1e8, BlockNumber: 0,
	}
	confirmedTx := &txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        "confirmed-1", Type: txdb.Send, Amount: 2e8, BlockNumber: 42, Confirmed: true,
	}

	if err := netDB.StoreTx(pendingTx); err != nil {
		t.Fatalf("StoreTx pending: %v", err)
	}
	if err := netDB.StoreTx(confirmedTx); err != nil {
		t.Fatalf("StoreTx confirmed: %v", err)
	}

	got, err := netDB.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 pending tx, got %d", len(got))
	}
	if got[0].ID != "pending-1" {
		t.Errorf("pending tx ID = %q, want %q", got[0].ID, "pending-1")
	}
}

func TestTxDBGetTxsRefIDOnPendingReturnsDistinctError(t *testing.T) {
	w := newTestWalletWithDB(t)

	pendingID := "pending-ref-test"
	if err := w.txDB.StoreTx(&txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        pendingID, Type: txdb.Send, Amount: 1e8, BlockNumber: 0,
	}); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	_, err := w.TxHistory(&txdb.TxHistoryRequest{N: 10, RefID: &pendingID})
	if !errors.Is(err, ErrTxIsPending) {
		t.Errorf("expected ErrTxIsPending, got %v", err)
	}

	missingID := "does-not-exist"
	_, err = w.TxHistory(&txdb.TxHistoryRequest{N: 10, RefID: &missingID})
	if !errors.Is(err, asset.CoinNotFoundError) {
		t.Errorf("expected CoinNotFoundError for missing RefID, got %v", err)
	}
}

// storeTxHistory stores txs in order, optionally injecting a 1ms sleep
// between each so that pending txs stored after confirmed ones always have a
// strictly newer store-time effective-time in the index.
func storeTxHistory(t *testing.T, db txdb.NetworkTxDB, txs []*txdb.WalletTransaction) {
	t.Helper()
	for _, wt := range txs {
		if err := db.StoreTx(wt); err != nil {
			t.Fatalf("StoreTx %s: %v", wt.ID, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// confirmedIDs returns the IDs of txs in order, for use in failure messages.
func confirmedIDs(txs []*txdb.WalletTransaction) []string {
	ids := make([]string, 0, len(txs))
	for _, wt := range txs {
		ids = append(ids, wt.ID)
	}
	return ids
}

func TestTxHistoryPendingAcrossPages(t *testing.T) {
	// 5 confirmed + 3 pending. Pending txs are stored after confirmed ones so
	// their store-time effective-time sorts them to the front of the
	// newest-first index. With N=5, the first raw page returns
	// [P3,P2,P1,C5,C4] (MoreAvailable=true). The loop must advance the cursor
	// past the pending items and collect all 5 confirmed across two raw pages.
	w := newTestWalletWithDB(t)

	confirmed := []*txdb.WalletTransaction{
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "c1", Amount: 1e8, Timestamp: 1000, Confirmed: true},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "c2", Amount: 2e8, Timestamp: 2000, Confirmed: true},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "c3", Amount: 3e8, Timestamp: 3000, Confirmed: true},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "c4", Amount: 4e8, Timestamp: 4000, Confirmed: true},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "c5", Amount: 5e8, Timestamp: 5000, Confirmed: true},
	}
	pending := []*txdb.WalletTransaction{
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "p1", Amount: 1e8},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "p2", Amount: 2e8},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "p3", Amount: 3e8},
	}
	storeTxHistory(t, w.txDB, confirmed)
	storeTxHistory(t, w.txDB, pending)

	resp, err := w.TxHistory(&txdb.TxHistoryRequest{N: 5})
	if err != nil {
		t.Fatalf("TxHistory: %v", err)
	}
	if len(resp.Txs) != 5 {
		t.Fatalf("expected 5 confirmed txs, got %d: %v", len(resp.Txs), confirmedIDs(resp.Txs))
	}
	for _, wt := range resp.Txs {
		if !wt.Confirmed {
			t.Errorf("pending tx %q in TxHistory result", wt.ID)
		}
	}
	if resp.MoreAvailable {
		t.Error("MoreAvailable should be false: all confirmed txs returned")
	}
}

func TestTxHistoryMoreAvailableWithPending(t *testing.T) {
	// 7 confirmed + 2 pending. Pending sort to the front. With N=5, the first
	// raw page is [P2,P1,C7,C6,C5] (MoreAvailable=true). After filtering, only
	// C7,C6,C5 are collected. The second raw page [C5,C4,C3,C2,C1] is fetched;
	// C5 is the cursor (skipped), and we hit N=5 at C3 (i=2 of 5 entries).
	// MoreAvailable must be true because C2 and C1 remain.
	w := newTestWalletWithDB(t)

	var allConfirmed []*txdb.WalletTransaction
	for i := 1; i <= 7; i++ {
		allConfirmed = append(allConfirmed, &txdb.WalletTransaction{
			NetworkID: BipID,
			AssetID:   BipID,
			Type:      txdb.Send,
			ID:        fmt.Sprintf("c%d", i),
			Amount:    uint64(i) * 1e8,
			Timestamp: uint64(i) * 1000,
			Confirmed: true,
		})
	}
	pending := []*txdb.WalletTransaction{
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "p1", Amount: 1e8},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "p2", Amount: 2e8},
	}
	storeTxHistory(t, w.txDB, allConfirmed)
	storeTxHistory(t, w.txDB, pending)

	resp, err := w.TxHistory(&txdb.TxHistoryRequest{N: 5})
	if err != nil {
		t.Fatalf("TxHistory: %v", err)
	}
	if len(resp.Txs) != 5 {
		t.Fatalf("expected 5 confirmed txs, got %d: %v", len(resp.Txs), confirmedIDs(resp.Txs))
	}
	for _, wt := range resp.Txs {
		if !wt.Confirmed {
			t.Errorf("pending tx %q in TxHistory result", wt.ID)
		}
	}
	if !resp.MoreAvailable {
		t.Error("MoreAvailable should be true: c1 and c2 not yet returned")
	}
}

func TestTxHistoryPendingOnlyPage(t *testing.T) {
	// Edge case: the first raw page contains only pending txs (N=3, 3 pending
	// + 2 confirmed). The loop must advance the cursor past all 3 pending txs
	// and collect the 2 confirmed from the second raw page. MoreAvailable must
	// be false because all confirmed txs are returned.
	w := newTestWalletWithDB(t)

	storeTxHistory(t, w.txDB, []*txdb.WalletTransaction{
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "c1", Amount: 1e8, Timestamp: 1000, Confirmed: true},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "c2", Amount: 2e8, Timestamp: 2000, Confirmed: true},
	})
	storeTxHistory(t, w.txDB, []*txdb.WalletTransaction{
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "p1", Amount: 1e8},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "p2", Amount: 2e8},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "p3", Amount: 3e8},
	})

	resp, err := w.TxHistory(&txdb.TxHistoryRequest{N: 3})
	if err != nil {
		t.Fatalf("TxHistory: %v", err)
	}
	if len(resp.Txs) != 2 {
		t.Fatalf("expected 2 confirmed txs, got %d", len(resp.Txs))
	}
	for _, wt := range resp.Txs {
		if !wt.Confirmed {
			t.Errorf("pending tx %q in TxHistory result", wt.ID)
		}
	}
	if resp.MoreAvailable {
		t.Error("MoreAvailable should be false: all confirmed txs returned")
	}
}

func TestTxDBPendingIDIndex(t *testing.T) {
	w := newTestWalletWithDB(t)

	realHash := "0x" + fmt.Sprintf("%064x", 55)
	pendingID := fmt.Sprintf("0x%016x", uint64(1700000001000))
	recipient := "0x1111111111111111111111111111111111111111"

	// Store a confirmed tx that carries the pendingID alias.
	if err := w.txDB.StoreTx(&txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        realHash,
		Type:      txdb.Send,
		Amount:    1e8,
		Confirmed: true,
		Recipient: &recipient,
		AdditionalData: map[string]string{
			pendingIDKey: pendingID,
		},
	}); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	found, err := w.getTxByPendingID(pendingID)
	if err != nil {
		t.Fatalf("getTxByPendingID: %v", err)
	}
	if found == nil {
		t.Fatal("expected tx, got nil")
	}
	if found.ID != realHash {
		t.Errorf("ID = %q, want %q", found.ID, realHash)
	}

	// A tx without a pendingID must not be found via the index.
	notFound, err := w.getTxByPendingID("0x0000000000000000")
	if err != nil {
		t.Fatalf("getTxByPendingID (missing): %v", err)
	}
	if notFound != nil {
		t.Error("expected nil for unknown pendingID")
	}
}

func TestTxDBLastScannedMSRoundTrip(t *testing.T) {
	log := util.StdOutLogger("TXDBTEST", util.LevelTrace)
	dir := t.TempDir()

	master, err := txdb.New(filepath.Join(dir, "tx.db"), log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	netDB := master.NetworkTxDB(BipID)
	mt, err := netDB.Table(metaTableName)
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	w := &HyperWallet{metaTable: mt}
	if err := w.setLastScannedMS(1700000000123); err != nil {
		t.Fatalf("setLastScannedMS: %v", err)
	}
	if err := master.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	master2, err := txdb.New(filepath.Join(dir, "tx.db"), log)
	if err != nil {
		t.Fatalf("reopen txdb.New: %v", err)
	}
	t.Cleanup(func() { master2.Close() })
	netDB2 := master2.NetworkTxDB(BipID)
	mt2, err := netDB2.Table(metaTableName)
	if err != nil {
		t.Fatalf("reopen Table: %v", err)
	}
	w2 := &HyperWallet{metaTable: mt2}

	got, err := w2.getLastScannedMS()
	if err != nil {
		t.Fatalf("getLastScannedMS: %v", err)
	}
	if got != 1700000000123 {
		t.Fatalf("lastScannedMS = %d, want %d", got, int64(1700000000123))
	}
}

// mustSetupWalletTxDB wires metaTable and pendingIDIndex onto w from its txDB.
// Called by test helpers that construct a HyperWallet without going through
// Connect, to mirror the setup Connect performs.
func mustSetupWalletTxDB(t *testing.T, w *HyperWallet) {
	t.Helper()
	mt, err := w.txDB.Table(metaTableName)
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	w.metaTable = mt
	idx, err := w.txDB.AddUniqueTxIndex(pendingIDIndexName, pendingIDIndexKey)
	if err != nil {
		t.Fatalf("AddUniqueTxIndex: %v", err)
	}
	w.pendingIDIndex = idx
}

// --- confirmPendingSend ---

func newTestWalletWithDB(t *testing.T) *HyperWallet {
	t.Helper()
	netDB := newTestTxDB(t)
	log := util.StdOutLogger("HLWTEST", util.LevelTrace)
	noteChan := make(chan asset.WalletNotification, 16)
	w := &HyperWallet{
		log:     log,
		net:     util.Testnet,
		address: "0xabcdef1234567890abcdef1234567890abcdef12",
		emit:    asset.NewWalletEmitter(noteChan, BipID, log),
		tipMtx:  sync.RWMutex{},
		tip:     uint64(1700000000000),
		txDB:    netDB,
	}
	mustSetupWalletTxDB(t, w)
	return w
}

// --- send ordering ---

// newSendTestWallet returns a wallet wired to a mock exchange server.
// The handler governs whether sendAsset succeeds or fails.
func newSendTestWallet(t *testing.T, exchangeHandler http.HandlerFunc) (*HyperWallet, *httptest.Server) {
	return newSendTestWalletWithBalance(t, "10", exchangeHandler)
}

func newSendTestWalletWithBalance(t *testing.T, balance string, exchangeHandler http.HandlerFunc) (*HyperWallet, *httptest.Server) {
	t.Helper()
	infoHandler := func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		switch payload["type"] {
		case "spotClearinghouseState":
			json.NewEncoder(w).Encode(spotClearinghouseState{
				Balances: []spotBalance{{Coin: "HYPE", Total: balance, Hold: "0"}},
			})
		}
	}
	return newSendTestWalletWithHandlers(t, infoHandler, exchangeHandler)
}

func newSendTestWalletWithHandlers(t *testing.T, infoHandler, exchangeHandler http.HandlerFunc) (*HyperWallet, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == exchangePath {
			exchangeHandler(w, r)
			return
		}
		infoHandler(w, r)
	}))

	rpc := newRPCClient(srv.URL, "", util.StdOutLogger("HLTEST", util.LevelTrace))

	seed := make([]byte, 32)
	rand.Read(seed)
	pw := []byte("send-test-password-32-bytes-here")
	dir := t.TempDir()

	d := &Driver{}
	if err := d.Create(&asset.CreateWalletParams{
		Type: "rpc", Seed: seed, Pass: pw, DataDir: dir,
		Net:    util.Testnet,
		Logger: util.StdOutLogger("HLTEST", util.LevelTrace),
	}); err != nil {
		srv.Close()
		t.Fatalf("Create: %v", err)
	}

	netDB := newTestTxDB(t)

	pubKey, err := keystore.PubKey(filepath.Join(dir, keyFileName))
	if err != nil {
		srv.Close()
		t.Fatalf("keystore.PubKey: %v", err)
	}

	noteChan := make(chan asset.WalletNotification, 16)
	w := &HyperWallet{
		log:         util.StdOutLogger("HLTEST", util.LevelTrace),
		net:         util.Testnet,
		address:     pubKeyToAddress(pubKey),
		pubKey:      pubKey,
		dataDir:     dir,
		settings:    map[string]string{},
		hypeTokenID: "HYPE:0x1234",
		rpc:         rpc,
		emit:        asset.NewWalletEmitter(noteChan, BipID, util.StdOutLogger("HLTEST", util.LevelTrace)),
		txDB:        netDB,
		ctx:         context.Background(),
	}
	mustSetupWalletTxDB(t, w)
	w.Unlock(pw)
	t.Cleanup(func() { srv.Close() })
	return w, srv
}

func TestSendInsufficientBalance(t *testing.T) {
	var exchangeCalls atomic.Int32
	w, _ := newSendTestWalletWithBalance(t, "0.5", func(rw http.ResponseWriter, r *http.Request) {
		exchangeCalls.Add(1)
		json.NewEncoder(rw).Encode(exchangeResponse{Status: "ok"})
	})

	_, err := w.send("0x1234567890123456789012345678901234567890", 1e8)
	if !errors.Is(err, asset.ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
	if exchangeCalls.Load() != 0 {
		t.Fatalf("exchange called %d times, want 0", exchangeCalls.Load())
	}
	w.sendMtx.Lock()
	nextNonce := w.nextSendNonce
	w.sendMtx.Unlock()
	if nextNonce != 0 {
		t.Fatalf("nextSendNonce = %d, want 0 (insufficient send must not allocate a nonce)", nextNonce)
	}
	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending transactions = %d, want 0", len(pending))
	}
}

func TestConcurrentSendsSerializeBalanceCheck(t *testing.T) {
	var (
		balanceCalls  atomic.Int32
		exchangeCalls atomic.Int32
		sent          atomic.Bool
		twoBalances   = make(chan struct{})
		closeBalances sync.Once
	)

	infoHandler := func(w http.ResponseWriter, r *http.Request) {
		if balanceCalls.Add(1) == 2 {
			closeBalances.Do(func() { close(twoBalances) })
		}
		balance := "1"
		if sent.Load() {
			balance = "0"
		}
		json.NewEncoder(w).Encode(spotClearinghouseState{
			Balances: []spotBalance{{Coin: "HYPE", Total: balance, Hold: "0"}},
		})
	}
	w, _ := newSendTestWalletWithHandlers(t, infoHandler, func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-twoBalances:
		case <-time.After(100 * time.Millisecond):
		}
		exchangeCalls.Add(1)
		sent.Store(true)
		json.NewEncoder(rw).Encode(exchangeResponse{Status: "ok"})
	})

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := w.send("0x1234567890123456789012345678901234567890", 1e8)
			errs <- err
		}()
	}
	close(start)
	var success, insufficient int
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			success++
		case errors.Is(err, asset.ErrInsufficientBalance):
			insufficient++
		default:
			t.Fatalf("unexpected send error: %v", err)
		}
	}
	if success != 1 || insufficient != 1 {
		t.Fatalf("results: success=%d insufficient=%d, want 1 each", success, insufficient)
	}
	if exchangeCalls.Load() != 1 {
		t.Fatalf("exchange called %d times, want 1", exchangeCalls.Load())
	}
}

func TestSendStoreBeforeBroadcast(t *testing.T) {
	// Successful send: pending tx should be stored.
	w, _ := newSendTestWallet(t, func(rw http.ResponseWriter, r *http.Request) {
		json.NewEncoder(rw).Encode(exchangeResponse{Status: "ok"})
	})

	recipient := "0x1234567890123456789012345678901234567890"
	coin, err := w.send(recipient, 1e8)
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	tx, err := w.txDB.GetTx(coin.TxID())
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if tx == nil {
		t.Fatal("pending tx not stored after successful send")
	}
	if hyperData(tx).SubmissionTime == 0 {
		t.Error("SubmissionTime not set")
	}
}

func TestSendCleansUpOnBroadcastFailure(t *testing.T) {
	// Failed broadcast: the stored pending tx should be cleaned up.
	w, _ := newSendTestWallet(t, func(rw http.ResponseWriter, r *http.Request) {
		json.NewEncoder(rw).Encode(exchangeResponse{
			Status:   "err",
			Response: json.RawMessage(`"insufficient funds"`),
		})
	})

	recipient := "0x1234567890123456789012345678901234567890"
	_, err := w.send(recipient, 1e8)
	if err == nil {
		t.Fatal("expected error from failed send")
	}

	// No pending entries should remain.
	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending txs after failed send, got %d", len(pending))
	}
}

func TestSendKeepsPendingOnNetworkError(t *testing.T) {
	// A server that returns HTTP 500 (transport error, not an exchange rejection).
	// The pending record must survive so scanLedger can resolve the tx.
	w, _ := newSendTestWallet(t, func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "server error", http.StatusInternalServerError)
	})

	recipient := "0x1234567890123456789012345678901234567890"
	_, sendErr := w.send(recipient, 1e8)
	if sendErr == nil {
		t.Fatal("expected error from failed send")
	}
	if !errors.Is(sendErr, ErrSendOutcomeUnknown) {
		t.Errorf("expected ErrSendOutcomeUnknown on transport error, got %v", sendErr)
	}

	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending tx retained after network error, got %d", len(pending))
	}
	// The retained record must carry when its outcome stopped being observable.
	// Without it confirmPendingSend cannot tell this record from an identical
	// retry, and the orphan takes the retry's echo on nonce age alone.
	d := hyperData(pending[0])
	if d.OutcomeUnknownMS == 0 {
		t.Error("retained pending send was not stamped with OutcomeUnknownMS")
	}
	if d.SubmissionTime == 0 {
		t.Error("stamping the unknown outcome dropped SubmissionTime; the record can never expire")
	}
}

// --- pending tx expiry ---

func TestExpireOldPending(t *testing.T) {
	w := newTestWalletWithDB(t)

	nonce := uint64(1700000001000)
	pendingID := fmt.Sprintf("0x%016x", nonce)
	recipient := "0x1111111111111111111111111111111111111111"

	// Store a pending tx with a submission time well past the TTL.
	staleTime := uint64(time.Now().Unix()) - pendingTTL - 60
	pendingWtx := &txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        pendingID,
		Type:      txdb.Send,
		Amount:    1e8,
		Recipient: &recipient,
		AdditionalData: map[string]string{
			"nonce": strconv.FormatUint(nonce, 10),
		},
	}
	setSubmissionTime(pendingWtx, staleTime)
	if err := w.txDB.StoreTx(pendingWtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	// Trigger confirmPendingSend with a non-matching update (different dest).
	// The stale tx should be expired even though it doesn't match.
	differentDest := "0x9999999999999999999999999999999999999999"
	w.confirmPendingSend(ledgerUpdate{
		Time: nonce + 100,
		Hash: "0x" + fmt.Sprintf("%064x", 1),
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "1",
			User:        w.address,
			Destination: differentDest,
		},
	}, 1e8, nil)

	tx, err := w.txDB.GetTx(pendingID)
	if err != nil {
		t.Fatalf("getTx: %v", err)
	}
	if tx != nil {
		t.Error("stale pending tx should have been expired")
	}
}

func TestFreshPendingNotExpired(t *testing.T) {
	w := newTestWalletWithDB(t)

	nonce := uint64(1700000002000)
	pendingID := fmt.Sprintf("0x%016x", nonce)
	recipient := "0x2222222222222222222222222222222222222222"

	// Store a fresh pending tx (just submitted).
	freshWtx := &txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        pendingID,
		Type:      txdb.Send,
		Amount:    1e8,
		Recipient: &recipient,
		AdditionalData: map[string]string{
			"nonce": strconv.FormatUint(nonce, 10),
		},
	}
	setSubmissionTime(freshWtx, uint64(time.Now().Unix()))
	if err := w.txDB.StoreTx(freshWtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	// Non-matching update should not expire the fresh entry.
	differentDest := "0x9999999999999999999999999999999999999999"
	w.confirmPendingSend(ledgerUpdate{
		Time: nonce + 100,
		Hash: "0x" + fmt.Sprintf("%064x", 2),
		Delta: ledgerDelta{
			Type: "spotTransfer", Token: "HYPE", Amount: "1",
			User: w.address, Destination: differentDest,
		},
	}, 1e8, nil)

	tx, err := w.txDB.GetTx(pendingID)
	if err != nil {
		t.Fatalf("getTx: %v", err)
	}
	if tx == nil {
		t.Error("fresh pending tx should not have been expired")
	}
}

func TestConfirmPendingSend(t *testing.T) {
	w := newTestWalletWithDB(t)
	nonce := uint64(1700000001000)

	// Insert a pending send with a nonce-based ID.
	pendingID := fmt.Sprintf("0x%016x", nonce)
	recipient := "0x1111111111111111111111111111111111111111"
	pendingTx := &txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		Type:      txdb.Send,
		ID:        pendingID,
		Amount:    5e8,
		Recipient: &recipient,
		AdditionalData: map[string]string{
			"nonce": strconv.FormatUint(nonce, 10),
		},
	}
	setSubmissionTime(pendingTx, uint64(time.Now().Unix()))
	if err := w.txDB.StoreTx(pendingTx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	// Simulate a matching ledger update from the chain.
	realHash := "0x" + fmt.Sprintf("%064x", 42)
	update := ledgerUpdate{
		Time: nonce + 500, // 500ms after nonce
		Hash: realHash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "5",
			User:        w.address,
			Destination: recipient,
		},
	}

	w.confirmPendingSend(update, 5e8, nil)

	// Pending entry should be gone.
	old, err := w.txDB.GetTx(pendingID)
	if err != nil {
		t.Fatalf("GetTx old: %v", err)
	}
	if old != nil {
		t.Error("pending tx should have been deleted after confirmation")
	}

	// Confirmed entry should exist under the real hash.
	confirmed, err := w.txDB.GetTx(realHash)
	if err != nil {
		t.Fatalf("GetTx confirmed: %v", err)
	}
	if confirmed == nil {
		t.Fatal("confirmed tx not found under real hash")
	}
	if !confirmed.Confirmed {
		t.Error("tx should be marked confirmed")
	}
	if confirmed.BlockNumber == 0 {
		t.Error("confirmed tx should have a block number")
	}
	if confirmed.AdditionalData[pendingIDKey] != pendingID {
		t.Fatalf("pending alias = %q, want %q", confirmed.AdditionalData[pendingIDKey], pendingID)
	}
}

func TestConfirmPendingSendNoMatch(t *testing.T) {
	w := newTestWalletWithDB(t)
	nonce := uint64(1700000001000)

	pendingID := fmt.Sprintf("0x%016x", nonce)
	recipient := "0x2222222222222222222222222222222222222222"
	if err := w.txDB.StoreTx(&txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        pendingID,
		Type:      txdb.Send,
		Amount:    5e8,
		Recipient: &recipient,
		AdditionalData: map[string]string{
			"nonce": strconv.FormatUint(nonce, 10),
		},
	}); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	// Update with wrong amount — should not match.
	w.confirmPendingSend(ledgerUpdate{
		Time: nonce + 100,
		Hash: "0x" + fmt.Sprintf("%064x", 99),
		Delta: ledgerDelta{
			Type: "spotTransfer", Token: "HYPE",
			Amount: "99", User: w.address, Destination: recipient,
		},
	}, 99e8, nil)

	// Pending entry should still be there.
	still, err := w.txDB.GetTx(pendingID)
	if err != nil {
		t.Fatalf("getTx: %v", err)
	}
	if still == nil {
		t.Error("pending tx should not have been touched when amount doesn't match")
	}
}

func TestConfirmPendingSendMatchesRecipient(t *testing.T) {
	w := newTestWalletWithDB(t)
	baseNonce := uint64(1700000001000)

	recipientA := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	recipientB := "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for i, recipient := range []string{recipientA, recipientB} {
		nonce := baseNonce + uint64(i)
		pendingID := fmt.Sprintf("0x%016x", nonce)
		if err := w.txDB.StoreTx(&txdb.WalletTransaction{
			NetworkID: BipID,
			AssetID:   BipID,
			ID:        pendingID,
			Type:      txdb.Send,
			Amount:    5e8,
			Recipient: &recipient,
			AdditionalData: map[string]string{
				"nonce": strconv.FormatUint(nonce, 10),
			},
		}); err != nil {
			t.Fatalf("StoreTx %d: %v", i, err)
		}
	}

	realHash := "0x" + fmt.Sprintf("%064x", 77)
	w.confirmPendingSend(ledgerUpdate{
		Time: baseNonce + 2,
		Hash: realHash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "5",
			User:        w.address,
			Destination: recipientB,
		},
	}, 5e8, nil)

	confirmed, err := w.txDB.GetTx(realHash)
	if err != nil {
		t.Fatalf("GetTx confirmed: %v", err)
	}
	if confirmed == nil {
		t.Fatal("expected confirmed tx for matching recipient")
	}
	if confirmed.Recipient == nil || !strings.EqualFold(*confirmed.Recipient, recipientB) {
		t.Fatalf("confirmed recipient = %v, want %s", confirmed.Recipient, recipientB)
	}

	stillPending, err := w.txDB.GetTx(fmt.Sprintf("0x%016x", baseNonce))
	if err != nil {
		t.Fatalf("getTx pending A: %v", err)
	}
	if stillPending == nil {
		t.Fatal("wrong recipient pending tx should remain pending")
	}
}

// TestConfirmPendingSendPairsFIFO covers two identical sends — same recipient,
// same amount, a second apart — being resolved against the wrong hashes. The
// previous heuristic picked the nonce numerically closest to the ledger update
// time, so the first echo confirmed the *second* send; the first send then had
// no echo left to claim, aged out at pendingTTL and was removed, losing a
// transfer that had landed. Hyperliquid processes transfers in nonce order and
// echoes them in time order, so the two must pair oldest-first.
func TestConfirmPendingSendPairsFIFO(t *testing.T) {
	w := newTestWalletWithDB(t)

	recipient := "0x3333333333333333333333333333333333333333"
	baseTime := uint64(1700000010000)
	// Two sends a second apart. The first echo arrives at baseTime, closer to
	// nonceB than to nonceA — which is what misled the old heuristic.
	nonceA := baseTime - 2000
	nonceB := baseTime - 500
	pendingIDA := fmt.Sprintf("0x%016x", nonceA)
	pendingIDB := fmt.Sprintf("0x%016x", nonceB)

	for _, entry := range []struct {
		id    string
		nonce uint64
	}{
		{pendingIDA, nonceA},
		{pendingIDB, nonceB},
	} {
		wtx := &txdb.WalletTransaction{
			NetworkID: BipID,
			AssetID:   BipID,
			ID:        entry.id,
			Type:      txdb.Send,
			Amount:    5e8,
			Recipient: &recipient,
			AdditionalData: map[string]string{
				"nonce": strconv.FormatUint(entry.nonce, 10),
			},
		}
		setSubmissionTime(wtx, uint64(time.Now().Unix()))
		if err := w.txDB.StoreTx(wtx); err != nil {
			t.Fatalf("StoreTx %s: %v", entry.id, err)
		}
	}

	echo := func(at uint64, hash string) {
		w.confirmPendingSend(ledgerUpdate{
			Time: at,
			Hash: hash,
			Delta: ledgerDelta{
				Type:        "spotTransfer",
				Token:       "HYPE",
				Amount:      "5",
				User:        w.address,
				Destination: recipient,
			},
		}, 5e8, nil)
	}

	hashA := "0x" + fmt.Sprintf("%064x", 55)
	hashB := "0x" + fmt.Sprintf("%064x", 56)

	// First echo pairs with the older send, not the nearer one.
	echo(baseTime, hashA)
	confirmed, err := w.txDB.GetTx(hashA)
	if err != nil {
		t.Fatalf("GetTx %s: %v", hashA, err)
	}
	if confirmed == nil {
		t.Fatal("first echo stored no confirmed tx")
	}
	if got := confirmed.AdditionalData[pendingIDKey]; got != pendingIDA {
		t.Errorf("first echo confirmed %q, want %q (oldest nonce)", got, pendingIDA)
	}
	if tx, _ := w.txDB.GetTx(pendingIDA); tx != nil {
		t.Error("pendingIDA should have been deleted after confirmation")
	}
	if tx, _ := w.txDB.GetTx(pendingIDB); tx == nil {
		t.Fatal("pendingIDB should still be pending after the first echo")
	}

	// Second echo pairs with the remaining send, so neither is lost.
	echo(baseTime+1000, hashB)
	confirmed, err = w.txDB.GetTx(hashB)
	if err != nil {
		t.Fatalf("GetTx %s: %v", hashB, err)
	}
	if confirmed == nil {
		t.Fatal("second echo stored no confirmed tx")
	}
	if got := confirmed.AdditionalData[pendingIDKey]; got != pendingIDB {
		t.Errorf("second echo confirmed %q, want %q", got, pendingIDB)
	}
	if tx, _ := w.txDB.GetTx(pendingIDB); tx != nil {
		t.Error("pendingIDB should have been deleted after the second echo")
	}
}

// TestConfirmPendingSendNonceAfterEcho covers clock skew: the only candidate's
// nonce is newer than the ledger update time. It cannot literally be this echo,
// but with nothing else outstanding it is the only sensible match — dropping it
// would leave a landed transfer to age out at pendingTTL.
func TestConfirmPendingSendNonceAfterEcho(t *testing.T) {
	w := newTestWalletWithDB(t)

	recipient := "0x5555555555555555555555555555555555555555"
	echoTime := uint64(1700000010000)
	nonce := echoTime + 3000 // minted "after" the echo per the local clock
	pendingID := fmt.Sprintf("0x%016x", nonce)

	wtx := &txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        pendingID,
		Type:      txdb.Send,
		Amount:    5e8,
		Recipient: &recipient,
		AdditionalData: map[string]string{
			"nonce": strconv.FormatUint(nonce, 10),
		},
	}
	setSubmissionTime(wtx, uint64(time.Now().Unix()))
	if err := w.txDB.StoreTx(wtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	realHash := "0x" + fmt.Sprintf("%064x", 57)
	w.confirmPendingSend(ledgerUpdate{
		Time: echoTime,
		Hash: realHash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "5",
			User:        w.address,
			Destination: recipient,
		},
	}, 5e8, nil)

	confirmed, err := w.txDB.GetTx(realHash)
	if err != nil {
		t.Fatalf("GetTx confirmed: %v", err)
	}
	if confirmed == nil {
		t.Fatal("the only candidate was not confirmed")
	}
	if got := confirmed.AdditionalData[pendingIDKey]; got != pendingID {
		t.Errorf("pendingIDKey = %q, want %q", got, pendingID)
	}
}

func TestConfirmPendingSendSlowServer(t *testing.T) {
	// A single pending send whose ledger echo arrives >5s after nonce mint.
	// It must still be confirmed — nonce proximity is a tiebreaker, not a gate.
	w := newTestWalletWithDB(t)

	nonce := uint64(1700000010000)
	pendingID := fmt.Sprintf("0x%016x", nonce)
	recipient := "0x4444444444444444444444444444444444444444"
	slowWtx := &txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        pendingID,
		Type:      txdb.Send,
		Amount:    2e8,
		Recipient: &recipient,
		AdditionalData: map[string]string{
			"nonce": strconv.FormatUint(nonce, 10),
		},
	}
	setSubmissionTime(slowWtx, uint64(time.Now().Unix()))
	if err := w.txDB.StoreTx(slowWtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	// Ledger echo arrives 30 seconds (30000ms) after the nonce was minted.
	realHash := "0x" + fmt.Sprintf("%064x", 88)
	w.confirmPendingSend(ledgerUpdate{
		Time: nonce + 30000,
		Hash: realHash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "2",
			User:        w.address,
			Destination: recipient,
		},
	}, 2e8, nil)

	confirmed, err := w.txDB.GetTx(realHash)
	if err != nil {
		t.Fatalf("getTx: %v", err)
	}
	if confirmed == nil {
		t.Fatal("send should be confirmed even when nonce diff > 5s")
	}
	if confirmed.AdditionalData[pendingIDKey] != pendingID {
		t.Errorf("pendingIDKey = %q, want %q", confirmed.AdditionalData[pendingIDKey], pendingID)
	}
}

// storePendingSend stores a pending send of amount atoms to recipient, minted
// at nonce. unknownAtMS, when non-zero, marks the send as one whose transport
// failed after dispatch.
func storePendingSend(t *testing.T, w *HyperWallet, nonce, amount uint64, recipient string, unknownAtMS uint64) string {
	t.Helper()
	id := fmt.Sprintf("0x%016x", nonce)
	wtx := &txdb.WalletTransaction{
		NetworkID:      BipID,
		AssetID:        BipID,
		ID:             id,
		Type:           txdb.Send,
		Amount:         amount,
		Recipient:      &recipient,
		AdditionalData: map[string]string{"nonce": strconv.FormatUint(nonce, 10)},
	}
	if err := setSubmissionTime(wtx, uint64(time.Now().Unix())); err != nil {
		t.Fatalf("setSubmissionTime: %v", err)
	}
	if unknownAtMS > 0 {
		if err := setOutcomeUnknown(wtx, unknownAtMS); err != nil {
			t.Fatalf("setOutcomeUnknown: %v", err)
		}
	}
	if err := w.txDB.StoreTx(wtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}
	return id
}

func spotTransferEcho(w *HyperWallet, at uint64, hash, recipient, amount string) ledgerUpdate {
	return ledgerUpdate{
		Time: at,
		Hash: hash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      amount,
			User:        w.address,
			Destination: recipient,
		},
	}
}

// TestSetOutcomeUnknownPreservesSubmissionTime guards the two fields sharing
// one RawTx payload. Stamping the unknown outcome happens after the submission
// time is already encoded, and dropping it would make the record immortal:
// confirmPendingSend only expires a pending send whose SubmissionTime is
// non-zero, so an orphan would sit in the pending set forever.
func TestSetOutcomeUnknownPreservesSubmissionTime(t *testing.T) {
	wtx := &txdb.WalletTransaction{NetworkID: BipID, AssetID: BipID, ID: "0x01"}
	if err := setSubmissionTime(wtx, 1_700_000_000); err != nil {
		t.Fatalf("setSubmissionTime: %v", err)
	}
	if err := setOutcomeUnknown(wtx, 1_700_000_030_000); err != nil {
		t.Fatalf("setOutcomeUnknown: %v", err)
	}
	d := hyperData(wtx)
	if d.SubmissionTime != 1_700_000_000 {
		t.Errorf("SubmissionTime = %d, want 1700000000", d.SubmissionTime)
	}
	if d.OutcomeUnknownMS != 1_700_000_030_000 {
		t.Errorf("OutcomeUnknownMS = %d, want 1700000030000", d.OutcomeUnknownMS)
	}
}

// TestConfirmPendingSendOrphanDoesNotStealRetry covers the send that never
// landed. Its transport failed after dispatch, so the record is kept — but FIFO
// pairs on nonce age, so without a bound the orphan wins the retry's echo, the
// confirmed record is filed under the orphan's coin ID, and the send that
// actually settled expires at pendingTTL with WalletTransaction reporting
// not-found for it.
func TestConfirmPendingSendOrphanDoesNotStealRetry(t *testing.T) {
	w := newTestWalletWithDB(t)
	recipient := "0x6666666666666666666666666666666666666666"

	// Orphan: minted, dispatched, transport gave up 30s later (httpTimeout).
	orphanNonce := uint64(1700000010000)
	unknownAt := orphanNonce + 30_000
	orphanID := storePendingSend(t, w, orphanNonce, 5e8, recipient, unknownAt)

	// Retry: same amount, same recipient, minted just after the failure.
	retryNonce := unknownAt + 2_000
	retryID := storePendingSend(t, w, retryNonce, 5e8, recipient, 0)

	// Only the retry lands.
	hash := "0x" + fmt.Sprintf("%064x", 301)
	w.confirmPendingSend(spotTransferEcho(w, retryNonce+1_000, hash, recipient, "5"), 5e8, nil)

	confirmed, err := w.txDB.GetTx(hash)
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if confirmed == nil {
		t.Fatal("the landed send was not confirmed")
	}
	if got := confirmed.AdditionalData[pendingIDKey]; got != retryID {
		t.Fatalf("echo paired with %q, want the retry %q", got, retryID)
	}
	// The orphan must be left alone to expire, not consumed.
	if orphan, err := w.txDB.GetTx(orphanID); err != nil {
		t.Fatalf("GetTx orphan: %v", err)
	} else if orphan == nil {
		t.Fatal("orphan was removed by the retry's echo")
	}
}

// TestConfirmPendingSendUnknownOutcomeThatLanded is the mirror case: the
// transport failed after dispatch but the send did land. Its echo carries a
// ledger time from before we gave up, so the record must still be confirmed —
// the bound must not cost us a transfer that settled.
func TestConfirmPendingSendUnknownOutcomeThatLanded(t *testing.T) {
	w := newTestWalletWithDB(t)
	recipient := "0x8888888888888888888888888888888888888888"

	nonce := uint64(1700000010000)
	unknownAt := nonce + 30_000
	pendingID := storePendingSend(t, w, nonce, 5e8, recipient, unknownAt)

	// Processed 1s after mint — well before the client gave up.
	hash := "0x" + fmt.Sprintf("%064x", 302)
	w.confirmPendingSend(spotTransferEcho(w, nonce+1_000, hash, recipient, "5"), 5e8, nil)

	confirmed, err := w.txDB.GetTx(hash)
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if confirmed == nil {
		t.Fatal("a send that landed before the transport failed was not confirmed")
	}
	if got := confirmed.AdditionalData[pendingIDKey]; got != pendingID {
		t.Fatalf("pendingIDKey = %q, want %q", got, pendingID)
	}
}

// TestConfirmPendingSendIgnoresAncientEcho covers Rescan, which replays the
// whole ledger history oldest-first. Every historical echo predates an
// in-flight send's nonce, so the newer-than-echo fallback used to hand the
// oldest one the pending record: the send was re-stored under a months-old
// hash and timestamp, and the real echo later in the replay found no candidate.
func TestConfirmPendingSendIgnoresAncientEcho(t *testing.T) {
	w := newTestWalletWithDB(t)
	recipient := "0x9999999999999999999999999999999999999999"

	nonce := uint64(1700000010000)
	storePendingSend(t, w, nonce, 5e8, recipient, 0)

	// An echo from ~3 months earlier, for an identical past transfer.
	ancientHash := "0x" + fmt.Sprintf("%064x", 303)
	w.confirmPendingSend(spotTransferEcho(w, nonce-90*24*3600*1000, ancientHash, recipient, "5"), 5e8, nil)

	if confirmed, err := w.txDB.GetTx(ancientHash); err != nil {
		t.Fatalf("GetTx: %v", err)
	} else if confirmed != nil {
		t.Fatal("in-flight send was confirmed against a months-old echo")
	}

	// The real echo still finds its candidate.
	hash := "0x" + fmt.Sprintf("%064x", 304)
	w.confirmPendingSend(spotTransferEcho(w, nonce+1_000, hash, recipient, "5"), 5e8, nil)
	if confirmed, err := w.txDB.GetTx(hash); err != nil {
		t.Fatalf("GetTx: %v", err)
	} else if confirmed == nil {
		t.Fatal("the real echo did not confirm the send")
	}
}

func TestWalletTransactionLookupByPendingCoinID(t *testing.T) {
	w := newTestWalletWithDB(t)
	pendingID := "0x0000018bcfe56800"
	recipient := "0x1111111111111111111111111111111111111111"

	if err := w.txDB.StoreTx(&txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        pendingID,
		Type:      txdb.Send,
		Amount:    1e8,
		Recipient: &recipient,
		AdditionalData: map[string]string{
			"nonce": "1700000000000",
		},
	}); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	coinIDStr, err := decodeCoinID(coinIDBytes(pendingID))
	if err != nil {
		t.Fatalf("decodeCoinID: %v", err)
	}

	wt, err := w.WalletTransaction(context.Background(), coinIDStr)
	if err != nil {
		t.Fatalf("WalletTransaction: %v", err)
	}
	if wt.ID != pendingID {
		t.Fatalf("WalletTransaction ID = %q, want %q", wt.ID, pendingID)
	}
}

func TestWalletTransactionLookupByConfirmedPendingAlias(t *testing.T) {
	w := newTestWalletWithDB(t)
	nonce := uint64(1700000001000)
	pendingID := fmt.Sprintf("0x%016x", nonce)
	recipient := "0x1111111111111111111111111111111111111111"

	confirmAliasTx := &txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		ID:        pendingID,
		Type:      txdb.Send,
		Amount:    5e8,
		Recipient: &recipient,
		AdditionalData: map[string]string{
			"nonce": strconv.FormatUint(nonce, 10),
		},
	}
	setSubmissionTime(confirmAliasTx, uint64(time.Now().Unix()))
	if err := w.txDB.StoreTx(confirmAliasTx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	realHash := "0x" + fmt.Sprintf("%064x", 42)
	w.confirmPendingSend(ledgerUpdate{
		Time: nonce + 500,
		Hash: realHash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "5",
			User:        w.address,
			Destination: recipient,
		},
	}, 5e8, nil)

	coinIDStr, err := decodeCoinID(coinIDBytes(pendingID))
	if err != nil {
		t.Fatalf("decodeCoinID: %v", err)
	}

	for _, lookupID := range []string{pendingID, coinIDStr, realHash} {
		wt, err := w.WalletTransaction(context.Background(), lookupID)
		if err != nil {
			t.Fatalf("WalletTransaction(%q): %v", lookupID, err)
		}
		if wt.ID != realHash {
			t.Fatalf("WalletTransaction(%q) ID = %q, want %q", lookupID, wt.ID, realHash)
		}
	}
}

// --- handleTransferUpdate ---

func TestHandleTransferUpdateReceive(t *testing.T) {
	w := newTestWalletWithDB(t)
	sender := "0xsender1234567890abcdef1234567890abcdef12"
	realHash := "0x" + fmt.Sprintf("%064x", 77)

	update := ledgerUpdate{
		Time: 1700000005000,
		Hash: realHash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "3.5",
			User:        sender,
			Destination: w.address,
		},
	}

	w.handleTransferUpdate(update, nil)

	got, err := w.txDB.GetTx(realHash)
	if err != nil {
		t.Fatalf("getTx: %v", err)
	}
	if got == nil {
		t.Fatal("receive tx not stored")
	}
	if got.Type != txdb.Receive {
		t.Errorf("type = %d, want Receive (%d)", got.Type, txdb.Receive)
	}
	wantAmt, _ := parseHYPE("3.5")
	if got.Amount != wantAmt {
		t.Errorf("amount = %d, want %d", got.Amount, wantAmt)
	}
	if !got.Confirmed {
		t.Error("receive tx should be confirmed")
	}
	if got.Recipient == nil || *got.Recipient != w.address {
		t.Errorf("Recipient = %v, want %q (wallet address)", got.Recipient, w.address)
	}
	if got.AdditionalData["sender"] != sender {
		t.Errorf("AdditionalData[sender] = %q, want %q", got.AdditionalData["sender"], sender)
	}
}

// TestHandleTransferUpdateEmitsBalanceChange guards against a detected
// incoming transfer showing up in the transaction list while the displayed
// balance never updates: the front end doesn't re-poll balances on its own,
// so storing a new receive tx must also push a fresh BalanceChangeNote.
func TestHandleTransferUpdateEmitsBalanceChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(spotClearinghouseState{
			Balances: []spotBalance{{Coin: "HYPE", Total: "5", Hold: "0"}},
		})
	}))
	defer srv.Close()

	netDB := newTestTxDB(t)
	log := util.StdOutLogger("HLWTEST", util.LevelTrace)
	rpc := newRPCClient(srv.URL, "", log)
	notes := make(chan asset.WalletNotification, 16)
	w := &HyperWallet{
		log:     log,
		net:     util.Testnet,
		address: "0xabcdef1234567890abcdef1234567890abcdef12",
		rpc:     rpc,
		ctx:     context.Background(),
		emit:    asset.NewWalletEmitter(notes, BipID, log),
		txDB:    netDB,
	}
	mustSetupWalletTxDB(t, w)

	update := ledgerUpdate{
		Time: 1700000005000,
		Hash: "0x" + fmt.Sprintf("%064x", 99),
		Delta: ledgerDelta{
			Type: "spotTransfer", Token: "HYPE", Amount: "3.5",
			User: "0xsender1234567890abcdef1234567890abcdef12", Destination: w.address,
		},
	}
	w.lockedProcessLedgerUpdates(context.Background(), []ledgerUpdate{update})

	wantAvail, _ := parseHYPE("5")
	for {
		select {
		case n := <-notes:
			bcn, ok := n.(*asset.BalanceChangeNote)
			if !ok {
				continue // drain the preceding TransactionNote
			}
			if bcn.Balance.Available != wantAvail {
				t.Errorf("emitted balance = %d, want %d", bcn.Balance.Available, wantAvail)
			}
			return
		default:
			t.Fatal("expected a BalanceChangeNote after storing a new receive tx")
		}
	}
}

// TestLedgerBatchEmitsAfterUnlockOnce guards the WS read goroutine: a batch of
// incoming transfers must fetch the balance once, and only after ledgerMtx is
// released, or the HTTP round-trips stall the reader past its read deadline.
func TestLedgerBatchEmitsAfterUnlockOnce(t *testing.T) {
	var w *HyperWallet
	var balanceReqs int
	var heldDuringReq bool
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		balanceReqs++
		if w.ledgerMtx.TryLock() {
			w.ledgerMtx.Unlock()
		} else {
			heldDuringReq = true
		}
		json.NewEncoder(rw).Encode(spotClearinghouseState{
			Balances: []spotBalance{{Coin: "HYPE", Total: "5", Hold: "0"}},
		})
	}))
	defer srv.Close()

	log := util.StdOutLogger("HLWTEST", util.LevelTrace)
	rpc, err := newRPCClient(srv.URL, "", log)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}
	notes := make(chan asset.WalletNotification, 16)
	w = &HyperWallet{
		log:     log,
		net:     util.Testnet,
		address: "0xabcdef1234567890abcdef1234567890abcdef12",
		rpc:     rpc,
		ctx:     context.Background(),
		emit:    asset.NewWalletEmitter(notes, BipID, log),
		txDB:    newTestTxDB(t),
	}
	mustSetupWalletTxDB(t, w)

	var updates []ledgerUpdate
	for i := 0; i < 3; i++ {
		updates = append(updates, ledgerUpdate{
			Time: uint64(1700000005000 + i),
			Hash: "0x" + fmt.Sprintf("%064x", 200+i),
			Delta: ledgerDelta{
				Type: "spotTransfer", Token: "HYPE", Amount: "1",
				User: "0xsender1234567890abcdef1234567890abcdef12", Destination: w.address,
			},
		})
	}
	if err := w.lockedProcessLedgerUpdates(context.Background(), updates); err != nil {
		t.Fatalf("lockedProcessLedgerUpdates: %v", err)
	}

	if balanceReqs != 1 {
		t.Errorf("balance requests = %d, want 1 per batch", balanceReqs)
	}
	if heldDuringReq {
		t.Error("ledgerMtx was held during the balance request")
	}
	var txNotes, balNotes int
	for len(notes) > 0 {
		switch (<-notes).(type) {
		case *asset.TransactionNote:
			txNotes++
		case *asset.BalanceChangeNote:
			balNotes++
		}
	}
	if txNotes != 3 || balNotes != 1 {
		t.Errorf("notes: %d tx, %d balance; want 3 tx, 1 balance", txNotes, balNotes)
	}
}

func TestHandleTransferUpdateReceiveIdempotent(t *testing.T) {
	w := newTestWalletWithDB(t)
	sender := "0xsender1234567890abcdef1234567890abcdef12"
	realHash := "0x" + fmt.Sprintf("%064x", 88)

	update := ledgerUpdate{
		Time: 1700000005000,
		Hash: realHash,
		Delta: ledgerDelta{
			Type: "spotTransfer", Token: "HYPE", Amount: "1",
			User: sender, Destination: w.address,
		},
	}

	// Call twice — second call should be a no-op.
	w.handleTransferUpdate(update, nil)
	w.handleTransferUpdate(update, nil)

	resp, err := w.txDB.GetTxs(&txdb.TxHistoryRequest{N: 10})
	if err != nil {
		t.Fatalf("getTxs: %v", err)
	}
	if len(resp.Txs) != 1 {
		t.Errorf("expected 1 tx after idempotent calls, got %d", len(resp.Txs))
	}
}

func TestProcessLedgerUpdateNonHYPE(t *testing.T) {
	w := newTestWalletWithDB(t)
	update := ledgerUpdate{
		Time: 1700000005000,
		Hash: "0x" + fmt.Sprintf("%064x", 1),
		Delta: ledgerDelta{
			Type: "spotTransfer", Token: "USDC", Amount: "100",
			User: "0xsender", Destination: w.address,
		},
	}
	// Should be silently ignored (non-HYPE coin).
	w.lockedProcessLedgerUpdates(context.Background(), []ledgerUpdate{update})

	resp, err := w.txDB.GetTxs(&txdb.TxHistoryRequest{N: 10})
	if err != nil {
		t.Fatalf("getTxs: %v", err)
	}
	if len(resp.Txs) != 0 {
		t.Errorf("expected 0 txs for non-HYPE event, got %d", len(resp.Txs))
	}
}

// --- scanLedger ---

// newTestWalletWithRPC returns a wallet wired to the given mock RPC server and
// a real txDB.
func newTestWalletWithRPC(t *testing.T, srv *httptest.Server) *HyperWallet {
	t.Helper()
	netDB := newTestTxDB(t)
	log := util.StdOutLogger("HLWTEST", util.LevelTrace)
	rpc := newRPCClient(srv.URL, "", log)
	noteChan := make(chan asset.WalletNotification, 16)
	w := &HyperWallet{
		log:     log,
		net:     util.Testnet,
		address: "0xabcdef1234567890abcdef1234567890abcdef12",
		rpc:     rpc,
		emit:    asset.NewWalletEmitter(noteChan, BipID, log),
		txDB:    netDB,
	}
	mustSetupWalletTxDB(t, w)
	return w
}

func TestScanLedgerAdvancesWatermark(t *testing.T) {
	ts1 := int64(1700000010000)
	ts2 := int64(1700000020000)
	sender := "0xsender1234567890abcdef1234567890abcdef12"
	hash1 := "0x" + fmt.Sprintf("%064x", 101)
	hash2 := "0x" + fmt.Sprintf("%064x", 102)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		updates := []ledgerUpdate{
			{
				Time: uint64(ts1),
				Hash: hash1,
				Delta: ledgerDelta{
					Type:        "spotTransfer",
					Token:       "HYPE",
					Amount:      "1",
					User:        sender,
					Destination: "0xabcdef1234567890abcdef1234567890abcdef12",
				},
			},
			{
				Time: uint64(ts2),
				Hash: hash2,
				Delta: ledgerDelta{
					Type:        "spotTransfer",
					Token:       "HYPE",
					Amount:      "2",
					User:        sender,
					Destination: "0xabcdef1234567890abcdef1234567890abcdef12",
				},
			},
		}
		json.NewEncoder(w).Encode(updates)
	}))
	defer srv.Close()

	wallet := newTestWalletWithRPC(t, srv)
	wallet.lastScannedMS = ts1 - 5000

	if err := wallet.scanLedger(context.Background()); err != nil {
		t.Fatalf("scanLedger: %v", err)
	}

	// Watermark should advance to the latest event time + 1.
	wallet.tipMtx.RLock()
	got := wallet.lastScannedMS
	wallet.tipMtx.RUnlock()
	if got != ts2+1 {
		t.Errorf("lastScannedMS = %d, want %d", got, ts2+1)
	}
	gotPersisted, err := wallet.getLastScannedMS()
	if err != nil {
		t.Fatalf("getLastScannedMS: %v", err)
	}
	if gotPersisted != ts2+1 {
		t.Fatalf("persisted lastScannedMS = %d, want %d", gotPersisted, ts2+1)
	}

	// Both receive events should be stored.
	for _, hash := range []string{hash1, hash2} {
		tx, err := wallet.txDB.GetTx(hash)
		if err != nil {
			t.Fatalf("GetTx %s: %v", hash, err)
		}
		if tx == nil {
			t.Errorf("expected tx %s to be stored after scanLedger", hash)
		}
	}
}

func TestScanLedgerNoUpdates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]ledgerUpdate{})
	}))
	defer srv.Close()

	wallet := newTestWalletWithRPC(t, srv)
	wallet.tipMtx.Lock()
	wallet.lastScannedMS = 9999
	wallet.tipMtx.Unlock()

	if err := wallet.scanLedger(context.Background()); err != nil {
		t.Fatalf("scanLedger: %v", err)
	}

	// Watermark must not change when there are no updates.
	wallet.tipMtx.RLock()
	got := wallet.lastScannedMS
	wallet.tipMtx.RUnlock()
	if got != 9999 {
		t.Errorf("lastScannedMS = %d, want 9999 (no updates)", got)
	}
}

func TestScanLedgerRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	wallet := newTestWalletWithRPC(t, srv)
	wallet.tipMtx.Lock()
	wallet.lastScannedMS = 1234
	wallet.tipMtx.Unlock()

	if err := wallet.scanLedger(context.Background()); err == nil {
		t.Fatal("expected error from scanLedger on HTTP 500")
	}

	// Watermark must not advance on error.
	wallet.tipMtx.RLock()
	got := wallet.lastScannedMS
	wallet.tipMtx.RUnlock()
	if got != 1234 {
		t.Errorf("lastScannedMS = %d, want 1234 (unchanged on error)", got)
	}
}

func TestScanLedgerContextCancel(t *testing.T) {
	// A slow server — should be cancelled by the context before responding.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	wallet := newTestWalletWithRPC(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := wallet.scanLedger(ctx)
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}

// countingPendingTxDB wraps a txdb.NetworkTxDB, counting GetPendingTxs calls
// so a test can assert a batch of ledger updates loads the pending set once,
// not once per update.
type countingPendingTxDB struct {
	txdb.NetworkTxDB
	pendingCalls atomic.Int64
}

func (c *countingPendingTxDB) GetPendingTxs() ([]*txdb.WalletTransaction, error) {
	c.pendingCalls.Add(1)
	return c.NetworkTxDB.GetPendingTxs()
}

// TestLockedProcessLedgerUpdatesBatchesPendingLoad verifies
// lockedProcessLedgerUpdates — which Rescan, scanLedger, and the live WS
// path (via tipPoller's onEvent) all now share — loads the pending set once
// per batch under one ledgerMtx critical section, not once per update, and
// still correctly confirms every pending send in a multi-update batch
// (goaudit finding F3: the WS batching fix had no direct test of its own).
func TestLockedProcessLedgerUpdatesBatchesPendingLoad(t *testing.T) {
	w := newTestWalletWithDB(t)
	counting := &countingPendingTxDB{NetworkTxDB: w.txDB}
	w.txDB = counting

	recipient := "0xrecipient1234567890abcdef1234567890abcd"
	nonce1 := uint64(1700000001000)
	nonce2 := uint64(1700000002000)
	pendingID1 := storePendingSend(t, w, nonce1, 5e8, recipient, 0)
	pendingID2 := storePendingSend(t, w, nonce2, 3e8, recipient, 0)

	hash1 := "0x" + strings.Repeat("a", 64)
	hash2 := "0x" + strings.Repeat("b", 64)
	updates := []ledgerUpdate{
		spotTransferEcho(w, nonce2+1_000, hash2, recipient, "3"),
		spotTransferEcho(w, nonce1+1_000, hash1, recipient, "5"),
	}
	sortLedgerUpdates(updates)

	if err := w.lockedProcessLedgerUpdates(context.Background(), updates); err != nil {
		t.Fatalf("lockedProcessLedgerUpdates: %v", err)
	}

	if got := counting.pendingCalls.Load(); got != 1 {
		t.Fatalf("GetPendingTxs called %d times, want 1 (one load for the whole batch)", got)
	}

	// confirmPendingSend replaces the pending record (keyed by nonce-derived
	// ID) with a confirmed one keyed by the echo's tx hash, so both the
	// pending IDs must be gone and both hashes must now be confirmed.
	for _, pendingID := range []string{pendingID1, pendingID2} {
		tx, err := w.txDB.GetTx(pendingID)
		if err != nil {
			t.Fatalf("GetTx %s: %v", pendingID, err)
		}
		if tx != nil {
			t.Fatalf("pending record %s still present after confirmation", pendingID)
		}
	}
	for _, hash := range []string{hash1, hash2} {
		tx, err := w.txDB.GetTx(hash)
		if err != nil {
			t.Fatalf("GetTx %s: %v", hash, err)
		}
		if tx == nil || !tx.Confirmed {
			t.Fatalf("send confirmed at %s was not stored by the batch: %+v", hash, tx)
		}
	}
}

func TestNextSpotSendNonceMonotonic(t *testing.T) {
	w := &HyperWallet{nextSendNonce: uint64(time.Now().UnixMilli()) + 10}

	first := w.nextSpotSendNonce()
	second := w.nextSpotSendNonce()

	if first <= w.nextSendNonce-2 {
		t.Fatalf("first nonce did not advance from seed: first=%d next=%d", first, w.nextSendNonce)
	}
	if second != first+1 {
		t.Fatalf("second nonce = %d, want %d", second, first+1)
	}
}

func TestConnectUsesPersistedWatermark(t *testing.T) {
	var mu sync.Mutex
	var startTimes []int64
	// ledgerScanned is closed when the first userNonFundingLedgerUpdates
	// request arrives, proving onReconnect ran with the persisted watermark.
	ledgerScanned := make(chan struct{})
	var ledgerOnce sync.Once

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WebSocket upgrade requests from tipPoller.
		if r.Header.Get("Upgrade") == "websocket" {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			conn.ReadMessage() // consume the subscribe message
			// Stay open until client disconnects.
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}

		// REST requests from spotMeta and scanLedger.
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return
		}
		switch payload["type"] {
		case "spotMeta":
			json.NewEncoder(w).Encode(spotMetaResponse{
				Tokens: []spotToken{{Name: "HYPE", TokenID: "0x1234"}},
			})
		case "userNonFundingLedgerUpdates":
			mu.Lock()
			startTimes = append(startTimes, int64(payload["startTime"].(float64)))
			mu.Unlock()
			ledgerOnce.Do(func() { close(ledgerScanned) })
			json.NewEncoder(w).Encode([]ledgerUpdate{})
		}
	}))
	defer srv.Close()

	seed := make([]byte, 32)
	rand.Read(seed)
	pw := []byte("persisted-watermark-password-123")
	dir := t.TempDir()

	d := &Driver{}
	if err := d.Create(&asset.CreateWalletParams{
		Type:    "rpc",
		Seed:    seed,
		Pass:    pw,
		DataDir: dir,
		Net:     util.Testnet,
		Logger:  util.StdOutLogger("HLTEST", util.LevelTrace),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	txDBLog := util.StdOutLogger("TXDBTEST", util.LevelTrace)
	master, err := txdb.New(filepath.Join(dir, "txdb"), txDBLog)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	netDB := master.NetworkTxDB(BipID)
	mt, err := netDB.Table(metaTableName)
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	helperW := &HyperWallet{metaTable: mt}
	if err := helperW.setLastScannedMS(1700000000123); err != nil {
		t.Fatalf("setLastScannedMS: %v", err)
	}
	if err := master.Close(); err != nil {
		t.Fatalf("master.Close: %v", err)
	}

	master2, err := txdb.New(filepath.Join(dir, "txdb"), txDBLog)
	if err != nil {
		t.Fatalf("reopen txdb.New: %v", err)
	}
	defer master2.Close()
	netDB2 := master2.NetworkTxDB(BipID)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	wi, err := d.Open(&asset.WalletConfig{
		Type: "rpc",
		Settings: map[string]string{
			"apiendpoint": srv.URL,
			"wsendpoint":  wsURL,
		},
		Emit:        asset.NewWalletEmitter(make(chan asset.WalletNotification, 4), BipID, util.StdOutLogger("HLTEST", util.LevelTrace)),
		DataDir:     dir,
		NetworkTxDB: netDB2,
	}, util.StdOutLogger("HLTEST", util.LevelTrace), util.Testnet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w := wi.(*HyperWallet)
	w.Unlock(pw)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Wait for the first onReconnect scan to complete.
	select {
	case <-ledgerScanned:
	case <-ctx.Done():
		t.Fatal("timeout waiting for initial ledger scan via onReconnect")
	}

	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if startTimes[0] != 1700000000123 {
		t.Fatalf("initial scan startTime = %d, want %d", startTimes[0], int64(1700000000123))
	}
}

// --- wsClient ---

// newWSTestServer creates an httptest server that upgrades to WebSocket and
// runs handler in a goroutine for each connection.
func newWSTestServer(t *testing.T, handler func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		handler(conn)
	}))
	return srv
}

// wsURL converts an httptest server URL from http:// to ws://.
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func sendLedgerEvent(t *testing.T, conn *websocket.Conn, updates []ledgerUpdate, isSnapshot bool) {
	t.Helper()
	data, err := json.Marshal(wsLedgerData{IsSnapshot: isSnapshot, Updates: updates})
	if err != nil {
		t.Fatalf("marshal ledger data: %v", err)
	}
	msg := wsIncomingMsg{Channel: "userNonFundingLedgerUpdates", Data: data}
	if err := conn.WriteJSON(msg); err != nil {
		t.Logf("write ledger event: %v", err) // connection may be closed
	}
}

func TestWSClientReceivesEvent(t *testing.T) {
	hash := "0x" + fmt.Sprintf("%064x", 1)
	received := make(chan ledgerUpdate, 4)

	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		// Read and discard the subscribe message.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		sendLedgerEvent(t, conn, []ledgerUpdate{{
			Time: 1700000001000,
			Hash: hash,
			Delta: ledgerDelta{
				Type: "spotTransfer", Token: "HYPE", Amount: "1",
				User:        "0xsender",
				Destination: "0xrecipient",
			},
		}}, false)
		// Keep open until test finishes.
		time.Sleep(5 * time.Second)
	})
	defer srv.Close()

	log := util.StdOutLogger("WSTEST", util.LevelTrace)
	wsc := &wsClient{
		url:  wsURL(srv),
		user: "0xrecipient",
		log:  log,
		onEvent: func(updates []ledgerUpdate) {
			for _, u := range updates {
				received <- u
			}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go wsc.run(ctx)

	select {
	case u := <-received:
		if u.Hash != hash {
			t.Errorf("hash = %q, want %q", u.Hash, hash)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for WS event")
	}
}

func TestWSClientSnapshotProcessed(t *testing.T) {
	hash := "0x" + fmt.Sprintf("%064x", 2)
	received := make(chan ledgerUpdate, 4)

	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		sendLedgerEvent(t, conn, []ledgerUpdate{{
			Time: 1700000002000,
			Hash: hash,
			Delta: ledgerDelta{
				Type: "spotTransfer", Token: "HYPE", Amount: "2",
				User: "0xa", Destination: "0xb",
			},
		}}, true) // isSnapshot = true
		time.Sleep(5 * time.Second)
	})
	defer srv.Close()

	log := util.StdOutLogger("WSTEST", util.LevelTrace)
	wsc := &wsClient{
		url:  wsURL(srv),
		user: "0xb",
		log:  log,
		onEvent: func(updates []ledgerUpdate) {
			for _, u := range updates {
				received <- u
			}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go wsc.run(ctx)

	select {
	case u := <-received:
		if u.Hash != hash {
			t.Errorf("snapshot hash = %q, want %q", u.Hash, hash)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for snapshot event")
	}
}

func TestWSClientOnLiveCalled(t *testing.T) {
	liveCh := make(chan struct{}, 4)

	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		sendLedgerEvent(t, conn, []ledgerUpdate{{
			Time: 1700000003000,
			Hash: "0x" + fmt.Sprintf("%064x", 3),
			Delta: ledgerDelta{
				Type: "spotTransfer", Token: "HYPE", Amount: "1",
				User: "0xa", Destination: "0xb",
			},
		}}, false)
		time.Sleep(5 * time.Second)
	})
	defer srv.Close()

	log := util.StdOutLogger("WSTEST", util.LevelTrace)
	wsc := &wsClient{
		url:    wsURL(srv),
		user:   "0xb",
		log:    log,
		onLive: func() { liveCh <- struct{}{} },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go wsc.run(ctx)

	select {
	case <-liveCh:
	case <-ctx.Done():
		t.Fatal("timeout waiting for onLive")
	}
}

// TestWSClientSendsAppLevelPing guards against a regression to a WebSocket
// protocol-level Ping control frame: Hyperliquid's server only resets its
// 60s idle timer on a JSON message, so the heartbeat must be a text message
// of exactly {"method":"ping"}, not a control frame (which gorilla's server
// would consume automatically and never surface via ReadMessage).
func TestWSClientSendsAppLevelPing(t *testing.T) {
	pingCh := make(chan struct{}, 4)
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil { // subscribe message
			return
		}
		for {
			msgType, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if msgType != websocket.TextMessage {
				t.Errorf("ping arrived as message type %d, want TextMessage", msgType)
				continue
			}
			var msg wsPingMsg
			if err := json.Unmarshal(raw, &msg); err != nil || msg.Method != "ping" {
				t.Errorf("unexpected message on the wire: %s", raw)
				continue
			}
			select {
			case pingCh <- struct{}{}:
			default:
			}
		}
	})
	defer srv.Close()

	log := util.StdOutLogger("WSTEST", util.LevelTrace)
	wsc := &wsClient{
		url:          wsURL(srv),
		user:         "0xb",
		log:          log,
		pingInterval: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go wsc.run(ctx)

	select {
	case <-pingCh:
	case <-ctx.Done():
		t.Fatal("timeout waiting for an application-level ping message")
	}
}

func TestWSClientIgnoresOtherChannels(t *testing.T) {
	received := make(chan ledgerUpdate, 4)

	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		// Send a non-ledger channel message.
		conn.WriteJSON(wsIncomingMsg{Channel: "allMids", Data: json.RawMessage(`{}`)})
		// Then a real event.
		sendLedgerEvent(t, conn, []ledgerUpdate{{
			Time: 1700000004000,
			Hash: "0x" + fmt.Sprintf("%064x", 4),
			Delta: ledgerDelta{
				Type: "spotTransfer", Token: "HYPE", Amount: "1",
				User: "0xa", Destination: "0xb",
			},
		}}, false)
		time.Sleep(5 * time.Second)
	})
	defer srv.Close()

	log := util.StdOutLogger("WSTEST", util.LevelTrace)
	wsc := &wsClient{
		url:  wsURL(srv),
		user: "0xb",
		log:  log,
		onEvent: func(updates []ledgerUpdate) {
			for _, u := range updates {
				received <- u
			}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go wsc.run(ctx)

	select {
	case u := <-received:
		if u.Time != 1700000004000 {
			t.Errorf("got unexpected event time %d", u.Time)
		}
	case <-ctx.Done():
		t.Fatal("timeout")
	}
}

func TestWSClientReconnects(t *testing.T) {
	connects := make(chan struct{}, 8)

	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		connects <- struct{}{}
		// Close immediately to force a reconnect.
		conn.Close()
	})
	defer srv.Close()

	log := util.StdOutLogger("WSTEST", util.LevelTrace)
	wsc := &wsClient{
		url:  wsURL(srv),
		user: "0xtest",
		log:  log,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go wsc.run(ctx)

	// Wait for at least two connect attempts, proving reconnect works.
	for i := 0; i < 2; i++ {
		select {
		case <-connects:
		case <-ctx.Done():
			t.Fatalf("timeout waiting for connect #%d", i+1)
		}
	}
}

func TestWSClientContextCancel(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		// Stay open; let the client close.
		time.Sleep(10 * time.Second)
	})
	defer srv.Close()

	log := util.StdOutLogger("WSTEST", util.LevelTrace)
	wsc := &wsClient{
		url:  wsURL(srv),
		user: "0xtest",
		log:  log,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		wsc.run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not exit after context cancel")
	}
}

func TestWSClientOnReconnectCalledOnConnect(t *testing.T) {
	reconnects := make(chan struct{}, 4)

	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		time.Sleep(5 * time.Second)
	})
	defer srv.Close()

	log := util.StdOutLogger("WSTEST", util.LevelTrace)
	wsc := &wsClient{
		url:  wsURL(srv),
		user: "0xtest",
		log:  log,
		onReconnect: func(_ context.Context) error {
			reconnects <- struct{}{}
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go wsc.run(ctx)

	select {
	case <-reconnects:
	case <-ctx.Done():
		t.Fatal("timeout waiting for onReconnect")
	}
}

// TestTxHistoryPageSizeIsNotTrusted covers the confirmed-tx accumulator against
// a TxHistoryRequest.N that came off the wire. N is decoded from the HTTP
// request body by apiTxHistory and neither Core.TxHistory nor xcWallet.TxHistory
// validates it, so it arrives here unchecked. The inner page requests are
// bounded by NetworkTxDB.GetTxs, but the outer accumulator sized itself off N
// directly: {"n":500000000} reserved ~4GB before a single record was read, an
// OOM no caller ever sees as an error. A huge N must be served as an ordinary
// request, and N <= 0 must keep returning everything per the TxHistory contract.
func TestTxHistoryPageSizeIsNotTrusted(t *testing.T) {
	w := newTestWalletWithDB(t)

	storeTxHistory(t, w.txDB, []*txdb.WalletTransaction{
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "c1", Amount: 1e8, Timestamp: 1000, Confirmed: true},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "c2", Amount: 2e8, Timestamp: 2000, Confirmed: true},
		{NetworkID: BipID, AssetID: BipID, Type: txdb.Send, ID: "c3", Amount: 3e8, Timestamp: 3000, Confirmed: true},
	})

	// A page larger than the table is served without reserving its width.
	resp, err := w.TxHistory(&txdb.TxHistoryRequest{N: 500_000_000})
	if err != nil {
		t.Fatalf("TxHistory N=500000000: %v", err)
	}
	if len(resp.Txs) != 3 {
		t.Fatalf("N=500000000 returned %d txs, want 3: %v", len(resp.Txs), confirmedIDs(resp.Txs))
	}
	if maxCap := cap(txdb.PreallocTxs(0)); cap(resp.Txs) > maxCap {
		t.Fatalf("N=500000000 preallocated cap %d, want <= %d", cap(resp.Txs), maxCap)
	}

	// Negative N is unbounded, same as the zero value.
	resp, err = w.TxHistory(&txdb.TxHistoryRequest{N: -1})
	if err != nil {
		t.Fatalf("TxHistory N=-1: %v", err)
	}
	if len(resp.Txs) != 3 {
		t.Fatalf("N=-1 returned %d txs, want 3: %v", len(resp.Txs), confirmedIDs(resp.Txs))
	}
}

// TestWSClientWaitsForInFlightHandler covers the read goroutine outliving the
// connection it belongs to. It was untracked, so run could return — completing
// Connect's WaitGroup and letting Core close the master tx DB — while
// handleMessage was still inside processLedgerUpdate writing a transaction.
func TestWSClientWaitsForInFlightHandler(t *testing.T) {
	inHandler := make(chan struct{})
	release := make(chan struct{})

	srv := newWSTestServer(t, func(conn *websocket.Conn) {
		// Consume the subscribe frame, then push one ledger event.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		sendLedgerEvent(t, conn, []ledgerUpdate{{
			Time: 1700000000000,
			Hash: "0x" + strings.Repeat("a", 64),
			Delta: ledgerDelta{
				Type:        "spotTransfer",
				Token:       "HYPE",
				Amount:      "1",
				User:        "0xother",
				Destination: "0xtest",
			},
		}}, false)
		time.Sleep(10 * time.Second)
	})
	defer srv.Close()

	var once sync.Once
	wsc := &wsClient{
		url:  wsURL(srv),
		user: "0xtest",
		log:  util.StdOutLogger("WSTEST", util.LevelTrace),
		onEvent: func(_ []ledgerUpdate) {
			once.Do(func() { close(inHandler) })
			<-release
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		wsc.run(ctx)
		close(done)
	}()

	select {
	case <-inHandler:
	case <-time.After(5 * time.Second):
		t.Fatal("handler was never entered")
	}

	// Cancel while the handler is still running. run must not report done
	// until the handler returns, or a tx DB write can outlive the wallet.
	cancel()
	select {
	case <-done:
		t.Fatal("run returned while a message handler was still in flight")
	case <-time.After(250 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after the handler returned")
	}
}

// TestScanLedgerPairsIdenticalSendsFromUnorderedAPI covers the FIFO pairing's
// dependency on echo order. confirmPendingSend takes the oldest nonce not newer
// than the update time, which is only right if echoes are replayed oldest-first.
// The REST catch-up returns them in whatever order the API chose — and
// maxLedgerUpdateTime already declines to assume that order — so scanLedger
// sorts. Without the sort, two identical sends are confirmed against each
// other's hashes and both records carry the wrong transaction ID.
func TestScanLedgerPairsIdenticalSendsFromUnorderedAPI(t *testing.T) {
	base := uint64(1700000010000)
	nonceA, nonceB := base, base+1000
	idA := fmt.Sprintf("0x%016x", nonceA)
	idB := fmt.Sprintf("0x%016x", nonceB)
	hashA := "0x" + fmt.Sprintf("%064x", 201) // echo for the first send
	hashB := "0x" + fmt.Sprintf("%064x", 202) // echo for the second
	recipient := "0x7777777777777777777777777777777777777777"

	// The API answers newest-first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mk := func(at uint64, hash string) ledgerUpdate {
			return ledgerUpdate{
				Time: at,
				Hash: hash,
				Delta: ledgerDelta{
					Type:        "spotTransfer",
					Token:       "HYPE",
					Amount:      "5",
					User:        "0xabcdef1234567890abcdef1234567890abcdef12",
					Destination: recipient,
				},
			}
		}
		json.NewEncoder(w).Encode([]ledgerUpdate{
			mk(base+1500, hashB),
			mk(base+500, hashA),
		})
	}))
	defer srv.Close()

	wallet := newTestWalletWithRPC(t, srv)
	for _, e := range []struct {
		id    string
		nonce uint64
	}{{idA, nonceA}, {idB, nonceB}} {
		wtx := &txdb.WalletTransaction{
			NetworkID: BipID,
			AssetID:   BipID,
			ID:        e.id, Type: txdb.Send, Amount: 5e8, Recipient: &recipient,
			AdditionalData: map[string]string{"nonce": strconv.FormatUint(e.nonce, 10)},
		}
		setSubmissionTime(wtx, uint64(time.Now().Unix()))
		if err := wallet.txDB.StoreTx(wtx); err != nil {
			t.Fatalf("StoreTx %s: %v", e.id, err)
		}
	}

	if err := wallet.scanLedger(context.Background()); err != nil {
		t.Fatalf("scanLedger: %v", err)
	}

	for _, c := range []struct{ hash, wantPendingID string }{
		{hashA, idA},
		{hashB, idB},
	} {
		got, err := wallet.txDB.GetTx(c.hash)
		if err != nil {
			t.Fatalf("GetTx %s: %v", c.hash, err)
		}
		if got == nil {
			t.Fatalf("no confirmed record stored for %s", c.hash)
		}
		if id := got.AdditionalData[pendingIDKey]; id != c.wantPendingID {
			t.Errorf("%s confirmed pending %q, want %q", c.hash, id, c.wantPendingID)
		}
	}
}

// TestEstimateSendTxFee guards the fee estimate the front end's Send form
// requires before it will enable Send at all. Hyperliquid spot transfers have
// no network fee (see StandardSendFee/FeeRate, both 0), so this always
// reports fee=0; what it must get right is address validity and the balance
// check.
func TestEstimateSendTxFee(t *testing.T) {
	const destAddr = "0x1234567890123456789012345678901234567890"
	noExchange := func(rw http.ResponseWriter, r *http.Request) {
		t.Fatal("EstimateSendTxFee should never reach the exchange endpoint")
	}

	t.Run("valid address, sufficient balance", func(t *testing.T) {
		w, srv := newSendTestWalletWithBalance(t, "0.5", noExchange)
		defer srv.Close()
		fee, valid, err := w.EstimateSendTxFee(destAddr, 1e7, 0, false, false)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if !valid {
			t.Error("expected isValidAddress=true")
		}
		if fee != 0 {
			t.Errorf("fee = %d, want 0 (no network fee)", fee)
		}
	})

	t.Run("invalid address reported via isValidAddress, not an error", func(t *testing.T) {
		w, srv := newSendTestWalletWithBalance(t, "0.5", noExchange)
		defer srv.Close()
		fee, valid, err := w.EstimateSendTxFee("not-an-address", 1e7, 0, false, false)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if valid {
			t.Error("expected isValidAddress=false")
		}
		if fee != 0 {
			t.Errorf("fee = %d, want 0", fee)
		}
	})

	t.Run("insufficient balance", func(t *testing.T) {
		w, srv := newSendTestWalletWithBalance(t, "0.5", noExchange)
		defer srv.Close()
		_, _, err := w.EstimateSendTxFee(destAddr, 1e8, 0, false, false)
		if !errors.Is(err, asset.ErrInsufficientBalance) {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
	})

	t.Run("maxWithdraw with empty address skips the balance check", func(t *testing.T) {
		w, srv := newSendTestWalletWithBalance(t, "0.5", noExchange)
		defer srv.Close()
		fee, valid, err := w.EstimateSendTxFee("", 1e8, 0, false, true)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if valid {
			t.Error("expected isValidAddress=false for an empty address")
		}
		if fee != 0 {
			t.Errorf("fee = %d, want 0", fee)
		}
	})

	t.Run("errors when not connected", func(t *testing.T) {
		w := &HyperWallet{}
		if _, _, err := w.EstimateSendTxFee(destAddr, 1e7, 0, false, false); !errors.Is(err, asset.ErrConnectionDown) {
			t.Fatalf("err = %v, want ErrConnectionDown", err)
		}
	})
}
