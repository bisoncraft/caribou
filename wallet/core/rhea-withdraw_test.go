package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/intents"
	"github.com/bisoncraft/caribou/wallet/rhea"
)

// fakeSubmitter is the relayer's submission API.
type fakeSubmitter struct {
	mtx       sync.Mutex
	cfg       *rhea.LendingConfig
	cfgErr    error
	submitErr error
	submitted [][]byte
	batches   map[string][]*rhea.BatchRequest
}

func newFakeSubmitter() *fakeSubmitter {
	return &fakeSubmitter{
		cfg: &rhea.LendingConfig{
			CollectFeeAccountID: rhea.Mainnet.Relayer,
			GasFee:              map[string]*big.Int{usdcTokenForTest: big.NewInt(1000)},
		},
		batches: make(map[string][]*rhea.BatchRequest),
	}
}

func (f *fakeSubmitter) Config(context.Context) (*rhea.LendingConfig, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	return f.cfg, f.cfgErr
}

func (f *fakeSubmitter) Submit(_ context.Context, _ string, req *rhea.SignedRequest) (string, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	b, _ := req.MarshalJSON()
	f.submitted = append(f.submitted, b)
	if f.submitErr != nil {
		return "", f.submitErr
	}
	return fmt.Sprintf("batch%d", len(f.submitted)), nil
}

func (f *fakeSubmitter) BatchStatus(_ context.Context, batchID string) ([]*rhea.BatchRequest, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	return f.batches[batchID], nil
}

type withdrawRig struct {
	*supplyRig
	sub *fakeSubmitter
	id  rhea.Identity
}

