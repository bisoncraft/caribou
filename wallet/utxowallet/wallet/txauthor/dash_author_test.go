// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package txauthor

import (
	"bytes"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet/txsizes"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
)

// countingSource wraps an InputSource and counts how often the fee loop asks
// it for inputs.
type countingSource struct {
	src   InputSource
	calls int
}

func (c *countingSource) fetch(target btcutil.Amount) (btcutil.Amount, []*wire.TxIn,
	[]btcutil.Amount, [][]byte, error) {

	c.calls++
	return c.src(target)
}

// p2pkhInputSource hands out n distinct inputs of value each, all paying to
// pkScript, in order until the target is met.
func p2pkhInputSource(n int, value btcutil.Amount, pkScript []byte) InputSource {
	next := 0
	var (
		total  btcutil.Amount
		ins    []*wire.TxIn
		values []btcutil.Amount
		// Scripts are returned one per input, as the real input source does.
		scripts [][]byte
	)
	return func(target btcutil.Amount) (btcutil.Amount, []*wire.TxIn, []btcutil.Amount, [][]byte, error) {
		for total < target && next < n {
			op := wire.OutPoint{Hash: [32]byte{byte(next + 1)}, Index: uint32(next)}
			ins = append(ins, wire.NewTxIn(&op, nil, nil))
			values = append(values, value)
			scripts = append(scripts, pkScript)
			total += value
			next++
		}
		return total, ins, values, scripts, nil
	}
}

func p2pkhChangeSource(pkScript []byte) *ChangeSource {
	return &ChangeSource{
		NewScript:  func() ([]byte, error) { return pkScript, nil },
		ScriptSize: txsizes.P2PKHPkScriptSize,
	}
}

// TestNewUnsignedSpecialTransactionFeeCoversSignedSize guards fee safety for
// special transactions: the fee is chosen before signing from an estimate, so
// the payload must be priced in or the signed ProRegTx lands below the relay
// fee rate. It signs the authored tx with the Dash signer and requires the paid
// fee to cover the real serialized size without paying for much more than that.
// The refetch case makes the first fetch short only because of the fee, so the
// fee loop must run again with a re-sized estimate.
func TestNewUnsignedSpecialTransactionFeeCoversSignedSize(t *testing.T) {
	t.Parallel()

	const (
		feeRate = btcutil.Amount(10_000) // per kB
		value   = btcutil.Amount(100_000_000)
	)
	_, pkScript := dashSpecialTx(t, 1, nil)

	tests := []struct {
		name       string
		available  int
		payloadLen int
		// output returns the single output's value given the payload size.
		output       func(payloadLen int) btcutil.Amount
		wantRefetch  bool
		wantMinInput int
	}{
		{name: "1 input", available: 1, payloadLen: 180, output: func(int) btcutil.Amount { return value - 50_000_000 }},
		{name: "2 inputs", available: 2, payloadLen: 180, output: func(int) btcutil.Amount { return 2*value - 50_000_000 }},
		{name: "3 inputs", available: 3, payloadLen: 180, output: func(int) btcutil.Amount { return 3*value - 50_000_000 }},
		{name: "5 inputs", available: 5, payloadLen: 180, output: func(int) btcutil.Amount { return 5*value - 50_000_000 }},
		{name: "payload at 1-byte prefix limit", available: 1, payloadLen: 252, output: func(int) btcutil.Amount { return value - 50_000_000 }},
		{name: "payload at 3-byte prefix", available: 1, payloadLen: 253, output: func(int) btcutil.Amount { return value - 50_000_000 }},
		{
			// Two inputs cover the first, single-input fee but leave less than
			// the two-input fee, so the loop must fetch a third.
			name: "refetch", available: 3, payloadLen: 180, wantRefetch: true, wantMinInput: 3,
			output: func(payloadLen int) btcutil.Amount {
				outs := []*wire.TxOut{wire.NewTxOut(0, pkScript)}
				fee1 := feeRate * btcutil.Amount(
					txsizes.EstimateSerializeSizeSpecial(1, outs, true, payloadLen)) / 1000
				return 2*value - fee1 - 1
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payload := bytes.Repeat([]byte{0xcd}, tc.payloadLen)
			outputs := []*wire.TxOut{wire.NewTxOut(int64(tc.output(tc.payloadLen)), pkScript)}
			src := &countingSource{src: p2pkhInputSource(tc.available, value, pkScript)}

			authored, err := NewUnsignedSpecialTransaction(outputs, feeRate, tc.payloadLen,
				src.fetch, p2pkhChangeSource(pkScript))
			if err != nil {
				t.Fatal(err)
			}
			if authored.Tx.Version != 3 {
				t.Fatalf("version %d, want 3 so the payload is serialized", authored.Tx.Version)
			}
			if authored.ChangeIndex < 0 {
				t.Fatal("expected a change output")
			}
			if tc.wantRefetch {
				if src.calls < 2 {
					t.Fatalf("fee loop fetched %d time(s), want a refetch", src.calls)
				}
				if len(authored.Tx.TxIn) < tc.wantMinInput {
					t.Fatalf("%d inputs, want at least %d after the refetch",
						len(authored.Tx.TxIn), tc.wantMinInput)
				}
			}

			tx := &bisonwire.Tx{MsgTx: *authored.Tx, Chain: "dash", Type: 1, ExtraPayload: payload}
			secrets := &stubSecrets{priv: fixedKey(t), compressed: true, params: &chaincfg.MainNetParams}
			if err := AddAllDashInputScripts(tx, authored.PrevScripts, secrets); err != nil {
				t.Fatalf("sign: %v", err)
			}

			var out btcutil.Amount
			for _, o := range tx.TxOut {
				out += btcutil.Amount(o.Value)
			}
			fee := authored.TotalInput - out
			required := feeRate * btcutil.Amount(tx.SerializeSize()) / 1000
			if fee < required {
				t.Errorf("fee %d is below %d required for the signed size %d",
					fee, required, tx.SerializeSize())
			}
			// The estimate overshoots by at most a few bytes per input.
			if slack := feeRate * btcutil.Amount(3*len(tx.TxIn)) / 1000; fee > required+slack {
				t.Errorf("fee %d overpays the required %d by more than %d", fee, required, slack)
			}
		})
	}
}

