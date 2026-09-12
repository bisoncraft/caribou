// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"encoding/hex"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/decred/dcrd/dcrjson/v4"
)

func TestListUnspent(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("listunspent", []*listUnspentResult{{TxID: tTxID, Amount: 1}})
	res, err := listUnspent(w)
	if err != nil {
		t.Fatalf("listUnspent error: %v", err)
	}
	if len(res) != 1 || res[0].TxID != tTxID {
		t.Fatalf("unexpected result: %+v", res)
	}

	cl.queueResponse("listunspent", errTest)
	if _, err := listUnspent(w); err == nil {
		t.Fatalf("expected error from listUnspent")
	}
}

func TestLockUnspent(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	ops := []*btc.Output{{Pt: btc.OutPoint{TxHash: *tTxHash, Vout: 0}}}

	cl.queueResponse("lockunspent", true)
	if err := lockUnspent(w, false, ops); err != nil {
		t.Fatalf("lockUnspent error: %v", err)
	}

	cl.queueResponse("lockunspent", false)
	if err := lockUnspent(w, false, ops); err == nil {
		t.Fatalf("expected error for unsuccessful lockunspent")
	}

	cl.queueResponse("lockunspent", errTest)
	if err := lockUnspent(w, true, nil); err == nil {
		t.Fatalf("expected error from lockunspent rpc failure")
	}
}

func TestGetTransaction(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	dTx := dummyTx()
	txB, _ := dTx.Bytes()

	cl.queueResponse("gettransaction", &GetTransactionResult{
		Bytes:     txB,
		BlockHash: tTxHash.String(),
	})
	zt, err := getTransaction(w, tTxHash)
	if err != nil {
		t.Fatalf("getTransaction error: %v", err)
	}
	if zt.blockHash.String() != tTxHash.String() {
		t.Fatalf("wrong block hash")
	}

	// Bad tx bytes.
	cl.queueResponse("gettransaction", &GetTransactionResult{Bytes: []byte{0xff}})
	if _, err := getTransaction(w, tTxHash); err == nil {
		t.Fatalf("expected error for bad tx bytes")
	}

	// Bad block hash.
	cl.queueResponse("gettransaction", &GetTransactionResult{Bytes: txB, BlockHash: "not-a-hash"})
	if _, err := getTransaction(w, tTxHash); err == nil {
		t.Fatalf("expected error for bad block hash")
	}

	// RPC error.
	cl.queueResponse("gettransaction", errTest)
	if _, err := getTransaction(w, tTxHash); err == nil {
		t.Fatalf("expected error from rpc failure")
	}
}

func TestGetRawTransaction(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getrawtransaction", hex.EncodeToString([]byte{1, 2, 3}))
	b, err := getRawTransaction(w, tTxHash)
	if err != nil {
		t.Fatalf("getRawTransaction error: %v", err)
	}
	if hex.EncodeToString(b) != "010203" {
		t.Fatalf("wrong bytes: %x", b)
	}
}

func TestGetZecTransaction(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	dTx := dummyTx()
	txB, _ := dTx.Bytes()
	cl.queueResponse("getrawtransaction", hex.EncodeToString(txB))
	tx, err := getZecTransaction(w, tTxHash)
	if err != nil {
		t.Fatalf("getZecTransaction error: %v", err)
	}
	if tx == nil {
		t.Fatalf("nil tx")
	}

	cl.queueResponse("getrawtransaction", errTest)
	if _, err := getZecTransaction(w, tTxHash); err == nil {
		t.Fatalf("expected error from rpc failure")
	}
}

