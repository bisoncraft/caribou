// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package wtxmgr

import (
	"bytes"
	"testing"
)

// TestNewTxRecordBisonTxMatchesMsgTxRecordForNormalTx pins that moving the
// publish path to NewTxRecordBisonTx changed nothing for ordinary
// transactions: same bytes, same hash as the record built from the bare
// MsgTx, which is what btc and ltc wallets stored before.
func TestNewTxRecordBisonTxMatchesMsgTxRecordForNormalTx(t *testing.T) {
	t.Parallel()

	for _, chain := range []string{"btc", "ltc"} {
		t.Run(chain, func(t *testing.T) {
			t.Parallel()
			parsed, err := NewTxRecord(chain, TstRecvSerializedTx, timeNow())
			if err != nil {
				t.Fatal(err)
			}

			got, err := NewTxRecordBisonTx(chain, &parsed.Tx, timeNow())
			if err != nil {
				t.Fatal(err)
			}
			want, err := NewTxRecordFromMsgTx(chain, &parsed.Tx.MsgTx, timeNow())
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(got.SerializedTx, want.SerializedTx) {
				t.Errorf("bytes differ: %x vs %x", got.SerializedTx, want.SerializedTx)
			}
			if got.Hash != want.Hash {
				t.Errorf("hash = %s, want %s", got.Hash, want.Hash)
			}
		})
	}
}
