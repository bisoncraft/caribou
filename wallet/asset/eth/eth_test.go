//go:build !harness && !rpclive

// These tests will not be run if the harness build tag is set.

package eth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/config"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

const (
	txConfsNeededToConfirm = 3
)

// testNode is the ethFetcher used throughout the offline tests. Shared test
// globals and bootstrap live in testmain_test.go.
var _ ethFetcher = (*testNode)(nil)

type tGetTxRes struct {
	tx     *types.Transaction
	height int64
}

type testNode struct {
	acct              *accounts.Account
	addr              common.Address
	connectErr        error
	bestHdr           *types.Header
	bestHdrErr        error
	syncProg          ethereum.SyncProgress
	syncProgT         uint64
	syncProgErr       error
	bal               *big.Int
	balErr            error
	signDataErr       error
	privKey           *ecdsa.PrivateKey
	baseFee           *big.Int
	tip               *big.Int
	netFeeStateErr    error
	confNonce         uint64
	confNonceErr      error
	getTxRes          *types.Transaction
	getTxResMap       map[common.Hash]*tGetTxRes
	getTxHeight       int64
	getTxErr          error
	receipt           *types.Receipt
	receiptTx         *types.Transaction
	receiptErr        error
	receipts          map[common.Hash]*types.Receipt
	receiptTxs        map[common.Hash]*types.Transaction
	receiptErrs       map[common.Hash]error
	hdrByHash         map[common.Hash]*types.Header
	lastSignedTx      *types.Transaction
	sentTxs           int
	sendTxTx          *types.Transaction
	sendTxErr         error
	lastSendTo        *common.Address
	lastSendData      []byte
	estimateGasResult uint64
	estimateGasErr    error
	simBackend        bind.ContractBackend
	maxFeeRate        *big.Int
	tokenContractor   *tTokenContractor
	tokenParent       *assetWallet // only set for tokens
	txConfirmations   map[common.Hash]uint32
	txConfsErr        map[common.Hash]error
	txAndReceiptBlock <-chan struct{}
	txAndReceiptHit   chan<- struct{}

	// nilReceiptNoErr makes transactionAndReceipt report neither a receipt nor
	// an error, which a real node never does. Only a test exercising the
	// caller's guard against that pair should set it; a miss in the receipt
	// maps above reports not-found, the way a node does.
	nilReceiptNoErr bool
}

func newBalance(current, in, out uint64) *Balance {
	return &Balance{
		Current:    GweiToWei(current),
		PendingIn:  GweiToWei(in),
		PendingOut: GweiToWei(out),
	}
}

func (n *testNode) newTransaction(nonce uint64, value *big.Int) *types.Transaction {
	to := common.BytesToAddress(encode.RandomBytes(20))
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		Nonce:   nonce,
		Value:   value,
		Gas:     50_000,
		To:      &to,
		ChainID: n.chainConfig().ChainID,
	}), signer, n.privKey)
	if err != nil {
		panic("tx signing error")
	}
	return tx
}

func (n *testNode) address() common.Address {
	return n.addr
}

func (n *testNode) connect(ctx context.Context) error {
	return n.connectErr
}

func (n *testNode) contractBackend() bind.ContractBackend {
	return n.simBackend
}

func (n *testNode) chainConfig() *params.ChainConfig {
	return params.AllEthashProtocolChanges
}

func (n *testNode) getConfirmedNonce(context.Context) (uint64, error) {
	return n.confNonce, n.confNonceErr
}

func (n *testNode) getTransaction(ctx context.Context, hash common.Hash) (*types.Transaction, int64, error) {
	if n.getTxErr != nil {
		return nil, 0, n.getTxErr
	}

	if n.getTxResMap != nil {
		if tx, ok := n.getTxResMap[hash]; ok {
			return tx.tx, tx.height, nil
		} else {
			return nil, 0, asset.CoinNotFoundError
		}
	}

	return n.getTxRes, n.getTxHeight, n.getTxErr
}

func (n *testNode) txOpts(ctx context.Context, val, maxGas uint64, maxFeeRate, tipRate, nonce *big.Int) (*bind.TransactOpts, error) {
	if maxFeeRate == nil {
		maxFeeRate = n.maxFeeRate
	}
	txOpts := newTxOpts(ctx, n.addr, val, maxGas, maxFeeRate, GweiToWei(2))
	txOpts.Nonce = big.NewInt(1)
	return txOpts, nil
}

func (n *testNode) currentFees(ctx context.Context) (baseFees, tipCap *big.Int, err error) {
	return n.baseFee, n.tip, n.netFeeStateErr
}

func (n *testNode) shutdown() {}

func (n *testNode) bestHeader(ctx context.Context) (*types.Header, error) {
	return n.bestHdr, n.bestHdrErr
}
func (n *testNode) addressBalance(ctx context.Context, addr common.Address) (*big.Int, error) {
	return n.bal, n.balErr
}
func (n *testNode) unlock(pw string) error {
	return nil
}
func (n *testNode) lock() error {
	return nil
}
func (n *testNode) locked() bool {
	return false
}
func (n *testNode) syncProgress(context.Context) (prog *ethereum.SyncProgress, bestBlockUNIXTime uint64, err error) {
	return &n.syncProg, n.syncProgT, n.syncProgErr
}
func (n *testNode) peerCount() uint32 {
	return 1
}

func (n *testNode) signData(data []byte) (sig, pubKey []byte, err error) {
	return n.signHash(crypto.Keccak256(data))
}

func (n *testNode) signHash(hash []byte) (sig, pubKey []byte, err error) {
	if n.signDataErr != nil {
		return nil, nil, n.signDataErr
	}

	if n.privKey == nil {
		return nil, nil, nil
	}

	sig, err = crypto.Sign(hash, n.privKey)
	if err != nil {
		return nil, nil, err
	}

	return sig, crypto.FromECDSAPub(&n.privKey.PublicKey), nil
}

func (n *testNode) sendTransaction(ctx context.Context, txOpts *bind.TransactOpts, to *common.Address, data []byte, filts ...acceptabilityFilter) (*types.Transaction, error) {
	n.sentTxs++
	if to != nil {
		addr := *to
		n.lastSendTo = &addr
	} else {
		n.lastSendTo = nil
	}
	n.lastSendData = append(n.lastSendData[:0], data...)
	return n.sendTxTx, n.sendTxErr
}

func (n *testNode) EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error) {
	if n.estimateGasResult != 0 || n.estimateGasErr != nil {
		return n.estimateGasResult, n.estimateGasErr
	}
	return 100_000, nil
}

func (n *testNode) sendSignedTransaction(ctx context.Context, tx *types.Transaction, filts ...acceptabilityFilter) error {
	n.lastSignedTx = tx
	return nil
}

func tTx(gasFeeCap, gasTipCap, value uint64, to *common.Address, data []byte, gasLimit uint64) *types.Transaction {
	return types.NewTx(&types.DynamicFeeTx{
		GasFeeCap: GweiToWei(gasFeeCap),
		GasTipCap: GweiToWei(gasTipCap),
		To:        to,
		Value:     GweiToWei(value),
		Data:      data,
		Gas:       gasLimit,
	})
}

func (n *testNode) transactionConfirmations(_ context.Context, txHash common.Hash) (uint32, error) {
	if n.txConfirmations == nil {
		return 0, nil
	}
	return n.txConfirmations[txHash], n.txConfsErr[txHash]
}

func (n *testNode) headerByHash(_ context.Context, blockHash common.Hash) (*types.Header, error) {
	return n.hdrByHash[blockHash], nil
}

func (n *testNode) transactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	if n.receiptErr != nil {
		return nil, n.receiptErr
	}
	if n.receipt != nil {
		return n.receipt, nil
	}

	return n.receipts[txHash], n.receiptErrs[txHash]
}

