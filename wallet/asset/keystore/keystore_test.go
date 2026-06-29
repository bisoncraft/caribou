// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package keystore

import (
	"bytes"
	"crypto/rand"
	"path/filepath"
	"testing"
)

func mustRead(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyfile.json")
	secret := mustRead(t, 32)
	pubKey := mustRead(t, 33)
	pw := mustRead(t, 32)

	if err := Save(path, secret, pubKey, pw); err != nil {
		t.Fatalf("Save: %v", err)
	}

	gotPub, err := PubKey(path)
	if err != nil {
		t.Fatalf("PubKey: %v", err)
	}
	if !bytes.Equal(gotPub, pubKey) {
		t.Fatalf("pubkey mismatch: got %x want %x", gotPub, pubKey)
	}

	gotSecret, err := Open(path, pw)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(gotSecret, secret) {
		t.Fatalf("secret mismatch: got %x want %x", gotSecret, secret)
	}
}

func TestWrongPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyfile.json")
	secret := mustRead(t, 32)
	pw := mustRead(t, 32)
	if err := Save(path, secret, mustRead(t, 33), pw); err != nil {
		t.Fatalf("Save: %v", err)
	}

	wrong := mustRead(t, 32)
	if _, err := Open(path, wrong); err == nil {
		t.Fatal("expected error decrypting with wrong password")
	}
}

func TestOpenMissingFile(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope.json"), mustRead(t, 32)); err == nil {
		t.Fatal("expected error opening missing file")
	}
}

func TestCiphertextTooShort(t *testing.T) {
	if _, err := decryptSecret([]byte{0x00, 0x01}, mustRead(t, 32)); err == nil {
		t.Fatal("expected error for short ciphertext")
	}
}

func TestSaveShortPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyfile.json")
	if err := Save(path, mustRead(t, 32), mustRead(t, 33), mustRead(t, 31)); err == nil {
		t.Fatal("expected error saving with short password")
	}
}

func TestPubKeyMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyfile.json")
	if err := Save(path, mustRead(t, 32), nil, mustRead(t, 32)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := PubKey(path); err == nil {
		t.Fatal("expected error reading key file with missing public key")
	}
}
