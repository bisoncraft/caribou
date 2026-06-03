//go:build !harness && !vspd

package dcr

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"decred.org/dcrwallet/v5/rpc/client/dcrwallet"
	walletjson "decred.org/dcrwallet/v5/rpc/jsonrpc/types"
	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/config"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrjson/v4"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/gcs/v4"
	"github.com/decred/dcrd/gcs/v4/blockcf2"
	chainjson "github.com/decred/dcrd/rpc/jsonrpc/types/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
)

var (
	tLogger        util.Logger
	tCtx           context.Context
	tLotSize       uint64 = 1e7
	optimalFeeRate uint64 = 22
	tErr                  = fmt.Errorf("test error")
	tTxID                 = "308e9a3675fc3ea3862b7863eeead08c621dcc37ff59de597dd3cdab41450ad9"
	tTxHash        *chainhash.Hash
	tPKHAddr       stdaddr.Address
	tP2PKHScript   []byte
	tChainParams   = chaincfg.MainNetParams()
	tAcctName      = "tAcctName"
)

func randBytes(l int) []byte {
	b := make([]byte, l)
	rand.Read(b)
	return b
}

func makeGetTxOutRes(confs, lots int64, pkScript []byte) *chainjson.GetTxOutResult {
	val := dcrutil.Amount(lots * int64(tLotSize)).ToCoin()
	return &chainjson.GetTxOutResult{
		Confirmations: confs,
		Value:         val,
		ScriptPubKey: chainjson.ScriptPubKeyResult{
			Hex: hex.EncodeToString(pkScript),
		},
	}
}

func makeTxHex(inputs []*wire.TxIn, pkScripts []util.Bytes) (string, error) {
	msgTx := wire.NewMsgTx()
	msgTx.TxIn = inputs
	for _, pkScript := range pkScripts {
		txOut := wire.NewTxOut(100000000, pkScript)
		msgTx.AddTxOut(txOut)
	}
	txBuf := bytes.NewBuffer(make([]byte, 0, msgTx.SerializeSize()))
	err := msgTx.Serialize(txBuf)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(txBuf.Bytes()), nil
}

// sometimes we want to not start monitor blocks to avoid a race condition in
// case we replace the wallet.
func tNewWalletMonitorBlocks(monitorBlocks bool) (*ExchangeWallet, *tRPCClient, func()) {
	client := newTRPCClient()
	log := tLogger.SubLogger("trpc")
	walletCfg := &asset.WalletConfig{
		Emit:        asset.NewWalletEmitter(client.emitC, BipID, log),
		PeersChange: func(uint32, error) {},
	}
	walletCtx, shutdown := context.WithCancel(tCtx)

	wallet, err := unconnectedWallet(walletCfg, &walletConfig{}, tChainParams, tLogger, util.Simnet)
	if err != nil {
		shutdown()
		panic(err.Error())
	}
	wallet.connected.Store(true)
	rpcw := &rpcWallet{
		rpcClient: client,
		log:       log,
	}
	rpcw.accountsV.Store(XCWalletAccounts{
		PrimaryAccount: tAcctName,
	})
	wallet.wallet = rpcw
	wallet.ctx = walletCtx

	// Initialize the best block.
	tip, _ := wallet.getBestBlock(walletCtx)
	wallet.currentTip.Store(tip)

	if monitorBlocks {
		go wallet.monitorBlocks(walletCtx)
	}

	return wallet, client, shutdown

}

func tNewWallet() (*ExchangeWallet, *tRPCClient, func()) {
	return tNewWalletMonitorBlocks(true)
}

func signFunc(msgTx *wire.MsgTx, scriptSize int) (*wire.MsgTx, bool, error) {
	for i := range msgTx.TxIn {
		msgTx.TxIn[i].SignatureScript = randBytes(scriptSize)
	}
	return msgTx, true, nil
}

type tRPCClient struct {
	sendRawHash    *chainhash.Hash
	sendRawErr     error
	sentRawTx      *wire.MsgTx
	txOutRes       map[outPoint]*chainjson.GetTxOutResult
	txOutErr       error
	bestBlockErr   error
	mempoolErr     error
	unspent        []walletjson.ListUnspentResult
	unspentErr     error
	balanceResult  *walletjson.GetBalanceResult
	balanceErr     error
	lockUnspentErr error
	changeAddr     stdaddr.Address
	changeAddrErr  error
	newAddr        stdaddr.Address
	newAddrErr     error
	signFunc       func(tx *wire.MsgTx) (*wire.MsgTx, bool, error)
	privWIF        *dcrutil.WIF
	privWIFErr     error
	walletTxFn     func() (*walletjson.GetTransactionResult, error)
	lockErr        error
	passErr        error
	disconnected   bool
	rawRes         map[string]json.RawMessage
	rawErr         map[string]error
	blockchain     *tBlockchain
	lluCoins       []walletjson.ListUnspentResult // Returned from ListLockUnspent
	lockedCoins    []*wire.OutPoint               // Last submitted to LockUnspent
	listLockedErr  error
	estFeeErr      error
	emitC          chan asset.WalletNotification
	// tickets
	purchasedTickets   [][]*chainhash.Hash
	purchaseTicketsErr error
	stakeInfo          walletjson.GetStakeInfoResult
	validateAddress    map[string]*walletjson.ValidateAddressResult
	abandonTxErr       error
}

type wireTxWithHeight struct {
	tx     *wire.MsgTx
	height int64
}

type tBlockchain struct {
	mtx               sync.RWMutex
	rawTxs            map[chainhash.Hash]*wireTxWithHeight
	mainchain         map[int64]*chainhash.Hash
	verboseBlocks     map[chainhash.Hash]*wire.MsgBlock
	blockHeaders      map[chainhash.Hash]*wire.BlockHeader
	v2CFilterBuilders map[chainhash.Hash]*tV2CFilterBuilder
}

func (blockchain *tBlockchain) addRawTx(blockHeight int64, tx *wire.MsgTx) (*chainhash.Hash, *wire.MsgBlock) {
	blockchain.mtx.Lock()
	defer blockchain.mtx.Unlock()

	blockchain.rawTxs[tx.TxHash()] = &wireTxWithHeight{tx, blockHeight}

	if blockHeight < 0 {
		return nil, nil
	}

	prevBlockHash := blockchain.mainchain[blockHeight-1]

	// Mined tx. Add to block.
	block := blockchain.blockAt(blockHeight)
	if block == nil {
		block = &wire.MsgBlock{
			Header: wire.BlockHeader{
				PrevBlock: *prevBlockHash,
				Height:    uint32(blockHeight),
				VoteBits:  1,
				Timestamp: time.Now(),
			},
		}
	}
	block.Transactions = append(block.Transactions, tx)
	blockHash := block.BlockHash()
	blockFilterBuilder := blockchain.v2CFilterBuilders[blockHash]
	blockchain.mainchain[blockHeight] = &blockHash
	blockchain.verboseBlocks[blockHash] = block

	blockchain.blockHeaders[blockHash] = &block.Header

	// Save prevout and output scripts in block cfilters.
	if blockFilterBuilder == nil {
		blockFilterBuilder = &tV2CFilterBuilder{}
		copy(blockFilterBuilder.key[:], randBytes(16))

	}
	blockchain.v2CFilterBuilders[blockHash] = blockFilterBuilder
	for _, txIn := range tx.TxIn {
		prevOut := &txIn.PreviousOutPoint
		prevTx, found := blockchain.rawTxs[prevOut.Hash]
		if !found || len(prevTx.tx.TxOut) <= int(prevOut.Index) {
			continue
		}
		blockFilterBuilder.data.AddRegularPkScript(prevTx.tx.TxOut[prevOut.Index].PkScript)
	}
	for _, txOut := range tx.TxOut {
		blockFilterBuilder.data.AddRegularPkScript(txOut.PkScript)
	}

	return &blockHash, block
}

// blockchain.mtx lock should be held for writes.
func (blockchain *tBlockchain) blockAt(height int64) *wire.MsgBlock {
	blkHash, found := blockchain.mainchain[height]
	if found {
		return blockchain.verboseBlocks[*blkHash]
	}

	return nil
}

type tV2CFilterBuilder struct {
	data blockcf2.Entries
	key  [gcs.KeySize]byte
}

func (filterBuilder *tV2CFilterBuilder) build() (*gcs.FilterV2, error) {
	return gcs.NewFilterV2(blockcf2.B, blockcf2.M, filterBuilder.key, filterBuilder.data)
}

func defaultSignFunc(tx *wire.MsgTx) (*wire.MsgTx, bool, error) { return tx, true, nil }

func newTRPCClient() *tRPCClient {
	// setup genesis block, required by bestblock polling goroutine
	var newHash chainhash.Hash
	copy(newHash[:], randBytes(32))
	return &tRPCClient{
		txOutRes: make(map[outPoint]*chainjson.GetTxOutResult),
		blockchain: &tBlockchain{
			rawTxs: map[chainhash.Hash]*wireTxWithHeight{},
			mainchain: map[int64]*chainhash.Hash{
				0: &newHash,
			},
			verboseBlocks: map[chainhash.Hash]*wire.MsgBlock{
				newHash: {},
			},
			blockHeaders: map[chainhash.Hash]*wire.BlockHeader{
				newHash: {},
			},
			v2CFilterBuilders: map[chainhash.Hash]*tV2CFilterBuilder{},
		},
		signFunc: defaultSignFunc,
		rawRes:   make(map[string]json.RawMessage),
		rawErr:   make(map[string]error),
		emitC:    make(chan asset.WalletNotification, 128),
	}
}

func (c *tRPCClient) GetCurrentNet(context.Context) (wire.CurrencyNet, error) {
	return tChainParams.Net, nil
}

func (c *tRPCClient) EstimateSmartFee(_ context.Context, confirmations int64, mode chainjson.EstimateSmartFeeMode) (*chainjson.EstimateSmartFeeResult, error) {
	if c.estFeeErr != nil {
		return nil, c.estFeeErr
	}
	optimalRate := float64(optimalFeeRate) * 1e-5 // optimalFeeRate: 22 atoms/byte = 0.00022 DCR/KB * 1e8 atoms/DCR * 1e-3 KB/Byte
	// fmt.Println((float64(optimalFeeRate)*1e-5)-0.00022)
	return &chainjson.EstimateSmartFeeResult{FeeRate: optimalRate}, nil
}

