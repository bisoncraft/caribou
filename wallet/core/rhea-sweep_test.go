package core

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/near"
	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/intents"
	"github.com/bisoncraft/caribou/wallet/rhea"
)

const (
	sweepGasFeeNEAR = "1000000000000000000000"
	// sweepOutFee is the NEAR fee plus one storage deposit.
	sweepOutFee = "2250000000000000000000"
)

// newSweepRig is a withdrawal rig whose MCA holds 1 USDC in its plain
// balance and whose relayer publishes a 0.001 NEAR fee.
func newSweepRig(t *testing.T) *withdrawRig {
	r := newWithdrawRig(t)
	r.sub.cfg.GasFee[rhea.GasTokenNEAR], _ = new(big.Int).SetString(sweepGasFeeNEAR, 10)
	r.setPlainBalance("1000000")
	return r
}

func (r *withdrawRig) setPlainBalance(amount string) {
	r.chain.setView(usdcTokenForTest, "ft_balance_of", `{"account_id":"`+rheaTestMCA+`"}`, `"`+amount+`"`)
}

func sweepDest() *uint32 {
	var a uint32
	return &a
}

// sweepOutTree scripts a sweep exec to a deposit address: the GasPayment
// and the transfer, each a child of the exec. With sent empty, sendFailed
// adds the transfer receipt as failed; otherwise it is absent.
func (f *fakeRheaChain) sweepOutTree(hash string, args []byte, fee, sent string, sendFailed bool) {
	root := "r-" + hash
	rs := []synthReceipt{{id: "gas-" + hash, parent: root, predecessor: rheaTestMCA, receiver: rhea.Mainnet.Relayer, transfer: fee}}
	switch {
	case sent != "":
		rs = append(rs, synthReceipt{id: "send-" + hash, parent: root, predecessor: rheaTestMCA, receiver: usdcTokenForTest,
			method: "ft_transfer", logs: []string{nep141Log(rheaTestMCA, supplyDepositAddr, sent)}})
	case sendFailed:
		rs = append(rs, synthReceipt{id: "send-" + hash, parent: root, predecessor: rheaTestMCA, receiver: usdcTokenForTest,
			method: "storage_deposit", failed: true})
	}
	f.synthTx(hash, rhea.Mainnet.Relayer, rheaTestMCA, "exec", args, true, rs...)
}

// sweepSupplyFailedTree scripts a sweep exec into supply whose
// ft_transfer_call failed at the token: the GasPayment, then the failed
// call with no transfer.
func (f *fakeRheaChain) sweepSupplyFailedTree(hash string, args []byte, fee string) {
	root := "r-" + hash
	f.synthTx(hash, rhea.Mainnet.Relayer, rheaTestMCA, "exec", args, true,
		synthReceipt{id: "gas-" + hash, parent: root, predecessor: rheaTestMCA, receiver: rhea.Mainnet.Relayer, transfer: fee},
		synthReceipt{id: "call-" + hash, parent: root, predecessor: rheaTestMCA, receiver: usdcTokenForTest, method: "ft_transfer_call", failed: true})
}

