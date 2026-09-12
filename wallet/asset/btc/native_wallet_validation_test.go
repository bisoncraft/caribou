//go:build !harness

package btc

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

func newOfflineNativeWallet(t *testing.T) *NativeWallet {
	t.Helper()
	cc := btcElectrumChainConfig(&chaincfg.RegressionNetParams)
	dir := t.TempDir()
	seed, err := hdkeychain.GenerateSeed(hdkeychain.RecommendedSeedLen)
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateElectrumWallet(cc, []byte("pass"), seed, time.Now(), dir, tLogger, 0, 0, wallet.WithScryptOptions(&waddrmgr.FastScryptOptions)); err != nil {
		t.Fatal(err)
	}
	loader := wallet.NewLoader(cc.NetParams, dir, true, dbTimeout, 0)
	btcw, err := loader.OpenExistingWallet([]byte(wallet.InsecurePubPassphrase))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := loader.UnloadWallet(); err != nil {
			t.Error(err)
		}
	})
	w := NewNativeWallet(cc, &asset.WalletConfig{}, tLogger)
	w.btcw = btcw
	return w
}

func TestValidateAddressRejectsWrongNetwork(t *testing.T) {
	w := NewNativeWallet(btcElectrumChainConfig(&chaincfg.TestNet3Params), &asset.WalletConfig{}, tLogger)
	addr, err := btcutil.NewAddressPubKeyHash(make([]byte, 20), &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	if w.ValidateAddress(addr.EncodeAddress()) {
		t.Fatalf("accepted mainnet address %q on testnet", addr)
	}
}

func TestWalletTransactionDisconnected(t *testing.T) {
	w := NewNativeWallet(btcElectrumChainConfig(&chaincfg.MainNetParams), &asset.WalletConfig{}, tLogger)
	_, err := w.WalletTransaction(tCtx, tTxID)
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("WalletTransaction error = %v, want disconnected-wallet error", err)
	}
}

func TestNativeWalletDisconnected(t *testing.T) {
	w := NewNativeWallet(btcElectrumChainConfig(&chaincfg.MainNetParams), &asset.WalletConfig{}, tLogger)

	if !w.Locked() {
		t.Fatal("disconnected wallet reported unlocked")
	}
	if peers, err := w.Peers(); err != nil || peers != nil {
		t.Fatalf("Peers = %v, %v", peers, err)
	}
	if status, err := w.SyncStatus(); err != nil || status.Synced || status.Blocks != 0 {
		t.Fatalf("SyncStatus = %+v, %v", status, err)
	}
	if w.FeeRate() != 0 {
		t.Fatal("disconnected wallet returned a fee rate")
	}
	if got := w.PendingTransactions(tCtx); len(got) != 0 {
		t.Fatalf("PendingTransactions = %v", got)
	}

	for name, f := range map[string]func() error{
		"Unlock":     func() error { return w.Unlock(nil) },
		"Lock":       w.Lock,
		"AddPeer":    func() error { return w.AddPeer("127.0.0.1") },
		"RemovePeer": func() error { return w.RemovePeer("127.0.0.1") },
		"Rescan":     func() error { return w.Rescan(tCtx, 0) },
		"Balance": func() error {
			_, err := w.Balance()
			return err
		},
		"DepositAddress": func() error {
			_, err := w.DepositAddress()
			return err
		},
		"OwnsDepositAddress": func() error {
			_, err := w.OwnsDepositAddress("")
			return err
		},
		"Send": func() error {
			_, err := w.Send("", 1, 1)
			return err
		},
		"EstimateSendTxFee": func() error {
			_, _, err := w.EstimateSendTxFee("", 1, 1, false, false)
			return err
		},
		"TxHistory": func() error {
			_, err := w.TxHistory(nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := f(); err == nil {
				t.Fatal("expected disconnected-wallet error")
			}
		})
	}
}

