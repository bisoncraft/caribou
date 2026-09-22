//go:build !harness && !rpclive

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func finTransferLog(t *testing.T, tokenAddress, recipient common.Address, amt *big.Int) *types.Log {
	t.Helper()
	data, err := omniFinTransferArgs.Pack(tokenAddress, amt, recipient, "fee.near")
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return &types.Log{
		Topics: []common.Hash{
			omniFinTransferTopic,
			{}, // originChain (indexed)
			{}, // originNonce (indexed)
		},
		Data: data,
	}
}

func TestParseNativeBridgePayout(t *testing.T) {
	us := common.HexToAddress("0x1000000000000000000000000000000000000001")
	them := common.HexToAddress("0x2000000000000000000000000000000000000002")
	token := common.HexToAddress("0x3000000000000000000000000000000000000003")
	wei := big.NewInt(1e15)

	receipt := func(logs ...*types.Log) *types.Receipt { return &types.Receipt{Logs: logs} }

	if got, ok := parseNativeBridgePayout(receipt(finTransferLog(t, common.Address{}, us, wei)), us); !ok || got.Cmp(wei) != 0 {
		t.Fatalf("native payout to us: got %v, %v", got, ok)
	}

	// Two payouts in one tx sum.
	if got, ok := parseNativeBridgePayout(receipt(
		finTransferLog(t, common.Address{}, us, wei),
		finTransferLog(t, common.Address{}, us, wei),
	), us); !ok || got.Cmp(new(big.Int).Add(wei, wei)) != 0 {
		t.Fatalf("summed payouts: got %v, %v", got, ok)
	}

	// A payout to someone else is not ours.
	if _, ok := parseNativeBridgePayout(receipt(finTransferLog(t, common.Address{}, them, wei)), us); ok {
		t.Fatal("attributed a payout to another recipient")
	}

	// A token payout is delivered by the token contract, not this event.
	if _, ok := parseNativeBridgePayout(receipt(finTransferLog(t, token, us, wei)), us); ok {
		t.Fatal("attributed a token payout as native")
	}

	// An unrelated event is ignored.
	unrelated := finTransferLog(t, common.Address{}, us, wei)
	unrelated.Topics[0] = common.HexToHash("0xdeadbeef")
	if _, ok := parseNativeBridgePayout(receipt(unrelated), us); ok {
		t.Fatal("attributed an unrelated event")
	}
}

// TestIncomingTxBaseUnsettled guards the provider states that arrive with a
// nil error: a nil receipt, and a receipt not yet bound to a block. Both must
// read as not-found — the recording retry loop keys on CoinNotFoundError —
// rather than dereferencing nil in a background goroutine.
func TestIncomingTxBaseUnsettled(t *testing.T) {
	w := &assetWallet{}
	if _, err := w.incomingTxBase(common.Hash{}, nil, 0); !errors.Is(err, asset.CoinNotFoundError) {
		t.Fatalf("nil receipt: err = %v, want CoinNotFoundError", err)
	}
	unbound := &types.Receipt{Status: types.ReceiptStatusSuccessful}
	if _, err := w.incomingTxBase(common.Hash{}, unbound, 0); !errors.Is(err, asset.CoinNotFoundError) {
		t.Fatalf("nil block number: err = %v, want CoinNotFoundError", err)
	}
}

func TestIncomingTxBaseFailedStatus(t *testing.T) {
	w := &assetWallet{}
	receipt := &types.Receipt{Status: types.ReceiptStatusFailed, BlockNumber: big.NewInt(1)}
	if _, err := w.incomingTxBase(common.Hash{}, receipt, 0); err == nil {
		t.Fatal("expected an error for a failed transaction")
	}
}

