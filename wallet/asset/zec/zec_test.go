// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/btc"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

const (
	tAddr        = "tmH2m5fi5yY3Qg2GpGwcCrnnoD4wp944RMJ"
	tUnifiedAddr = "uregtest1w2khftfjd8w7vgw32g24nwqdt0f3zfrwe9a4uy6trsq3swlpgsfssf2uqsfluu3490jdgers4vwz8l9yg39c9x70phllu8cy57f8mdc6ym5e7xtra0f99wtxvnm0wg4uz7an0wvl5s7jt2y7fqla2j976ej0e4hsspq73m0zrw5lzly797fhku0q74xeshuwvwmrku5u8f7gyd2r0sx"
)

var (
	tCtx    context.Context
	tLogger = util.StdOutLogger("T", util.LevelError)
	tTxID   = "308e9a3675fc3ea3862b7863eeead08c621dcc37ff59de597dd3cdab41450ad9"
	tTxHash *chainhash.Hash
	tP2PKH  []byte
	errTest = errors.New("test error")
)

type msgBlockWithHeight struct {
	msgBlock *Block
	height   int64
}

func makeRawTx(pkScripts []util.Bytes, inputs []*wire.TxIn) *Tx {
	tx := &wire.MsgTx{
		TxIn: inputs,
	}
	for _, pkScript := range pkScripts {
		tx.TxOut = append(tx.TxOut, wire.NewTxOut(1, pkScript))
	}
	return NewTxFromMsgTx(tx, MaxExpiryHeight)
}

func dummyInput() *wire.TxIn {
	return wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{0x01}, 0), nil, nil)
}

func dummyTx() *Tx {
	return makeRawTx([]util.Bytes{encode.RandomBytes(32)}, []*wire.TxIn{dummyInput()})
}

func signRawJSONTx(raw json.RawMessage) (*Tx, []byte) {
	var txB util.Bytes
	if err := json.Unmarshal(raw, &txB); err != nil {
		panic(fmt.Sprintf("error unmarshaling tx: %v", err))
	}
	tx, err := DeserializeTx(txB)
	if err != nil {
		panic(fmt.Sprintf("error deserializing tx: %v", err))
	}
	for i := range tx.TxIn {
		tx.TxIn[i].SignatureScript = encode.RandomBytes(btc.RedeemP2PKHSigScriptSize)
	}
	txB, err = tx.Bytes()
	if err != nil {
		panic(fmt.Sprintf("error serializing tx: %v", err))
	}
	return tx, txB
}

type tRPCClient struct {
	tipChanged   chan asset.WalletNotification
	responsesMtx sync.Mutex
	responses    map[string][]any

	// If there is an "any" key in the getTransactionMap, that value will be
	// returned for all requests. Otherwise the tx id is looked up.
	getTransactionMap map[string]*getTransactionResult
	getTransactionErr error

	blockchainMtx       sync.RWMutex
	verboseBlocks       map[string]*msgBlockWithHeight
	mainchain           map[int64]*chainhash.Hash
	getBestBlockHashErr error
}

func newRPCClient() *tRPCClient {
	genesisHash := chaincfg.MainNetParams.GenesisHash
	return &tRPCClient{
		responses:  make(map[string][]any),
		tipChanged: make(chan asset.WalletNotification),
		verboseBlocks: map[string]*msgBlockWithHeight{
			genesisHash.String(): {msgBlock: &Block{}},
		},
		mainchain: map[int64]*chainhash.Hash{
			0: genesisHash,
		},
	}
}

func (c *tRPCClient) queueResponse(method string, resp any) {
	c.responsesMtx.Lock()
	defer c.responsesMtx.Unlock()
	c.responses[method] = append(c.responses[method], resp)
}

func (c *tRPCClient) checkEmptiness(t *testing.T) {
	t.Helper()
	c.responsesMtx.Lock()
	defer c.responsesMtx.Unlock()
	var stillQueued = map[string]int{}
	for method, resps := range c.responses {
		if len(resps) > 0 {
			stillQueued[method] = len(resps)
			fmt.Printf("Method %s still has %d responses queued \n", method, len(resps))
		}
	}
	if len(stillQueued) > 0 {
		t.Fatalf("response queue not empty: %+v", stillQueued)
	}
}

func (c *tRPCClient) getBlock(blockHash string) *msgBlockWithHeight {
	c.blockchainMtx.Lock()
	defer c.blockchainMtx.Unlock()
	return c.verboseBlocks[blockHash]
}

func (c *tRPCClient) bestBlock() (*chainhash.Hash, int64) {
	c.blockchainMtx.RLock()
	defer c.blockchainMtx.RUnlock()
	var bestHash *chainhash.Hash
	var bestBlkHeight int64
	for height, hash := range c.mainchain {
		if height >= bestBlkHeight {
			bestBlkHeight = height
			bestHash = hash
		}
	}
	return bestHash, bestBlkHeight
}