func TestOfflineNativeWallet(t *testing.T) {
	w := newOfflineNativeWallet(t)
	if err := extendAddresses(2, 2, w.btcw); err != nil {
		t.Fatalf("extendAddresses: %v", err)
	}
	if !w.Locked() {
		t.Fatal("new wallet is unlocked")
	}
	if err := w.Unlock([]byte("pass")); err != nil {
		t.Fatal(err)
	}
	if w.Locked() {
		t.Fatal("wallet remained locked")
	}
	if err := w.Lock(); err != nil {
		t.Fatal(err)
	}
	bal, err := w.Balance()
	if err != nil || bal.Available != 0 || bal.Immature != 0 {
		t.Fatalf("Balance = %+v, %v", bal, err)
	}
	if _, err := w.WalletTransaction(tCtx, tTxID); err != asset.CoinNotFoundError {
		t.Fatalf("WalletTransaction error = %v, want CoinNotFoundError", err)
	}
	if _, err := w.WalletTransaction(tCtx, "not-a-txid"); err == nil {
		t.Fatal("expected invalid txid error")
	}
	coinID := ToCoinID(tTxHash, 3)
	if !bytes.Equal(coinID[:32], tTxHash[:]) {
		t.Fatal("coin ID does not retain transaction hash")
	}
}

func TestForceRescanOfflineWallet(t *testing.T) {
	w := newOfflineNativeWallet(t)
	w.btcw.Stop()
	w.btcw.WaitForShutdown()
	if err := w.forceRescan(); err != nil {
		t.Fatalf("forceRescan: %v", err)
	}
}

func TestOfflineWalletStatusAndFeeRate(t *testing.T) {
	w := newOfflineNativeWallet(t)
	if status, err := w.SyncStatus(); err != nil || status.Synced || status.Blocks != 0 {
		t.Fatalf("SyncStatus = %+v, %v", status, err)
	}
	if _, err := w.DepositAddress(); err == nil {
		t.Fatal("expected inactive-backend address error")
	}
}

func TestScanNewTransactionsOfflineWallet(t *testing.T) {
	w := newOfflineNativeWallet(t)
	master, err := txdb.New(t.TempDir()+"/tx.db", tLogger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	w.txdb = master.NetworkTxDB(BipID)
	notes := make(chan asset.WalletNotification, 1)
	w.emit = asset.NewWalletEmitter(notes, BipID, tLogger)
	w.scanNewTransactions(tCtx, 1)
	if w.receiveTxLastQuery.Load() != 1 {
		t.Fatalf("last query = %d", w.receiveTxLastQuery.Load())
	}
}

func TestNativeWalletLoopsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(tCtx)
	cancel()
	w := NewNativeWallet(btcElectrumChainConfig(&chaincfg.MainNetParams), &asset.WalletConfig{PeersChange: func(uint32, error) {}}, tLogger)
	w.pendingTxLoop(ctx)
	w.peerLoop(ctx)
	w.syncTxHistory(ctx, 1)
}

func TestBlockLoopCancel(t *testing.T) {
	w := newOfflineNativeWallet(t)
	ctx, cancel := context.WithCancel(tCtx)
	cancel()
	w.blockLoop(ctx)
}

func TestOfflineConnectedHelpers(t *testing.T) {
	w := newOfflineNativeWallet(t)
	w.cfg.FeeRateSources = nil
	w.feeFetcher = nil
	w.ctx.Store(tCtx)
	if rate := w.FeeRate(); rate != 0 {
		t.Fatalf("offline fee rate = %d", rate)
	}
	if peers, err := w.Peers(); err != nil || len(peers) != 0 {
		t.Fatalf("Peers = %v, %v", peers, err)
	}
	if hash, height := w.bestBlock(); height != 0 || *hash != (chainhash.Hash{}) {
		t.Fatalf("best block = %v, %d", hash, height)
	}
}

func TestOfflineRescanUnsupported(t *testing.T) {
	w := newOfflineNativeWallet(t)
	if err := w.Rescan(tCtx, 0); err == nil {
		t.Fatal("expected rescan error without an active backend")
	}
}

func TestPendingTxLoopReconciles(t *testing.T) {
	old := pendingTxLoopInterval
	pendingTxLoopInterval = 5 * time.Millisecond
	t.Cleanup(func() { pendingTxLoopInterval = old })

	w := newOfflineNativeWallet(t)
	w.pendingTxs[*tTxHash] = txdb.WalletTransaction{ID: tTxID, Confirms: &txdb.Confirms{Target: confTxFinality}}
	ctx, cancel := context.WithTimeout(tCtx, 50*time.Millisecond)
	defer cancel()
	w.pendingTxLoop(ctx)
	if len(w.PendingTransactions(tCtx)) != 1 {
		t.Fatal("missing transaction was removed from pending state")
	}
}

