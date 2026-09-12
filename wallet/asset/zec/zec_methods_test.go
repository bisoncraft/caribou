// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/btc"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/wire"
)

func TestPureHelpers(t *testing.T) {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxOut(wire.NewTxOut(1, []byte{0x01}))
	dTx := zecTx(tx)
	if dTx == nil {
		t.Fatalf("zecTx returned nil")
	}

	if !isDust(1, 1000) {
		t.Fatalf("expected small value to be dust")
	}
	if isDust(1e8, 34) {
		t.Fatalf("expected large value to not be dust")
	}

	if got := toZats(1.5); got != 150000000 {
		t.Fatalf("toZats(1.5) = %d, want 150000000", got)
	}

	h := hashTx(tx)
	if h == nil {
		t.Fatalf("hashTx returned nil")
	}

	dummy := dummyTx()
	b, _ := dummy.Bytes()
	msgTx, err := deserializeTx(b)
	if err != nil {
		t.Fatalf("deserializeTx error: %v", err)
	}
	if msgTx == nil {
		t.Fatalf("deserializeTx returned nil tx")
	}
	if _, err := deserializeTx([]byte{0xff}); err == nil {
		t.Fatalf("expected error for bad tx bytes")
	}
}

func TestDecodeCoinIDHelper(t *testing.T) {
	coinID := encodeCoinID(tTxHash, 3)
	h, vout, err := decodeCoinID(coinID)
	if err != nil {
		t.Fatalf("decodeCoinID error: %v", err)
	}
	if h.String() != tTxHash.String() || vout != 3 {
		t.Fatalf("unexpected decode: hash=%s vout=%d", h, vout)
	}

	if _, _, err := decodeCoinID([]byte{0x01}); err == nil {
		t.Fatalf("expected error for wrong-length coin id")
	}
}

func TestTxCoin(t *testing.T) {
	c := &txCoin{txHash: tTxHash, v: 100}
	if string(c.ID()) != string(tTxHash[:]) {
		t.Fatalf("wrong ID")
	}
	if c.String() != tTxHash.String() {
		t.Fatalf("wrong String")
	}
	if c.Value() != 100 {
		t.Fatalf("wrong Value")
	}
	if c.TxID() != tTxHash.String() {
		t.Fatalf("wrong TxID")
	}
}

func TestSendEnough(t *testing.T) {
	enough := sendEnough(1e8)
	ok, _ := enough(1, 200, 1e8)
	if ok {
		t.Fatalf("expected not enough when sum only covers value with no fees")
	}
	ok, extra := enough(1, 200, 2e8)
	if !ok {
		t.Fatalf("expected enough with double the value")
	}
	if extra == 0 {
		t.Fatalf("expected nonzero extra")
	}
}

func TestStandardSendFeeAndFeeRate(t *testing.T) {
	w, _, shutdown := tNewWallet()
	defer shutdown()

	if got := w.StandardSendFee(0); got == 0 {
		t.Fatalf("expected nonzero standard send fee")
	}
	if got := w.FeeRate(); got != 5000 {
		t.Fatalf("FeeRate() = %d, want 5000", got)
	}
}

func TestValidateAddress(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsValid: true})
	if !w.ValidateAddress(tAddr) {
		t.Fatalf("expected valid address")
	}

	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsValid: false})
	if w.ValidateAddress(tAddr) {
		t.Fatalf("expected invalid address")
	}

	cl.queueResponse(methodZValidateAddress, errTest)
	if w.ValidateAddress(tAddr) {
		t.Fatalf("expected false on rpc error")
	}
}

func TestLastShieldedAddressCached(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cached := tAddr
	w.lastAddress.Store(&cached)
	addr, err := w.lastShieldedAddress()
	if err != nil {
		t.Fatalf("lastShieldedAddress error: %v", err)
	}
	if addr != cached {
		t.Fatalf("expected cached address, got %s", addr)
	}
}

func TestLastShieldedAddressFromAccounts(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZListAccounts, []zListAccountsResult{
		{
			Number: shieldedAcctNumber,
			Addresses: []struct {
				Diversifier uint32 `json:"diversifier"`
				UnifiedAddr string `json:"ua"`
			}{{UnifiedAddr: tAddr}},
		},
	})
	addr, err := w.lastShieldedAddress()
	if err != nil {
		t.Fatalf("lastShieldedAddress error: %v", err)
	}
	if addr != tAddr {
		t.Fatalf("unexpected address: %s", addr)
	}

	// Cached now, no further RPC calls needed.
	addr2, err := w.lastShieldedAddress()
	if err != nil {
		t.Fatalf("lastShieldedAddress (cached) error: %v", err)
	}
	if addr2 != tAddr {
		t.Fatalf("unexpected cached address: %s", addr2)
	}
}

