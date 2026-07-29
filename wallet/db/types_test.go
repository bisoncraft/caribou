// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db_test

import (
	"testing"

	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/db"
)

// TestDecodeNotificationShortTimestamp asserts that a note record whose
// timestamp push is not the expected 8 bytes is rejected as an error.
//
// The timestamp is read with a fixed-width big-endian decoder, so a short push
// is an index-out-of-range panic rather than a decode error. This matters
// beyond reads: the notes table's stamp index derives its entry by decoding the
// record on every write, so a single malformed note would panic the writer
// instead of failing its Set.
func TestDecodeNotificationShortTimestamp(t *testing.T) {
	note := func(stamp []byte) []byte {
		return encode.BuildyBytes{0}.
			AddData([]byte("notetype")).
			AddData([]byte("subject")).
			AddData([]byte("detail")).
			AddData([]byte{byte(db.Success)}).
			AddData(stamp).
			AddData([]byte("topic"))
	}

	// The well-formed record decodes, so the cases below fail on the timestamp
	// width and nothing else.
	if _, err := db.DecodeNotification(note(make([]byte, 8))); err != nil {
		t.Fatalf("well-formed notification failed to decode: %v", err)
	}

	for _, n := range []int{0, 1, 7, 9} {
		if _, err := db.DecodeNotification(note(make([]byte, n))); err == nil {
			t.Fatalf("no error decoding notification with %d-byte timestamp", n)
		}
	}
}