func (n *testNode) transactionAndReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, *types.Transaction, error) {
	if n.txAndReceiptHit != nil {
		select {
		case n.txAndReceiptHit <- struct{}{}:
		default:
		}
	}
	if n.txAndReceiptBlock != nil {
		select {
		case <-n.txAndReceiptBlock:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	if n.receiptErr != nil {
		return nil, nil, n.receiptErr
	}
	if n.receipt != nil {
		return n.receipt, n.receiptTx, nil
	}

	receipt, tx, err := n.receipts[txHash], n.receiptTxs[txHash], n.receiptErrs[txHash]
	if err == nil && receipt == nil && !n.nilReceiptNoErr {
		// A hash no test set up a receipt for is a hash the node has never
		// heard of, and a node says so with an error. Returning the bare nils
		// instead hands the caller a nil receipt under a nil error, which it
		// reads as a fetched receipt and dereferences.
		return nil, tx, asset.CoinNotFoundError
	}
	return receipt, tx, err
}

func (n *testNode) nonce(ctx context.Context) (*big.Int, *big.Int, error) {
	return big.NewInt(0), big.NewInt(1), nil
}

func (n *testNode) l1FeeForCalldata(ctx context.Context, calldata []byte) (*big.Int, error) {
	return new(big.Int), nil
}

func (n *testNode) l1FeeFromReceipt(ctx context.Context, txHash common.Hash) (*big.Int, error) {
	return new(big.Int), nil
}

func (n *testNode) setBalanceError(w *assetWallet, err error) {
	n.balErr = err
	n.tokenContractor.balErr = err
	w.balances.m = nil
}

type tMempoolNode struct {
	*testNode
	pendingTxs []*types.Transaction
}

func (n *tMempoolNode) pendingTransactions() ([]*types.Transaction, error) {
	return n.pendingTxs, nil
}

type tTokenContractor struct {
	tokenAddr           common.Address
	bal                 *big.Int
	balErr              error
	transferTx          *types.Transaction
	transferErr         error
	transferEstimate    uint64
	transferEstimateErr error
	valueIn             map[common.Hash]uint64
	valueOut            map[common.Hash]uint64
	valueErr            error
}

var _ tokenContractor = (*tTokenContractor)(nil)

func (c *tTokenContractor) tokenAddress() common.Address {
	return c.tokenAddr
}

func (c *tTokenContractor) balance(context.Context) (*big.Int, error) {
	return c.bal, c.balErr
}

func (c *tTokenContractor) transfer(*bind.TransactOpts, common.Address, *big.Int) (*types.Transaction, error) {
	return c.transferTx, c.transferErr
}

func (c *tTokenContractor) parseTransfer(*types.Receipt) (uint64, error) {
	return 0, nil
}

func (c *tTokenContractor) estimateTransferGas(context.Context, *big.Int) (uint64, error) {
	return c.transferEstimate, c.transferEstimateErr
}

func (c *tTokenContractor) value(_ context.Context, tx *types.Transaction) (incoming, outgoing uint64, err error) {
	incoming, outgoing = c.valueIn[tx.Hash()], c.valueOut[tx.Hash()]
	if incoming > 0 {
		delete(c.valueIn, tx.Hash())
	}
	if outgoing > 0 {
		delete(c.valueOut, tx.Hash())
	}
	return incoming, outgoing, c.valueErr
}

type tTxDB struct {
	storeTxCalled  bool
	storedTx       *extendedWalletTx
	storeTxErr     error
	txToGet        *extendedWalletTx
	txs            map[common.Hash]*extendedWalletTx // for looking up multiple txs by hash
	getTxErr       error
	pendingBridges []*extendedWalletTx
}

var _ txDB = (*tTxDB)(nil)

func (db *tTxDB) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	return &sync.WaitGroup{}, nil
}
func (db *tTxDB) storeTx(wt *extendedWalletTx) error {
	db.storeTxCalled = true
	db.storedTx = wt
	return db.storeTxErr
}
func (db *tTxDB) getTxs(tokenID *uint32, req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	return nil, nil
}

// getTx gets a single transaction. It is not an error if the tx is not known.
// In that case, a nil tx is returned.
func (db *tTxDB) getTx(txHash common.Hash) (tx *extendedWalletTx, _ error) {
	if db.txs != nil {
		if tx, ok := db.txs[txHash]; ok {
			return tx, db.getTxErr
		}
	}
	return db.txToGet, db.getTxErr
}
func (db *tTxDB) getPendingTxs() ([]*extendedWalletTx, error) {
	panic("getPendingTxs not implemented")
}
func (db *tTxDB) getBridges(tokenID *uint32, n int, refID *common.Hash, past bool) ([]*txdb.WalletTransaction, error) {
	panic("getBridges not implemented")
}
func (db *tTxDB) getPendingBridges(tokenID *uint32) ([]*extendedWalletTx, error) {
	return db.pendingBridges, nil
}
func (db *tTxDB) getBridgeCompletions(initiationTxID string) ([]*extendedWalletTx, error) {
	return []*extendedWalletTx{db.txToGet}, db.getTxErr
}

func TestCheckPendingTxs(t *testing.T) {
	// Subtests share a single eth/node fixture that each case mutates, so this
	// test is not parallelized.
	_, eth, node, shutdown := tassetWallet(BipID)
	defer shutdown()

	const tip = 12552
	const finalized = tip - txConfsNeededToConfirm + 1
	now := uint64(time.Now().Unix())
	finalizedStamp := now - txConfsNeededToConfirm*10
	rebroadcastable := now - 300
	mature := now - 600 // able to send actions
	agedOut := now - uint64(txAgeOut.Seconds()) - 1

	val := GweiToWei(1)
	extendedTx := func(nonce, blockNum, blockStamp, submissionStamp uint64) *extendedWalletTx {
		pendingTx := eth.extendedTx(&genTxResult{
			tx:     node.newTransaction(nonce, val),
			txType: txdb.Send,
			amt:    1,
		})
		pendingTx.BlockNumber = blockNum
		pendingTx.Confirmed = blockNum > 0 && blockNum <= finalized
		pendingTx.Timestamp = blockStamp
		pendingTx.SubmissionTime = submissionStamp
		pendingTx.lastBroadcast = time.Unix(int64(submissionStamp), 0)
		pendingTx.lastFeeCheck = time.Unix(int64(submissionStamp), 0)
		return pendingTx
	}

	newReceipt := func(confs uint64) *types.Receipt {
		r := &types.Receipt{
			EffectiveGasPrice: big.NewInt(1),
		}
		if confs > 0 {
			r.BlockNumber = big.NewInt(int64(tip - confs + 1))
		}
		return r
	}

	emitChan := make(chan asset.WalletNotification, 128)
	eth.emit = asset.NewWalletEmitter(emitChan, BipID, eth.log)

	getAction := func(t *testing.T) string {
		for {
			select {
			case ni := <-emitChan:
				if n, ok := ni.(*asset.ActionRequiredNote); ok {
					return n.ActionID
				}
			default:
				t.Fatalf("no ActionRequiredNote found")
			}
		}
	}

	for _, tt := range []*struct {
		name        string
		dbErr       error
		pendingTxs  []*extendedWalletTx
		receipts    []*types.Receipt
		receiptErrs []error
		txs         []bool
		noncesAfter []uint64
		actionID    string
		bridgeDone  string
		recast      bool
		assumedLost []bool // if set, check AssumedLost on remaining txs
	}{
		{
			name: "first of two is confirmed",
			pendingTxs: []*extendedWalletTx{
				extendedTx(0, finalized, finalizedStamp, finalizedStamp),
				extendedTx(1, finalized+1, finalizedStamp, finalizedStamp), // won't even be checked.
			},
			noncesAfter: []uint64{1},
		},
		{
			name: "second one is confirmed first",
			pendingTxs: []*extendedWalletTx{
				extendedTx(2, 0, 0, rebroadcastable),
				extendedTx(3, finalized, finalizedStamp, finalizedStamp),
			},
			noncesAfter: []uint64{2, 3},
			receipts:    []*types.Receipt{nil, nil},
			txs:         []bool{false, true},
			receiptErrs: []error{asset.CoinNotFoundError, nil},
			actionID:    actionTypeLostNonce,
		},
		{
			name: "confirm one with receipt",
			pendingTxs: []*extendedWalletTx{
				extendedTx(4, 0, 0, finalizedStamp),
			},
			receipts: []*types.Receipt{newReceipt(txConfsNeededToConfirm)},
		},
		{
			name: "old and unindexed",
			pendingTxs: []*extendedWalletTx{
				extendedTx(5, 0, 0, agedOut),
			},
			noncesAfter: []uint64{5},
			receipts:    []*types.Receipt{nil},
			receiptErrs: []error{asset.CoinNotFoundError},
			txs:         []bool{false},
			actionID:    actionTypeLostNonce,
		},
		{
			name: "mature and indexed, low fees",
			pendingTxs: []*extendedWalletTx{
				extendedTx(6, 0, 0, mature),
			},
			noncesAfter: []uint64{6},
			receipts:    []*types.Receipt{newReceipt(0)},
			actionID:    actionTypeTooCheap,
		}, {
			name: "missing nonces",
			pendingTxs: []*extendedWalletTx{
				extendedTx(8, finalized, finalizedStamp, finalizedStamp),
				extendedTx(11, finalized+1, finalizedStamp, finalizedStamp),
			},
			noncesAfter: []uint64{11},
			actionID:    actionTypeMissingNonces,
		}, {
			name: "aged out with persistent RPC error marks assumed lost",
			pendingTxs: []*extendedWalletTx{
				extendedTx(9, 0, 0, agedOut),
			},
			noncesAfter: []uint64{9},
			receipts:    []*types.Receipt{nil},
			receiptErrs: []error{errors.New("json parse error")},
			txs:         []bool{false},
			assumedLost: []bool{true},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			eth.confirmedNonceAt = tt.pendingTxs[0].Nonce
			eth.pendingNonceAt = new(big.Int).Add(tt.pendingTxs[len(tt.pendingTxs)-1].Nonce, big.NewInt(1))

			node.lastSignedTx = nil
			eth.currentTip = &types.Header{Number: new(big.Int).SetUint64(tip)}
			eth.pendingTxs = tt.pendingTxs
			for i, r := range tt.receipts {
				pendingTx := tt.pendingTxs[i]
				if tt.receiptErrs != nil {
					node.receiptErrs[pendingTx.txHash] = tt.receiptErrs[i]
				}
				if r == nil {
					continue
				}
				node.receipts[pendingTx.txHash] = r
				if len(tt.txs) < i+1 || tt.txs[i] {
					node.receiptTxs[pendingTx.txHash], _ = pendingTx.tx()
				}
				if pendingTx.Timestamp == 0 && r.BlockNumber != nil && r.BlockNumber.Uint64() != 0 {
					node.hdrByHash[r.BlockHash] = &types.Header{
						Number: r.BlockNumber,
						Time:   now,
					}
				}
			}
			eth.checkPendingTxs()
			if len(eth.pendingTxs) != len(tt.noncesAfter) {
				t.Fatalf("wrong number of pending txs. expected %d got %d", len(tt.noncesAfter), len(eth.pendingTxs))
			}
			for i, pendingTx := range eth.pendingTxs {
				if pendingTx.Nonce.Uint64() != tt.noncesAfter[i] {
					t.Fatalf("Expected nonce %d at index %d, but got nonce %s", tt.noncesAfter[i], i, pendingTx.Nonce)
				}
			}
			if tt.actionID != "" {
				if actionID := getAction(t); actionID != tt.actionID {
					t.Fatalf("expected action %s, got %s", tt.actionID, actionID)
				}
			}
			if tt.recast != (node.lastSignedTx != nil) {
				t.Fatalf("wrong recast result recast = %t, lastSignedTx = %t", tt.recast, node.lastSignedTx != nil)
			}
			for i, expLost := range tt.assumedLost {
				if eth.pendingTxs[i].AssumedLost != expLost {
					t.Fatalf("expected AssumedLost=%v for tx at index %d, got %v", expLost, i, eth.pendingTxs[i].AssumedLost)
				}
			}
		})
	}
}