func (c *tRPCClient) SendRawTransaction(_ context.Context, tx *wire.MsgTx, allowHighFees bool) (*chainhash.Hash, error) {
	c.sentRawTx = tx
	if c.sendRawErr == nil && c.sendRawHash == nil {
		h := tx.TxHash()
		return &h, nil
	}
	return c.sendRawHash, c.sendRawErr
}

func (c *tRPCClient) GetTxOut(_ context.Context, txHash *chainhash.Hash, vout uint32, tree int8, mempool bool) (*chainjson.GetTxOutResult, error) {
	return c.txOutRes[newOutPoint(txHash, vout)], c.txOutErr
}

func (c *tRPCClient) GetBestBlock(_ context.Context) (*chainhash.Hash, int64, error) {
	if c.bestBlockErr != nil {
		return nil, -1, c.bestBlockErr
	}
	bestHash, bestBlkHeight := c.getBestBlock()
	return bestHash, bestBlkHeight, nil
}

func (c *tRPCClient) getBestBlock() (*chainhash.Hash, int64) {
	c.blockchain.mtx.RLock()
	defer c.blockchain.mtx.RUnlock()
	var bestHash *chainhash.Hash
	var bestBlkHeight int64
	for height, hash := range c.blockchain.mainchain {
		if height >= bestBlkHeight {
			bestBlkHeight = height
			bestHash = hash
		}
	}
	return bestHash, bestBlkHeight
}

func (c *tRPCClient) GetBlockHash(_ context.Context, blockHeight int64) (*chainhash.Hash, error) {
	c.blockchain.mtx.RLock()
	defer c.blockchain.mtx.RUnlock()
	h, found := c.blockchain.mainchain[blockHeight]
	if !found {
		return nil, fmt.Errorf("no test block at height %d", blockHeight)
	}
	return h, nil
}

func (c *tRPCClient) GetBlock(_ context.Context, blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	c.blockchain.mtx.RLock()
	defer c.blockchain.mtx.RUnlock()
	blk, found := c.blockchain.verboseBlocks[*blockHash]
	if !found {
		return nil, fmt.Errorf("no test block found for %s", blockHash)
	}
	return blk, nil
}

func (c *tRPCClient) GetBlockHeaderVerbose(_ context.Context, blockHash *chainhash.Hash) (*chainjson.GetBlockHeaderVerboseResult, error) {
	c.blockchain.mtx.RLock()
	defer c.blockchain.mtx.RUnlock()
	hdr, found := c.blockchain.blockHeaders[*blockHash]
	if !found {
		return nil, fmt.Errorf("no test block header found for %s", blockHash)
	}
	return &chainjson.GetBlockHeaderVerboseResult{
		Height:        hdr.Height,
		Hash:          blockHash.String(),
		PreviousHash:  hdr.PrevBlock.String(),
		Confirmations: 1,  // just not -1, which indicates side chain
		NextHash:      "", // empty string signals that it is tip
	}, nil
}

func (c *tRPCClient) GetBlockHeader(_ context.Context, blockHash *chainhash.Hash) (*wire.BlockHeader, error) {
	c.blockchain.mtx.RLock()
	defer c.blockchain.mtx.RUnlock()
	hdr, found := c.blockchain.blockHeaders[*blockHash]
	if !found {
		return nil, fmt.Errorf("no test block header found for %s", blockHash)
	}
	return hdr, nil
}

func (c *tRPCClient) GetRawMempool(_ context.Context, txType chainjson.GetRawMempoolTxTypeCmd) ([]*chainhash.Hash, error) {
	if c.mempoolErr != nil {
		return nil, c.mempoolErr
	}
	c.blockchain.mtx.RLock()
	defer c.blockchain.mtx.RUnlock()
	txHashes := make([]*chainhash.Hash, 0)
	for _, tx := range c.blockchain.rawTxs {
		if tx.height < 0 {
			txHash := tx.tx.TxHash()
			txHashes = append(txHashes, &txHash)
		}
	}
	return txHashes, nil
}

func (c *tRPCClient) GetBalanceMinConf(_ context.Context, account string, minConfirms int) (*walletjson.GetBalanceResult, error) {
	return c.balanceResult, c.balanceErr
}

func (c *tRPCClient) LockUnspent(_ context.Context, unlock bool, ops []*wire.OutPoint) error {
	if unlock == false {
		if c.lockedCoins == nil {
			c.lockedCoins = ops
		} else {
			c.lockedCoins = append(c.lockedCoins, ops...)
		}
	}
	return c.lockUnspentErr
}

func (c *tRPCClient) GetRawChangeAddress(_ context.Context, account string, net stdaddr.AddressParams) (stdaddr.Address, error) {
	return c.changeAddr, c.changeAddrErr
}

func (c *tRPCClient) GetNewAddressGapPolicy(_ context.Context, account string, gapPolicy dcrwallet.GapPolicy) (stdaddr.Address, error) {
	return c.newAddr, c.newAddrErr
}

func (c *tRPCClient) DumpPrivKey(_ context.Context, address stdaddr.Address) (*dcrutil.WIF, error) {
	return c.privWIF, c.privWIFErr
}

func (c *tRPCClient) GetTransaction(_ context.Context, txHash *chainhash.Hash) (*walletjson.GetTransactionResult, error) {
	if c.walletTxFn != nil {
		return c.walletTxFn()
	}
	c.blockchain.mtx.RLock()
	defer c.blockchain.mtx.RUnlock()
	if rawTx, has := c.blockchain.rawTxs[*txHash]; has {
		b, err := rawTx.tx.Bytes()
		if err != nil {
			return nil, err
		}
		walletTx := &walletjson.GetTransactionResult{
			Hex: hex.EncodeToString(b),
		}
		return walletTx, nil
	}
	return nil, dcrjson.NewRPCError(dcrjson.ErrRPCNoTxInfo, "no test transaction")
}

func (c *tRPCClient) AccountUnlocked(_ context.Context, acct string) (*walletjson.AccountUnlockedResult, error) {
	return &walletjson.AccountUnlockedResult{}, nil // go the walletlock/walletpassphrase route
}

func (c *tRPCClient) LockAccount(_ context.Context, acct string) error       { return nil }
func (c *tRPCClient) UnlockAccount(_ context.Context, acct, pw string) error { return nil }

func (c *tRPCClient) WalletLock(_ context.Context) error {
	return c.lockErr
}

func (c *tRPCClient) WalletPassphrase(_ context.Context, passphrase string, timeoutSecs int64) error {
	return c.passErr
}

func (c *tRPCClient) WalletInfo(_ context.Context) (*walletjson.WalletInfoResult, error) {
	return &walletjson.WalletInfoResult{
		Unlocked: true,
	}, nil
}

func (c *tRPCClient) ValidateAddress(_ context.Context, address stdaddr.Address) (*walletjson.ValidateAddressWalletResult, error) {
	if c.validateAddress != nil {
		if c.validateAddress[address.String()] != nil {
			return c.validateAddress[address.String()], nil
		}
		return &walletjson.ValidateAddressWalletResult{}, nil
	}

	return &walletjson.ValidateAddressWalletResult{
		IsMine:  true,
		Account: tAcctName,
	}, nil
}

func (c *tRPCClient) Disconnected() bool {
	return c.disconnected
}

func (c *tRPCClient) GetStakeInfo(ctx context.Context) (*walletjson.GetStakeInfoResult, error) {
	return &c.stakeInfo, nil
}

func (c *tRPCClient) PurchaseTicket(ctx context.Context, fromAccount string, minConf *int,
	numTickets *int,
	expiry *int, ticketChange *bool, ticketFee *dcrutil.Amount) (tix []*chainhash.Hash, _ error) {

	if c.purchaseTicketsErr != nil {
		return nil, c.purchaseTicketsErr
	}

	if len(c.purchasedTickets) > 0 {
		tix = c.purchasedTickets[0]
		c.purchasedTickets = c.purchasedTickets[1:]
	}

	return tix, nil
}

func (c *tRPCClient) GetTickets(ctx context.Context, includeImmature bool) ([]*chainhash.Hash, error) {
	return nil, nil
}

func (c *tRPCClient) GetVoteChoices(ctx context.Context) (*walletjson.GetVoteChoicesResult, error) {
	return nil, nil
}

func (c *tRPCClient) SetVoteChoice(ctx context.Context, agendaID, choiceID string) error {
	return nil
}