func TestLastShieldedAddressGeneratesNew(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZListAccounts, []zListAccountsResult{{Number: shieldedAcctNumber}}) // no addresses yet
	cl.queueResponse(methodZGetAddressForAccount, &zGetAddressForAccountResult{Address: tAddr})

	addr, err := w.lastShieldedAddress()
	if err != nil {
		t.Fatalf("lastShieldedAddress error: %v", err)
	}
	if addr != tAddr {
		t.Fatalf("unexpected address: %s", addr)
	}
}

func TestNewShieldedAddress(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZGetAddressForAccount, &zGetAddressForAccountResult{Address: tAddr})
	addr, err := w.newShieldedAddress()
	if err != nil {
		t.Fatalf("newShieldedAddress error: %v", err)
	}
	if addr != tAddr {
		t.Fatalf("unexpected address: %s", addr)
	}
	if got := w.lastAddress.Load(); got == nil || *got != tAddr {
		t.Fatalf("expected lastAddress to be updated")
	}
}

func TestDepositAddressAndNewAddress(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZGetAddressForAccount, &zGetAddressForAccountResult{Address: tUnifiedAddr})
	cl.queueResponse(methodZListUnifiedReceivers, &unifiedReceivers{Transparent: tAddr})
	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress error: %v", err)
	}
	if addr[:len(depositAddrPrefix)] != depositAddrPrefix {
		t.Fatalf("expected depositAddrPrefix, got %s", addr)
	}

	cl.queueResponse(methodZGetAddressForAccount, &zGetAddressForAccountResult{Address: tUnifiedAddr})
	cl.queueResponse(methodZListUnifiedReceivers, &unifiedReceivers{Transparent: tAddr})
	addr2, err := w.NewAddress()
	if err != nil {
		t.Fatalf("NewAddress error: %v", err)
	}
	if addr2 == "" {
		t.Fatalf("expected non-empty address")
	}

	// zGetAddressForAccount error.
	cl.queueResponse(methodZGetAddressForAccount, errTest)
	if _, err := w.DepositAddress(); err == nil {
		t.Fatalf("expected error from zGetAddressForAccount failure")
	}

	// zGetUnifiedReceivers error.
	cl.queueResponse(methodZGetAddressForAccount, &zGetAddressForAccountResult{Address: tUnifiedAddr})
	cl.queueResponse(methodZListUnifiedReceivers, errTest)
	if _, err := w.DepositAddress(); err == nil {
		t.Fatalf("expected error from zGetUnifiedReceivers failure")
	}
}

func TestAddressUsed(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getreceivedbyaddress", uint64(0))
	used, err := w.AddressUsed(tAddr)
	if err != nil {
		t.Fatalf("AddressUsed error: %v", err)
	}
	if used {
		t.Fatalf("expected unused address")
	}

	cl.queueResponse("getreceivedbyaddress", uint64(100))
	used, err = w.AddressUsed(tAddr)
	if err != nil {
		t.Fatalf("AddressUsed error: %v", err)
	}
	if !used {
		t.Fatalf("expected used address")
	}
}

func TestOwnsDepositAddress(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsMine: true})
	owns, err := w.OwnsDepositAddress(tAddr)
	if err != nil {
		t.Fatalf("OwnsDepositAddress error: %v", err)
	}
	if !owns {
		t.Fatalf("expected owned address")
	}

	// Unified-prefixed address string.
	unified := depositAddrPrefix + `{"unified":"` + tAddr + `"}`
	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsMine: false})
	owns, err = w.OwnsDepositAddress(unified)
	if err != nil {
		t.Fatalf("OwnsDepositAddress (unified) error: %v", err)
	}
	if owns {
		t.Fatalf("expected not-owned")
	}

	// Bad JSON in the unified-prefixed address.
	if _, err := w.OwnsDepositAddress(depositAddrPrefix + "{bad json"); err == nil {
		t.Fatalf("expected JSON decode error")
	}

	// RPC error.
	cl.queueResponse(methodZValidateAddress, errTest)
	if _, err := w.OwnsDepositAddress(tAddr); err == nil {
		t.Fatalf("expected error from zValidateAddress failure")
	}
}

