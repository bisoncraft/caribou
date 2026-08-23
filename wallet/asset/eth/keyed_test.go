//go:build !harness && !rpclive

package eth

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/crypto"
)

// keystoreAddress opens the keystore CreateEVMWalletKeyed/CreateEVMWallet
// wrote under dataDir for net and returns the single account's address.
func keystoreAddress(t *testing.T, dataDir string, net util.Network) string {
	t.Helper()
	ks := keystore.NewKeyStore(filepath.Join(getWalletDir(dataDir, net), "keystore"),
		keystore.LightScryptN, keystore.LightScryptP)
	accts := ks.Accounts()
	if len(accts) != 1 {
		t.Fatalf("expected 1 account in keystore, got %d", len(accts))
	}
	return accts[0].Address.Hex()
}

// TestCreateEVMWalletKeyedUsesProvidedKeyFn guards the mechanism a chain like
// hyperevm depends on to recover funds at a shared address: the keystore
// CreateEVMWalletKeyed writes must hold the key keyFn derived, not the
// package's own default BIP-39-based derivation.
func TestCreateEVMWalletKeyedUsesProvidedKeyFn(t *testing.T) {
	t.Parallel()

	stubPriv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	stubRaw := crypto.FromECDSA(stubPriv)
	wantAddr := crypto.PubkeyToAddress(stubPriv.PublicKey).Hex()

	keyFn := func([]byte) ([]byte, func(), error) {
		raw := make([]byte, len(stubRaw))
		copy(raw, stubRaw)
		return raw, func() { encode.ClearBytes(raw) }, nil
	}

	tmpDir := t.TempDir()
	err = CreateEVMWalletKeyed(ChainIDs[util.Testnet], &asset.CreateWalletParams{
		Type:     walletTypeRPC,
		Seed:     encode.RandomBytes(32), // must be ignored: keyFn doesn't use it
		Pass:     encode.RandomBytes(32),
		Settings: map[string]string{providersKey: "a.ipc"},
		DataDir:  tmpDir,
		Net:      util.Testnet,
		Logger:   tLogger,
	}, &testnetCompatibilityData, true, keyFn)
	if err != nil {
		t.Fatalf("CreateEVMWalletKeyed error: %v", err)
	}

	gotAddr := keystoreAddress(t, tmpDir, util.Testnet)
	if gotAddr != wantAddr {
		t.Fatalf("keystore address = %s, want %s (from keyFn)", gotAddr, wantAddr)
	}
}

// TestCreateEVMWalletUsesDefaultDerivation guards that CreateEVMWallet's
// refactor into a CreateEVMWalletKeyed(..., privKeyFromSeed) wrapper preserves
// today's behavior byte-for-byte: the keystore it writes must hold the same
// key the package's own privKeyFromSeed derives for that seed.
func TestCreateEVMWalletUsesDefaultDerivation(t *testing.T) {
	t.Parallel()

	seed := encode.RandomBytes(32)
	wantRaw, zero, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("privKeyFromSeed: %v", err)
	}
	defer zero()
	wantPriv, err := crypto.ToECDSA(wantRaw)
	if err != nil {
		t.Fatalf("ToECDSA: %v", err)
	}
	wantAddr := crypto.PubkeyToAddress(wantPriv.PublicKey).Hex()

	tmpDir := t.TempDir()
	err = CreateEVMWallet(ChainIDs[util.Testnet], &asset.CreateWalletParams{
		Type:     walletTypeRPC,
		Seed:     seed,
		Pass:     encode.RandomBytes(32),
		Settings: map[string]string{providersKey: "a.ipc"},
		DataDir:  tmpDir,
		Net:      util.Testnet,
		Logger:   tLogger,
	}, &testnetCompatibilityData, true)
	if err != nil {
		t.Fatalf("CreateEVMWallet error: %v", err)
	}

	gotAddr := keystoreAddress(t, tmpDir, util.Testnet)
	if gotAddr != wantAddr {
		t.Fatalf("keystore address = %s, want %s (from privKeyFromSeed)", gotAddr, wantAddr)
	}
}

// TestRestorationInfoUsesOverriddenKey guards that a wallet configured with a
// non-default PrivKeyFromSeed (e.g. hyperevm sharing hyperliquid's key) hands
// out that key for external-wallet import, not the package default — handing
// out the wrong key here would point the user at the wrong account.
func TestRestorationInfoUsesOverriddenKey(t *testing.T) {
	t.Parallel()

	stubPriv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	stubRaw := crypto.FromECDSA(stubPriv)
	wantHex := hex.EncodeToString(stubRaw)

	w := &ETHWallet{
		assetWallet: &assetWallet{
			baseWallet: &baseWallet{
				privKeyFn: func([]byte) ([]byte, func(), error) {
					raw := make([]byte, len(stubRaw))
					copy(raw, stubRaw)
					return raw, func() { encode.ClearBytes(raw) }, nil
				},
			},
		},
	}

	restorations, err := w.RestorationInfo(encode.RandomBytes(32))
	if err != nil {
		t.Fatalf("RestorationInfo error: %v", err)
	}
	if len(restorations) != 1 {
		t.Fatalf("expected 1 restoration, got %d", len(restorations))
	}
	if restorations[0].Seed != wantHex {
		t.Fatalf("RestorationInfo returned key %s, want %s (from the overridden privKeyFn)", restorations[0].Seed, wantHex)
	}
}
