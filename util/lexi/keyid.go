// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"encoding"
	"encoding/hex"
)

// KeyIDSize is the size of the KeyID. It is 8 bytes to match the size of a
// byte-encoded uint64.
const KeyIDSize = 8

// KeyID is a row-unique ID assigned to a datum when it is first inserted and
// stored in the datum itself. Keys can be any length, but to prevent long keys
// from being echoed in all the indexes, index entries reference the row by its
// KeyID.
type KeyID [KeyIDSize]byte

var (
	_ encoding.BinaryMarshaler = KeyID{}

	lastKeyID = KeyID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
)

// MarshalBinary satisfies encoding.BinaryMarshaler for the KeyID.
func (id KeyID) MarshalBinary() ([]byte, error) {
	return id[:], nil
}

// String encodes the KeyID as a 16-character hexadecimal string.
func (id KeyID) String() string {
	return hex.EncodeToString(id[:])
}

func newKeyIDFromBytes(b []byte) (id KeyID) {
	copy(id[:], b)
	return id
}
