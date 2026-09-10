// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// usdcLine is an account_lines entry for the test wallet's USDC issuer.
func usdcLine(balance string) accountLine {
	return accountLine{
		Account:  TokenUSDC.NetIssuers[util.Mainnet],
		Currency: TokenUSDC.Currency,
		Balance:  balance,
		Limit:    "999999999",
	}
}

// trustlineNotes wires a note channel into tw and returns a drain that
// collects every "trustline" custom note emitted so far.
func trustlineNotes(tw *TokenWallet) func() []*asset.TrustlineState {
	ch := make(chan asset.WalletNotification, 32)
	tw.emit = asset.NewWalletEmitter(ch, tw.assetID, util.NewLogger("test", util.LevelError, nil))
	return func() []*asset.TrustlineState {
		var states []*asset.TrustlineState
		for {
			select {
			case n := <-ch:
				if cn, ok := n.(*asset.CustomWalletNote); ok && cn.Route == "trustline" {
					states = append(states, cn.Payload.(*asset.TrustlineState))
				}
			default:
				return states
			}
		}
	}
}

// An existing line must never be re-set: that burns a fee and a sequence for
// a no-op. Restores and repeated Receive visits hit this path.
func TestEstablishTrustlineIdempotentWhenLineExists(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	tw.parent.ownerReserve = OwnerReserveDrops
	drain := trustlineNotes(tw)
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse([]accountLine{usdcLine("5")}), nil
	})
	var submits atomic.Int32
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		submits.Add(1)
		return map[string]any{"status": "success", "engine_result": "tesSUCCESS"}, nil
	})

	txID, err := tw.EstablishTrustline()
	if err != nil {
		t.Fatalf("EstablishTrustline error: %v", err)
	}
	if txID != "" {
		t.Fatalf("txID = %q for an existing line, want empty", txID)
	}
	if submits.Load() != 0 {
		t.Fatalf("submit called %d times for an existing line, want 0", submits.Load())
	}
	if st := tw.TrustlineStatus(); st.Status != asset.Approved || st.Reserve != OwnerReserveDrops || st.Shortfall != 0 {
		t.Fatalf("TrustlineStatus = %+v, want Approved with reserve %d and no shortfall", st, OwnerReserveDrops)
	}
	notes := drain()
	if len(notes) != 1 || notes[0].Status != asset.Approved {
		t.Fatalf("trustline notes = %+v, want one Approved note", notes)
	}
}

// A TrustSet still awaiting validation must be reported, not duplicated: a
// second request while the first is in flight returns the pending ID.
func TestEstablishTrustlineReturnsPendingTrustSet(t *testing.T) {
	const pendingID = "4444000000000000000000000000000000000000000000000000000000000004"
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	txDB, _ := newTestTxDB(t)
	tw.txDB = txDB
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse(nil), nil
	})
	var submits atomic.Int32
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		submits.Add(1)
		return map[string]any{"status": "success", "engine_result": "tesSUCCESS"}, nil
	})
	wtx := &txdb.WalletTransaction{
		Type: txdb.ApproveToken, ID: pendingID, Amount: trustlineLimitAtomic, Fees: 12,
		NetworkID: BipID, AssetID: tw.assetID,
	}
	setLastLedgerSequence(wtx, 100)
	if err := txDB.StoreTx(wtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	txID, err := tw.EstablishTrustline()
	if err != nil {
		t.Fatalf("EstablishTrustline error: %v", err)
	}
	if txID != pendingID {
		t.Fatalf("txID = %q, want the pending TrustSet %q", txID, pendingID)
	}
	if submits.Load() != 0 {
		t.Fatalf("submit called %d times with a TrustSet already pending, want 0", submits.Load())
	}
	if st := tw.TrustlineStatus(); st.Status != asset.Pending {
		t.Fatalf("TrustlineStatus = %+v, want Pending", st)
	}
}

// An account that does not exist on the ledger cannot submit anything, so
// its shortfall is the base reserve plus the owner reserve plus the fee, not
// just the fee-plus-owner-reserve an existing account would need.
func TestEstablishTrustlineShortfallForUnfundedAccount(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	tw.parent.baseReserve = BaseReserveDrops
	tw.parent.ownerReserve = OwnerReserveDrops
	drain := trustlineNotes(tw)
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{errType: "actNotFound", msg: "account not found"}
	})
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{errType: "actNotFound", msg: "account not found"}
	})
	m.handle("fee", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":               "success",
			"drops":                map[string]any{"open_ledger_fee": "10"},
			"ledger_current_index": uint64(100),
		}, nil
	})
	var submits atomic.Int32
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		submits.Add(1)
		return map[string]any{"status": "success", "engine_result": "tesSUCCESS"}, nil
	})

	_, err := tw.EstablishTrustline()
	if !errors.Is(err, asset.ErrInsufficientBalance) {
		t.Fatalf("EstablishTrustline error = %v, want ErrInsufficientBalance", err)
	}
	if submits.Load() != 0 {
		t.Fatalf("submit called %d times for an unfunded account, want 0", submits.Load())
	}
	want := uint64(BaseReserveDrops + OwnerReserveDrops + 10)
	if st := tw.TrustlineStatus(); st.Status != asset.NotApproved || st.Shortfall != want {
		t.Fatalf("TrustlineStatus = %+v, want NotApproved with shortfall %d", st, want)
	}
	notes := drain()
	if len(notes) == 0 || notes[len(notes)-1].Shortfall != want {
		t.Fatalf("trustline notes = %+v, want the last to carry shortfall %d", notes, want)
	}
}