func TestOfflinePeerOperations(t *testing.T) {
	w := newOfflineNativeWallet(t)
	if err := w.AddPeer("127.0.0.1:1"); err == nil {
		t.Fatal("expected inactive-backend add-peer error")
	}
	if err := w.RemovePeer("127.0.0.1:1"); err == nil {
		t.Fatal("expected inactive-backend remove-peer error")
	}
}

func TestOfflineSendValidation(t *testing.T) {
	w := newOfflineNativeWallet(t)
	if _, err := w.Send("", 1, 0); err == nil {
		t.Fatal("expected zero fee-rate error")
	}
	if _, err := w.Send("not-an-address", 1, 1); err == nil {
		t.Fatal("expected invalid address error")
	}
	addr, err := btcutil.NewAddressPubKeyHash(make([]byte, 20), &chaincfg.RegressionNetParams)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Send(addr.EncodeAddress(), 1, 1); err == nil {
		t.Fatal("expected inactive-backend send error")
	}
	if _, _, err := w.EstimateSendTxFee(addr.EncodeAddress(), 0, 1, false, false); err == nil {
		t.Fatal("expected zero value error")
	}
	if _, _, err := w.EstimateSendTxFee(addr.EncodeAddress(), 1, 0, false, false); err == nil {
		t.Fatal("expected zero fee-rate error")
	}
	if _, valid, err := w.EstimateSendTxFee("not-an-address", 1, 1, false, false); err == nil || valid {
		t.Fatalf("invalid address estimate = valid %v, err %v", valid, err)
	}
	if _, valid, err := w.EstimateSendTxFee(addr.EncodeAddress(), 1, 1, true, false); err == nil || !valid {
		t.Fatalf("subtract estimate = valid %v, err %v", valid, err)
	}
	if _, valid, err := w.EstimateSendTxFee(addr.EncodeAddress(), 1, 1, false, true); err == nil || !valid {
		t.Fatalf("max-withdraw estimate = valid %v, err %v", valid, err)
	}
}