func (c *tRPCClient) getBestBlockHeight() int64 {
	c.blockchainMtx.RLock()
	defer c.blockchainMtx.RUnlock()
	var bestBlkHeight int64
	for height := range c.mainchain {
		if height >= bestBlkHeight {
			bestBlkHeight = height
		}
	}
	return bestBlkHeight
}

func (c *tRPCClient) RawRequest(ctx context.Context, method string, args []json.RawMessage) (json.RawMessage, error) {
	respond := func(resp any) (json.RawMessage, error) {
		b, err := json.Marshal(resp)
		if err != nil {
			panic(fmt.Sprintf("error marshaling test response: %v", err))
		}
		return b, nil
	}

	c.responsesMtx.Lock()
	responses := c.responses[method]
	if len(responses) > 0 {
		resp := responses[0]
		c.responses[method] = responses[1:]
		c.responsesMtx.Unlock()
		if err, is := resp.(error); is {
			return nil, err
		}
		if f, is := resp.(func([]json.RawMessage) (json.RawMessage, error)); is {
			return f(args)
		}
		return respond(resp)
	}
	c.responsesMtx.Unlock()

	// Some methods are handled by the testing infrastructure
	switch method {
	case "getbestblockhash":
		c.blockchainMtx.RLock()
		if c.getBestBlockHashErr != nil {
			c.blockchainMtx.RUnlock()
			return nil, c.getBestBlockHashErr
		}
		c.blockchainMtx.RUnlock()
		bestHash, _ := c.bestBlock()
		return respond(bestHash.String())
	case "getblockhash":
		var blockHeight int64
		if err := json.Unmarshal(args[0], &blockHeight); err != nil {
			panic(fmt.Sprintf("error unmarshaling block height: %v", err))
		}
		c.blockchainMtx.RLock()
		defer c.blockchainMtx.RUnlock()
		for height, blockHash := range c.mainchain {
			if height == blockHeight {
				return respond(blockHash.String())
			}
		}
		return nil, fmt.Errorf("block not found")
	case "getblock":
		c.blockchainMtx.Lock()
		defer c.blockchainMtx.Unlock()
		var blockHashStr string
		if err := json.Unmarshal(args[0], &blockHashStr); err != nil {
			panic(fmt.Sprintf("error unmarshaling block hash: %v", err))
		}
		blk, found := c.verboseBlocks[blockHashStr]
		if !found {
			return nil, fmt.Errorf("block not found")
		}
		var buf bytes.Buffer
		err := blk.msgBlock.Serialize(&buf)
		if err != nil {
			return nil, fmt.Errorf("block serialization error: %v", err)
		}
		return respond(hex.EncodeToString(buf.Bytes()))
	case "getblockheader":
		var blkHash string
		if err := json.Unmarshal(args[0], &blkHash); err != nil {
			panic(fmt.Sprintf("error unmarshaling block hash: %v", err))
		}
		block := c.getBlock(blkHash)
		if block == nil {
			return nil, fmt.Errorf("no block verbose found")
		}
		// block may get modified concurrently, lock mtx before reading fields.
		c.blockchainMtx.RLock()
		defer c.blockchainMtx.RUnlock()
		return respond(&btc.BlockHeader{
			Hash:   block.msgBlock.BlockHash().String(),
			Height: block.height,
			// Confirmations: block.Confirmations,
			// Time:          block.Time,
		})
	case "gettransaction":
		if c.getTransactionErr != nil {
			return nil, c.getTransactionErr
		}

		c.blockchainMtx.Lock()
		defer c.blockchainMtx.Unlock()
		var txID string
		if err := json.Unmarshal(args[0], &txID); err != nil {
			panic(fmt.Sprintf("error unmarshaling block hash: %v", err))
		}
		var txData *getTransactionResult
		if c.getTransactionMap != nil {
			if txData = c.getTransactionMap["any"]; txData == nil {
				txData = c.getTransactionMap[txID]
			}
		}
		if txData == nil {
			return nil, walletTransactionNotFound
		}
		return respond(txData)
	case "signrawtransaction":
		_, txB := signRawJSONTx(args[0])
		return respond(&signTxResult{
			Hex:      txB,
			Complete: true,
		})
	}
	return nil, fmt.Errorf("no test response queued for  %q", method)
}

func tNewWallet() (*zecWallet, *tRPCClient, func()) {
	dataDir, err := os.MkdirTemp("", "")
	if err != nil {
		panic("couldn't create data dir:" + err.Error())
	}

	cl := newRPCClient()
	walletCfg := &asset.WalletConfig{
		Emit: asset.NewWalletEmitter(cl.tipChanged, BipID, tLogger),
		PeersChange: func(num uint32, err error) {
			fmt.Printf("peer count = %d, err = %v", num, err)
		},
		DataDir: dataDir,
		Settings: map[string]string{
			"rpcuser":     "a",
			"rpcpassword": "b",
		},
	}
	walletCtx, shutdown := context.WithCancel(tCtx)

	wi, err := NewWallet(walletCfg, tLogger, util.Simnet)
	if err != nil {
		panic(fmt.Sprintf("NewWallet error: %v", err))
	}

	w := wi.(*zecWallet)
	w.node = cl
	bestHash, _ := cl.bestBlock()
	w.currentTip = &blockVector{
		Height: cl.getBestBlockHeight(),
		Hash:   *bestHash,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.watchBlocks(walletCtx)
	}()
	shutdownAndWait := func() {
		shutdown()
		os.RemoveAll(dataDir)
		wg.Wait()
	}
	return w, cl, shutdownAndWait
}