// TestCheckPendingTxsHeaderMiss exercises the path where the phase-2 header
// pre-fetch returns nil (provider doesn't have the block yet). The tx must not
// be finalized on the first call, must not trigger a lost-nonce action, and
// must finalize correctly once the header becomes available on the next tip.
// It also verifies that subsequent header-miss retries do not write to the DB
// again (updated=false prevents spam writes).
func TestCheckPendingTxsHeaderMiss(t *testing.T) {
	_, eth, node, shutdown := tassetWallet(BipID)
	defer shutdown()

	const tip1 = uint64(1000)
	const tip2 = uint64(1001)
	const tip3 = uint64(1002)
	now := uint64(time.Now().Unix())

	emitChan := make(chan asset.WalletNotification, 128)
	eth.emit = asset.NewWalletEmitter(emitChan, BipID, eth.log)

	db := eth.txDB.(*tTxDB)

	val := GweiToWei(1)
	pendingTx := eth.extendedTx(&genTxResult{
		tx:     node.newTransaction(0, val),
		txType: txdb.Send,
		amt:    1,
	})
	// Timestamp=0 forces a header fetch; BlockNumber=0 means not yet mined.
	pendingTx.Timestamp = 0
	pendingTx.BlockNumber = 0

	receipt := &types.Receipt{
		BlockNumber:       big.NewInt(int64(tip1 - 2)),
		EffectiveGasPrice: big.NewInt(1),
	}
	node.receipts[pendingTx.txHash] = receipt
	node.receiptTxs[pendingTx.txHash], _ = pendingTx.tx()
	// Deliberately omit node.hdrByHash entry so headerByHash returns (nil, nil).

	eth.currentTip = &types.Header{Number: new(big.Int).SetUint64(tip1)}
	eth.pendingTxs = []*extendedWalletTx{pendingTx}
	eth.confirmedNonceAt = pendingTx.Nonce
	eth.pendingNonceAt = new(big.Int).Add(pendingTx.Nonce, big.NewInt(1))

	eth.checkPendingTxs()

	// Tx must still be pending: header was unavailable so state is incomplete.
	if len(eth.pendingTxs) != 1 {
		t.Fatalf("expected 1 pending tx after header miss, got %d", len(eth.pendingTxs))
	}
	if pendingTx.Timestamp != 0 {
		t.Errorf("Timestamp should still be 0 after header miss, got %d", pendingTx.Timestamp)
	}
	if pendingTx.BlockNumber != 0 {
		t.Errorf("BlockNumber should still be 0 after header miss, got %d", pendingTx.BlockNumber)
	}
	// First save should have occurred via the !savedToDB path.
	if !db.storeTxCalled {
		t.Error("expected first DB save via !savedToDB, but storeTx was not called")
	}
	// No spurious action should be queued.
	select {
	case n := <-emitChan:
		if _, ok := n.(*asset.ActionRequiredNote); ok {
			t.Fatalf("unexpected action note after header miss: %v", n)
		}
	default:
	}

	// Second run: header still missing, tip advanced. Should NOT write to DB
	// again — updated=false prevents spam writes on every tip.
	db.storeTxCalled = false
	eth.currentTip = &types.Header{Number: new(big.Int).SetUint64(tip2)}
	eth.checkPendingTxs()
	if db.storeTxCalled {
		t.Error("DB should not be written again on subsequent header-miss retries")
	}

	// Third run: header now available. Tx should finalize and DB should be written.
	node.hdrByHash[receipt.BlockHash] = &types.Header{
		Number: receipt.BlockNumber,
		Time:   now,
	}
	db.storeTxCalled = false
	eth.currentTip = &types.Header{Number: new(big.Int).SetUint64(tip3)}
	eth.checkPendingTxs()

	if pendingTx.Timestamp == 0 {
		t.Error("Timestamp should be set after header becomes available")
	}
	if pendingTx.BlockNumber == 0 {
		t.Error("BlockNumber should be set after header becomes available")
	}
	if !db.storeTxCalled {
		t.Error("expected DB write after successful finalization")
	}
}

func TestTakeAction(t *testing.T) {
	t.Parallel()
	_, eth, node, shutdown := tassetWallet(BipID)
	defer shutdown()

	aGwei := GweiToWei(1)
	pendingTx := eth.extendedTx(&genTxResult{
		tx:     node.newTransaction(0, aGwei),
		txType: txdb.Send,
		amt:    1,
	})
	eth.pendingTxs = []*extendedWalletTx{pendingTx}

	feeCap := new(big.Int).Mul(aGwei, big.NewInt(5))
	tipCap := new(big.Int).Mul(aGwei, big.NewInt(2))
	replacementTx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		Nonce:     1,
		GasTipCap: tipCap,
		GasFeeCap: feeCap,
		Gas:       50_000,
		ChainID:   node.chainConfig().ChainID,
	}), signer, node.privKey)
	node.sendTxTx = replacementTx

	tooCheapAction := fmt.Appendf(nil, `{"txID":"%s","bump":true}`, pendingTx.ID)
	if err := eth.TakeAction(actionTypeTooCheap, tooCheapAction); err != nil {
		t.Fatalf("TakeAction error: %v", err)
	}

	newPendingTx := eth.pendingTxs[0]
	if pendingTx.txHash == newPendingTx.txHash {
		t.Fatal("tx wasn't replaced")
	}
	tx, _ := newPendingTx.tx()
	if tx.GasFeeCap().Cmp(feeCap) != 0 {
		t.Fatalf("wrong fee cap. wanted %s, got %s", feeCap, tx.GasFeeCap())
	}
	if tx.GasTipCap().Cmp(tipCap) != 0 {
		t.Fatalf("wrong tip cap. wanted %s, got %s", tipCap, tx.GasTipCap())
	}
	if !newPendingTx.savedToDB {
		t.Fatal("didn't save to DB")
	}

	pendingTx = eth.extendedTx(&genTxResult{
		tx:     node.newTransaction(1, aGwei),
		txType: txdb.Send,
		amt:    1,
	})
	eth.pendingTxs = []*extendedWalletTx{pendingTx}
	pendingTx.SubmissionTime = 0
	// Neglecting to bump should reset submission time.
	tooCheapAction = fmt.Appendf(nil, `{"txID":"%s","bump":false}`, pendingTx.ID)
	if err := eth.TakeAction(actionTypeTooCheap, tooCheapAction); err != nil {
		t.Fatalf("TakeAction bump=false error: %v", err)
	}
	tx, _ = pendingTx.tx()
	if tx.GasTipCap().Uint64() != 0 {
		t.Fatal("The fee was bumped. The fee shouldn't have been bumped.")
	}
	if pendingTx.actionIgnored.IsZero() {
		t.Fatalf("The ignore time wasn't reset")
	}
	if len(eth.pendingTxs) != 1 {
		t.Fatalf("Tx was removed")
	}

	// Nonce-replaced tx
	eth.pendingTxs = []*extendedWalletTx{pendingTx}
	lostNonceAction := fmt.Appendf(nil, `{"txID":"%s","abandon":true}`, pendingTx.ID)
	if err := eth.TakeAction(actionTypeLostNonce, lostNonceAction); err != nil {
		t.Fatalf("TakeAction replacment=false, abandon=true error: %v", err)
	}
	if len(eth.pendingTxs) != 0 {
		t.Fatalf("Tx wasn't abandoned")
	}
	eth.pendingTxs = []*extendedWalletTx{pendingTx}
	node.getTxRes = replacementTx
	lostNonceAction = fmt.Appendf(nil, `{"txID":"%s","abandon":false,"replacementID":"%s"}`, pendingTx.ID, replacementTx.Hash())
	if err := eth.TakeAction(actionTypeLostNonce, lostNonceAction); err != nil {
		t.Fatalf("TakeAction replacment=true, error: %v", err)
	}
	newPendingTx = eth.pendingTxs[0]
	if newPendingTx.txHash != replacementTx.Hash() {
		t.Fatalf("replacement tx wasn't accepted")
	}
	// wrong nonce is an error though
	pendingTx = eth.extendedTx(&genTxResult{
		tx:     node.newTransaction(5050, aGwei),
		txType: txdb.Send,
		amt:    1,
	})
	eth.pendingTxs = []*extendedWalletTx{pendingTx}
	lostNonceAction = fmt.Appendf(nil, `{"txID":"%s","abandon":false,"replacementID":"%s"}`, pendingTx.ID, replacementTx.Hash())
	if err := eth.TakeAction(actionTypeLostNonce, lostNonceAction); err == nil {
		t.Fatalf("no error for wrong nonce")
	}

	// Missing nonces
	tx5 := eth.extendedTx(&genTxResult{
		tx:     node.newTransaction(5, aGwei),
		txType: txdb.Send,
		amt:    1,
	})
	eth.pendingTxs = []*extendedWalletTx{tx5}
	eth.confirmedNonceAt = big.NewInt(2)
	eth.pendingNonceAt = big.NewInt(6)
	nonceRecoveryAction := []byte(`{"recover":true}`)
	node.sentTxs = 0
	if err := eth.TakeAction(actionTypeMissingNonces, nonceRecoveryAction); err != nil {
		t.Fatalf("error for nonce recover: %v", err)
	}
	if node.sentTxs != 3 {
		t.Fatalf("expected 2 new txs. saw %d", node.sentTxs)
	}

}

func TestApplyTipChange(t *testing.T) {
	t.Parallel()
	header0 := &types.Header{Number: new(big.Int)}
	header1 := &types.Header{Number: big.NewInt(1)}
	tests := []struct {
		name         string
		bestHeader   *types.Header
		hasTipChange bool
	}{{
		name:         "ok",
		bestHeader:   header1,
		hasTipChange: true,
	}, {
		name:       "ok same hash",
		bestHeader: header0,
	}, {
		name:       "ignore lower height",
		bestHeader: &types.Header{Number: big.NewInt(-1)},
	}}

	for _, test := range tests {
		ctx, cancel := context.WithCancel(context.Background())
		node := &testNode{}
		notes := make(chan asset.WalletNotification, 1)
		emit := asset.NewWalletEmitter(notes, BipID, tLogger)

		node.bestHdr = test.bestHeader
		w := &ETHWallet{
			assetWallet: &assetWallet{
				baseWallet: &baseWallet{
					node:             node,
					addr:             node.address(),
					ctx:              ctx,
					log:              tLogger,
					currentTip:       header0,
					confirmedNonceAt: new(big.Int),
					pendingNonceAt:   new(big.Int),
					txDB:             &tTxDB{},
					finalizeConfs:    txConfsNeededToConfirm,
				},
				log:     tLogger.SubLogger("ETH"),
				emit:    emit,
				assetID: BipID,
			},
		}
		w.wallets = map[uint32]*assetWallet{BipID: w.assetWallet}
		w.assetWallet.connected.Store(true)
		w.applyTipChange(test.bestHeader)

		if test.hasTipChange {
		out:
			for {
				select {
				case ni := <-notes:
					switch n := ni.(type) {
					case *asset.TipChangeNote:
						if n.Tip != test.bestHeader.Number.Uint64() {
							t.Fatalf("expected tip change but didn't get it")
						}
						break out
					}
				case <-time.After(time.Second * 5):
					t.Fatal("timed out waiting for tip change")
				}
			}
		} else {
			if w.currentTip.Number.Cmp(header0.Number) != 0 {
				t.Fatalf("tip was changed. wasn't supposed to be changed")
			}
		}
		cancel()

	}
}

