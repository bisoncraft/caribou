//go:build !harness

package btc

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func boolPtr(v bool) *bool {
	return &v
}

func btcAddr(segwit bool) btcutil.Address {
	var addr btcutil.Address
	if segwit {
		addr, _ = btcutil.DecodeAddress(tP2WPKHAddr, &chaincfg.MainNetParams)
	} else {
		addr, _ = btcutil.DecodeAddress(tP2PKHAddr, &chaincfg.MainNetParams)
	}
	return addr
}

func randBytes(l int) []byte {
	b := make([]byte, l)
	crand.Read(b)
	return b
}

func signFunc(tx *wire.MsgTx, sizeTweak int, segwit bool) {
	// Set the sigScripts to random bytes of the correct length for spending a
	// p2pkh output.
	if segwit {
		sigSize := 73 + sizeTweak
		for i := range tx.TxIn {
			tx.TxIn[i].Witness = wire.TxWitness{
				randBytes(sigSize),
				randBytes(33),
			}
		}
	} else {
		scriptSize := RedeemP2PKHSigScriptSize + sizeTweak
		for i := range tx.TxIn {
			tx.TxIn[i].SignatureScript = randBytes(scriptSize)
		}
	}
}

type msgBlockWithHeight struct {
	msgBlock *wire.MsgBlock
	height   int64
}

type testData struct {
	walletCfg     *baseWalletConfig
	badSendHash   *chainhash.Hash
	sendErr       error
	sentRawTx     *wire.MsgTx
	txOutRes      *btcjson.GetTxOutResult
	txOutErr      error
	sigIncomplete bool
	signFunc      func(*wire.MsgTx)

	blockchainMtx       sync.RWMutex
	rnd                 *rand.Rand // guarded by blockchainMtx, see addRawTx
	verboseBlocks       map[chainhash.Hash]*msgBlockWithHeight
	dbBlockForTx        map[chainhash.Hash]*hashEntry
	mainchain           map[int64]*chainhash.Hash
	getBlockchainInfo   *GetBlockchainInfoResult
	getBestBlockHashErr error

	mempoolTxs        map[chainhash.Hash]*wire.MsgTx
	rawVerboseErr     error
	lockedCoins       []*RPCOutpoint
	estFeeErr         error
	listLockUnspent   []*RPCOutpoint
	getBalances       *GetBalancesResult
	getBalancesErr    error
	lockUnspentErr    error
	changeAddr        string
	changeAddrErr     error
	newAddress        string
	newAddressErr     error
	privKeyForAddr    *btcutil.WIF
	privKeyForAddrErr error
	birthdayTime      time.Time

	// If there is an "any" key in the getTransactionMap, that value will be
	// returned for all requests. Otherwise the tx id is looked up.
	getTransactionMap map[string]*GetTransactionResult
	getTransactionErr error

	getBlockchainInfoErr error
	unlockErr            error
	lockErr              error
	sendToAddress        string
	sendToAddressErr     error
	setTxFee             bool
	signTxErr            error
	listUnspent          []*ListUnspentResult
	listUnspentErr       error
	tipChanged           chan asset.WalletNotification

	// spv
	fetchInputInfoTx  *wire.MsgTx
	getCFilterScripts map[chainhash.Hash][][]byte
	checkpoints       map[OutPoint]*ScanCheckpoint
	confs             uint32
	confsSpent        bool
	confsErr          error
	walletTxSpent     bool
	txFee             uint64
	ownedAddresses    map[string]bool
	ownsAddress       bool
	locked            bool
	noPeers           bool
}

func newTestData() *testData {
	// setup genesis block, required by bestblock polling goroutine
	genesisHash := chaincfg.MainNetParams.GenesisHash
	return &testData{
		rnd:      rand.New(rand.NewPCG(0x12345678, 0x87654321)),
		txOutRes: newTxOutResult([]byte{}, 1, 0),
		verboseBlocks: map[chainhash.Hash]*msgBlockWithHeight{
			*genesisHash: {msgBlock: &wire.MsgBlock{}},
		},
		dbBlockForTx: make(map[chainhash.Hash]*hashEntry),
		mainchain: map[int64]*chainhash.Hash{
			0: genesisHash,
		},
		mempoolTxs:        make(map[chainhash.Hash]*wire.MsgTx),
		fetchInputInfoTx:  dummyTx(),
		getCFilterScripts: make(map[chainhash.Hash][][]byte),
		confsErr:          WalletTransactionNotFound,
		checkpoints:       make(map[OutPoint]*ScanCheckpoint),
		tipChanged:        make(chan asset.WalletNotification, 1),
		getTransactionMap: make(map[string]*GetTransactionResult),
	}
}

func (c *testData) GetTransactions(startHeight, endHeight int32, accountName string, cancel <-chan struct{}) (*wallet.GetTransactionsResult, error) {
	return nil, fmt.Errorf("not implemented")
}

func (c *testData) getBlock(blockHash chainhash.Hash) *msgBlockWithHeight {
	c.blockchainMtx.Lock()
	defer c.blockchainMtx.Unlock()
	return c.verboseBlocks[blockHash]
}