// sweepSupplyTree scripts a sweep exec into supply: the GasPayment, then
// ft_transfer_call to Burrow with its ft_on_transfer (Burrow's deposit
// event for what it used) and ft_resolve_transfer.
func (f *fakeRheaChain) sweepSupplyTree(hash string, args []byte, fee, amount, used string, depositEvent bool) {
	root := "r-" + hash
	call := "call-" + hash
	otr := synthReceipt{id: "otr-" + hash, parent: call, predecessor: usdcTokenForTest, receiver: rhea.Mainnet.Burrow, method: "ft_on_transfer"}
	if depositEvent && used != "0" {
		otr.logs = []string{burrowLog(rhea.BurrowEventDeposit, rheaTestMCA, usdcTokenForTest, internal12(used))}
	}
	u, _ := new(big.Int).SetString(used, 10)
	a, _ := new(big.Int).SetString(amount, 10)
	otr.result = `"` + new(big.Int).Sub(a, u).String() + `"`
	res := synthReceipt{id: "res-" + hash, parent: call, predecessor: usdcTokenForTest, receiver: usdcTokenForTest, method: "ft_resolve_transfer",
		args: []byte(`{"sender_id":"` + rheaTestMCA + `","receiver_id":"` + rhea.Mainnet.Burrow + `","amount":"` + amount + `"}`), result: `"` + used + `"`}
	if refund := new(big.Int).Sub(a, u); refund.Sign() > 0 {
		res.logs = []string{nep141Log(rhea.Mainnet.Burrow, rheaTestMCA, refund.String())}
	}
	f.synthTx(hash, rhea.Mainnet.Relayer, rheaTestMCA, "exec", args, true,
		synthReceipt{id: "gas-" + hash, parent: root, predecessor: rheaTestMCA, receiver: rhea.Mainnet.Relayer, transfer: fee},
		synthReceipt{id: call, parent: root, predecessor: rheaTestMCA, receiver: usdcTokenForTest, method: "ft_transfer_call",
			logs: []string{nep141Log(rheaTestMCA, rhea.Mainnet.Burrow, amount)}},
		otr, res)
}

func TestRheaSweepOut(t *testing.T) {
	r := newSweepRig(t)
	c := r.rig.core
	pv, err := c.RheaSweepPreview(tCtx, usdcTokenForTest, sweepDest(), "")
	if err != nil {
		t.Fatalf("RheaSweepPreview: %v", err)
	}
	feeCap, _ := rheaFeeCap(3, 24)
	// 0.001 NEAR plus 0.00125 NEAR for the deposit address's registration,
	// worth 6750 USDC base units at 3 USDC per NEAR; the MCA's unlocked NEAR
	// is its balance less 494 bytes of storage.
	if pv.ToSupply || pv.Balance != "1000000" || pv.Attributed != "0" || pv.Recovered != "1000000" || pv.Amount != "1000000" ||
		pv.Fee != sweepOutFee || pv.FeeInToken != "6750" || pv.FeeCap != feeCap.String() ||
		pv.NEARAvailable != "152668019447165699999630" || pv.MinAtomsOut != 4950000 {
		t.Fatalf("preview %+v", pv)
	}
	if _, ok := r.mock.lastReq["appFees"]; ok {
		t.Fatalf("sweep quote carries app fees")
	}
	if r.mock.requestField("refundTo") != rheaTestMCA || r.mock.requestField("amount") != "1000000" ||
		r.mock.requestField("originAsset") != "nep141:"+usdcTokenForTest {
		t.Fatalf("sweep quote %v", r.mock.lastReq)
	}

	if _, err := c.RheaSweepExecute(nil, usdcTokenForTest, sweepDest(), "", 4950000, sweepOutFee); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("no password: %v", err)
	}
	if _, err := c.RheaSweepExecute(tPW, usdcTokenForTest, sweepDest(), "", 4950000, "2249999999999999999999"); err == nil || !strings.Contains(err.Error(), "fee rose") {
		t.Fatalf("fee rise: %v", err)
	}
	if _, err := c.RheaSweepExecute(tPW, usdcTokenForTest, sweepDest(), "", 4950001, sweepOutFee); err == nil || !strings.Contains(err.Error(), "rate moved") {
		t.Fatalf("floor: %v", err)
	}
	op, err := c.RheaSweepExecute(tPW, usdcTokenForTest, sweepDest(), "", 4950000, sweepOutFee)
	if err != nil {
		t.Fatalf("RheaSweepExecute: %v", err)
	}
	if op.Kind != db.RheaOpSweep || op.State != db.RheaStateSubmitted || op.GasToken != rhea.GasTokenNEAR || op.Fee != sweepOutFee ||
		op.Leg == nil || op.Leg.RefundTo != rheaTestMCA || len(r.sub.submitted) != 1 {
		t.Fatalf("sweep op %+v", op)
	}
	sr, err := rhea.ParseSignedRequest(op.Relayer.SignedRequest)
	if err != nil {
		t.Fatal(err)
	}
	reqs := sr.Business.TxRequests
	if len(reqs) != 2 || reqs[0].GasPayment == nil || reqs[0].GasPayment.TokenID != "near" || reqs[0].GasPayment.Amount != sweepOutFee ||
		reqs[1].FunctionCall == nil || reqs[1].FunctionCall.ReceiverID != usdcTokenForTest {
		t.Fatalf("business %s", op.Relayer.SignedRequest)
	}
	// Another relayer business is not admitted while this one is pending.
	if _, err := c.RheaSweepPreview(tCtx, usdcTokenForTest, sweepDest(), ""); err == nil || !strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("second sweep: %v", err)
	}

	// Reconciliation runs without fiat rates.
	r.eng.tickerRates = tripRates(t)
	batchReports(r, "sweeptx")
	r.chain.sweepOutTree("sweeptx", execArgs(t, op.Relayer.SignedRequest), sweepOutFee, "1000000", false)
	r.mock.setStatus(supplyDepositAddr, intents.StatusProcessing)
	r.eng.pollOps(tCtx)
	got := r.op(t, op.ID)
	if got.State != db.RheaStateDelivering || got.Fees != sweepOutFee || got.Leg.DepositTxID != "sweeptx" {
		t.Fatalf("executed: %+v (%s)", got, got.Notes)
	}
	r.mock.setStatus(supplyDepositAddr, intents.StatusSuccess)
	r.eng.pollOps(tCtx)
	if got = r.op(t, op.ID); got.State != db.RheaStateWithdrawn || got.Active() {
		t.Fatalf("delivered: %+v", got)
	}
}

