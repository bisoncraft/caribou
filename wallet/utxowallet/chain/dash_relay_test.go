// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package chain

import (
	"errors"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/wire"
)

// TestNeutrinoRefusesDashSpecialTx guards that neutrino reports a special tx
// as unsupported, with a sentinel the wallet can tell apart from a rejection.
// Its peer layer relays a wire.MsgTx, which cannot carry the type or payload.
func TestNeutrinoRefusesDashSpecialTx(t *testing.T) {
	tx := &bisonwire.Tx{MsgTx: *wire.NewMsgTx(3), Chain: "dash", Type: 1, ExtraPayload: []byte{0x01}}

	// The refusal happens before the chain service is used.
	_, err := (&NeutrinoClient{}).SendRawTransaction(tx, false)
	if !errors.Is(err, ErrUnsupportedTx) {
		t.Fatalf("err = %v, want ErrUnsupportedTx", err)
	}
	if err := (&NeutrinoClient{}).CheckRelay(tx); !errors.Is(err, ErrUnsupportedTx) {
		t.Fatalf("CheckRelay err = %v, want ErrUnsupportedTx", err)
	}
}
