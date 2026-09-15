// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// fullLimit is trustlineLimitAtomic as account_lines reports it at USDC's
// six decimals: the limit the wallet sets, and the bar for Approved.
const fullLimit = "999999999.999999"

// usdcLine is an account_lines entry for the test wallet's USDC issuer at
// the full limit.
func usdcLine(balance string) accountLine {
	return usdcLineWithLimit(balance, fullLimit)
}

// usdcLineWithLimit is usdcLine at an arbitrary limit.
func usdcLineWithLimit(balance, limit string) accountLine {
	return accountLine{
		Account:  TokenUSDC.NetIssuers[util.Mainnet],
		Currency: TokenUSDC.Currency,
		Balance:  balance,
		Limit:    limit,
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

// fundedAccountHandlers serves account_info for an account that already
// holds one RippleState (owner count 1) with a few drops spare above its
// reserve, and a fee of 10 drops. Tests that expect the TrustSet to need
// only the fee have room; tests that expect it to add the owner reserve
// do not.
func fundedAccountHandlers(m *mockRippled) {
	// 1 XRP base + 0.2 XRP owner reserve locked, 50 drops available.
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("1200050", 1, 0), nil
	})
	m.handle("fee", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":               "success",
			"drops":                map[string]any{"open_ledger_fee": "10"},
			"ledger_current_index": uint64(100),
		}, nil
	})
}

func countingSubmit(m *mockRippled) *atomic.Int32 {
	var submits atomic.Int32
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		submits.Add(1)
		return map[string]any{"status": "success", "engine_result": "tesSUCCESS", "tx_json": map[string]any{}}, nil
	})
	return &submits
}

// A line below the full limit, zero included, is not ready: XRPL refuses a
// payment that would raise the balance above the holder's limit, so the
// address must stay withheld and EstablishTrustline must re-set the limit.
func TestTrustlineBelowFullLimitIsNotApproved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit string
		want  asset.ApprovalStatus
	}{
		{"zero limit", "0", asset.NotApproved},
		{"partial limit", "100", asset.NotApproved},
		{"just under full", "999999999.999998", asset.NotApproved},
		{"full limit", fullLimit, asset.Approved},
		{"above full", "1000000000", asset.Approved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockRippled(t)
			tw := makeTokenTestWallet(t, m)
			tw.parent.cacheAccountState(true, 5_000_000)
			m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
				return accountLinesResponse([]accountLine{usdcLineWithLimit("5", tc.limit)}), nil
			})
			bal, err := tw.Balance()
			if err != nil {
				t.Fatalf("Balance: %v", err)
			}
			if bal.Available != 5_000_000 {
				t.Fatalf("Balance.Available = %d, want the line balance regardless of limit", bal.Available)
			}
			if st := tw.TrustlineStatus(); st.Status != tc.want {
				t.Fatalf("TrustlineStatus = %v for limit %q, want %v", st.Status, tc.limit, tc.want)
			}
		})
	}
}

// A zero-limit line kept alive by a balance already carries this side's
// reserve, so restoring the limit costs only the fee, and must submit rather
// than report the line as done.
func TestEstablishTrustlineRestoresZeroLimitLine(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	tw.parent.baseReserve = BaseReserveDrops
	tw.parent.ownerReserve = OwnerReserveDrops
	txDB, _ := newTestTxDB(t)
	tw.txDB = txDB
	fundedAccountHandlers(m)
	submits := countingSubmit(m)
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse([]accountLine{usdcLineWithLimit("5", "0")}), nil
	})

	txID, err := tw.EstablishTrustline()
	if err != nil {
		t.Fatalf("EstablishTrustline error: %v", err)
	}
	if txID == "" {
		t.Fatal("EstablishTrustline returned an empty ID for a zero-limit line; it must submit a TrustSet")
	}
	if submits.Load() != 1 {
		t.Fatalf("submit called %d times, want 1", submits.Load())
	}
	if st := tw.TrustlineStatus(); st.Status != asset.Pending {
		t.Fatalf("TrustlineStatus = %+v after submit, want Pending", st)
	}
}

// A zero-limit, zero-balance line on this side is default, so setting the
// limit makes the side reserved: the owner reserve is due on top of the fee.
func TestEstablishTrustlineEmptyZeroLimitLineNeedsReserve(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	tw.parent.baseReserve = BaseReserveDrops
	tw.parent.ownerReserve = OwnerReserveDrops
	fundedAccountHandlers(m)
	submits := countingSubmit(m)
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse([]accountLine{usdcLineWithLimit("0", "0")}), nil
	})

	_, err := tw.EstablishTrustline()
	if !errors.Is(err, asset.ErrInsufficientBalance) {
		t.Fatalf("EstablishTrustline error = %v, want ErrInsufficientBalance", err)
	}
	if submits.Load() != 0 {
		t.Fatalf("submit called %d times, want 0", submits.Load())
	}
	want := uint64(OwnerReserveDrops + 10 - 50)
	if st := tw.TrustlineStatus(); st.Shortfall != want {
		t.Fatalf("TrustlineStatus.Shortfall = %d, want %d (owner reserve plus fee less the 50 drops available)", st.Shortfall, want)
	}
}

