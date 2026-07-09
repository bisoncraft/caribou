// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bisoncraft/caribou/util"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// rawRequester defines decred's rpcclient RawRequest func through which all RPC
// requests are sent. For testing, it can be satisfied by a stub.
type rawRequester interface {
	RawRequest(context.Context, string, []json.RawMessage) (json.RawMessage, error)
}

// blockVector is a block's height and hash.
type blockVector struct {
	Height int64
	Hash   chainhash.Hash
}

type listUnspentResult struct {
	TxID          string     `json:"txid"`
	Vout          uint32     `json:"vout"`
	Address       string     `json:"address"`
	Label         string     `json:"label"`
	ScriptPubKey  util.Bytes `json:"scriptPubKey"`
	Amount        float64    `json:"amount"`
	Confirmations uint32     `json:"confirmations"`
	RedeemScript  util.Bytes `json:"redeemScript"`
	Spendable     bool       `json:"spendable"`
	Solvable      bool       `json:"solvable"`
	SafePtr       *bool      `json:"safe"`
}

func (l *listUnspentResult) Safe() bool {
	return l.SafePtr == nil || *l.SafePtr
}

// getTransactionResult models the required data from the gettransaction command.
type getTransactionResult struct {
	Confirmations uint64     `json:"confirmations"`
	BlockHash     string     `json:"blockhash"`
	BlockIndex    int64      `json:"blockindex"`
	BlockTime     uint64     `json:"blocktime"`
	TxID          string     `json:"txid"`
	Time          uint64     `json:"time"`
	TimeReceived  uint64     `json:"timereceived"`
	Bytes         util.Bytes `json:"hex"` // []byte, although it marshals/unmarshals a hex string
}

// rpcOutpoint is used to specify outputs to lock in calls to lockunspent.
type rpcOutpoint struct {
	TxID string `json:"txid"`
	Vout uint32 `json:"vout"`
}

// walletTransactionNotFound is returned when a transaction is not found in the
// wallet.
const walletTransactionNotFound = util.ErrorKind("wallet transaction not found")

// signTxResult models the data from the signrawtransaction command.
type signTxResult struct {
	Hex      util.Bytes     `json:"hex"`
	Complete bool           `json:"complete"`
	Errors   []*signTxError `json:"errors"`
}

// signTxError models the data that contains script verification errors from the
// signrawtransaction request
type signTxError struct {
	TxID      string     `json:"txid"`
	Vout      uint32     `json:"vout"`
	ScriptSig util.Bytes `json:"scriptSig"`
	Sequence  uint64     `json:"sequence"`
	Error     string     `json:"error"`
}

// getBlockchainInfoResult models the data returned from the getblockchaininfo
// command.
type getBlockchainInfoResult struct {
	Chain         string `json:"chain"`
	Blocks        int64  `json:"blocks"`
	Headers       int64  `json:"headers"`
	BestBlockHash string `json:"bestblockhash"`
	// InitialBlockDownload will be true if the node is still in the initial
	// block download mode.
	InitialBlockDownload *bool `json:"initialblockdownload"`
	// InitialBlockDownloadComplete will be true if this node has completed its
	// initial block download and is expected to be synced to the network.
	// Zcash uses this terminology instead of initialblockdownload.
	InitialBlockDownloadComplete *bool `json:"initial_block_download_complete"`
}

func (r *getBlockchainInfoResult) Syncing() bool {
	if r.InitialBlockDownloadComplete != nil && *r.InitialBlockDownloadComplete {
		return false
	}
	if r.InitialBlockDownload != nil && *r.InitialBlockDownload {
		return true
	}
	return r.Headers-r.Blocks > 1
}

// txOutFromTxBytes parses the specified *wire.TxOut from the serialized
// transaction.
func txOutFromTxBytes(
	txB []byte,
	vout uint32,
	deserializeTx func([]byte) (*wire.MsgTx, error),
	hashTx func(*wire.MsgTx) *chainhash.Hash,
) (*wire.TxOut, error) {
	msgTx, err := deserializeTx(txB)
	if err != nil {
		return nil, fmt.Errorf("error decoding transaction bytes: %v", err)
	}

	if len(msgTx.TxOut) <= int(vout) {
		return nil, fmt.Errorf("no vout %d in tx %s", vout, hashTx(msgTx))
	}
	return msgTx.TxOut[vout], nil
}
