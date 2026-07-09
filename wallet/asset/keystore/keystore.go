// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package keystore provides at-rest encryption of a native wallet's secret
// (seed or private key) for the seeded asset wallets.
//
// The "password" supplied by core for a seeded wallet is not a human password.
// It is a high-entropy 32-byte value derived from the application seed (see
// core.AssetSeedAndPass), so a memory-hard KDF would add no security while
// costing memory that matters on mobile. The secret is therefore encrypted
// directly under AES-256-GCM with a key fixed at sha256(pw). The human password
// protection lives one layer up, in core, where the application seed is
// encrypted with a real argon2id KDF.
package keystore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
)

// keyFile is the on-disk JSON format. PubKey is stored in the clear so the
// address can be derived while the wallet is locked. Secret is the encrypted
// seed or private key.
type keyFile struct {
	PubKey util.Bytes `json:"pubKey"`
	Secret util.Bytes `json:"secret"` // nonce || AES-256-GCM ciphertext+tag
}

// Save encrypts secret under pw and atomically writes a key file at path,
// storing pubKey alongside it for later verification. pw is the 32-byte value
// derived by core; secret is the wallet seed or private key bytes.
func Save(path string, secret, pubKey, pw []byte) error {
	if len(pw) < 32 {
		return errors.New("keystore: password must be at least 32 bytes")
	}
	enc, err := encryptSecret(secret, pw)
	if err != nil {
		return fmt.Errorf("encrypting secret: %w", err)
	}
	b, err := json.Marshal(keyFile{PubKey: pubKey, Secret: enc})
	if err != nil {
		return fmt.Errorf("marshaling key file: %w", err)
	}
	return writeFileAtomic(path, b)
}

// PubKey reads the stored public key without decrypting the secret.
func PubKey(path string) ([]byte, error) {
	kf, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	if len(kf.PubKey) == 0 {
		return nil, errors.New("keystore: key file missing public key")
	}
	return kf.PubKey, nil
}

// Open reads the key file at path and decrypts the secret with pw.
func Open(path string, pw []byte) ([]byte, error) {
	kf, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	return decryptSecret(kf.Secret, pw)
}

func readKeyFile(path string) (*keyFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kf keyFile
	if err := json.Unmarshal(b, &kf); err != nil {
		return nil, fmt.Errorf("parsing key file: %w", err)
	}
	return &kf, nil
}

// encryptSecret encrypts secret with AES-256-GCM under sha256(pw). Output is
// nonce || ciphertext+tag.
func encryptSecret(secret, pw []byte) ([]byte, error) {
	key := sha256.Sum256(pw)
	defer encode.ClearBytes(key[:])
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, secret, nil), nil
}

// decryptSecret reverses encryptSecret.
func decryptSecret(blob, pw []byte) ([]byte, error) {
	key := sha256.Sum256(pw)
	defer encode.ClearBytes(key[:])
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("ciphertext too short")
	}
	secret, err := gcm.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed (wrong password?): %w", err)
	}
	return secret, nil
}

func newGCM(key [32]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// writeFileAtomic writes data to path via a temp file and rename so a crash
// mid-write cannot corrupt an existing key file. The file is mode 0600.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keyfile-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp key file: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod temp key file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing temp key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("syncing temp key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing temp key file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp key file: %w", err)
	}
	return syncDir(filepath.Dir(path))
}
