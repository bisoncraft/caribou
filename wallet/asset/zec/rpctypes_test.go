// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func TestListUnspentResultSafe(t *testing.T) {
	r := &listUnspentResult{}
	if !r.Safe() {
		t.Errorf("nil SafePtr should be treated as safe")
	}

	f := false
	r.SafePtr = &f
	if r.Safe() {
		t.Errorf("false SafePtr should not be safe")
	}

	tr := true
	r.SafePtr = &tr
	if !r.Safe() {
		t.Errorf("true SafePtr should be safe")
	}
}

func TestGetBlockchainInfoResultSyncing(t *testing.T) {
	tru, fls := true, false

	tests := []struct {
		name string
		r    *getBlockchainInfoResult
		want bool
	}{
		{"download complete flag false", &getBlockchainInfoResult{InitialBlockDownloadComplete: &tru}, false},
		{"download in progress flag true", &getBlockchainInfoResult{InitialBlockDownload: &tru}, true},
		{"download not in progress flag false, headers match", &getBlockchainInfoResult{InitialBlockDownload: &fls, Headers: 10, Blocks: 10}, false},
		{"no flags, far behind", &getBlockchainInfoResult{Headers: 10, Blocks: 5}, true},
		{"no flags, caught up", &getBlockchainInfoResult{Headers: 10, Blocks: 9}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.Syncing(); got != tt.want {
				t.Errorf("Syncing() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTxOutFromTxBytes(t *testing.T) {
	msgTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			wire.NewTxOut(1, []byte{0x01}),
			wire.NewTxOut(2, []byte{0x02}),
		},
	}
	deserialize := func([]byte) (*wire.MsgTx, error) { return msgTx, nil }
	hashTx := func(*wire.MsgTx) *chainhash.Hash { return &chainhash.Hash{} }

	out, err := txOutFromTxBytes(nil, 1, deserialize, hashTx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Value != 2 {
		t.Errorf("wrong vout selected: got value %d, want 2", out.Value)
	}

	// deserialize error path.
	deserializeErr := func([]byte) (*wire.MsgTx, error) { return nil, errors.New("bad tx") }
	if _, err := txOutFromTxBytes(nil, 0, deserializeErr, hashTx); err == nil {
		t.Fatalf("expected error from deserialize failure")
	}

	// vout out of range.
	if _, err := txOutFromTxBytes(nil, 5, deserialize, hashTx); err == nil {
		t.Fatalf("expected error for out-of-range vout")
	}
}