func TestIncomingTxBaseSuccess(t *testing.T) {
	addr := common.HexToAddress("0x1000000000000000000000000000000000000001")
	w := &assetWallet{
		baseWallet: &baseWallet{addr: addr, baseChainID: BipID},
		assetID:    BipID,
	}
	txHash := common.HexToHash("0xa000000000000000000000000000000000000000000000000000000000000001")
	receipt := &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(100)}

	wt, err := w.incomingTxBase(txHash, receipt, 7)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Type != txdb.Receive {
		t.Errorf("Type = %v, want Receive", wt.Type)
	}
	if wt.ID != txHash.String() {
		t.Errorf("ID = %q, want %q", wt.ID, txHash.String())
	}
	if wt.Recipient == nil || *wt.Recipient != addr.String() {
		t.Errorf("Recipient = %v, want %q", wt.Recipient, addr.String())
	}
	if wt.BlockNumber != 100 {
		t.Errorf("BlockNumber = %d, want 100", wt.BlockNumber)
	}
	if !wt.Confirmed {
		t.Error("Confirmed = false, want true")
	}
	if wt.AdditionalData[txHistoryNonceKey] != "7" {
		t.Errorf("AdditionalData[%s] = %q, want \"7\"", txHistoryNonceKey, wt.AdditionalData[txHistoryNonceKey])
	}
}

func TestETHWalletFetchIncomingTx(t *testing.T) {
	txHash := common.HexToHash("0xb000000000000000000000000000000000000000000000000000000000000001")

	t.Run("no transaction is not-found", func(t *testing.T) {
		w, _, node, cancel := tassetWallet(BipID)
		defer cancel()
		ew := w.(*ETHWallet)
		// A receipt with no matching tx: testNode returns tx == nil, receipt
		// != nil, err == nil.
		node.receipts[txHash] = &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(1)}
		if _, err := ew.fetchIncomingTx(context.Background(), txHash, 0); !errors.Is(err, asset.CoinNotFoundError) {
			t.Fatalf("err = %v, want CoinNotFoundError", err)
		}
	})

	t.Run("incomingTxBase error propagates", func(t *testing.T) {
		w, _, node, cancel := tassetWallet(BipID)
		defer cancel()
		ew := w.(*ETHWallet)
		node.receipts[txHash] = &types.Receipt{Status: types.ReceiptStatusFailed, BlockNumber: big.NewInt(1)}
		node.receiptTxs[txHash] = tTx(0, 0, 0, nil, nil, 21000)
		if _, err := ew.fetchIncomingTx(context.Background(), txHash, 0); err == nil {
			t.Fatal("expected an error for a failed transaction")
		}
	})

	t.Run("direct transfer credits tx.Value", func(t *testing.T) {
		w, aw, node, cancel := tassetWallet(BipID)
		defer cancel()
		ew := w.(*ETHWallet)
		to := aw.addr
		node.receipts[txHash] = &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(1)}
		node.receiptTxs[txHash] = tTx(0, 0, 5, &to, nil, 21000)

		wt, err := ew.fetchIncomingTx(context.Background(), txHash, 0)
		if err != nil {
			t.Fatal(err)
		}
		if wt.Amount != 5 {
			t.Errorf("Amount = %d, want 5", wt.Amount)
		}
		if _, marked := wt.AdditionalData[txHistoryAmountSourceKey]; marked {
			t.Error("a direct transfer should not be marked as a reported amount")
		}
	})

	t.Run("bridge payout is credited when tx.To is not us", func(t *testing.T) {
		w, aw, node, cancel := tassetWallet(BipID)
		defer cancel()
		ew := w.(*ETHWallet)
		bridgeAddr := common.HexToAddress("0x9000000000000000000000000000000000000009")
		wei := big.NewInt(3_000_000_000) // 3 gwei
		node.receipts[txHash] = &types.Receipt{
			Status:      types.ReceiptStatusSuccessful,
			BlockNumber: big.NewInt(1),
			Logs:        []*types.Log{finTransferLog(t, common.Address{}, aw.addr, wei)},
		}
		node.receiptTxs[txHash] = tTx(0, 0, 0, &bridgeAddr, nil, 21000)

		wt, err := ew.fetchIncomingTx(context.Background(), txHash, 0)
		if err != nil {
			t.Fatal(err)
		}
		if wt.Amount != 3 {
			t.Errorf("Amount = %d, want 3", wt.Amount)
		}
	})

	t.Run("falls back to the reported amtHint", func(t *testing.T) {
		w, _, node, cancel := tassetWallet(BipID)
		defer cancel()
		ew := w.(*ETHWallet)
		other := common.HexToAddress("0x8000000000000000000000000000000000000008")
		node.receipts[txHash] = &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(1)}
		node.receiptTxs[txHash] = tTx(0, 0, 0, &other, nil, 21000)

		wt, err := ew.fetchIncomingTx(context.Background(), txHash, 42)
		if err != nil {
			t.Fatal(err)
		}
		if wt.Amount != 42 {
			t.Errorf("Amount = %d, want 42", wt.Amount)
		}
		if wt.AdditionalData[txHistoryAmountSourceKey] != "reported" {
			t.Errorf("AdditionalData[%s] = %q, want %q", txHistoryAmountSourceKey, wt.AdditionalData[txHistoryAmountSourceKey], "reported")
		}
	})

	t.Run("no attributable payment errors", func(t *testing.T) {
		w, _, node, cancel := tassetWallet(BipID)
		defer cancel()
		ew := w.(*ETHWallet)
		other := common.HexToAddress("0x7000000000000000000000000000000000000007")
		node.receipts[txHash] = &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(1)}
		node.receiptTxs[txHash] = tTx(0, 0, 0, &other, nil, 21000)

		if _, err := ew.fetchIncomingTx(context.Background(), txHash, 0); err == nil {
			t.Fatal("expected an error when nothing attributes a payment")
		}
	})
}