func (c *tRPCClient) RawRequest(_ context.Context, method string, params []json.RawMessage) (json.RawMessage, error) {
	if rr, found := c.rawRes[method]; found {
		return rr, c.rawErr[method] // err probably should be nil, but respect the config
	}
	if re, found := c.rawErr[method]; found {
		return nil, re
	}

	switch method {
	case methodGetPeerInfo:
		return json.Marshal([]*walletjson.GetPeerInfoResult{
			{
				Addr: "127.0.0.1",
			},
		})
	case methodGetCFilterV2:
		if len(params) != 1 {
			return nil, fmt.Errorf("getcfilterv2 requires 1 param, got %d", len(params))
		}

		var hashStr string
		json.Unmarshal(params[0], &hashStr)
		blkHash, _ := chainhash.NewHashFromStr(hashStr)

		c.blockchain.mtx.RLock()
		defer c.blockchain.mtx.RUnlock()
		blockFilterBuilder := c.blockchain.v2CFilterBuilders[*blkHash]
		if blockFilterBuilder == nil {
			return nil, fmt.Errorf("cfilters builder not found for block %s", blkHash)
		}
		v2CFilter, err := blockFilterBuilder.build()
		if err != nil {
			return nil, err
		}
		res := &walletjson.GetCFilterV2Result{
			BlockHash: blkHash.String(),
			Filter:    hex.EncodeToString(v2CFilter.Bytes()),
			Key:       hex.EncodeToString(blockFilterBuilder.key[:]),
		}
		return json.Marshal(res)

	case methodListUnspent:
		if c.unspentErr != nil {
			return nil, c.unspentErr
		}

		var acct string
		if len(params) > 3 {
			// filter with provided acct param
			_ = json.Unmarshal(params[3], &acct)
		}
		allAccts := acct == "" || acct == "*"

		var unspents []walletjson.ListUnspentResult
		for _, unspent := range c.unspent {
			if allAccts || unspent.Account == acct {
				unspents = append(unspents, unspent)
			}
		}

		response, _ := json.Marshal(unspents)
		return response, nil

	case methodListLockUnspent:
		if c.listLockedErr != nil {
			return nil, c.listLockedErr
		}

		var acct string
		if len(params) > 0 {
			_ = json.Unmarshal(params[0], &acct)
		}
		allAccts := acct == "" || acct == "*"

		var locked []chainjson.TransactionInput
		for _, utxo := range c.lluCoins {
			if allAccts || utxo.Account == acct {
				locked = append(locked, chainjson.TransactionInput{
					Txid:   utxo.TxID,
					Amount: utxo.Amount,
					Vout:   utxo.Vout,
					Tree:   utxo.Tree,
				})
			}
		}
		response, _ := json.Marshal(locked)
		return response, nil

	case methodSignRawTransaction:
		if len(params) != 1 {
			return nil, fmt.Errorf("needed 1 param")
		}

		var msgTxHex string
		err := json.Unmarshal(params[0], &msgTxHex)
		if err != nil {
			return nil, err
		}

		msgTx, err := msgTxFromHex(msgTxHex)
		if err != nil {
			res := walletjson.SignRawTransactionResult{
				Hex: msgTxHex,
				Errors: []walletjson.SignRawTransactionError{
					{
						TxID:  msgTx.CachedTxHash().String(),
						Error: err.Error(),
					},
				},
				// Complete stays false.
			}
			return json.Marshal(&res)
		}

		if c.signFunc == nil {
			return nil, fmt.Errorf("no signFunc configured")
		}

		signedTx, complete, err := c.signFunc(msgTx)
		if err != nil {
			res := walletjson.SignRawTransactionResult{
				Hex: msgTxHex,
				Errors: []walletjson.SignRawTransactionError{
					{
						TxID:  msgTx.CachedTxHash().String(),
						Error: err.Error(),
					},
				},
				// Complete stays false.
			}
			return json.Marshal(&res)
		}

		txHex, err := msgTxToHex(signedTx)
		if err != nil {
			return nil, fmt.Errorf("failed to encode MsgTx: %w", err)
		}

		res := walletjson.SignRawTransactionResult{
			Hex:      txHex,
			Complete: complete,
		}
		return json.Marshal(&res)

	case methodWalletInfo:
		return json.Marshal(new(walletjson.WalletInfoResult))
	}

	return nil, fmt.Errorf("method %v not implemented by (*tRPCClient).RawRequest", method)
}

func (c *tRPCClient) SetTxFee(ctx context.Context, fee dcrutil.Amount) error {
	return nil
}

func (c *tRPCClient) GetReceivedByAddressMinConf(ctx context.Context, address stdaddr.Address, minConfs int) (dcrutil.Amount, error) {
	return 0, nil
}

func (c *tRPCClient) ListSinceBlock(ctx context.Context, hash *chainhash.Hash) (*walletjson.ListSinceBlockResult, error) {
	return nil, nil
}

func (c *tRPCClient) AbandonTransaction(_ context.Context, hash *chainhash.Hash) error {
	return c.abandonTxErr
}

func (c *tRPCClient) CommittedTickets(ctx context.Context, tickets []*chainhash.Hash) ([]*chainhash.Hash, []stdaddr.Address, error) {
	return nil, nil, nil
}

func (c *tRPCClient) GetAccount(ctx context.Context, address stdaddr.Address) (string, error) {
	return "", nil
}

func (c *tRPCClient) SignMessage(ctx context.Context, addr stdaddr.Address, msg string) (string, error) {
	return "", nil
}