func TestApplyTipChangeDoesNotWaitForPendingTxChecks(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	header0 := &types.Header{Number: big.NewInt(0)}
	header1 := &types.Header{Number: big.NewInt(1)}
	blockRPC := make(chan struct{})
	rpcHit := make(chan struct{}, 1)
	node := &testNode{
		bestHdr:           header1,
		txAndReceiptBlock: blockRPC,
		txAndReceiptHit:   rpcHit,
	}
	notes := make(chan asset.WalletNotification, 2)
	emit := asset.NewWalletEmitter(notes, BipID, tLogger)

	pendingTx := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			Type:   txdb.Send,
			Amount: 1,
			Fees:   1,
		},
		txHash: common.HexToHash("0x1"),
		Nonce:  big.NewInt(1),
	}
	w := &ETHWallet{
		assetWallet: &assetWallet{
			baseWallet: &baseWallet{
				node:             node,
				addr:             node.address(),
				ctx:              ctx,
				log:              tLogger,
				currentTip:       header0,
				confirmedNonceAt: new(big.Int),
				pendingNonceAt:   big.NewInt(2),
				pendingTxs:       []*extendedWalletTx{pendingTx},
				txDB:             &tTxDB{},
				finalizeConfs:    txConfsNeededToConfirm,
			},
			log:     tLogger.SubLogger("ETH"),
			emit:    emit,
			assetID: BipID,
		},
	}
	w.wallets = map[uint32]*assetWallet{BipID: w.assetWallet}
	w.assetWallet.connected.Store(true)

	w.applyTipChange(header1)

	select {
	case <-rpcHit:
	case <-time.After(time.Second):
		t.Fatal("transactionAndReceipt was not called")
	}

	select {
	case ni := <-notes:
		tipNote, ok := ni.(*asset.TipChangeNote)
		if !ok {
			t.Fatalf("expected tip change note, got %T", ni)
		}
		if tipNote.Tip != 1 {
			t.Fatalf("expected tip 1, got %d", tipNote.Tip)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("tip change blocked on pending tx receipt fetch")
	}

	close(blockRPC)
	awaitPendingTxChecks(t, w.baseWallet)
}

// awaitPendingTxChecks waits out the background pending-tx check that
// applyTipChange queues. A test that returns while one is still in flight
// leaves it racing that test's own cleanup: the deferred context cancellation
// is the only thing that stops it, and checkPendingTxs only rechecks the
// context between phases, so it can go on to apply results against a node the
// test has finished with. That is how a nil receipt from an unstubbed hash used
// to surface - as a panic in an unrelated test's output.
func awaitPendingTxChecks(t *testing.T, w *baseWallet) {
	t.Helper()
	for range 200 {
		if !w.pendingTxCheckRunning.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pending tx check still running after 2s")
}

func TestApplyTipChangeCoalescesPendingTxChecks(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	header0 := &types.Header{Number: big.NewInt(0)}
	blockRPC := make(chan struct{})
	rpcHit := make(chan struct{}, 4)
	node := &testNode{
		txAndReceiptBlock: blockRPC,
		txAndReceiptHit:   rpcHit,
		receiptErr:        asset.CoinNotFoundError,
	}
	w := &ETHWallet{
		assetWallet: &assetWallet{
			baseWallet: &baseWallet{
				node:             node,
				addr:             node.address(),
				ctx:              ctx,
				log:              tLogger,
				currentTip:       header0,
				confirmedNonceAt: new(big.Int),
				pendingNonceAt:   big.NewInt(2),
				pendingTxs: []*extendedWalletTx{{
					WalletTransaction: &txdb.WalletTransaction{Type: txdb.Send, Amount: 1, Fees: 1},
					txHash:            common.HexToHash("0x1"),
					Nonce:             big.NewInt(1),
				}},
				txDB:          &tTxDB{},
				finalizeConfs: txConfsNeededToConfirm,
			},
			log:     tLogger.SubLogger("ETH"),
			emit:    asset.NewWalletEmitter(make(chan asset.WalletNotification, 4), BipID, tLogger),
			assetID: BipID,
		},
	}
	w.wallets = map[uint32]*assetWallet{BipID: w.assetWallet}
	w.assetWallet.connected.Store(true)

	w.applyTipChange(&types.Header{Number: big.NewInt(1)})
	select {
	case <-rpcHit:
	case <-time.After(time.Second):
		t.Fatal("first pending tx check did not start")
	}

	w.applyTipChange(&types.Header{Number: big.NewInt(2)})
	select {
	case <-rpcHit:
		t.Fatal("pending tx checks ran concurrently")
	case <-time.After(100 * time.Millisecond):
	}

	close(blockRPC)

	select {
	case <-rpcHit:
	case <-time.After(time.Second):
		t.Fatal("queued pending tx check did not run after the first completed")
	}

	awaitPendingTxChecks(t, w.baseWallet)
}

func TestSendLatestHeader(t *testing.T) {
	t.Parallel()

	hdr1 := &types.Header{Number: big.NewInt(1)}
	hdr2 := &types.Header{Number: big.NewInt(2)}
	ch := make(chan *types.Header, 1)
	ch <- hdr1

	sendLatestHeader(ch, hdr2)

	select {
	case hdr := <-ch:
		if hdr != hdr2 {
			t.Fatalf("expected latest header %v, got %v", hdr2.Number, hdr.Number)
		}
	default:
		t.Fatal("expected header in channel")
	}
}

func TestQueuePendingTxCheckNilContext(t *testing.T) {
	t.Parallel()

	w := &baseWallet{}
	w.queuePendingTxCheck()

	if w.pendingTxCheckQueued.Load() {
		t.Fatal("nil-context wallet should not queue pending tx checks")
	}
	if w.pendingTxCheckRunning.Load() {
		t.Fatal("nil-context wallet should not start pending tx checks")
	}
}

// TestCheckPendingTxsNilReceipt covers a node that answers with neither a
// receipt nor an error. updatePendingTx read the nil error as "we have a
// receipt" and dereferenced it one line later, taking the wallet down from the
// goroutine applyTipChange queues, where nothing recovers and the panic lands
// in whatever test happened to be running.
func TestCheckPendingTxsNilReceipt(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	node := &testNode{
		bestHdr:         &types.Header{Number: big.NewInt(1)},
		nilReceiptNoErr: true,
	}
	pendingTx := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{Type: txdb.Send, Amount: 1, Fees: 1},
		txHash:            common.HexToHash("0x1"),
		Nonce:             big.NewInt(1),
	}
	w := &ETHWallet{
		assetWallet: &assetWallet{
			baseWallet: &baseWallet{
				node:             node,
				addr:             node.address(),
				ctx:              ctx,
				log:              tLogger,
				currentTip:       &types.Header{Number: big.NewInt(1)},
				confirmedNonceAt: new(big.Int),
				pendingNonceAt:   big.NewInt(2),
				pendingTxs:       []*extendedWalletTx{pendingTx},
				txDB:             &tTxDB{},
				finalizeConfs:    txConfsNeededToConfirm,
			},
			log:     tLogger.SubLogger("ETH"),
			emit:    asset.NewWalletEmitter(make(chan asset.WalletNotification, 4), BipID, tLogger),
			assetID: BipID,
		},
	}
	w.wallets = map[uint32]*assetWallet{BipID: w.assetWallet}
	w.assetWallet.connected.Store(true)

	w.checkPendingTxs()

	// Treated as not found, which is what the node actually told us: the tx is
	// still pending, unconfirmed, and not written off as rejected or lost.
	if pendingTx.Confirmed || pendingTx.Rejected || pendingTx.AssumedLost {
		t.Fatalf("nil receipt resolved the tx (confirmed %v, rejected %v, lost %v)",
			pendingTx.Confirmed, pendingTx.Rejected, pendingTx.AssumedLost)
	}
	if pendingTx.Receipt != nil {
		t.Fatal("a nil receipt was stored on the pending tx")
	}
}

// TestETHGetReceivingTransactionNilDerefs covers the two nils
// ETHWallet.getReceivingTransaction dereferences on one line. The first is the
// same pair TestCheckPendingTxsNilReceipt covers, from the other fetch: node is
// an interface, and nothing states that a nil transaction always comes with an
// error. The second needs no misbehaving node at all - To() is nil for a
// contract creation, an ordinary on-chain shape, and this wallet is never its
// recipient.
func TestETHGetReceivingTransactionNilDerefs(t *testing.T) {
	t.Parallel()

	nilTxHash := common.HexToHash("0x1")
	contractCreationHash := common.HexToHash("0x2")
	node := &testNode{
		getTxResMap: map[common.Hash]*tGetTxRes{
			// No error, no transaction.
			nilTxHash: {},
			contractCreationHash: {tx: types.NewContractCreation(
				1, big.NewInt(1), 1, big.NewInt(1), nil), height: 1},
		},
	}
	w := &ETHWallet{
		assetWallet: &assetWallet{
			baseWallet: &baseWallet{
				node: node,
				addr: node.address(),
				log:  tLogger,
			},
			log:     tLogger.SubLogger("ETH"),
			assetID: BipID,
		},
	}

	for _, tt := range []struct {
		name string
		hash common.Hash
	}{
		{"nil transaction under a nil error", nilTxHash},
		{"contract creation has no recipient", contractCreationHash},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.getReceivingTransaction(t.Context(), tt.hash)
			if !errors.Is(err, asset.CoinNotFoundError) {
				t.Fatalf("expected CoinNotFoundError, got %v", err)
			}
		})
	}
}

func TestSyncStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                string
		syncProg            ethereum.SyncProgress
		syncProgErr         error
		subSecs             uint64
		wantErr, wantSynced bool
		wantRatio           float32
	}{{
		name: "ok synced",
		syncProg: ethereum.SyncProgress{
			CurrentBlock: 25,
			HighestBlock: 25,
		},
		wantRatio:  1,
		wantSynced: true,
	}, {
		name: "ok header too old",
		syncProg: ethereum.SyncProgress{
			CurrentBlock: 25,
			HighestBlock: 25,
		},
		subSecs:    MaxBlockInterval + 1,
		wantRatio:  0.999,
		wantSynced: false,
	}, {
		name: "sync progress error",
		syncProg: ethereum.SyncProgress{
			CurrentBlock: 25,
			HighestBlock: 0,
		},
		syncProgErr: errors.New("test error"),
		wantErr:     true,
	}}

	for _, test := range tests {
		nowInSecs := uint64(time.Now().Unix())
		ctx, cancel := context.WithCancel(context.Background())
		node := &testNode{
			syncProg:    test.syncProg,
			syncProgT:   nowInSecs - test.subSecs,
			syncProgErr: test.syncProgErr,
		}
		eth := &baseWallet{
			node:          node,
			addr:          node.address(),
			ctx:           ctx,
			log:           tLogger,
			finalizeConfs: txConfsNeededToConfirm,
		}
		ss, err := eth.SyncStatus()
		cancel()
		if test.wantErr {
			if err == nil {
				t.Fatalf("expected error for test %q", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unexpected error for test %q: %v", test.name, err)
		}
		if ss.Synced != test.wantSynced {
			t.Fatalf("want synced %v got %v for test %q", test.wantSynced, ss.Synced, test.name)
		}
		if ss.BlockProgress() != test.wantRatio {
			t.Fatalf("want ratio %v got %v for test %q", test.wantRatio, ss.BlockProgress(), test.name)
		}
	}
}

func newTestNode(assetID uint32) *tMempoolNode {
	privKey, _ := crypto.HexToECDSA("9447129055a25c8496fca9e5ee1b9463e47e6043ff0c288d07169e8284860e34")
	addr := common.HexToAddress("2b84C791b79Ee37De042AD2ffF1A253c3ce9bc27")
	acct := &accounts.Account{
		Address: addr,
	}

	ttc := &tTokenContractor{
		valueIn:  make(map[common.Hash]uint64),
		valueOut: make(map[common.Hash]uint64),
	}

	return &tMempoolNode{
		testNode: &testNode{
			acct:            acct,
			addr:            acct.Address,
			maxFeeRate:      GweiToWei(100),
			baseFee:         GweiToWei(100),
			tip:             GweiToWei(2),
			privKey:         privKey,
			tokenContractor: ttc,
			txConfirmations: make(map[common.Hash]uint32),
			txConfsErr:      make(map[common.Hash]error),
			receipts:        make(map[common.Hash]*types.Receipt),
			receiptErrs:     make(map[common.Hash]error),
			receiptTxs:      make(map[common.Hash]*types.Transaction),
			hdrByHash:       make(map[common.Hash]*types.Header),
		},
	}
}

func tassetWallet(assetID uint32) (asset.Wallet, *assetWallet, *tMempoolNode, context.CancelFunc) {
	node := newTestNode(assetID)
	ctx, cancel := context.WithCancel(context.Background())

	versionedGases := make(map[uint32]*Gases)
	if assetID == BipID { // just make a copy
		maps.Copy(versionedGases, VersionedGases)
	} else {
		netToken := Tokens[assetID].NetTokens[util.Simnet]
		maps.Copy(versionedGases, netToken.Gas)
	}

	emitChan := make(chan asset.WalletNotification, 128)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-emitChan:
			}
		}
	}()

	aw := &assetWallet{
		baseWallet: &baseWallet{
			baseChainID:      BipID,
			chainID:          ChainIDs[util.Simnet],
			tokens:           Tokens,
			addr:             node.addr,
			net:              util.Simnet,
			node:             node,
			ctx:              ctx,
			log:              tLogger,
			gasFeeLimitV:     defaultGasFeeLimit,
			pendingNonceAt:   new(big.Int),
			confirmedNonceAt: new(big.Int),
			pendingTxs:       make([]*extendedWalletTx, 0),
			txDB:             &tTxDB{},
			currentTip:       &types.Header{Number: new(big.Int), GasLimit: 30_000_000},
			finalizeConfs:    txConfsNeededToConfirm,
			maxTxFeeGwei:     GweiFactor, // 1 ETH
		},
		versionedGases:    versionedGases,
		log:               tLogger.SubLogger(strings.ToUpper(util.BipIDSymbol(assetID))),
		assetID:           assetID,
		evmify:            GweiToWei,
		atomize:           WeiToGwei,
		pendingTxCheckBal: new(big.Int),
		pendingApprovals:  make(map[common.Address]*pendingApproval),
		approvalCache:     make(map[common.Address]bool),
		// move up after review
		wi:   WalletInfo,
		emit: asset.NewWalletEmitter(emitChan, BipID, tLogger),
	}
	if assetID != BipID {
		aw.tokenContractor = node.tokenContractor
	}
	aw.wallets = map[uint32]*assetWallet{
		BipID: aw,
	}

	var w asset.Wallet
	if assetID == BipID {
		w = &ETHWallet{assetWallet: aw}
		aw.wallets = map[uint32]*assetWallet{
			BipID: aw,
		}
	} else {
		node.tokenParent = &assetWallet{
			baseWallet:        aw.baseWallet,
			log:               tLogger.SubLogger("ETH"),
			assetID:           BipID,
			evmify:            GweiToWei,
			atomize:           WeiToGwei,
			pendingTxCheckBal: new(big.Int),
			pendingApprovals:  make(map[common.Address]*pendingApproval),
			approvalCache:     make(map[common.Address]bool),
			emit:              asset.NewWalletEmitter(emitChan, BipID, tLogger),
		}
		w = &TokenWallet{
			assetWallet: aw,
			cfg:         &tokenWalletConfig{},
			parent:      node.tokenParent,
			token:       Tokens[assetID],
			netToken:    Tokens[assetID].NetTokens[util.Simnet],
		}
		aw.wallets = map[uint32]*assetWallet{
			assetID: aw,
			BipID:   node.tokenParent,
		}
	}

	return w, aw, node, cancel
}

