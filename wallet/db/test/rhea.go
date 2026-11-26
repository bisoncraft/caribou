// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dbtest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/intents"
)

// testRheaOps exercises RHEA operation storage: round trip, newest-first
// listing, the active set, and leaving the active set on a terminal
// re-store. A withdrawal whose 1Click leg is refunded stays active.
func testRheaOps(t *testing.T, d db.DB) {
	if _, err := d.RheaOp("nope"); !errors.Is(err, db.ErrRheaOpNotFound) {
		t.Fatalf("expected ErrRheaOpNotFound, got %v", err)
	}
	if ops, err := d.RheaOps(); err != nil || len(ops) != 0 {
		t.Fatalf("RheaOps on empty store: %d, %v", len(ops), err)
	}
	if err := d.StoreRheaOp(&db.RheaOp{Kind: db.RheaOpSupply}); err == nil {
		t.Fatal("stored an operation without an ID")
	}

	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	op := func(i int, kind, state string) *db.RheaOp {
		return &db.RheaOp{
			ID:        fmt.Sprintf("op%d", i),
			Kind:      kind,
			State:     state,
			MCA:       "rhea000001.multica.near",
			Amount:    fmt.Sprint(i + 1),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			Relayer:   &db.RheaRelayerLeg{Nonce: uint64(i)},
		}
	}
	ops := []*db.RheaOp{
		op(0, db.RheaOpSupply, intents.StatusRefunded),         // terminal
		op(1, db.RheaOpWithdraw, db.RheaStateSubmitUnknown),    // active
		op(2, db.RheaOpWithdraw, intents.StatusRefunded),       // active: refund to attribute
		op(3, db.RheaOpSupply, db.RheaStateDeliveryUnresolved), // active
	}
	for _, i := range []int{2, 0, 3, 1} {
		if err := d.StoreRheaOp(ops[i]); err != nil {
			t.Fatalf("StoreRheaOp(%d): %v", i, err)
		}
	}
	got, err := d.RheaOp("op1")
	if err != nil || got.State != db.RheaStateSubmitUnknown || got.Relayer == nil || got.Relayer.Nonce != 1 || got.UpdatedAt.IsZero() {
		t.Fatalf("RheaOp round trip: %+v, %v", got, err)
	}
	all, err := d.RheaOps()
	if err != nil || len(all) != 4 {
		t.Fatalf("RheaOps: %d, %v", len(all), err)
	}
	for i, want := range []string{"op3", "op2", "op1", "op0"} {
		if all[i].ID != want {
			t.Fatalf("listing position %d is %s, want %s", i, all[i].ID, want)
		}
	}
	active, err := d.ActiveRheaOps()
	if err != nil || len(active) != 3 {
		t.Fatalf("ActiveRheaOps: %d, %v", len(active), err)
	}
	for i, want := range []string{"op1", "op2", "op3"} {
		if active[i].ID != want {
			t.Fatalf("active position %d is %s, want %s", i, active[i].ID, want)
		}
	}

	// Re-storing in a terminal state leaves the active set.
	ops[1].State = db.RheaStateNotExecuted
	if err := d.StoreRheaOp(ops[1]); err != nil {
		t.Fatalf("StoreRheaOp: %v", err)
	}
	if active, err = d.ActiveRheaOps(); err != nil || len(active) != 2 {
		t.Fatalf("ActiveRheaOps after terminal re-store: %d, %v", len(active), err)
	}
	if all, err = d.RheaOps(); err != nil || len(all) != 4 {
		t.Fatalf("RheaOps after re-store: %d, %v", len(all), err)
	}
}