func TestMain(m *testing.M) {
	tChainParams = chaincfg.MainNetParams()
	tPKHAddr, _ = stdaddr.DecodeAddress("DsTya4cCFBgtofDLiRhkyPYEQjgs3HnarVP", tChainParams)
	tLogger = util.StdOutLogger("TEST", util.LevelTrace)
	var shutdown func()
	tCtx, shutdown = context.WithCancel(context.Background())
	tTxHash, _ = chainhash.NewHashFromStr(tTxID)
	tP2PKHScript, _ = hex.DecodeString("76a9148fc02268f208a61767504fe0b48d228641ba81e388ac")
	// tP2SH, _ = hex.DecodeString("76a91412a9abf5c32392f38bd8a1f57d81b1aeecc5699588ac")
	doIt := func() int {
		// Not counted as coverage, must test Archiver constructor explicitly.
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

// Since ReturnCoins takes the wallet.Coin interface, make sure any interface
// is acceptable.
type tCoin struct{ id []byte }

func (c *tCoin) ID() util.Bytes {
	if len(c.id) > 0 {
		return c.id
	}
	return make([]byte, 36)
}
func (c *tCoin) String() string                                    { return hex.EncodeToString(c.id) }
func (c *tCoin) Value() uint64                                     { return 100 }
func (c *tCoin) Confirmations(ctx context.Context) (uint32, error) { return 2, nil }
func (c *tCoin) TxID() string                                      { return hex.EncodeToString(c.id) }

type TAuditInfo struct{}

func (ai *TAuditInfo) Recipient() string      { return tPKHAddr.String() }
func (ai *TAuditInfo) Expiration() time.Time  { return time.Time{} }
func (ai *TAuditInfo) Coin() asset.Coin       { return &tCoin{} }
func (ai *TAuditInfo) Contract() util.Bytes   { return nil }
func (ai *TAuditInfo) SecretHash() util.Bytes { return nil }

type tSenderType byte

const (
	tSendSender tSenderType = iota
	tWithdrawSender
)

func testSender(t *testing.T, senderType tSenderType) {
	wallet, node, shutdown := tNewWallet()
	defer shutdown()

	var sendVal uint64 = 1e8
	var unspentVal uint64 = 100e8
	const feeSuggestion = 100
	funName := "Send"
	sender := func(addr string, val uint64) (asset.Coin, error) {
		return wallet.Send(addr, val, feeSuggestion)
	}
	if senderType == tWithdrawSender {
		funName = "Withdraw"
		// For withdraw, test with unspent total = withdraw value
		unspentVal = sendVal
		sender = func(addr string, val uint64) (asset.Coin, error) {
			return wallet.Withdraw(addr, val, feeSuggestion)
		}
	}

	addr := tPKHAddr.String()
	node.changeAddr = tPKHAddr

	node.unspent = []walletjson.ListUnspentResult{{
		TxID:          tTxID,
		Address:       tPKHAddr.String(),
		Account:       tAcctName,
		Amount:        float64(unspentVal) / 1e8,
		Confirmations: 5,
		ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
		Spendable:     true,
	}}
	//node.unspent = append(node.unspent, node.unspent[0])

	_, err := sender(addr, sendVal)
	if err != nil {
		t.Fatalf(funName+" error: %v", err)
	}

	// invalid address
	_, err = sender("badaddr", sendVal)
	if err == nil {
		t.Fatalf("no error for bad address: %v", err)
	}

	// GetRawChangeAddress error
	if senderType == tSendSender { // withdraw test does not get a change address
		node.changeAddrErr = tErr
		_, err = sender(addr, sendVal)
		if err == nil {
			t.Fatalf("no error for rawchangeaddress: %v", err)
		}
		node.changeAddrErr = nil
	}

	// good again
	_, err = sender(addr, sendVal)
	if err != nil {
		t.Fatalf(funName+" error afterwards: %v", err)
	}
}

func TestWithdraw(t *testing.T) {
	testSender(t, tWithdrawSender)
}

func TestSend(t *testing.T) {
	testSender(t, tSendSender)
}

func Test_withdraw(t *testing.T) {
	wallet, node, shutdown := tNewWallet()
	defer shutdown()

	address := tPKHAddr.String()
	node.changeAddr = tPKHAddr

	var unspentVal uint64 = 100e8
	node.unspent = []walletjson.ListUnspentResult{{
		TxID:          tTxID,
		Address:       tPKHAddr.String(),
		Account:       tAcctName,
		Amount:        float64(unspentVal) / 1e8,
		Confirmations: 5,
		ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
		Spendable:     true,
	}}

	addr, err := stdaddr.DecodeAddress(address, tChainParams)
	if err != nil {
		t.Fatal(err)
	}

	// This should make a msgTx with one input and one output.
	msgTx, val, err := wallet.withdraw(addr, unspentVal, optimalFeeRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgTx.TxOut) != 1 {
		t.Fatalf("expected 1 output, got %d", len(msgTx.TxOut))
	}
	if val != uint64(msgTx.TxOut[0].Value) {
		t.Errorf("expected non-change output to be %d, got %d", val, msgTx.TxOut[0].Value)
	}
	if val >= unspentVal {
		t.Errorf("expected output to be have fees deducted")
	}

	// Then with unspentVal just slightly larger than send. This should still
	// make a msgTx with one output, but larger than before. The sent value is
	// SMALLER than requested because it was required for fees.
	avail := unspentVal + 77
	node.unspent[0].Amount = float64(avail) / 1e8
	msgTx, val, err = wallet.withdraw(addr, unspentVal, optimalFeeRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgTx.TxOut) != 1 {
		t.Fatalf("expected 1 output, got %d", len(msgTx.TxOut))
	}
	if val != uint64(msgTx.TxOut[0].Value) {
		t.Errorf("expected non-change output to be %d, got %d", val, msgTx.TxOut[0].Value)
	}
	if val >= unspentVal {
		t.Errorf("expected output to be have fees deducted")
	}

	// Still no change, but this time the sent value is LARGER than requested
	// because change would be dust, and we don't over pay fees.
	avail = unspentVal + 3000
	node.unspent[0].Amount = float64(avail) / 1e8
	msgTx, val, err = wallet.withdraw(addr, unspentVal, optimalFeeRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgTx.TxOut) != 1 {
		t.Fatalf("expected 1 output, got %d", len(msgTx.TxOut))
	}
	if val <= unspentVal {
		t.Errorf("expected output to be more thrifty")
	}

	// Then with unspentVal considerably larger than (double) send. This should
	// make a msgTx with two outputs as the change is no longer dust. The change
	// should be exactly unspentVal and the sent amount should be
	// unspentVal-fees.
	node.unspent[0].Amount = float64(unspentVal*2) / 1e8
	msgTx, val, err = wallet.withdraw(addr, unspentVal, optimalFeeRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgTx.TxOut) != 2 {
		t.Fatalf("expected 2 outputs, got %d", len(msgTx.TxOut))
	}
	if val != uint64(msgTx.TxOut[0].Value) {
		t.Errorf("expected non-change output to be %d, got %d", val, msgTx.TxOut[0].Value)
	}
	if unspentVal != uint64(msgTx.TxOut[1].Value) {
		t.Errorf("expected change output to be %d, got %d", unspentVal, msgTx.TxOut[1].Value)
	}
}

func TestWithdrawTxSizeValidation(t *testing.T) {
	wallet, node, shutdown := tNewWallet()
	defer shutdown()

	node.changeAddr = tPKHAddr
	node.signFunc = func(msgTx *wire.MsgTx) (*wire.MsgTx, bool, error) {
		return signFunc(msgTx, P2PKHSigScriptSize)
	}

	// Test 1: Normal transaction with single UTXO (should succeed)
	t.Run("normal tx size", func(t *testing.T) {
		node.unspent = []walletjson.ListUnspentResult{{
			TxID:          tTxID,
			Address:       tPKHAddr.String(),
			Account:       tAcctName,
			Amount:        100.0, // 100 DCR
			Confirmations: 5,
			ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
			Spendable:     true,
		}}

		_, err := wallet.Withdraw(tPKHAddr.String(), 90e8, 10)
		if err != nil {
			t.Fatalf("normal tx failed: %v", err)
		}
	})

	// Test 2: Transaction with many inputs but under hard limit (should succeed)
	t.Run("tx near but under size limit", func(t *testing.T) {
		// 500 inputs * 166 bytes/input = 83,000 bytes + overhead < 100KB hard limit
		unspents := make([]walletjson.ListUnspentResult, 500)
		for i := 0; i < 500; i++ {
			txID := hex.EncodeToString(encode.RandomBytes(32))
			unspents[i] = walletjson.ListUnspentResult{
				TxID:          txID,
				Vout:          0,
				Address:       tPKHAddr.String(),
				Account:       tAcctName,
				Amount:        0.01, // 0.01 DCR each = 5 DCR total
				Confirmations: 1,
				ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
				Spendable:     true,
			}
		}
		node.unspent = unspents

		// Withdraw 4.9 DCR (requires ~490 inputs)
		_, err := wallet.Withdraw(tPKHAddr.String(), 490e6, 10)
		if err != nil {
			t.Fatalf("under-limit tx failed: %v", err)
		}
	})

	// Test 3: Transaction exceeding hard size limit (should fail at broadcast stage)
	t.Run("tx exceeds hard size limit", func(t *testing.T) {
		// 650 inputs * 166 bytes/input = 107,900 bytes > 100KB hard limit
		unspents := make([]walletjson.ListUnspentResult, 650)
		for i := 0; i < 650; i++ {
			txID := hex.EncodeToString(encode.RandomBytes(32))
			unspents[i] = walletjson.ListUnspentResult{
				TxID:          txID,
				Vout:          0,
				Address:       tPKHAddr.String(),
				Account:       tAcctName,
				Amount:        0.01, // 0.01 DCR each = 6.5 DCR total
				Confirmations: 1,
				ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
				Spendable:     true,
			}
		}
		node.unspent = unspents

		// Try to withdraw 6.4 DCR (requires ~640 inputs - exceeds hard limit)
		_, err := wallet.Withdraw(tPKHAddr.String(), 640e6, 10)
		if err == nil {
			t.Fatalf("expected error for oversized tx, got nil")
		}

		// Verify error message relates to size limit
		if !strings.Contains(err.Error(), "exceed") && !strings.Contains(err.Error(), "size") {
			t.Fatalf("expected size-related error, got: %v", err)
		}
	})

	// Test 4: Verify hard limit protection
	t.Run("hard limit verification", func(t *testing.T) {
		// Hard limit in broadcastTx() prevents oversized transactions
		unspents := make([]walletjson.ListUnspentResult, 700)
		for i := 0; i < 700; i++ {
			txID := hex.EncodeToString(encode.RandomBytes(32))
			unspents[i] = walletjson.ListUnspentResult{
				TxID:          txID,
				Vout:          0,
				Address:       tPKHAddr.String(),
				Account:       tAcctName,
				Amount:        0.01, // 0.01 DCR each
				Confirmations: 1,
				ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
				Spendable:     true,
			}
		}
		node.unspent = unspents

		// Try to withdraw amount requiring 700 inputs
		// 700 inputs * 166 bytes = 116,200 bytes > 100KB hard limit
		_, err := wallet.Withdraw(tPKHAddr.String(), 690e6, 10)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}

		// Verify error is size-related (caught by hard limit in broadcastTx)
		errStr := err.Error()
		if !strings.Contains(errStr, "exceed") && !strings.Contains(errStr, "size") {
			t.Fatalf("expected size-related error, got: %v", err)
		}
	})

	// Test 5: Verify hard limit uses MaxStandardTxSize (100KB) not MaxTxSize (393KB)
	// This test ensures the bug fix is working correctly
	t.Run("hard limit at maxstandardtxsize boundary", func(t *testing.T) {
		// Test transactions at exactly 100KB boundary
		// 602 inputs × 166 bytes ≈ 99,932 bytes (under limit) - should PASS
		// 603 inputs × 166 bytes ≈ 100,098 bytes (over limit) - should FAIL

		// Test 5a: Transaction just under 100KB limit (should succeed)
		unspents := make([]walletjson.ListUnspentResult, 602)
		for i := 0; i < 602; i++ {
			txID := hex.EncodeToString(encode.RandomBytes(32))
			unspents[i] = walletjson.ListUnspentResult{
				TxID:          txID,
				Vout:          0,
				Address:       tPKHAddr.String(),
				Account:       tAcctName,
				Amount:        0.01,
				Confirmations: 1,
				ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
				Spendable:     true,
			}
		}
		node.unspent = unspents

		// This should succeed (just under 100KB)
		_, err := wallet.Withdraw(tPKHAddr.String(), 595e6, 10)
		if err != nil {
			t.Fatalf("expected success for tx under 100KB limit, got: %v", err)
		}

		// Test 5b: Transaction just over 100KB limit (should fail with specific error)
		unspents = make([]walletjson.ListUnspentResult, 650)
		for i := 0; i < 650; i++ {
			txID := hex.EncodeToString(encode.RandomBytes(32))
			unspents[i] = walletjson.ListUnspentResult{
				TxID:          txID,
				Vout:          0,
				Address:       tPKHAddr.String(),
				Account:       tAcctName,
				Amount:        0.01,
				Confirmations: 1,
				ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
				Spendable:     true,
			}
		}
		node.unspent = unspents

		// This should fail (over 100KB)
		_, err = wallet.Withdraw(tPKHAddr.String(), 640e6, 10)
		if err == nil {
			t.Fatal("expected error for transaction > 100KB, got nil")
		}

		// Verify error mentions size limit
		errStr := err.Error()
		if !strings.Contains(errStr, "exceed") && !strings.Contains(errStr, "size") {
			t.Errorf("error should mention exceeding size limit, got: %v", err)
		}

		// This test confirms hard limit is aligned with mempool policy (100KB)
		// not consensus limit (393KB), fixing the bug where transactions
		// between 100-393KB could pass validation but get stuck in mempool
	})
}

func TestBroadcastTxDustValidation(t *testing.T) {
	wallet, _, shutdown := tNewWallet()
	defer shutdown()

	feeRate := uint64(10) // 10 atoms/byte

	// Helper to create a simple P2PKH output
	createP2PKHOutput := func(value int64) *wire.TxOut {
		return newTxOut(value, 0, tP2PKHScript)
	}

	// Helper to create OP_RETURN output
	createOPReturnOutput := func() *wire.TxOut {
		// OP_RETURN script
		builder := txscript.NewScriptBuilder()
		builder.AddOp(txscript.OP_RETURN)
		builder.AddData([]byte("test data"))
		script, _ := builder.Script()
		return newTxOut(0, 0, script)
	}

	// Test 1: Normal transaction with non-dust output (should succeed)
	t.Run("normal tx with non-dust output", func(t *testing.T) {
		tx := wire.NewMsgTx()
		// Add dummy input
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, 1e8, nil))
		// Add non-dust output (1 DCR = 100,000,000 atoms)
		tx.AddTxOut(createP2PKHOutput(1e8))

		_, err := wallet.broadcastTx(tCtx, tx, feeRate)
		if err != nil {
			// Error might be from SendRawTransaction, but should not be dust error
			if strings.Contains(err.Error(), "dust") {
				t.Fatalf("normal tx should not trigger dust error: %v", err)
			}
			// Other errors (like sendrawtx connection issues) are okay in test
		}
	})

	// Test 2: Transaction with dust output (should fail)
	t.Run("tx with dust output", func(t *testing.T) {
		tx := wire.NewMsgTx()
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, 10000, nil))
		// Add dust output
		// For P2PKH (36 bytes), at 10 atoms/byte: min = 10 * (36 + 165) * 3 = 6,030 atoms
		// 5000 atoms is below threshold
		tx.AddTxOut(createP2PKHOutput(5000))

		_, err := wallet.broadcastTx(tCtx, tx, feeRate)
		if err == nil {
			t.Fatal("expected dust error, got nil")
		}

		errStr := err.Error()
		if !strings.Contains(errStr, "dust") {
			t.Fatalf("expected dust error, got: %v", err)
		}
		if !strings.Contains(errStr, "output 0") {
			t.Fatalf("error should identify output index, got: %v", err)
		}
	})

	// Test 3: Transaction with OP_RETURN output (should succeed)
	t.Run("tx with OP_RETURN output", func(t *testing.T) {
		tx := wire.NewMsgTx()
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, 1e8, nil))
		// OP_RETURN outputs are allowed to be zero value
		tx.AddTxOut(createOPReturnOutput())
		// Add a normal non-dust output as well
		tx.AddTxOut(createP2PKHOutput(1e8))

		_, err := wallet.broadcastTx(tCtx, tx, feeRate)
		if err != nil {
			// Should not be dust error since OP_RETURN is skipped
			if strings.Contains(err.Error(), "dust") {
				t.Fatalf("OP_RETURN should not trigger dust error: %v", err)
			}
		}
	})

	// Test 4: Transaction at dust boundary (edge case)
	t.Run("tx at dust boundary", func(t *testing.T) {
		tx := wire.NewMsgTx()
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, 10000, nil))
		// Exactly at dust threshold
		// For P2PKH: min = 10 * (36 + 165) * 3 = 6,030 atoms
		tx.AddTxOut(createP2PKHOutput(6030))

		_, err := wallet.broadcastTx(tCtx, tx, feeRate)
		// At exact boundary, should NOT be dust
		if err != nil && strings.Contains(err.Error(), "dust") {
			t.Fatalf("boundary value should not be dust: %v", err)
		}
	})

	// Test 5: Multiple outputs, one is dust (should fail and identify correct output)
	t.Run("multiple outputs with one dust", func(t *testing.T) {
		tx := wire.NewMsgTx()
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, 2e8, nil))
		// Output 0: Non-dust
		tx.AddTxOut(createP2PKHOutput(1e8))
		// Output 1: Dust
		tx.AddTxOut(createP2PKHOutput(5000))
		// Output 2: Non-dust
		tx.AddTxOut(createP2PKHOutput(1e8))

		_, err := wallet.broadcastTx(tCtx, tx, feeRate)
		if err == nil {
			t.Fatal("expected dust error, got nil")
		}

		errStr := err.Error()
		if !strings.Contains(errStr, "dust") {
			t.Fatalf("expected dust error, got: %v", err)
		}
		// Should identify output 1 as dust
		if !strings.Contains(errStr, "output 1") {
			t.Fatalf("error should identify output 1 as dust, got: %v", err)
		}
	})

	// Test 6: Zero fee rate should use fallback (should not panic)
	t.Run("zero fee rate uses fallback", func(t *testing.T) {
		tx := wire.NewMsgTx()
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, 1e8, nil))
		tx.AddTxOut(createP2PKHOutput(1e8))

		// Pass zero fee rate - should use fallback
		_, err := wallet.broadcastTx(tCtx, tx, 0)
		if err != nil && strings.Contains(err.Error(), "dust") {
			t.Fatalf("fallback fee rate should not cause dust error for 1 DCR: %v", err)
		}
		// Other errors are fine, just testing dust logic doesn't panic
	})
}