func TestTokenWalletFetchIncomingTx(t *testing.T) {
	txHash := common.HexToHash("0xc000000000000000000000000000000000000000000000000000000000000001")

	t.Run("transfer log value is used", func(t *testing.T) {
		w, _, node, cancel := tassetWallet(usdcEthID)
		defer cancel()
		tw := w.(*TokenWallet)
		node.receipts[txHash] = &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(1)}
		node.receiptTxs[txHash] = tTx(0, 0, 0, nil, nil, 21000)
		node.tokenContractor.parseTransferAmt = 7

		wt, err := tw.fetchIncomingTx(context.Background(), txHash, 0)
		if err != nil {
			t.Fatal(err)
		}
		if wt.Amount != 7 {
			t.Errorf("Amount = %d, want 7", wt.Amount)
		}
	})

	t.Run("falls back to amtHint when no transfer log is found", func(t *testing.T) {
		w, _, node, cancel := tassetWallet(usdcEthID)
		defer cancel()
		tw := w.(*TokenWallet)
		node.receipts[txHash] = &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(1)}
		node.receiptTxs[txHash] = tTx(0, 0, 0, nil, nil, 21000)
		node.tokenContractor.parseTransferErr = errors.New("no transfer log")

		wt, err := tw.fetchIncomingTx(context.Background(), txHash, 9)
		if err != nil {
			t.Fatal(err)
		}
		if wt.Amount != 9 {
			t.Errorf("Amount = %d, want 9", wt.Amount)
		}
		if wt.AdditionalData[txHistoryAmountSourceKey] != "reported" {
			t.Errorf("AdditionalData[%s] = %q, want %q", txHistoryAmountSourceKey, wt.AdditionalData[txHistoryAmountSourceKey], "reported")
		}
	})

	t.Run("no attribution errors", func(t *testing.T) {
		w, _, node, cancel := tassetWallet(usdcEthID)
		defer cancel()
		tw := w.(*TokenWallet)
		node.receipts[txHash] = &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(1)}
		node.receiptTxs[txHash] = tTx(0, 0, 0, nil, nil, 21000)
		node.tokenContractor.parseTransferErr = errors.New("no transfer log")

		if _, err := tw.fetchIncomingTx(context.Background(), txHash, 0); err == nil {
			t.Fatal("expected an error when nothing attributes a payment")
		}
	})
}