// The shortfall for an existing account is what the live fee and owner
// reserve exceed the available balance by, exactly.
func TestEstablishTrustlineShortfallForFundedAccount(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	tw.parent.baseReserve = BaseReserveDrops
	tw.parent.ownerReserve = OwnerReserveDrops
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse(nil), nil
	})
	// 1.1 XRP raw: base reserve locked, 0.1 XRP available.
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("1100000", 0, 0), nil
	})
	m.handle("fee", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":               "success",
			"drops":                map[string]any{"open_ledger_fee": "10"},
			"ledger_current_index": uint64(100),
		}, nil
	})
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		t.Error("submit must not be reached")
		return nil, &rpcError{errType: "internal"}
	})

	_, err := tw.EstablishTrustline()
	if !errors.Is(err, asset.ErrInsufficientBalance) {
		t.Fatalf("EstablishTrustline error = %v, want ErrInsufficientBalance", err)
	}
	want := uint64(OwnerReserveDrops + 10 - 100_000)
	if st := tw.TrustlineStatus(); st.Shortfall != want {
		t.Fatalf("TrustlineStatus.Shortfall = %d, want %d", st.Shortfall, want)
	}
}

// Balance doubles as the status refresh: the cache follows what
// account_lines reports, and only transitions are announced.
func TestBalanceRefreshesTrustlineStatus(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	tw.parent.baseReserve = BaseReserveDrops
	tw.parent.ownerReserve = OwnerReserveDrops
	tw.parent.cacheAccountState(true, 5_000_000)
	drain := trustlineNotes(tw)

	if st := tw.TrustlineStatus(); st.Status != asset.NotApproved {
		t.Fatalf("unqueried TrustlineStatus = %+v, want NotApproved", st)
	}

	var lines []accountLine
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse(lines), nil
	})

	if _, err := tw.Balance(); err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if st := tw.TrustlineStatus(); st.Status != asset.NotApproved || st.Shortfall != 0 {
		t.Fatalf("TrustlineStatus = %+v after no-line query, want NotApproved and affordable", st)
	}
	if notes := drain(); len(notes) != 1 {
		t.Fatalf("got %d trustline notes after the first query, want 1", len(notes))
	}

	// Same observation again: no new note.
	if _, err := tw.Balance(); err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if notes := drain(); len(notes) != 0 {
		t.Fatalf("got %d trustline notes for an unchanged state, want 0", len(notes))
	}

	lines = []accountLine{usdcLine("12.5")}
	bal, err := tw.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 12_500_000 {
		t.Fatalf("Balance.Available = %d, want 12500000", bal.Available)
	}
	if st := tw.TrustlineStatus(); st.Status != asset.Approved {
		t.Fatalf("TrustlineStatus = %+v after the line appeared, want Approved", st)
	}
	notes := drain()
	if len(notes) != 1 || notes[0].Status != asset.Approved {
		t.Fatalf("trustline notes = %+v, want one Approved note", notes)
	}
}

// The parent's balance change fans out to its connected token wallets, since
// token balances live on the same account and the token wallet has no chain
// observer of its own.
func TestParentBalanceChangeRefreshesTokenWallets(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	w := tw.parent
	log := util.NewLogger("test", util.LevelError, nil)
	nativeNotes := make(chan asset.WalletNotification, 16)
	w.emit = asset.NewWalletEmitter(nativeNotes, BipID, log)
	tokenNotes := make(chan asset.WalletNotification, 16)
	tw.emit = asset.NewWalletEmitter(tokenNotes, tw.assetID, log)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("3000000", 0, 0), nil
	})
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse([]accountLine{usdcLine("7")}), nil
	})
	w.registerTokenWallet(tw)

	w.emitBalanceChange()

	var tokenBal *asset.Balance
	for done := false; !done; {
		select {
		case n := <-tokenNotes:
			if bn, ok := n.(*asset.BalanceChangeNote); ok {
				tokenBal = bn.Balance
			}
		default:
			done = true
		}
	}
	if tokenBal == nil {
		t.Fatal("no token BalanceChangeNote after the parent's balance change")
	}
	if tokenBal.Available != 7_000_000 {
		t.Fatalf("token balance = %d, want 7000000", tokenBal.Available)
	}
	select {
	case n := <-nativeNotes:
		if _, ok := n.(*asset.BalanceChangeNote); !ok {
			t.Fatalf("native note = %T, want BalanceChangeNote", n)
		}
	default:
		t.Fatal("no native BalanceChangeNote")
	}
}

func TestMinimumDepositIsBaseReserve(t *testing.T) {
	w := &xrpWallet{baseReserve: BaseReserveDrops}
	if got := w.MinimumDeposit(); got != BaseReserveDrops {
		t.Fatalf("MinimumDeposit = %d, want %d", got, BaseReserveDrops)
	}
}