func Test_sendToAddress(t *testing.T) {
	wallet, node, shutdown := tNewWallet()
	defer shutdown()

	address := tPKHAddr.String()
	node.changeAddr = tPKHAddr

	var unspentVal uint64 = 100e8
	node.unspent = []walletjson.ListUnspentResult{{
		TxID:          tTxID,
		Address:       tPKHAddr.String(),
		Account:       tAcctName,
		Amount:        float64(unspentVal) / 1e8,
		Confirmations: 5,
		ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
		Spendable:     true,
	}}

	addr, err := stdaddr.DecodeAddress(address, tChainParams)
	if err != nil {
		t.Fatal(err)
	}

	// This should return an error, not enough funds to send.
	_, _, _, err = wallet.sendToAddress(addr, unspentVal, optimalFeeRate)
	if err == nil {
		t.Fatal("Expected error, not enough funds to send.")
	}

	// With a lower send value, send should be successful.
	var sendVal uint64 = 10e8
	node.unspent[0].Amount = float64(unspentVal)
	msgTx, val, _, err := wallet.sendToAddress(addr, sendVal, optimalFeeRate)
	if err != nil {
		t.Fatal(err)
	}
	if val != uint64(msgTx.TxOut[0].Value) {
		t.Errorf("expected non-change output to be %d, got %d", val, msgTx.TxOut[0].Value)
	}
	if val != sendVal {
		t.Errorf("expected non-change output to be %d, got %d", sendVal, val)
	}
}

func TestLookupTxOutput(t *testing.T) {
	wallet, node, shutdown := tNewWallet()
	defer shutdown()

	coinID := make([]byte, 36)
	copy(coinID[:32], tTxHash[:])
	op := newOutPoint(tTxHash, 0)

	// Bad output coin
	op.vout = 10
	_, _, spent, err := wallet.lookupTxOutput(context.Background(), &op.txHash, op.vout)
	if err == nil {
		t.Fatalf("no error for bad output coin")
	}
	if spent != 0 {
		t.Fatalf("spent is not 0 for bad output coin")
	}
	op.vout = 0

	// Add the txOutRes with 2 confs and BestBlock correctly set.
	node.txOutRes[op] = makeGetTxOutRes(2, 1, tP2PKHScript)
	_, confs, spent, err := wallet.lookupTxOutput(context.Background(), &op.txHash, op.vout)
	if err != nil {
		t.Fatalf("unexpected error for gettxout path: %v", err)
	}
	if confs != 2 {
		t.Fatalf("confs not retrieved from gettxout path. expected 2, got %d", confs)
	}
	if spent != 0 {
		t.Fatalf("expected spent = 0 for gettxout path, got true")
	}

	// gettransaction error
	delete(node.txOutRes, op)
	walletTx := &walletjson.GetTransactionResult{}
	node.walletTxFn = func() (*walletjson.GetTransactionResult, error) {
		return walletTx, tErr
	}
	_, _, spent, err = wallet.lookupTxOutput(context.Background(), &op.txHash, op.vout)
	if err == nil {
		t.Fatalf("no error for gettransaction error")
	}
	if spent != 0 {
		t.Fatalf("spent is not 0 with gettransaction error")
	}
	node.walletTxFn = func() (*walletjson.GetTransactionResult, error) {
		return walletTx, nil
	}

	// wallet.lookupTxOutput will check if the tx is confirmed, its hex
	// is valid and contains an output at index 0, for the output to be
	// considered spent.
	tx := wire.NewMsgTx()
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{
		PkScript: tP2PKHScript,
	})
	txHex, err := msgTxToHex(tx)
	if err != nil {
		t.Fatalf("error preparing tx hex with 1 output: %v", err)
	}
	walletTx.Hex = txHex // unconfirmed = unspent

	_, _, spent, err = wallet.lookupTxOutput(context.Background(), &op.txHash, op.vout)
	if err != nil {
		t.Fatalf("unexpected error for gettransaction path (unconfirmed): %v", err)
	}
	if spent != 0 {
		t.Fatalf("expected spent = 0 for gettransaction path (unconfirmed), got true")
	}

	// Confirmed wallet tx without gettxout response is spent.
	walletTx.Confirmations = 2
	_, _, spent, err = wallet.lookupTxOutput(context.Background(), &op.txHash, op.vout)
	if err != nil {
		t.Fatalf("unexpected error for gettransaction path (confirmed): %v", err)
	}
	if spent != 1 {
		t.Fatalf("expected spent = 1 for gettransaction path (confirmed), got false")
	}

	// In spv mode, spent status is unknown without a block filters scan.
	wallet.wallet.(*rpcWallet).spvMode = true
	_, _, spent, err = wallet.lookupTxOutput(context.Background(), &op.txHash, op.vout)
	if err != nil {
		t.Fatalf("unexpected error for spv gettransaction path (non-wallet output): %v", err)
	}
	if spent != -1 {
		t.Fatalf("expected spent = -1 for spv gettransaction path (non-wallet output), got true")
	}

	// In spv mode, output is spent if it pays to the wallet (but no txOutRes).
	/* what is the use case for this since a contract never pays to wallet?
	node.walletTx.Details = []walletjson.GetTransactionDetailsResult{{
		Vout:     0,
		Category: "receive", // output at index 0 pays to the wallet
	}}
	_, _, spent, err = wallet.lookupTxOutput(context.Background(), &op.txHash, op.vout)
	if err != nil {
		t.Fatalf("unexpected error for spv gettransaction path (wallet output): %v", err)
	}
	if spent != 1 {
		t.Fatalf("expected spent = 1 for spv gettransaction path (wallet output), got false")
	}
	*/
}

func TestSendEdges(t *testing.T) {
	wallet, node, shutdown := tNewWallet()
	defer shutdown()

	const feeRate uint64 = 3

	const swapVal = 2e8 // leaving untyped. NewTxOut wants int64

	contractAddr, _ := stdaddr.NewAddressScriptHashV0(randBytes(20), tChainParams)
	// See IsDust for the source of this dustCoverage voodoo.
	dustCoverage := (P2PKHOutputSize + 165) * feeRate * 3
	dexReqFees := InitTxSize * feeRate

	_, pkScript := contractAddr.PaymentScript()

	newBaseTx := func(funding uint64) *wire.MsgTx {
		baseTx := wire.NewMsgTx()
		baseTx.AddTxIn(wire.NewTxIn(new(wire.OutPoint), int64(funding), nil))
		baseTx.AddTxOut(wire.NewTxOut(swapVal, pkScript))
		return baseTx
	}

	node.signFunc = func(tx *wire.MsgTx) (*wire.MsgTx, bool, error) {
		return signFunc(tx, P2PKHSigScriptSize)
	}

	tests := []struct {
		name      string
		funding   uint64
		expChange bool
	}{
		{
			name:    "not enough for change output",
			funding: swapVal + dexReqFees - 1,
		},
		{
			// Still dust here, but a different path.
			name:    "exactly enough for change output",
			funding: swapVal + dexReqFees,
		},
		{
			name:    "more than enough for change output but still dust",
			funding: swapVal + dexReqFees + 1,
		},
		{
			name:    "1 atom short to not be dust",
			funding: swapVal + dexReqFees + dustCoverage - 1,
		},
		{
			name:      "exactly enough to not be dust",
			funding:   swapVal + dexReqFees + dustCoverage,
			expChange: true,
		},
	}

	// tPKHAddrV3, _ := stdaddr.DecodeAddress(tPKHAddr.String(), tChainParams)
	node.changeAddr = tPKHAddr

	for _, tt := range tests {
		tx, err := wallet.sendWithReturn(newBaseTx(tt.funding), feeRate, -1)
		if err != nil {
			t.Fatalf("sendWithReturn error: %v", err)
		}

		if len(tx.TxOut) == 1 && tt.expChange {
			t.Fatalf("%s: no change added", tt.name)
		} else if len(tx.TxOut) == 2 && !tt.expChange {
			t.Fatalf("%s: change output added for dust. Output value = %d", tt.name, tx.TxOut[1].Value)
		}
	}
}

