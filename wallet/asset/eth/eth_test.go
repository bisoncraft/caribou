//go:build !harness && !rpclive

// These tests will not be run if the harness build tag is set.

package eth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/config"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/broadcast"
	swapv0 "github.com/bisoncraft/bosswallet/wallet/asset/eth/contracts/v0"
	swapv1 "github.com/bisoncraft/bosswallet/wallet/asset/eth/contracts/v1"
	"github.com/davecgh/go-spew/spew"
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

var (
	_       ethFetcher = (*testNode)(nil)
	tLogger            = util.StdOutLogger("ETHTEST", util.LevelTrace)

	testAddressA = common.HexToAddress("dd93b447f7eBCA361805eBe056259853F3912E04")
	testAddressB = common.HexToAddress("8d83B207674bfd53B418a6E47DA148F5bFeCc652")
	testAddressC = common.HexToAddress("2b84C791b79Ee37De042AD2ffF1A253c3ce9bc27")

	ethGasesV0   = VersionedGases[0]
	tokenGasesV0 = Tokens[usdcEthID].NetTokens[util.Simnet].SwapContracts[0].Gas
	ethGasesV1   = VersionedGases[1]
	tokenGasesV1 = Tokens[usdcEthID].NetTokens[util.Simnet].SwapContracts[1].Gas

	tETHV0 = &util.Asset{
		Version:    0,
		ID:         60,
		Symbol:     "ETH",
		MaxFeeRate: 100,
		SwapConf:   1,
	}

	tETHV1 = &util.Asset{
		Version:    1,
		ID:         60,
		Symbol:     "ETH",
		MaxFeeRate: 100,
		SwapConf:   1,
	}

	tBTC = &util.Asset{
		ID:         0,
		Symbol:     "btc",
		Version:    0, // match btc.version
		MaxFeeRate: 10,
		SwapConf:   1,
	}

	tTokenV0 = &util.Asset{
		ID:         usdcEthID,
		Symbol:     "usdc.eth",
		Version:    0,
		MaxFeeRate: 20,
		SwapConf:   1,
	}

	tTokenV1 = &util.Asset{
		ID:         usdcEthID,
		Symbol:     "dextt.eth",
		Version:    1,
		MaxFeeRate: 20,
		SwapConf:   1,
	}

	signer = types.LatestSigner(params.AllEthashProtocolChanges)

	// simBackend = backends.NewSimulatedBackend(core.GenesisAlloc{
	// 	testAddressA: core.GenesisAccount{Balance: GweiToWei(5e10)},
	// }, 1e9)
)

type tGetTxRes struct {
	tx     *types.Transaction
	height int64
}

