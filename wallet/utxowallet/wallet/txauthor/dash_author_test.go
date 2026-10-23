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

// TestNewUnsignedSpecialTransactionFeeCoversSignedSize guards fee safety for
// special transactions: the fee is chosen before signing from an estimate, so
// the payload must be priced in or the signed ProRegTx lands below the relay
// fee rate. It signs the authored tx with the Dash signer and requires the paid
// fee to cover the real serialized size, across input counts that force the
// fee loop to re-run, without paying for much more than that.
func TestNewUnsignedSpecialTransactionFeeCoversSignedSize(t *testing.T) {
	t.Parallel()

	const (
		feeRate    = btcutil.Amount(10_000) // per kB
		payloadLen = 180
	)

	for _, inputs := range []int{1, 2, 3, 5} {
		_, pkScript := dashSpecialTx(t, 1, nil)
		payload := bytes.Repeat([]byte{0xcd}, payloadLen)

		// Each input is too small alone, so the number needed grows with
		// the target and the loop's fee is recomputed with every input.
		value := btcutil.Amount(100_000_000)
		outputs := []*wire.TxOut{wire.NewTxOut(int64(value)*int64(inputs)-50_000_000, pkScript)}
		change := &ChangeSource{
			NewScript:  func() ([]byte, error) { return pkScript, nil },
			ScriptSize: txsizes.P2PKHPkScriptSize,
		}

		authored, err := NewUnsignedSpecialTransaction(outputs, feeRate, payloadLen,
			p2pkhInputSource(inputs, value, pkScript), change)
		if err != nil {
			t.Fatalf("%d inputs: %v", inputs, err)
		}
		if authored.Tx.Version != 3 {
			t.Fatalf("%d inputs: version %d, want 3 so the payload is serialized",
				inputs, authored.Tx.Version)
		}

		tx := &bisonwire.Tx{MsgTx: *authored.Tx, Chain: "dash", Type: 1, ExtraPayload: payload}
		secrets := &stubSecrets{priv: fixedKey(t), compressed: true, params: &chaincfg.MainNetParams}
		if err := AddAllDashInputScripts(tx, authored.PrevScripts, secrets); err != nil {
			t.Fatalf("%d inputs: sign: %v", inputs, err)
		}

		var out btcutil.Amount
		for _, o := range tx.TxOut {
			out += btcutil.Amount(o.Value)
		}
		fee := authored.TotalInput - out
		required := feeRate * btcutil.Amount(tx.SerializeSize()) / 1000
		if fee < required {
			t.Errorf("%d inputs: fee %d is below %d required for the signed size %d",
				inputs, fee, required, tx.SerializeSize())
		}
		// The estimate overshoots by at most a few bytes per input.
		if slack := feeRate * btcutil.Amount(3*len(tx.TxIn)) / 1000; fee > required+slack {
			t.Errorf("%d inputs: fee %d overpays the required %d by more than %d",
				inputs, fee, required, slack)
		}
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