func (c *testData) GetBestBlockHeight() int64 {
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

func (c *testData) bestBlock() (*chainhash.Hash, int64) {
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

func encodeOrError(thing any, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	return json.Marshal(thing)
}

type tRawRequester struct {
	*testData
}

func (c *tRawRequester) RawRequest(_ context.Context, method string, params []json.RawMessage) (json.RawMessage, error) {
	switch method {
	// TODO: handle methodGetBlockHash and add actual tests to cover it.
	case methodGetNetworkInfo:
		return json.Marshal(&btcjson.GetNetworkInfoResult{
			Connections: 1,
		})
	case methodEstimateSmartFee:
		if c.testData.estFeeErr != nil {
			return nil, c.testData.estFeeErr
		}
		optimalRate := float64(optimalFeeRate) * 1e-5 // ~0.00024
		return json.Marshal(&btcjson.EstimateSmartFeeResult{
			Blocks:  2,
			FeeRate: &optimalRate,
		})
	case methodSendRawTransaction:
		var txHex string
		err := json.Unmarshal(params[0], &txHex)
		if err != nil {
			return nil, err
		}
		tx, err := msgTxFromHex(txHex)
		if err != nil {
			return nil, err
		}
		c.sentRawTx = tx
		if c.sendErr == nil && c.badSendHash == nil {
			h := tx.TxHash().String()
			return json.Marshal(&h)
		}
		if c.sendErr != nil {
			return nil, c.sendErr
		}
		return json.Marshal(c.badSendHash.String())
	case methodGetTxOut:
		return encodeOrError(c.txOutRes, c.txOutErr)
	case methodGetBestBlockHash:
		c.blockchainMtx.RLock()
		if c.getBestBlockHashErr != nil {
			c.blockchainMtx.RUnlock()
			return nil, c.getBestBlockHashErr
		}
		c.blockchainMtx.RUnlock()
		bestHash, _ := c.bestBlock()
		return json.Marshal(bestHash.String())
	case methodGetBlockHash:
		var blockHeight int64
		if err := json.Unmarshal(params[0], &blockHeight); err != nil {
			return nil, err
		}
		c.blockchainMtx.RLock()
		defer c.blockchainMtx.RUnlock()
		for height, blockHash := range c.mainchain {
			if height == blockHeight {
				return json.Marshal(blockHash.String())
			}
		}
		return nil, fmt.Errorf("block not found")

	case methodGetRawMempool:
		hashes := make([]string, 0, len(c.mempoolTxs))
		for txHash := range c.mempoolTxs {
			hashes = append(hashes, txHash.String())
		}
		return json.Marshal(hashes)
	case methodGetRawTransaction:
		if c.testData.rawVerboseErr != nil {
			return nil, c.testData.rawVerboseErr
		}
		var hashStr string
		err := json.Unmarshal(params[0], &hashStr)
		if err != nil {
			return nil, err
		}
		txHash, err := chainhash.NewHashFromStr(hashStr)
		if err != nil {
			return nil, err
		}
		msgTx := c.mempoolTxs[*txHash]
		if msgTx == nil {
			return nil, fmt.Errorf("transaction not found")
		}
		txB, _ := serializeMsgTx(msgTx)
		return json.Marshal(hex.EncodeToString(txB))
	case methodSignTx:
		if c.signTxErr != nil {
			return nil, c.signTxErr
		}
		signTxRes := SignTxResult{
			Complete: !c.sigIncomplete,
		}
		var msgHex string
		err := json.Unmarshal(params[0], &msgHex)
		if err != nil {
			return nil, fmt.Errorf("json.Unmarshal error for tRawRequester -> RawRequest -> methodSignTx: %v", err)
		}
		msgBytes, _ := hex.DecodeString(msgHex)
		txReader := bytes.NewReader(msgBytes)
		msgTx := wire.NewMsgTx(wire.TxVersion)
		err = msgTx.Deserialize(txReader)
		if err != nil {
			return nil, fmt.Errorf("MsgTx.Deserialize error for tRawRequester -> RawRequest -> methodSignTx: %v", err)
		}

		c.signFunc(msgTx)

		buf := new(bytes.Buffer)
		err = msgTx.Serialize(buf)
		if err != nil {
			return nil, fmt.Errorf("MsgTx.Serialize error for tRawRequester -> RawRequest -> methodSignTx: %v", err)
		}
		signTxRes.Hex = buf.Bytes()
		return mustMarshal(signTxRes), nil
	case methodGetBlock:
		c.blockchainMtx.Lock()
		defer c.blockchainMtx.Unlock()
		var blockHashStr string
		err := json.Unmarshal(params[0], &blockHashStr)
		if err != nil {
			return nil, err
		}
		blkHash, err := chainhash.NewHashFromStr(blockHashStr)
		if err != nil {
			return nil, err
		}
		blk, found := c.verboseBlocks[*blkHash]
		if !found {
			return nil, fmt.Errorf("block not found")
		}
		var buf bytes.Buffer
		err = blk.msgBlock.Serialize(&buf)
		if err != nil {
			return nil, err
		}
		return json.Marshal(hex.EncodeToString(buf.Bytes()))

	case methodGetBlockHeader:
		var blkHashStr string
		_ = json.Unmarshal(params[0], &blkHashStr)
		blkHash, err := chainhash.NewHashFromStr(blkHashStr)
		if err != nil {
			return nil, err
		}
		block := c.getBlock(*blkHash)
		if block == nil {
			return nil, fmt.Errorf("no block verbose found")
		}
		// block may get modified concurrently, lock mtx before reading fields.
		c.blockchainMtx.RLock()
		defer c.blockchainMtx.RUnlock()
		return json.Marshal(&BlockHeader{
			Hash:   blkHash.String(),
			Height: block.height,
			// Confirmations: block.Confirmations,
			// Time:          block.Time,
		})
	case methodLockUnspent:
		if c.lockUnspentErr != nil {
			return json.Marshal(false)
		}
		coins := make([]*RPCOutpoint, 0)
		_ = json.Unmarshal(params[1], &coins)
		if string(params[0]) == "false" {
			if c.lockedCoins != nil {
				c.lockedCoins = append(c.lockedCoins, coins...)
			} else {
				c.lockedCoins = coins
			}
		}
		return json.Marshal(true)
	case methodListLockUnspent:
		return mustMarshal(c.listLockUnspent), nil
	case methodGetBalances:
		return encodeOrError(c.getBalances, c.getBalancesErr)
	case methodChangeAddress:
		return encodeOrError(c.changeAddr, c.changeAddrErr)
	case methodNewAddress:
		return encodeOrError(c.newAddress, c.newAddressErr)
	case methodPrivKeyForAddress:
		if c.privKeyForAddrErr != nil {
			return nil, c.privKeyForAddrErr
		}
		return json.Marshal(c.privKeyForAddr.String())
	case methodGetTransaction:
		if c.getTransactionErr != nil {
			return nil, c.getTransactionErr
		}

		c.blockchainMtx.Lock()
		defer c.blockchainMtx.Unlock()
		var txID string
		err := json.Unmarshal(params[0], &txID)
		if err != nil {
			return nil, err
		}

		var txData *GetTransactionResult
		if c.getTransactionMap != nil {
			if txData = c.getTransactionMap["any"]; txData == nil {
				txData = c.getTransactionMap[txID]
			}
		}
		if txData == nil {
			return nil, WalletTransactionNotFound
		}
		return json.Marshal(txData)
	case methodGetBlockchainInfo:
		c.blockchainMtx.RLock()
		defer c.blockchainMtx.RUnlock()
		return encodeOrError(c.getBlockchainInfo, c.getBlockchainInfoErr)
	case methodLock:
		return nil, c.lockErr
	case methodUnlock:
		return nil, c.unlockErr
	case methodSendToAddress:
		return encodeOrError(c.sendToAddress, c.sendToAddressErr)
	case methodSetTxFee:
		return json.Marshal(c.setTxFee)
	case methodListUnspent:
		return encodeOrError(c.listUnspent, c.listUnspentErr)
	case methodFundRawTransaction:
		b, _ := serializeMsgTx(wire.NewMsgTx(wire.TxVersion))
		resp := &struct {
			Transaction string  `json:"hex"`
			Fee         float64 `json:"fee"`
		}{
			Transaction: hex.EncodeToString(b),
			Fee:         toBTC(c.txFee),
		}
		return json.Marshal(resp)
	case methodGetWalletInfo:
		return json.Marshal(&GetWalletInfoResult{UnlockedUntil: nil /* unencrypted -> unlocked */})
	case methodGetAddressInfo:
		var addr string
		err := json.Unmarshal(params[0], &addr)
		if err != nil {
			panic(err)
		}
		owns := c.ownedAddresses != nil && c.ownedAddresses[addr]
		if !owns {
			owns = c.ownsAddress
		}
		return json.Marshal(&btcjson.GetAddressInfoResult{
			IsMine: owns,
		})
	}
	panic("method not registered: " + method)
}

const testBlocksPerBlockTimeOffset = 4

func generateTestBlockTime(blockHeight int64) time.Time {
	return time.Unix(1e6, 0).Add(time.Duration(blockHeight) * maxFutureBlockTime / testBlocksPerBlockTimeOffset)
}

func (c *testData) addRawTx(blockHeight int64, tx *wire.MsgTx) (*chainhash.Hash, *wire.MsgBlock) {
	c.blockchainMtx.Lock()
	defer c.blockchainMtx.Unlock()
	blockHash, found := c.mainchain[blockHeight]
	if !found {
		prevBlock := &chainhash.Hash{}
		if blockHeight > 0 {
			var exists bool
			prevBlock, exists = c.mainchain[blockHeight-1]
			if !exists {
				prevBlock = &chainhash.Hash{}
			}
		}
		nonce, bits := c.rnd.Uint32(), c.rnd.Uint32()
		header := wire.NewBlockHeader(0, prevBlock, &chainhash.Hash{} /* lie, maybe fix this */, bits, nonce)
		header.Timestamp = generateTestBlockTime(blockHeight)
		msgBlock := wire.NewMsgBlock(header) // only now do we know the block hash
		hash := msgBlock.BlockHash()
		blockHash = &hash
		c.verboseBlocks[*blockHash] = &msgBlockWithHeight{
			msgBlock: msgBlock,
			height:   blockHeight,
		}
		c.mainchain[blockHeight] = blockHash
	}
	block := c.verboseBlocks[*blockHash]
	// NOTE: Adding a transaction changes the msgBlock.BlockHash() so the
	// map key is technically always wrong.
	block.msgBlock.AddTransaction(tx)
	return blockHash, block.msgBlock
}

func (c *testData) getBlockAtHeight(blockHeight int64) (*chainhash.Hash, *msgBlockWithHeight) {
	c.blockchainMtx.RLock()
	defer c.blockchainMtx.RUnlock()
	blockHash, found := c.mainchain[blockHeight]
	if !found {
		return nil, nil
	}
	blk := c.verboseBlocks[*blockHash]
	return blockHash, blk
}

func makeRawTx(pkScripts []util.Bytes, inputs []*wire.TxIn) *wire.MsgTx {
	tx := &wire.MsgTx{
		TxIn: inputs,
	}
	for _, pkScript := range pkScripts {
		tx.TxOut = append(tx.TxOut, wire.NewTxOut(1, pkScript))
	}
	return tx
}

// msgTxFromHex creates a wire.MsgTx by deserializing the hex-encoded
// transaction.
func msgTxFromHex(txHex string) (*wire.MsgTx, error) {
	return deserializeMsgTx(hex.NewDecoder(strings.NewReader(txHex)))
}

func dummyInput() *wire.TxIn {
	return wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{0x01}, 0), nil, nil)
}