// A positive limit below the full one already holds the reserve; raising it
// costs the fee alone and must submit.
func TestEstablishTrustlineRaisesPartialLimit(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	tw.parent.baseReserve = BaseReserveDrops
	tw.parent.ownerReserve = OwnerReserveDrops
	fundedAccountHandlers(m)
	submits := countingSubmit(m)
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse([]accountLine{usdcLineWithLimit("0", "100")}), nil
	})

	if _, err := tw.EstablishTrustline(); err != nil {
		t.Fatalf("EstablishTrustline error: %v", err)
	}
	if submits.Load() != 1 {
		t.Fatalf("submit called %d times, want 1", submits.Load())
	}
}

// Send must refuse a payment the destination's limit cannot absorb, since
// the network would reject it after the fee is paid.
func TestSendRejectsDestinationOverTrustLimit(t *testing.T) {
	const destAddr = "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"
	for _, tc := range []struct {
		name          string
		limit, bal    string
		value         uint64
		wantRejection bool
	}{
		{"zero limit", "0", "0", 1_000_000, true},
		{"over remaining room", "100", "90", 20_000_000, true},
		{"exactly remaining room", "100", "90", 10_000_000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockRippled(t)
			tw := makeTokenTestWallet(t, m)
			m.handle("account_lines", func(params json.RawMessage) (any, *rpcError) {
				var p map[string]any
				_ = json.Unmarshal(params, &p)
				if acct, _ := p["account"].(string); acct == destAddr {
					return accountLinesResponse([]accountLine{usdcLineWithLimit(tc.bal, tc.limit)}), nil
				}
				return accountLinesResponse([]accountLine{usdcLine("100")}), nil
			})
			m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
				return accountInfoResponse("100000000", 0, 0), nil
			})
			m.handle("fee", func(_ json.RawMessage) (any, *rpcError) {
				return map[string]any{
					"status":               "success",
					"drops":                map[string]any{"open_ledger_fee": "12"},
					"ledger_current_index": uint64(100),
				}, nil
			})
			submits := countingSubmit(m)

			_, err := tw.Send(destAddr, tc.value, 0)
			if tc.wantRejection {
				if err == nil || !strings.Contains(err.Error(), "trustline limit") {
					t.Fatalf("Send error = %v, want a trustline-limit rejection", err)
				}
				if submits.Load() != 0 {
					t.Fatalf("submit called %d times for an over-limit send, want 0", submits.Load())
				}
				return
			}
			if err != nil {
				t.Fatalf("Send error: %v", err)
			}
		})
	}
}

// trustSetTx is an observed TrustSet by account for USDC at limit.
func trustSetTx(account, issuer, hash, limit string, ledger uint64) *txResult {
	return &txResult{
		Account:         account,
		TransactionType: "TrustSet",
		Hash:            hash,
		Fee:             "12",
		LedgerIndex:     ledger,
		LimitAmount: map[string]interface{}{
			"currency": TokenUSDC.Currency,
			"issuer":   issuer,
			"value":    limit,
		},
	}
}

// externalTrustSetRig wires a parent wallet with a registered token wallet
// whose cache is seeded Approved, an account with no line any more, and a
// note channel for the token. Processing a tip or an observed TrustSet must
// then flip the cache to NotApproved.
func externalTrustSetRig(t *testing.T) (*xrpWallet, *TokenWallet, *mockRippled, func() []*asset.TrustlineState) {
	t.Helper()
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	w := tw.parent
	txDB, mt := newTestTxDB(t)
	w.txDB, w.metaTable = txDB, mt
	tw.txDB = txDB
	w.tokenSpecs = networkTokenSpecs(util.Mainnet)
	w.baseReserve, w.ownerReserve = BaseReserveDrops, OwnerReserveDrops
	w.registerTokenWallet(tw)
	drain := trustlineNotes(tw)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("5000000", 0, 0), nil
	})

	// Seed Approved from a full-limit line, then take the line away.
	tw.updateTrustlineState(&accountLine{Account: tw.issuerAddr, Currency: TokenUSDC.Currency, Balance: "0", Limit: fullLimit})
	if st := tw.TrustlineStatus(); st.Status != asset.Approved {
		t.Fatalf("seed status = %v, want Approved", st.Status)
	}
	drain()
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse(nil), nil
	})
	return w, tw, m, drain
}

