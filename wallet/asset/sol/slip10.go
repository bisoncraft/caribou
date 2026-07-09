// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"fmt"

	"github.com/bisoncraft/caribou/util/encode"
	"github.com/decred/dcrd/hdkeychain/v3"
)

var seedDerivationPath = []uint32{
	hdkeychain.HardenedKeyStart + 44,
	hdkeychain.HardenedKeyStart + 501,
	hdkeychain.HardenedKeyStart + 0,
	hdkeychain.HardenedKeyStart + 0,
}

func privKeyFromSeed(seed []byte) (ed25519.PrivateKey, func(), error) {
	mac := hmac.New(sha512.New, []byte("ed25519 seed"))
	mac.Write(seed)
	sum := mac.Sum(nil)
	key := append([]byte(nil), sum[:32]...)
	chainCode := append([]byte(nil), sum[32:]...)

	for _, child := range seedDerivationPath {
		if child < hdkeychain.HardenedKeyStart {
			encode.ClearBytes(key)
			encode.ClearBytes(chainCode)
			return nil, nil, fmt.Errorf("non-hardened child %d not supported for ed25519", child)
		}
		data := make([]byte, 1+32+4)
		data[0] = 0
		copy(data[1:33], key)
		binary.BigEndian.PutUint32(data[33:], child)
		mac = hmac.New(sha512.New, chainCode)
		mac.Write(data)
		sum = mac.Sum(nil)
		encode.ClearBytes(key)
		encode.ClearBytes(chainCode)
		key = append([]byte(nil), sum[:32]...)
		chainCode = append([]byte(nil), sum[32:]...)
	}

	privKey := ed25519.NewKeyFromSeed(key)
	zero := func() {
		encode.ClearBytes(key)
		encode.ClearBytes(chainCode)
	}
	return privKey, zero, nil
}
