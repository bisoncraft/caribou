package spv

import (
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestCheckTransactionSanityCoinbaseScriptLen checks the coinbase script length
// bounds. Dash coinbase special transactions (type 5) carry the height in their
// payload, so Dash Core accepts a one byte script for them. Rejecting it makes
// the wallet ban every peer serving a post-DIP-3 block as invalid.
func TestCheckTransactionSanityCoinbaseScriptLen(t *testing.T) {
	t.Parallel()

	params := &netparams.ChainParams{MaxSatoshi: 21_000_000 * 1e8}

	newCoinbase := func(txType uint16, scriptLen int) *bisonwire.Tx {
		msgTx := wire.NewMsgTx(3)
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{}, Index: wire.MaxPrevOutIndex},
			SignatureScript:  make([]byte, scriptLen),
		})
		msgTx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
		return &bisonwire.Tx{MsgTx: *msgTx, Type: txType}
	}

	tests := []struct {
		name      string
		txType    uint16
		scriptLen int
		wantErr   bool
	}{
		{"normal coinbase, one byte", 0, 1, true},
		{"normal coinbase, min length", 0, blockchain.MinCoinbaseScriptLen, false},
		{"normal coinbase, max length", 0, blockchain.MaxCoinbaseScriptLen, false},
		{"normal coinbase, over max", 0, blockchain.MaxCoinbaseScriptLen + 1, true},
		{"dash coinbase, empty script", 5, 0, true},
		{"dash coinbase, one byte", 5, 1, false},
		{"dash coinbase, over max", 5, blockchain.MaxCoinbaseScriptLen + 1, true},
		{"other special type, one byte", 1, 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkTransactionSanity(newCoinbase(tc.txType, tc.scriptLen), params)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestCheckTransactionSanityEmptyInputsOutputs checks which Dash special
// transaction types may have no inputs or outputs, mirroring Dash Core's
// CheckTransaction. Quorum commitments appear in ordinary blocks, so rejecting
// them makes the wallet ban every peer that serves such a block.
func TestCheckTransactionSanityEmptyInputsOutputs(t *testing.T) {
	t.Parallel()

	params := &netparams.ChainParams{MaxSatoshi: 21_000_000 * 1e8}

	tests := []struct {
		name               string
		txType             uint16
		noInputs, noOutput bool
		wantErr            bool
	}{
		{"normal, no inputs", 0, true, false, true},
		{"normal, no outputs", 0, false, true, true},
		{"quorum commitment, no inputs or outputs", 6, true, true, false},
		{"mnhf signal, no inputs or outputs", 7, true, true, false},
		{"asset unlock, no inputs", 9, true, false, false},
		{"asset unlock, no outputs", 9, false, true, true},
		{"provider register, no inputs", 1, true, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			msgTx := wire.NewMsgTx(3)
			if !tc.noInputs {
				msgTx.AddTxIn(&wire.TxIn{
					PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0},
				})
			}
			if !tc.noOutput {
				msgTx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
			}
			tx := &bisonwire.Tx{MsgTx: *msgTx, Type: tc.txType}

			err := checkTransactionSanity(tx, params)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