func TestFundOrchard(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZListUnspent, []*zListUnspentResult{
		{Account: shieldedAcctNumber, Confirmations: minOrchardConfs, Amount: 1},
		{Account: shieldedAcctNumber, Confirmations: minOrchardConfs, Amount: 2},
		{Account: shieldedAcctNumber, Confirmations: 0, Amount: 100},                   // excluded: not enough confs
		{Account: shieldedAcctNumber + 1, Confirmations: minOrchardConfs, Amount: 100}, // excluded: wrong account
	})
	sum, _, n, funded, err := w.fundOrchard(toSatoshi(2))
	if err != nil {
		t.Fatalf("fundOrchard error: %v", err)
	}
	if !funded {
		t.Fatalf("expected funded")
	}
	if n < 0 {
		t.Fatalf("unexpected n: %d", n)
	}
	if sum == 0 {
		t.Fatalf("expected nonzero sum")
	}

	// Not enough available.
	cl.queueResponse(methodZListUnspent, []*zListUnspentResult{
		{Account: shieldedAcctNumber, Confirmations: minOrchardConfs, Amount: 0.001},
	})
	_, _, _, funded, err = w.fundOrchard(toSatoshi(100))
	if err != nil {
		t.Fatalf("fundOrchard error: %v", err)
	}
	if funded {
		t.Fatalf("expected not funded")
	}

	// RPC error.
	cl.queueResponse(methodZListUnspent, errTest)
	if _, _, _, _, err := w.fundOrchard(toSatoshi(1)); err == nil {
		t.Fatalf("expected error from zListUnspent failure")
	}
}

func TestMaxWithdrawFees(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("listunspent", []*listUnspentResult{{TxID: tTxID}})
	cl.queueResponse(methodZGetNotesCount, &zNotesCount{Orchard: 2, Sapling: 1})
	fee, err := w.maxWithdrawFees(transparentAddressType)
	if err != nil {
		t.Fatalf("maxWithdrawFees error: %v", err)
	}
	if fee == 0 {
		t.Fatalf("expected nonzero fee")
	}

	cl.queueResponse("listunspent", []*listUnspentResult{{TxID: tTxID}})
	cl.queueResponse(methodZGetNotesCount, &zNotesCount{Orchard: 2, Sapling: 1})
	if _, err := w.maxWithdrawFees(unifiedAddressType); err != nil {
		t.Fatalf("maxWithdrawFees (unified) error: %v", err)
	}

	cl.queueResponse("listunspent", []*listUnspentResult{{TxID: tTxID}})
	cl.queueResponse(methodZGetNotesCount, &zNotesCount{Orchard: 2, Sapling: 1})
	if _, err := w.maxWithdrawFees(saplingAddressType); err != nil {
		t.Fatalf("maxWithdrawFees (sapling) error: %v", err)
	}

	// listunspent error.
	cl.queueResponse("listunspent", errTest)
	if _, err := w.maxWithdrawFees(transparentAddressType); err == nil {
		t.Fatalf("expected error from listunspent failure")
	}

	// zGetNotesCount error.
	cl.queueResponse("listunspent", []*listUnspentResult{{TxID: tTxID}})
	cl.queueResponse(methodZGetNotesCount, errTest)
	if _, err := w.maxWithdrawFees(transparentAddressType); err == nil {
		t.Fatalf("expected error from zGetNotesCount failure")
	}
}

func TestEstimateSendTxFeeMaxWithdraw(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsValid: true, AddressType: transparentAddressType})
	cl.queueResponse("listunspent", []*listUnspentResult{{TxID: tTxID}})
	cl.queueResponse(methodZGetNotesCount, &zNotesCount{})
	fee, isValid, err := w.EstimateSendTxFee(tAddr, 0, 0, false, true)
	if err != nil {
		t.Fatalf("EstimateSendTxFee error: %v", err)
	}
	if !isValid {
		t.Fatalf("expected valid address")
	}
	if fee == 0 {
		t.Fatalf("expected nonzero fee")
	}
}