func TestSyncStatus(t *testing.T) {
	wallet, node, shutdown := tNewWallet()
	defer shutdown()

	node.rawRes[methodSyncStatus], node.rawErr[methodSyncStatus] = json.Marshal(&walletjson.SyncStatusResult{
		Synced:               true,
		InitialBlockDownload: false,
		HeadersFetchProgress: 1,
	})
	ss, err := wallet.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus error (synced expected): %v", err)
	}
	if !ss.Synced {
		t.Fatalf("synced = false for progress=1")
	}
	if ss.BlockProgress() < 1 {
		t.Fatalf("progress not complete with sync true")
	}

	node.rawErr[methodSyncStatus] = tErr
	_, err = wallet.SyncStatus()
	if err == nil {
		t.Fatalf("SyncStatus error not propagated")
	}
	node.rawErr[methodSyncStatus] = nil

	nodeSyncStatusResult := &walletjson.SyncStatusResult{
		Synced:               false,
		InitialBlockDownload: false,
		HeadersFetchProgress: 0.5, // Headers: 200, WalletTip: 100
	}
	node.rawRes[methodSyncStatus], node.rawErr[methodSyncStatus] = json.Marshal(nodeSyncStatusResult)
	node.rawRes[methodGetPeerInfo], node.rawErr[methodGetPeerInfo] = json.Marshal([]*walletjson.GetPeerInfoResult{{StartingHeight: 1000}})

	ss, err = wallet.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus error (half-synced): %v", err)
	}
	if ss.Synced {
		t.Fatalf("synced = true for progress=0.5")
	}
	if ss.BlockProgress() != nodeSyncStatusResult.HeadersFetchProgress {
		t.Fatalf("progress out of range. Expected %.2f, got %.2f", nodeSyncStatusResult.HeadersFetchProgress, ss.BlockProgress())
	}
	if ss.Blocks != 500 {
		t.Fatalf("wrong header sync height. expected 500, got %d", ss.Blocks)
	}
}

func Test_dcrPerKBToAtomsPerByte(t *testing.T) {
	tests := []struct {
		name             string
		estimatedFeeRate float64
		want             uint64
		wantErr          bool
	}{
		{
			"catch negative", // but caller should check
			-0.0002,
			0,
			true,
		},
		{
			"ok 0", // but caller should check
			0.0,
			0,
			false,
		},
		{
			"ok 10",
			0.0001,
			10,
			false,
		},
		{
			"ok 11",
			0.00011,
			11,
			false,
		},
		{
			"ok 1",
			0.00001,
			1,
			false,
		},
		{
			"ok 1 rounded up",
			0.000002,
			1,
			false,
		},
		{
			"catch NaN err",
			math.NaN(),
			0,
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dcrPerKBToAtomsPerByte(tt.estimatedFeeRate)
			if (err != nil) != tt.wantErr {
				t.Errorf("dcrPerKBToAtomsPerByte() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("dcrPerKBToAtomsPerByte() = %v, want %v", got, tt.want)
			}
		})
	}
}

type tReconfigurer struct {
	*rpcWallet
	restart bool
	err     error
}

func (r *tReconfigurer) Reconfigure(ctx context.Context, cfg *asset.WalletConfig, net util.Network, currentAddress string) (restartRequired bool, err error) {
	return r.restart, r.err
}

func TestReconfigure(t *testing.T) {
	wallet, _, shutdown := tNewWalletMonitorBlocks(false)
	defer shutdown()

	reconfigurer := tReconfigurer{
		rpcWallet: wallet.wallet.(*rpcWallet),
	}

	wallet.wallet = &reconfigurer

	ctx := t.Context()

	cfg1 := &walletConfig{
		UseSplitTx:       true,
		FallbackFeeRate:  55,
		FeeRateLimit:     98,
		RedeemConfTarget: 7,
		ApiFeeFallback:   true,
	}

	cfg2 := &walletConfig{
		UseSplitTx:       false,
		FallbackFeeRate:  66,
		FeeRateLimit:     97,
		RedeemConfTarget: 5,
		ApiFeeFallback:   false,
	}

	// TODO: Test account names reconfiguration for rpcwallets.
	checkConfig := func(cfg *walletConfig) {
		if cfg.UseSplitTx != wallet.config().useSplitTx ||
			toAtoms(cfg.FallbackFeeRate/1000) != wallet.config().fallbackFeeRate ||
			toAtoms(cfg.FeeRateLimit/1000) != wallet.config().feeRateLimit ||
			cfg.RedeemConfTarget != wallet.config().redeemConfTarget ||
			cfg.ApiFeeFallback != wallet.config().apiFeeFallback {
			t.Fatalf("wallet not configured with the correct values")
		}
	}

	settings1, err := config.Mapify(cfg1)
	if err != nil {
		t.Fatalf("failed to mapify: %v", err)
	}

	settings2, err := config.Mapify(cfg2)
	if err != nil {
		t.Fatalf("failed to mapify: %v", err)
	}

	walletCfg := &asset.WalletConfig{
		Type:     walletTypeDcrwRPC,
		Settings: settings1,
		DataDir:  "abcd",
	}

	// restart = false
	restart, err := wallet.Reconfigure(ctx, walletCfg, "123456")
	if err != nil {
		t.Fatalf("did not expect an error")
	}
	if restart {
		t.Fatalf("expected false restart but got true")
	}
	checkConfig(cfg1)

	// restart = 2
	reconfigurer.restart = true
	restart, err = wallet.Reconfigure(ctx, walletCfg, "123456")
	if err != nil {
		t.Fatalf("did not expect an error")
	}
	if !restart {
		t.Fatalf("expected true restart but got false")
	}
	checkConfig(cfg1)

	// try to set new configs, but get error. config should not change.
	reconfigurer.err = errors.New("reconfigure error")
	walletCfg.Settings = settings2
	_, err = wallet.Reconfigure(ctx, walletCfg, "123456")
	if err == nil {
		t.Fatalf("expected an error")
	}
	checkConfig(cfg1)
}

