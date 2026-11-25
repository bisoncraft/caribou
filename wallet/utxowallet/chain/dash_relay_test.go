// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package chain

import (
	"errors"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/spv/pushtx"
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

// sendErrService is a chain service whose SendTransaction fails with err.
type sendErrService struct {
	*mockChainService
	err error
}

func (s sendErrService) SendTransaction(*wire.MsgTx) error { return s.err }

// TestNeutrinoSendRawTransactionErrors guards what a failed send says: only a
// peer's rejection refuses a transaction. Any other failure, such as a stopped
// broadcaster, is ErrBackendUnavailable, so the wallet keeps a transaction
// that may be in the mempool.
func TestNeutrinoSendRawTransactionErrors(t *testing.T) {
	tx := &bisonwire.Tx{MsgTx: *wire.NewMsgTx(1), Chain: "btc"}
	send := func(err error) error {
		c := &NeutrinoClient{CS: sendErrService{&mockChainService{}, err}}
		_, got := c.SendRawTransaction(tx, false)
		return got
	}

	if err := send(pushtx.ErrBroadcasterStopped); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("a stopped broadcaster: err = %v, want ErrBackendUnavailable", err)
	}
	err := send(&pushtx.BroadcastError{Code: pushtx.Invalid, Reason: "bad-txns-inputs-missingorspent"})
	if err == nil || errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("a peer's rejection: err = %v, want a refusal", err)
	}
}
