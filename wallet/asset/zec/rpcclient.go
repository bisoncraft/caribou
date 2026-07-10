// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset/btc"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrjson/v4" // for dcrjson.RPCError returns from rpcclient
)

// isTxNotFoundErr will return true if the error indicates that the requested
// transaction is not known. The error must be dcrjson.RPCError with a numeric
// code equal to btcjson.ErrRPCNoTxInfo. WARNING: This is specific to errors
// from an RPC to a bitcoind (or clone) using dcrd's rpcclient!
func isTxNotFoundErr(err error) bool {
	// We are using dcrd's client with Bitcoin Core, so errors will be of type
	// dcrjson.RPCError, but numeric codes should come from btcjson.
	const errRPCNoTxInfo = int(btcjson.ErrRPCNoTxInfo)
	var rpcErr *dcrjson.RPCError
	return errors.As(err, &rpcErr) && int(rpcErr.Code) == errRPCNoTxInfo
}

// anylist is a list of RPC parameters to be converted to []json.RawMessage and
// sent via RawRequest.
type anylist []any

// chainOK is for screening the chain field of the getblockchaininfo result.
func chainOK(net util.Network, str string) bool {
	var chainStr string
	switch net {
	case util.Mainnet:
		chainStr = "main"
	case util.Testnet:
		chainStr = "test"
	case util.Regtest:
		chainStr = "reg"
	}
	return strings.Contains(str, chainStr)
}

// findRedemptionsInMempool attempts to find spending info for the specified
// contracts by searching every input of all txs in the mempool.
func findRedemptionsInMempool(
	ctx context.Context,
	log util.Logger,
	reqs map[btc.OutPoint]*findRedemptionReq,
	getMempool func() ([]*chainhash.Hash, error),
	getTx func(txHash *chainhash.Hash) (*wire.MsgTx, error),
	segwit bool,
	hashTx func(*wire.MsgTx) *chainhash.Hash,
	chainParams *chaincfg.Params,

) (discovered map[btc.OutPoint]*findRedemptionResult) {
	contractsCount := len(reqs)
	log.Debugf("finding redemptions for %d contracts in mempool", contractsCount)

	discovered = make(map[btc.OutPoint]*findRedemptionResult, len(reqs))

	var totalFound, totalCanceled int
	logAbandon := func(reason string) {
		// Do not remove the contracts from the findRedemptionQueue
		// as they could be subsequently redeemed in some mined tx(s),
		// which would be captured when a new tip is reported.
		if totalFound+totalCanceled > 0 {
			log.Debugf("%d redemptions found, %d canceled out of %d contracts in mempool",
				totalFound, totalCanceled, contractsCount)
		}
		log.Errorf("abandoning mempool redemption search for %d contracts because of %s",
			contractsCount-totalFound-totalCanceled, reason)
	}

	mempoolTxs, err := getMempool()
	if err != nil {
		logAbandon(fmt.Sprintf("error retrieving transactions: %v", err))
		return
	}

	for _, txHash := range mempoolTxs {
		if ctx.Err() != nil {
			return nil
		}
		tx, err := getTx(txHash)
		if err != nil {
			logAbandon(fmt.Sprintf("getrawtransaction error for tx hash %v: %v", txHash, err))
			return
		}
		newlyDiscovered := findRedemptionsInTxWithHasher(ctx, segwit, reqs, tx, chainParams, hashTx)
		maps.Copy(discovered, newlyDiscovered)
	}
	return
}

// searchBlockForRedemptions attempts to find spending info for the specified
// contracts by searching every input of all txs in the provided block range.
func searchBlockForRedemptions(
	ctx context.Context,
	reqs map[btc.OutPoint]*findRedemptionReq,
	msgBlock *wire.MsgBlock,
	segwit bool,
	hashTx func(*wire.MsgTx) *chainhash.Hash,
	chainParams *chaincfg.Params,
) (discovered map[btc.OutPoint]*findRedemptionResult) {

	discovered = make(map[btc.OutPoint]*findRedemptionResult, len(reqs))

	for _, msgTx := range msgBlock.Transactions {
		newlyDiscovered := findRedemptionsInTxWithHasher(ctx, segwit, reqs, msgTx, chainParams, hashTx)
		maps.Copy(discovered, newlyDiscovered)
	}
	return
}

func call(ctx context.Context, r rawRequester, method string, args anylist, thing any) error {
	params := make([]json.RawMessage, 0, len(args))
	for i := range args {
		p, err := json.Marshal(args[i])
		if err != nil {
			return err
		}
		params = append(params, p)
	}

	b, err := r.RawRequest(ctx, method, params)
	if err != nil {
		return fmt.Errorf("rawrequest (%v) error: %w", method, err)
	}
	if thing != nil {
		return json.Unmarshal(b, thing)
	}
	return nil
}
