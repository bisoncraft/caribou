// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package evmkey derives the secp256k1 keys behind Caribou's EVM wallets
// from an asset seed. It registers no asset driver, so a package that needs
// an EVM wallet's key without the wallet (RHEA lending identities) can
// import it without pulling in the eth wallet.
package evmkey

import (
	"fmt"

	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/keygen"
	"github.com/bisoncraft/go-bip39"
	"github.com/decred/dcrd/hdkeychain/v3"
)

// WalletSeed uses the asset seed as the entropy of a BIP-39 mnemonic and
// returns the mnemonic's seed (empty passphrase), from which the HD keys
// are derived.
func WalletSeed(entropy []byte) ([]byte, error) {
	if len(entropy) < 32 || len(entropy) > 64 {
		return nil, fmt.Errorf("wallet entropy must be 32 to 64 bytes long")
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, fmt.Errorf("error deriving mnemonic: %w", err)
	}
	return bip39.NewSeed(mnemonic, ""), nil
}

// DerivationPath is the BIP-44 path m/44'/60'/account'/0/0.
func DerivationPath(account uint32) []uint32 {
	return []uint32{
		hdkeychain.HardenedKeyStart + 44,      // purpose 44' for HD wallets
		hdkeychain.HardenedKeyStart + 60,      // eth coin type 60'
		hdkeychain.HardenedKeyStart + account, // account'
		0,                                     // branch 0
		0,                                     // index 0
	}
}

// PrivKeyFromSeedAccount derives the raw secp256k1 private key at
// m/44'/60'/account'/0/0 of WalletSeed(seed). Account 0 is the key, and
// therefore the address, behind every EVM wallet; callers that must share
// that identity derive through this function rather than re-implementing
// the path so the two cannot drift. pk aliases the derived extended key's
// storage, and the returned function zeroes both; pk must not be used after
// calling it.
func PrivKeyFromSeedAccount(seed []byte, account uint32) (pk []byte, zero func(), err error) {
	if account >= hdkeychain.HardenedKeyStart {
		return nil, nil, fmt.Errorf("account %d out of range", account)
	}
	walletSeed, err := WalletSeed(seed)
	if err != nil {
		return nil, nil, err
	}
	defer encode.ClearBytes(walletSeed)

	extKey, err := keygen.GenDeepChild(walletSeed, DerivationPath(account))
	if err != nil {
		return nil, nil, err
	}
	pk, err = extKey.SerializedPrivKey()
	if err != nil {
		extKey.Zero()
		return nil, nil, err
	}
	return pk, extKey.Zero, nil
}
