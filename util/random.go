// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package util

import (
	crand "crypto/rand"
	"encoding/hex"
)

// RandomHex returns n cryptographically random bytes encoded as a hex string
// (2n characters). The intermediate buffer is zeroed before returning.
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	s := hex.EncodeToString(b)
	for i := range b {
		b[i] = 0
	}
	return s, nil
}