func dummyTx() *wire.MsgTx {
	return makeRawTx([]util.Bytes{randBytes(32)}, []*wire.TxIn{dummyInput()})
}

func newTxOutResult(script []byte, value uint64, confs int64) *btcjson.GetTxOutResult {
	return &btcjson.GetTxOutResult{
		Confirmations: confs,
		Value:         float64(value) / 1e8,
		ScriptPubKey: btcjson.ScriptPubKeyResult{
			Hex: hex.EncodeToString(script),
		},
	}
}

func makeSwapContract(segwit bool, lockTimeOffset time.Duration) (secret []byte, secretHash [32]byte, pkScript, contract []byte, addr, contractAddr btcutil.Address, lockTime time.Time) {
	secret = randBytes(32)
	secretHash = sha256.Sum256(secret)

	addr = btcAddr(segwit)

	lockTime = time.Now().Add(lockTimeOffset)
	contract, err := MakeContract(addr, addr, secretHash[:], lockTime.Unix(), segwit, &chaincfg.MainNetParams)
	if err != nil {
		panic("error making swap contract:" + err.Error())
	}
	contractAddr, _ = scriptHashAddress(segwit, contract, &chaincfg.MainNetParams)
	pkScript, _ = txscript.PayToAddrScript(contractAddr)
	return
}

