// Package masternode holds the Dash masternode owner functionality.
package masternode

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
)

// DIP-9 feature path: m/9'/coin'/3'/subfeature'/index.
const (
	dip9Purpose      = 9
	dip9FeatureMN    = 3
	subFeatureVoting = 1
	subFeatureOwner  = 2
	hardened         = hdkeychain.HardenedKeyStart
	maxMasternodeIdx = hardened - 1
)

// KeyDeriver derives a private key from an explicit path under the wallet's
// master root. The caller owns the returned key. The wallet's
// Wallet.DeriveFromRootPath satisfies it.
type KeyDeriver interface {
	DeriveFromRootPath(path []uint32) (*btcec.PrivateKey, error)
}

// OwnerKey derives the owner key for masternode n, at m/9'/coin'/3'/2'/n. The
// caller owns the key and should Zero it after use.
func OwnerKey(d KeyDeriver, coinType, n uint32) (*btcec.PrivateKey, error) {
	return deriveKey(d, coinType, subFeatureOwner, n)
}

// VotingKey derives the voting key for masternode n, at m/9'/coin'/3'/1'/n.
// The caller owns the key and should Zero it after use.
func VotingKey(d KeyDeriver, coinType, n uint32) (*btcec.PrivateKey, error) {
	return deriveKey(d, coinType, subFeatureVoting, n)
}

func deriveKey(d KeyDeriver, coinType, subFeature, n uint32) (*btcec.PrivateKey, error) {
	// Index n is a non-hardened child. A hardened one would silently derive
	// a different key family.
	if n > maxMasternodeIdx {
		return nil, fmt.Errorf("masternode index %d out of range", n)
	}
	if coinType > maxMasternodeIdx {
		return nil, fmt.Errorf("coin type %d out of range", coinType)
	}
	path := []uint32{
		hardened + dip9Purpose,
		hardened + coinType,
		hardened + dip9FeatureMN,
		hardened + subFeature,
		n,
	}
	return d.DeriveFromRootPath(path)
}