func TestEstimateSendTxFeeOrchardFunded(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsValid: true, AddressType: transparentAddressType})
	cl.queueResponse(methodZListUnspent, []*zListUnspentResult{
		{Account: shieldedAcctNumber, Confirmations: minOrchardConfs, Amount: 10},
	})
	fee, isValid, err := w.EstimateSendTxFee(tAddr, toSatoshi(1), 0, false, false)
	if err != nil {
		t.Fatalf("EstimateSendTxFee error: %v", err)
	}
	if !isValid {
		t.Fatalf("expected valid address")
	}
	if fee == 0 {
		t.Fatalf("expected nonzero fee")
	}
}

func TestEstimateSendTxFeeNotOrchardFunded(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZValidateAddress, errTest)             // invalid address path, still proceeds
	cl.queueResponse(methodZListUnspent, []*zListUnspentResult{}) // orchard not funded
	resp := zAccountBalance{}
	resp.Pools.Sapling.ValueZat = 0
	cl.queueResponse(methodZGetBalanceForAccount, &resp)
	cl.queueResponse("listunspent", []*listUnspentResult{newTestListUnspentResult(tTxID, 0, 1, 1)})

	fee, isValid, err := w.EstimateSendTxFee(tAddr, toSatoshi(0.5), 0, false, false)
	if err != nil {
		t.Fatalf("EstimateSendTxFee error: %v", err)
	}
	if isValid {
		t.Fatalf("expected invalid address given zValidateAddress error")
	}
	if fee == 0 {
		t.Fatalf("expected nonzero fee")
	}
}

func TestBalancesAndBalance(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	zeroConf := zAccountBalance{}
	zeroConf.Pools.Orchard.ValueZat = 500
	zeroConf.Pools.Transparent.ValueZat = 200
	zeroConf.Pools.Sapling.ValueZat = 50
	mature := zAccountBalance{}
	mature.Pools.Orchard.ValueZat = 400
	mature.Pools.Transparent.ValueZat = 150

	cl.queueResponse(methodZGetBalanceForAccount, &zeroConf)
	cl.queueResponse(methodZGetBalanceForAccount, &mature)
	cl.queueResponse(methodZGetNotesCount, &zNotesCount{Orchard: 3})

	bals, err := w.balances()
	if err != nil {
		t.Fatalf("balances error: %v", err)
	}
	if bals.orchard.avail != 400 || bals.orchard.maturing != 100 {
		t.Fatalf("unexpected orchard breakdown: %+v", bals.orchard)
	}
	if bals.transparent.avail != 150 || bals.transparent.maturing != 50 {
		t.Fatalf("unexpected transparent breakdown: %+v", bals.transparent)
	}
	if bals.sapling != 50 {
		t.Fatalf("unexpected sapling balance: %d", bals.sapling)
	}

	// balances error propagation.
	cl.queueResponse(methodZGetBalanceForAccount, errTest)
	if _, err := w.balances(); err == nil {
		t.Fatalf("expected error from zGetBalanceForAccount failure")
	}

	// Full Balance() call.
	cl.queueResponse(methodZGetBalanceForAccount, &zeroConf)
	cl.queueResponse(methodZGetBalanceForAccount, &mature)
	cl.queueResponse(methodZGetNotesCount, &zNotesCount{Orchard: 3})
	cl.queueResponse("listlockunspent", []*rpcOutpoint{})
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance error: %v", err)
	}
	if bal.Available != bals.orchard.avail+bals.transparent.avail {
		t.Fatalf("unexpected available balance: %d", bal.Available)
	}

	// Balance() propagates balances() error with errBalanceRetrieval code.
	cl.queueResponse(methodZGetBalanceForAccount, errTest)
	if _, err := w.Balance(); !errorHasCode(err, errBalanceRetrieval) {
		t.Fatalf("expected errBalanceRetrieval, got %v", err)
	}
}

func TestLockedZats(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	// No locked outputs.
	cl.queueResponse("listlockunspent", []*rpcOutpoint{})
	sum, err := w.lockedZats()
	if err != nil {
		t.Fatalf("lockedZats error: %v", err)
	}
	if sum != 0 {
		t.Fatalf("expected zero sum, got %d", sum)
	}

	// One locked output, resolved via cache.
	pt := btc.NewOutPoint(tTxHash, 0)
	w.cm.LockUTXOs([]*btc.UTxO{{TxHash: tTxHash, Vout: 0, Amount: toSatoshi(1)}})
	cl.queueResponse("listlockunspent", []*rpcOutpoint{{TxID: tTxHash.String(), Vout: 0}})
	cl.queueResponse("gettxout", &btcjson.GetTxOutResult{Confirmations: 1})
	sum, err = w.lockedZats()
	if err != nil {
		t.Fatalf("lockedZats (cached) error: %v", err)
	}
	if sum != toSatoshi(1) {
		t.Fatalf("wrong sum: %d", sum)
	}
	_ = pt

	// listlockunspent error.
	cl.queueResponse("listlockunspent", errTest)
	if _, err := w.lockedZats(); err == nil {
		t.Fatalf("expected error from listlockunspent failure")
	}
}