func TestRecordIncomingTx(t *testing.T) {
	txHash := common.HexToHash("0xd000000000000000000000000000000000000000000000000000000000000001")

	t.Run("already recorded is a no-op", func(t *testing.T) {
		_, aw, _, cancel := tassetWallet(BipID)
		defer cancel()
		aw.txDB.(*tTxDB).txToGet = &extendedWalletTx{}

		var called bool
		err := aw.recordIncomingTx(txHash, func() (*txdb.WalletTransaction, error) {
			called = true
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if called {
			t.Error("fetch should not be called when the tx is already recorded")
		}
	})

	t.Run("fetch error propagates", func(t *testing.T) {
		_, aw, _, cancel := tassetWallet(BipID)
		defer cancel()
		wantErr := errors.New("boom")
		err := aw.recordIncomingTx(txHash, func() (*txdb.WalletTransaction, error) {
			return nil, wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
	})

	t.Run("success stores and stamps a timestamp", func(t *testing.T) {
		_, aw, _, cancel := tassetWallet(BipID)
		defer cancel()
		wt := &txdb.WalletTransaction{ID: txHash.String(), Amount: 5, AdditionalData: map[string]string{}}
		err := aw.recordIncomingTx(txHash, func() (*txdb.WalletTransaction, error) {
			return wt, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tdb := aw.txDB.(*tTxDB)
		if !tdb.storeTxCalled {
			t.Fatal("storeTx was not called")
		}
		if tdb.storedTx.txHash != txHash {
			t.Errorf("stored txHash = %s, want %s", tdb.storedTx.txHash, txHash)
		}
		if tdb.storedTx.Timestamp == 0 {
			t.Error("expected a non-zero timestamp to be stamped")
		}
	})

	t.Run("store failure errors", func(t *testing.T) {
		_, aw, _, cancel := tassetWallet(BipID)
		defer cancel()
		aw.txDB.(*tTxDB).storeTxErr = errors.New("disk full")
		wt := &txdb.WalletTransaction{AdditionalData: map[string]string{}}
		err := aw.recordIncomingTx(txHash, func() (*txdb.WalletTransaction, error) {
			return wt, nil
		})
		if err == nil {
			t.Fatal("expected an error when storeTx fails")
		}
	})
}

func TestETHWalletRecordIncomingTx(t *testing.T) {
	w, aw, node, cancel := tassetWallet(BipID)
	defer cancel()
	ew := w.(*ETHWallet)
	txHash := common.HexToHash("0xe000000000000000000000000000000000000000000000000000000000000001")
	to := aw.addr
	node.receipts[txHash] = &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(1)}
	node.receiptTxs[txHash] = tTx(0, 0, 4, &to, nil, 21000)

	if err := ew.RecordIncomingTx(context.Background(), txHash.Hex(), 0); err != nil {
		t.Fatal(err)
	}
	tdb := aw.txDB.(*tTxDB)
	if !tdb.storeTxCalled {
		t.Fatal("expected the incoming tx to be stored")
	}
	if tdb.storedTx.Amount != 4 {
		t.Errorf("stored Amount = %d, want 4", tdb.storedTx.Amount)
	}
}

func TestTokenWalletRecordIncomingTx(t *testing.T) {
	w, aw, node, cancel := tassetWallet(usdcEthID)
	defer cancel()
	tw := w.(*TokenWallet)
	txHash := common.HexToHash("0xf000000000000000000000000000000000000000000000000000000000000001")
	node.receipts[txHash] = &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(1)}
	node.receiptTxs[txHash] = tTx(0, 0, 0, nil, nil, 21000)
	node.tokenContractor.parseTransferAmt = 6

	if err := tw.RecordIncomingTx(context.Background(), txHash.Hex(), 0); err != nil {
		t.Fatal(err)
	}
	tdb := aw.txDB.(*tTxDB)
	if !tdb.storeTxCalled {
		t.Fatal("expected the incoming tx to be stored")
	}
	if tdb.storedTx.Amount != 6 {
		t.Errorf("stored Amount = %d, want 6", tdb.storedTx.Amount)
	}
}
