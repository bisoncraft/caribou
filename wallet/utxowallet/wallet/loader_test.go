package wallet

import (
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
)

// TestCreateNewWalletFailureLeavesNoDatabase checks that a wallet creation
// failing after the database file is made removes that file again. WalletExists
// keys on the file, so a leftover would report as an existing wallet and block
// every later creation with ErrExists.
func TestCreateNewWalletFailureLeavesNoDatabase(t *testing.T) {
	netParams := assets.BTCParams["testnet"]
	loader := NewLoader(netParams, t.TempDir(), true, defaultDBTimeout, 250)
	pubPass, privPass := []byte("public"), []byte("private")

	seed := make([]byte, hdkeychain.RecommendedSeedLen)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	rootKey, err := hdkeychain.NewMaster(seed, netParams.BTCDParams())
	if err != nil {
		t.Fatalf("NewMaster error: %v", err)
	}

	pubKey, err := rootKey.Neuter()
	if err != nil {
		t.Fatalf("Neuter error: %v", err)
	}

	// Wallet initialization rejects a public root key for a wallet that is not
	// watching-only, after the database file has been created.
	if _, err = loader.CreateNewWalletExtendedKey(pubPass, privPass, pubKey, time.Now()); err == nil {
		t.Fatal("no error creating a wallet from a public root key")
	}
	exists, err := loader.WalletExists()
	if err != nil {
		t.Fatalf("WalletExists error: %v", err)
	}
	if exists {
		t.Fatal("the database of a failed creation survived as an existing wallet")
	}

	// The same loader creates the wallet once given a private root key.
	if _, err = loader.CreateNewWalletExtendedKey(pubPass, privPass, rootKey, time.Now()); err != nil {
		t.Fatalf("error creating wallet after a failed attempt: %v", err)
	}
	if err = loader.UnloadWallet(); err != nil {
		t.Fatalf("UnloadWallet error: %v", err)
	}
	if exists, err = loader.WalletExists(); err != nil || !exists {
		t.Fatalf("wallet missing after successful creation: exists = %v, err = %v", exists, err)
	}
}
