// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"encoding/binary"
	"encoding/hex"

	"github.com/dgraph-io/badger/v4"
)

const (
	PrefixSize = 2
	// indexNameSeparator joins a table name and an index name into the
	// registered name an index's key prefix is stored under.
	indexNameSeparator = "__idx__"
)

// keyPrefix is a prefix for a key in the badger DB. Every table and index has
// a unique keyPrefix. This enables sorting and iteration of data.
type keyPrefix [PrefixSize]byte

func (p keyPrefix) String() string {
	return hex.EncodeToString(p[:])
}

// NO RAW BADGER KEYS CAN BE LENGTH 2. IT CAN'T JUST BE A PREFIX, OR ELSE
// REVERSE ITERATION OF INDEXES FAILS

var (
	// reserved prefixes
	prefixToNamePrefix    = keyPrefix{0x00, 0x00}
	nameToPrefixPrefix    = keyPrefix{0x00, 0x01}
	primarySequencePrefix = keyPrefix{0x00, 0x02}
	// IDToKeyPrefix is the range of the KeyID→key map. Entries are keyed by
	// the table prefix followed by the row's KeyID, and store the row's bare
	// user key. Index entries reference rows by KeyID, and this map is how an
	// index iteration resolves the entry back to the row.
	IDToKeyPrefix = keyPrefix{0x00, 0x03}

	firstAvailablePrefix = keyPrefix{0x01, 0x00}
)

func incrementPrefix(prefix keyPrefix) (p keyPrefix) {
	v := binary.BigEndian.Uint16(prefix[:])
	binary.BigEndian.PutUint16(p[:], v+1)
	return p
}

func bytesToPrefix(b []byte) (p keyPrefix) {
	copy(p[:], b)
	return
}

func lastKeyForPrefix(txn *badger.Txn, p keyPrefix) (k []byte) {
	reverseIteratePrefix(txn, p[:], nil, func(iter *badger.Iterator) error {
		k = iter.Item().KeyCopy(nil)[PrefixSize:]
		return ErrEndIteration
	}, withPrefetchSize(1))
	return
}

// PrefixedKey returns a new byte slice with the prefix prepended to the key.
func PrefixedKey(p keyPrefix, k []byte) []byte {
	pk := make([]byte, PrefixSize+len(k))
	copy(pk, p[:])
	copy(pk[PrefixSize:], k)
	return pk
}

// CloneBytes returns a copy of the given byte slice. If the input is nil or empty, it returns nil.
func CloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