// TestRheaSweepFeeCapPrice needs no rate for the market token, prices the
// 2 USD NEAR fee cap at Caribou's NEAR rate when current and at the fresh
// 1Click catalog's wNEAR price otherwise, refuses only when neither is
// usable, and prices again at execute.
func TestRheaSweepFeeCapPrice(t *testing.T) {
	r := newSweepRig(t)
	c := r.rig.core
	ieng, _ := r.eng.oneClick()
	preview := func(dest *uint32) *RheaSweepPreview {
		t.Helper()
		pv, err := c.RheaSweepPreview(tCtx, usdcTokenForTest, dest, "")
		if err != nil {
			t.Fatalf("RheaSweepPreview: %v", err)
		}
		return pv
	}
	execute := func(pv *RheaSweepPreview) (*RheaOp, error) {
		return c.RheaSweepExecute(tPW, usdcTokenForTest, sweepDest(), "", pv.MinAtomsOut, pv.Fee)
	}
	nothingSent := func() {
		t.Helper()
		if ops, _ := r.rig.db.RheaOps(); len(ops) != 0 || len(r.sub.submitted) != 0 {
			t.Fatalf("refused sweep left %d ops, %d submissions", len(ops), len(r.sub.submitted))
		}
	}
	refetch := func(price float64) {
		r.mock.mtx.Lock()
		r.mock.wnearPrice = price
		r.mock.mtx.Unlock()
		ieng.catMtx.Lock()
		ieng.catalog, ieng.catalogFetch = nil, false
		ieng.catMtx.Unlock()
	}

	// No USDC rate is needed. 2 USD at 4 USD per NEAR is 0.5 NEAR; the
	// catalog's wNEAR price is 3.
	r.setRate("USDC", 0)
	r.setRate("NEAR", 4)
	pv := preview(sweepDest())
	if pv.FeeCap != "500000000000000000000000" || pv.FeeCapSource != RheaFeeCapFiatRate {
		t.Fatalf("fiat rate cap %s (%s)", pv.FeeCap, pv.FeeCapSource)
	}

	// No NEAR rate: the catalog's wNEAR price of 3 sets the cap, out and
	// back into supply.
	r.setRate("NEAR", 0)
	catalogCap, _ := rheaFeeCap(3, 24)
	for _, dest := range []*uint32{sweepDest(), nil} {
		if pv := preview(dest); pv.FeeCap != catalogCap.String() || pv.FeeCapSource != RheaFeeCapCatalog {
			t.Fatalf("catalog cap (to supply %v) %s (%s)", dest == nil, pv.FeeCap, pv.FeeCapSource)
		}
	}

	// Neither: no NEAR rate and a stale catalog. The refusal names both.
	ieng.catMtx.Lock()
	ieng.catalog.FetchedAt = time.Now().Add(-time.Hour)
	ieng.catalogFetch = true // keep the background refresh from replacing it
	ieng.catMtx.Unlock()
	const neither = "no current USD rate for NEAR, and the 1Click price of wNEAR is not fresh; the 2 USD fee cap cannot be set"
	if _, err := c.RheaSweepPreview(tCtx, usdcTokenForTest, nil, ""); err == nil || err.Error() != neither {
		t.Fatalf("preview with neither price: %v", err)
	}
	if _, err := execute(pv); err == nil || err.Error() != neither {
		t.Fatalf("execute with neither price: %v", err)
	}
	nothingSent()

	// Previewed at the NEAR rate, executed at the catalog price: the cap
	// holds at execute. At 1e6 USD per wNEAR it is 2e18 yocto, below the
	// fee, so the execute is refused.
	refetch(3)
	r.setRate("NEAR", 4)
	pv = preview(sweepDest())
	r.setRate("NEAR", 0)
	refetch(1e6)
	if _, err := execute(pv); err == nil || !strings.Contains(err.Error(), "exceeds the 2 USD cap") {
		t.Fatalf("execute over the catalog cap: %v", err)
	}
	nothingSent()
	// At the catalog's 3 the switched execute proceeds.
	refetch(3)
	if _, err := execute(pv); err != nil {
		t.Fatalf("execute at the catalog price: %v", err)
	}
}