type testNode struct {
	acct                   *accounts.Account
	addr                   common.Address
	connectErr             error
	bestHdr                *types.Header
	bestHdrErr             error
	syncProg               ethereum.SyncProgress
	syncProgT              uint64
	syncProgErr            error
	bal                    *big.Int
	balErr                 error
	signDataErr            error
	privKey                *ecdsa.PrivateKey
	swapVers               map[uint32]struct{} // For SwapConfirmations -> swap. TODO for other contractor methods
	refundable             bool
	baseFee                *big.Int
	tip                    *big.Int
	netFeeStateErr         error
	confNonce              uint64
	confNonceErr           error
	getTxRes               *types.Transaction
	getTxResMap            map[common.Hash]*tGetTxRes
	getTxHeight            int64
	getTxErr               error
	receipt                *types.Receipt
	receiptTx              *types.Transaction
	receiptErr             error
	receipts               map[common.Hash]*types.Receipt
	receiptTxs             map[common.Hash]*types.Transaction
	receiptErrs            map[common.Hash]error
	hdrByHash              map[common.Hash]*types.Header
	lastSignedTx           *types.Transaction
	sentTxs                int
	sendTxTx               *types.Transaction
	sendTxErr              error
	lastSendTo             *common.Address
	lastSendData           []byte
	estimateGasResult      uint64
	estimateGasErr         error
	simBackend             bind.ContractBackend
	maxFeeRate             *big.Int
	tContractor            *tContractor
	tokenContractor        *tTokenContractor
	signedRedeemContractor *tSignedRedeemContractor
	contractor             contractor
	tokenParent            *assetWallet // only set for tokens
	txConfirmations        map[common.Hash]uint32
	txConfsErr             map[common.Hash]error
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
	if n.receiptErr != nil {
		return nil, nil, n.receiptErr
	}
	if n.receipt != nil {
		return n.receipt, n.receiptTx, nil
	}

	return n.receipts[txHash], n.receiptTxs[txHash], n.receiptErrs[txHash]
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

type tContractor struct {
	gasEstimates      *Gases
	swapMap           map[[32]byte]*SwapState
	swapErr           error
	initTx            *types.Transaction
	initErr           error
	redeemTx          *types.Transaction
	redeemErr         error
	refundTx          *types.Transaction
	refundErr         error
	initGasErr        error
	redeemGasErr      error
	refundGasErr      error
	redeemGasOverride *uint64
	redeemable        bool
	redeemableErr     error
	redeemableMap     map[string]bool
	valueIn           map[common.Hash]uint64
	valueOut          map[common.Hash]uint64
	valueErr          error
	refundable        bool
	refundableErr     error
	lastRedeemOpts    *bind.TransactOpts
	lastRedeems       []*asset.Redemption
	lastRefund        struct {
		// tx          *types.Transaction
		secretHash [32]byte
		maxFeeRate *big.Int
		// contractVer uint32
	}
}

func (c *tContractor) status(ctx context.Context, locator []byte) (*SwapStatus, error) {
	if c.swapErr != nil {
		return nil, c.swapErr
	}
	vector, err := c.vector(ctx, locator)
	if err != nil {
		return nil, err
	}
	swap, ok := c.swapMap[vector.SecretHash]
	if !ok {
		return nil, errors.New("swap not in map")
	}
	s := &SwapStatus{
		Step:        swap.State,
		Secret:      swap.Secret,
		BlockHeight: swap.BlockHeight,
	}
	return s, nil
}

func (c *tContractor) vector(ctx context.Context, locator []byte) (*SwapVector, error) {
	if c.swapErr != nil {
		return nil, c.swapErr
	}
	if len(locator) == LocatorV1Length {
		return ParseV1Locator(locator)
	}
	var secretHash [32]byte
	copy(secretHash[:], locator)
	swap, ok := c.swapMap[secretHash]
	if !ok {
		return nil, errors.New("swap not in map")
	}
	v := &SwapVector{
		From:       swap.Initiator,
		To:         swap.Participant,
		Value:      swap.Value,
		SecretHash: secretHash,
		LockTime:   uint64(swap.LockTime.Unix()),
	}
	return v, nil
}

func (c *tContractor) statusAndVector(ctx context.Context, locator []byte) (*SwapStatus, *SwapVector, error) {
	if c.swapErr != nil {
		return nil, nil, c.swapErr
	}
	vector, err := c.vector(ctx, locator)
	if err != nil {
		return nil, nil, err
	}
	swap, ok := c.swapMap[vector.SecretHash]
	if !ok {
		return nil, nil, errors.New("swap not in map")
	}
	v := &SwapVector{
		From:       swap.Initiator,
		To:         swap.Participant,
		Value:      swap.Value,
		SecretHash: vector.SecretHash,
		LockTime:   uint64(swap.LockTime.Unix()),
	}
	s := &SwapStatus{
		Step:        swap.State,
		Secret:      swap.Secret,
		BlockHeight: swap.BlockHeight,
	}
	return s, v, nil
}

func (c *tContractor) swap(ctx context.Context, secretHash [32]byte) (*SwapState, error) {
	if c.swapErr != nil {
		return nil, c.swapErr
	}
	swap, ok := c.swapMap[secretHash]
	if !ok {
		return nil, errors.New("swap not in map")
	}
	return swap, nil
}

func (c *tContractor) initiate(*bind.TransactOpts, []*asset.Contract) (*types.Transaction, error) {
	return c.initTx, c.initErr
}

func (c *tContractor) redeem(txOpts *bind.TransactOpts, redeems []*asset.Redemption) (*types.Transaction, error) {
	c.lastRedeemOpts = txOpts
	c.lastRedeems = redeems
	return c.redeemTx, c.redeemErr
}

func (c *tContractor) refund(opts *bind.TransactOpts, locator []byte) (*types.Transaction, error) {
	vector, err := c.vector(context.Background(), locator)
	if err != nil {
		return nil, err
	}
	c.lastRefund.secretHash = vector.SecretHash
	c.lastRefund.maxFeeRate = opts.GasFeeCap
	return c.refundTx, c.refundErr
}

func (c *tContractor) estimateInitGas(ctx context.Context, n int) (uint64, error) {
	return c.gasEstimates.SwapN(n), c.initGasErr
}

func (c *tContractor) estimateRedeemGas(ctx context.Context, secrets [][32]byte, locators [][]byte) (uint64, error) {
	if c.redeemGasOverride != nil {
		return *c.redeemGasOverride, nil
	}
	return c.gasEstimates.RedeemN(len(secrets)), c.redeemGasErr
}

func (c *tContractor) estimateRefundGas(ctx context.Context, locator []byte) (uint64, error) {
	return c.gasEstimates.Refund, c.refundGasErr
}

func (c *tContractor) packInitiateData(n int) ([]byte, error) {
	return make([]byte, 100*n), nil // representative-sized calldata
}

func (c *tContractor) packRedeemData(n int) ([]byte, error) {
	return make([]byte, 100*n), nil
}

func (c *tContractor) packRefundData() ([]byte, error) {
	return make([]byte, 100), nil
}

func (c *tContractor) isRedeemable(locator []byte, secret [32]byte) (bool, error) {
	if c.redeemableErr != nil {
		return false, c.redeemableErr
	}

	vector, err := c.vector(context.Background(), locator)
	if err != nil {
		return false, err
	}

	if c.swapMap != nil && c.swapMap[vector.SecretHash] == nil {
		return false, fmt.Errorf("test error: no swap in swap map")
	}

	if c.redeemableMap != nil {
		return c.redeemableMap[string(locator)], nil
	}

	return c.redeemable, c.redeemableErr
}

func (c *tContractor) value(_ context.Context, tx *types.Transaction) (incoming, outgoing uint64, err error) {
	incoming, outgoing = c.valueIn[tx.Hash()], c.valueOut[tx.Hash()]
	if incoming > 0 {
		delete(c.valueIn, tx.Hash())
	}
	if outgoing > 0 {
		delete(c.valueOut, tx.Hash())
	}
	return incoming, outgoing, c.valueErr
}

func (c *tContractor) isRefundable(locator []byte) (bool, error) {
	return c.refundable, c.refundableErr
}

func (c *tContractor) voidUnusedNonce() {}

type tSignedRedeemContractor struct {
	*tContractor
	digest      common.Hash
	digestErr   error
	calldata    []byte
	calldataErr error
	nonce       *big.Int
	nonceErr    error
	redemptions []swapv1.ETHSwapRedemption
	convertErr  error
	tokenAddr   common.Address
	lastNonce   *big.Int // nonce passed to redeemWithSignatureCalldata
}

var _ contractor = (*tSignedRedeemContractor)(nil)
var _ signedRedeemContractor = (*tSignedRedeemContractor)(nil)

func (c *tSignedRedeemContractor) eip712RedeemDigest(_ common.Address, _ []swapv1.ETHSwapRedemption,
	_, _, _ *big.Int) (common.Hash, error) {
	return c.digest, c.digestErr
}
func (c *tSignedRedeemContractor) redeemWithSignatureCalldata(_ []swapv1.ETHSwapRedemption,
	_ common.Address, _ /* relayerFee */, nonce, _ *big.Int, _ []byte) ([]byte, error) {
	c.lastNonce = nonce
	return c.calldata, c.calldataErr
}
func (c *tSignedRedeemContractor) nonces(_ common.Address) (*big.Int, error) {
	return c.nonce, c.nonceErr
}
func (c *tSignedRedeemContractor) convertRedeems(redeems []*asset.Redemption) ([]swapv1.ETHSwapRedemption, error) {
	return c.redemptions, c.convertErr
}
func (c *tSignedRedeemContractor) eip712TokenAddr() common.Address {
	return c.tokenAddr
}

type tTokenContractor struct {
	*tContractor
	tokenAddr           common.Address
	bal                 *big.Int
	balErr              error
	allow               *big.Int
	allowErr            error
	approveTx           *types.Transaction
	approved            bool
	approveErr          error
	approveEstimate     uint64
	approveEstimateErr  error
	transferTx          *types.Transaction
	transferErr         error
	transferEstimate    uint64
	transferEstimateErr error
}

var _ tokenContractor = (*tTokenContractor)(nil)

func (c *tTokenContractor) tokenAddress() common.Address {
	return c.tokenAddr
}

func (c *tTokenContractor) balance(context.Context) (*big.Int, error) {
	return c.bal, c.balErr
}

func (c *tTokenContractor) allowance(context.Context) (*big.Int, error) {
	return c.allow, c.allowErr
}

func (c *tTokenContractor) approve(*bind.TransactOpts, *big.Int) (*types.Transaction, error) {
	if c.approveErr == nil {
		c.approved = true
	}
	return c.approveTx, c.approveErr
}

func (c *tTokenContractor) estimateApproveGas(context.Context, *big.Int) (uint64, error) {
	return c.approveEstimate, c.approveEstimateErr
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

type tTxDB struct {
	storeTxCalled  bool
	storedTx       *extendedWalletTx
	storeTxErr     error
	removeTxCalled bool
	removeTxErr    error
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
func (db *tTxDB) removeTx(_ /* id */ string) error {
	db.removeTxCalled = true
	return db.removeTxErr
}
func (db *tTxDB) getTxs(tokenID *uint32, req *asset.TxHistoryRequest) (*asset.TxHistoryResponse, error) {
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
func (db *tTxDB) close() error {
	return nil
}
func (db *tTxDB) getBridges(tokenID *uint32, n int, refID *common.Hash, past bool) ([]*asset.WalletTransaction, error) {
	panic("getBridges not implemented")
}
func (db *tTxDB) getPendingBridges(tokenID *uint32) ([]*extendedWalletTx, error) {
	return db.pendingBridges, nil
}
func (db *tTxDB) getBridgeCompletions(initiationTxID string) ([]*extendedWalletTx, error) {
	return []*extendedWalletTx{db.txToGet}, db.getTxErr
}

// func TestCheckUnconfirmedTxs(t *testing.T) {
// 	const tipHeight = 50
// 	const baseFeeGwei = 100
// 	const gasTipCapGwei = 2

// 	type tExtendedWalletTx struct {
// 		wt           *extendedWalletTx
// 		confs        uint32
// 		gasUsed      uint64
// 		txReceiptErr error
// 	}

// 	newExtendedWalletTx := func(assetID uint32, nonce int64, maxFees uint64, currBlockNumber uint64, txReceiptConfs uint32,
// 		txReceiptGasUsed uint64, txReceiptErr error, timeStamp int64, savedToDB bool) *tExtendedWalletTx {
// 		var tokenID *uint32
// 		if assetID != BipID {
// 			tokenID = &assetID
// 		}

// 		return &tExtendedWalletTx{
// 			wt: &extendedWalletTx{
// 				WalletTransaction: &asset.WalletTransaction{
// 					BlockNumber: currBlockNumber,
// 					TokenID:     tokenID,
// 					Fees:        maxFees,
// 				},
// 				SubmissionTime: uint64(timeStamp),
// 				nonce:          big.NewInt(nonce),
// 				savedToDB:      savedToDB,
// 			},
// 			confs:        txReceiptConfs,
// 			gasUsed:      txReceiptGasUsed,
// 			txReceiptErr: txReceiptErr,
// 		}
// 	}

// 	gasFee := func(gasUsed uint64) uint64 {
// 		return gasUsed * (baseFeeGwei + gasTipCapGwei)
// 	}

// 	now := time.Now().Unix()

// 	tests := []struct {
// 		name           string
// 		assetID        uint32
// 		unconfirmedTxs []*tExtendedWalletTx
// 		confirmedNonce uint64

// 		expTxsAfter       []*extendedWalletTx
// 		expStoreTxCalled  bool
// 		expRemoveTxCalled bool
// 		storeTxErr        error
// 		removeTxErr       error
// 	}{
// 		{
// 			name:    "coin not found",
// 			assetID: BipID,
// 			unconfirmedTxs: []*tExtendedWalletTx{
// 				newExtendedWalletTx(BipID, 5, 1e7, 0, 0, 0, asset.CoinNotFoundError, now, true),
// 			},
// 			expTxsAfter: []*extendedWalletTx{
// 				newExtendedWalletTx(BipID, 5, 1e7, 0, 0, 0, asset.CoinNotFoundError, now, true).wt,
// 			},
// 		},
// 		{
// 			name:    "still in mempool",
// 			assetID: BipID,
// 			unconfirmedTxs: []*tExtendedWalletTx{
// 				newExtendedWalletTx(BipID, 5, 1e7, 0, 0, 0, nil, now, true),
// 			},
// 			expTxsAfter: []*extendedWalletTx{
// 				newExtendedWalletTx(BipID, 5, 1e7, 0, 0, 0, nil, now, true).wt,
// 			},
// 		},
// 		{
// 			name:    "1 confirmation",
// 			assetID: BipID,
// 			unconfirmedTxs: []*tExtendedWalletTx{
// 				newExtendedWalletTx(BipID, 5, 1e7, 0, 1, 6e5, nil, now, true),
// 			},
// 			expTxsAfter: []*extendedWalletTx{
// 				newExtendedWalletTx(BipID, 5, gasFee(6e5), tipHeight, 0, 0, nil, now, true).wt,
// 			},
// 			expStoreTxCalled: true,
// 		},
// 		{
// 			name:    "3 confirmations",
// 			assetID: BipID,
// 			unconfirmedTxs: []*tExtendedWalletTx{
// 				newExtendedWalletTx(BipID, 1, gasFee(6e5), tipHeight, 3, 6e5, nil, now, true),
// 			},
// 			expTxsAfter:      []*extendedWalletTx{},
// 			expStoreTxCalled: true,
// 		},
// 		{
// 			name:    "3 confirmations, leave in unconfirmed txs if txDB.storeTx fails",
// 			assetID: BipID,
// 			unconfirmedTxs: []*tExtendedWalletTx{
// 				newExtendedWalletTx(BipID, 5, gasFee(6e5), tipHeight-2, 3, 6e5, nil, now, true),
// 			},
// 			expTxsAfter: []*extendedWalletTx{
// 				newExtendedWalletTx(BipID, 5, gasFee(6e5), tipHeight-2, 3, 6e5, nil, now, true).wt,
// 			},
// 			expStoreTxCalled: true,
// 			storeTxErr:       errors.New("test error"),
// 		},
// 		{
// 			name:    "was confirmed but now not found",
// 			assetID: BipID,
// 			unconfirmedTxs: []*tExtendedWalletTx{
// 				newExtendedWalletTx(BipID, 5, 1e7, tipHeight-1, 0, 0, asset.CoinNotFoundError, now, true),
// 			},
// 			expTxsAfter: []*extendedWalletTx{
// 				newExtendedWalletTx(BipID, 5, 1e7, 0, 0, 0, asset.CoinNotFoundError, now, true).wt,
// 			},
// 			expStoreTxCalled: true,
// 		},
// 	}

// 	for _, tt := range tests {
// 		t.Run(tt.name, func(t *testing.T) {
// 			_, eth, node, shutdown := tassetWallet(tt.assetID)
// 			defer shutdown()

// 			node.tokenContractor.bal = unlimitedAllowance
// 			node.receipts = make(map[common.Hash]*types.Receipt)
// 			node.receiptTxs = make(map[common.Hash]*types.Transaction)
// 			node.receiptErrs = make(map[common.Hash]error)
// 			node.hdrByHash = &types.Header{
// 				BaseFee: GweiToWei(baseFeeGwei),
// 			}
// 			node.confNonce = tt.confirmedNonce
// 			eth.connected.Store(true)
// 			eth.tipMtx.Lock()
// 			eth.currentTip = &types.Header{Number: new(big.Int).SetUint64(tipHeight)}
// 			eth.tipMtx.Unlock()

// 			txDB := &tTxDB{
// 				storeTxErr:  tt.storeTxErr,
// 				removeTxErr: tt.removeTxErr,
// 			}
// 			eth.txDB = txDB

// 			for _, pt := range tt.unconfirmedTxs {
// 				txHash := common.BytesToHash(encode.RandomBytes(32))
// 				pt.wt.ID = txHash.String()
// 				pt.wt.txHash = txHash
// 				eth.pendingTxs = append(eth.pendingTxs, pt.wt)
// 				var blockNumber *big.Int
// 				if pt.confs > 0 {
// 					blockNumber = big.NewInt(int64(tipHeight - pt.confs + 1))
// 				}
// 				node.receipts[txHash] = &types.Receipt{BlockNumber: blockNumber, GasUsed: pt.gasUsed}

// 				node.receiptTxs[txHash] = types.NewTx(&types.DynamicFeeTx{
// 					GasTipCap: GweiToWei(gasTipCapGwei),
// 					GasFeeCap: GweiToWei(2 * baseFeeGwei),
// 				})
// 				node.receiptErrs[txHash] = pt.txReceiptErr
// 			}

// 			eth.checkPendingTxs()

// 			if len(eth.pendingTxs) != len(tt.expTxsAfter) {
// 				t.Fatalf("expected %d unconfirmed txs, got %d", len(tt.expTxsAfter), len(eth.pendingTxs))
// 			}
// 			for i, expTx := range tt.expTxsAfter {
// 				tx := eth.pendingTxs[i]
// 				if expTx.nonce.Cmp(tx.nonce) != 0 {
// 					t.Fatalf("expected tx index %d to have nonce %d, not %d", i, expTx.nonce, tx.nonce)
// 				}
// 			}

// 			if txDB.storeTxCalled != tt.expStoreTxCalled {
// 				t.Fatalf("expected storeTx called %v, got %v", tt.expStoreTxCalled, txDB.storeTxCalled)
// 			}
// 			if txDB.removeTxCalled != tt.expRemoveTxCalled {
// 				t.Fatalf("expected removeTx called %v, got %v", tt.expRemoveTxCalled, txDB.removeTxCalled)
// 			}
// 		})
// 	}
// }

type tRelayer struct {
	taskID        string
	submitErr     error
	feeEstimate   *feeEstimate
	estimateErr   error
	healthErr     error
	statusResults map[string]*relayStatus
	statusErrs    map[string]error
	submittedReqs []*relayRequest
}

var _ relayer = (*tRelayer)(nil)

func (r *tRelayer) estimateFee(_ context.Context, _ int, _ common.Address) (*feeEstimate, error) {
	return r.feeEstimate, r.estimateErr
}

func (r *tRelayer) checkHealth(_ context.Context) error {
	return r.healthErr
}

func (r *tRelayer) submitSignedRedeem(_ context.Context, req *relayRequest) (string, error) {
	r.submittedReqs = append(r.submittedReqs, req)
	return r.taskID, r.submitErr
}

func (r *tRelayer) getRelayStatus(_ context.Context, taskID string) (*relayStatus, error) {
	var err error
	if r.statusErrs != nil {
		err = r.statusErrs[taskID]
	}
	return r.statusResults[taskID], err
}

func newTRelayer() *tRelayer {
	return &tRelayer{
		statusResults: make(map[string]*relayStatus),
		statusErrs:    make(map[string]error),
		submittedReqs: make([]*relayRequest, 0),
	}
}

func TestCheckPendingRelays(t *testing.T) {
	type test struct {
		name                string
		pendingRelaysBefore map[common.Hash]*extendedWalletTx
		pendingRelaysAfter  map[common.Hash]*extendedWalletTx
		statusResults       map[string]*relayStatus
		statusErrs          map[string]error
		receipts            map[common.Hash]*types.Receipt
		receiptErrs         map[common.Hash]error
		hdrByHash           map[common.Hash]*types.Header
	}

	relayTaskHashes := make([]common.Hash, 5)
	txHashes := make([]common.Hash, 5)
	blockHashes := make([]common.Hash, 5)
	relayTaskIDs := make([]string, 5)
	for i := range relayTaskHashes {
		relayTaskHashes[i] = common.BytesToHash(encode.RandomBytes(32))
		txHashes[i] = common.BytesToHash(encode.RandomBytes(32))
		blockHashes[i] = common.BytesToHash(encode.RandomBytes(32))
		relayTaskIDs[i] = fmt.Sprintf("relay-task-%d", i)
	}

	tipHeight := uint64(100)

	tests := []test{
		{
			name:                "no relays",
			pendingRelaysBefore: map[common.Hash]*extendedWalletTx{},
			pendingRelaysAfter:  map[common.Hash]*extendedWalletTx{},
		},
		{
			name: "relay submitted, pending - still waiting",
			pendingRelaysBefore: map[common.Hash]*extendedWalletTx{
				relayTaskHashes[0]: {
					WalletTransaction: &asset.WalletTransaction{
						ID: relayTaskHashes[0].String(),
					},
					RelayTaskID: relayTaskIDs[0],
				},
			},
			pendingRelaysAfter: map[common.Hash]*extendedWalletTx{
				relayTaskHashes[0]: {
					WalletTransaction: &asset.WalletTransaction{
						ID: relayTaskHashes[0].String(),
					},
					RelayTaskID: relayTaskIDs[0],
					savedToDB:   true,
				},
			},
			statusResults: map[string]*relayStatus{
				relayTaskIDs[0]: {State: "pending"},
			},
		},
		{
			name: "relay submitted, success with tx hash",
			pendingRelaysBefore: map[common.Hash]*extendedWalletTx{
				relayTaskHashes[0]: {
					WalletTransaction: &asset.WalletTransaction{
						ID: relayTaskHashes[0].String(),
					},
					RelayTaskID: relayTaskIDs[0],
				},
			},
			pendingRelaysAfter: map[common.Hash]*extendedWalletTx{
				relayTaskHashes[0]: {
					WalletTransaction: &asset.WalletTransaction{
						ID:        relayTaskHashes[0].String(),
						RelayTxID: txHashes[0].Hex(),
					},
					RelayTaskID: relayTaskIDs[0],
					savedToDB:   true,
				},
			},
			statusResults: map[string]*relayStatus{
				relayTaskIDs[0]: {State: "success", TxHash: txHashes[0]},
			},
		},
		{
			name: "relay submitted, failed",
			pendingRelaysBefore: map[common.Hash]*extendedWalletTx{
				relayTaskHashes[0]: {
					WalletTransaction: &asset.WalletTransaction{
						ID: relayTaskHashes[0].String(),
					},
					RelayTaskID: relayTaskIDs[0],
				},
			},
			pendingRelaysAfter: map[common.Hash]*extendedWalletTx{},
			statusResults: map[string]*relayStatus{
				relayTaskIDs[0]: {State: "failed", FailureReason: "dropped"},
			},
		},
		{
			name: "already has tx hash, receipt confirms",
			pendingRelaysBefore: map[common.Hash]*extendedWalletTx{
				relayTaskHashes[0]: {
					WalletTransaction: &asset.WalletTransaction{
						ID:        relayTaskHashes[0].String(),
						RelayTxID: txHashes[0].String(),
					},
				},
			},
			pendingRelaysAfter: map[common.Hash]*extendedWalletTx{
				relayTaskHashes[0]: {
					WalletTransaction: &asset.WalletTransaction{
						ID:          relayTaskHashes[0].String(),
						RelayTxID:   txHashes[0].String(),
						BlockNumber: tipHeight - 1,
						Timestamp:   12345,
					},
					savedToDB: true,
				},
			},
			receipts: map[common.Hash]*types.Receipt{
				txHashes[0]: {
					BlockNumber: big.NewInt(int64(tipHeight - 1)),
					BlockHash:   blockHashes[0],
					TxHash:      txHashes[0],
					Status:      1,
				},
			},
			hdrByHash: map[common.Hash]*types.Header{
				blockHashes[0]: {
					Number: big.NewInt(int64(tipHeight - 1)),
					Time:   12345,
				},
			},
		},
		{
			name: "confirmed and finalized, removed from pending",
			pendingRelaysBefore: map[common.Hash]*extendedWalletTx{
				relayTaskHashes[0]: {
					WalletTransaction: &asset.WalletTransaction{
						ID:          relayTaskHashes[0].String(),
						RelayTxID:   txHashes[0].String(),
						BlockNumber: tipHeight - txConfsNeededToConfirm + 1,
						Confirmed:   true,
					},
				},
			},
			pendingRelaysAfter: map[common.Hash]*extendedWalletTx{},
			receipts: map[common.Hash]*types.Receipt{
				txHashes[0]: {
					BlockNumber: big.NewInt(int64(tipHeight - txConfsNeededToConfirm + 1)),
					BlockHash:   blockHashes[0],
					TxHash:      txHashes[0],
					Status:      1,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, eth, node, shutdown := tassetWallet(BipID)
			defer shutdown()

			tr := newTRelayer()
			if tt.statusResults != nil {
				tr.statusResults = tt.statusResults
			}
			if tt.statusErrs != nil {
				tr.statusErrs = tt.statusErrs
			}
			eth.relayer = tr

			eth.pendingRelays = tt.pendingRelaysBefore
			eth.currentTip = &types.Header{Number: new(big.Int).SetUint64(tipHeight)}
			if tt.hdrByHash != nil {
				node.hdrByHash = tt.hdrByHash
			}
			if tt.receipts != nil {
				node.receipts = tt.receipts
			}
			if tt.receiptErrs != nil {
				node.receiptErrs = tt.receiptErrs
			}

			eth.checkPendingRelays()

			if len(eth.pendingRelays) != len(tt.pendingRelaysAfter) {
				t.Fatalf("expected %d pending relays, got %d", len(tt.pendingRelaysAfter), len(eth.pendingRelays))
			}
			for hash, exp := range tt.pendingRelaysAfter {
				tx := eth.pendingRelays[hash]
				if tx == nil {
					t.Fatalf("expected pending relay %s, but not found", hash)
				}
				if !reflect.DeepEqual(tx.WalletTransaction, exp.WalletTransaction) {
					t.Fatalf("expected tx \n%+v\n\ngot\n%+v", spew.Sdump(exp.WalletTransaction), spew.Sdump(tx.WalletTransaction))
				}
				if tx.RelayTaskID != exp.RelayTaskID {
					t.Fatalf("expected RelayTaskID %q, got %q", exp.RelayTaskID, tx.RelayTaskID)
				}
			}
		})
	}
}

func TestCheckPendingTxs(t *testing.T) {
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
			txType: asset.Send,
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

func TestTakeAction(t *testing.T) {
	_, eth, node, shutdown := tassetWallet(BipID)
	defer shutdown()

	aGwei := GweiToWei(1)
	pendingTx := eth.extendedTx(&genTxResult{
		tx:     node.newTransaction(0, aGwei),
		txType: asset.Send,
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

	tooCheapAction := []byte(fmt.Sprintf(`{"txID":"%s","bump":true}`, pendingTx.ID))
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
		txType: asset.Send,
		amt:    1,
	})
	eth.pendingTxs = []*extendedWalletTx{pendingTx}
	pendingTx.SubmissionTime = 0
	// Neglecting to bump should reset submission time.
	tooCheapAction = []byte(fmt.Sprintf(`{"txID":"%s","bump":false}`, pendingTx.ID))
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
	lostNonceAction := []byte(fmt.Sprintf(`{"txID":"%s","abandon":true}`, pendingTx.ID))
	if err := eth.TakeAction(actionTypeLostNonce, lostNonceAction); err != nil {
		t.Fatalf("TakeAction replacment=false, abandon=true error: %v", err)
	}
	if len(eth.pendingTxs) != 0 {
		t.Fatalf("Tx wasn't abandoned")
	}
	eth.pendingTxs = []*extendedWalletTx{pendingTx}
	node.getTxRes = replacementTx
	lostNonceAction = []byte(fmt.Sprintf(`{"txID":"%s","abandon":false,"replacementID":"%s"}`, pendingTx.ID, replacementTx.Hash()))
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
		txType: asset.Send,
		amt:    1,
	})
	eth.pendingTxs = []*extendedWalletTx{pendingTx}
	lostNonceAction = []byte(fmt.Sprintf(`{"txID":"%s","abandon":false,"replacementID":"%s"}`, pendingTx.ID, replacementTx.Hash()))
	if err := eth.TakeAction(actionTypeLostNonce, lostNonceAction); err == nil {
		t.Fatalf("no error for wrong nonce")
	}

	// Missing nonces
	tx5 := eth.extendedTx(&genTxResult{
		tx:     node.newTransaction(5, aGwei),
		txType: asset.Send,
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

func TestCheckForNewBlocks(t *testing.T) {
	header0 := &types.Header{Number: new(big.Int)}
	header1 := &types.Header{Number: big.NewInt(1)}
	tests := []struct {
		name              string
		hashErr, blockErr error
		bestHeader        *types.Header
		hasTipChange      bool
	}{{
		name:         "ok",
		bestHeader:   header1,
		hasTipChange: true,
	}, {
		name:       "ok same hash",
		bestHeader: header0,
	}}

	for _, test := range tests {
		ctx, cancel := context.WithCancel(context.Background())
		node := &testNode{}
		notes := make(chan asset.WalletNotification, 1)
		emit := asset.NewWalletEmitter(notes, BipID, tLogger)

		node.bestHdr = test.bestHeader
		node.bestHdrErr = test.blockErr
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
				versionedContracts: map[uint32]common.Address{
					0: {},
				},
				log:     tLogger.SubLogger("ETH"),
				emit:    emit,
				assetID: BipID,
			},
		}
		w.wallets = map[uint32]*assetWallet{BipID: w.assetWallet}
		w.assetWallet.connected.Store(true)
		w.checkForNewBlocks(ctx)

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
			if w.currentTip.Number.Cmp(test.bestHeader.Number) != 0 {
				t.Fatalf("tip was changed. wasn't supposed to be changed")
			}
		}
		cancel()

	}
}

func TestSyncStatus(t *testing.T) {
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

	tc := &tContractor{
		gasEstimates: ethGasesV0,
		swapMap:      make(map[[32]byte]*SwapState),
		valueIn:      make(map[common.Hash]uint64),
		valueOut:     make(map[common.Hash]uint64),
	}

	signedRedeemContractor := &tSignedRedeemContractor{
		tContractor: tc,
		nonce:       big.NewInt(0),
		digest:      common.Hash{},
		calldata:    []byte{},
		redemptions: []swapv1.ETHSwapRedemption{},
	}

	var c contractor = signedRedeemContractor

	ttc := &tTokenContractor{
		tContractor: tc,
		allow:       new(big.Int),
	}
	if assetID != BipID {
		ttc.tContractor.gasEstimates = &tokenGasesV0
		c = ttc
	}

	return &tMempoolNode{
		testNode: &testNode{
			acct:                   acct,
			addr:                   acct.Address,
			maxFeeRate:             GweiToWei(100),
			baseFee:                GweiToWei(100),
			tip:                    GweiToWei(2),
			privKey:                privKey,
			contractor:             c,
			tContractor:            tc,
			signedRedeemContractor: signedRedeemContractor,
			tokenContractor:        ttc,
			txConfirmations:        make(map[common.Hash]uint32),
			txConfsErr:             make(map[common.Hash]error),
			receipts:               make(map[common.Hash]*types.Receipt),
			receiptErrs:            make(map[common.Hash]error),
			receiptTxs:             make(map[common.Hash]*types.Transaction),
			hdrByHash:              make(map[common.Hash]*types.Header),
		},
	}
}

func tassetWallet(assetID uint32) (asset.Wallet, *assetWallet, *tMempoolNode, context.CancelFunc) {
	node := newTestNode(assetID)
	ctx, cancel := context.WithCancel(context.Background())
	var c contractor = node.contractor
	if assetID != BipID {
		c = node.tokenContractor
	}

	versionedGases := make(map[uint32]*Gases)
	if assetID == BipID { // just make a copy
		maps.Copy(versionedGases, VersionedGases)
	} else {
		netToken := Tokens[assetID].NetTokens[util.Simnet]
		for ver, c := range netToken.SwapContracts {
			versionedGases[ver] = &c.Gas
		}
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
			pendingRelays:    make(map[common.Hash]*extendedWalletTx),
			txDB:             &tTxDB{},
			currentTip:       &types.Header{Number: new(big.Int), GasLimit: 30_000_000},
			finalizeConfs:    txConfsNeededToConfirm,
			maxTxFeeGwei:     GweiFactor, // 1 ETH
		},
		versionedGases:     versionedGases,
		log:                tLogger.SubLogger(strings.ToUpper(util.BipIDSymbol(assetID))),
		assetID:            assetID,
		contractorV0:       c,
		contractorV1:       c,
		findRedemptionReqs: make(map[string]*findRedemptionRequest),
		evmify:             GweiToWei,
		atomize:            WeiToGwei,
		pendingTxCheckBal:  new(big.Int),
		pendingApprovals:   make(map[common.Address]*pendingApproval),
		approvalCache:      make(map[common.Address]bool),
		swapSeen:           broadcast.NewCache[*ethSwapSeen](),
		redeemSeen:         broadcast.NewCache[*ethSwapSeen](),
		// move up after review
		wi:   WalletInfo,
		emit: asset.NewWalletEmitter(emitChan, BipID, tLogger),
		versionedContracts: map[uint32]common.Address{
			0: {},
		},
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
			baseWallet:       aw.baseWallet,
			log:              tLogger.SubLogger("ETH"),
			contractorV0:     node.tContractor,
			contractorV1:     node.tContractor,
			assetID:          BipID,
			atomize:          WeiToGwei,
			pendingApprovals: make(map[common.Address]*pendingApproval),
			approvalCache:    make(map[common.Address]bool),
			emit:             asset.NewWalletEmitter(emitChan, BipID, tLogger),
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
		name:         "ok pending in",
		bal:          newBalance(1e8, 3e8, 0),
		wantBal:      1e8,
		wantImmature: 3e8,
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
		wantImmature: 2e8,
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
			tx := newTx(new(big.Int))
			node.pendingTxs = append(node.pendingTxs, tx)
			node.tContractor.valueIn[tx.Hash()] = WeiToGwei(test.bal.PendingIn)
		}
		if test.bal.PendingOut.Cmp(new(big.Int)) > 0 {
			if test.token {
				tx := newTx(nil)
				node.pendingTxs = append(node.pendingTxs, tx)
				node.tContractor.valueOut[tx.Hash()] = WeiToGwei(test.bal.PendingOut)
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
	var nonceCounter int64

	newExtendedWalletTx := func(assetID uint32, amt, maxFees, blockNum uint64, txType asset.TransactionType) *extendedWalletTx {
		var tokenID *uint32
		if assetID != BipID {
			tokenID = &assetID
		}
		txHash := common.BytesToHash(encode.RandomBytes(32))

		et := &extendedWalletTx{
			WalletTransaction: &asset.WalletTransaction{
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
				newExtendedWalletTx(BipID, 1, 2, 0, asset.Send),
			},
			expPendingOut: 3,
		},
		{
			name:    "eth with token fees",
			assetID: BipID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(usdcEthID, 4, 5, 0, asset.Send),
			},
			expPendingOut: 5,
		},
		{
			name:    "token with 1 tx and other ignored assets",
			assetID: usdcEthID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(usdcEthID, 4, 5, 0, asset.Send),
				newExtendedWalletTx(usdcEthID+1, 8, 9, 0, asset.Send),
			},
			expPendingOut: 4,
		},
		{
			name:    "token with 1 tx incoming",
			assetID: usdcEthID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(usdcEthID, 15, 5, 0, asset.Redeem),
			},
			expPendingIn: 15,
		},
		{
			name:    "eth mixed txs",
			assetID: BipID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(BipID, 1, 2, 0, asset.Swap),     // 3 eth out
				newExtendedWalletTx(usdcEthID, 3, 4, 1, asset.Send), // confirmed
				newExtendedWalletTx(usdcEthID, 5, 6, 0, asset.Swap), // 6 eth out
				newExtendedWalletTx(BipID, 7, 1, 0, asset.Refund),   // 1 eth out, 7 eth in
			},
			expPendingOut: 10,
			expPendingIn:  7,
		},
		{
			name:    "already confirmed, but still waiting for txConfsNeededToConfirm",
			assetID: usdcEthID,
			unconfirmedTxs: []*extendedWalletTx{
				newExtendedWalletTx(usdcEthID, 15, 5, 1, asset.Redeem),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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

// badCoin fulfills the asset.Coin interface, but the ID does not match the ID expected in the
// ETH wallet code.
type badCoin uint64

func (*badCoin) ID() util.Bytes {
	return []byte{123}
}
func (*badCoin) String() string {
	return "abc"
}
func (*badCoin) TxID() string {
	return "abc"
}
func (b *badCoin) Value() uint64 {
	return uint64(*b)
}

func TestGaslessRedeem(t *testing.T) {
	numSecrets := 3
	secrets := make([][32]byte, 0, numSecrets)
	secretHashes := make([][32]byte, 0, numSecrets)
	for i := 0; i < numSecrets; i++ {
		var secret [32]byte
		copy(secret[:], encode.RandomBytes(32))
		secretHash := sha256.Sum256(secret[:])
		secrets = append(secrets, secret)
		secretHashes = append(secretHashes, secretHash)
	}

	value := uint64(1e9)
	calldata := []byte{0x01, 0x02, 0x03}
	relayTaskID := "test-relay-task-id-abc123"
	contractNonce := big.NewInt(123)
	ContractAddresses[1][util.Simnet] = common.BytesToAddress(encode.RandomBytes(20))

	initiator := common.HexToAddress("0x1111111111111111111111111111111111111111")
	participant := common.HexToAddress("0x2222222222222222222222222222222222222222")

	newSwapVector := func(idx int) *SwapVector {
		return &SwapVector{
			From:       initiator,
			To:         participant,
			Value:      big.NewInt(int64(value)),
			SecretHash: secretHashes[idx],
			LockTime:   0,
		}
	}

	newRedeem := func(idx int) *asset.Redemption {
		sv := newSwapVector(idx)
		return &asset.Redemption{
			Spends: &asset.AuditInfo{
				Contract:   EncodeContractData(1, sv.Locator()),
				SecretHash: secretHashes[idx][:],
				Coin: &coin{
					txHash: randomHash(),
					value:  value,
				},
			},
			Secret: secrets[idx][:],
		}
	}

	// Node fee values
	nodeBaseFee := GweiToWei(100)
	nodeTip := GweiToWei(10)
	nodeRecommendedFee := uint64(2*100 + 10)

	type test struct {
		name        string
		redemptions []*asset.Redemption
		swapState   map[[32]byte]*SwapState
		relayTaskID string
		submitErr   error
		estimateErr error
		balance     uint64
		nonceErr    error
		digestErr   error
		calldataErr error
		signDataErr error
		convertErr  error
		redeemErr   error // error from on-chain redeem (contractor)
		noRelay     bool  // if true, don't configure relayer

		expSubmitted bool // true = regular redeem
		expError     bool
		expNonce     *big.Int // if set, assert nonce passed to calldata builder
		expCoinValue uint64   // if set, override the default coin value check
	}

	tests := []test{
		{
			name:    "sufficient funds for regular redeem",
			balance: ethGasesV1.Redeem * nodeRecommendedFee,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			expSubmitted: true,
		},
		{
			name:    "successful gasless redeem via relay",
			balance: ethGasesV1.Redeem*nodeRecommendedFee - 1,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			relayTaskID: relayTaskID,
		},
		{
			name:    "relay fee estimation error",
			balance: ethGasesV1.Redeem*nodeRecommendedFee - 1,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			estimateErr: errors.New("estimate error"),
			expError:    true,
		},
		{
			name:    "relay submission error",
			balance: ethGasesV1.Redeem*nodeRecommendedFee - 1,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			submitErr: errors.New("test error"),
			expError:  true,
		},
		{
			name:    "nonce error",
			balance: ethGasesV1.Redeem*nodeRecommendedFee - 1,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			nonceErr: errors.New("nonce error"),
			expError: true,
		},
		{
			name:    "digest error",
			balance: ethGasesV1.Redeem*nodeRecommendedFee - 1,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			digestErr: errors.New("digest error"),
			expError:  true,
		},
		{
			name:    "calldata error",
			balance: ethGasesV1.Redeem*nodeRecommendedFee - 1,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			calldataErr: errors.New("calldata error"),
			expError:    true,
		},
		{
			name:    "sign hash error",
			balance: ethGasesV1.Redeem*nodeRecommendedFee - 1,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			signDataErr: errors.New("signing error"),
			expError:    true,
		},
		{
			name:    "convert redeems error",
			balance: ethGasesV1.Redeem*nodeRecommendedFee - 1,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			convertErr: errors.New("convert error"),
			expError:   true,
		},
		{
			name:    "multi-redemption batch via relay",
			balance: ethGasesV1.Redeem*nodeRecommendedFee - 1,
			redemptions: []*asset.Redemption{
				newRedeem(0),
				newRedeem(1),
				newRedeem(2),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
				secretHashes[1]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
				secretHashes[2]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			relayTaskID:  relayTaskID,
			expCoinValue: 3 * value,
		},
		{
			name:    "concurrent nonce bump",
			balance: ethGasesV1.Redeem*nodeRecommendedFee - 1,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			relayTaskID: relayTaskID,
			expNonce:    new(big.Int).Add(contractNonce, big.NewInt(1)),
		},
		{
			name:    "balance error routes to relay",
			balance: 0, // Will cause balance retrieval issues
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			relayTaskID: relayTaskID,
		},
		{
			name:    "on-chain redeem insufficient funds, relay fallback succeeds",
			balance: ethGasesV1.Redeem * nodeRecommendedFee, // enough for lockFunds
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			redeemErr:   errors.New("insufficient funds for gas * price + value"),
			relayTaskID: relayTaskID,
		},
		{
			name:    "on-chain redeem insufficient funds, relay fallback fails",
			balance: ethGasesV1.Redeem * nodeRecommendedFee,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			redeemErr:   errors.New("insufficient funds for gas * price + value"),
			relayTaskID: relayTaskID,
			submitErr:   errors.New("relay down"),
			expError:    true,
		},
		{
			name:    "on-chain redeem insufficient funds, no relay",
			balance: ethGasesV1.Redeem * nodeRecommendedFee,
			redemptions: []*asset.Redemption{
				newRedeem(0),
			},
			swapState: map[[32]byte]*SwapState{
				secretHashes[0]: {
					State: SSInitiated,
					Value: GweiToWei(value),
				},
			},
			redeemErr: errors.New("insufficient funds for gas * price + value"),
			noRelay:   true,
			expError:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w, eth, node, shutdown := tassetWallet(BipID)
			ethWallet := w.(*ETHWallet)
			defer shutdown()

			tr := newTRelayer()
			tr.taskID = test.relayTaskID
			tr.submitErr = test.submitErr
			tr.estimateErr = test.estimateErr
			tr.feeEstimate = &feeEstimate{
				RelayAddr: common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678"),
				Fee:       GweiToWei(100),
			}
			eth.relayer = tr

			eth.versionedContracts = map[uint32]common.Address{
				1: ContractAddresses[1][util.Simnet],
			}
			node.baseFee = nodeBaseFee
			node.tip = nodeTip

			node.tContractor.swapMap = test.swapState
			node.signedRedeemContractor.nonce = contractNonce
			node.signedRedeemContractor.calldata = calldata
			node.signedRedeemContractor.digest = randomHash()
			node.signedRedeemContractor.nonceErr = test.nonceErr
			node.signedRedeemContractor.digestErr = test.digestErr
			node.signedRedeemContractor.calldataErr = test.calldataErr
			node.signedRedeemContractor.convertErr = test.convertErr
			node.signDataErr = test.signDataErr
			node.bal = GweiToWei(test.balance)
			node.tContractor.redeemTx = types.NewTx(&types.DynamicFeeTx{})
			node.tContractor.redeemErr = test.redeemErr
			if test.noRelay {
				eth.relayer = nil
			}

			if test.expNonce != nil {
				// Pre-populate a pending relay at the on-chain nonce
				// to simulate a concurrent gasless redeem.
				eth.pendingRelays[common.Hash{1}] = &extendedWalletTx{
					WalletTransaction: &asset.WalletTransaction{},
					SubmissionTime:    uint64(time.Now().Unix()),
					Nonce:             new(big.Int).Set(contractNonce),
				}
			}

			ids, out, fees, submitted, err := ethWallet.GaslessRedeem(t.Context(), &asset.RedeemForm{
				Redemptions: test.redemptions,
			})
			if test.expError {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.expSubmitted != submitted {
				t.Fatalf("expected submitted %v, but got %v", test.expSubmitted, submitted)
			}
			if submitted {
				// Regular redeem path. Tested by TestRedeem.
				return
			}
			// Gasless redeem path. Check that relay was called.
			if len(tr.submittedReqs) != 1 {
				t.Fatalf("expected 1 relay request, got %d", len(tr.submittedReqs))
			}
			req := tr.submittedReqs[0]
			if req.Target != ContractAddresses[1][util.Simnet] {
				t.Fatalf("wrong relay target %s", req.Target)
			}
			if len(ids) != len(test.redemptions) {
				t.Fatalf("expected %d IDs, got %d", len(test.redemptions), len(ids))
			}
			if out == nil {
				t.Fatalf("expected non-nil output coin")
			}
			expValue := value
			if test.expCoinValue != 0 {
				expValue = test.expCoinValue
			}
			if out.Value() != expValue {
				t.Fatalf("expected coin value %d, got %d", expValue, out.Value())
			}
			if fees == 0 {
				t.Fatalf("expected non-zero fees")
			}
			if test.expNonce != nil {
				if node.signedRedeemContractor.lastNonce == nil {
					t.Fatalf("expected nonce to be captured")
				}
				if node.signedRedeemContractor.lastNonce.Cmp(test.expNonce) != 0 {
					t.Fatalf("expected nonce %s, got %s", test.expNonce, node.signedRedeemContractor.lastNonce)
				}
			}
		})
	}
}

func makeEmergencySignedRedeemCalldata(t *testing.T, signer *testNode, participant, feeRecipient common.Address,
	digest common.Hash, nonce, deadline, relayerFee *big.Int) ([]byte, []swapv1.ETHSwapRedemption) {
	t.Helper()

	redemptions := []swapv1.ETHSwapRedemption{{
		V: swapv1.ETHSwapVector{
			SecretHash:      randomHash(),
			Value:           big.NewInt(1),
			Initiator:       common.HexToAddress("0x0000000000000000000000000000000000000001"),
			RefundTimestamp: 1,
			Participant:     participant,
		},
		Secret: [32]byte{1},
	}}
	sig, _, err := signer.signHash(digest.Bytes())
	if err != nil {
		t.Fatalf("signHash error: %v", err)
	}
	calldata, err := ABIs[1].Pack("redeemWithSignature", redemptions, feeRecipient, relayerFee, nonce, deadline, sig)
	if err != nil {
		t.Fatalf("abi.Pack error: %v", err)
	}
	return calldata, redemptions
}

func TestEmergencyGaslessRedeemCalldataUsesOnChainNonce(t *testing.T) {
	w, eth, node, shutdown := tassetWallet(BipID)
	defer shutdown()

	ethWallet := w.(*ETHWallet)
	contractAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	eth.versionedContracts = map[uint32]common.Address{
		1: contractAddr,
	}

	value := uint64(1e9)
	secret := encode.RandomBytes(32)
	secretHash := sha256.Sum256(secret)
	swapVector := &SwapVector{
		From:       common.Address{},
		To:         common.Address{},
		Value:      big.NewInt(int64(value)),
		SecretHash: secretHash,
		LockTime:   0,
	}
	node.tContractor.swapMap[secretHash] = &SwapState{
		State: SSInitiated,
		Value: GweiToWei(value),
	}
	var redeemSecret [32]byte
	copy(redeemSecret[:], secret)
	node.signedRedeemContractor.redemptions = []swapv1.ETHSwapRedemption{{
		V: swapv1.ETHSwapVector{
			SecretHash:      swapVector.SecretHash,
			Value:           swapVector.Value,
			Initiator:       swapVector.From,
			RefundTimestamp: swapVector.LockTime,
			Participant:     eth.addr,
		},
		Secret: redeemSecret,
	}}
	node.signedRedeemContractor.calldata = []byte{0xde, 0xad, 0xbe, 0xef}
	node.signedRedeemContractor.digest = randomHash()
	node.signedRedeemContractor.nonce = big.NewInt(17)

	result, err := ethWallet.GaslessRedeemCalldata(t.Context(), &asset.RedeemForm{
		Redemptions: []*asset.Redemption{{
			Spends: &asset.AuditInfo{
				Contract:   EncodeContractData(1, swapVector.Locator()),
				SecretHash: secretHash[:],
				Coin: &coin{
					txHash: randomHash(),
					value:  value,
				},
			},
			Secret: secret,
		}},
	}, "0x1234567890abcdef1234567890abcdef12345678")
	if err != nil {
		t.Fatalf("GaslessRedeemCalldata error: %v", err)
	}
	if result.ContractAddress != contractAddr.String() {
		t.Fatalf("wrong contract address %s", result.ContractAddress)
	}
	if !bytes.Equal(result.Calldata, node.signedRedeemContractor.calldata) {
		t.Fatalf("wrong calldata %x", result.Calldata)
	}
	if node.signedRedeemContractor.lastNonce == nil || node.signedRedeemContractor.lastNonce.Cmp(big.NewInt(17)) != 0 {
		t.Fatalf("expected on-chain nonce 17, got %v", node.signedRedeemContractor.lastNonce)
	}
}

func TestValidateGaslessRedeemCalldata(t *testing.T) {
	w, eth, node, shutdown := tassetWallet(BipID)
	defer shutdown()

	ethWallet := w.(*ETHWallet)
	contractAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	eth.versionedContracts = map[uint32]common.Address{
		1: contractAddr,
	}

	digest := randomHash()
	nonce := big.NewInt(21)
	deadline := big.NewInt(time.Now().Add(time.Hour).Unix())
	relayerFee := big.NewInt(12345)
	calldata, _ := makeEmergencySignedRedeemCalldata(t, node.testNode, eth.addr, eth.addr, digest, nonce, deadline, relayerFee)
	node.signedRedeemContractor.digest = digest
	node.estimateGasResult = 123456

	validation, err := ethWallet.ValidateGaslessRedeemCalldata(t.Context(), contractAddr.String(), calldata)
	if err != nil {
		t.Fatalf("ValidateGaslessRedeemCalldata error: %v", err)
	}
	if validation.FeeRecipient != eth.addr.String() {
		t.Fatalf("wrong fee recipient %s", validation.FeeRecipient)
	}
	if validation.Nonce != nonce.String() {
		t.Fatalf("wrong nonce %s", validation.Nonce)
	}
	if validation.Deadline != deadline.Uint64() {
		t.Fatalf("wrong deadline %d", validation.Deadline)
	}
	if validation.RelayerFee != relayerFee.String() {
		t.Fatalf("wrong relayer fee %s", validation.RelayerFee)
	}
	if validation.GasEstimate != node.estimateGasResult {
		t.Fatalf("wrong gas estimate %d", validation.GasEstimate)
	}
	wantTxCost := new(big.Int).Mul(GweiToWei(202), new(big.Int).SetUint64(node.estimateGasResult))
	if validation.EstimatedTxCost != wantTxCost.String() {
		t.Fatalf("wrong estimated tx cost %s", validation.EstimatedTxCost)
	}
	if validation.Profitable {
		t.Fatalf("expected unprofitable validation")
	}
}

func TestValidateGaslessRedeemCalldataRejectsWrongFeeRecipient(t *testing.T) {
	w, eth, node, shutdown := tassetWallet(BipID)
	defer shutdown()

	ethWallet := w.(*ETHWallet)
	contractAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	eth.versionedContracts = map[uint32]common.Address{
		1: contractAddr,
	}

	digest := randomHash()
	nonce := big.NewInt(21)
	deadline := big.NewInt(time.Now().Add(time.Hour).Unix())
	relayerFee := big.NewInt(12345)
	calldata, _ := makeEmergencySignedRedeemCalldata(t, node.testNode, eth.addr,
		common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678"),
		digest, nonce, deadline, relayerFee)
	node.signedRedeemContractor.digest = digest

	_, err := ethWallet.ValidateGaslessRedeemCalldata(t.Context(), contractAddr.String(), calldata)
	if err == nil || !strings.Contains(err.Error(), "fee recipient") {
		t.Fatalf("expected fee recipient error, got %v", err)
	}
}

func TestSubmitGaslessRedeemCalldata(t *testing.T) {
	w, eth, node, shutdown := tassetWallet(BipID)
	defer shutdown()

	ethWallet := w.(*ETHWallet)
	contractAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	eth.versionedContracts = map[uint32]common.Address{
		1: contractAddr,
	}

	digest := randomHash()
	nonce := big.NewInt(21)
	deadline := big.NewInt(time.Now().Add(time.Hour).Unix())
	relayerFee := big.NewInt(12345)
	calldata, _ := makeEmergencySignedRedeemCalldata(t, node.testNode, eth.addr, eth.addr, digest, nonce, deadline, relayerFee)
	node.signedRedeemContractor.digest = digest
	node.estimateGasResult = 123456
	node.sendTxTx = node.newTransaction(3, big.NewInt(0))

	txHash, err := ethWallet.SubmitGaslessRedeemCalldata(t.Context(), contractAddr.String(), calldata)
	if err != nil {
		t.Fatalf("SubmitGaslessRedeemCalldata error: %v", err)
	}
	if !bytes.Equal(txHash, node.sendTxTx.Hash().Bytes()) {
		t.Fatalf("wrong tx hash %x", txHash)
	}
	if node.sentTxs != 1 {
		t.Fatalf("expected 1 sent tx, got %d", node.sentTxs)
	}
	if node.lastSendTo == nil || *node.lastSendTo != contractAddr {
		t.Fatalf("wrong send target %v", node.lastSendTo)
	}
	if !bytes.Equal(node.lastSendData, calldata) {
		t.Fatalf("wrong send calldata %x", node.lastSendData)
	}
}

func TestNextContractNonce(t *testing.T) {
	now := time.Now()
	fresh := uint64(now.Unix())
	expired := uint64(now.Add(-signedRedeemDeadline - time.Minute).Unix())

	tests := []struct {
		name         string
		onChainNonce int64
		ops          map[common.Hash]*extendedWalletTx
		expected     int64
	}{
		{
			name:         "no pending ops",
			onChainNonce: 5,
			ops:          map[common.Hash]*extendedWalletTx{},
			expected:     5,
		},
		{
			name:         "one pending at on-chain nonce",
			onChainNonce: 5,
			ops: map[common.Hash]*extendedWalletTx{
				{1}: {
					WalletTransaction: &asset.WalletTransaction{},
					Nonce:             big.NewInt(5),
					SubmissionTime:    fresh,
				},
			},
			expected: 6,
		},
		{
			name:         "pending below on-chain",
			onChainNonce: 7,
			ops: map[common.Hash]*extendedWalletTx{
				{1}: {
					WalletTransaction: &asset.WalletTransaction{},
					Nonce:             big.NewInt(5),
					SubmissionTime:    fresh,
				},
			},
			expected: 7,
		},
		{
			name:         "multiple pending",
			onChainNonce: 5,
			ops: map[common.Hash]*extendedWalletTx{
				{1}: {
					WalletTransaction: &asset.WalletTransaction{},
					Nonce:             big.NewInt(5),
					SubmissionTime:    fresh,
				},
				{2}: {
					WalletTransaction: &asset.WalletTransaction{},
					Nonce:             big.NewInt(6),
					SubmissionTime:    fresh,
				},
			},
			expected: 7,
		},
		{
			name:         "expired pending",
			onChainNonce: 5,
			ops: map[common.Hash]*extendedWalletTx{
				{1}: {
					WalletTransaction: &asset.WalletTransaction{},
					Nonce:             big.NewInt(5),
					SubmissionTime:    expired,
				},
			},
			expected: 5,
		},
		{
			name:         "mix fresh and expired",
			onChainNonce: 5,
			ops: map[common.Hash]*extendedWalletTx{
				{1}: {
					WalletTransaction: &asset.WalletTransaction{},
					Nonce:             big.NewInt(5),
					SubmissionTime:    expired,
				},
				{2}: {
					WalletTransaction: &asset.WalletTransaction{},
					Nonce:             big.NewInt(6),
					SubmissionTime:    fresh,
				},
			},
			expected: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &baseWallet{
				pendingRelays: tt.ops,
			}
			got := w.nextContractNonce(big.NewInt(tt.onChainNonce))
			if got.Int64() != tt.expected {
				t.Fatalf("expected %d, got %d", tt.expected, got.Int64())
			}
		})
	}
}

func packInitiateDataV0(initiations []*Initiation) ([]byte, error) {
	abiInitiations := make([]swapv0.ETHSwapInitiation, 0, len(initiations))
	for _, init := range initiations {
		bigVal := new(big.Int).Set(init.Value)
		abiInitiations = append(abiInitiations, swapv0.ETHSwapInitiation{
			RefundTimestamp: big.NewInt(init.LockTime.Unix()),
			SecretHash:      init.SecretHash,
			Participant:     init.Participant,
			Value:           new(big.Int).Mul(bigVal, big.NewInt(GweiFactor)),
		})
	}
	return (*ABIs[0]).Pack("initiate", abiInitiations)
}

func packRedeemDataV0(redemptions []*Redemption) ([]byte, error) {
	abiRedemptions := make([]swapv0.ETHSwapRedemption, 0, len(redemptions))
	for _, redeem := range redemptions {
		abiRedemptions = append(abiRedemptions, swapv0.ETHSwapRedemption{
			Secret:     redeem.Secret,
			SecretHash: redeem.SecretHash,
		})
	}
	return (*ABIs[0]).Pack("redeem", abiRedemptions)
}

func TestOwnsAddress(t *testing.T) {
	address := "0b84C791b79Ee37De042AD2ffF1A253c3ce9bc27" // no "0x" prefix
	if !common.IsHexAddress(address) {
		t.Fatalf("bad test address")
	}

	var otherAddress common.Address
	rand.Read(otherAddress[:])

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

	// Relay coin ID falls back to the relay task hash until a tx hash is known.
	relayTaskHash := common.BytesToHash(encode.RandomBytes(common.HashLength))
	coinID, err = drv.DecodeCoinID(relayCoinID(relayTaskHash, common.Hash{}))
	if err != nil {
		t.Fatalf("error decoding relay coin id without tx hash: %v", err)
	}
	wantPendingRelayCoinID := "relayTaskHash:" + relayTaskHash.String()
	if coinID != wantPendingRelayCoinID {
		t.Fatalf("expected relay coin id to be %s but got %s", wantPendingRelayCoinID, coinID)
	}

	// Once the relay tx hash is known, display that instead of the relay task hash.
	relayTxHash := common.BytesToHash(encode.RandomBytes(common.HashLength))
	coinID, err = drv.DecodeCoinID(relayCoinID(relayTaskHash, relayTxHash))
	if err != nil {
		t.Fatalf("error decoding relay coin id with tx hash: %v", err)
	}
	if coinID != relayTxHash.String() {
		t.Fatalf("expected relay coin id to be %s but got %s", relayTxHash.String(), coinID)
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
func ethToGwei(v uint64) uint64 {
	return v * GweiFactor
}

func ethToWei(v uint64) *big.Int {
	bigV := new(big.Int).SetUint64(ethToGwei(v))
	return new(big.Int).Mul(bigV, big.NewInt(GweiFactor))
}

func TestReconfigure(t *testing.T) {
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

	// Clearing relayer setting should clear any in-memory relayer too.
	eth.relayer = newTRelayer()
	eth.settingsMtx.Lock()
	eth.settings[relayerKey] = ""
	eth.settingsMtx.Unlock()

	restart, err = reconfigurer.Reconfigure(context.Background(), walletCfg, "")
	if err != nil {
		t.Fatalf("unexpected error when clearing relayer setting: %v", err)
	}
	if restart {
		t.Fatalf("unexpected restart when clearing relayer setting")
	}
	if eth.relayer != nil {
		t.Fatal("relayer was not cleared when relayer setting was empty")
	}
}

func TestSend(t *testing.T) {
	t.Run("eth", func(t *testing.T) { testSend(t, BipID) })
	t.Run("token", func(t *testing.T) { testSend(t, usdcEthID) })
	t.Run("link", func(t *testing.T) { testSend(t, linkEthID) })
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

func TestConfirmTransaction(t *testing.T) {
	t.Run("eth", func(t *testing.T) { testConfirmTransaction(t, BipID) })
	t.Run("token", func(t *testing.T) { testConfirmTransaction(t, usdcEthID) })
}

func testConfirmTransaction(t *testing.T, assetID uint32) {
	wi, eth, node, shutdown := tassetWallet(assetID)
	defer shutdown()

	db := eth.txDB.(*tTxDB)

	const tip = 12
	const confBlock = tip - txConfsNeededToConfirm + 1

	var secret, secretHash [32]byte
	copy(secret[:], encode.RandomBytes(32))
	copy(secretHash[:], encode.RandomBytes(32))
	var txHash common.Hash
	copy(txHash[:], encode.RandomBytes(32))

	confirmTx := asset.NewRedeemConfTx(&asset.AuditInfo{
		Contract: EncodeContractData(0, secretHash[:]),
	}, secret[:])

	pendingTx := &extendedWalletTx{
		WalletTransaction: &asset.WalletTransaction{
			ID:          txHash.String(),
			BlockNumber: confBlock + 1,
		},
		txHash: txHash,
	}
	dbTx := &extendedWalletTx{
		WalletTransaction: &asset.WalletTransaction{
			ID:          txHash.String(),
			BlockNumber: confBlock,
		},
	}

	type test struct {
		name                      string
		expectedConfs             uint64
		expectErr                 bool
		expectSwapRefundedErr     bool
		expectRedemptionFailedErr bool
		expectTxLostErr           bool
		pendingTx                 *extendedWalletTx
		dbTx                      *extendedWalletTx
		dbErr                     error
		step                      SwapStep
		receipt                   *types.Receipt
		receiptErr                error
		confirmTx                 *asset.ConfirmTx
	}

	tests := []*test{
		{
			name: "found on-chain. not yet confirmed",
			step: SSInitiated,
			receipt: &types.Receipt{
				Status:      types.ReceiptStatusSuccessful,
				BlockNumber: big.NewInt(confBlock + 1),
			},
			confirmTx:     confirmTx,
			expectedConfs: txConfsNeededToConfirm - 1,
		},
		{
			name:          "found on-chain. confirmed",
			step:          SSRedeemed,
			expectedConfs: txConfsNeededToConfirm,
			receipt: &types.Receipt{
				Status:      types.ReceiptStatusSuccessful,
				BlockNumber: big.NewInt(confBlock),
			},
			confirmTx: confirmTx,
		},
		{
			name:          "found in pending txs",
			step:          SSRedeemed,
			pendingTx:     pendingTx,
			confirmTx:     confirmTx,
			expectedConfs: txConfsNeededToConfirm - 1,
		},
		{
			name:          "found in db",
			step:          SSRedeemed,
			dbTx:          dbTx,
			expectedConfs: txConfsNeededToConfirm,
			confirmTx:     confirmTx,
			receipt: &types.Receipt{
				Status:      types.ReceiptStatusSuccessful,
				BlockNumber: big.NewInt(confBlock),
			},
		},
		{
			name:          "db error not propagated. unconfirmed",
			step:          SSRedeemed,
			dbErr:         errors.New("test error"),
			expectedConfs: txConfsNeededToConfirm, // contract shows redeemed, treated as confirmed
			confirmTx:     confirmTx,
			receipt: &types.Receipt{
				Status:      types.ReceiptStatusSuccessful,
				BlockNumber: big.NewInt(confBlock + 1),
			},
		},
		{
			name:                      "found on-chain. tx failed",
			step:                      SSInitiated,
			expectErr:                 true,
			expectRedemptionFailedErr: true,
			confirmTx:                 confirmTx,
			receipt: &types.Receipt{
				Status:      types.ReceiptStatusFailed,
				BlockNumber: big.NewInt(confBlock),
			},
		},
		{
			name: "found on-chain. redeemed by another unknown transaction",
			step: SSRedeemed,
			receipt: &types.Receipt{
				Status:      types.ReceiptStatusFailed,
				BlockNumber: big.NewInt(confBlock),
			},
			confirmTx:     confirmTx,
			expectedConfs: txConfsNeededToConfirm,
		},
		{
			name: "refund found on-chain. refunded by another unknown transaction",
			step: SSRefunded,
			receipt: &types.Receipt{
				Status:      types.ReceiptStatusFailed,
				BlockNumber: big.NewInt(confBlock),
			},
			confirmTx:     asset.NewRefundConfTx(txHash[:], EncodeContractData(0, secretHash[:]), secret[:]),
			expectedConfs: txConfsNeededToConfirm,
		},
		{
			name: "redeem refunded by another unknown transaction",
			step: SSRefunded,
			receipt: &types.Receipt{
				Status:      types.ReceiptStatusFailed,
				BlockNumber: big.NewInt(confBlock),
			},
			confirmTx: confirmTx,
			expectErr: true,
		},
		{
			name: "refund redeemed by another unknown transaction",
			step: SSRedeemed,
			receipt: &types.Receipt{
				Status:      types.ReceiptStatusFailed,
				BlockNumber: big.NewInt(confBlock),
			},
			confirmTx: asset.NewRefundConfTx(txHash[:], EncodeContractData(0, secretHash[:]), secret[:]),
			expectErr: true,
		},
		{
			name:            "contract shows no swap",
			step:            SSNone,
			confirmTx:       confirmTx,
			expectErr:       true,
			expectTxLostErr: true,
		},
		{
			name:          "receipt RPC error, contract initiated, retries",
			step:          SSInitiated,
			confirmTx:     confirmTx,
			receiptErr:    errors.New("json parse error: non-standard signature"),
			expectedConfs: 0,
		},
	}

	runTest := func(test *test) {
		fmt.Printf("###### %s ###### \n", test.name)

		node.tContractor.swapMap = map[[32]byte]*SwapState{
			secretHash: {State: test.step, Value: big.NewInt(1)},
		}
		node.tContractor.lastRedeems = nil
		node.tokenContractor.bal = big.NewInt(1e9)
		node.bal = big.NewInt(1e9)

		eth.pendingTxs = []*extendedWalletTx{}
		if test.pendingTx != nil {
			eth.pendingTxs = append(eth.pendingTxs, test.pendingTx)
		}

		db.txToGet = test.dbTx
		db.getTxErr = test.dbErr

		node.lastSignedTx = nil
		eth.currentTip = &types.Header{Number: big.NewInt(tip)}
		node.receipt = test.receipt
		node.receiptErr = test.receiptErr

		result, err := wi.(interface {
			ConfirmTransaction(util.Bytes, *asset.ConfirmTx, uint64) (*asset.ConfirmTxStatus, error)
		}).ConfirmTransaction(txHash[:], test.confirmTx, 0)
		if test.expectErr {
			if err == nil {
				t.Fatalf("%s: expected error but did not get", test.name)
			}
			if test.expectRedemptionFailedErr && !errors.Is(err, asset.ErrTxRejected) {
				t.Fatalf("%s: expected rejected tx error. got %v", test.name, err)
			}
			if test.expectSwapRefundedErr && !errors.Is(asset.ErrSwapRefunded, err) {
				t.Fatalf("%s: expected swap refunded error but got %v", test.name, err)
			}
			if test.expectTxLostErr && !errors.Is(err, asset.ErrTxLost) {
				t.Fatalf("%s: expected tx lost error but got %v", test.name, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		// Check that the resulting status is as expected
		if test.expectedConfs != result.Confs ||
			txConfsNeededToConfirm != result.Req {
			t.Fatalf("%s: expected confs %d != result %d", test.name, test.expectedConfs, result.Confs)
		}
	}

	for _, test := range tests {
		runTest(test)
	}
}

func TestConfirmRelay(t *testing.T) {
	const tip = 12
	const confBlock = tip - txConfsNeededToConfirm + 1

	var secret, secretHash [32]byte
	copy(secret[:], encode.RandomBytes(32))
	copy(secretHash[:], encode.RandomBytes(32))
	var relayTaskHash common.Hash
	copy(relayTaskHash[:], encode.RandomBytes(32))
	var txHash common.Hash
	copy(txHash[:], encode.RandomBytes(32))

	agedOutSubmission := time.Now().Add(-1*txAgeOut - 1)

	ci := &asset.AuditInfo{
		Contract: EncodeContractData(0, secretHash[:]),
	}
	confirmTx := asset.NewRedeemConfTx(ci, secret[:])

	pendingRelay := func(confirmed bool, agedOut bool, rejected bool, hasReceipt bool) *extendedWalletTx {
		blockNumber := confBlock
		if !confirmed {
			blockNumber = tip
		}
		submissionTime := time.Now()
		if agedOut {
			submissionTime = agedOutSubmission
		}
		receipt := &types.Receipt{
			BlockNumber: big.NewInt(int64(blockNumber)),
			TxHash:      txHash,
		}
		if !hasReceipt {
			receipt = nil
		}
		return &extendedWalletTx{
			WalletTransaction: &asset.WalletTransaction{
				Confirmed:   confirmed,
				ID:          relayTaskHash.String(),
				BlockNumber: uint64(blockNumber),
				Rejected:    rejected,
				RelayTxID:   txHash.String(),
			},
			Receipt:        receipt,
			SubmissionTime: uint64(submissionTime.Unix()),
		}
	}

	type test struct {
		name           string
		expectedResult *asset.ConfirmTxStatus
		expectedError  error

		pendingRelay *extendedWalletTx
		step         SwapStep
	}

	tests := []test{
		{
			name: "success - confirmed - from pending",
			expectedResult: &asset.ConfirmTxStatus{
				Confs:  txConfsNeededToConfirm,
				Req:    txConfsNeededToConfirm,
				CoinID: relayCoinID(relayTaskHash, txHash),
			},
			pendingRelay: pendingRelay(true, false, false, true),
			step:         SSRedeemed,
		},
		{
			name: "pending - not enough confirmations - from pending",
			expectedResult: &asset.ConfirmTxStatus{
				Confs:  1,
				Req:    txConfsNeededToConfirm,
				CoinID: relayCoinID(relayTaskHash, txHash),
			},
			pendingRelay: pendingRelay(false, false, false, true),
			step:         SSRedeemed,
		},
		{
			name:          "rejected tx - from pending",
			expectedError: asset.ErrTxRejected,
			pendingRelay:  pendingRelay(true, false, true, true),
			step:          SSInitiated,
		},
		{
			name:          "swap refunded - from pending",
			expectedError: asset.ErrSwapRefunded,
			pendingRelay:  pendingRelay(true, true, false, false),
			step:          SSRefunded,
		},
		{
			name: "swap redeemed, receipt not found - from pending",
			expectedResult: &asset.ConfirmTxStatus{
				Confs:  txConfsNeededToConfirm,
				Req:    txConfsNeededToConfirm,
				CoinID: relayCoinID(relayTaskHash, txHash),
			},
			pendingRelay: pendingRelay(false, true, false, false),
			step:         SSRedeemed,
		},
		{
			name: "not in pending, status redeemed on-chain",
			expectedResult: &asset.ConfirmTxStatus{
				Confs:  txConfsNeededToConfirm,
				Req:    txConfsNeededToConfirm,
				CoinID: relayCoinID(relayTaskHash, common.Hash{}),
			},
			step: SSRedeemed,
		},
		{
			name:          "not in pending, still initiated - tx lost",
			expectedError: asset.ErrTxLost,
			step:          SSInitiated,
		},
	}

	runTest := func(test *test) {
		wi, eth, node, shutdown := tassetWallet(BipID)
		defer shutdown()
		node.tContractor.swapMap = map[[32]byte]*SwapState{
			secretHash: {State: test.step, Value: big.NewInt(1)},
		}
		node.bal = big.NewInt(1e9)

		eth.pendingRelays = make(map[common.Hash]*extendedWalletTx)
		if test.pendingRelay != nil {
			eth.pendingRelays[relayTaskHash] = test.pendingRelay
		}
		eth.currentTip = &types.Header{Number: big.NewInt(tip)}

		result, err := wi.(*ETHWallet).ConfirmTransaction(relayCoinID(relayTaskHash, txHash), confirmTx, 0)
		if err != test.expectedError {
			t.Fatalf("%s: expected error %v != result %v", test.name, test.expectedError, err)
		}
		if !reflect.DeepEqual(test.expectedResult, result) {
			t.Fatalf("%s: expected result %+v != result %+v", test.name, test.expectedResult, result)
		}
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runTest(&test)
		})
	}
}

// Ensures that a small rise in the base fee between estimation
// and sending will not cause a failure.
func TestEstimateVsActualSendFees(t *testing.T) {
	t.Run("eth", func(t *testing.T) { testEstimateVsActualSendFees(t, BipID) })
	t.Run("token", func(t *testing.T) { testEstimateVsActualSendFees(t, usdcEthID) })
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
	t.Run("eth", func(t *testing.T) { testEstimateSendTxFee(t, BipID) })
	t.Run("token", func(t *testing.T) { testEstimateSendTxFee(t, usdcEthID) })
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

func TestSwapOrRedemptionFeesPaid(t *testing.T) {
	ctx := t.Context()
	_, bw, node, shutdown := tassetWallet(BipID)
	defer shutdown()

	secretHA, secretHB := encode.RandomBytes(32), encode.RandomBytes(32)
	contractDataFn := func(ver uint32, secretH []byte) []byte {
		s := [32]byte{}
		copy(s[:], secretH)
		return EncodeContractData(ver, s[:])
	}
	const tip = 100
	const feeRate = 2 // gwei / gas
	const gasUsed = 100
	const fees = feeRate * gasUsed

	bw.currentTip = &types.Header{
		BaseFee: GweiToWei(2),
		Number:  big.NewInt(tip),
	}

	confirmedReceipt := &types.Receipt{
		GasUsed:           gasUsed,
		EffectiveGasPrice: GweiToWei(feeRate),
		BlockNumber:       big.NewInt(tip - txConfsNeededToConfirm + 1),
	}

	unconfirmedReceipt := &types.Receipt{
		BlockNumber: big.NewInt(tip - txConfsNeededToConfirm + 2),
	}

	initFn := func(secretHs [][]byte) []byte {
		inits := make([]*Initiation, 0, len(secretHs))
		for i := range secretHs {
			s := [32]byte{}
			copy(s[:], secretHs[i])
			init := &Initiation{
				LockTime:   time.Now(),
				SecretHash: s,
				Value:      big.NewInt(0),
			}
			inits = append(inits, init)
		}
		data, err := packInitiateDataV0(inits)
		if err != nil {
			t.Fatalf("problem packing inits: %v", err)
		}
		return data
	}
	redeemFn := func(secretHs [][]byte) []byte {
		redeems := make([]*Redemption, 0, len(secretHs))
		for i := range secretHs {
			s := [32]byte{}
			copy(s[:], secretHs[i])
			redeem := &Redemption{
				SecretHash: s,
			}
			redeems = append(redeems, redeem)
		}
		data, err := packRedeemDataV0(redeems)
		if err != nil {
			t.Fatalf("problem packing redeems: %v", err)
		}
		return data
	}
	abFn := func() [][]byte {
		return [][]byte{secretHA, secretHB}
	}
	sortedFn := func() [][]byte {
		ab := abFn()
		sort.Slice(ab, func(i, j int) bool { return bytes.Compare(ab[i], ab[j]) < 0 })
		return ab
	}
	initTx := tTx(200, 2, 0, nil, initFn(abFn()), 200)
	redeemTx := tTx(200, 3, 0, nil, redeemFn(abFn()), 200)
	tests := []struct {
		name            string
		contractData    []byte
		isInit, wantErr bool
		receipt         *types.Receipt
		receiptTx       *types.Transaction
		receiptErr      error
		wantSecrets     [][]byte
		pendingTx       *types.Transaction
		pendingTxBlock  uint64
	}{{
		name:         "ok init",
		contractData: contractDataFn(0, secretHA),
		isInit:       true,
		receipt:      confirmedReceipt,
		receiptTx:    initTx,
		wantSecrets:  sortedFn(),
	}, {
		name:         "ok redeem",
		contractData: contractDataFn(0, secretHB),
		receipt:      confirmedReceipt,
		receiptTx:    redeemTx,
		wantSecrets:  sortedFn(),
	}, {
		name:           "ok init from pending txs",
		contractData:   contractDataFn(0, secretHA),
		isInit:         true,
		pendingTx:      initTx,
		pendingTxBlock: confirmedReceipt.BlockNumber.Uint64(),
		wantSecrets:    sortedFn(),
	}, {
		name:           "ok redeem from pending txs",
		contractData:   contractDataFn(0, secretHB),
		pendingTx:      redeemTx,
		pendingTxBlock: confirmedReceipt.BlockNumber.Uint64(),
		wantSecrets:    sortedFn(),
	}, {
		name:         "bad contract data",
		contractData: nil,
		wantErr:      true,
	}, {
		name:         "receipt error",
		contractData: contractDataFn(0, secretHA),
		receiptErr:   errors.New("test error"),
		wantErr:      true,
	}, {
		name:         "not enough confirms",
		contractData: contractDataFn(0, secretHA),
		receipt:      unconfirmedReceipt,
		wantErr:      true,
	}, {
		name:           "not enough confs, pending tx",
		contractData:   contractDataFn(0, secretHA),
		isInit:         true,
		pendingTx:      initTx,
		pendingTxBlock: confirmedReceipt.BlockNumber.Uint64() + 1,
		wantSecrets:    sortedFn(),
		wantErr:        true,
	}, {
		name:         "bad init data",
		contractData: contractDataFn(0, secretHA),
		isInit:       true,
		receipt:      confirmedReceipt,
		receiptTx:    tTx(200, 2, 0, nil, nil, 200),
		wantErr:      true,
	}, {
		name:         "bad redeem data",
		contractData: contractDataFn(0, secretHA),
		receipt:      confirmedReceipt,
		receiptTx:    tTx(200, 2, 0, nil, nil, 200),
		wantErr:      true,
	}, {
		name:         "secret hash not found",
		contractData: contractDataFn(0, secretHB),
		isInit:       true,
		receipt:      confirmedReceipt,
		receiptTx:    tTx(200, 2, 0, nil, initFn([][]byte{secretHA}), 200),
		wantErr:      true,
	}}
	for _, test := range tests {
		var txHash common.Hash
		if test.pendingTx != nil {
			wt := bw.extendedTx(&genTxResult{
				tx:     test.pendingTx,
				txType: asset.Unknown,
				amt:    1,
			})
			wt.BlockNumber = test.pendingTxBlock
			wt.Fees = fees
			bw.pendingTxs = []*extendedWalletTx{wt}
			txHash = test.pendingTx.Hash()
		}
		node.receiptTx = test.receiptTx
		node.receipt = test.receipt
		node.receiptErr = test.receiptErr
		feesPaid, secretHs, err := bw.swapOrRedemptionFeesPaid(ctx, txHash[:], test.contractData, test.isInit)
		if test.wantErr {
			if err == nil {
				t.Fatalf("%q: expected error", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", test.name, err)
		}
		if feesPaid != fees {
			t.Fatalf("%q: wanted fee %d but got %d", test.name, fees, feesPaid)
		}
		if len(test.wantSecrets) != len(secretHs) {
			t.Fatalf("%q: wanted %d secrets but got %d", test.name, len(test.wantSecrets), len(secretHs))
		}
		for i := range test.wantSecrets {
			sGot := secretHs[i]
			sWant := test.wantSecrets[i]
			if !bytes.Equal(sGot, sWant) {
				t.Fatalf("%q: wanted secret %x but got %x at position %d", test.name, sWant, sGot, i)
			}
		}
	}
}

func TestReceiptCache(t *testing.T) {
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

func parseRecoveryID(c asset.Coin) []byte {
	return c.(asset.RecoveryCoin).RecoveryID()
}

func randomHash() common.Hash {
	return common.BytesToHash(encode.RandomBytes(20))
}
