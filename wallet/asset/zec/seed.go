// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset/keystore"
	"github.com/bisoncraft/go-bip39"
)

// seedFileName is the file of the encrypted seed in the wallet's data
// directory. Its presence marks a complete wallet.
const seedFileName = "seed.json"

// entropyLen is the length of the seed core gives the wallet: BIP 39
// entropy for a 24-word phrase.
const entropyLen = 32

// seedStore keeps the wallet's seed: the 32 bytes of BIP 39 entropy core
// gives the wallet, encrypted under the wallet password, beside the ZIP 32
// seed fingerprint of the account. Where the seed is kept is decided here
// alone.
type seedStore struct {
	path string
}

func newSeedStore(dataDir string) seedStore {
	return seedStore{path: filepath.Join(dataDir, seedFileName)}
}

// exists reports whether the seed has been saved.
func (s seedStore) exists() (bool, error) {
	_, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// save encrypts the entropy under pw and writes it, with the account's seed
// fingerprint in the clear.
func (s seedStore) save(entropy, fingerprint, pw []byte) error {
	if len(entropy) != entropyLen {
		return fmt.Errorf("the seed is %d bytes, want %d", len(entropy), entropyLen)
	}
	return keystore.Save(s.path, entropy, fingerprint, pw)
}

// load decrypts the entropy. The caller erases it.
//
// A file that is missing, cannot be read or does not parse fails with
// errSeedFile, the file system's or the parser's error kept in the chain,
// so errors.Is finds fs.ErrNotExist for a missing file. The file is read and
// parsed once for that check (keystore.PubKey) and again by keystore.Open,
// which does not tell its failures apart. A failure of Open after the check
// passed is errSeedDecrypt, unless the file system's error says the file
// went away in between: a wrong password, or a secret that is damaged,
// absent or too short to decrypt. A decrypted seed of the wrong length is a
// damaged file.
func (s seedStore) load(pw []byte) ([]byte, error) {
	if _, err := keystore.PubKey(s.path); err != nil {
		return nil, fmt.Errorf("%w: %w", errSeedFile, err)
	}
	entropy, err := keystore.Open(s.path, pw)
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return nil, fmt.Errorf("%w: %w", errSeedFile, err)
		}
		return nil, fmt.Errorf("%w: %v", errSeedDecrypt, err)
	}
	if len(entropy) != entropyLen {
		encode.ClearBytes(entropy)
		return nil, fmt.Errorf("%w: the stored seed is %d bytes, want %d", errSeedFile, len(entropy), entropyLen)
	}
	return entropy, nil
}

// fingerprint returns the account's ZIP 32 seed fingerprint, stored in the
// clear.
func (s seedStore) fingerprint() ([]byte, error) {
	return keystore.PubKey(s.path)
}

// zip32Seed derives the ZIP 32 seed of an account from the entropy: the
// 24-word BIP 39 phrase of the entropy, stretched with an empty passphrase
// into a 64-byte seed. Zcash wallets that restore a 24-word phrase derive
// the same seed.
//
// The returned seed is the caller's to erase. The phrase is a Go string,
// which cannot be erased, and the BIP 39 library makes copies of its own
// that are not erased either.
func zip32Seed(entropy []byte) ([]byte, error) {
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, fmt.Errorf("making the seed phrase: %w", err)
	}
	return bip39.NewSeed(mnemonic, ""), nil
}

// mnemonicFor returns the 24-word phrase of the entropy.
func mnemonicFor(entropy []byte) (string, error) {
	return bip39.NewMnemonic(entropy)
}
