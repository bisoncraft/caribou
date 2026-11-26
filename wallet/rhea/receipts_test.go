package rhea

import (
	"encoding/json"
	"strings"
	"testing"
)

func loadTree(t *testing.T, name string) *TxTree {
	t.Helper()
	tree, err := DecodeTxTree(readTestdata(t, name))
	if err != nil {
		t.Fatalf("DecodeTxTree(%s): %v", name, err)
	}
	return tree
}

func TestTxTreesComplete(t *testing.T) {
	for _, name := range []string{
		"tx-create-supply-1aqcDa8Y.json",
		"tx-supply-existing-Ey4MNC5d.json",
		"tx-exec-4SDDqzRj.json",
		"tx-exec-gaspayment-4175G6iY.json",
	} {
		tree := loadTree(t, name)
		if err := tree.Complete(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(tree.Receipts()) == 0 || tree.Outcome == nil {
			t.Fatalf("%s: empty tree", name)
		}
	}
}

// TestCreationSupplyEvidence extracts the evidence of the mainnet MCA
// creation 1aqcDa8Y…: 0.629935 USDT delivered to the account manager, two
// 0.3 USDT fees, and 0.029935 USDT supplied to Burrow for rhea001594.
func TestCreationSupplyEvidence(t *testing.T) {
	tree := loadTree(t, "tx-create-supply-1aqcDa8Y.json")
	if tree.SignerID != "intents.near" {
		t.Fatalf("signer %s", tree.SignerID)
	}
	const usdt = "usdt.tether-token.near"
	const mca = "rhea001594.multica.near"

	ws, err := tree.VerifierWithdrawals("intents.near")
	if err != nil || len(ws) != 1 {
		t.Fatalf("verifier withdrawals %v, %v", ws, err)
	}
	if w := ws[0]; w.Token != usdt || w.ReceiverID != "multica.near" || w.Amount.Int64() != 629935 || !strings.Contains(w.Msg, TemplateBurrowRegisterAndSupply) {
		t.Fatalf("withdrawal %+v", w)
	}

	fees, err := tree.AccountManagerFees("multica.near")
	if err != nil || len(fees) != 2 {
		t.Fatalf("fees %v, %v", fees, err)
	}
	got := map[string]int64{}
	for _, f := range fees {
		if f.TokenID != usdt {
			t.Fatalf("fee token %s", f.TokenID)
		}
		got[f.Event] = f.Amount.Int64()
	}
	if got["collect_create_mca_fee"] != 300000 || got["collect_dapp_registration_fee"] != 300000 {
		t.Fatalf("fees %v", got)
	}

	xfers, err := tree.FTTransfers()
	if err != nil {
		t.Fatalf("FTTransfers: %v", err)
	}
	type hop struct {
		from, to string
		amt      int64
	}
	var hops []hop
	for _, x := range xfers {
		if x.Token != usdt {
			t.Fatalf("transfer token %s", x.Token)
		}
		hops = append(hops, hop{x.From, x.To, x.Amount.Int64()})
	}
	wantHops := []hop{{"intents.near", "multica.near", 629935}, {"multica.near", mca, 29935}, {mca, Mainnet.Burrow, 29935}}
	if len(hops) != len(wantHops) {
		t.Fatalf("hops %v", hops)
	}
	for i := range hops {
		if hops[i] != wantHops[i] {
			t.Fatalf("hop %d = %v, want %v", i, hops[i], wantHops[i])
		}
	}

	evs, err := tree.BurrowEvents(Mainnet.Burrow)
	if err != nil || len(evs) != 1 {
		t.Fatalf("burrow events %v, %v", evs, err)
	}
	if e := evs[0]; e.Event != BurrowEventDeposit || e.AccountID != mca || e.TokenID != usdt || e.Amount.String() != "29935000000000000" {
		t.Fatalf("deposit event %+v", e)
	}

	res, err := tree.ResolvedTransfers()
	if err != nil || len(res) != 3 {
		t.Fatalf("resolved transfers %v, %v", res, err)
	}
	for _, r := range res {
		if r.Token != usdt || r.Refunded().Sign() != 0 {
			t.Fatalf("resolution %+v refunded %s", r, r.Refunded())
		}
	}
	if calls, err := tree.ExecCalls(Mainnet); err != nil || len(calls) != 0 {
		t.Fatalf("exec calls in a supply: %v, %v", calls, err)
	}
}

// TestWithdrawExecEvidence extracts the evidence of the mainnet supply-only
// withdrawal 4SDDqzRj… on rhea000747: the signed business, the fee and
// withdrawal events, and the transfers to the relayer and the deposit
// address.
func TestWithdrawExecEvidence(t *testing.T) {
	tree := loadTree(t, "tx-exec-4SDDqzRj.json")
	const mca = "rhea000747.multica.near"
	const deposit = "0b3e64d9ca0d2a4aaff3b78494781de580da729c0bab921c3b857772c2bf3f45"

	calls, err := tree.ExecCalls(Mainnet)
	if err != nil || len(calls) != 1 {
		t.Fatalf("exec calls %v, %v", calls, err)
	}
	c := calls[0]
	if c.MCA != mca || c.PredecessorID != Mainnet.Relayer || !c.Outcome.Status.OK() || c.Deposit.String() != StorageDeposit {
		t.Fatalf("exec %+v", c)
	}
	if signer, err := c.Args.Business.Signer(c.Args.Signature); err != nil || signer != "ebd2c088dbce397eaa9d123083a08c43ee0bbe26" {
		t.Fatalf("exec signer %s, %v", signer, err)
	}
	if c.Args.Business.Nonce != "32" {
		t.Fatalf("nonce %s", c.Args.Business.Nonce)
	}

	evs, err := tree.BurrowEvents(Mainnet.Burrow)
	if err != nil {
		t.Fatalf("BurrowEvents: %v", err)
	}
	var started, succeeded int
	for _, e := range evs {
		if e.AccountID != mca || e.TokenID != usdcToken {
			t.Fatalf("event %+v", e)
		}
		switch e.Event {
		case BurrowEventWithdrawStarted:
			started++
		case BurrowEventWithdrawSucceeded:
			succeeded++
		}
	}
	if started != 2 || succeeded != 2 {
		t.Fatalf("withdraw events started %d succeeded %d", started, succeeded)
	}

	xfers, err := tree.FTTransfers()
	if err != nil {
		t.Fatalf("FTTransfers: %v", err)
	}
	var toRelayer, toMCA, toDeposit bool
	for _, x := range xfers {
		switch {
		case x.From == Mainnet.Burrow && x.To == Mainnet.Relayer && x.Amount.Int64() == 4919:
			toRelayer = true
		case x.From == Mainnet.Burrow && x.To == mca:
			toMCA = true
		case x.From == mca && x.To == deposit && x.Amount.Int64() == 143574:
			toDeposit = true
		}
	}
	if !toRelayer || !toMCA || !toDeposit {
		t.Fatalf("transfers %+v", xfers)
	}
}

// TestGasPaymentEvidence checks the mainnet GasPayment business 4175G6iY…:
// the fee is a native NEAR transfer from the MCA to the relayer.
func TestGasPaymentEvidence(t *testing.T) {
	tree := loadTree(t, "tx-exec-gaspayment-4175G6iY.json")
	const mca = "rhea001589.multica.near"
	var paid bool
	for _, r := range tree.Receipts() {
		if r.PredecessorID != mca || r.ReceiverID != Mainnet.Relayer {
			continue
		}
		for _, a := range r.Actions {
			if a.Kind == "Transfer" && a.Deposit.String() == "2250000000000000000000" && r.Outcome.Status.OK() {
				paid = true
			}
		}
	}
	if !paid {
		t.Fatalf("gas payment transfer not found")
	}
	calls, err := tree.ExecCalls(Mainnet)
	if err != nil || len(calls) != 1 || calls[0].Args.Business.TxRequests[0].GasPayment == nil {
		t.Fatalf("exec calls %v, %v", calls, err)
	}
}

// mutateTree decodes a captured tree after applying f to its raw JSON.
func mutateTree(t *testing.T, name string, f func(m map[string]any)) *TxTree {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(readTestdata(t, name), &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	b, _ := json.Marshal(m)
	tree, err := DecodeTxTree(b)
	if err != nil {
		t.Fatalf("DecodeTxTree: %v", err)
	}
	return tree
}

func outcomes(m map[string]any) []any {
	return m["receipts_outcome"].([]any)
}

func outcomeOf(o any) map[string]any {
	return o.(map[string]any)["outcome"].(map[string]any)
}

func TestTxTreeIncomplete(t *testing.T) {
	const name = "tx-create-supply-1aqcDa8Y.json"
	cases := map[string]func(m map[string]any){
		"not final": func(m map[string]any) { m["final_execution_status"] = "EXECUTED_OPTIMISTIC" },
		"receipt outcome missing": func(m map[string]any) {
			outs := outcomes(m)
			m["receipts_outcome"] = append(outs[:5:5], outs[6:]...)
		},
		"receipt outcome unknown": func(m map[string]any) { outcomeOf(outcomes(m)[3])["status"] = "Unknown" },
		"transaction outcome unknown": func(m map[string]any) {
			m["transaction_outcome"].(map[string]any)["outcome"].(map[string]any)["status"] = "Unknown"
		},
	}
	for name2, f := range cases {
		if err := mutateTree(t, name, f).Complete(); err == nil {
			t.Errorf("%s: reported complete", name2)
		}
	}
}

// TestFailedReceiptEventsIgnored marks the Burrow deposit receipt failed:
// its deposit event must no longer be evidence.
func TestFailedReceiptEventsIgnored(t *testing.T) {
	tree := mutateTree(t, "tx-create-supply-1aqcDa8Y.json", func(m map[string]any) {
		for _, o := range outcomes(m) {
			out := outcomeOf(o)
			for _, l := range out["logs"].([]any) {
				if strings.Contains(l.(string), `"event":"deposit"`) {
					out["status"] = map[string]any{"Failure": map[string]any{"ActionError": map[string]any{}}}
				}
			}
		}
	})
	evs, err := tree.BurrowEvents(Mainnet.Burrow)
	if err != nil {
		t.Fatalf("BurrowEvents: %v", err)
	}
	for _, e := range evs {
		if e.Event == BurrowEventDeposit {
			t.Fatalf("deposit event from a failed receipt reported")
		}
	}
}

func TestResolvedTransferIncomplete(t *testing.T) {
	tree := mutateTree(t, "tx-create-supply-1aqcDa8Y.json", func(m map[string]any) {
		for _, r := range m["receipts"].([]any) {
			rm := r.(map[string]any)
			action, _ := rm["receipt"].(map[string]any)["Action"].(map[string]any)
			if action == nil {
				continue
			}
			for _, a := range action["actions"].([]any) {
				am, _ := a.(map[string]any)
				fc, _ := am["FunctionCall"].(map[string]any)
				if fc != nil && fc["method_name"] == "ft_resolve_transfer" {
					id := rm["receipt_id"]
					for _, o := range outcomes(m) {
						if o.(map[string]any)["id"] == id {
							outcomeOf(o)["status"] = "Unknown"
						}
					}
				}
			}
		}
	})
	if _, err := tree.ResolvedTransfers(); err == nil {
		t.Fatalf("unresolved transfer accepted")
	}
}

func TestDecodeTxTreeRejects(t *testing.T) {
	for _, bad := range []string{`{}`, `[]`, `{"transaction":{"hash":""}}`} {
		if _, err := DecodeTxTree([]byte(bad)); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

func TestSubtree(t *testing.T) {
	tree := loadTree(t, "tx-exec-4SDDqzRj.json")
	calls, err := tree.ExecCalls(Mainnet)
	if err != nil || len(calls) != 1 {
		t.Fatal(err)
	}
	sub := tree.Subtree(calls[0].ReceiptID)
	if !sub[calls[0].ReceiptID] {
		t.Fatalf("subtree lacks its root")
	}
	// Every transfer of the withdrawal descends from the exec.
	xfers, _ := tree.FTTransfers()
	for _, x := range xfers {
		if !sub[x.ReceiptID] {
			t.Fatalf("transfer %+v outside the exec subtree", x)
		}
	}
	// A receipt's subtree excludes its siblings.
	var feeCall string
	for _, r := range tree.Receipts() {
		for _, a := range r.Actions {
			if a.MethodName == "simple_withdraw" {
				feeCall = r.ID
			}
		}
	}
	feeSub := tree.Subtree(feeCall)
	n := 0
	for _, x := range xfers {
		if feeSub[x.ReceiptID] {
			n++
			if x.To != Mainnet.Relayer {
				t.Fatalf("fee subtree holds %+v", x)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d transfers in the fee subtree", n)
	}
}