func TestBalanceWithMempool(t *testing.T) {
	t.Parallel()
	tinyBal := newBalance(0, 0, 0)
	tinyBal.Current = big.NewInt(GweiFactor - 1)

	// subGweiOutBal carries a half-gwei remainder in pending-out. Available is
	// computed as floor((current - pendingOut)/gwei), so 4e8 - (1.4e8 + 0.5)
	// gwei floors to 2.6e8 - 1. Flooring pending-out independently first (the
	// old behavior) would have dropped the half gwei and yielded 2.6e8.
	subGweiOutBal := newBalance(4e8, 0, 0)
	subGweiOutBal.PendingOut = new(big.Int).Add(GweiToWei(1.4e8), big.NewInt(GweiFactor/2))

	tests := []struct {
		name         string
		bal          *Balance
		balErr       error
		wantErr      bool
		wantBal      uint64
		wantImmature uint64
		wantLocked   uint64
		token        bool
	}{{
		name:         "ok zero",
		bal:          newBalance(0, 0, 0),
		wantBal:      0,
		wantImmature: 0,
	}, {
		name:    "ok rounded down",
		bal:     tinyBal,
		wantBal: 0,
	}, {
		name:    "ok one",
		bal:     newBalance(1, 0, 0),
		wantBal: 1,
	}, {
		name:    "ok one token",
		bal:     newBalance(1, 0, 0),
		wantBal: 1,
		token:   true,
	}, {
		name:       "ok pending out",
		bal:        newBalance(4e8, 0, 1.4e8),
		wantBal:    2.6e8,
		wantLocked: 0,
	}, {
		name:    "ok pending out sub-gwei floored once",
		bal:     subGweiOutBal,
		wantBal: 2.6e8 - 1,
	}, {
		name:       "ok pending out token",
		bal:        newBalance(4e8, 0, 1.4e8),
		wantBal:    2.6e8,
		wantLocked: 0,
		token:      true,
	}, {
		// ETH pending-in was previously detected via swap redemptions,
		// which have been removed. ETH now reports no immature balance.
		name:         "ok pending in",
		bal:          newBalance(1e8, 3e8, 0),
		wantBal:      1e8,
		wantImmature: 0,
	}, {
		name:         "ok pending in token",
		bal:          newBalance(1e8, 3e8, 0),
		wantBal:      1e8,
		wantImmature: 3e8,
		token:        true,
	}, {
		name:         "ok pending out and in",
		bal:          newBalance(4e8, 2e8, 1e8),
		wantBal:      3e8,
		wantLocked:   0,
		wantImmature: 0,
	}, {
		name:         "ok pending out and in token",
		bal:          newBalance(4e8, 2e8, 1e8),
		wantBal:      3e8,
		wantLocked:   0,
		wantImmature: 2e8,
		token:        true,
	}, {
		// 	name:         "ok max int",
		// 	bal:          maxWei,
		// 	pendingTxs:   []*types.Transaction{},
		// 	wantBal:      maxInt,
		// 	wantImmature: 0,
		// }, {
		// 	name:       "over max int",
		// 	bal:        overMaxWei,
		// 	pendingTxs: []*types.Transaction{},
		// 	wantErr:    true,
		// }, {
		name:    "node balance error",
		bal:     newBalance(0, 0, 0),
		balErr:  errors.New("test error"),
		wantErr: true,
	}}

	for _, test := range tests {
		var assetID uint32 = BipID
		if test.token {
			assetID = usdcEthID
		}

		_, eth, node, shutdown := tassetWallet(assetID)
		defer shutdown()

		if test.token {
			node.tokenContractor.bal = test.bal.Current
			node.tokenContractor.balErr = test.balErr
		} else {
			node.bal = test.bal.Current
			node.balErr = test.balErr
		}

		var nonce uint64
		newTx := func(value *big.Int) *types.Transaction {
			nonce++
			return node.newTransaction(nonce, value)
		}

		if test.bal.PendingIn.Cmp(new(big.Int)) > 0 {
			// Incoming pending value is only detected for tokens, via the
			// token contractor's parse of incoming transfers.
			tx := newTx(new(big.Int))
			node.pendingTxs = append(node.pendingTxs, tx)
			if test.token {
				node.tokenContractor.valueIn[tx.Hash()] = WeiToGwei(test.bal.PendingIn)
			}
		}
		if test.bal.PendingOut.Cmp(new(big.Int)) > 0 {
			if test.token {
				tx := newTx(nil)
				node.pendingTxs = append(node.pendingTxs, tx)
				node.tokenContractor.valueOut[tx.Hash()] = WeiToGwei(test.bal.PendingOut)
			} else {
				node.pendingTxs = append(node.pendingTxs, newTx(test.bal.PendingOut))
			}
		}

		bal, err := eth.Balance()
		if test.wantErr {
			if err == nil {
				t.Fatalf("expected error for test %q", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unexpected error for test %q: %v", test.name, err)
		}
		if bal.Available != test.wantBal {
			t.Fatalf("%s: want available balance %v got %v for test %q", test.name, test.wantBal, bal.Available, test.name)
		}
		if bal.Immature != test.wantImmature {
			t.Fatalf("%s: want immature balance %v got %v for test %q", test.name, test.wantImmature, bal.Immature, test.name)
		}
		if bal.Locked != test.wantLocked {
			t.Fatalf("%s: want locked balance %v got %v for test %q", test.name, test.wantLocked, bal.Locked, test.name)
		}
	}
}

func TestBalanceNoMempool(t *testing.T) {
	t.Parallel()
	var nonceCounter int64

	newExtendedWalletTx := func(assetID uint32, amt, maxFees, blockNum uint64, txType txdb.TransactionType) *extendedWalletTx {
		var tokenID *uint32
		if assetID != BipID {
			tokenID = &assetID
		}
		txHash := common.BytesToHash(encode.RandomBytes(32))

		et := &extendedWalletTx{
			WalletTransaction: &txdb.WalletTransaction{
				ID:          txHash.String(),
				Type:        txType,
				Amount:      amt,
				BlockNumber: blockNum,
				TokenID:     tokenID,
				Fees:        maxFees,
			},
			Nonce:  big.NewInt(nonceCounter),
			txHash: txHash,
		}
		nonceCounter++

		return et
	}

	tests := []struct {
		name           string
		assetID        uint32
		unconfirmedTxs []*extendedWalletTx
		expPendingIn   uint64
		expPendingOut  uint64
	}{
		{
			name:    "single eth tx",
			assetID: BipID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(BipID, 1, 2, 0, txdb.Send),
			},
			expPendingOut: 3,
		},
		{
			name:    "eth with token fees",
			assetID: BipID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(usdcEthID, 4, 5, 0, txdb.Send),
			},
			expPendingOut: 5,
		},
		{
			name:    "token with 1 tx and other ignored assets",
			assetID: usdcEthID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(usdcEthID, 4, 5, 0, txdb.Send),
				newExtendedWalletTx(usdcEthID+1, 8, 9, 0, txdb.Send),
			},
			expPendingOut: 4,
		},
		{
			name:    "token with 1 tx incoming",
			assetID: usdcEthID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(usdcEthID, 15, 5, 0, txdb.Redeem),
			},
			expPendingIn: 15,
		},
		{
			name:    "eth mixed txs",
			assetID: BipID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(BipID, 1, 2, 0, txdb.Swap),     // 3 eth out
				newExtendedWalletTx(usdcEthID, 3, 4, 1, txdb.Send), // confirmed
				newExtendedWalletTx(usdcEthID, 5, 6, 0, txdb.Swap), // 6 eth out
				newExtendedWalletTx(BipID, 7, 1, 0, txdb.Refund),   // 1 eth out, 7 eth in
			},
			expPendingOut: 10,
			expPendingIn:  7,
		},
		{
			name:    "already confirmed, but still waiting for txConfsNeededToConfirm",
			assetID: usdcEthID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(usdcEthID, 15, 5, 1, txdb.Redeem),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, eth, node, shutdown := tassetWallet(tt.assetID)
			defer shutdown()
			eth.node = node.testNode // no mempool
			node.bal = unlimitedAllowance
			node.tokenContractor.bal = unlimitedAllowance
			eth.connected.Store(true)

			eth.pendingTxs = tt.unconfirmedTxs

			bal, err := eth.balanceWithTxPool()
			if err != nil {
				t.Fatalf("balanceWithTxPool error: %v", err)
			}

			if in := WeiToGwei(bal.PendingIn); in != tt.expPendingIn {
				t.Fatalf("wrong PendingIn. wanted %d, got %d", tt.expPendingIn, in)
			}

			if out := WeiToGwei(bal.PendingOut); out != tt.expPendingOut {
				t.Fatalf("wrong PendingOut. wanted %d, got %d", tt.expPendingOut, out)
			}
		})
	}
}

func TestFeeRate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	node := &testNode{}
	eth := &baseWallet{
		node:          node,
		ctx:           ctx,
		log:           tLogger,
		finalizeConfs: txConfsNeededToConfirm,
		currentTip:    &types.Header{Number: big.NewInt(100)},
	}

	maxInt := ^int64(0)
	tests := []struct {
		name           string
		baseFee        *big.Int
		tip            *big.Int
		netFeeStateErr error
		wantFeeRate    uint64
	}{
		{
			name:        "ok",
			baseFee:     big.NewInt(100e9),
			tip:         big.NewInt(2e9),
			wantFeeRate: 202,
		},
		{
			name:           "net fee state error",
			netFeeStateErr: errors.New("test error"),
		},
		{
			name:    "overflow error",
			baseFee: big.NewInt(maxInt),
			tip:     big.NewInt(1),
		},
	}

	for _, test := range tests {
		eth.currentFees.blockNum = 0
		node.baseFee = test.baseFee
		node.tip = test.tip
		node.netFeeStateErr = test.netFeeStateErr
		feeRate := eth.FeeRate()
		if feeRate != test.wantFeeRate {
			t.Fatalf("%v: expected fee rate %d but got %d", test.name, test.wantFeeRate, feeRate)
		}
	}
}

func TestOwnsAddress(t *testing.T) {
	t.Parallel()
	address := "0b84C791b79Ee37De042AD2ffF1A253c3ce9bc27" // no "0x" prefix
	if !common.IsHexAddress(address) {
		t.Fatalf("bad test address")
	}

	var otherAddress common.Address
	if _, err := rand.Read(otherAddress[:]); err != nil {
		t.Fatalf("rand.Read error: %v", err)
	}

	eth := &baseWallet{
		addr:          common.HexToAddress(address),
		finalizeConfs: txConfsNeededToConfirm,
	}

	tests := []struct {
		name     string
		address  string
		wantOwns bool
		wantErr  bool
	}{
		{
			name:     "same (exact)",
			address:  address,
			wantOwns: true,
			wantErr:  false,
		},
		{
			name:     "same (lower)",
			address:  strings.ToLower(address),
			wantOwns: true,
			wantErr:  false,
		},
		{
			name:     "same (upper)",
			address:  strings.ToUpper(address),
			wantOwns: true,
			wantErr:  false,
		},
		{
			name:     "same (0x prefix)",
			address:  "0x" + address,
			wantOwns: true,
			wantErr:  false,
		},
		{
			name:     "different (valid canonical)",
			address:  otherAddress.String(),
			wantOwns: false,
			wantErr:  false,
		},
		{
			name:     "different (valid hex)",
			address:  otherAddress.Hex(),
			wantOwns: false,
			wantErr:  false,
		},
		{
			name:     "error (bad hex char)",
			address:  strings.Replace(address, "b", "r", 1),
			wantOwns: false,
			wantErr:  true,
		},
		{
			name:     "error (bad length)",
			address:  "ababababababab",
			wantOwns: false,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owns, err := eth.OwnsDepositAddress(tt.address)
			if (err == nil) && tt.wantErr {
				t.Error("expected error")
			}
			if (err != nil) && !tt.wantErr {
				t.Errorf("unexpected error: %v", err)
			}
			if owns != tt.wantOwns {
				t.Errorf("got %v, want %v", owns, tt.wantOwns)
			}
		})
	}
}

func TestDriverOpen(t *testing.T) {
	t.Parallel()
	drv := &Driver{}
	logger := util.StdOutLogger("ETHTEST", util.LevelOff)
	tmpDir := t.TempDir()

	settings := map[string]string{providersKey: "a.ipc"}
	err := CreateEVMWallet(ChainIDs[util.Testnet], &asset.CreateWalletParams{
		Type:     walletTypeRPC,
		Seed:     encode.RandomBytes(32),
		Pass:     encode.RandomBytes(32),
		Settings: settings,
		DataDir:  tmpDir,
		Net:      util.Testnet,
		Logger:   logger,
	}, &testnetCompatibilityData, true)
	if err != nil {
		t.Fatalf("CreateWallet error: %v", err)
	}

	// Make sure default gas fee limit is used when nothing is set
	cfg := &asset.WalletConfig{
		Type:     walletTypeRPC,
		Settings: settings,
		DataDir:  tmpDir,
	}
	wallet, err := drv.Open(cfg, logger, util.Testnet)
	if err != nil {
		t.Fatalf("driver open error: %v", err)
	}
	eth, ok := wallet.(*ETHWallet)
	if !ok {
		t.Fatalf("failed to cast wallet as ETHWallet")
	}
	if eth.GasFeeLimit() != defaultGasFeeLimit {
		t.Fatalf("expected GasFeeLimit to be default, but got %v", eth.GasFeeLimit())
	}

	// Make sure gas fee limit is properly parsed from settings
	cfg.Settings["gasfeelimit"] = "150"
	wallet, err = drv.Open(cfg, logger, util.Testnet)
	if err != nil {
		t.Fatalf("driver open error: %v", err)
	}
	eth, ok = wallet.(*ETHWallet)
	if !ok {
		t.Fatalf("failed to cast wallet as ETHWallet")
	}
	if eth.GasFeeLimit() != 150 {
		t.Fatalf("expected GasFeeLimit to be 150, but got %v", eth.GasFeeLimit())
	}
}