func TestListLockUnspent(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	unspentTxID := "aa" + tTxID[2:]

	cl.queueResponse("listlockunspent", []*rpcOutpoint{
		{TxID: unspentTxID, Vout: 0},
		{TxID: tTxID, Vout: 1},
	})
	// First is genuinely unspent.
	cl.queueResponse("gettxout", &btcjson.GetTxOutResult{Confirmations: 1})
	// Second is spent: gettxout returns nil, so it should get unlocked.
	cl.queueResponse("gettxout", (*btcjson.GetTxOutResult)(nil))
	cl.queueResponse("lockunspent", true)

	res, err := listLockUnspent(w, tLogger)
	if err != nil {
		t.Fatalf("listLockUnspent error: %v", err)
	}
	if len(res) != 1 || res[0].TxID != unspentTxID {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestListLockUnspentErrors(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("listlockunspent", errTest)
	if _, err := listLockUnspent(w, tLogger); err == nil {
		t.Fatalf("expected error from listlockunspent rpc failure")
	}

	// gettxout errors are logged and the outpoint dropped, not fatal.
	cl.queueResponse("listlockunspent", []*rpcOutpoint{{TxID: tTxID, Vout: 0}})
	cl.queueResponse("gettxout", errTest)
	res, err := listLockUnspent(w, tLogger)
	if err != nil {
		t.Fatalf("listLockUnspent error: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("expected no results, got %+v", res)
	}

	// gettxout says spent, but lockunspent to release it fails; logged, not fatal.
	cl.queueResponse("listlockunspent", []*rpcOutpoint{{TxID: tTxID, Vout: 0}})
	cl.queueResponse("gettxout", (*btcjson.GetTxOutResult)(nil))
	cl.queueResponse("lockunspent", errTest)
	res, err = listLockUnspent(w, tLogger)
	if err != nil {
		t.Fatalf("listLockUnspent error: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("expected no results, got %+v", res)
	}
}

func TestGetVersion(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getnetworkinfo", &struct {
		Version         uint64 `json:"version"`
		SubVersion      string `json:"subversion"`
		ProtocolVersion uint64 `json:"protocolversion"`
	}{Version: 5, ProtocolVersion: 170100})
	ver, proto, err := getVersion(w)
	if err != nil {
		t.Fatalf("getVersion error: %v", err)
	}
	if ver != 5 || proto != 170100 {
		t.Fatalf("wrong version/protocol: %d %d", ver, proto)
	}

	cl.queueResponse("getnetworkinfo", errTest)
	if _, _, err := getVersion(w); err == nil {
		t.Fatalf("expected error from getVersion rpc failure")
	}
}

func TestGetBlockchainInfo(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getblockchaininfo", &getBlockchainInfoResult{Blocks: 10})
	info, err := getBlockchainInfo(w)
	if err != nil {
		t.Fatalf("getBlockchainInfo error: %v", err)
	}
	if info.Blocks != 10 {
		t.Fatalf("wrong blocks: %d", info.Blocks)
	}

	cl.queueResponse("getblockchaininfo", errTest)
	if _, err := getBlockchainInfo(w); err == nil {
		t.Fatalf("expected error")
	}
}

func TestGetBestBlockHeaderAndHeight(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getbestblockhash", tTxHash.String())
	cl.queueResponse("getblockheader", &btc.BlockHeader{Hash: tTxHash.String(), Height: 42, Confirmations: 1})
	hdr, err := getBestBlockHeader(w)
	if err != nil {
		t.Fatalf("getBestBlockHeader error: %v", err)
	}
	if hdr.Height != 42 {
		t.Fatalf("wrong height: %d", hdr.Height)
	}

	cl.queueResponse("getbestblockhash", tTxHash.String())
	cl.queueResponse("getblockheader", &btc.BlockHeader{Hash: tTxHash.String(), Height: 42, Confirmations: 1})
	height, err := getBestBlockHeight(w)
	if err != nil {
		t.Fatalf("getBestBlockHeight error: %v", err)
	}
	if height != 42 {
		t.Fatalf("wrong height: %d", height)
	}

	// getbestblockhash error propagates.
	cl.queueResponse("getbestblockhash", errTest)
	if _, err := getBestBlockHeader(w); err == nil {
		t.Fatalf("expected error")
	}
}

func TestGetVerboseBlockHeaderOrphan(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getblockheader", &btc.BlockHeader{Height: 5, Confirmations: -1})
	hdr, mainchain, err := getVerboseBlockHeader(w, tTxHash)
	if err != nil {
		t.Fatalf("getVerboseBlockHeader error: %v", err)
	}
	if mainchain {
		t.Fatalf("expected orphaned (non-mainchain) block")
	}
	if hdr.Height != 5 {
		t.Fatalf("wrong height")
	}

	cl.queueResponse("getblockheader", errTest)
	if _, _, err := getVerboseBlockHeader(w, tTxHash); err == nil {
		t.Fatalf("expected error")
	}
}

func TestGetWalletTransaction(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("gettransaction", &GetTransactionResult{TxID: tTxID})
	tx, err := getWalletTransaction(w, tTxHash)
	if err != nil {
		t.Fatalf("getWalletTransaction error: %v", err)
	}
	if tx.TxID != tTxID {
		t.Fatalf("wrong txid")
	}

	// Not-found error should translate to asset.CoinNotFoundError.
	notFoundErr := &dcrjson.RPCError{Code: dcrjson.RPCErrorCode(btcjson.ErrRPCNoTxInfo)}
	cl.queueResponse("gettransaction", notFoundErr)
	if _, err := getWalletTransaction(w, tTxHash); err != asset.CoinNotFoundError {
		t.Fatalf("expected asset.CoinNotFoundError, got %v", err)
	}

	cl.queueResponse("gettransaction", errTest)
	if _, err := getWalletTransaction(w, tTxHash); err == nil {
		t.Fatalf("expected error")
	}
}

func TestGetBalance(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getbalance", uint64(1e8))
	bal, err := getBalance(w)
	if err != nil {
		t.Fatalf("getBalance error: %v", err)
	}
	if bal != 1e8 {
		t.Fatalf("wrong balance: %d", bal)
	}
}

func TestPeerCount(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getnetworkinfo", &networkInfo{Connections: 3})
	n, err := peerCount(w)
	if err != nil {
		t.Fatalf("peerCount error: %v", err)
	}
	if n != 3 {
		t.Fatalf("wrong peer count: %d", n)
	}

	cl.queueResponse("getnetworkinfo", errTest)
	if _, err := peerCount(w); !errorHasCode(err, errGetNetInfo) {
		t.Fatalf("expected errGetNetInfo, got %v", err)
	}
}

func TestGetBlockHeight(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getblockheader", &btc.BlockHeader{Height: 7, Confirmations: 1})
	h, err := getBlockHeight(w, tTxHash)
	if err != nil {
		t.Fatalf("getBlockHeight error: %v", err)
	}
	if h != 7 {
		t.Fatalf("wrong height: %d", h)
	}

	// Negative height (orphan) is an error.
	cl.queueResponse("getblockheader", &btc.BlockHeader{Height: -1, Confirmations: -1})
	if _, err := getBlockHeight(w, tTxHash); err == nil {
		t.Fatalf("expected error for orphaned block")
	}
}

func TestGetBlockWrapper(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getblock", hex.EncodeToString(block1624455))
	blk, err := getBlock(w, *tTxHash)
	if err != nil {
		t.Fatalf("getBlock error: %v", err)
	}
	if blk == nil {
		t.Fatalf("nil block")
	}

	cl.queueResponse("getblock", errTest)
	if _, err := getBlock(w, *tTxHash); err == nil {
		t.Fatalf("expected error")
	}
}

func TestGetBlockHash(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getblockhash", tTxHash.String())
	h, err := getBlockHash(w, 10)
	if err != nil {
		t.Fatalf("getBlockHash error: %v", err)
	}
	if h.String() != tTxHash.String() {
		t.Fatalf("wrong hash")
	}
}

func TestGetRawMempool(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getrawmempool", []string{tTxHash.String()})
	hashes, err := getRawMempool(w)
	if err != nil {
		t.Fatalf("getRawMempool error: %v", err)
	}
	if len(hashes) != 1 || hashes[0].String() != tTxHash.String() {
		t.Fatalf("unexpected hashes: %+v", hashes)
	}

	cl.queueResponse("getrawmempool", []string{"not-a-hash"})
	if _, err := getRawMempool(w); err == nil {
		t.Fatalf("expected error for bad hash")
	}

	cl.queueResponse("getrawmempool", errTest)
	if _, err := getRawMempool(w); err == nil {
		t.Fatalf("expected error from rpc failure")
	}
}

func TestSyncStatusHelper(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getblockchaininfo", &getBlockchainInfoResult{Blocks: 10, Headers: 10})
	ss, err := syncStatus(w)
	if err != nil {
		t.Fatalf("syncStatus error: %v", err)
	}
	if !ss.Synced {
		t.Fatalf("expected synced")
	}

	cl.queueResponse("getblockchaininfo", errTest)
	if _, err := syncStatus(w); !errorHasCode(err, errGetChainInfo) {
		t.Fatalf("expected errGetChainInfo, got %v", err)
	}
}

func TestListSinceBlock(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("listsinceblock", &listSinceBlockRes{
		Transactions: []btcjson.ListTransactionsResult{{TxID: tTxID}},
	})
	txs, err := listSinceBlock(w, tTxHash)
	if err != nil {
		t.Fatalf("listSinceBlock error: %v", err)
	}
	if len(txs) != 1 || txs[0].TxID != tTxID {
		t.Fatalf("unexpected result: %+v", txs)
	}

	cl.queueResponse("listsinceblock", errTest)
	if _, err := listSinceBlock(w, tTxHash); err == nil {
		t.Fatalf("expected error")
	}
}

func TestGetReceivedByAddress(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getreceivedbyaddress", uint64(500))
	recv, err := getReceivedByAddress(w, tAddr)
	if err != nil {
		t.Fatalf("getReceivedByAddress error: %v", err)
	}
	if recv != 500 {
		t.Fatalf("wrong amount: %d", recv)
	}
}

func TestTranslateRPCCancelErr(t *testing.T) {
	if translateRPCCancelErr(nil) != nil {
		t.Fatalf("expected nil for nil input")
	}
	if translateRPCCancelErr(errTest) != errTest {
		t.Fatalf("expected passthrough for unrelated errors")
	}
}
