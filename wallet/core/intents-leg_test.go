package core

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/intents"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

func TestLegTimeout(t *testing.T) {
	deadline := time.Unix(1_000_000, 0)
	after := deadline.Add(intentsExpiryGrace + time.Second)
	tests := []struct {
		state        string
		now          time.Time
		want         legTimeout
		wantState    string
		wantInferred bool
		inferredFrom string
	}{
		{IntentsStateQuoted, after, legTimeoutExpired, IntentsStateExpired, false, ""},
		{IntentsStateDepositSent, after, legTimeoutUnobserved, intents.StatusFailed, true, intents.StatusPendingDeposit},
		{intents.StatusIncompleteDeposit, after, legTimeoutIncomplete, intents.StatusFailed, true, intents.StatusIncompleteDeposit},
		{intents.StatusKnownDepositTx, after, legTimeoutNone, intents.StatusKnownDepositTx, false, ""},
		{intents.StatusProcessing, after, legTimeoutNone, intents.StatusProcessing, false, ""},
		{IntentsStateDepositSent, deadline.Add(intentsExpiryGrace - time.Second), legTimeoutNone, IntentsStateDepositSent, false, ""},
	}
	for _, tt := range tests {
		l := &intentsLeg{State: tt.state, Deadline: deadline}
		if got := l.applyTimeout(tt.now); got != tt.want || l.State != tt.wantState || l.Inferred != tt.wantInferred || l.InferredFrom != tt.inferredFrom {
			t.Errorf("%s: outcome %d, leg %+v", tt.state, got, l)
		}
		if tt.wantInferred && l.RefundReason == "" {
			t.Errorf("%s: no reason recorded", tt.state)
		}
	}
	// A zero deadline never proves a timeout.
	l := &intentsLeg{State: IntentsStateDepositSent}
	if l.applyTimeout(after) != legTimeoutNone {
		t.Errorf("zero deadline timed out")
	}
}

func TestLegLateSettlement(t *testing.T) {
	inferred := func(from string) *intentsLeg {
		return &intentsLeg{State: intents.StatusFailed, Inferred: true, InferredFrom: from}
	}
	tests := []struct {
		name   string
		leg    *intentsLeg
		status string
		want   bool
	}{
		{"active leg takes anything", &intentsLeg{State: IntentsStateDepositSent}, intents.StatusProcessing, true},
		{"confirmed terminal is never regressed", &intentsLeg{State: intents.StatusSuccess}, intents.StatusRefunded, false},
		{"confirmed failure is never regressed", &intentsLeg{State: intents.StatusFailed}, intents.StatusSuccess, false},
		{"inferred failure yields to settlement", inferred(intents.StatusPendingDeposit), intents.StatusSuccess, true},
		{"inferred failure yields to a refund", inferred(intents.StatusPendingDeposit), intents.StatusRefunded, true},
		{"inferred failure yields to processing", inferred(intents.StatusPendingDeposit), intents.StatusProcessing, true},
		{"pending deposit restates the inference", inferred(intents.StatusPendingDeposit), intents.StatusPendingDeposit, false},
		{"incomplete restates its inference", inferred(intents.StatusIncompleteDeposit), intents.StatusIncompleteDeposit, false},
		{"pending never supersedes", inferred(intents.StatusIncompleteDeposit), intents.StatusPendingDeposit, false},
	}
	for _, tt := range tests {
		if got := tt.leg.canAdopt(tt.status); got != tt.want {
			t.Errorf("%s: canAdopt = %t", tt.name, got)
		}
	}
	l := inferred(intents.StatusPendingDeposit)
	l.adopt(intents.StatusSuccess)
	if l.State != intents.StatusSuccess || l.Inferred || l.InferredFrom != "" {
		t.Fatalf("adopt left %+v", l)
	}
}