func TestDriverExists(t *testing.T) {
	t.Parallel()
	drv := &Driver{}
	tmpDir := t.TempDir()

	settings := map[string]string{providersKey: "a.ipc"}

	// no wallet
	exists, err := drv.Exists(walletTypeRPC, tmpDir, settings, util.Simnet)
	if err != nil {
		t.Fatalf("Exists error for no geth wallet: %v", err)
	}
	if exists {
		t.Fatalf("Uninitiated wallet exists")
	}

	// Create the wallet.
	err = CreateEVMWallet(ChainIDs[util.Simnet], &asset.CreateWalletParams{
		Type:     walletTypeRPC,
		Seed:     encode.RandomBytes(32),
		Pass:     encode.RandomBytes(32),
		Settings: settings,
		DataDir:  tmpDir,
		Net:      util.Simnet,
		Logger:   tLogger,
	}, &testnetCompatibilityData, true)
	if err != nil {
		t.Fatalf("CreateEVMWallet error: %v", err)
	}

	// exists
	exists, err = drv.Exists(walletTypeRPC, tmpDir, settings, util.Simnet)
	if err != nil {
		t.Fatalf("Exists error for existent geth wallet: %v", err)
	}
	if !exists {
		t.Fatalf("Initiated wallet doesn't exist")
	}

	// Wrong wallet type
	if _, err := drv.Exists("not-geth", tmpDir, settings, util.Simnet); err == nil {
		t.Fatalf("no error for unknown wallet type")
	}
}

func TestDriverDecodeCoinID(t *testing.T) {
	t.Parallel()
	drv := &Driver{}
	addressStr := "0xB6De8BB5ed28E6bE6d671975cad20C03931bE981"
	address := common.HexToAddress(addressStr)

	// Test tx hash
	txHash := encode.RandomBytes(common.HashLength)
	coinID, err := drv.DecodeCoinID(txHash)
	if err != nil {
		t.Fatalf("error decoding coin id: %v", err)
	}
	var hash common.Hash
	hash.SetBytes(txHash)
	if coinID != hash.String() {
		t.Fatalf("expected coin id to be %s but got %s", hash.String(), coinID)
	}

	// Test funding coin id
	fundingCoin := createFundingCoin(address, 1000)
	coinID, err = drv.DecodeCoinID(fundingCoin.RecoveryID())
	if err != nil {
		t.Fatalf("error decoding coin id: %v", err)
	}
	if coinID != fundingCoin.String() {
		t.Fatalf("expected coin id to be %s but got %s", fundingCoin.String(), coinID)
	}

	// Token funding coin
	fc := createTokenFundingCoin(address, 1000, 1)
	coinID, err = drv.DecodeCoinID(fc.RecoveryID())
	if err != nil {
		t.Fatalf("error decoding token coin id: %v", err)
	}
	if coinID != fc.String() {
		t.Fatalf("expected coin id to be %s but got %s", fc.String(), coinID)
	}

	// Test byte encoded address string
	coinID, err = drv.DecodeCoinID([]byte(addressStr))
	if err != nil {
		t.Fatalf("error decoding coin id: %v", err)
	}
	if coinID != addressStr {
		t.Fatalf("expected coin id to be %s but got %s", addressStr, coinID)
	}

	// Test invalid coin id
	_, err = drv.DecodeCoinID(encode.RandomBytes(20))
	if err == nil {
		t.Fatal("expected error but did not get")
	}
}
func TestReconfigure(t *testing.T) {
	t.Parallel()
	w, eth, _, shutdown := tassetWallet(BipID)
	defer shutdown()

	reconfigurer, is := w.(asset.LiveReconfigurer)
	if !is {
		t.Fatal("wallet is not a reconfigurer")
	}

	ethCfg := &WalletConfig{
		GasFeeLimit: 123,
	}

	settings, err := config.Mapify(ethCfg)
	if err != nil {
		t.Fatal("failed to mapify")
	}

	walletCfg := &asset.WalletConfig{
		Type:     walletTypeRPC,
		Settings: settings,
	}

	restart, err := reconfigurer.Reconfigure(context.Background(), walletCfg, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if restart {
		t.Fatalf("unexpected restart")
	}

	if eth.baseWallet.GasFeeLimit() != ethCfg.GasFeeLimit {
		t.Fatal("gas fee limit was not updated properly")
	}
}

func TestSend(t *testing.T) {
	t.Parallel()
	t.Run("eth", func(t *testing.T) { t.Parallel(); testSend(t, BipID) })
	t.Run("token", func(t *testing.T) { t.Parallel(); testSend(t, usdcEthID) })
	t.Run("link", func(t *testing.T) { t.Parallel(); testSend(t, linkEthID) })
}

func TestTokenSendEmitsParentBalanceChange(t *testing.T) {
	t.Parallel()

	w, eth, node, shutdown := tassetWallet(usdcEthID)
	defer shutdown()

	tokenWallet := w.(*TokenWallet)
	notes := make(chan asset.WalletNotification, 8)
	tokenWallet.emit = asset.NewWalletEmitter(notes, usdcEthID, tLogger)
	tokenWallet.parent.emit = asset.NewWalletEmitter(notes, BipID, tLogger)

	tx := tTx(0, 0, 0, &testAddressA, nil, 21000)
	node.sendTxTx = tx
	node.tokenContractor.transferTx = tx

	maxFeeRate, _, _ := eth.recommendedMaxFeeRate(eth.ctx)
	tokenFees := WeiToGwei(maxFeeRate) * tokenGasesV1.Transfer

	const val = 10e9
	node.tokenContractor.bal = GweiToWei(val)
	node.bal = GweiToWei(tokenFees)

	if _, err := w.Send("dd93b447f7eBCA361805eBe056259853F3912E04", val, 0); err != nil {
		t.Fatalf("unexpected send error: %v", err)
	}

	var seenToken, seenParent bool
	for !(seenToken && seenParent) {
		select {
		case note := <-notes:
			balNote, ok := note.(*asset.BalanceChangeNote)
			if !ok {
				continue
			}
			switch balNote.AssetID {
			case usdcEthID:
				seenToken = true
			case BipID:
				seenParent = true
			}
		case <-time.After(time.Second):
			t.Fatalf("expected token and parent balance updates, saw token=%v parent=%v", seenToken, seenParent)
		}
	}
}

func testSend(t *testing.T, assetID uint32) {
	w, eth, node, shutdown := tassetWallet(assetID)
	defer shutdown()

	tx := tTx(0, 0, 0, &testAddressA, nil, 21000)
	txHash := tx.Hash()

	node.sendTxTx = tx
	node.tokenContractor.transferTx = tx

	maxFeeRate, _, _ := eth.recommendedMaxFeeRate(eth.ctx)
	ethFees := WeiToGwei(maxFeeRate) * defaultSendGasLimit
	tokenFees := WeiToGwei(maxFeeRate) * tokenGasesV1.Transfer

	const val = 10e9
	const testAddr = "dd93b447f7eBCA361805eBe056259853F3912E04"
	tests := []struct {
		name              string
		sendAdj, feeAdj   uint64
		balErr, sendTxErr error
		addr              string
		wantErr           bool
	}{{
		name: "ok",
		addr: testAddr,
	}, {
		name:    "balance error",
		balErr:  errors.New("test error"),
		wantErr: true,
		addr:    testAddr,
	}, {
		name:    "not enough",
		sendAdj: 1,
		wantErr: true,
		addr:    testAddr,
	}, {
		name:    "low fees",
		feeAdj:  1,
		wantErr: true,
		addr:    testAddr,
	}, {
		name:      "sendToAddr error",
		sendTxErr: errors.New("test error"),
		wantErr:   true,
		addr:      testAddr,
	}, {
		name:      "Invalid address",
		sendTxErr: errors.New("invalid hex address error"),
		wantErr:   true,
		addr:      "",
	}}

	for _, test := range tests {
		node.setBalanceError(eth, test.balErr)
		node.sendTxErr = test.sendTxErr
		node.tokenContractor.transferErr = test.sendTxErr
		if assetID == BipID {
			node.bal = GweiToWei(val + ethFees - test.sendAdj - test.feeAdj)
		} else {
			node.tokenContractor.bal = GweiToWei(val - test.sendAdj)
			node.bal = GweiToWei(tokenFees - test.feeAdj)
		}
		coin, err := w.Send(test.addr, val, 0)
		if test.wantErr {
			if err == nil {
				t.Fatalf("expected error for test %q", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unexpected error for test %q: %v", test.name, err)
		}
		if !bytes.Equal(txHash[:], coin.ID()) {
			t.Fatal("coin is not the tx hash")
		}
	}
}

// Ensures that a small rise in the base fee between estimation
// and sending will not cause a failure.
func TestEstimateVsActualSendFees(t *testing.T) {
	t.Parallel()
	t.Run("eth", func(t *testing.T) { t.Parallel(); testEstimateVsActualSendFees(t, BipID) })
	t.Run("token", func(t *testing.T) { t.Parallel(); testEstimateVsActualSendFees(t, usdcEthID) })
}

func testEstimateVsActualSendFees(t *testing.T, assetID uint32) {
	w, _, node, shutdown := tassetWallet(assetID)
	defer shutdown()

	tx := tTx(0, 0, 0, &testAddressA, nil, 21000)
	node.sendTxTx = tx
	node.tokenContractor.transferTx = tx

	const testAddr = "dd93b447f7eBCA361805eBe056259853F3912E04"

	txFeeEstimator := w.(asset.TxFeeEstimator)
	fee, _, err := txFeeEstimator.EstimateSendTxFee("", 0, 0, false, false)
	if err != nil {
		t.Fatalf("error estimating fee: %v", err)
	}

	// Increase the base fee by 10%.
	node.baseFee = node.baseFee.Mul(node.baseFee, big.NewInt(11))
	node.baseFee = node.baseFee.Div(node.baseFee, big.NewInt(10))

	if assetID == BipID {
		node.bal = GweiToWei(11e9)
		canSend := new(big.Int).Sub(node.bal, GweiToWei(fee))
		canSendGwei, err := WeiToGweiSafe(canSend)
		if err != nil {
			t.Fatalf("error converting canSend to gwei: %v", err)
		}
		_, err = w.Send(testAddr, canSendGwei, 0)
		if err != nil {
			t.Fatalf("error sending: %v", err)
		}
	} else {
		tokenVal := uint64(10e9)
		node.tokenContractor.bal = GweiToWei(tokenVal)
		node.bal = GweiToWei(fee)
		_, err = w.Send(testAddr, tokenVal, 0)
		if err != nil {
			t.Fatalf("error sending: %v", err)
		}
	}
}

func TestEstimateSendTxFee(t *testing.T) {
	t.Parallel()
	t.Run("eth", func(t *testing.T) { t.Parallel(); testEstimateSendTxFee(t, BipID) })
	t.Run("token", func(t *testing.T) { t.Parallel(); testEstimateSendTxFee(t, usdcEthID) })
}

func testEstimateSendTxFee(t *testing.T, assetID uint32) {
	w, eth, node, shutdown := tassetWallet(assetID)
	defer shutdown()

	maxFeeRate, _, _ := eth.recommendedMaxFeeRate(eth.ctx)
	ethFees := WeiToGwei(maxFeeRate) * defaultSendGasLimit
	tokenFees := WeiToGwei(maxFeeRate) * tokenGasesV1.Transfer

	ethFees = ethFees * 12 / 10
	tokenFees = tokenFees * 12 / 10

	const testAddr = "dd93b447f7eBCA361805eBe056259853F3912E04"

	const val = 10e9
	tests := []struct {
		name, addr      string
		sendAdj, feeAdj uint64
		balErr          error
		withdraw        bool
		wantErr         bool
	}{{
		name: "ok",
		addr: testAddr,
	}, {
		name: "ok: empty address",
		addr: "",
	}, {
		name:    "not enough",
		sendAdj: 1,
		wantErr: true,
		addr:    testAddr,
	}, {
		name:    "low fees",
		feeAdj:  1,
		wantErr: true,
		addr:    testAddr,
	}, {
		name:     "subtract",
		feeAdj:   1,
		withdraw: true,
		wantErr:  true,
		addr:     testAddr,
	}, {
		name:    "balance error",
		balErr:  errors.New("test error"),
		wantErr: true,
		addr:    testAddr,
	}}

	for _, test := range tests {
		node.setBalanceError(eth, test.balErr)
		if assetID == BipID {
			node.bal = GweiToWei(val + ethFees - test.sendAdj - test.feeAdj)
		} else {
			node.tokenContractor.bal = GweiToWei(val - test.sendAdj)
			node.bal = GweiToWei(tokenFees - test.feeAdj)
		}
		txFeeEstimator := w.(asset.TxFeeEstimator)
		estimate, _, err := txFeeEstimator.EstimateSendTxFee(test.addr, val, 0, false, false)
		if test.wantErr {
			if err == nil {
				t.Fatalf("expected error for test %v", test.name)
			}
			continue
		}
		if assetID == BipID {
			if estimate != ethFees {
				t.Fatalf("%s: expected fees to be %v, got %v", test.name, ethFees, estimate)
			}
		} else {
			if estimate != tokenFees {
				t.Fatalf("%s: expected fees to be %v, got %v", test.name, tokenFees, estimate)
			}
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}
	}
}

func TestReceiptCache(t *testing.T) {
	t.Parallel()
	m := &multiRPCClient{
		finalizeConfs: 3,
	}
	c := make(map[common.Hash]*receiptRecord)
	m.receipts.cache = c

	r := &receiptRecord{
		r: &types.Receipt{
			Type: 50,
		},
		lastAccess: time.Now(),
	}

	var txHash common.Hash
	copy(txHash[:], encode.RandomBytes(32))
	c[txHash] = r

	if r := m.cachedReceipt(txHash); r == nil {
		t.Fatalf("cached receipt not returned")
	}

	r.lastAccess = time.Now().Add(-(unconfirmedReceiptExpiration + 1))
	if r := m.cachedReceipt(txHash); r != nil {
		t.Fatalf("expired receipt returned")
	}

	// The receipt still hasn't been pruned.
	if len(c) != 1 {
		t.Fatalf("receipt was pruned?")
	}

	// An if it was confirmed, it would be returned.
	r.confirmed = true
	if r := m.cachedReceipt(txHash); r == nil {
		t.Fatalf("confirmed receipt not returned")
	}

	r.lastAccess = time.Now().Add(-(receiptCacheExpiration + 1))
	m.receipts.lastClean = time.Time{}
	m.cachedReceipt(common.Hash{})
	// The receipt still hasn't been pruned.
	if len(c) != 0 {
		t.Fatalf("receipt wasn't pruned")
	}

}

func TestFreshProviderList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		times    []int64
		expOrder []int
	}{
		{
			times:    []int64{1, 2, 3},
			expOrder: []int{0, 1, 2},
		},
		{
			times:    []int64{3, 2, 1},
			expOrder: []int{2, 1, 0},
		},
		{
			times:    []int64{1, 5, 4, 2, 3},
			expOrder: []int{0, 3, 4, 2, 1},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("test#%d", i), func(t *testing.T) {
			t.Parallel()
			node := &multiRPCClient{
				finalizeConfs: 3,
				providers:     make([]*provider, len(tt.times)),
			}
			for i, stamp := range tt.times {
				p := &provider{}
				p.tip.headerStamp = time.Unix(stamp, 0)
				p.tip.failCount = i // hi-jacking field for initial sort order
				node.providers[i] = p
			}
			providers := node.freshnessSortedProviders()
			for i, p := range providers {
				if p.tip.failCount != tt.expOrder[i] {
					t.Fatalf("%d'th provider in sorted list is unexpected", i)
				}
			}
		})
	}
}

func TestDomain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr       string
		wantDomain string
		wantErr    bool
	}{
		{
			addr:       "http://www.place.io/v3/234234wfsefe",
			wantDomain: "place.io",
		},
		{
			addr:       "wss://www.place.nz/stuff?=token",
			wantDomain: "place.nz",
		},
		{
			addr:       "http://en.us.lotssubdomains.place.io/v3/234234wfsefe",
			wantDomain: "place.io",
		},
		{
			addr:       "https://www.place.co.uk:443/blog/article/search?docid=720&hl=en#dayone",
			wantDomain: "place.co.uk:443",
		},
		{
			addr:       "wmba://www.place.com",
			wantDomain: "place.com",
		},
		{
			addr:       "ws://127.0.0.1:3000",
			wantDomain: "127.0.0.1:3000",
		},
		{
			addr:       "ws://localhost:3000",
			wantDomain: "localhost:3000",
		},
		{
			addr:       "http://123.123.123.123:3000",
			wantDomain: "123.123.123.123:3000",
		},
		{
			addr:       "https://123.123.123.123",
			wantDomain: "123.123.123.123",
		},
		{
			addr:       "ws://[abab:fde3:0:0:0:0:0:1]:8080",
			wantDomain: "[abab:fde3:0:0:0:0:0:1]:8080",
		},
		{
			addr:       "ws://[::1]:8000",
			wantDomain: "[::1]:8000",
		},
		{
			addr:       "[::1]:8000",
			wantDomain: "[::1]:8000",
		},
		{
			addr:       "[::1]",
			wantDomain: "[::1]",
		},
		{
			addr:       "127.0.0.1",
			wantDomain: "127.0.0.1",
		},
		{
			addr:       "/home/john/.geth/geth.ipc",
			wantDomain: "/home/john/.geth/geth.ipc",
		},
		{
			addr:       "/home/john/.geth/geth",
			wantDomain: "/home/john/.geth/geth",
		},
		{
			addr:    "https://\n:1234",
			wantErr: true,
		},
		{
			addr:    ":asdf",
			wantErr: true,
		},
		{
			wantErr: true,
		},
	}

	for i, test := range tests {
		t.Run(fmt.Sprintf("test#%d", i), func(t *testing.T) {
			t.Parallel()
			d, err := domain(test.addr)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.wantDomain != d {
				t.Fatalf("wanted domain %s but got %s", test.wantDomain, d)
			}
		})
	}
}