func TestHandleUnminedTxChangeOffline(t *testing.T) {
	w := newOfflineNativeWallet(t)
	master, err := txdb.New(t.TempDir()+"/tx.db", tLogger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	w.txdb = master.NetworkTxDB(BipID)
	w.emit = asset.NewWalletEmitter(make(chan asset.WalletNotification, 1), BipID, tLogger)
	w.handleUnminedTxChange(tCtx)
}

func TestRecordBtcjsonTx(t *testing.T) {
	w := NewNativeWallet(btcElectrumChainConfig(&chaincfg.MainNetParams), &asset.WalletConfig{}, tLogger)
	notes := make(chan asset.WalletNotification, 2)
	w.emit = asset.NewWalletEmitter(notes, BipID, tLogger)
	master, err := txdb.New(t.TempDir()+"/tx.db", tLogger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	w.txdb = master.NetworkTxDB(BipID)

	fee := -0.00001
	height := int32(12)
	w.recordBtcjsonTx(tCtx, &btcjson.ListTransactionsResult{
		TxID:        tTxID,
		Category:    "send",
		Amount:      -0.29,
		Fee:         &fee,
		Address:     "recipient",
		BlockHeight: &height,
		BlockTime:   100,
	}, 20, true)

	got, err := w.txdb.GetTx(tTxID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != txdb.Send || got.Amount != 29_000_000 || got.Fees != 1_000 || !got.Confirmed || got.BlockNumber != uint64(height) {
		t.Fatalf("unexpected transaction: %+v", got)
	}

	// A duplicate record must leave the stored transaction unchanged.
	w.recordBtcjsonTx(tCtx, &btcjson.ListTransactionsResult{TxID: tTxID, Amount: 1}, 20, true)
	again, err := w.txdb.GetTx(tTxID)
	if err != nil || again.Amount != got.Amount {
		t.Fatalf("duplicate record = %+v, %v", again, err)
	}

	const pendingTxID = "1111111111111111111111111111111111111111111111111111111111111111"
	nearTip := int32(19)
	w.recordBtcjsonTx(tCtx, &btcjson.ListTransactionsResult{
		TxID: pendingTxID, Amount: 0.07, BlockHeight: &nearTip,
	}, 20, true)
	pending, err := w.txdb.GetTx(pendingTxID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Confirmed || pending.BlockNumber != 0 || pending.Confirms == nil || len(w.PendingTransactions(tCtx)) != 1 {
		t.Fatalf("near-tip transaction was not pending: %+v", pending)
	}

	const receiveTxID = "2222222222222222222222222222222222222222222222222222222222222222"
	w.recordBtcjsonTx(tCtx, &btcjson.ListTransactionsResult{TxID: receiveTxID, Amount: 0.01, Address: "deposit"}, 20, false)
	receive, err := w.txdb.GetTx(receiveTxID)
	if err != nil || receive.Type != txdb.Receive || receive.Amount != 1_000_000 || receive.Recipient == nil || *receive.Recipient != "deposit" {
		t.Fatalf("receive transaction = %+v, %v", receive, err)
	}
	select {
	case <-notes:
	default:
		t.Fatal("expected transaction notification")
	}
}

func TestWalletTransactionFromLocalHistory(t *testing.T) {
	w := NewNativeWallet(btcElectrumChainConfig(&chaincfg.MainNetParams), &asset.WalletConfig{}, tLogger)
	master, err := txdb.New(t.TempDir()+"/tx.db", tLogger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	w.txdb = master.NetworkTxDB(BipID)
	want := &txdb.WalletTransaction{ID: tTxID, NetworkID: BipID, AssetID: BipID, Confirmed: true, BlockNumber: 1}
	if err := w.txdb.StoreTx(want); err != nil {
		t.Fatal(err)
	}

	got, err := w.WalletTransaction(tCtx, tTxID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || !got.Confirmed || got.BlockNumber != want.BlockNumber {
		t.Fatalf("WalletTransaction = %+v", got)
	}
}

func TestStandardSendFee(t *testing.T) {
	segwit := NewNativeWallet(btcElectrumChainConfig(&chaincfg.MainNetParams), &asset.WalletConfig{}, tLogger)
	if segwit.StandardSendFee(2) == 0 {
		t.Fatal("segwit fee is zero")
	}
}

func TestStorePendingTx(t *testing.T) {
	w := NewNativeWallet(btcElectrumChainConfig(&chaincfg.MainNetParams), &asset.WalletConfig{}, tLogger)
	master, err := txdb.New(t.TempDir()+"/tx.db", tLogger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	w.txdb = master.NetworkTxDB(BipID)
	notes := make(chan asset.WalletNotification, 1)
	w.emit = asset.NewWalletEmitter(notes, BipID, tLogger)
	wt := &txdb.WalletTransaction{ID: tTxID, NetworkID: BipID, AssetID: BipID, Amount: 7, Confirms: &txdb.Confirms{Target: confTxFinality}}
	w.storePendingTx(tTxHash, wt)
	stored, err := w.txdb.GetTx(tTxID)
	if err != nil || stored.Amount != wt.Amount || len(w.PendingTransactions(tCtx)) != 1 {
		t.Fatalf("stored pending transaction = %+v, %v", stored, err)
	}
	select {
	case <-notes:
	default:
		t.Fatal("expected pending transaction notification")
	}
}

func TestUpdatePendingTransactionMissingDetails(t *testing.T) {
	w := newOfflineNativeWallet(t)
	master, err := txdb.New(t.TempDir()+"/tx.db", tLogger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	w.txdb = master.NetworkTxDB(BipID)
	w.pendingTxs[*tTxHash] = txdb.WalletTransaction{ID: tTxID, NetworkID: BipID, AssetID: BipID, Confirms: &txdb.Confirms{Target: confTxFinality}}
	w.updatePendingTransactions(tCtx, 100)
	if len(w.PendingTransactions(tCtx)) != 1 {
		t.Fatal("missing wallet transaction was removed from pending state")
	}
}

func TestNativeWalletConstructionAndConnectFailure(t *testing.T) {
	cfg := &asset.WalletConfig{DataDir: t.TempDir()}
	w := openElectrumWallet(cfg, &chaincfg.MainNetParams, tLogger)
	if w.Info() != WalletInfo {
		t.Fatal("wrong wallet info")
	}
	if hash, height := w.bestBlock(); height != 0 || *hash != (chainhash.Hash{}) {
		t.Fatalf("best block = %v, %d", hash, height)
	}
	if _, err := w.Connect(tCtx); err == nil {
		t.Fatal("expected missing wallet error")
	}
	if openSPVWallet(cfg, &chaincfg.MainNetParams, tLogger).cfg.BackendKind != wallet.BackendNeutrino {
		t.Fatal("SPV wallet did not select neutrino")
	}
}
