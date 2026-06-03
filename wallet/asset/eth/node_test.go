// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"crypto/ecdsa"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/crypto"
)

var testKeyStorePW = []byte("password123")

func newTestKeyStore(t *testing.T) *keystore.KeyStore {
	t.Helper()
	return keystore.NewKeyStore(t.TempDir(), keystore.LightScryptN, keystore.LightScryptP)
}

func genKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	priv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestImportKeyToKeyStore(t *testing.T) {
	t.Parallel()
	t.Run("import into empty keystore", func(t *testing.T) {
		t.Parallel()
		ks := newTestKeyStore(t)
		if err := importKeyToKeyStore(ks, genKey(t), testKeyStorePW); err != nil {
			t.Fatal(err)
		}
		if n := len(ks.Accounts()); n != 1 {
			t.Fatalf("accounts = %d, want 1", n)
		}
	})

	t.Run("re-import matching key is a no-op", func(t *testing.T) {
		t.Parallel()
		ks := newTestKeyStore(t)
		priv := genKey(t)
		if err := importKeyToKeyStore(ks, priv, testKeyStorePW); err != nil {
			t.Fatal(err)
		}
		// Same key again: the address matches the single existing account, so
		// it must return nil without a second import.
		if err := importKeyToKeyStore(ks, priv, testKeyStorePW); err != nil {
			t.Fatalf("re-import errored: %v", err)
		}
		if n := len(ks.Accounts()); n != 1 {
			t.Fatalf("accounts = %d, want 1", n)
		}
	})

	t.Run("mismatched key errors", func(t *testing.T) {
		t.Parallel()
		ks := newTestKeyStore(t)
		if err := importKeyToKeyStore(ks, genKey(t), testKeyStorePW); err != nil {
			t.Fatal(err)
		}
		if err := importKeyToKeyStore(ks, genKey(t), testKeyStorePW); err == nil {
			t.Error("expected error importing a different key into a populated keystore")
		}
	})

	t.Run("multiple accounts errors", func(t *testing.T) {
		t.Parallel()
		ks := newTestKeyStore(t)
		// Seed two accounts directly so importKeyToKeyStore sees len > 1.
		if _, err := ks.ImportECDSA(genKey(t), string(testKeyStorePW)); err != nil {
			t.Fatal(err)
		}
		if _, err := ks.ImportECDSA(genKey(t), string(testKeyStorePW)); err != nil {
			t.Fatal(err)
		}
		if err := importKeyToKeyStore(ks, genKey(t), testKeyStorePW); err == nil {
			t.Error("expected error for keystore with multiple accounts")
		}
	})
}

func TestCredentialsFromKeyStore(t *testing.T) {
	t.Parallel()
	t.Run("single account", func(t *testing.T) {
		t.Parallel()
		ks := newTestKeyStore(t)
		priv := genKey(t)
		if err := importKeyToKeyStore(ks, priv, testKeyStorePW); err != nil {
			t.Fatal(err)
		}
		creds, err := credentialsFromKeyStore(ks)
		if err != nil {
			t.Fatal(err)
		}
		if want := crypto.PubkeyToAddress(priv.PublicKey); creds.addr != want {
			t.Errorf("addr = %s, want %s", creds.addr, want)
		}
		if creds.ks != ks || creds.acct == nil || creds.wallet == nil {
			t.Error("incomplete credentials")
		}
	})

	t.Run("no accounts errors", func(t *testing.T) {
		t.Parallel()
		if _, err := credentialsFromKeyStore(newTestKeyStore(t)); err == nil {
			t.Error("expected error for empty keystore")
		}
	})

	t.Run("multiple accounts errors", func(t *testing.T) {
		t.Parallel()
		ks := newTestKeyStore(t)
		if _, err := ks.ImportECDSA(genKey(t), string(testKeyStorePW)); err != nil {
			t.Fatal(err)
		}
		if _, err := ks.ImportECDSA(genKey(t), string(testKeyStorePW)); err != nil {
			t.Fatal(err)
		}
		if _, err := credentialsFromKeyStore(ks); err == nil {
			t.Error("expected error for keystore with multiple accounts")
		}
	})
}

func TestPathCredentials(t *testing.T) {
	t.Parallel()
	t.Run("empty dir errors", func(t *testing.T) {
		t.Parallel()
		if _, err := pathCredentials(t.TempDir()); err == nil {
			t.Error("expected error for directory with no accounts")
		}
	})

	t.Run("dir with one key", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// pathCredentials opens the dir with LightScrypt params, so seed it with
		// a keystore using the same params.
		ks := keystore.NewKeyStore(dir, keystore.LightScryptN, keystore.LightScryptP)
		priv := genKey(t)
		if err := importKeyToKeyStore(ks, priv, testKeyStorePW); err != nil {
			t.Fatal(err)
		}
		creds, err := pathCredentials(dir)
		if err != nil {
			t.Fatal(err)
		}
		if want := crypto.PubkeyToAddress(priv.PublicKey); creds.addr != want {
			t.Errorf("addr = %s, want %s", creds.addr, want)
		}
	})
}

func TestSignHash(t *testing.T) {
	t.Parallel()
	ks := newTestKeyStore(t)
	priv := genKey(t)
	if err := importKeyToKeyStore(ks, priv, testKeyStorePW); err != nil {
		t.Fatal(err)
	}
	creds, err := credentialsFromKeyStore(ks)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Unlock(*creds.acct, string(testKeyStorePW)); err != nil {
		t.Fatal(err)
	}

	hash := crypto.Keccak256([]byte("redeem this swap"))
	sig, pub, err := signHash(creds, hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 65 {
		t.Fatalf("sig length = %d, want 65", len(sig))
	}
	// signHash bumps the recovery id into Ethereum's 27/28 range.
	if v := sig[64]; v != 27 && v != 28 {
		t.Errorf("recovery id = %d, want 27 or 28", v)
	}

	// The recovered pubkey must belong to the signing account.
	ecdsaPub, err := crypto.UnmarshalPubkey(pub)
	if err != nil {
		t.Fatalf("returned pubkey is not unmarshalable: %v", err)
	}
	if got := crypto.PubkeyToAddress(*ecdsaPub); got != creds.addr {
		t.Errorf("recovered address = %s, want %s", got, creds.addr)
	}
}