// testRheaAccounts exercises MCA account records, keyed by account
// manager, derivation version, and identity.
func testRheaAccounts(t *testing.T, d db.DB) {
	if _, err := d.RheaAccount("multica.near", 1, "ab"); !errors.Is(err, db.ErrRheaAccountNotFound) {
		t.Fatalf("expected ErrRheaAccountNotFound, got %v", err)
	}
	for _, bad := range []*db.RheaAccount{
		{Identity: "ab", MCA: "m"},
		{AccountManager: "multica.near", MCA: "m"},
		{AccountManager: "multica.near", Identity: "ab"},
	} {
		if err := d.StoreRheaAccount(bad); err == nil {
			t.Fatalf("stored incomplete account %+v", bad)
		}
	}
	seen := time.Unix(0, 1794949549020383055)
	a := &db.RheaAccount{
		AccountManager:    "multica.near",
		Identity:          "ab",
		DerivationVersion: 1,
		MCA:               "rhea000001.multica.near",
		FirstSeenHeight:   225016693,
		FirstSeenHash:     "4jRB",
		FirstSeenTime:     seen,
		VerifiedBelow:     7,
	}
	if err := d.StoreRheaAccount(a); err != nil {
		t.Fatalf("StoreRheaAccount: %v", err)
	}
	got, err := d.RheaAccount("multica.near", 1, "ab")
	if err != nil || got.MCA != a.MCA || !got.FirstSeenTime.Equal(seen) || got.VerifiedBelow != 7 || got.FirstSeenHeight != 225016693 {
		t.Fatalf("RheaAccount round trip: %+v, %v", got, err)
	}
	// Other versions and account managers are separate records.
	if _, err := d.RheaAccount("multica.near", 2, "ab"); !errors.Is(err, db.ErrRheaAccountNotFound) {
		t.Fatalf("version 2 lookup: %v", err)
	}
	if _, err := d.RheaAccount("other.near", 1, "ab"); !errors.Is(err, db.ErrRheaAccountNotFound) {
		t.Fatalf("other account manager lookup: %v", err)
	}
	a.VerifiedBelow = 9
	if err := d.StoreRheaAccount(a); err != nil {
		t.Fatalf("StoreRheaAccount: %v", err)
	}
	if got, err = d.RheaAccount("multica.near", 1, "ab"); err != nil || got.VerifiedBelow != 9 {
		t.Fatalf("replaced record: %+v, %v", got, err)
	}
}

// testRheaInferredOps checks that a terminal leg state inferred locally
// keeps the operation active, and that only a service-reported status or
// deliberate retirement removes it.
func testRheaInferredOps(t *testing.T, d db.DB) {
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	inferred := func(id, state, from string) *db.RheaOp {
		return &db.RheaOp{
			ID: id, Kind: db.RheaOpSupply, State: state, CreatedAt: base,
			Leg: &db.RheaLeg{DepositAddress: id, Inferred: true, InferredFrom: from},
		}
	}
	failed := inferred("failed", intents.StatusFailed, intents.StatusPendingDeposit)
	expired := inferred("expired", db.IntentsStateExpired, intents.StatusPendingDeposit)
	refundedWithdraw := &db.RheaOp{ID: "wd", Kind: db.RheaOpWithdraw, State: intents.StatusRefunded, CreatedAt: base}
	for _, op := range []*db.RheaOp{failed, expired, refundedWithdraw} {
		if !op.Terminal() && op.Kind == db.RheaOpSupply {
			t.Fatalf("%s: state should be terminal", op.ID)
		}
		if !op.Active() {
			t.Fatalf("%s: not active", op.ID)
		}
		if err := d.StoreRheaOp(op); err != nil {
			t.Fatalf("StoreRheaOp(%s): %v", op.ID, err)
		}
	}
	activeIDs := func() map[string]bool {
		t.Helper()
		ops, err := d.ActiveRheaOps()
		if err != nil {
			t.Fatalf("ActiveRheaOps: %v", err)
		}
		ids := make(map[string]bool, len(ops))
		for _, op := range ops {
			ids[op.ID] = true
		}
		return ids
	}
	if ids := activeIDs(); !ids["failed"] || !ids["expired"] || !ids["wd"] || len(ids) != 3 {
		t.Fatalf("active set %v", ids)
	}
	// Re-stored still inferred: still active.
	if err := d.StoreRheaOp(failed); err != nil {
		t.Fatal(err)
	}
	if !activeIDs()["failed"] {
		t.Fatalf("inferred FAILED left the active set on re-store")
	}
	// A service-reported status supersedes the inference: final.
	failed.State = intents.StatusRefunded
	failed.Leg.Inferred, failed.Leg.InferredFrom = false, ""
	if err := d.StoreRheaOp(failed); err != nil {
		t.Fatal(err)
	}
	// Deliberate retirement makes the inferred EXPIRED final.
	expired.RetireInference()
	if expired.Leg.Inferred || expired.Leg.InferredFrom != "" || expired.Active() {
		t.Fatalf("RetireInference left %+v", expired.Leg)
	}
	if err := d.StoreRheaOp(expired); err != nil {
		t.Fatal(err)
	}
	if ids := activeIDs(); ids["failed"] || ids["expired"] || !ids["wd"] {
		t.Fatalf("active set after finalization %v", ids)
	}
	// Records persist through all of it.
	for _, id := range []string{"failed", "expired", "wd"} {
		if _, err := d.RheaOp(id); err != nil {
			t.Fatalf("RheaOp(%s): %v", id, err)
		}
	}
}
