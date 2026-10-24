package spv

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

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

	// wantCode is the rule an invalid coinbase must be rejected by, 0 for a
	// valid one.
	tests := []struct {
		name      string
		txType    uint16
		scriptLen int
		wantCode  blockchain.ErrorCode
	}{
		{"normal coinbase, one byte", 0, 1, blockchain.ErrBadCoinbaseScriptLen},
		{"normal coinbase, min length", 0, blockchain.MinCoinbaseScriptLen, ok},
		{"normal coinbase, max length", 0, blockchain.MaxCoinbaseScriptLen, ok},
		{"normal coinbase, over max", 0, blockchain.MaxCoinbaseScriptLen + 1, blockchain.ErrBadCoinbaseScriptLen},
		{"dash coinbase, empty script", 5, 0, blockchain.ErrBadCoinbaseScriptLen},
		{"dash coinbase, one byte", 5, 1, ok},
		{"dash coinbase, over max", 5, blockchain.MaxCoinbaseScriptLen + 1, blockchain.ErrBadCoinbaseScriptLen},
		{"other special type, one byte", 1, 1, blockchain.ErrBadCoinbaseScriptLen},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkTransactionSanity(newCoinbase(tc.txType, tc.scriptLen), params)
			requireRule(t, err, tc.wantCode)
		})
	}
}

// ok marks a case that must pass its sanity check.
const ok = blockchain.ErrorCode(-1)

// requireRule checks err is the rule violation code, or nil for ok.
func requireRule(t *testing.T, err error, code blockchain.ErrorCode) {
	t.Helper()
	if code == ok {
		require.NoError(t, err)
		return
	}
	var re blockchain.RuleError
	require.True(t, errors.As(err, &re), "error %v is not a rule error", err)
	require.Equal(t, code, re.ErrorCode, "rule error: %v", err)
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
		wantCode           blockchain.ErrorCode
	}{
		{"normal, no inputs", 0, true, false, blockchain.ErrNoTxInputs},
		{"normal, no outputs", 0, false, true, blockchain.ErrNoTxOutputs},
		{"quorum commitment, no inputs or outputs", 6, true, true, ok},
		{"mnhf signal, no inputs or outputs", 7, true, true, ok},
		{"asset unlock, no inputs", 9, true, false, ok},
		{"asset unlock, no outputs", 9, false, true, blockchain.ErrNoTxOutputs},
		{"provider register, no inputs", 1, true, false, blockchain.ErrNoTxInputs},
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

			requireRule(t, checkTransactionSanity(tx, params), tc.wantCode)
		})
	}
}

// TestCheckTransactionSanityDashRegtestBlock runs the transactions of a block
// mined by Dash Core v23.1.4 on regtest after DIP-3 activated through the
// sanity check. It is the block the SPV wallet used to reject: the coinbase is
// a type 5 transaction with a one byte script, and five quorum commitments have
// no inputs or outputs.
func TestCheckTransactionSanityDashRegtestBlock(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/dash_regtest_post_dip3_block.hex")
	require.NoError(t, err)
	b, err := hex.DecodeString(string(bytes.TrimSpace(raw)))
	require.NoError(t, err)

	blk := &bisonwire.Block{Chain: "dash"}
	require.NoError(t, blk.BtcDecode(bytes.NewReader(b), wire.ProtocolVersion, wire.BaseEncoding))

	var sawShortCoinbase, sawQuorumCommitments int
	for _, tx := range blk.Transactions {
		switch {
		case tx.Type == dashCoinbaseTxType && len(tx.TxIn[0].SignatureScript) == 1:
			sawShortCoinbase++
		case tx.Type == dashQuorumCommitmentTxType && len(tx.TxIn) == 0 && len(tx.TxOut) == 0:
			sawQuorumCommitments++
		}
	}
	require.Equal(t, 1, sawShortCoinbase, "vector should hold a one byte type 5 coinbase")
	require.Equal(t, 5, sawQuorumCommitments, "vector should hold five empty quorum commitments")

	params := &netparams.ChainParams{MaxSatoshi: 21_000_000 * 1e8}
	for i, tx := range blk.Transactions {
		require.NoError(t, checkTransactionSanity(tx, params), "tx %d (type %d)", i, tx.Type)
	}
}

// TestCheckBlockSanityMaxSize checks the serialized block size limit. Dash has
// allowed blocks of up to 2 MB since DIP-1, so a valid Dash block over
// Bitcoin's 1 MB limit must not be treated as invalid, which would ban the peer
// that served it.
func TestCheckBlockSanityMaxSize(t *testing.T) {
	t.Parallel()

	params := &netparams.ChainParams{
		PowLimit:   maxPowLimit,
		MaxSatoshi: 21_000_000 * 1e8,
	}

	// newBlock builds a block of numPadding transactions that each hold a
	// 100000 byte output script, after a coinbase, with a valid header.
	newBlock := func(chain string, numPadding int) *bisonwire.Block {
		cb := wire.NewMsgTx(3)
		cb.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
			SignatureScript:  []byte{0x01, 0x01},
		})
		cb.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
		txs := []*bisonwire.Tx{{MsgTx: *cb, Chain: chain}}
		if chain == "dash" {
			txs[0].Type = dashCoinbaseTxType
		}

		for i := range numPadding {
			tx := wire.NewMsgTx(2)
			tx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{byte(i + 1)}, Index: 0},
			})
			script := bytes.Repeat([]byte{0x51}, 100_000)
			tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})
			txs = append(txs, &bisonwire.Tx{MsgTx: *tx, Chain: chain})
		}

		hashes := make([]chainhash.Hash, len(txs))
		for i, tx := range txs {
			hashes[i] = tx.TxHash()
		}
		header := wire.BlockHeader{
			Version:    1,
			MerkleRoot: calcMerkleRootFromHashes(hashes),
			Timestamp:  time.Unix(time.Now().Unix(), 0),
			Bits:       blockchain.BigToCompact(maxPowLimit),
		}
		// The target is half of the hash space, so a nonce is found quickly.
		for ; ; header.Nonce++ {
			h := header.BlockHash()
			if blockchain.HashToBig(&h).Cmp(maxPowLimit) <= 0 {
				break
			}
		}
		return &bisonwire.Block{Chain: chain, Header: header, Transactions: txs}
	}

	tests := []struct {
		name       string
		chain      string
		numPadding int
		wantCode   blockchain.ErrorCode
	}{
		{"dash block over 1 MB", "dash", 12, ok},
		{"dash block over 2 MB", "dash", 21, blockchain.ErrBlockTooBig},
		{"btc block over 1 MB", "btc", 12, blockchain.ErrBlockTooBig},
		{"btc block under 1 MB", "btc", 5, ok},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkBlockSanity(newBlock(tc.chain, tc.numPadding), params, blockchain.NewMedianTime())
			requireRule(t, err, tc.wantCode)
		})
	}
}