func TestEstimateSendTxFee(t *testing.T) {
	wallet, node, shutdown := tNewWallet()
	defer shutdown()

	addr := tPKHAddr.String()
	node.changeAddr = tPKHAddr
	var unspentVal uint64 = 100e8
	unspents := make([]walletjson.ListUnspentResult, 0)
	balanceResult := &walletjson.GetBalanceResult{
		Balances: []walletjson.GetAccountBalanceResult{
			{
				AccountName: tAcctName,
			},
		},
	}
	node.balanceResult = balanceResult

	var vout uint32
	addUtxo := func(atomAmt uint64, confs int64, updateUnspent bool) {
		if updateUnspent {
			node.unspent[0].Amount += float64(atomAmt) / 1e8
			return
		}
		utxo := walletjson.ListUnspentResult{
			TxID:          tTxID,
			Vout:          vout,
			Address:       tPKHAddr.String(),
			Account:       tAcctName,
			Amount:        float64(atomAmt) / 1e8,
			Confirmations: confs,
			ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
			Spendable:     true,
		}
		unspents = append(unspents, utxo)
		node.unspent = unspents
		// update balance
		balanceResult.Balances[0].Spendable += utxo.Amount
		vout++
	}

	tx := wire.NewMsgTx()
	payScriptVer, payScript := tPKHAddr.PaymentScript()
	tx.AddTxOut(newTxOut(int64(unspentVal), payScriptVer, payScript))

	// bSize is the base size for a single tx input.
	bSize := TxInOverhead + uint32(wire.VarIntSerializeSize(uint64(P2PKHSigScriptSize))) + P2PKHSigScriptSize

	txSize := uint32(tx.SerializeSize()) + bSize
	estFee := uint64(txSize) * optimalFeeRate
	changeFee := P2PKHOutputSize * optimalFeeRate
	estFeeWithChange := changeFee + estFee

	// This should return fee rate estimate for one output.
	addUtxo(unspentVal, 1, false)
	estimate, _, err := wallet.EstimateSendTxFee(addr, unspentVal, optimalFeeRate, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if estimate != estFee {
		t.Fatalf("expected estimate to be %v, got %v)", estFee, estimate)
	}

	// This should return fee rate estimate for two output.
	estimate, _, err = wallet.EstimateSendTxFee(addr, unspentVal/2, optimalFeeRate, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if estimate != estFeeWithChange {
		t.Fatalf("expected estimate to be %v, got %v)", estFeeWithChange, estimate)
	}

	// This should return an error, not enough funds to cover fees.
	_, _, err = wallet.EstimateSendTxFee(addr, unspentVal, optimalFeeRate, false, false)
	if err == nil {
		t.Fatal("Expected error not enough to cover funds required")
	}

	dust := uint64(100)
	addUtxo(dust, 0, true)
	// This should return fee rate estimate for one output with dust added to fee.
	estFeeWithDust := estFee + 100
	estimate, _, err = wallet.EstimateSendTxFee(addr, unspentVal, optimalFeeRate, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if estimate != estFeeWithDust {
		t.Fatalf("expected estimate to be %v, got %v)", estFeeWithDust, estimate)
	}

	// Invalid address
	_, valid, _ := wallet.EstimateSendTxFee("invalidsendaddress", unspentVal, optimalFeeRate, true, false)
	if valid {
		t.Fatal("Expected false for an invalid address")
	}

	// Successful estimate for empty address
	_, _, err = wallet.EstimateSendTxFee("", unspentVal, optimalFeeRate, true, false)
	if err != nil {
		t.Fatalf("Error for empty address: %v", err)
	}

	// Zero send amount
	_, _, err = wallet.EstimateSendTxFee(addr, 0, optimalFeeRate, true, false)
	if err == nil {
		t.Fatal("Expected error, send amount is zero")
	}
}

func TestPurchaseTickets(t *testing.T) {
	const feeSuggestion = 100
	const sdiff = 1

	wallet, cl, shutdown := tNewWalletMonitorBlocks(false)
	defer shutdown()
	wallet.connected.Store(true)
	cl.stakeInfo.Difficulty = dcrutil.Amount(sdiff).ToCoin()
	cl.balanceResult = &walletjson.GetBalanceResult{Balances: []walletjson.GetAccountBalanceResult{{AccountName: tAcctName}}}
	setBalance := func(n, reserves uint64) {
		ticketCost := n * (sdiff + feeSuggestion*minVSPTicketPurchaseSize)
		cl.balanceResult.Balances[0].Spendable = dcrutil.Amount(ticketCost).ToCoin()
		wallet.bondReserves.Store(reserves)
	}

	var blocksToConfirm atomic.Int64
	cl.walletTxFn = func() (*walletjson.GetTransactionResult, error) {
		txHex, _ := makeTxHex(nil, []util.Bytes{randBytes(25)})
		var confs int64 = 1
		if blocksToConfirm.Load() > 0 {
			confs = 0
		}
		return &walletjson.GetTransactionResult{Hex: txHex, Confirmations: confs}, nil
	}

	var remains []uint32
	checkRemains := func(exp ...uint32) {
		t.Helper()
		if len(remains) != len(exp) {
			t.Fatalf("wrong number of remains, wanted %d, got %+v", len(exp), remains)
		}
		for i := 0; i < len(remains); i++ {
			if remains[i] != exp[i] {
				t.Fatalf("wrong remains updates: wanted %+v, got %+v", exp, remains)
			}
		}
	}

	waitForTicketLoopToExit := func() {
		// Ensure the loop closes
		timeout := time.After(time.Second)
		for {
			if !wallet.ticketBuyer.running.Load() {
				break
			}
			select {
			case <-time.After(time.Millisecond):
				return
			case <-timeout:
				t.Fatalf("ticket loop didn't exit")
			}
		}
	}

	buyTickets := func(n int, wantErr bool) {
		defer waitForTicketLoopToExit()
		remains = make([]uint32, 0)
		if err := wallet.PurchaseTickets(n, feeSuggestion); err != nil {
			t.Fatalf("initial PurchaseTickets error: %v", err)
		}

		var emitted int
		timeout := time.After(time.Second)
	out:
		for {
			var ni asset.WalletNotification
			select {
			case ni = <-cl.emitC:
			case <-timeout:
				t.Fatalf("timed out looking for ticket updates")
			default:
				blocksToConfirm.Add(-1)
				wallet.runTicketBuyer()
				continue
			}
			switch nt := ni.(type) {
			case *asset.CustomWalletNote:
				switch n := nt.Payload.(type) {
				case *TicketPurchaseUpdate:
					remains = append(remains, n.Remaining)
					if n.Err != "" {
						if wantErr {
							return
						}
						t.Fatalf("Error received in TicketPurchaseUpdate: %s", n.Err)
					}
					if n.Remaining == 0 {
						break out
					}
					emitted++
				}

			}
		}
	}

	tixHashes := func(n int) []*chainhash.Hash {
		hs := make([]*chainhash.Hash, n)
		for i := 0; i < n; i++ {
			var ticketHash chainhash.Hash
			copy(ticketHash[:], randBytes(32))
			hs[i] = &ticketHash
		}
		return hs
	}

	// Single ticket purchased right away.
	cl.purchasedTickets = [][]*chainhash.Hash{tixHashes(1)}
	setBalance(1, 0)
	buyTickets(1, false)
	checkRemains(1, 0)

	// Multiple tickets purchased right away.
	cl.purchasedTickets = [][]*chainhash.Hash{tixHashes(2)}
	setBalance(2, 0)
	buyTickets(2, false)
	checkRemains(2, 0)

	// Two tickets, purchased in two tries, skipping some tries for unconfirmed
	// tickets.
	blocksToConfirm.Store(3)
	cl.purchasedTickets = [][]*chainhash.Hash{tixHashes(1), tixHashes(1)}
	buyTickets(2, false)
	checkRemains(2, 1, 0)

	// (Wallet).PurchaseTickets error
	cl.purchasedTickets = [][]*chainhash.Hash{tixHashes(4)}
	cl.purchaseTicketsErr = errors.New("test error")
	setBalance(4, 0)
	buyTickets(4, true)
	checkRemains(4, 0)

	// Low-balance error
	cl.purchasedTickets = [][]*chainhash.Hash{tixHashes(1)}
	setBalance(1, 1) // reserves make our available balance 0
	buyTickets(1, true)
	checkRemains(1, 0)
}

func TestFindBond(t *testing.T) {
	wallet, node, shutdown := tNewWallet()
	defer shutdown()

	privBytes, _ := hex.DecodeString("b07209eec1a8fb6cfe5cb6ace36567406971a75c330db7101fb21bc679bc5330")
	bondKey := secp256k1.PrivKeyFromBytes(privBytes)

	amt := uint64(50_000)
	acctID := [32]byte{}
	lockTime := time.Now().Add(time.Hour * 12)
	utxo := walletjson.ListUnspentResult{
		TxID:          tTxID,
		Address:       tPKHAddr.String(),
		Account:       tAcctName,
		Amount:        1.0,
		Confirmations: 1,
		ScriptPubKey:  hex.EncodeToString(tP2PKHScript),
		Spendable:     true,
	}
	node.unspent = []walletjson.ListUnspentResult{utxo}
	node.newAddr = tPKHAddr
	node.changeAddr = tPKHAddr

	bond, _, err := wallet.MakeBondTx(0, amt, 200, lockTime, bondKey, acctID[:])
	if err != nil {
		t.Fatal(err)
	}

	txFn := func(err error, tx []byte) func() (*walletjson.GetTransactionResult, error) {
		return func() (*walletjson.GetTransactionResult, error) {
			if err != nil {
				return nil, err
			}
			h := hex.EncodeToString(tx)
			return &walletjson.GetTransactionResult{
				BlockHash: hex.EncodeToString(randBytes(32)),
				Hex:       h,
			}, nil
		}
	}

	newBondTx := func() *wire.MsgTx {
		msgTx := wire.NewMsgTx()
		if err := msgTx.FromBytes(bond.SignedTx); err != nil {
			t.Fatal(err)
		}
		return msgTx
	}
	tooFewOutputs := newBondTx()
	tooFewOutputs.TxOut = tooFewOutputs.TxOut[2:]
	tooFewOutputsBytes, err := tooFewOutputs.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	badBondScript := newBondTx()
	badBondScript.TxOut[1].PkScript = badBondScript.TxOut[1].PkScript[1:]
	badBondScriptBytes, err := badBondScript.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	noBondMatch := newBondTx()
	noBondMatch.TxOut[0].PkScript = noBondMatch.TxOut[0].PkScript[1:]
	noBondMatchBytes, err := noBondMatch.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	node.blockchain.addRawTx(1, newBondTx())
	verboseBlocks := node.blockchain.verboseBlocks

	tests := []struct {
		name          string
		coinID        []byte
		txRes         func() (*walletjson.GetTransactionResult, error)
		bestBlockErr  error
		verboseBlocks map[chainhash.Hash]*wire.MsgBlock
		searchUntil   time.Time
		wantErr       bool
	}{{
		name:   "ok",
		coinID: bond.CoinID,
		txRes:  txFn(nil, bond.SignedTx),
	}, {
		name:   "ok with find blocks",
		coinID: bond.CoinID,
		txRes:  txFn(asset.CoinNotFoundError, nil),
	}, {
		name:    "bad coin id",
		coinID:  make([]byte, 0),
		txRes:   txFn(nil, bond.SignedTx),
		wantErr: true,
	}, {
		name:    "missing an output",
		coinID:  bond.CoinID,
		txRes:   txFn(nil, tooFewOutputsBytes),
		wantErr: true,
	}, {
		name:    "bad bond commitment script",
		coinID:  bond.CoinID,
		txRes:   txFn(nil, badBondScriptBytes),
		wantErr: true,
	}, {
		name:    "bond script does not match commitment",
		coinID:  bond.CoinID,
		txRes:   txFn(nil, noBondMatchBytes),
		wantErr: true,
	}, {
		name:    "bad msgtx",
		coinID:  bond.CoinID,
		txRes:   txFn(nil, bond.SignedTx[100:]),
		wantErr: true,
	}, {
		name:         "get best block error",
		coinID:       bond.CoinID,
		txRes:        txFn(asset.CoinNotFoundError, nil),
		bestBlockErr: errors.New("some error"),
		wantErr:      true,
	}, {
		name:          "block not found",
		coinID:        bond.CoinID,
		txRes:         txFn(asset.CoinNotFoundError, nil),
		verboseBlocks: map[chainhash.Hash]*wire.MsgBlock{},
		wantErr:       true,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node.walletTxFn = test.txRes
			node.bestBlockErr = test.bestBlockErr
			node.blockchain.verboseBlocks = verboseBlocks
			if test.verboseBlocks != nil {
				node.blockchain.verboseBlocks = test.verboseBlocks
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

func makeSwapContract(lockTimeOffset time.Duration) (pkScriptVer uint16, pkScript []byte) {
	secret := randBytes(32)
	secretHash := sha256.Sum256(secret)

	lockTime := time.Now().Add(lockTimeOffset)
	var err error
	contract, err := MakeContract(tPKHAddr.String(), tPKHAddr.String(), secretHash[:], lockTime.Unix(), chaincfg.MainNetParams())
	if err != nil {
		panic("error making swap contract:" + err.Error())
	}

	scriptAddr, err := stdaddr.NewAddressScriptHashV0(contract, chaincfg.MainNetParams())
	if err != nil {
		panic("error making script address:" + err.Error())
	}

	return scriptAddr.PaymentScript()
}

func TestIDUnknownTx(t *testing.T) {
	// Swap Tx - any tx with p2sh outputs that is not a bond.
	_, swapPKScript := makeSwapContract(time.Hour * 12)
	swapTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, 0, nil)},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toAtoms(1)), swapPKScript)},
	}

	// Redeem Tx
	swapContract, _ := MakeContract(tPKHAddr.String(), tPKHAddr.String(), randBytes(32), time.Now().Unix(), chaincfg.MainNetParams())
	txIn := wire.NewTxIn(&wire.OutPoint{}, 0, nil)
	txIn.SignatureScript, _ = RedeemP2SHContract(swapContract, randBytes(73), randBytes(33), randBytes(32))
	redeemFee := 0.0000143
	_, tP2PKH := tPKHAddr.PaymentScript()
	redemptionTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{txIn},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toAtoms(5-redeemFee)), tP2PKH)},
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
	contractAddr, err := stdaddr.NewAddressScriptHashV0(bondContract, chaincfg.MainNetParams())
	if err != nil {
		t.Fatal("error making script address:" + err.Error())
	}
	_, bondPkScript := contractAddr.PaymentScript()

	bondOutput := wire.NewTxOut(int64(toAtoms(2)), bondPkScript)
	bondCommitPkScript, _ := bondPushDataScript(0, accountID, int64(bondLockTime), bondID)
	bondCommitmentOutput := wire.NewTxOut(0, bondCommitPkScript)
	createBondTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, 0, nil)},
		TxOut: []*wire.TxOut{bondOutput, bondCommitmentOutput},
	}

	// Redeem Bond Tx
	txIn = wire.NewTxIn(&wire.OutPoint{}, 0, nil)
	txIn.SignatureScript, _ = RefundBondScript(bondContract, randBytes(73), randBytes(33))
	redeemBondTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{txIn},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toAtoms(5)), tP2PKH)},
	}

	// Split Tx
	splitTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, 0, nil)},
		TxOut: []*wire.TxOut{wire.NewTxOut(0, tP2PKH), wire.NewTxOut(0, tP2PKH)},
	}

	// Send Tx
	cpAddr, _ := stdaddr.DecodeAddress("Dsedb5o6Tw225Loq5J56BZ9jS4ehnEnmQ16", tChainParams)
	_, cpPkScript := cpAddr.PaymentScript()
	sendTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, 0, nil)},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toAtoms(0.001)), tP2PKH), wire.NewTxOut(int64(toAtoms(4)), cpPkScript)},
	}

	// Receive Tx
	receiveTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, 0, nil)},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toAtoms(0.001)), cpPkScript), wire.NewTxOut(int64(toAtoms(4)), tP2PKH)},
	}
	type test struct {
		name            string
		ltr             *ListTransactionsResult
		tx              *wire.MsgTx
		validateAddress map[string]*walletjson.ValidateAddressResult
		exp             *asset.WalletTransaction
	}

	// Ticket Tx
	ticketTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{wire.NewTxIn(&wire.OutPoint{}, 0, nil)},
		TxOut: []*wire.TxOut{wire.NewTxOut(int64(toAtoms(1)), cpPkScript)},
	}

	float64Ptr := func(f float64) *float64 {
		return &f
	}

	stringPtr := func(s string) *string {
		return &s
	}

	regularTx := walletjson.LTTTRegular
	ticketPurchaseTx := walletjson.LTTTTicket
	ticketRevocationTx := walletjson.LTTTRevocation
	ticketVote := walletjson.LTTTVote

	tests := []*test{
		{
			name: "swap",
			ltr: &ListTransactionsResult{
				TxType: &regularTx,
				Fee:    float64Ptr(0.0000321),
				TxID:   swapTx.TxHash().String(),
			},
			tx: swapTx,
			exp: &asset.WalletTransaction{
				Type:   asset.SwapOrSend,
				ID:     swapTx.TxHash().String(),
				Amount: toAtoms(1),
				Fees:   toAtoms(0.0000321),
			},
		},
		{
			name: "redeem",
			ltr: &ListTransactionsResult{
				TxType: &regularTx,
				TxID:   redemptionTx.TxHash().String(),
			},
			tx: redemptionTx,
			exp: &asset.WalletTransaction{
				Type:   asset.Redeem,
				ID:     redemptionTx.TxHash().String(),
				Amount: toAtoms(5 - redeemFee),
				Fees:   0,
			},
		},
		{
			name: "create bond",
			ltr: &ListTransactionsResult{
				TxType: &regularTx,
				Fee:    float64Ptr(0.0000222),
				TxID:   createBondTx.TxHash().String(),
			},
			tx: createBondTx,
			exp: &asset.WalletTransaction{
				Type:   asset.CreateBond,
				ID:     createBondTx.TxHash().String(),
				Amount: toAtoms(2),
				Fees:   toAtoms(0.0000222),
				BondInfo: &asset.BondTxInfo{
					AccountID: accountID,
					BondID:    bondID,
					LockTime:  uint64(bondLockTime),
				},
			},
		},
		{
			name: "redeem bond",
			ltr: &ListTransactionsResult{
				TxType: &regularTx,
				TxID:   redeemBondTx.TxHash().String(),
			},
			tx: redeemBondTx,
			exp: &asset.WalletTransaction{
				Type:   asset.RedeemBond,
				ID:     redeemBondTx.TxHash().String(),
				Amount: toAtoms(5),
				BondInfo: &asset.BondTxInfo{
					AccountID: []byte{},
					BondID:    bondID,
					LockTime:  uint64(bondLockTime),
				},
			},
		},
		{
			name: "split",
			ltr: &ListTransactionsResult{
				TxType: &regularTx,
				Fee:    float64Ptr(-0.0000251),
				Send:   true,
				TxID:   splitTx.TxHash().String(),
			},
			tx: splitTx,
			exp: &asset.WalletTransaction{
				Type: asset.Split,
				ID:   splitTx.TxHash().String(),
				Fees: toAtoms(0.0000251),
			},
		},
		{
			name: "send",
			ltr: &ListTransactionsResult{
				TxType: &regularTx,
				Send:   true,
				Fee:    float64Ptr(0.0000504),
				TxID:   sendTx.TxHash().String(),
			},
			tx: sendTx,
			exp: &asset.WalletTransaction{
				Type:      asset.Send,
				ID:        sendTx.TxHash().String(),
				Amount:    toAtoms(4),
				Recipient: stringPtr(cpAddr.String()),
				Fees:      toAtoms(0.0000504),
			},
			validateAddress: map[string]*walletjson.ValidateAddressResult{
				tPKHAddr.String(): {
					IsMine:  true,
					Account: tAcctName,
				},
			},
		},
		{
			name: "receive",
			ltr: &ListTransactionsResult{
				TxType: &regularTx,
				TxID:   receiveTx.TxHash().String(),
			},
			tx: receiveTx,
			exp: &asset.WalletTransaction{
				Type:      asset.Receive,
				ID:        receiveTx.TxHash().String(),
				Amount:    toAtoms(4),
				Recipient: stringPtr(tPKHAddr.String()),
			},
			validateAddress: map[string]*walletjson.ValidateAddressResult{
				tPKHAddr.String(): {
					IsMine:  true,
					Account: tAcctName,
				},
			},
		},
		{
			name: "ticket purchase",
			ltr: &ListTransactionsResult{
				TxType: &ticketPurchaseTx,
				TxID:   ticketTx.TxHash().String(),
			},
			tx: ticketTx,
			exp: &asset.WalletTransaction{
				Type:   asset.TicketPurchase,
				ID:     ticketTx.TxHash().String(),
				Amount: toAtoms(1),
			},
		},
		{
			name: "ticket vote",
			ltr: &ListTransactionsResult{
				TxType: &ticketVote,
				TxID:   ticketTx.TxHash().String(),
			},
			tx: ticketTx,
			exp: &asset.WalletTransaction{
				Type:   asset.TicketVote,
				ID:     ticketTx.TxHash().String(),
				Amount: toAtoms(1),
			},
		},
		{
			name: "ticket revocation",
			ltr: &ListTransactionsResult{
				TxType: &ticketRevocationTx,
				TxID:   ticketTx.TxHash().String(),
			},
			tx: ticketTx,
			exp: &asset.WalletTransaction{
				Type:   asset.TicketRevocation,
				ID:     ticketTx.TxHash().String(),
				Amount: toAtoms(1),
			},
		},
	}

	runTest := func(tt *test) {
		t.Run(tt.name, func(t *testing.T) {
			wallet, node, shutdown := tNewWallet()
			defer shutdown()
			node.validateAddress = tt.validateAddress
			node.blockchain.rawTxs[tt.tx.TxHash()] = &wireTxWithHeight{
				tx: tt.tx,
			}
			wt, err := wallet.idUnknownTx(context.Background(), tt.ltr)
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

func TestRescanSync(t *testing.T) {
	wallet, node, shutdown := tNewWalletMonitorBlocks(false)
	defer shutdown()

	const tip = 1000
	wallet.currentTip.Store(&block{height: tip})

	node.rawRes[methodSyncStatus], node.rawErr[methodSyncStatus] = json.Marshal(&walletjson.SyncStatusResult{
		Synced:               true,
		InitialBlockDownload: false,
		HeadersFetchProgress: 1,
	})

	node.blockchain.mainchain[tip] = &chainhash.Hash{}

	checkProgress := func(expSynced bool, expProgress float32) {
		t.Helper()
		ss, err := wallet.SyncStatus()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if ss.Synced != expSynced {
			t.Fatalf("expected synced = %t, bot %t", expSynced, ss.Synced)
		}
		if !ss.Synced {
			txProgress := float32(*ss.Transactions) / float32(ss.TargetHeight)
			if math.Abs(float64(expProgress/txProgress)-1) > 0.001 {
				t.Fatalf("expected progress %f, got %f", expProgress, txProgress)
			}
		}
	}

	// No rescan in progress.
	checkProgress(true, 1)

	// Rescan running. No progress.
	wallet.rescan.progress = &rescanProgress{}
	checkProgress(false, 0)

	// Halfway done.
	wallet.rescan.progress = &rescanProgress{scannedThrough: tip / 2}
	checkProgress(false, 0.5)

	// Not synced until progress is nil.
	wallet.rescan.progress = &rescanProgress{scannedThrough: tip}
	checkProgress(false, 1)

	// Scanned > tip OK
	wallet.rescan.progress = &rescanProgress{scannedThrough: tip * 2}
	checkProgress(false, 1)

	// Rescan complete.
	wallet.rescan.progress = nil
	checkProgress(true, 1)

}