func TestTransactionConfirmationsHelper(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("gettransaction", &GetTransactionResult{Confirmations: 5})
	confs, err := w.TransactionConfirmations(tCtx, tTxID)
	if err != nil {
		t.Fatalf("TransactionConfirmations error: %v", err)
	}
	if confs != 5 {
		t.Fatalf("wrong confs: %d", confs)
	}

	// Negative confirmations clamp to zero.
	cl.queueResponse("gettransaction", &GetTransactionResult{Confirmations: -1})
	confs, err = w.TransactionConfirmations(tCtx, tTxID)
	if err != nil {
		t.Fatalf("TransactionConfirmations (negative) error: %v", err)
	}
	if confs != 0 {
		t.Fatalf("expected clamped confs of 0, got %d", confs)
	}

	// Bad txid.
	if _, err := w.TransactionConfirmations(tCtx, "not-a-txid"); err == nil {
		t.Fatalf("expected error for bad txid")
	}
}

func TestListSinceBlockMethod(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse("getblockhash", tTxHash.String())
	cl.queueResponse("listsinceblock", &listSinceBlockRes{})
	if _, err := w.listSinceBlock(10); err != nil {
		t.Fatalf("listSinceBlock error: %v", err)
	}

	// getblockhash error.
	cl.queueResponse("getblockhash", errTest)
	if _, err := w.listSinceBlock(10); err == nil {
		t.Fatalf("expected error from getblockhash failure")
	}
}

func TestCheckPeersAndMonitorPeers(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	var gotCount uint32
	var gotErr error
	w.peersChange = func(n uint32, err error) {
		gotCount = n
		gotErr = err
	}

	cl.queueResponse("getnetworkinfo", &networkInfo{Connections: 4})
	w.checkPeers()
	if gotCount != 4 || gotErr != nil {
		t.Fatalf("unexpected peersChange call: count=%d err=%v", gotCount, gotErr)
	}

	// Same peer count again: no callback invoked (queueEmptiness would fail
	// otherwise since we don't queue another response).
	w.checkPeers()

	cl.queueResponse("getnetworkinfo", errTest)
	w.checkPeers()
	if gotCount != 0 || gotErr == nil {
		t.Fatalf("expected error callback on rpc failure")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cl.queueResponse("getnetworkinfo", &networkInfo{Connections: 1})
	done := make(chan struct{})
	go func() {
		w.monitorPeers(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("monitorPeers did not return after cancellation")
	}
}

func TestReportNewTipMethod(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	newTip := &blockVector{Hash: *tTxHash, Height: w.currentTip.Height + 1}

	// rf.ReportNewTip looks up the previous tip's header to determine the
	// search starting point; with no pending redemption requests, no further
	// RPC calls are made.
	cl.queueResponse("getblockheader", &btc.BlockHeader{Height: w.currentTip.Height, Confirmations: 1})

	w.reportNewTip(tCtx, newTip)

	if w.currentTip.Height != newTip.Height {
		t.Fatalf("expected currentTip to be updated")
	}
}

func TestWalletInfo(t *testing.T) {
	w, _, shutdown := tNewWallet()
	defer shutdown()

	if w.Info() != WalletInfo {
		t.Fatalf("Info() did not return the package WalletInfo")
	}
}

func TestSyncTxHistoryNoDB(t *testing.T) {
	w, _, shutdown := tNewWallet()
	defer shutdown()

	// No netTxDB configured: should return promptly without panicking.
	w.syncTxHistory(0)
}

func TestStartTxHistoryDBNoDB(t *testing.T) {
	w, _, shutdown := tNewWallet()
	defer shutdown()

	wg, err := w.startTxHistoryDB(tCtx)
	if err != nil {
		t.Fatalf("startTxHistoryDB error: %v", err)
	}
	if wg == nil {
		t.Fatalf("expected non-nil waitgroup")
	}
}