// A TrustSet submitted from another client of the same account arrives
// through the account scan with no Payment alongside it. The tip must still
// refresh the token's trust-line state and record the TrustSet under the
// token, since it burned a fee and changed the reserve.
func TestScanExternalTrustSetRefreshesTrustline(t *testing.T) {
	const txID = "5555000000000000000000000000000000000000000000000000000000000005"
	w, tw, m, drain := externalTrustSetRig(t)
	m.handle("account_tx", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"transactions": []any{
				map[string]any{
					"tx": map[string]any{
						"hash":            txID,
						"TransactionType": "TrustSet",
						"Account":         w.address,
						"Fee":             "12",
						"ledger_index":    uint64(51),
						"LimitAmount": map[string]any{
							"currency": TokenUSDC.Currency,
							"issuer":   tw.issuerAddr,
							"value":    "0",
						},
					},
					"meta":         map[string]any{"TransactionResult": "tesSUCCESS"},
					"validated":    true,
					"ledger_index": uint64(51),
				},
			},
		}, nil
	})
	w.lastScannedLedger.Store(50)
	w.tipMtx.Lock()
	w.tip = 50
	w.tipMtx.Unlock()

	w.handleValidatedTip(context.Background(), 51)

	if st := tw.TrustlineStatus(); st.Status != asset.NotApproved {
		t.Fatalf("TrustlineStatus = %v after an external TrustSet removed the line, want NotApproved", st.Status)
	}
	notes := drain()
	if len(notes) == 0 || notes[len(notes)-1].Status != asset.NotApproved {
		t.Fatalf("trustline notes = %+v, want a NotApproved note", notes)
	}
	stored, err := w.txDB.GetTx(txID)
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if stored == nil {
		t.Fatal("external TrustSet was not recorded")
	}
	if stored.Type != txdb.RevokeTokenApproval || stored.AssetID != tw.assetID || stored.Fees != 12 {
		t.Fatalf("stored = type %v asset %d fees %d, want RevokeTokenApproval under %d with fee 12", stored.Type, stored.AssetID, stored.Fees, tw.assetID)
	}
}

// The websocket feed delivers the same TrustSet without a scan.
func TestObservedExternalTrustSetRefreshesTrustline(t *testing.T) {
	const txID = "6666000000000000000000000000000000000000000000000000000000000006"
	w, tw, _, drain := externalTrustSetRig(t)

	w.processObservedTransaction(trustSetTx(w.address, tw.issuerAddr, txID, "0", 500), txMeta{TransactionResult: "tesSUCCESS"}, 500)

	if st := tw.TrustlineStatus(); st.Status != asset.NotApproved {
		t.Fatalf("TrustlineStatus = %v after an observed external TrustSet, want NotApproved", st.Status)
	}
	if notes := drain(); len(notes) == 0 {
		t.Fatal("no trustline note after an observed external TrustSet")
	}
	stored, err := w.txDB.GetTx(txID)
	if err != nil || stored == nil {
		t.Fatalf("observed TrustSet not recorded (err %v)", err)
	}
	if stored.Type != txdb.RevokeTokenApproval {
		t.Fatalf("stored type = %v, want RevokeTokenApproval", stored.Type)
	}
}

// A TrustSet from the issuer's side is not this wallet's transaction and is
// not recorded, but it changed the line, so the state still refreshes: here
// the line reappears at the full limit and the state flips to Approved.
func TestObservedIssuerTrustSetRefreshesTrustline(t *testing.T) {
	const txID = "7777000000000000000000000000000000000000000000000000000000000007"
	w, tw, m, drain := externalTrustSetRig(t)
	// Start from NotApproved (no line), then have the line come back.
	tw.updateTrustlineState(nil)
	drain()
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse([]accountLine{usdcLine("0")}), nil
	})

	w.processObservedTransaction(trustSetTx(tw.issuerAddr, w.address, txID, "0", 600), txMeta{TransactionResult: "tesSUCCESS"}, 600)

	if st := tw.TrustlineStatus(); st.Status != asset.Approved {
		t.Fatalf("TrustlineStatus = %v after an issuer-side TrustSet, want Approved from the fresh account_lines", st.Status)
	}
	stored, err := w.txDB.GetTx(txID)
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if stored != nil {
		t.Fatalf("issuer-side TrustSet was recorded as %v; only the wallet's own TrustSets belong in history", stored.Type)
	}
}