// TestRheaSweepOutNotSent ends a sweep out as EXEC_FAILED only on a failed
// step with nothing leaving the account; an exec that shows neither a
// transfer nor a failure stays EXECUTED until its evidence settles.
func TestRheaSweepOutNotSent(t *testing.T) {
	r := newSweepRig(t)
	op, err := r.rig.core.RheaSweepExecute(tPW, usdcTokenForTest, sweepDest(), "", 4950000, sweepOutFee)
	if err != nil {
		t.Fatal(err)
	}
	args := execArgs(t, op.Relayer.SignedRequest)
	batchReports(r, "sweeptx")
	r.chain.sweepOutTree("sweeptx", args, sweepOutFee, "", false)
	r.eng.pollOps(tCtx)
	got := r.op(t, op.ID)
	if got.State != db.RheaStateExecuted || !got.Active() || got.Relayer.TxHash != "sweeptx" || !strings.Contains(got.Notes, "unresolved") {
		t.Fatalf("no evidence: %+v (%s)", got, got.Notes)
	}
	// Retried with backoff: an immediate poll reads nothing.
	calls := r.chain.txCalls
	r.eng.pollOps(tCtx)
	if r.chain.txCalls != calls {
		t.Fatalf("retried during backoff")
	}
	// The evidence now shows the transfer failed.
	r.chain.sweepOutTree("sweeptx", args, sweepOutFee, "", true)
	r.eng.clearAttributionBackoff(op.ID)
	r.eng.pollOps(tCtx)
	if got = r.op(t, op.ID); got.State != db.RheaStateExecFailed || got.Fees != sweepOutFee || got.Stranded != "" || got.Active() {
		t.Fatalf("send failed: %+v (%s)", got, got.Notes)
	}

	// An unresolved sweep out whose evidence then shows the transfer is
	// delivering.
	r = newSweepRig(t)
	op, err = r.rig.core.RheaSweepExecute(tPW, usdcTokenForTest, sweepDest(), "", 4950000, sweepOutFee)
	if err != nil {
		t.Fatal(err)
	}
	args = execArgs(t, op.Relayer.SignedRequest)
	batchReports(r, "sweeptx")
	r.chain.sweepOutTree("sweeptx", args, sweepOutFee, "", false)
	r.eng.pollOps(tCtx)
	r.chain.sweepOutTree("sweeptx", args, sweepOutFee, "1000000", false)
	r.mock.setStatus(supplyDepositAddr, intents.StatusProcessing)
	r.eng.clearAttributionBackoff(op.ID)
	r.eng.pollOps(tCtx)
	if got = r.op(t, op.ID); got.State != db.RheaStateDelivering || got.Leg.Status != intents.StatusProcessing {
		t.Fatalf("corrected evidence: %+v (%s)", got, got.Notes)
	}
}