// TestNewUnsignedSpecialTransactionFeeTracksPayloadSize pins the payload and
// its length prefix exactly. At 1 duff/byte the authored fee is the estimated
// size, so growing the payload must grow the fee by the payload bytes plus any
// growth of the varint prefix (1 byte through 252, 3 bytes from 253). Signature
// slack hides a missing prefix in the signed-size test, but not here.
func TestNewUnsignedSpecialTransactionFeeTracksPayloadSize(t *testing.T) {
	t.Parallel()

	const value = btcutil.Amount(100_000_000)
	_, pkScript := dashSpecialTx(t, 1, nil)

	feeFor := func(payloadLen int) btcutil.Amount {
		outputs := []*wire.TxOut{wire.NewTxOut(int64(value)/2, pkScript)}
		authored, err := NewUnsignedSpecialTransaction(outputs, 1000, payloadLen,
			p2pkhInputSource(1, value, pkScript), p2pkhChangeSource(pkScript))
		if err != nil {
			t.Fatalf("payload %d: %v", payloadLen, err)
		}
		var out btcutil.Amount
		for _, o := range authored.Tx.TxOut {
			out += btcutil.Amount(o.Value)
		}
		return authored.TotalInput - out
	}

	if got := feeFor(252) - feeFor(32); got != 220 {
		t.Errorf("fee grew by %d from payload 32 to 252, want 220", got)
	}
	if got := feeFor(253) - feeFor(252); got != 3 {
		t.Errorf("fee grew by %d from payload 252 to 253, want 3 (1 byte plus 2 of prefix)", got)
	}
}

// TestNewUnsignedSpecialTransactionRejectsBadInput pins the preconditions the
// estimate relies on: a special tx without a payload is not special, and a
// non-P2PKH change script would be mis-sized by the P2PKH estimate.
func TestNewUnsignedSpecialTransactionRejectsBadInput(t *testing.T) {
	t.Parallel()

	_, pkScript := dashSpecialTx(t, 1, nil)
	outputs := []*wire.TxOut{wire.NewTxOut(1, pkScript)}
	src := p2pkhInputSource(1, 1_000_000, pkScript)
	p2pkhChange := &ChangeSource{
		NewScript:  func() ([]byte, error) { return pkScript, nil },
		ScriptSize: txsizes.P2PKHPkScriptSize,
	}
	p2wpkhChange := &ChangeSource{
		NewScript:  func() ([]byte, error) { return nil, nil },
		ScriptSize: txsizes.P2WPKHPkScriptSize,
	}

	if _, err := NewUnsignedSpecialTransaction(outputs, 1000, 0, src, p2pkhChange); err == nil {
		t.Error("zero payload: expected an error")
	}
	if _, err := NewUnsignedSpecialTransaction(outputs, 1000, 10, src, p2wpkhChange); err == nil {
		t.Error("p2wpkh change: expected an error")
	}
}
