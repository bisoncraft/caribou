package oneclickmock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/rhea"
)

// rheaRig is a mock server with the RHEA world and fast blocks.
type rheaRig struct {
	t   *testing.T
	srv *Server
	w   *rheaWorld
	url string
}

func newRheaRig(t *testing.T) *rheaRig {
	t.Helper()
	srv, err := New(&Config{
		Drivers:         map[string]Driver{"btc": NewMemDriver("btc")},
		ProcessingDelay: time.Millisecond,
		Rhea:            &RheaConfig{BlockInterval: 5 * time.Millisecond},
		Logger:          log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv.Run(ctx)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &rheaRig{t: t, srv: srv, w: srv.rhea, url: hs.URL}
}

// blocks waits for n blocks.
func (r *rheaRig) blocks(n int) {
	time.Sleep(time.Duration(n) * 5 * time.Millisecond * 3)
}

func (r *rheaRig) rpc(method string, params any) (json.RawMessage, *rpcErrorBody) {
	r.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(r.url+"/rhea/rpc", "application/json", bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcErrorBody   `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		r.t.Fatal(err)
	}
	return env.Result, env.Error
}

func (r *rheaRig) view(contract, method string, args any, blockID string) (string, string, *rpcErrorBody) {
	r.t.Helper()
	a, _ := json.Marshal(args)
	params := map[string]any{"request_type": "call_function", "account_id": contract, "method_name": method,
		"args_base64": base64.StdEncoding.EncodeToString(a)}
	if blockID != "" {
		params["block_id"] = blockID
	} else {
		params["finality"] = "final"
	}
	res, e := r.rpc("query", params)
	if e != nil {
		return "", "", e
	}
	var v struct {
		Result    []byte `json:"result"`
		BlockHash string `json:"block_hash"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		r.t.Fatal(err)
	}
	return string(v.Result), v.BlockHash, nil
}

func (r *rheaRig) tree(hash, sender string) *rhea.TxTree {
	r.t.Helper()
	res, e := r.rpc("EXPERIMENTAL_tx_status", map[string]any{"tx_hash": hash, "sender_account_id": sender, "wait_until": "NONE"})
	if e != nil {
		r.t.Fatalf("tx status %s: %+v", hash, e)
	}
	tree, err := rhea.DecodeTxTree(res)
	if err != nil {
		r.t.Fatal(err)
	}
	return tree
}

// awaitFinal waits for a transaction to be FINAL and complete.
func (r *rheaRig) awaitFinal(hash, sender string) *rhea.TxTree {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, e := r.rpc("EXPERIMENTAL_tx_status", map[string]any{"tx_hash": hash, "sender_account_id": sender, "wait_until": "NONE"})
		if e == nil {
			if tree, err := rhea.DecodeTxTree(res); err == nil && tree.Complete() == nil {
				return tree
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("transaction %s not final", hash)
	return nil
}

// account creates an MCA bound to a fresh key through a creation delivery
// of 10 USDC and returns the key and MCA.
func (r *rheaRig) account() (*rhea.Key, string) {
	r.t.Helper()
	seed := make([]byte, 32)
	_, _ = rand.Read(seed)
	k, err := rhea.DeriveKey(rhea.DerivationV1, seed)
	if err != nil {
		r.t.Fatal(err)
	}
	sig, _ := k.SignCreation()
	msg, err := rhea.RegisterAndSupplyMsg(k.Identity(), sig)
	if err != nil {
		r.t.Fatal(err)
	}
	hash := r.w.payout("usdc.test.near", r.w.dep.AccountManager, big.NewInt(10_000_000), msg, "depositor")
	tree := r.awaitFinal(hash, rheaVerifier)
	fees, err := tree.AccountManagerFees(r.w.dep.AccountManager)
	if err != nil || len(fees) != 2 {
		r.t.Fatalf("creation fees %v, %v", fees, err)
	}
	mca := r.srv.Rhea().MCA(string(k.Identity()))
	if mca == "" {
		r.t.Fatalf("no MCA created")
	}
	// 10 USDC less 0.6 USDC of fees is supplied.
	if got := r.srv.Rhea().Supplied(mca, "usdc.test.near"); got.Int64() != 9_400_000 {
		r.t.Fatalf("supplied %s", got)
	}
	return k, mca
}

// sweep signs a sweep of amount into supply at the MCA's nonce and returns
// the request JSON.
func (r *rheaRig) sweep(k *rhea.Key, mca string, nonce uint64, amount int64, blockTime time.Time) []byte {
	r.t.Helper()
	p := &rhea.SweepParams{
		MCA: mca, TokenID: "usdc.test.near", Nonce: nonce, BlockTime: blockTime, Deadline: rhea.DeadlineFrom(blockTime),
		GasToken: rhea.GasTokenNEAR, Fee: rheaGasFeeNEAR, FeeCap: new(big.Int).Mul(rheaGasFeeNEAR, big.NewInt(100)),
		NearValue: big.NewInt(3_000_000), Amount: big.NewInt(amount),
	}
	b, err := rhea.BuildSweep(rhea.Simnet, p)
	if err != nil {
		r.t.Fatal(err)
	}
	sr, err := k.SignSweep(rhea.Simnet, b, p, blockTime)
	if err != nil {
		r.t.Fatal(err)
	}
	j, _ := sr.MarshalJSON()
	return j
}

func (r *rheaRig) submit(mca string, req []byte) (string, error) {
	r.t.Helper()
	body, _ := json.Marshal(map[string]any{"mca_id": mca, "wallet": "", "request": []string{string(req)}})
	resp, err := http.Post(r.url+"/rhea/relayer/multichain_lending_requests", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		Code int    `json:"code"`
		Data string `json:"data"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.Data, nil
}

// batchResult waits for a batch's exec and returns its tx hash and error.
func (r *rheaRig) batchResult(id string) (string, string) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.w.mtx.Lock()
		b := r.w.batches[id]
		hash, errMsg := b.txHash, b.txErr
		var execDone bool
		if tx := r.w.txs[hash]; tx != nil && len(tx.receipts) > 0 {
			execDone = tx.receipts[0].done
		}
		r.w.mtx.Unlock()
		if execDone {
			return hash, errMsg
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatalf("batch %s not executed", id)
	return "", ""
}

func TestRheaViewsAtBlocks(t *testing.T) {
	r := newRheaRig(t)
	r.blocks(4)
	_, mca := r.account()
	_, before, _ := r.view("usdc.test.near", "ft_balance_of", map[string]string{"account_id": mca}, "")

	// A credit at the head reaches final views only after the final lag.
	r.w.SetFaults(RheaFaults{FinalLag: 50})
	if err := r.w.Fund("usdc.test.near", mca, big.NewInt(5)); err != nil {
		t.Fatal(err)
	}
	r.blocks(4)
	if bal, _, _ := r.view("usdc.test.near", "ft_balance_of", map[string]string{"account_id": mca}, ""); bal != `"0"` {
		t.Fatalf("stale final view shows %s", bal)
	}
	r.w.SetFaults(RheaFaults{FinalLag: 2})
	r.blocks(4)
	if bal, _, _ := r.view("usdc.test.near", "ft_balance_of", map[string]string{"account_id": mca}, ""); bal != `"5"` {
		t.Fatalf("final view shows %s", bal)
	}
	// A view at an earlier block reads that block's state.
	bal, at, _ := r.view("usdc.test.near", "ft_balance_of", map[string]string{"account_id": mca}, before)
	if bal != `"0"` || at != before {
		t.Fatalf("view at %s: %s at %s", before, bal, at)
	}
	if _, _, e := r.view("usdc.test.near", "ft_balance_of", map[string]string{"account_id": mca}, "nosuchblock"); e == nil || e.Cause.Name != "UNKNOWN_BLOCK" {
		t.Fatalf("unknown block: %+v", e)
	}
	// Unbound identities read null; unknown accounts are UNKNOWN_ACCOUNT.
	if v, _, _ := r.view(r.w.dep.AccountManager, "get_mca_by_wallet", map[string]any{"wallet": map[string]string{"EVM": "00"}}, ""); v != "null" {
		t.Fatalf("unbound identity: %s", v)
	}
	if _, _, e := r.view("rhea999999.multica.test.near", "get_nonce", map[string]any{}, ""); e == nil || e.Cause.Name != "UNKNOWN_ACCOUNT" {
		t.Fatalf("unknown MCA: %+v", e)
	}
}

func TestRheaBusinessExecution(t *testing.T) {
	r := newRheaRig(t)
	r.blocks(4)
	k, mca := r.account()
	if err := r.w.Fund("usdc.test.near", mca, big.NewInt(2_000_000)); err != nil {
		t.Fatal(err)
	}
	now := r.srv.Rhea().ChainTime()

	// The exec consumes the nonce in its own block; its child receipts run
	// later, and the transaction is FINAL only after the last of them.
	r.w.SetFaults(RheaFaults{FinalLag: 2, HoldReceipts: true})
	batch, err := r.submit(mca, r.sweep(k, mca, 0, 1_000_000, now))
	if err != nil {
		t.Fatal(err)
	}
	hash, execErr := r.batchResult(batch)
	if execErr != "" || r.srv.Rhea().Nonce(mca) != 1 {
		t.Fatalf("exec: %q, nonce %d", execErr, r.srv.Rhea().Nonce(mca))
	}
	r.blocks(6)
	tree := r.tree(hash, r.w.dep.Relayer)
	if tree.Complete() == nil {
		t.Fatalf("tree complete while child receipts are held")
	}
	r.w.SetFaults(RheaFaults{FinalLag: 2})
	tree = r.awaitFinal(hash, r.w.dep.Relayer)
	evs, _ := tree.BurrowEvents(r.w.dep.Burrow)
	if len(evs) != 1 || evs[0].Event != "deposit" || evs[0].AccountID != mca {
		t.Fatalf("Burrow events %+v", evs)
	}
	calls, err := tree.ExecCalls(rhea.Simnet)
	if err != nil || len(calls) != 1 {
		t.Fatalf("exec calls %v, %v", calls, err)
	}

	// Rejections leave the nonce where it is.
	reject := func(name string, req []byte, want string) {
		t.Helper()
		batch, err := r.submit(mca, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, got := r.batchResult(batch); got != want {
			t.Fatalf("%s: %q, want %q", name, got, want)
		}
		if n := r.srv.Rhea().Nonce(mca); n != 1 {
			t.Fatalf("%s: nonce moved to %d", name, n)
		}
	}
	now = r.srv.Rhea().ChainTime()
	reject("replay", r.sweep(k, mca, 0, 1_000_000, now), "InvalidNonce")
	tampered := bytes.Replace(r.sweep(k, mca, 1, 1_000_000, now), []byte(`"amount\":\"1000000\"`), []byte(`"amount\":\"1000001\"`), 1)
	reject("tampered", tampered, "InvalidEvmSignature")
	other, _ := r.account()
	reject("foreign signer", r.sweep(other, mca, 1, 1_000_000, now), "WalletDoesNotExist")
	r.w.SetFaults(RheaFaults{FinalLag: 2, ExecDelay: 200 * time.Millisecond})
	expired := r.sweep(k, mca, 1, 1_000_000, now)
	r.w.Warp(rhea.DeadlineWindow + time.Minute)
	reject("expired", expired, "DeadlineExpired")
	r.w.SetFaults(RheaFaults{FinalLag: 2})

	// The MCA verifies its own encoding: key order in the request does
	// not matter.
	req := r.sweep(k, mca, 1, 500_000, r.srv.Rhea().ChainTime())
	var generic map[string]any
	_ = json.Unmarshal(req, &generic)
	sorted, _ := json.Marshal(generic) // Go sorts map keys
	if bytes.Equal(sorted, req) {
		t.Fatalf("reordering left the request unchanged")
	}
	batch, _ = r.submit(mca, sorted)
	if _, got := r.batchResult(batch); got != "" {
		t.Fatalf("reordered request: %s", got)
	}
}

func TestRheaLostReply(t *testing.T) {
	r := newRheaRig(t)
	r.blocks(4)
	k, mca := r.account()
	if err := r.w.Fund("usdc.test.near", mca, big.NewInt(1_000_000)); err != nil {
		t.Fatal(err)
	}
	r.w.SetFaults(RheaFaults{FinalLag: 2, LostResponses: 1})
	if _, err := r.submit(mca, r.sweep(k, mca, 0, 1_000_000, r.srv.Rhea().ChainTime())); err == nil {
		t.Fatalf("reply not lost")
	}
	// The business executed anyway and appears in the relayer history.
	deadline := time.Now().Add(5 * time.Second)
	for r.srv.Rhea().Nonce(mca) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := http.Get(r.url + "/rhea/relayer/get_multichain_lending_history?mca_id=" + mca + "&page_number=1&page_size=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page struct {
		Records []struct {
			Type        int    `json:"type"`
			RequestHash string `json:"request_hash"`
		} `json:"record_list"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&page)
	if len(page.Records) != 1 || page.Records[0].Type != 1 || r.tree(page.Records[0].RequestHash, r.w.dep.Relayer) == nil {
		t.Fatalf("history %+v", page.Records)
	}
	if f := r.w.Faults(); f.LostResponses != 0 {
		t.Fatalf("fault not consumed: %+v", f)
	}
}

func TestRheaControlAPI(t *testing.T) {
	r := newRheaRig(t)
	r.blocks(4)
	_, mca := r.account()
	post := func(path, body string) map[string]any {
		t.Helper()
		resp, err := http.Post(r.url+path, "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s: HTTP %d %s", path, resp.StatusCode, b)
		}
		var v map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&v)
		return v
	}
	// Faults merge: members absent from the body keep their values.
	post("/test/rhea/faults", `{"failWithdraws":2}`)
	f := post("/test/rhea/faults", `{"holdReceipts":true}`)
	if f["failWithdraws"] != float64(2) || f["holdReceipts"] != true || f["finalLag"] != float64(2) {
		t.Fatalf("faults %v", f)
	}
	post("/test/rhea/faults", `{"failWithdraws":0,"holdReceipts":false}`)
	if w := post("/test/rhea/warp", `{"seconds":90}`); w["warp"] != "1m30s" {
		t.Fatalf("warp %v", w)
	}
	post("/test/rhea/fund", `{"token":"usdt.test.near","account":"`+mca+`","amount":"42"}`)
	if b := r.srv.Rhea().Balance("usdt.test.near", mca); b.Int64() != 42 {
		t.Fatalf("funded %s", b)
	}
	post("/test/rhea/nonce", `{"mca":"`+mca+`","nonce":7}`)
	if n := r.srv.Rhea().Nonce(mca); n != 7 {
		t.Fatalf("nonce %d", n)
	}
	resp, err := http.Get(r.url + "/test/rhea/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Accounts []struct {
			MCA   string `json:"mca"`
			Nonce uint64 `json:"nonce"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || len(st.Accounts) != 1 || st.Accounts[0].MCA != mca || st.Accounts[0].Nonce != 7 {
		t.Fatalf("state %+v, %v", st, err)
	}
}