func TestRheaSweepSupply(t *testing.T) {
	type script func(c *fakeRheaChain, hash string, args []byte)
	supplyTree := func(amount, used string, depositEvent bool) script {
		return func(c *fakeRheaChain, hash string, args []byte) {
			c.sweepSupplyTree(hash, args, sweepGasFeeNEAR, amount, used, depositEvent)
		}
	}
	tests := []struct {
		name     string
		script   script
		state    string
		supplied string
		stranded string
	}{
		{"all supplied", supplyTree("1000000", "1000000", true), db.RheaStateSupplied, "1000000", ""},
		{"part supplied", supplyTree("1000000", "600000", true), db.RheaStatePartial, "600000", "400000"},
		{"returned in full", supplyTree("1000000", "0", true), db.RheaStateExecFailed, "", ""},
		{"transfer failed", func(c *fakeRheaChain, hash string, args []byte) {
			c.sweepSupplyFailedTree(hash, args, sweepGasFeeNEAR)
		}, db.RheaStateExecFailed, "", ""},
		// Missing or contradictory evidence is unresolved, not failure.
		{"no deposit event", supplyTree("1000000", "1000000", false), db.RheaStateExecuted, "", ""},
		{"wrong amount", supplyTree("999999", "999999", true), db.RheaStateExecuted, "", ""},
		{"no resolution", func(c *fakeRheaChain, hash string, args []byte) {
			c.sweepOutTree(hash, args, sweepGasFeeNEAR, "", false)
		}, db.RheaStateExecuted, "", ""},
	}
	for _, tt := range tests {
		r := newSweepRig(t)
		c := r.rig.core
		pv, err := c.RheaSweepPreview(tCtx, usdcTokenForTest, nil, "")
		if err != nil {
			t.Fatalf("%s: RheaSweepPreview: %v", tt.name, err)
		}
		// No storage deposit in a sweep into supply: the NEAR fee alone.
		if !pv.ToSupply || pv.Fee != sweepGasFeeNEAR || pv.FeeInToken != "3000" || pv.AtomsOut != 0 {
			t.Fatalf("%s: preview %+v", tt.name, pv)
		}
		op, err := c.RheaSweepExecute(tPW, usdcTokenForTest, nil, "", 0, sweepGasFeeNEAR)
		if err != nil {
			t.Fatalf("%s: RheaSweepExecute: %v", tt.name, err)
		}
		if op.Leg != nil {
			t.Fatalf("%s: sweep into supply has a 1Click leg", tt.name)
		}
		args := execArgs(t, op.Relayer.SignedRequest)
		batchReports(r, "sweeptx")
		tt.script(r.chain, "sweeptx", args)
		r.eng.pollOps(tCtx)
		got := r.op(t, op.ID)
		if got.State != tt.state || got.Supplied != tt.supplied || got.Stranded != tt.stranded || got.Fees != sweepGasFeeNEAR ||
			got.Active() != (tt.state == db.RheaStateExecuted) || got.Relayer.TxHash != "sweeptx" {
			t.Errorf("%s: %s supplied %q stranded %q fees %q active %t (%s)", tt.name, got.State, got.Supplied, got.Stranded,
				got.Fees, got.Active(), got.Notes)
			continue
		}
		if tt.state != db.RheaStateExecuted {
			continue
		}
		// The dispatcher retries a sweep into supply without a 1Click leg,
		// and corrected evidence resolves it.
		r.chain.sweepSupplyTree("sweeptx", args, sweepGasFeeNEAR, "1000000", "1000000", true)
		r.eng.clearAttributionBackoff(op.ID)
		r.eng.pollOps(tCtx)
		if got = r.op(t, op.ID); got.State != db.RheaStateSupplied || got.Supplied != "1000000" || got.Active() {
			t.Errorf("%s: corrected evidence: %+v (%s)", tt.name, got, got.Notes)
		}
	}
}