func TestMain(m *testing.M) {
	tLogger = util.StdOutLogger("TEST", util.LevelCritical)
	var shutdown func()
	tCtx, shutdown = context.WithCancel(context.Background())
	tTxHash, _ = chainhash.NewHashFromStr(tTxID)
	tP2PKH, _ = hex.DecodeString("76a9148fc02268f208a61767504fe0b48d228641ba81e388ac")
	// tP2SH, _ = hex.DecodeString("76a91412a9abf5c32392f38bd8a1f57d81b1aeecc5699588ac")
	doIt := func() int {
		// Not counted as coverage, must test Archiver constructor explicitly.
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

func TestSend(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	const sendAmt uint64 = 1e8
	tAddr := string(tUnifiedAddr)
	w.lastAddress.Store(&tAddr)

	successOp := []*operationStatus{{
		Status: "success",
		Result: &opResult{TxID: tTxID},
	}}

	dTx := dummyTx()
	txB, _ := dTx.Bytes()
	tx := &getTransactionResult{
		Bytes: txB,
	}

	// Unified address.
	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsValid: true, AddressType: unifiedAddressType})
	cl.queueResponse(methodZListUnifiedReceivers, &unifiedReceivers{Transparent: tAddr})
	cl.queueResponse(methodZSendMany, "operationid123")
	cl.queueResponse(methodZGetOperationResult, successOp)
	cl.queueResponse("gettransaction", tx)
	if _, err := w.Send(tUnifiedAddr, sendAmt, 0); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	// Transparent address.
	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsValid: true, AddressType: transparentAddressType})
	cl.queueResponse(methodZSendMany, "operationid123")
	cl.queueResponse(methodZGetOperationResult, successOp)
	cl.queueResponse("gettransaction", tx)
	if _, err := w.Send(tAddr, sendAmt, 0); err != nil {
		t.Fatalf("Send error: %v", err)
	}
}

func TestSyncStatus(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	bci := &getBlockchainInfoResult{
		Headers: 100,
		Blocks:  99, // full node allowed to be synced when 1 block behind
	}

	const numConns = 2
	ni := &networkInfo{
		Connections: numConns,
	}

	// ok
	cl.queueResponse("getblockchaininfo", bci)
	cl.queueResponse("getnetworkinfo", ni)
	ss, err := w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus error (synced expected): %v", err)
	}
	if !ss.Synced {
		t.Fatalf("synced = false")
	}
	if ss.BlockProgress() < 1 {
		t.Fatalf("progress not complete when loading last block")
	}

	// getblockchaininfo error
	cl.queueResponse("getblockchaininfo", errTest)
	_, err = w.SyncStatus()
	if !errorHasCode(err, errGetChainInfo) {
		t.Fatalf("SyncStatus wrong error: %v", err)
	}

	// getnetworkinfo error
	cl.queueResponse("getblockchaininfo", bci)
	cl.queueResponse("getnetworkinfo", errTest)
	_, err = w.SyncStatus()
	if !errorHasCode(err, errGetNetInfo) {
		t.Fatalf("SyncStatus wrong error: %v", err)
	}

	// no peers is not synced = false
	ni.Connections = 0
	cl.queueResponse("getblockchaininfo", bci)
	cl.queueResponse("getnetworkinfo", ni)
	ss, err = w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus error (!synced expected): %v", err)
	}
	if ss.Synced {
		t.Fatalf("synced = true")
	}
	ni.Connections = 2

	// No headers is progress = 0
	bci.Headers = 0
	cl.queueResponse("getblockchaininfo", bci)
	ss, err = w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus error (no headers): %v", err)
	}
	if ss.Synced || ss.BlockProgress() != 0 {
		t.Fatal("wrong sync status for no headers", ss.Synced, ss.BlockProgress())
	}
	bci.Headers = 100

	// 50% synced
	w.tipAtConnect.Store(100)
	bci.Headers = 200
	bci.Blocks = 150
	cl.queueResponse("getblockchaininfo", bci)
	ss, err = w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus error (half-synced): %v", err)
	}
	if ss.Synced {
		t.Fatalf("synced = true for 50 blocks to go")
	}
	if ss.BlockProgress() > 0.500001 || ss.BlockProgress() < 0.4999999 {
		t.Fatalf("progress out of range. Expected 0.5, got %.2f", ss.BlockProgress())
	}
}
