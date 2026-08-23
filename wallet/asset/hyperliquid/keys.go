// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"github.com/bisoncraft/caribou/util/encode"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// PrivKeyFromSeed derives the raw secp256k1 private key bytes for a HyperCore
// wallet seed. Exported for the hyperevm wallet, which must control the same
// address as this one: HyperCore and HyperEVM share one secp256k1 address per
// account, so duplicating the derivation there would risk silent divergence
// and unreachable funds. Matches the eth package's PrivKeyFromSeed-style
// signature (raw private key bytes, a zeroing func, error) so it can be
// plugged directly into eth.EVMWalletConfig.PrivKeyFromSeed.
func PrivKeyFromSeed(seed []byte) ([]byte, func(), error) {
	priv, _, zeroOrig, err := privKeyFromSeed(seed)
	if err != nil {
		return nil, nil, err
	}
	raw := gethcrypto.FromECDSA(priv)
	zero := func() {
		encode.ClearBytes(raw)
		zeroOrig()
	}
	return raw, zero, nil
}
