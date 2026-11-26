package core

import (
	"bytes"
	"context"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encrypt"
	oneclickmock "github.com/bisoncraft/caribou/util/testing/one-click"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/intents"
	"github.com/bisoncraft/caribou/wallet/rhea"
)

const simUSDC = "usdc.test.near"

// simnetRig is a Core on simnet whose lending engine and 1Click client
// talk HTTP to the simnet mock (util/testing/one-click) with its RHEA
// world, through the same wiring the app uses (startRheaEngine with a
// RheaTest configuration). The mock's BTC side is in memory.
type simnetRig struct {
	rig   *testRig
	c     *Core
	eng   *rheaEngine
	btc   *oneclickmock.MemDriver
	world *oneclickmock.RheaWorld
	tw    *TXCWallet
	url   string
}

func newSimnetRig(t *testing.T) *simnetRig {
	t.Helper()
	btc := oneclickmock.NewMemDriver("btc")
	srv, err := oneclickmock.New(&oneclickmock.Config{
		Drivers:         map[string]oneclickmock.Driver{"btc": btc},
		ProcessingDelay: time.Millisecond,
		Rhea:            &oneclickmock.RheaConfig{BlockInterval: 5 * time.Millisecond},
		Logger:          log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(tCtx)
	t.Cleanup(cancel)
	srv.Run(ctx)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	r := &simnetRig{rig: newTestRig(), btc: btc, world: srv.Rhea(), url: hs.URL}
	t.Cleanup(r.rig.shutdown)
	c := r.rig.core
	r.c = c
	c.net = util.Simnet
	cc := &copyCrypter{}
	c.reCrypter = func([]byte, []byte) (encrypt.Crypter, error) { return cc, nil }
	c.setRheaIdentities(c.creds().EncSeed) // the test crypter stores the seed in the clear

	ieng := newTestIntentsEngine(r.rig)
	_, pub := oneclickmock.DevSigningKey()
	if ieng.client, err = intents.NewClient(&intents.Config{BaseURL: hs.URL, SigningKey: pub, AppFees: intentsAppFees()}, tLogger); err != nil {
		t.Fatal(err)
	}
	c.intentsEng.Store(ieng)

	xcw, tw := newTWallet(0)
	tw.info = &asset.WalletInfo{BlockchainClass: asset.BlockchainClassAccount, UnitInfo: tWalletInfo.UnitInfo}
	tw.bal = &asset.Balance{Available: 1e10}
	tw.sendCoin = &tCoin{}
	xcw.address = "bc1qrefund"
	c.wallets[0] = xcw
	if err := xcw.Connect(); err != nil {
		t.Fatal(err)
	}
	r.tw = tw

	dep := *rhea.Simnet
	dep.RelayerAPI = hs.URL + "/rhea/relayer"
	c.cfg.RheaTest = &RheaTestConfig{Deployment: &dep, RPCEndpoints: []string{hs.URL + "/rhea/rpc"}}
	c.startRheaEngine(c.ctx)
	if r.eng, err = c.rheaEngine(); err != nil {
		t.Fatal(err)
	}
	// The host clock follows the simulated chain clock, which a test warps
	// past the admission window.
	r.eng.now = r.world.ChainTime
	r.eng.tickerRates = simnetRates
	return r
}

// simnetRates gives the simnet markets' tickers and NEAR a rate of 1. The
// engine reads it in place of the app's ticker rates, which cover only
// assets registered in this test binary.
func simnetRates() map[string]float64 {
	rates := map[string]float64{rheaGasFiatTicker: 1}
	for _, m := range rhea.Simnet.Markets {
		rates[m.FiatTicker] = 1
	}
	return rates
}

// await polls the engine until the operation reaches state, failing after
// a timeout. Attribution backoff is cleared on each pass in place of
// waiting it out.
func (r *simnetRig) await(t *testing.T, id, state string) *RheaOp {
	t.Helper()
	return r.awaitWhere(t, id, state, func(*RheaOp) bool { return true })
}

// awaitWhere is await with a further condition on the operation.
func (r *simnetRig) awaitWhere(t *testing.T, id, state string, cond func(*RheaOp) bool) *RheaOp {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var op *RheaOp
	for time.Now().Before(deadline) {
		r.eng.clearAttributionBackoff(id)
		r.eng.pollOps(tCtx)
		var err error
		if op, err = r.rig.db.RheaOp(id); err != nil {
			t.Fatal(err)
		}
		if op.State == state && cond(op) {
			return op
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("operation %s reached %s, not %s (%s)", id, op.State, state, op.Notes)
	return nil
}

// openAdmission records the account's first sighting, then advances chain
// time past the admission window.
func (r *simnetRig) openAdmission(t *testing.T) {
	t.Helper()
	if _, err := r.c.RheaAccount(tCtx); err != nil {
		t.Fatalf("RheaAccount: %v", err)
	}
	r.world.Warp(rhea.MaxDeadlineWindow + 3*time.Minute)
	time.Sleep(50 * time.Millisecond) // final blocks carry the warped time
}

// fund credits the MCA's plain balance and waits for the credit to reach
// a final block.
func (r *simnetRig) fund(t *testing.T, mca string, amount int64) {
	t.Helper()
	if err := r.world.Fund(simUSDC, mca, big.NewInt(amount)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
}

// verifyNonces runs the refresh that walks the relayer history and
// advances the nonce checkpoint, which admission pokes in the background.
func (r *simnetRig) verifyNonces(t *testing.T) {
	t.Helper()
	r.eng.refresh(tCtx)
	r.eng.walkMtx.Lock()
	defer r.eng.walkMtx.Unlock()
	for mca, w := range r.eng.lastWalks {
		if w.Error != "" {
			t.Fatalf("nonce walk for %s: %s", mca, w.Error)
		}
	}
}

// TestRheaSimnetEndToEnd runs supply with account creation, withdrawal,
// and both sweeps through the simnet mock over HTTP.
func TestRheaSimnetEndToEnd(t *testing.T) {
	r := newSimnetRig(t)
	c := r.c

	// Supply 0.001 BTC (60 USD at the mock's fallback price), creating the
	// lending account.
	pv, err := c.RheaSupplyPreview(tCtx, 0, simUSDC, 100_000)
	if err != nil {
		t.Fatalf("RheaSupplyPreview: %v", err)
	}
	if !pv.Creating || pv.CreationFee == nil || pv.CreationFee.Amount != "600000" || !pv.CreationFee.OneTime {
		t.Fatalf("supply preview %+v", pv)
	}
	op, err := c.RheaSupplyExecute(tPW, 0, simUSDC, 100_000, pv.MinDelivered, true, pv.CreationFee.Amount)
	if err != nil {
		t.Fatalf("RheaSupplyExecute: %v", err)
	}
	r.deposit(t, op.Leg.DepositAddress, 100_000)
	op = r.await(t, op.ID, db.RheaStateSupplied)
	mca := r.world.MCA(string(c.rheaIdentities()[0].Identity))
	if mca == "" || op.MCA != mca {
		t.Fatalf("supplied to %q, world has %q", op.MCA, mca)
	}
	supplied := r.world.Supplied(mca, simUSDC)
	if op.Supplied != supplied.String() || supplied.Sign() == 0 {
		t.Fatalf("op supplied %s, world %s", op.Supplied, supplied)
	}

	// Withdraw everything to BTC once admission opens.
	r.openAdmission(t)
	wpv, err := c.RheaWithdrawPreview(tCtx, simUSDC, 0, "")
	if err != nil {
		t.Fatalf("RheaWithdrawPreview: %v", err)
	}
	wop, err := c.RheaWithdrawExecute(tPW, simUSDC, 0, wpv.Amount, wpv.MinAtomsOut, wpv.Fee)
	if err != nil {
		t.Fatalf("RheaWithdrawExecute: %v", err)
	}
	wop = r.await(t, wop.ID, db.RheaStateWithdrawn)
	if r.world.Nonce(mca) != 1 || wop.Fees != wpv.Fee {
		t.Fatalf("withdrawn: nonce %d, fees %s (%s)", r.world.Nonce(mca), wop.Fees, wop.Notes)
	}
	if r.btc.Sent("bc1qrefund").Sign() == 0 {
		t.Fatalf("no BTC paid out")
	}

	// A plain balance of unknown origin is swept back into supply.
	r.fund(t, mca, 1_000_000)
	r.verifyNonces(t)
	spv, err := c.RheaSweepPreview(tCtx, simUSDC, nil, "")
	if err != nil {
		t.Fatalf("RheaSweepPreview: %v", err)
	}
	if spv.Recovered != "1000000" || spv.Attributed != "0" {
		t.Fatalf("sweep preview %+v", spv)
	}
	sop, err := c.RheaSweepExecute(tPW, simUSDC, nil, "", 0, spv.Fee)
	if err != nil {
		t.Fatalf("RheaSweepExecute: %v", err)
	}
	sop = r.await(t, sop.ID, db.RheaStateSupplied)
	if sop.Supplied != "1000000" || sop.Fees != spv.Fee {
		t.Fatalf("swept into supply: %+v (%s)", sop, sop.Notes)
	}

	// And another out to the wallet.
	r.fund(t, mca, 1_000_000)
	r.verifyNonces(t)
	var dest uint32
	opv, err := c.RheaSweepPreview(tCtx, simUSDC, &dest, "")
	if err != nil {
		t.Fatalf("RheaSweepPreview out: %v", err)
	}
	oop, err := c.RheaSweepExecute(tPW, simUSDC, &dest, "", opv.MinAtomsOut, opv.Fee)
	if err != nil {
		t.Fatalf("RheaSweepExecute out: %v", err)
	}
	r.await(t, oop.ID, db.RheaStateWithdrawn)
	if n := r.world.Nonce(mca); n != 3 {
		t.Fatalf("nonce %d after three businesses", n)
	}
}

// supplyCreating supplies 0.001 BTC to USDC, creating the account, and
// returns the operation once it reaches state.
func (r *simnetRig) supplyCreating(t *testing.T, state string) *RheaOp {
	t.Helper()
	pv, err := r.c.RheaSupplyPreview(tCtx, 0, simUSDC, 100_000)
	if err != nil {
		t.Fatalf("RheaSupplyPreview: %v", err)
	}
	op, err := r.c.RheaSupplyExecute(tPW, 0, simUSDC, 100_000, pv.MinDelivered, true, pv.CreationFee.Amount)
	if err != nil {
		t.Fatalf("RheaSupplyExecute: %v", err)
	}
	r.deposit(t, op.Leg.DepositAddress, 100_000)
	return r.await(t, op.ID, state)
}

// withdraw executes a withdrawal of amount (empty for the most) to BTC.
func (r *simnetRig) withdraw(t *testing.T, amount string) *RheaOp {
	t.Helper()
	pv, err := r.c.RheaWithdrawPreview(tCtx, simUSDC, 0, amount)
	if err != nil {
		t.Fatalf("RheaWithdrawPreview: %v", err)
	}
	op, err := r.c.RheaWithdrawExecute(tPW, simUSDC, 0, pv.Amount, pv.MinAtomsOut, pv.Fee)
	if err != nil {
		t.Fatalf("RheaWithdrawExecute: %v", err)
	}
	return op
}

// deposit credits a BTC deposit to a swap's deposit address and tells the
// mock, which otherwise finds it on its next poll.
func (r *simnetRig) deposit(t *testing.T, addr string, atoms int64) {
	t.Helper()
	r.btc.Credit(addr, big.NewInt(atoms))
	resp, err := http.Post(r.url+"/v0/deposit/submit", "application/json",
		bytes.NewReader([]byte(`{"depositAddress":"`+addr+`","txHash":"simnet-btc-deposit"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// restart stops the running engine and waits for its loop to exit, then
// starts a replacement on the same Core and mock. With wipe, every local
// lending operation and account record is discarded first: a restore of
// the same seed into a clean database.
func (r *simnetRig) restart(t *testing.T, wipe bool) {
	t.Helper()
	old := r.eng
	old.stop()
	if wipe {
		r.rig.db.rheaMtx.Lock()
		r.rig.db.rheaOps = nil
		r.rig.db.rheaAccounts = nil
		r.rig.db.rheaMtx.Unlock()
	}
	r.c.startRheaEngine(r.c.ctx)
	eng, err := r.c.rheaEngine()
	if err != nil || eng == old {
		t.Fatalf("engine not replaced: %v", err)
	}
	eng.now = r.world.ChainTime
	eng.tickerRates = simnetRates
	r.eng = eng
}

// account returns the stored account record for the rig's identity.
func (r *simnetRig) account(t *testing.T) *db.RheaAccount {
	t.Helper()
	id := r.c.rheaIdentities()[0]
	acct, err := r.rig.db.RheaAccount(rhea.Simnet.AccountManager, uint32(id.Version), string(id.Identity))
	if err != nil {
		t.Fatalf("account record: %v", err)
	}
	return acct
}

// walkError runs the nonce walk and returns its error text ("" on
// success).
func (r *simnetRig) walkError(mca string) string {
	r.eng.refresh(tCtx)
	r.eng.walkMtx.Lock()
	defer r.eng.walkMtx.Unlock()
	if w := r.eng.lastWalks[mca]; w != nil {
		return w.Error
	}
	return ""
}

// force arms a 1Click outcome for the next swap.
func (r *simnetRig) force(t *testing.T, outcome string) {
	t.Helper()
	resp, err := http.Post(r.url+"/test/force", "application/json", bytes.NewReader([]byte(`{"outcome":"`+outcome+`"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// TestRheaSimnetFaults drives the engine through the mock's fault
// injections.
func TestRheaSimnetFaults(t *testing.T) {
	t.Run("lost relayer reply", func(t *testing.T) {
		r := newSimnetRig(t)
		r.supplyCreating(t, db.RheaStateSupplied)
		r.openAdmission(t)
		r.world.SetFaults(oneclickmock.RheaFaults{LostResponses: 1})
		op := r.withdraw(t, "1000000")
		if op.State != db.RheaStateSubmitUnknown {
			t.Fatalf("lost reply: %s", op.State)
		}
		// The identical resubmission fails on the consumed nonce; the
		// original exec is found and attributed.
		op = r.await(t, op.ID, db.RheaStateWithdrawn)
		if !strings.Contains(op.Notes, "fee paid true") {
			t.Fatalf("notes: %s", op.Notes)
		}
	})

	t.Run("failed Withdraw", func(t *testing.T) {
		r := newSimnetRig(t)
		r.supplyCreating(t, db.RheaStateSupplied)
		r.openAdmission(t)
		r.world.SetFaults(oneclickmock.RheaFaults{FailWithdraws: 1})
		op := r.await(t, r.withdraw(t, "1000000").ID, db.RheaStateExecFailed)
		if op.Fees == "" || op.Stranded != "" || op.Active() {
			t.Fatalf("failed Withdraw: %+v (%s)", op, op.Notes)
		}
	})

	t.Run("refunded withdrawal leg", func(t *testing.T) {
		r := newSimnetRig(t)
		r.supplyCreating(t, db.RheaStateSupplied)
		r.openAdmission(t)
		r.force(t, "REFUND")
		op := r.await(t, r.withdraw(t, "1000000").ID, db.RheaStateStranded)
		mca := op.MCA
		if r.world.Balance(simUSDC, mca).String() != op.Stranded || op.Stranded == "" {
			t.Fatalf("stranded %s, MCA holds %s (%s)", op.Stranded, r.world.Balance(simUSDC, mca), op.Notes)
		}
		// The attributed refund is swept back into supply.
		r.verifyNonces(t)
		pv, err := r.c.RheaSweepPreview(tCtx, simUSDC, nil, "")
		if err != nil {
			t.Fatalf("RheaSweepPreview: %v", err)
		}
		if pv.Attributed != op.Stranded || pv.Recovered != "0" {
			t.Fatalf("sweep preview %+v", pv)
		}
		sop, err := r.c.RheaSweepExecute(tPW, simUSDC, nil, "", 0, pv.Fee)
		if err != nil {
			t.Fatalf("RheaSweepExecute: %v", err)
		}
		if sop = r.await(t, sop.ID, db.RheaStateSupplied); len(sop.Covers) != 1 || sop.Covers[0] != op.ID {
			t.Fatalf("sweep covers %v", sop.Covers)
		}
	})

	t.Run("expired deadline", func(t *testing.T) {
		r := newSimnetRig(t)
		r.supplyCreating(t, db.RheaStateSupplied)
		r.openAdmission(t)
		r.world.SetFaults(oneclickmock.RheaFaults{FinalLag: 2, ExecDelay: 300 * time.Millisecond})
		op := r.withdraw(t, "1000000")
		mca := op.MCA
		// Chain time passes the deadline before the exec is included.
		r.world.Warp(rhea.DeadlineWindow + time.Minute)
		time.Sleep(500 * time.Millisecond)
		r.world.Warp(rheaNotExecutedMargin + time.Minute)
		time.Sleep(50 * time.Millisecond)
		op = r.await(t, op.ID, db.RheaStateNotExecuted)
		if !op.Relayer.Unconfirmed || r.world.Nonce(mca) != 0 {
			t.Fatalf("not executed: %+v, nonce %d", op.Relayer, r.world.Nonce(mca))
		}
	})

	t.Run("stale nonce", func(t *testing.T) {
		r := newSimnetRig(t)
		r.supplyCreating(t, db.RheaStateSupplied)
		r.openAdmission(t)
		r.world.SetFaults(oneclickmock.RheaFaults{FinalLag: 2, ExecDelay: 300 * time.Millisecond})
		op := r.withdraw(t, "1000000")
		// Another business consumes the nonce first.
		r.world.SetNonce(op.MCA, 1)
		op = r.await(t, op.ID, db.RheaStateNonceConsumedUnattributed)
		if !op.Active() {
			t.Fatalf("unattributed op inactive")
		}
	})

	t.Run("held receipts", func(t *testing.T) {
		r := newSimnetRig(t)
		r.supplyCreating(t, db.RheaStateSupplied)
		r.openAdmission(t)
		r.world.SetFaults(oneclickmock.RheaFaults{FinalLag: 2, HoldReceipts: true})
		op := r.withdraw(t, "1000000")
		// The exec consumes the nonce while its child receipts wait; the
		// operation stays unresolved and no further business is admitted.
		deadline := time.Now().Add(10 * time.Second)
		for r.world.Nonce(op.MCA) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		for i := 0; i < 20; i++ {
			r.eng.pollOps(tCtx)
			time.Sleep(10 * time.Millisecond)
		}
		if got, _ := r.rig.db.RheaOp(op.ID); got.State != db.RheaStateSubmitted || !strings.Contains(got.Notes, "receipts pending") {
			t.Fatalf("held: %s (%s)", got.State, got.Notes)
		}
		if _, err := r.c.RheaWithdrawPreview(tCtx, simUSDC, 0, "1000000"); err == nil {
			t.Fatalf("second withdrawal admitted while receipts are held")
		}
		r.world.SetFaults(oneclickmock.RheaFaults{FinalLag: 2})
		r.await(t, op.ID, db.RheaStateWithdrawn)
	})

	t.Run("delivery refused by Burrow", func(t *testing.T) {
		r := newSimnetRig(t)
		r.world.SetFaults(oneclickmock.RheaFaults{FinalLag: 2, RejectBurrowDeposits: 1})
		op := r.supplyCreating(t, db.RheaStateStranded)
		if op.Stranded == "" || r.world.Balance(simUSDC, op.MCA).String() != op.Stranded {
			t.Fatalf("stranded %s, MCA holds %s (%s)", op.Stranded, r.world.Balance(simUSDC, op.MCA), op.Notes)
		}
	})

	t.Run("forward returned to the account manager", func(t *testing.T) {
		r := newSimnetRig(t)
		r.world.SetFaults(oneclickmock.RheaFaults{FinalLag: 2, RejectForwards: 1})
		op := r.supplyCreating(t, db.RheaStateDeliveryUnresolved)
		// Unresolved while receipts are pending too; wait for the evidence.
		op = r.awaitWhere(t, op.ID, db.RheaStateDeliveryUnresolved, func(o *RheaOp) bool { return len(o.Refunds) > 0 })
		if len(op.Refunds) != 1 || op.Refunds[0].Account != rhea.Simnet.AccountManager || !op.Active() {
			t.Fatalf("returned to the account manager: %+v (%s)", op.Refunds, op.Notes)
		}
	})

	t.Run("restart mid-flight", func(t *testing.T) {
		r := newSimnetRig(t)
		r.supplyCreating(t, db.RheaStateSupplied)
		r.openAdmission(t)
		op := r.withdraw(t, "1000000")
		// The first engine is stopped before a new one over the same
		// database picks the operation up.
		r.restart(t, false)
		r.await(t, op.ID, db.RheaStateWithdrawn)
	})
}

// TestRheaSimnetRestore restores the identity into a clean database
// against the same mock: no local operation or account record survives,
// so admission rests on chain state and the relayer history alone.
func TestRheaSimnetRestore(t *testing.T) {
	t.Run("child receipts outstanding", func(t *testing.T) {
		r := newSimnetRig(t)
		r.supplyCreating(t, db.RheaStateSupplied)
		r.openAdmission(t)
		r.world.SetFaults(oneclickmock.RheaFaults{FinalLag: 2, HoldReceipts: true})
		op := r.withdraw(t, "1000000")
		mca := op.MCA
		deadline := time.Now().Add(10 * time.Second)
		for r.world.Nonce(mca) != 1 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if r.world.Nonce(mca) != 1 {
			t.Fatalf("exec did not consume the nonce")
		}

		r.restart(t, true)
		// Finalized views lag the head by 40 blocks from here on.
		r.world.SetFaults(oneclickmock.RheaFaults{FinalLag: 40, HoldReceipts: true})
		time.Sleep(300 * time.Millisecond)
		// The restored discovery is a new first sighting; chain time then
		// passes both the original business's deadline and the restored
		// window.
		if _, err := r.c.RheaAccount(tCtx); err != nil {
			t.Fatalf("RheaAccount: %v", err)
		}
		firstSeen := r.account(t).FirstSeenTime
		r.world.Warp(rhea.DeadlineWindow + rhea.MaxDeadlineWindow + 3*time.Minute)
		time.Sleep(300 * time.Millisecond)
		if final, err := r.eng.chain.FinalBlock(tCtx); err != nil || !final.Timestamp.After(firstSeen.Add(rhea.MaxDeadlineWindow+2*time.Minute)) ||
			!final.Timestamp.After(op.Relayer.Deadline) {
			t.Fatalf("final block %v (%v) not past the window", final, err)
		}

		// The exec that consumed nonce 0 is found through the history but
		// its child receipts are outstanding: no checkpoint, no business.
		if e := r.walkError(mca); !strings.Contains(e, "not complete") {
			t.Fatalf("walk with receipts held: %q", e)
		}
		if v := r.account(t).VerifiedBelow; v != 0 {
			t.Fatalf("checkpoint advanced to %d over an incomplete exec", v)
		}
		if _, err := r.c.RheaWithdrawPreview(tCtx, simUSDC, 0, "1000000"); err == nil || !strings.Contains(err.Error(), "not verified complete") {
			t.Fatalf("withdrawal admitted: %v", err)
		}
		r.fund(t, mca, 1_000_000)
		if _, err := r.c.RheaSweepPreview(tCtx, simUSDC, nil, ""); err == nil || !strings.Contains(err.Error(), "not verified complete") {
			t.Fatalf("sweep admitted: %v", err)
		}

		// The receipts run and become final; the walk then verifies the
		// contiguous range and admission opens.
		r.world.SetFaults(oneclickmock.RheaFaults{FinalLag: 40})
		deadline = time.Now().Add(10 * time.Second)
		var e string
		for time.Now().Before(deadline) {
			if e = r.walkError(mca); e == "" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if e != "" {
			t.Fatalf("walk after release: %s", e)
		}
		if v := r.account(t).VerifiedBelow; v != 1 {
			t.Fatalf("checkpoint %d after release", v)
		}
		if _, err := r.c.RheaWithdrawPreview(tCtx, simUSDC, 0, "1000000"); err != nil {
			t.Fatalf("withdrawal after release: %v", err)
		}
		pv, err := r.c.RheaSweepPreview(tCtx, simUSDC, nil, "")
		if err != nil {
			t.Fatalf("sweep after release: %v", err)
		}
		sop, err := r.c.RheaSweepExecute(tPW, simUSDC, nil, "", 0, pv.Fee)
		if err != nil {
			t.Fatalf("RheaSweepExecute: %v", err)
		}
		r.await(t, sop.ID, db.RheaStateSupplied)
	})

	t.Run("completed refund in the account", func(t *testing.T) {
		r := newSimnetRig(t)
		r.supplyCreating(t, db.RheaStateSupplied)
		r.openAdmission(t)
		r.force(t, "REFUND")
		wop := r.await(t, r.withdraw(t, "1000000").ID, db.RheaStateStranded)
		mca := wop.MCA

		r.restart(t, true)
		r.openAdmission(t)
		if e := r.walkError(mca); e != "" {
			t.Fatalf("walk: %s", e)
		}
		if v := r.account(t).VerifiedBelow; v != 1 {
			t.Fatalf("history covers nonces below %d", v)
		}
		// The refund is discovered as a balance of unknown origin and
		// swept without the operation that put it there.
		pv, err := r.c.RheaSweepPreview(tCtx, simUSDC, nil, "")
		if err != nil {
			t.Fatalf("RheaSweepPreview: %v", err)
		}
		if pv.Attributed != "0" || pv.Recovered != wop.Stranded || pv.Balance != wop.Stranded {
			t.Fatalf("restored sweep preview %+v, refund %s", pv, wop.Stranded)
		}
		sop, err := r.c.RheaSweepExecute(tPW, simUSDC, nil, "", 0, pv.Fee)
		if err != nil {
			t.Fatalf("RheaSweepExecute: %v", err)
		}
		if sop = r.await(t, sop.ID, db.RheaStateSupplied); len(sop.Covers) != 0 {
			t.Fatalf("restored sweep covers %v", sop.Covers)
		}
		ops, _ := r.rig.db.RheaOps()
		if len(ops) != 1 || ops[0].ID != sop.ID {
			t.Fatalf("%d operations after the restore", len(ops))
		}
	})
}
