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

// TestDecodeShortFixedWidthPushes asserts that every persisted record whose
// fixed-width push is the wrong length is rejected as an error.
//
// Same reasoning as the notification timestamp above: these are read with a
// fixed-width big-endian decoder, so a short push panics the caller rather than
// failing the decode, and every caller is written to handle an error.
func TestDecodeShortFixedWidthPushes(t *testing.T) {
	creds := func(birthday []byte) []byte {
		return encode.BuildyBytes{0}.
			AddData([]byte("encseed")).
			AddData([]byte("encinnerkey")).
			AddData([]byte("innerparams")).
			AddData([]byte("outerparams")).
			AddData(birthday)
	}
	assetBal := func(avail, immature, locked []byte) []byte {
		return encode.BuildyBytes{0}.AddData(avail).AddData(immature).AddData(locked)
	}
	bal := func(assetBal, stamp []byte) []byte {
		return encode.BuildyBytes{0}.AddData(assetBal).AddData(stamp)
	}

	eight := make([]byte, 8)
	if _, err := db.DecodeCredentials(creds(eight)); err != nil {
		t.Fatalf("well-formed credentials failed to decode: %v", err)
	}
	if _, err := db.DecodeBalance(bal(assetBal(eight, eight, eight), eight)); err != nil {
		t.Fatalf("well-formed balance failed to decode: %v", err)
	}

	for _, n := range []int{0, 1, 7, 9} {
		short := make([]byte, n)
		if _, err := db.DecodeCredentials(creds(short)); err == nil {
			t.Fatalf("no error decoding credentials with %d-byte birthday", n)
		}
		if _, err := db.DecodeBalance(bal(assetBal(eight, eight, eight), short)); err == nil {
			t.Fatalf("no error decoding balance with %d-byte stamp", n)
		}
		for i, fields := range [][3][]byte{
			{short, eight, eight},
			{eight, short, eight},
			{eight, eight, short},
		} {
			ab := assetBal(fields[0], fields[1], fields[2])
			if _, err := db.DecodeBalance(bal(ab, eight)); err == nil {
				t.Fatalf("no error decoding asset balance with %d-byte field %d", n, i)
			}
		}
	}
}