func TestLegRetirementAndRejection(t *testing.T) {
	deadline := time.Unix(1_000_000, 0)
	l := &intentsLeg{State: intents.StatusFailed, Inferred: true, InferredFrom: intents.StatusPendingDeposit, Deadline: deadline}
	if l.retireDue(deadline.Add(intentsInferredHorizon)) || !l.retireDue(deadline.Add(intentsInferredHorizon+time.Second)) {
		t.Fatalf("retirement horizon misapplied")
	}
	if !l.retire() || l.Inferred || l.InferredFrom != "" || l.State != intents.StatusFailed {
		t.Fatalf("retire left %+v", l)
	}
	if l.retire() {
		t.Fatalf("retired twice")
	}
	if (&intentsLeg{State: IntentsStateDepositSent}).retire() {
		t.Fatalf("retired a non-terminal leg")
	}

	r := &intentsLeg{State: IntentsStateDepositSent}
	if !r.applyRejection() || r.State != IntentsStateDepositFailed || r.RefundReason == "" {
		t.Fatalf("rejection left %+v", r)
	}
	if (&intentsLeg{State: intents.StatusProcessing}).applyRejection() {
		t.Fatalf("rejected a leg the service is driving")
	}
}

func TestDepositRejected(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	xcw, tw := newTWallet(0)
	rig.core.wallets[0] = xcw
	l := &intentsLeg{State: IntentsStateDepositSent, DepositTxID: "tx"}
	if rig.core.depositRejected(tCtx, l, 0) {
		t.Fatalf("rejected with no wallet record")
	}
	tw.walletTx = &txdb.WalletTransaction{ID: "tx", Rejected: true}
	if !rig.core.depositRejected(tCtx, l, 0) {
		t.Fatalf("rejection not seen")
	}
	if rig.core.depositRejected(tCtx, &intentsLeg{State: intents.StatusProcessing, DepositTxID: "tx"}, 0) {
		t.Fatalf("rejection checked outside DEPOSIT_SENT")
	}
	if rig.core.depositRejected(tCtx, l, 12345) {
		t.Fatalf("rejection reported for an unknown wallet")
	}
}

// TestFundLeg checks the funding sequence shared by swaps and supplies:
// nothing is sent unless the record is persisted first, a failed send
// leaves the persisted record alone, and a broadcast is always recorded.
func TestFundLeg(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	var submitted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/tokens":
			_, _ = w.Write([]byte(`[]`))
		case "/v0/deposit/submit":
			submitted++
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	eng := newTestIntentsEngine(rig)
	client, err := intents.NewClient(&intents.Config{BaseURL: srv.URL}, tLogger)
	if err != nil {
		t.Fatal(err)
	}
	eng.client = client
	xcw, tw := newTWallet(0)
	tw.info = &asset.WalletInfo{BlockchainClass: asset.BlockchainClassAccount, UnitInfo: tWalletInfo.UnitInfo}

	var steps []string
	funding := func() *legFunding {
		steps = nil
		return &legFunding{
			wallet: xcw, depositAddress: "dep", atoms: 5000,
			persist: func() error { steps = append(steps, "persist"); return nil },
			recordSent: func(txID string, fee uint64, _ float64) {
				steps = append(steps, "recorded")
			},
		}
	}

	// A persist failure sends nothing.
	f := funding()
	f.persist = func() error { return errors.New("db down") }
	tw.sendCoin = &tCoin{}
	if _, err := eng.fundLeg(tCtx, f); err == nil || tw.sendCoin.val != 0 {
		t.Fatalf("send after a persist failure: %v", err)
	}

	// A send failure leaves the persisted record and records nothing.
	f = funding()
	tw.sendErr = errors.New("insufficient funds")
	if _, err := eng.fundLeg(tCtx, f); err == nil || !strings.Contains(err.Error(), "no deposit was recorded") {
		t.Fatalf("send failure: %v", err)
	}
	if len(steps) != 1 || steps[0] != "persist" {
		t.Fatalf("steps after a send failure: %v", steps)
	}
	tw.sendErr = nil

	// A broadcast is recorded, with the wallet's fee, and hinted.
	f = funding()
	tw.walletTx = &txdb.WalletTransaction{Fees: 210}
	var gotFee uint64
	f.recordSent = func(_ string, fee uint64, _ float64) {
		steps = append(steps, "recorded")
		gotFee = fee
	}
	if _, err := eng.fundLeg(tCtx, f); err != nil {
		t.Fatalf("fundLeg: %v", err)
	}
	if len(steps) != 2 || steps[0] != "persist" || steps[1] != "recorded" || tw.sendCoin.val != 5000 || gotFee != 210 || submitted != 1 {
		t.Fatalf("funded: steps %v, sent %d, fee %d, hints %d", steps, tw.sendCoin.val, gotFee, submitted)
	}
}