func tNewWallet(segwit bool, walletType string) (*intermediaryWallet, *testData, func()) {
	dataDir, err := os.MkdirTemp("", "")
	if err != nil {
		panic("couldn't create data dir:" + err.Error())
	}

	data := newTestData()
	walletCfg := &asset.WalletConfig{
		Emit: asset.NewWalletEmitter(data.tipChanged, BipID, tLogger),
		PeersChange: func(num uint32, err error) {
			fmt.Printf("peer count = %d, err = %v", num, err)
		},
		DataDir: dataDir,
	}
	walletCtx, shutdown := context.WithCancel(tCtx)
	cfg := &BTCCloneCFG{
		WalletCFG:           walletCfg,
		Symbol:              "btc",
		Logger:              tLogger,
		ChainParams:         &chaincfg.MainNetParams,
		WalletInfo:          WalletInfo,
		DefaultFallbackFee:  defaultFee,
		DefaultFeeRateLimit: defaultFeeRateLimit,
		Segwit:              segwit,
		FeeEstimator:        rpcFeeRate,
		AddressDecoder:      btcutil.DecodeAddress,
	}

	var wallet *intermediaryWallet
	switch walletType {
	case walletTypeRPC:
		wallet, err = newRPCWallet(&tRawRequester{data}, cfg, &RPCWalletConfig{})
	case walletTypeSPV:
		w, err := newUnconnectedWallet(cfg, &WalletConfig{})
		if err == nil {
			neutrinoClient := &tNeutrinoClient{data}
			spvw := &spvWallet{
				chainParams: &chaincfg.MainNetParams,
				cfg:         &WalletConfig{},
				wallet:      &tBtcWallet{testData: data},
				cl:          neutrinoClient,
				tipChan:     make(chan *BlockVector, 1),
				acctNum:     0,
				log:         cfg.Logger.SubLogger("SPV"),
				decodeAddr:  btcutil.DecodeAddress,
			}
			spvw.BlockFiltersScanner = NewBlockFiltersScanner(spvw, spvw.log)
			spvw.txBlocks = data.dbBlockForTx
			spvw.checkpoints = data.checkpoints
			w.setNode(spvw)
			wallet = &intermediaryWallet{
				baseWallet:     w,
				txFeeEstimator: spvw,
				tipRedeemer:    spvw,
			}
			wallet.prepareRedemptionFinder()
		}
	}

	data.walletCfg = wallet.cfgV.Load()

	if err != nil {
		shutdown()
		os.RemoveAll(dataDir)
		panic(err.Error())
	}
	// Initialize the best block.
	bestHash, err := wallet.node.GetBestBlockHash()
	if err != nil {
		shutdown()
		os.RemoveAll(dataDir)
		panic(err.Error())
	}
	wallet.ctx = walletCtx
	wallet.tipMtx.Lock()
	wallet.currentTip = &BlockVector{
		Height: data.GetBestBlockHeight(),
		Hash:   *bestHash,
	}
	wallet.tipMtx.Unlock()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		wallet.watchBlocks(walletCtx)
	}()
	shutdownAndWait := func() {
		shutdown()
		os.RemoveAll(dataDir)
		wg.Wait()
	}
	return wallet, data, shutdownAndWait
}

func mustMarshal(thing any) []byte {
	b, err := json.Marshal(thing)
	if err != nil {
		panic("mustMarshal error: " + err.Error())
	}
	return b
}

type testFunc func(t *testing.T, segwit bool, walletType string)

func runRubric(t *testing.T, f testFunc) {
	t.Run("rpc|segwit", func(t *testing.T) {
		t.Parallel()
		f(t, true, walletTypeRPC)
	})
	t.Run("rpc|non-segwit", func(t *testing.T) {
		t.Parallel()
		f(t, false, walletTypeRPC)
	})
	t.Run("spv|segwit", func(t *testing.T) {
		t.Parallel()
		f(t, true, walletTypeSPV)
	})
}