// newWithdrawRig binds the rig's own identity to rhea001569 with 10 USDC of
// plain supply, an account record old enough and verified far enough for
// the admission gate, and a relayer that charges 1000 USDC base units.
func newWithdrawRig(t *testing.T) *withdrawRig {
	r := &withdrawRig{supplyRig: newSupplyRig(t, true), sub: newFakeSubmitter()}
	r.eng.submitter = r.sub
	r.eng.history = &fakeRheaHistory{}
	r.id = r.rig.core.rheaIdentities()[0].Identity
	r.chain.setView("multica.near", "get_mca_by_wallet", `{"wallet":{"EVM":"`+string(r.id)+`"}}`, `"`+rheaTestMCA+`"`)
	r.chain.setView(rhea.Mainnet.Burrow, "get_account_all_positions", `{"account_id":"`+rheaTestMCA+`"}`,
		`{"account_id":"`+rheaTestMCA+`","supplied":[{"token_id":"`+usdcTokenForTest+`","balance":"10000000000000000000","shares":"1"}],"positions":{}}`)
	if err := r.rig.db.StoreRheaAccount(&db.RheaAccount{
		AccountManager: rhea.Mainnet.AccountManager, Identity: string(r.id), DerivationVersion: 1, MCA: rheaTestMCA,
		FirstSeenTime: rheaTestBlockTime.Add(-time.Hour), VerifiedBelow: 90, LastLookup: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	r.eng.now = func() time.Time { return rheaTestBlockTime.Add(time.Second) }
	return r
}

func (r *withdrawRig) op(t *testing.T, id string) *RheaOp {
	t.Helper()
	op, err := r.rig.db.RheaOp(id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestRheaWithdrawPreviewExecute(t *testing.T) {
	r := newWithdrawRig(t)
	c := r.rig.core

	pv, err := c.RheaWithdrawPreview(tCtx, usdcTokenForTest, 0, "")
	if err != nil {
		t.Fatalf("RheaWithdrawPreview: %v", err)
	}
	// 0.001 USDC base fee plus 0.00125 NEAR at 3 USDC per NEAR; the most
	// withdrawable is the supply less the fee.
	if pv.Fee != "4750" || pv.MaxAmount != "9995250" || pv.Amount != "9995250" || pv.FeeCap != "2000000" ||
		pv.RegisterMCA || pv.Supplied != "10000000" || pv.MinAtomsOut != 4950000 {
		t.Fatalf("preview %+v", pv)
	}
	// Withdrawal quotes carry no application fee, refund to the MCA, and pay
	// the wallet.
	if _, ok := r.mock.lastReq["appFees"]; ok {
		t.Fatalf("withdrawal quote carries app fees: %s", r.mock.lastReq["appFees"])
	}
	if r.mock.requestField("refundTo") != rheaTestMCA || r.mock.requestField("recipient") != "bc1qrefund" ||
		r.mock.requestField("originAsset") != "nep141:"+usdcTokenForTest || r.mock.requestField("amount") != "9995250" {
		t.Fatalf("withdrawal quote %v", r.mock.lastReq)
	}

	// Bounds from the preview are enforced before anything is signed.
	if _, err := c.RheaWithdrawExecute(tPW, usdcTokenForTest, 0, "1000000", 4950001, "4750"); err == nil || !strings.Contains(err.Error(), "rate moved") {
		t.Fatalf("floor violation: %v", err)
	}
	if _, err := c.RheaWithdrawExecute(tPW, usdcTokenForTest, 0, "1000000", 4950000, "4749"); err == nil || !strings.Contains(err.Error(), "fee rose") {
		t.Fatalf("fee rise: %v", err)
	}
	if _, err := c.RheaWithdrawExecute(nil, usdcTokenForTest, 0, "1000000", 4950000, "4750"); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("no password: %v", err)
	}
	if ops, _ := r.rig.db.RheaOps(); len(ops) != 0 || len(r.sub.submitted) != 0 {
		t.Fatalf("refused withdrawals left %d ops, %d submissions", len(ops), len(r.sub.submitted))
	}

	op, err := c.RheaWithdrawExecute(tPW, usdcTokenForTest, 0, "1000000", 4950000, "4750")
	if err != nil {
		t.Fatalf("RheaWithdrawExecute: %v", err)
	}
	if op.State != db.RheaStateSubmitted || op.Relayer.BatchID != "batch1" || op.Amount != "1000000" || op.Fee != "4750" ||
		op.Relayer.Nonce != 90 || op.Leg.RefundTo != rheaTestMCA || op.WalletAssetID != 0 {
		t.Fatalf("withdrawal op %+v", op)
	}
	// What was submitted is exactly what was persisted, signed by the
	// identity over the business built for these parameters.
	if len(r.sub.submitted) != 1 || string(r.sub.submitted[0]) != string(op.Relayer.SignedRequest) {
		t.Fatalf("submitted request differs from the persisted one")
	}
	sr, err := rhea.ParseSignedRequest(op.Relayer.SignedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if signer, err := sr.Business.Signer(sr.Signature); err != nil || signer != r.id {
		t.Fatalf("signer %s, %v", signer, err)
	}
	canon, _ := sr.Business.Canonical()
	for _, want := range []string{`\"amount_with_inner_decimal\":\"4750000000000000\"`, `\"max_amount\":\"1000000000000000000\"`,
		`\"receiver_id\":\"` + supplyDepositAddr + `\",\"amount\":\"1000000\"`} {
		if !strings.Contains(string(canon), want) {
			t.Fatalf("business lacks %s:\n%s", want, canon)
		}
	}

	// One business per MCA in flight.
	if _, err := c.RheaWithdrawExecute(tPW, usdcTokenForTest, 0, "1000000", 4950000, "4750"); err == nil || !strings.Contains(err.Error(), "may still be admitted") {
		t.Fatalf("second withdrawal: %v", err)
	}
}

// TestRheaWithdrawFeeCapPrice prices the 2 USD relayer fee cap at Caribou's
// fiat rate when one is current and at the fresh 1Click catalog price
// otherwise, refuses only when neither is usable, and prices again at
// execute, where the source may differ from the preview's.
func TestRheaWithdrawFeeCapPrice(t *testing.T) {
	r := newWithdrawRig(t)
	c := r.rig.core
	r.mock.mtx.Lock()
	r.mock.usdcPrice = 0.5 // the catalog's price, unlike the 0.625 rate below
	r.mock.mtx.Unlock()
	ieng, _ := r.eng.oneClick()
	preview := func() *RheaWithdrawPreview {
		t.Helper()
		pv, err := c.RheaWithdrawPreview(tCtx, usdcTokenForTest, 0, "1000000")
		if err != nil {
			t.Fatalf("RheaWithdrawPreview: %v", err)
		}
		return pv
	}
	execute := func(pv *RheaWithdrawPreview) (*RheaOp, error) {
		return c.RheaWithdrawExecute(tPW, usdcTokenForTest, 0, "1000000", pv.MinAtomsOut, pv.Fee)
	}
	nothingSent := func() {
		t.Helper()
		if ops, _ := r.rig.db.RheaOps(); len(ops) != 0 || len(r.sub.submitted) != 0 {
			t.Fatalf("refused withdrawal left %d ops, %d submissions", len(ops), len(r.sub.submitted))
		}
	}

	// The fiat rate: 2 USD at 0.625 is 3.2 USDC.
	r.setRate("USDC", 0.625)
	pv := preview()
	if pv.FeeCap != "3200000" || pv.FeeCapSource != RheaFeeCapFiatRate || math.Abs(pv.FeeUSD-0.00296875) > 1e-12 {
		t.Fatalf("fiat rate cap %s (%s), fee USD %v", pv.FeeCap, pv.FeeCapSource, pv.FeeUSD)
	}

	// No rate: the catalog's 0.5 makes the cap 4 USDC, and the withdrawal
	// goes ahead.
	r.setRate("USDC", 0)
	if pv = preview(); pv.FeeCap != "4000000" || pv.FeeCapSource != RheaFeeCapCatalog || math.Abs(pv.FeeUSD-0.002375) > 1e-12 {
		t.Fatalf("catalog cap %s (%s), fee USD %v", pv.FeeCap, pv.FeeCapSource, pv.FeeUSD)
	}

	// Neither: no rate and a stale catalog. The refusal names both.
	ieng.catMtx.Lock()
	ieng.catalog.FetchedAt = time.Now().Add(-time.Hour)
	ieng.catalogFetch = true // keep the background refresh from replacing it
	ieng.catMtx.Unlock()
	const neither = "no current USD rate for USDC, and the 1Click price of USDC is not fresh; the 2 USD fee cap cannot be set"
	if _, err := c.RheaWithdrawPreview(tCtx, usdcTokenForTest, 0, "1000000"); err == nil || err.Error() != neither {
		t.Fatalf("preview with neither price: %v", err)
	}
	if _, err := execute(pv); err == nil || err.Error() != neither {
		t.Fatalf("execute with neither price: %v", err)
	}
	nothingSent()
	refetch := func(price float64) {
		r.mock.mtx.Lock()
		r.mock.usdcPrice = price
		r.mock.mtx.Unlock()
		ieng.catMtx.Lock()
		ieng.catalog, ieng.catalogFetch = nil, false
		ieng.catMtx.Unlock()
	}
	refetch(0.5)

	// Previewed at the fiat rate, executed at the catalog price: the cap
	// holds at execute. At 1000 USD per USDC it is 2000 base units, below
	// the 4750 fee, so the execute is refused.
	r.setRate("USDC", 1)
	pv = preview()
	if pv.FeeCapSource != RheaFeeCapFiatRate {
		t.Fatalf("preview source %s", pv.FeeCapSource)
	}
	r.setRate("USDC", 0)
	refetch(1000)
	if _, err := execute(pv); err == nil || !strings.Contains(err.Error(), "exceeds the 2 USD cap (2000)") {
		t.Fatalf("execute over the catalog cap: %v", err)
	}
	nothingSent()
	// At the catalog's 0.5 the switched execute proceeds.
	refetch(0.5)
	if _, err := execute(pv); err != nil {
		t.Fatalf("execute at the catalog price: %v", err)
	}
}

func TestRheaWithdrawRefusals(t *testing.T) {
	r := newWithdrawRig(t)
	c := r.rig.core
	preview := func(amount string) error {
		_, err := c.RheaWithdrawPreview(tCtx, usdcTokenForTest, 0, amount)
		return err
	}
	if err := preview("9995251"); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("over the maximum: %v", err)
	}
	if err := preview("237499"); err == nil || !strings.Contains(err.Error(), "2%") {
		t.Fatalf("fee above 2%%: %v", err)
	}
	if err := preview("237500"); err != nil {
		t.Fatalf("fee at 2%%: %v", err)
	}

	// The absolute cap: at 1000 USD per USDC the fee is worth far more
	// than 2 USD.
	r.setRate("USDC", 1000)
	if err := preview(""); err == nil || !strings.Contains(err.Error(), "2 USD") {
		t.Fatalf("fee above the USD cap: %v", err)
	}
	r.setRate("USDC", 1)

	// The relayer configuration must name the pinned fee account and
	// publish a fee for the token.
	r.sub.cfg = &rhea.LendingConfig{CollectFeeAccountID: "evil.near", GasFee: r.sub.cfg.GasFee}
	if err := preview(""); err == nil || !strings.Contains(err.Error(), "collects fees") {
		t.Fatalf("foreign fee account: %v", err)
	}
	r.sub.cfg = &rhea.LendingConfig{CollectFeeAccountID: rhea.Mainnet.Relayer, GasFee: map[string]*big.Int{}}
	if err := preview(""); err == nil || !strings.Contains(err.Error(), "no fee") {
		t.Fatalf("missing fee: %v", err)
	}
	r.sub.cfg = newFakeSubmitter().cfg

	// An unregistered MCA pays for its token registration too.
	r.chain.setView(usdcTokenForTest, "storage_balance_of", `{"account_id":"`+rheaTestMCA+`"}`, "null")
	pv, err := c.RheaWithdrawPreview(tCtx, usdcTokenForTest, 0, "")
	if err != nil || !pv.RegisterMCA || pv.Fee != "8500" {
		t.Fatalf("registration fee: %+v, %v", pv, err)
	}

	// Collateral is never withdrawn: with only collateral there is nothing.
	r.chain.setView(rhea.Mainnet.Burrow, "get_account_all_positions", `{"account_id":"`+rheaTestMCA+`"}`,
		`{"account_id":"`+rheaTestMCA+`","supplied":[],"positions":{"REGULAR":{"collateral":[{"token_id":"`+usdcTokenForTest+`","balance":"10000000000000000000","shares":"1"}],"borrowed":[]}}}`)
	if err := preview(""); err == nil || !strings.Contains(err.Error(), "nothing is withdrawable") {
		t.Fatalf("collateral only: %v", err)
	}

	// The admission gate must be open.
	r2 := newWithdrawRig(t)
	acct, _ := r2.rig.db.RheaAccount(rhea.Mainnet.AccountManager, 1, string(r2.id))
	acct.VerifiedBelow = 80
	_ = r2.rig.db.StoreRheaAccount(acct)
	if _, err := r2.rig.core.RheaWithdrawPreview(tCtx, usdcTokenForTest, 0, ""); err == nil || !strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("closed gate: %v", err)
	}
}

type xfer struct {
	token, from, to, amount string
}

// synthReceipt is one receipt of a scripted transaction. parent names the
// receipt that created it; empty means the transaction itself.
type synthReceipt struct {
	id, parent                    string
	predecessor, receiver, method string
	logs                          []string
	failed                        bool
	// args are the call's arguments ("{}" when nil); result is its JSON
	// return value; transfer, when set, makes the receipt a native NEAR
	// Transfer of that many yoctoNEAR instead of a call.
	args     []byte
	result   string
	transfer string
}

func eventLog(standard, event string, data map[string]any) string {
	b, _ := json.Marshal(map[string]any{"standard": standard, "version": "1.0.0", "event": event, "data": []any{data}})
	return "EVENT_JSON:" + string(b)
}

func nep141Log(from, to, amount string) string {
	return eventLog("nep141", "ft_transfer", map[string]any{"old_owner_id": from, "new_owner_id": to, "amount": amount})
}

func burrowLog(event, account, token, amount string) string {
	return eventLog("burrow", event, map[string]any{"account_id": account, "token_id": token, "amount": amount})
}

func dip4WithdrawLog(account, token, receiver, amount, msg string) string {
	return eventLog("dip4", "ft_withdraw", map[string]any{"account_id": account, "token": token, "receiver_id": receiver, "amount": amount, "msg": msg})
}

// synthTx scripts a transaction whose first receipt calls method on
// receiver with args, plus the given receipts.
func (f *fakeRheaChain) synthTx(hash, signer, receiver, method string, args []byte, final bool, rs ...synthReceipt) {
	action := func(m string, a []byte) map[string]any {
		return map[string]any{"FunctionCall": map[string]any{
			"method_name": m, "args": base64.StdEncoding.EncodeToString(a), "gas": 300000000000000, "deposit": rhea.StorageDeposit,
		}}
	}
	root := "r-" + hash
	children := map[string][]any{}
	for _, r := range rs {
		parent := r.parent
		if parent == "" {
			parent = "tx"
		}
		children[parent] = append(children[parent], r.id)
	}
	receipts := []any{map[string]any{
		"receipt_id": root, "predecessor_id": signer, "receiver_id": receiver,
		"receipt": map[string]any{"Action": map[string]any{"actions": []any{action(method, args)}}},
	}}
	outcomes := []any{map[string]any{"id": root, "block_hash": "b", "outcome": map[string]any{
		"executor_id": receiver, "logs": []any{}, "receipt_ids": append([]any{}, children[root]...), "status": map[string]any{"SuccessValue": ""},
	}}}
	for _, r := range rs {
		args := r.args
		if args == nil {
			args = []byte("{}")
		}
		act := action(r.method, args)
		if r.transfer != "" {
			act = map[string]any{"Transfer": map[string]any{"deposit": r.transfer}}
		}
		receipts = append(receipts, map[string]any{
			"receipt_id": r.id, "predecessor_id": r.predecessor, "receiver_id": r.receiver,
			"receipt": map[string]any{"Action": map[string]any{"actions": []any{act}}},
		})
		status := map[string]any{"SuccessValue": base64.StdEncoding.EncodeToString([]byte(r.result))}
		if r.failed {
			status = map[string]any{"Failure": map[string]any{"ActionError": map[string]any{}}}
		}
		logs := []any{}
		for _, l := range r.logs {
			logs = append(logs, l)
		}
		outcomes = append(outcomes, map[string]any{"id": r.id, "block_hash": "b", "outcome": map[string]any{
			"executor_id": r.receiver, "logs": logs, "receipt_ids": append([]any{}, children[r.id]...), "status": status,
		}})
	}
	fes := rhea.FinalExecutionFinal
	if !final {
		fes = "EXECUTED_OPTIMISTIC"
	}
	tx := map[string]any{
		"final_execution_status": fes,
		"transaction":            map[string]any{"hash": hash, "signer_id": signer, "receiver_id": receiver, "actions": []any{action(method, args)}},
		"transaction_outcome": map[string]any{"id": hash, "block_hash": "b", "outcome": map[string]any{
			"executor_id": signer, "logs": []any{}, "receipt_ids": append([]any{root}, children["tx"]...), "status": map[string]any{"SuccessReceiptId": root},
		}},
		"receipts":         receipts,
		"receipts_outcome": outcomes,
	}
	b, _ := json.Marshal(tx)
	f.mtx.Lock()
	f.txs[hash] = b
	f.mtx.Unlock()
}

// withdrawSteps describes what a scripted withdrawal exec did: amounts in
// token units, empty for a step that moved nothing.
type withdrawSteps struct {
	fee, withdrawn, sent string
	// burrowEvents omits Burrow's withdraw events when false.
	noBurrowEvents bool
	// extra receipts, such as siblings outside the exec.
	extra []synthReceipt
}

func internal12(amount string) string {
	return amount + "000000000000"
}

// execTree scripts a withdrawal exec laid out as the mainnet exec 4SDDqzRj
// is: each step a child of the exec, with Burrow's withdraw_succeeded in
// the step's callback.
func (f *fakeRheaChain) execTree(hash string, args []byte, final bool, st withdrawSteps) {
	root := "r-" + hash
	var rs []synthReceipt
	step := func(name, method, to, amount string) {
		call := name + "-" + hash
		rs = append(rs, synthReceipt{id: call, parent: root, predecessor: rheaTestMCA, receiver: rhea.Mainnet.Burrow, method: method})
		rs = append(rs, synthReceipt{id: call + "-xfer", parent: call, predecessor: rhea.Mainnet.Burrow, receiver: usdcTokenForTest,
			method: "ft_transfer", logs: []string{nep141Log(rhea.Mainnet.Burrow, to, amount)}})
		cb := synthReceipt{id: call + "-cb", parent: call, predecessor: rhea.Mainnet.Burrow, receiver: rhea.Mainnet.Burrow, method: "after_ft_transfer"}
		if !st.noBurrowEvents {
			cb.logs = []string{burrowLog(rhea.BurrowEventWithdrawSucceeded, rheaTestMCA, usdcTokenForTest, internal12(amount))}
		}
		rs = append(rs, cb)
	}
	if st.fee != "" {
		step("fee", "simple_withdraw", rhea.Mainnet.Relayer, st.fee)
	}
	if st.withdrawn != "" {
		step("withdraw", "execute", rheaTestMCA, st.withdrawn)
	}
	if st.sent != "" {
		rs = append(rs, synthReceipt{id: "send-" + hash, parent: root, predecessor: rheaTestMCA, receiver: usdcTokenForTest,
			method: "ft_transfer", logs: []string{nep141Log(rheaTestMCA, supplyDepositAddr, st.sent)}})
	}
	rs = append(rs, st.extra...)
	f.synthTx(hash, rhea.Mainnet.Relayer, rheaTestMCA, "exec", args, final, rs...)
}

// refundTree scripts a verifier refund payout to the MCA: the payout event
// in the verifier's receipt and the transfer in its child, plus extras.
func (f *fakeRheaChain) refundTree(hash, amount, msg string, transfer bool, extra ...synthReceipt) {
	root := "r-" + hash
	rs := []synthReceipt{}
	if transfer {
		rs = append(rs, synthReceipt{id: "pay-" + hash, parent: root, predecessor: rheaVerifier, receiver: usdcTokenForTest,
			method: "ft_transfer", logs: []string{nep141Log(rheaVerifier, rheaTestMCA, amount)}})
	}
	rs = append(rs, extra...)
	f.synthTx(hash, rheaVerifier, rheaVerifier, "execute_intents", []byte("{}"), true, rs...)
	// The payout event is logged by the verifier's own receipt.
	var m map[string]any
	_ = json.Unmarshal(f.txs[hash], &m)
	out := m["receipts_outcome"].([]any)[0].(map[string]any)["outcome"].(map[string]any)
	out["logs"] = []any{dip4WithdrawLog("depositaccount", usdcTokenForTest, rheaTestMCA, amount, msg)}
	b, _ := json.Marshal(m)
	f.mtx.Lock()
	f.txs[hash] = b
	f.mtx.Unlock()
}

// execArgs builds exec arguments from a persisted signed request.
func execArgs(t *testing.T, signed []byte) []byte {
	t.Helper()
	sr, err := rhea.ParseSignedRequest(signed)
	if err != nil {
		t.Fatal(err)
	}
	canon, _ := sr.Business.Canonical()
	return []byte(`{"business":` + string(canon) + `,"signature":"` + sr.Signature + `","signer_wallet":` + sr.SignerWallet.String() + `}`)
}

// submittedWithdraw executes a withdrawal of 1 USDC on a fresh rig. The
// rig's fiat rates then fail the test when read, so reconciliation of the
// withdrawal runs without them.
func submittedWithdraw(t *testing.T) (*withdrawRig, *RheaOp) {
	r := newWithdrawRig(t)
	op, err := r.rig.core.RheaWithdrawExecute(tPW, usdcTokenForTest, 0, "1000000", 4950000, "4750")
	if err != nil {
		t.Fatalf("RheaWithdrawExecute: %v", err)
	}
	r.eng.tickerRates = tripRates(t)
	return r, op
}

var fullSteps = withdrawSteps{fee: "4750", withdrawn: "1000000", sent: "1000000"}

func batchReports(r *withdrawRig, hash string) {
	r.sub.mtx.Lock()
	r.sub.batches["batch1"] = []*rhea.BatchRequest{{BatchID: "batch1", BatchStatus: rhea.BatchComplete,
		Result: &rhea.RequestResult{TxHash: hash}}}
	r.sub.mtx.Unlock()
}

func setRefunded(r *withdrawRig, refunded string, hashes ...string) {
	r.mock.mtx.Lock()
	defer r.mock.mtx.Unlock()
	hj, _ := json.Marshal(hashes)
	r.mock.statuses[supplyDepositAddr] = fmt.Sprintf(`{"status":"REFUNDED","updatedAt":"x","swapDetails":{"nearTxHashes":%s,"refundedAmount":%q}}`, hj, refunded)
}

func TestRheaWithdrawExecuted(t *testing.T) {
	r, op := submittedWithdraw(t)
	// The relayer reports the exec; its receipts are pending at first.
	batchReports(r, "exectx")
	r.chain.execTree("exectx", execArgs(t, op.Relayer.SignedRequest), false, fullSteps)
	r.eng.pollOps(tCtx)
	if got := r.op(t, op.ID); got.State != db.RheaStateSubmitted || got.Relayer.TxHash != "exectx" {
		t.Fatalf("pending exec: %+v", got)
	}
	// Final: executed, and the 1Click leg is tracked.
	r.chain.execTree("exectx", execArgs(t, op.Relayer.SignedRequest), true, fullSteps)
	r.mock.setStatus(supplyDepositAddr, intents.StatusProcessing)
	r.eng.pollOps(tCtx)
	got := r.op(t, op.ID)
	if got.State != db.RheaStateDelivering || got.Leg.Status != intents.StatusProcessing || got.Fees != "4750" ||
		got.Stranded != "" || strings.Contains(got.Notes, "Burrow recorded") {
		t.Fatalf("executed: %+v (%s)", got, got.Notes)
	}
	r.mock.setStatus(supplyDepositAddr, intents.StatusSuccess)
	r.eng.pollOps(tCtx)
	if got = r.op(t, op.ID); got.State != db.RheaStateWithdrawn || got.Active() {
		t.Fatalf("withdrawn: %+v", got)
	}
}

// TestRheaWithdrawPartialExecs covers execs that did part of their work:
// whatever reached the deposit address is tracked as a 1Click leg, and
// whatever stayed in the MCA is recorded there.
func TestRheaWithdrawPartialExecs(t *testing.T) {
	tests := []struct {
		name        string
		steps       withdrawSteps
		state       string
		fees        string
		stranded    string
		notesSubstr string
	}{
		{"withdrawn, not sent", withdrawSteps{fee: "4750", withdrawn: "1000000"}, db.RheaStateExecFailed, "4750", "1000000", "left in the account"},
		{"partly withdrawn, not sent", withdrawSteps{fee: "4750", withdrawn: "500000"}, db.RheaStateExecFailed, "4750", "500000", "left in the account"},
		{"nothing moved but the fee", withdrawSteps{fee: "4750"}, db.RheaStateExecFailed, "4750", "", "sent 0"},
		{"sent without the fee", withdrawSteps{withdrawn: "1000000", sent: "1000000"}, db.RheaStateDelivering, "", "", "fee paid false"},
		{"partly withdrawn, sent from the account's balance", withdrawSteps{fee: "4750", withdrawn: "500000", sent: "1000000"},
			db.RheaStateDelivering, "4750", "", "more was sent than withdrawn"},
		{"withdrawn without Burrow's record", withdrawSteps{fee: "4750", withdrawn: "1000000", sent: "1000000", noBurrowEvents: true},
			db.RheaStateDelivering, "", "", "Burrow recorded"},
	}
	for _, tt := range tests {
		r, op := submittedWithdraw(t)
		batchReports(r, "exectx")
		r.chain.execTree("exectx", execArgs(t, op.Relayer.SignedRequest), true, tt.steps)
		r.mock.setStatus(supplyDepositAddr, intents.StatusProcessing)
		r.eng.pollOps(tCtx)
		got := r.op(t, op.ID)
		if got.State != tt.state || got.Fees != tt.fees || got.Stranded != tt.stranded || !strings.Contains(got.Notes, tt.notesSubstr) {
			t.Errorf("%s: state %s fees %q stranded %q (%s)", tt.name, got.State, got.Fees, got.Stranded, got.Notes)
		}
		if tt.state == db.RheaStateDelivering && got.Leg.Status != intents.StatusProcessing {
			t.Errorf("%s: leg not tracked: %s", tt.name, got.Leg.Status)
		}
	}

	// A fee transfer outside the exec's receipts is someone else's.
	r, op := submittedWithdraw(t)
	batchReports(r, "exectx")
	r.chain.execTree("exectx", execArgs(t, op.Relayer.SignedRequest), true, withdrawSteps{withdrawn: "1000000", sent: "1000000",
		extra: []synthReceipt{
			{id: "sibling", predecessor: "other.multica.near", receiver: rhea.Mainnet.Burrow, method: "simple_withdraw"},
			{id: "sibling-xfer", parent: "sibling", predecessor: rhea.Mainnet.Burrow, receiver: usdcTokenForTest, method: "ft_transfer",
				logs: []string{nep141Log(rhea.Mainnet.Burrow, rhea.Mainnet.Relayer, "4750")}},
			{id: "sibling-cb", parent: "sibling", predecessor: rhea.Mainnet.Burrow, receiver: rhea.Mainnet.Burrow, method: "after_ft_transfer",
				logs: []string{burrowLog(rhea.BurrowEventWithdrawSucceeded, rheaTestMCA, usdcTokenForTest, internal12("4750"))}},
		}})
	r.mock.setStatus(supplyDepositAddr, intents.StatusProcessing)
	r.eng.pollOps(tCtx)
	if got := r.op(t, op.ID); got.State != db.RheaStateDelivering || got.Fees != "" {
		t.Fatalf("sibling fee credited: %+v (%s)", got, got.Notes)
	}

	// A refund after a partly failed exec is still followed and attributed.
	r, op = submittedWithdraw(t)
	batchReports(r, "exectx")
	r.chain.execTree("exectx", execArgs(t, op.Relayer.SignedRequest), true, withdrawSteps{withdrawn: "1000000", sent: "1000000"})
	r.mock.setStatus(supplyDepositAddr, intents.StatusProcessing)
	r.eng.pollOps(tCtx)
	r.chain.refundTree("refundtx", "1000000", "", true)
	setRefunded(r, "1000000", "exectx", "refundtx")
	r.eng.pollOps(tCtx)
	if got := r.op(t, op.ID); got.State != db.RheaStateStranded || got.Stranded != "1000000" {
		t.Fatalf("refund after a partial exec: %+v (%s)", got, got.Notes)
	}
}

func TestRheaWithdrawUnattributed(t *testing.T) {
	r, op := submittedWithdraw(t)
	// The relayer points at an exec of a different business; the nonce is
	// consumed, and no exec carrying this signature is in the history.
	other := strings.Replace(string(execArgs(t, op.Relayer.SignedRequest)), `"signature":"`, `"signature":"00`, 1)
	batchReports(r, "othertx")
	r.chain.execTree("othertx", []byte(other), true, fullSteps)
	r.chain.setView(rheaTestMCA, "get_nonce", `{}`, `"91"`)
	h := r.eng.history.(*fakeRheaHistory)
	h.add(rhea.HistoryTypeRelayer, "othertx")
	r.eng.pollOps(tCtx)
	if got := r.op(t, op.ID); got.State != db.RheaStateNonceConsumedUnattributed || !got.Active() {
		t.Fatalf("unattributed: %+v (%s)", got, got.Notes)
	}
	// The history search backs off: an immediate poll reads no history.
	h.pages = 0
	r.eng.pollOps(tCtx)
	if h.pages != 0 {
		t.Fatalf("history searched during backoff")
	}
	// The exec carrying the signature turns up in the history later.
	r.chain.execTree("ourtx", execArgs(t, op.Relayer.SignedRequest), true, fullSteps)
	h.add(rhea.HistoryTypeRelayer, "ourtx")
	r.mock.setStatus(supplyDepositAddr, intents.StatusProcessing)
	r.eng.clearAttributionBackoff(op.ID)
	r.eng.pollOps(tCtx)
	if got := r.op(t, op.ID); got.State != db.RheaStateDelivering || got.Relayer.TxHash != "ourtx" {
		t.Fatalf("found later: %+v (%s)", got, got.Notes)
	}
}

// TestRheaWithdrawSearchSkipsFailedReplay finds the admitted exec behind a
// newer failed exec of the same business (an identical resubmission the
// MCA rejected after the original executed).
func TestRheaWithdrawSearchSkipsFailedReplay(t *testing.T) {
	r := newWithdrawRig(t)
	r.sub.submitErr = errors.New("connection reset")
	op, err := r.rig.core.RheaWithdrawExecute(tPW, usdcTokenForTest, 0, "1000000", 4950000, "4750")
	if err != nil {
		t.Fatal(err)
	}
	args := execArgs(t, op.Relayer.SignedRequest)
	r.chain.execTree("originaltx", args, true, fullSteps)
	r.chain.execTree("replaytx", args, true, withdrawSteps{})
	// The replay's exec receipt failed.
	var m map[string]any
	_ = json.Unmarshal(r.chain.txs["replaytx"], &m)
	m["receipts_outcome"].([]any)[0].(map[string]any)["outcome"].(map[string]any)["status"] = map[string]any{"Failure": map[string]any{"ActionError": map[string]any{}}}
	r.chain.txs["replaytx"], _ = json.Marshal(m)
	h := r.eng.history.(*fakeRheaHistory)
	h.add(rhea.HistoryTypeRelayer, "originaltx")
	h.add(rhea.HistoryTypeRelayer, "replaytx") // newest first
	r.chain.setView(rheaTestMCA, "get_nonce", `{}`, `"91"`)
	r.mock.setStatus(supplyDepositAddr, intents.StatusProcessing)
	r.eng.pollOps(tCtx)
	if got := r.op(t, op.ID); got.State != db.RheaStateDelivering || got.Relayer.TxHash != "originaltx" {
		t.Fatalf("replay shadowed the original: %+v (%s)", got, got.Notes)
	}
}

func TestRheaWithdrawSubmitUnknown(t *testing.T) {
	r := newWithdrawRig(t)
	r.sub.submitErr = errors.New("connection reset")
	op, err := r.rig.core.RheaWithdrawExecute(tPW, usdcTokenForTest, 0, "1000000", 4950000, "4750")
	if err != nil {
		t.Fatalf("RheaWithdrawExecute: %v", err)
	}
	if op.State != db.RheaStateSubmitUnknown || op.Relayer.RelayerError == "" {
		t.Fatalf("submit unknown: %+v", op)
	}
	// Before the deadline the identical request is resubmitted, at most
	// once per interval.
	r.sub.submitErr = nil
	r.eng.pollOps(tCtx)
	if len(r.sub.submitted) != 2 || string(r.sub.submitted[1]) != string(op.Relayer.SignedRequest) {
		t.Fatalf("resubmission: %d submissions", len(r.sub.submitted))
	}
	if got := r.op(t, op.ID); got.State != db.RheaStateSubmitted || got.Relayer.BatchID != "batch2" {
		t.Fatalf("resubmitted: %+v", got)
	}
	// Past the deadline plus margin at a final block, an unconsumed nonce
	// concludes NOT_EXECUTED, flagged unconfirmed.
	r.chain.block.Timestamp = op.Relayer.Deadline.Add(rheaNotExecutedMargin + time.Second)
	r.eng.now = func() time.Time { return r.chain.block.Timestamp }
	r.eng.pollOps(tCtx)
	got := r.op(t, op.ID)
	if got.State != db.RheaStateNotExecuted || !got.Relayer.Unconfirmed || got.Active() {
		t.Fatalf("not executed: %+v (%s)", got, got.Notes)
	}
	// Inside the margin it waits.
	r2 := newWithdrawRig(t)
	r2.sub.submitErr = errors.New("timeout")
	op2, _ := r2.rig.core.RheaWithdrawExecute(tPW, usdcTokenForTest, 0, "1000000", 4950000, "4750")
	r2.chain.block.Timestamp = op2.Relayer.Deadline.Add(time.Minute)
	r2.eng.pollOps(tCtx)
	if got := r2.op(t, op2.ID); got.State != db.RheaStateSubmitUnknown || len(r2.sub.submitted) != 1 {
		t.Fatalf("inside the margin: %+v, %d submissions", got, len(r2.sub.submitted))
	}
}

// TestRheaWithdrawRefundEvidence attributes a refunded leg only on positive
// evidence of a plain verifier payout to the MCA that stayed there.
func TestRheaWithdrawRefundEvidence(t *testing.T) {
	delivering := func() (*withdrawRig, *RheaOp) {
		r, op := submittedWithdraw(t)
		batchReports(r, "exectx")
		r.chain.execTree("exectx", execArgs(t, op.Relayer.SignedRequest), true, fullSteps)
		r.mock.setStatus(supplyDepositAddr, intents.StatusProcessing)
		r.eng.pollOps(tCtx)
		return r, op
	}
	onward := synthReceipt{id: "onward", parent: "pay-refundtx", predecessor: rheaTestMCA, receiver: usdcTokenForTest,
		method: "ft_transfer_call", logs: []string{nep141Log(rheaTestMCA, rhea.Mainnet.Burrow, "1000000")}}
	tests := []struct {
		name     string
		refunded string
		script   func(c *fakeRheaChain)
		hashes   []string
		stranded bool
		notes    string
	}{
		{"plain payout", "1000000", func(c *fakeRheaChain) { c.refundTree("refundtx", "1000000", "", true) },
			[]string{"exectx", "refundtx"}, true, "refunded 1000000"},
		{"no refunded amount", "", func(c *fakeRheaChain) { c.refundTree("refundtx", "1000000", "", true) },
			[]string{"refundtx"}, false, "no refunded amount"},
		{"amount mismatch", "999999", func(c *fakeRheaChain) { c.refundTree("refundtx", "1000000", "", true) },
			[]string{"refundtx"}, false, "does not match"},
		{"payout with a message", "1000000", func(c *fakeRheaChain) { c.refundTree("refundtx", "1000000", "{}", true) },
			[]string{"refundtx"}, false, "message"},
		{"payout without its transfer", "1000000", func(c *fakeRheaChain) { c.refundTree("refundtx", "1000000", "", false) },
			[]string{"refundtx"}, false, "not in its receipts"},
		{"refund moved on", "1000000", func(c *fakeRheaChain) { c.refundTree("refundtx", "1000000", "", true, onward) },
			[]string{"refundtx"}, false, "moved on"},
		{"plain transfer without a payout", "1000000", func(c *fakeRheaChain) {
			c.synthTx("refundtx", rheaVerifier, rheaVerifier, "execute_intents", []byte("{}"), true,
				synthReceipt{id: "pay", parent: "r-refundtx", predecessor: rheaVerifier, receiver: usdcTokenForTest,
					method: "ft_transfer", logs: []string{nep141Log(rheaVerifier, rheaTestMCA, "1000000")}})
		}, []string{"refundtx"}, false, "no reported NEAR transaction shows the refund"},
		{"unknown transaction", "1000000", func(c *fakeRheaChain) { c.refundTree("refundtx", "1000000", "", true) },
			[]string{"refundtx", "unknownhash"}, false, "could not be read"},
	}
	for _, tt := range tests {
		r, op := delivering()
		tt.script(r.chain)
		setRefunded(r, tt.refunded, tt.hashes...)
		r.eng.pollOps(tCtx)
		got := r.op(t, op.ID)
		if tt.stranded {
			if got.State != db.RheaStateStranded || got.Stranded != "1000000" || got.Active() || !strings.Contains(got.Notes, tt.notes) {
				t.Errorf("%s: %s stranded %q (%s)", tt.name, got.State, got.Stranded, got.Notes)
			}
			continue
		}
		if got.State != intents.StatusRefunded || !got.Active() || !strings.Contains(got.Notes, tt.notes) {
			t.Errorf("%s: %s (%s)", tt.name, got.State, got.Notes)
		}
	}
}

func TestRheaFeeCap(t *testing.T) {
	for _, tt := range []struct {
		price    float64
		decimals uint8
		want     string
	}{
		{1, 6, "2000000"},
		{0.9996, 6, "2000800"},
		{900, 8, "222222"},
		{3.25, 24, "615384615384615384615384"},
	} {
		got, err := rheaFeeCap(tt.price, tt.decimals)
		if err != nil || got.String() != tt.want {
			t.Fatalf("rheaFeeCap(%v, %d) = %v, %v; want %s", tt.price, tt.decimals, got, err, tt.want)
		}
	}
	for _, bad := range []float64{0, -1, 1e-9} {
		if _, err := rheaFeeCap(bad, 6); err == nil {
			t.Fatalf("price %v accepted", bad)
		}
	}
	// The cap is worth at most 2 USD at the exact price, and one base unit
	// more is worth more. A price rounded to micro-USD (1.4e-6 to 1e-6)
	// would put the cap at 2.8 USD.
	two := big.NewRat(rheaFeeCapMicroUSD, 1_000_000)
	worth := func(units *big.Int, decimals uint8, price float64) *big.Rat {
		v := new(big.Rat).SetFrac(units, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
		return v.Mul(v, new(big.Rat).SetFloat64(price))
	}
	for _, price := range []float64{1e-6, 1.4e-6, 1.6e-6, 0.3333333, 0.9996, 1.0000006, 3.25, 899.99} {
		for _, decimals := range []uint8{6, 8, 24} {
			got, err := rheaFeeCap(price, decimals)
			if err != nil {
				t.Fatalf("price %v, decimals %d: %v", price, decimals, err)
			}
			if worth(got, decimals, price).Cmp(two) > 0 {
				t.Errorf("price %v, decimals %d: cap %s is worth %s USD", price, decimals, got,
					worth(got, decimals, price).FloatString(12))
			}
			if more := new(big.Int).Add(got, big.NewInt(1)); worth(more, decimals, price).Cmp(two) <= 0 {
				t.Errorf("price %v, decimals %d: cap %s is not the most worth 2 USD", price, decimals, got)
			}
		}
	}
}