// TestCheckPendingApprovalsCallback verifies that a confirmed approval's
// onConfirm callback runs before checkPendingApprovals returns, and runs without
// approvalsMtx held. Callbacks notify core that the approval landed, and the
// approval is deleted from pendingApprovals whether or not the callback runs, so
// a callback detached onto its own goroutine can be dropped at shutdown and is
// never retried. Keeping it on the caller's goroutine also keeps it inside
// whatever lifecycle the tip-processing pass has.
func TestCheckPendingApprovalsCallback(t *testing.T) {
	_, w, node, shutdown := tassetWallet(BipID)
	defer shutdown()

	txHash := common.BytesToHash([]byte{0x01})
	node.txConfirmations = map[common.Hash]uint32{txHash: 1}
	node.txConfsErr = map[common.Hash]error{}

	// The callback blocks until released, so checkPendingApprovals can only
	// return early if the callback was detached onto its own goroutine.
	started := make(chan struct{})
	release := make(chan struct{})
	var remainingDuringCallback int
	w.pendingApprovals[common.BytesToAddress([]byte{0x02})] = &pendingApproval{
		txHash: txHash,
		onConfirm: func() {
			// Taking approvalsMtx here deadlocks unless the collection loop has
			// already released it, and the confirmed approval must already be
			// removed by the time the callback runs.
			w.approvalsMtx.Lock()
			remainingDuringCallback = len(w.pendingApprovals)
			w.approvalsMtx.Unlock()
			close(started)
			<-release
		},
	}

	returned := make(chan struct{})
	go func() {
		w.checkPendingApprovals()
		close(returned)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("onConfirm was never called")
	}

	select {
	case <-returned:
		t.Fatal("checkPendingApprovals returned while onConfirm was still running: " +
			"the callback is detached and can outlive wallet shutdown")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("checkPendingApprovals did not return after onConfirm completed")
	}

	if remainingDuringCallback != 0 {
		t.Fatalf("confirmed approval not removed before onConfirm ran: %d remaining",
			remainingDuringCallback)
	}
}