func TestRheaSweepRefusals(t *testing.T) {
	tests := []struct {
		name   string
		dest   *uint32
		amount string
		setup  func(r *withdrawRig)
		errStr string
	}{
		{"unknown market", sweepDest(), "", func(r *withdrawRig) {}, "not a supported"},
		{"more than the balance", sweepDest(), "1000001", func(r *withdrawRig) {}, "holds only"},
		{"empty balance", nil, "", func(r *withdrawRig) { r.setPlainBalance("0") }, "no USDC to sweep"},
		{"fee over 2 %", nil, "", func(r *withdrawRig) { r.setPlainBalance("149999") }, "exceeds 2%"},
		{"no NEAR fee", nil, "", func(r *withdrawRig) { delete(r.sub.cfg.GasFee, rhea.GasTokenNEAR) }, "no NEAR fee"},
		{"wrong fee account", nil, "", func(r *withdrawRig) { r.sub.cfg.CollectFeeAccountID = "evil.near" }, "collects fees"},
		{"too little NEAR", nil, "", func(r *withdrawRig) {
			r.chain.accounts[rheaTestMCA] = &near.AccountView{Amount: "5000000000000000000000", StorageUsage: 494, BlockHash: rheaTestBlockHash}
		}, "unlocked NEAR"},
		{"supply being delivered", nil, "", func(r *withdrawRig) {
			if err := r.rig.db.StoreRheaOp(&db.RheaOp{ID: "s1", Kind: db.RheaOpSupply, State: db.RheaStateDelivered, MCA: rheaTestMCA,
				TokenID: usdcTokenForTest, Leg: &db.RheaLeg{DepositAddress: "x", Status: intents.StatusSuccess}, CreatedAt: time.Now()}); err != nil {
				panic(err)
			}
		}, "still being delivered"},
		{"admission window", nil, "", func(r *withdrawRig) {
			acct, _ := r.rig.db.RheaAccount(rhea.Mainnet.AccountManager, 1, string(r.id))
			acct.FirstSeenTime = rheaTestBlockTime.Add(-time.Minute)
			_ = r.rig.db.StoreRheaAccount(acct)
		}, "not admitted"},
	}
	for _, tt := range tests {
		r := newSweepRig(t)
		tt.setup(r)
		token := usdcTokenForTest
		if tt.name == "unknown market" {
			token = "evil.near"
		}
		if _, err := r.rig.core.RheaSweepPreview(tCtx, token, tt.dest, tt.amount); err == nil || !strings.Contains(err.Error(), tt.errStr) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

// TestRheaSweepCovers separates attributed STRANDED funds from recovered
// balance, and settles the attributed records once a sweep moves them.
func TestRheaSweepCovers(t *testing.T) {
	r := newSweepRig(t)
	c := r.rig.core
	if err := r.rig.db.StoreRheaOp(&db.RheaOp{ID: "w1", Kind: db.RheaOpWithdraw, State: db.RheaStateStranded, MCA: rheaTestMCA,
		TokenID: usdcTokenForTest, Stranded: "400000", Relayer: &db.RheaRelayerLeg{}, Leg: &db.RheaLeg{}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pv, err := c.RheaSweepPreview(tCtx, usdcTokenForTest, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if pv.Attributed != "400000" || pv.Recovered != "600000" {
		t.Fatalf("preview %+v", pv)
	}
	// A sweep of less than the attributed funds settles no record.
	op, err := c.RheaSweepExecute(tPW, usdcTokenForTest, nil, "300000", 0, sweepGasFeeNEAR)
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Covers) != 0 {
		t.Fatalf("partial sweep covers %v", op.Covers)
	}
	batchReports(r, "sweep1")
	r.chain.sweepSupplyTree("sweep1", execArgs(t, op.Relayer.SignedRequest), sweepGasFeeNEAR, "300000", "300000", true)
	r.eng.pollOps(tCtx)
	if got := r.op(t, op.ID); got.State != db.RheaStateSupplied {
		t.Fatalf("first sweep: %+v (%s)", got, got.Notes)
	}

	// A sweep of everything covers the record; once it moved the tokens,
	// the record no longer counts as attributed funds in the account.
	r.setPlainBalance("700000")
	r.chain.setView(rheaTestMCA, "get_nonce", `{}`, `"91"`)
	acct, _ := r.rig.db.RheaAccount(rhea.Mainnet.AccountManager, 1, string(r.id))
	acct.VerifiedBelow = 91
	_ = r.rig.db.StoreRheaAccount(acct)
	op, err = c.RheaSweepExecute(tPW, usdcTokenForTest, nil, "", 0, sweepGasFeeNEAR)
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Covers) != 1 || op.Covers[0] != "w1" {
		t.Fatalf("full sweep covers %v", op.Covers)
	}
	r.sub.mtx.Lock()
	r.sub.batches["batch2"] = []*rhea.BatchRequest{{BatchID: "batch2", BatchStatus: rhea.BatchComplete, Result: &rhea.RequestResult{TxHash: "sweep2"}}}
	r.sub.mtx.Unlock()
	r.chain.sweepSupplyTree("sweep2", execArgs(t, op.Relayer.SignedRequest), sweepGasFeeNEAR, "700000", "500000", true)
	r.eng.pollOps(tCtx)
	if got := r.op(t, op.ID); got.State != db.RheaStatePartial || got.Stranded != "200000" {
		t.Fatalf("second sweep: %+v (%s)", got, got.Notes)
	}
	ops, _ := r.rig.db.RheaOps()
	attributed, ids := attributedStranded(ops, rheaTestMCA, usdcTokenForTest)
	if attributed.String() != "200000" || len(ids) != 1 || ids[0] != op.ID {
		t.Fatalf("attributed after the sweep: %s %v", attributed, ids)
	}
}

// TestRheaSweepGasMismatch credits no fee when the exec's GasPayment is not
// the signed amount.
func TestRheaSweepGasMismatch(t *testing.T) {
	r := newSweepRig(t)
	op, err := r.rig.core.RheaSweepExecute(tPW, usdcTokenForTest, nil, "", 0, sweepGasFeeNEAR)
	if err != nil {
		t.Fatal(err)
	}
	batchReports(r, "sweeptx")
	r.chain.sweepSupplyTree("sweeptx", execArgs(t, op.Relayer.SignedRequest), "999", "1000000", "1000000", true)
	r.eng.pollOps(tCtx)
	got := r.op(t, op.ID)
	if got.State != db.RheaStateSupplied || got.Fees != "" || !strings.Contains(got.Notes, "gas paid false") {
		t.Fatalf("gas mismatch: %+v (%s)", got, got.Notes)
	}
}