func decodeString(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

// Since ReturnCoins takes the asset.Coin interface, make sure any interface
// is acceptable.
type tCoin struct{ id []byte }

func (c *tCoin) ID() util.Bytes {
	if len(c.id) > 0 {
		return c.id
	}
	return make([]byte, 36)
}
func (c *tCoin) String() string { return hex.EncodeToString(c.id) }
func (c *tCoin) TxID() string   { return hex.EncodeToString(c.id) }
func (c *tCoin) Value() uint64  { return 100 }

type TAuditInfo struct{}

func (ai *TAuditInfo) Recipient() string      { return tP2PKHAddr }
func (ai *TAuditInfo) Expiration() time.Time  { return time.Time{} }
func (ai *TAuditInfo) Coin() asset.Coin       { return &tCoin{} }
func (ai *TAuditInfo) Contract() util.Bytes   { return nil }
func (ai *TAuditInfo) SecretHash() util.Bytes { return nil }

type tSenderType byte

const (
	tSendSender tSenderType = iota
	tWithdrawSender
)

func testSender(t *testing.T, senderType tSenderType, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()
	const feeSuggestion = 100
	sender := func(addr string, val uint64) (asset.Coin, error) {
		return wallet.Send(addr, val, defaultFee)
	}
	if senderType == tWithdrawSender {
		sender = func(addr string, val uint64) (asset.Coin, error) {
			return wallet.Withdraw(addr, val, feeSuggestion)
		}
	}

	node.signFunc = func(tx *wire.MsgTx) {
		signFunc(tx, 0, wallet.segwit)
	}

	addr := btcAddr(segwit)
	node.setTxFee = true
	node.changeAddr = btcAddr(segwit).String()

	pkScript, _ := txscript.PayToAddrScript(addr)
	tx := makeRawTx([]util.Bytes{randBytes(5), pkScript}, []*wire.TxIn{dummyInput()})
	txHash := tx.TxHash()

	changeAddr := btcAddr(segwit)
	node.changeAddr = changeAddr.String()

	expectedFees := func(numInputs int) uint64 {
		txSize := MinimumTxOverhead
		if segwit {
			txSize += (P2WPKHOutputSize * 2) + (numInputs * RedeemP2WPKHInputTotalSize)
		} else {
			txSize += (P2PKHOutputSize * 2) + (numInputs * RedeemP2PKHInputSize)
		}
		return uint64(txSize * feeSuggestion)
	}

	expectedSentVal := func(sendVal, fees uint64) uint64 {
		if senderType == tSendSender {
			return sendVal
		}
		return sendVal - fees
	}

	expectedChangeVal := func(totalInput, sendVal, fees uint64) uint64 {
		if senderType == tSendSender {
			return totalInput - sendVal - fees
		}
		return totalInput - sendVal
	}

	requiredVal := func(sendVal, fees uint64) uint64 {
		if senderType == tSendSender {
			return sendVal + fees
		}
		return sendVal
	}

	type test struct {
		name         string
		val          uint64
		unspents     []*ListUnspentResult
		bondReserves uint64

		expectedInputs []*OutPoint
		expectSentVal  uint64
		expectChange   uint64
		expectErr      bool
	}
	tests := []test{
		{
			name: "plenty of funds",
			val:  toSatoshi(5),
			unspents: []*ListUnspentResult{{
				TxID:          txHash.String(),
				Address:       addr.String(),
				Amount:        100,
				Confirmations: 1,
				Vout:          0,
				ScriptPubKey:  pkScript,
				SafePtr:       boolPtr(true),
				Spendable:     true,
			}},
			expectedInputs: []*OutPoint{
				{TxHash: txHash, Vout: 0},
			},
			expectSentVal: expectedSentVal(toSatoshi(5), expectedFees(1)),
			expectChange:  expectedChangeVal(toSatoshi(100), toSatoshi(5), expectedFees(1)),
		},
		{
			name: "just enough change for bond reserves",
			val:  toSatoshi(5),
			unspents: []*ListUnspentResult{{
				TxID:          txHash.String(),
				Address:       addr.String(),
				Amount:        5.2,
				Confirmations: 1,
				Vout:          0,
				ScriptPubKey:  pkScript,
				SafePtr:       boolPtr(true),
				Spendable:     true,
			}},
			expectedInputs: []*OutPoint{
				{TxHash: txHash,
					Vout: 0},
			},
			expectSentVal: expectedSentVal(toSatoshi(5), expectedFees(1)),
			expectChange:  expectedChangeVal(toSatoshi(5.2), toSatoshi(5), expectedFees(1)),
			bondReserves:  expectedChangeVal(toSatoshi(5.2), toSatoshi(5), expectedFees(1)),
		},
		{
			name: "not enough change for bond reserves",
			val:  toSatoshi(5),
			unspents: []*ListUnspentResult{{
				TxID:          txHash.String(),
				Address:       addr.String(),
				Amount:        5.2,
				Confirmations: 1,
				Vout:          0,
				ScriptPubKey:  pkScript,
				SafePtr:       boolPtr(true),
				Spendable:     true,
			}},
			expectedInputs: []*OutPoint{
				{TxHash: txHash, Vout: 0},
			},
			bondReserves: expectedChangeVal(toSatoshi(5.2), toSatoshi(5), expectedFees(1)) + 1,
			expectErr:    true,
		},
		{
			name: "1 satoshi less than needed",
			val:  toSatoshi(5),
			unspents: []*ListUnspentResult{{
				TxID:          txHash.String(),
				Address:       addr.String(),
				Amount:        toBTC(requiredVal(toSatoshi(5), expectedFees(1)) - 1),
				Confirmations: 1,
				Vout:          0,
				ScriptPubKey:  pkScript,
				SafePtr:       boolPtr(true),
				Spendable:     true,
			}},
			expectErr: true,
		},
		{
			name: "exact amount needed",
			val:  toSatoshi(5),
			unspents: []*ListUnspentResult{{
				TxID:          txHash.String(),
				Address:       addr.String(),
				Amount:        toBTC(requiredVal(toSatoshi(5), expectedFees(1))),
				Confirmations: 1,
				Vout:          0,
				ScriptPubKey:  pkScript,
				SafePtr:       boolPtr(true),
				Spendable:     true,
			}},
			expectedInputs: []*OutPoint{
				{TxHash: txHash, Vout: 0},
			},
			expectSentVal: expectedSentVal(toSatoshi(5), expectedFees(1)),
			expectChange:  0,
		},
	}

	for _, test := range tests {
		node.listUnspent = test.unspents
		wallet.bondReserves.Store(test.bondReserves)

		_, err := sender(addr.String(), test.val)
		if test.expectErr {
			if err == nil {
				t.Fatalf("%s: no error for expected error", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		tx := node.sentRawTx
		if len(test.expectedInputs) != len(tx.TxIn) {
			t.Fatalf("expected %d inputs, got %d", len(test.expectedInputs), len(tx.TxIn))
		}

		for i, input := range tx.TxIn {
			if input.PreviousOutPoint.Hash != test.expectedInputs[i].TxHash ||
				input.PreviousOutPoint.Index != test.expectedInputs[i].Vout {
				t.Fatalf("expected input %d to be %v, got %v", i, test.expectedInputs[i], input.PreviousOutPoint)
			}
		}

		if test.expectChange > 0 && len(tx.TxOut) != 2 {
			t.Fatalf("expected 2 outputs, got %d", len(tx.TxOut))
		}
		if test.expectChange == 0 && len(tx.TxOut) != 1 {
			t.Fatalf("expected 2 outputs, got %d", len(tx.TxOut))
		}

		if tx.TxOut[0].Value != int64(test.expectSentVal) {
			t.Fatalf("expected sent value to be %d, got %d", test.expectSentVal, tx.TxOut[0].Value)
		}

		if test.expectChange > 0 && tx.TxOut[1].Value != int64(test.expectChange) {
			t.Fatalf("expected change value to be %d, got %d", test.expectChange, tx.TxOut[1].Value)
		}
	}
}

func TestPrettyBTC(t *testing.T) {
	t.Parallel()
	type test struct {
		v   uint64
		exp string
	}

	tests := []*test{{
		v:   1,
		exp: "0.00000001",
	}, {
		v:   1e8,
		exp: "1",
	}, {
		v:   100000001,
		exp: "1.00000001",
	}, {
		v:   0,
		exp: "0",
	}, {
		v:   123000,
		exp: "0.00123",
	}, {
		v:   100123000,
		exp: "1.00123",
	}}

	for _, tt := range tests {
		if prettyBTC(tt.v) != tt.exp {
			t.Fatalf("prettyBTC(%d) = %s != %q", tt.v, prettyBTC(tt.v), tt.exp)
		}
	}
}

type tReconfigurer struct {
	*rpcClient
	restart bool
	err     error
}

func (r *tReconfigurer) Reconfigure(walletCfg *asset.WalletConfig, currentAddress string) (restartRequired bool, err error) {
	return r.restart, r.err
}

func TestReconfigure(t *testing.T) {
	t.Parallel()
	wallet, _, shutdown := tNewWallet(false, walletTypeRPC)
	// We don't need the wallet running, and we need to write to the node field,
	// which is used in the block loop.
	shutdown()

	reconfigurer := &tReconfigurer{rpcClient: wallet.node.(*rpcClient)}
	wallet.baseWallet.setNode(reconfigurer)

	cfg := &asset.WalletConfig{
		Settings: map[string]string{
			"redeemconftarget": "3",
		},
	}

	restart, err := wallet.Reconfigure(tCtx, cfg, "")
	if err != nil {
		t.Fatalf("initial Reconfigure error: %v", err)
	}
	if restart {
		t.Fatal("restart = false not propagated")
	}
	if wallet.redeemConfTarget() != 3 {
		t.Fatal("redeemconftarget not updated", wallet.redeemConfTarget())
	}

	cfg.Settings["redeemconftarget"] = "2"

	reconfigurer.err = errTest
	if _, err = wallet.Reconfigure(tCtx, cfg, ""); err == nil {
		t.Fatal("node reconfigure error not propagated")
	}
	reconfigurer.err = nil
	// Redeem target should be unchanged
	if wallet.redeemConfTarget() != 3 {
		t.Fatal("redeemconftarget updated for node reconfigure error", wallet.redeemConfTarget())
	}

	reconfigurer.restart = true
	if restart, err := wallet.Reconfigure(tCtx, cfg, ""); err != nil {
		t.Fatal("Reconfigure error for restart = true")
	} else if !restart {
		t.Fatal("restart = true not propagated")
	}
	reconfigurer.restart = false

	// One last success, and make sure baseWalletConfig is updated.
	if _, err := wallet.Reconfigure(tCtx, cfg, ""); err != nil {
		t.Fatalf("Reconfigure error for final success: %v", err)
	}
	// Redeem target should be changed now.
	if wallet.redeemConfTarget() != 2 {
		t.Fatal("redeemconftarget not updated in final success", wallet.redeemConfTarget())
	}
}

func TestAddressRecycling(t *testing.T) {
	t.Parallel()
	w, td, shutdown := tNewWallet(false, walletTypeSPV)
	defer shutdown()

	compareAddrLists := func(tag string, exp, actual []string) {
		if len(exp) != len(actual) {
			t.Fatalf("%s: Wrong number of recycled addrs. Expected %d, got %d", tag, len(exp), len(actual))
		}
		unfound := make(map[string]bool, len(exp))
		for _, addr := range exp {
			unfound[addr] = true
		}
		for _, addr := range actual {
			delete(unfound, addr)
		}
		if len(unfound) > 0 {
			t.Fatalf("%s: Wrong addresses stored. Expected %+v, got %+v", tag, exp, actual)
		}
	}

	checkAddrs := func(tag string, expAddrs ...string) {
		memList := make([]string, 0, len(w.ar.addrs))
		for addr := range w.ar.addrs {
			memList = append(memList, addr)
		}
		compareAddrLists(tag, expAddrs, memList)
	}

	addr1, _ := btcutil.NewAddressPubKeyHash(encode.RandomBytes(20), &chaincfg.MainNetParams)
	addr2, _ := btcutil.NewAddressPubKeyHash(encode.RandomBytes(20), &chaincfg.MainNetParams)

	w.ReturnRedemptionAddress(addr1.String())

	checkAddrs("first single addr", addr1.String())

	td.ownsAddress = true
	redemptionAddr, _ := w.recyclableAddress()
	if redemptionAddr != addr1.String() {
		t.Fatalf("recycled address not returned for redemption address")
	}

	contracts := make([][]byte, 2)
	contracts[0], _ = MakeContract(addr1, addr2, encode.RandomBytes(32), time.Now().Unix(), false, &chaincfg.MainNetParams)
	contracts[1], _ = MakeContract(addr2, addr1, encode.RandomBytes(32), time.Now().Unix(), false, &chaincfg.MainNetParams)

	w.ReturnRefundContracts(contracts)

	checkAddrs("two addrs", addr1.String(), addr2.String())

	// Check that unowned addresses are not returned
	td.ownsAddress = false
	td.newAddress = "1JoiKZz2QRd47ARtcYgvgxC9jhnre9aphv"
	priv, _ := btcec.NewPrivateKey()
	td.privKeyForAddr, _ = btcutil.NewWIF(priv, &chaincfg.MainNetParams, true)
	redemptionAddr, err := w.recyclableAddress()
	if err != nil {
		t.Fatalf("RedemptionAddress error: %v", err)
	}
	if redemptionAddr == addr1.String() || redemptionAddr == addr2.String() {
		t.Fatalf("unowned address returned (1)")
	}
	redemptionAddr, _ = w.recyclableAddress()
	if redemptionAddr == addr1.String() || redemptionAddr == addr2.String() {
		t.Fatalf("unowned address returned (2)")
	}

	checkAddrs("after unowned")

	// Check address loading.
	w.ReturnRefundContracts(contracts)

	w.ar.WriteRecycledAddrsToFile()
	b, _ := os.ReadFile(w.ar.recyclePath)
	var fileAddrs []string
	for addr := range strings.SplitSeq(string(b), "\n") {
		if addr == "" {
			continue
		}
		fileAddrs = append(fileAddrs, addr)
	}
	compareAddrLists("filecheck", []string{addr1.String(), addr2.String()}, fileAddrs)

	otherW, _ := newUnconnectedWallet(w.cloneParams, &WalletConfig{})
	if len(otherW.ar.addrs) != 2 {
		t.Fatalf("newly opened wallet didn't load recycled addrs")
	}

}

func TestFindBond(t *testing.T) {
	// Subtests share a single wallet/node fixture that each case mutates, so
	// this test is not parallelized.
	wallet, node, shutdown := tNewWallet(false, walletTypeRPC)
	defer shutdown()

	node.signFunc = func(tx *wire.MsgTx) {
		signFunc(tx, 0, wallet.segwit)
	}

	privBytes, _ := hex.DecodeString("b07209eec1a8fb6cfe5cb6ace36567406971a75c330db7101fb21bc679bc5330")
	bondKey, _ := btcec.PrivKeyFromBytes(privBytes)

	amt := uint64(500_000)
	acctID := [32]byte{}
	lockTime := time.Now().Add(time.Hour * 12)
	utxo := &ListUnspentResult{
		TxID:          tTxID,
		Address:       tP2PKHAddr,
		Amount:        1.0,
		Confirmations: 1,
		Spendable:     true,
		ScriptPubKey:  decodeString("76a914e114d5bb20cdbd75f3726f27c10423eb1332576288ac"),
	}
	node.listUnspent = []*ListUnspentResult{utxo}
	node.changeAddr = tP2PKHAddr
	node.newAddress = tP2PKHAddr

	bond, _, err := wallet.MakeBondTx(0, amt, 200, lockTime, bondKey, acctID[:])
	if err != nil {
		t.Fatal(err)
	}

	txRes := func(tx []byte) *GetTransactionResult {
		return &GetTransactionResult{
			BlockHash: hex.EncodeToString(randBytes(32)),
			Bytes:     tx,
		}
	}

	newBondTx := func() *wire.MsgTx {
		msgTx, err := msgTxFromBytes(bond.SignedTx)
		if err != nil {
			t.Fatal(err)
		}
		return msgTx
	}
	tooFewOutputs := newBondTx()
	tooFewOutputs.TxOut = tooFewOutputs.TxOut[2:]
	tooFewOutputsBytes, err := serializeMsgTx(tooFewOutputs)
	if err != nil {
		t.Fatal(err)
	}

	badBondScript := newBondTx()
	badBondScript.TxOut[1].PkScript = badBondScript.TxOut[1].PkScript[1:]
	badBondScriptBytes, err := serializeMsgTx(badBondScript)
	if err != nil {
		t.Fatal(err)
	}

	noBondMatch := newBondTx()
	noBondMatch.TxOut[0].PkScript = noBondMatch.TxOut[0].PkScript[1:]
	noBondMatchBytes, err := serializeMsgTx(noBondMatch)
	if err != nil {
		t.Fatal(err)
	}

	node.addRawTx(1, newBondTx())
	verboseBlocks := node.verboseBlocks
	for _, blk := range verboseBlocks {
		blk.msgBlock.Header.Timestamp = time.Now()
	}

	tests := []struct {
		name              string
		coinID            []byte
		txRes             *GetTransactionResult
		bestBlockErr      error
		getTransactionErr error
		verboseBlocks     map[chainhash.Hash]*msgBlockWithHeight
		searchUntil       time.Time
		wantErr           bool
	}{{
		name:   "ok",
		coinID: bond.CoinID,
		txRes:  txRes(bond.SignedTx),
	}, {
		name:              "ok with find blocks",
		coinID:            bond.CoinID,
		getTransactionErr: asset.CoinNotFoundError,
	}, {
		name:    "bad coin id",
		coinID:  make([]byte, 0),
		txRes:   txRes(bond.SignedTx),
		wantErr: true,
	}, {
		name:    "missing an output",
		coinID:  bond.CoinID,
		txRes:   txRes(tooFewOutputsBytes),
		wantErr: true,
	}, {
		name:    "bad bond commitment script",
		coinID:  bond.CoinID,
		txRes:   txRes(badBondScriptBytes),
		wantErr: true,
	}, {
		name:    "bond script does not match commitment",
		coinID:  bond.CoinID,
		txRes:   txRes(noBondMatchBytes),
		wantErr: true,
	}, {
		name:    "bad msgtx",
		coinID:  bond.CoinID,
		txRes:   txRes(bond.SignedTx[1:]),
		wantErr: true,
	}, {
		name:              "get best block error",
		coinID:            bond.CoinID,
		getTransactionErr: asset.CoinNotFoundError,
		bestBlockErr:      errors.New("some error"),
		wantErr:           true,
	}, {
		name:              "block not found",
		coinID:            bond.CoinID,
		getTransactionErr: asset.CoinNotFoundError,
		verboseBlocks:     map[chainhash.Hash]*msgBlockWithHeight{},
		wantErr:           true,
	}, {
		name:              "did not find by search until time",
		coinID:            bond.CoinID,
		getTransactionErr: asset.CoinNotFoundError,
		searchUntil:       time.Now().Add(time.Hour),
		wantErr:           true,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node.getTransactionMap["any"] = test.txRes
			node.getBestBlockHashErr = test.bestBlockErr
			node.verboseBlocks = verboseBlocks
			if test.verboseBlocks != nil {
				node.verboseBlocks = test.verboseBlocks
			}
			bd, err := wallet.FindBond(tCtx, test.coinID, test.searchUntil)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bd.CheckPrivKey(bondKey) {
				t.Fatal("pkh not equal")
			}
		})
	}
}

func TestIDUnknownTx(t *testing.T) {
	t.Parallel()
	t.Run("non-segwit", func(t *testing.T) {
		t.Parallel()
		testIDUnknownTx(t, false)
	})
	t.Run("segwit", func(t *testing.T) {
		t.Parallel()
		testIDUnknownTx(t, true)
	})
}

func testIDUnknownTx(t *testing.T, segwit bool) {
	// Swap Tx - any tx with p2sh outputs that is not a bond.
	_, _, swapPKScript, _, _, _, _ := makeSwapContract(true, time.Hour*12)
	swapTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, nil, nil)},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toSatoshi(1)), swapPKScript)},
	}

	// Redeem Tx
	addrStr := tP2PKHAddr
	if segwit {
		addrStr = tP2WPKHAddr
	}
	addr, _ := decodeAddress(addrStr, &chaincfg.MainNetParams)
	swapContract, _ := MakeContract(addr, addr, randBytes(32), time.Now().Unix(), segwit, &chaincfg.MainNetParams)
	txIn := wire.NewTxIn(&wire.OutPoint{}, nil, nil)
	if segwit {
		txIn.Witness = RedeemP2WSHContract(swapContract, randBytes(73), randBytes(33), randBytes(32))
	} else {
		txIn.SignatureScript, _ = RedeemP2SHContract(swapContract, randBytes(73), randBytes(33), randBytes(32))
	}
	redeemFee := 0.0000143
	redemptionTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{txIn},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toSatoshi(5-redeemFee)), tP2PKH)},
	}

	h2b := func(h string) []byte {
		b, _ := hex.DecodeString(h)
		return b
	}

	// Create Bond Tx
	bondLockTime := 1711637410
	bondID := h2b("0e39bbb09592fd00b7d770cc832ddf4d625ae3a0")
	accountID := h2b("a0836b39b5ceb84f422b8a8cd5940117087a8522457c6d81d200557652fbe6ea")
	bondContract, _ := MakeBondScript(0, uint32(bondLockTime), bondID)
	contractAddr, _ := scriptHashAddress(segwit, bondContract, &chaincfg.MainNetParams)
	bondPkScript, _ := txscript.PayToAddrScript(contractAddr)
	bondOutput := wire.NewTxOut(int64(toSatoshi(2)), bondPkScript)
	bondCommitPkScript, _ := bondPushDataScript(0, accountID, int64(bondLockTime), bondID)
	bondCommitmentOutput := wire.NewTxOut(0, bondCommitPkScript)
	createBondTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, nil, nil)},
		TxOut: []*wire.TxOut{bondOutput, bondCommitmentOutput},
	}

	// Redeem Bond Tx
	txIn = wire.NewTxIn(&wire.OutPoint{}, nil, nil)
	if segwit {
		txIn.Witness = RefundBondScriptSegwit(bondContract, randBytes(73), randBytes(33))
	} else {
		txIn.SignatureScript, _ = RefundBondScript(bondContract, randBytes(73), randBytes(33))
	}
	redeemBondTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{txIn},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toSatoshi(5)), tP2PKH)},
	}

	// Acceleration Tx
	accelerationTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, nil, nil)},
		TxOut: []*wire.TxOut{wire.NewTxOut(0, tP2PKH)},
	}

	// Split Tx
	splitTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, nil, nil)},
		TxOut: []*wire.TxOut{wire.NewTxOut(0, tP2PKH), wire.NewTxOut(0, tP2PKH)},
	}

	// Send Tx
	ourPkScript, cpPkScript := tP2PKH, tP2WPKH
	if segwit {
		ourPkScript, cpPkScript = tP2WPKH, tP2PKH
	}

	_, ourAddr, _, _ := txscript.ExtractPkScriptAddrs(ourPkScript, &chaincfg.MainNetParams)
	ourAddrStr := ourAddr[0].String()

	_, cpAddr, _, _ := txscript.ExtractPkScriptAddrs(cpPkScript, &chaincfg.MainNetParams)
	cpAddrStr := cpAddr[0].String()

	sendTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, nil, nil)},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toSatoshi(0.001)), ourPkScript), wire.NewTxOut(int64(toSatoshi(4)), cpPkScript)},
	}

	// Receive Tx
	receiveTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, nil, nil)},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toSatoshi(4)), ourPkScript), wire.NewTxOut(int64(toSatoshi(0.001)), cpPkScript)},
	}

	floatPtr := func(f float64) *float64 {
		return &f
	}

	type test struct {
		name           string
		ltr            *ListTransactionsResult
		tx             *wire.MsgTx
		ownedAddresses map[string]bool
		ownsAddress    bool
		exp            *txdb.WalletTransaction
	}

	tests := []*test{
		{
			name: "swap",
			ltr: &ListTransactionsResult{
				Send: true,
			},
			tx: swapTx,
			exp: &txdb.WalletTransaction{
				Type:   txdb.SwapOrSend,
				ID:     swapTx.TxHash().String(),
				Amount: toSatoshi(1),
			},
		},
		{
			name: "redeem",
			ltr: &ListTransactionsResult{
				Send: false,
				Fee:  &redeemFee,
			},
			tx: redemptionTx,
			exp: &txdb.WalletTransaction{
				Type:   txdb.Redeem,
				ID:     redemptionTx.TxHash().String(),
				Amount: toSatoshi(5),
				Fees:   toSatoshi(redeemFee),
			},
		},
		{
			name: "create bond",
			ltr: &ListTransactionsResult{
				Send: true,
				Fee:  floatPtr(0.0000222),
			},
			tx: createBondTx,
			exp: &txdb.WalletTransaction{
				Type:   txdb.CreateBond,
				ID:     createBondTx.TxHash().String(),
				Amount: toSatoshi(2),
				Fees:   toSatoshi(0.0000222),
				BondInfo: &txdb.BondTxInfo{
					AccountID: accountID,
					BondID:    bondID,
					LockTime:  uint64(bondLockTime),
				},
			},
		},
		{
			name: "redeem bond",
			ltr: &ListTransactionsResult{
				Send: false,
			},
			tx: redeemBondTx,
			exp: &txdb.WalletTransaction{
				Type:   txdb.RedeemBond,
				ID:     redeemBondTx.TxHash().String(),
				Amount: toSatoshi(5),
				BondInfo: &txdb.BondTxInfo{
					AccountID: []byte{},
					BondID:    bondID,
					LockTime:  uint64(bondLockTime),
				},
			},
		},
		{
			name: "acceleration",
			ltr: &ListTransactionsResult{
				Send: true,
			},
			tx: accelerationTx,
			exp: &txdb.WalletTransaction{
				Type: txdb.Acceleration,
				ID:   accelerationTx.TxHash().String(),
			},
			ownsAddress: true,
		},
		{
			name: "split",
			ltr: &ListTransactionsResult{
				Send: true,
			},
			tx: splitTx,
			exp: &txdb.WalletTransaction{
				Type: txdb.Split,
				ID:   splitTx.TxHash().String(),
			},
			ownsAddress: true,
		},
		{
			name: "send",
			ltr: &ListTransactionsResult{
				Send: true,
			},
			tx: sendTx,
			exp: &txdb.WalletTransaction{
				Type:      txdb.Send,
				ID:        sendTx.TxHash().String(),
				Amount:    toSatoshi(4),
				Recipient: &cpAddrStr,
			},
			ownedAddresses: map[string]bool{ourAddrStr: true},
		},
		{
			name: "receive",
			ltr:  &ListTransactionsResult{},
			tx:   receiveTx,
			exp: &txdb.WalletTransaction{
				Type:      txdb.Receive,
				ID:        receiveTx.TxHash().String(),
				Amount:    toSatoshi(4),
				Recipient: &ourAddrStr,
			},
			ownedAddresses: map[string]bool{ourAddrStr: true},
		},
	}

	runTest := func(tt *test) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wallet, node, shutdown := tNewWallet(true, walletTypeRPC)
			defer shutdown()
			txID := tt.tx.TxHash().String()
			tt.ltr.TxID = txID

			buf := new(bytes.Buffer)
			err := tt.tx.Serialize(buf)
			if err != nil {
				t.Fatalf("%s: error serializing tx: %v", tt.name, err)
			}

			node.getTransactionMap[txID] = &GetTransactionResult{Bytes: buf.Bytes()}
			node.ownedAddresses = tt.ownedAddresses
			node.ownsAddress = tt.ownsAddress
			wt, err := wallet.idUnknownTx(tt.ltr)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tt.name, err)
			}
			if !reflect.DeepEqual(wt, tt.exp) {
				t.Fatalf("%s: expected %+v, got %+v", tt.name, tt.exp, wt)
			}
		})
	}

	for _, tt := range tests {
		runTest(tt)
	}
}
