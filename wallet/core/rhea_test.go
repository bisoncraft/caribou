package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/near"
	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/rhea"
)

// The captured mainnet views in wallet/rhea/testdata were all read at this
// final block.
const (
	rheaTestBlockHash   = "4jRBh7o3ND46sRyPzqLenVKarw2NcCYxnwf72aEfehHH"
	rheaTestBlockHeight = 225016693
	rheaTestBlockNanos  = 1794949549020383055
	rheaTestMCA         = "rhea001569.multica.near"
	rheaTestIdentity    = rhea.Identity("242c9fc3162321d326892cfe044fc0d9ffc3e9b0")
)

var rheaTestBlockTime = time.Unix(0, rheaTestBlockNanos)

func rheaCanonArgs(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) == 0 {
		b = []byte("{}")
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("bad args %s: %v", b, err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// fakeRheaChain serves the captured mainnet views and scripted
// transactions.
type fakeRheaChain struct {
	t        *testing.T
	mtx      sync.Mutex
	block    *near.BlockHeader
	views    map[string][]byte // contract method args -> result
	viewErr  map[string]error  // contract method -> error
	accounts map[string]*near.AccountView
	txs      map[string][]byte
	txErr    map[string]error
	txCalls  int
	// allErr fails CallFunctionAll; allAnswers are extra endpoints'
	// answers it reports beside the captured one.
	allErr     error
	allAnswers []string
}

func newFakeRheaChain(t *testing.T) *fakeRheaChain {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "rhea", "testdata", "views.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cv struct {
		Views []struct {
			Contract string          `json:"contract"`
			Method   string          `json:"method"`
			Args     json.RawMessage `json:"args"`
			Result   *string         `json:"result"`
		} `json:"views"`
		Accounts []struct {
			AccountID string           `json:"account_id"`
			View      near.AccountView `json:"view"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(b, &cv); err != nil {
		t.Fatal(err)
	}
	f := &fakeRheaChain{
		t:        t,
		block:    &near.BlockHeader{Height: rheaTestBlockHeight, Hash: rheaTestBlockHash, Timestamp: rheaTestBlockTime},
		views:    make(map[string][]byte),
		viewErr:  make(map[string]error),
		accounts: make(map[string]*near.AccountView),
		txs:      make(map[string][]byte),
		txErr:    make(map[string]error),
	}
	for _, v := range cv.Views {
		if v.Result != nil {
			f.views[v.Contract+" "+v.Method+" "+rheaCanonArgs(t, v.Args)] = []byte(*v.Result)
		}
	}
	for _, a := range cv.Accounts {
		av := a.View
		f.accounts[a.AccountID] = &av
	}
	return f
}

func (f *fakeRheaChain) setView(contract, method, args, result string) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.views[contract+" "+method+" "+rheaCanonArgs(f.t, []byte(args))] = []byte(result)
}

func (f *fakeRheaChain) CallFunction(_ context.Context, contract, method string, args []byte, blockHash string) (*rhea.ViewResult, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	if err := f.viewErr[contract+" "+method]; err != nil {
		return nil, err
	}
	if blockHash != "" && blockHash != f.block.Hash {
		return nil, errors.New("unknown block")
	}
	res, ok := f.views[contract+" "+method+" "+rheaCanonArgs(f.t, args)]
	if !ok {
		return nil, fmt.Errorf("not captured: %s.%s %s", contract, method, args)
	}
	return &rhea.ViewResult{Result: res, BlockHeight: f.block.Height, BlockHash: f.block.Hash}, nil
}

func (f *fakeRheaChain) CallFunctionAll(ctx context.Context, contract, method string, args []byte, blockHash string) ([]*rhea.ViewResult, error) {
	f.mtx.Lock()
	errAll := f.allErr
	answers := f.allAnswers
	f.mtx.Unlock()
	if errAll != nil {
		return nil, errAll
	}
	res, err := f.CallFunction(ctx, contract, method, args, blockHash)
	if err != nil {
		return nil, err
	}
	out := []*rhea.ViewResult{res}
	for _, a := range answers {
		out = append(out, &rhea.ViewResult{Result: []byte(a), BlockHeight: res.BlockHeight, BlockHash: res.BlockHash})
	}
	return out, nil
}

func (f *fakeRheaChain) FinalBlock(context.Context) (*near.BlockHeader, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	h := *f.block
	return &h, nil
}

func (f *fakeRheaChain) ViewAccount(_ context.Context, accountID, blockHash string) (*near.AccountView, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	if a := f.accounts[accountID]; a != nil {
		return a, nil
	}
	return nil, errors.New("unknown account")
}

func (f *fakeRheaChain) TxStatus(_ context.Context, txHash, senderID, _ string) (json.RawMessage, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.txCalls++
	if senderID != rhea.Mainnet.Relayer && senderID != rheaVerifier {
		return nil, fmt.Errorf("wrong sender %s", senderID)
	}
	if err := f.txErr[txHash]; err != nil {
		return nil, err
	}
	raw, ok := f.txs[txHash]
	if !ok {
		return nil, errors.New("unknown transaction")
	}
	return raw, nil
}

// execTx scripts an exec transaction on mca carrying a business with the
// given nonce. final selects a FINAL tree; execOK whether the exec receipt
// succeeded.
func (f *fakeRheaChain) execTx(hash, mca string, nonce uint64, final, execOK bool) {
	args := fmt.Sprintf(`{"business":{"nonce":"%d","deadline":"1","tx_requests":[]},"signature":"00","signer_wallet":{"EVM":"00"}}`, nonce)
	action := map[string]any{"FunctionCall": map[string]any{
		"method_name": "exec", "args": base64.StdEncoding.EncodeToString([]byte(args)), "gas": 300000000000000, "deposit": rhea.StorageDeposit,
	}}
	status := map[string]any{"SuccessValue": ""}
	if !execOK {
		status = map[string]any{"Failure": map[string]any{"ActionError": map[string]any{}}}
	}
	fes := rhea.FinalExecutionFinal
	if !final {
		fes = "EXECUTED_OPTIMISTIC"
	}
	rid := "r-" + hash
	tx := map[string]any{
		"final_execution_status": fes,
		"transaction": map[string]any{
			"hash": hash, "signer_id": rhea.Mainnet.Relayer, "receiver_id": mca, "actions": []any{action},
		},
		"transaction_outcome": map[string]any{"id": hash, "block_hash": "b", "outcome": map[string]any{
			"executor_id": rhea.Mainnet.Relayer, "logs": []any{}, "receipt_ids": []any{rid}, "status": map[string]any{"SuccessReceiptId": rid},
		}},
		"receipts": []any{map[string]any{
			"receipt_id": rid, "predecessor_id": rhea.Mainnet.Relayer, "receiver_id": mca,
			"receipt": map[string]any{"Action": map[string]any{"actions": []any{action}}},
		}},
		"receipts_outcome": []any{map[string]any{"id": rid, "block_hash": "b", "outcome": map[string]any{
			"executor_id": mca, "logs": []any{}, "receipt_ids": []any{}, "status": status,
		}}},
	}
	b, _ := json.Marshal(tx)
	f.mtx.Lock()
	f.txs[hash] = b
	f.mtx.Unlock()
}

// fakeRheaHistory serves relayer history newest first.
type fakeRheaHistory struct {
	mtx     sync.Mutex
	records []*rhea.HistoryRecord // newest first
	err     error
	pages   int
}

func (h *fakeRheaHistory) add(typ int, hash string) {
	h.mtx.Lock()
	defer h.mtx.Unlock()
	h.records = append([]*rhea.HistoryRecord{{Type: typ, MCAID: rheaTestMCA, RequestHash: hash}}, h.records...)
}

func (h *fakeRheaHistory) History(_ context.Context, mcaID string, page, size int) (*rhea.HistoryPage, error) {
	h.mtx.Lock()
	defer h.mtx.Unlock()
	h.pages++
	if h.err != nil {
		return nil, h.err
	}
	total := (len(h.records) + size - 1) / size
	p := &rhea.HistoryPage{PageNumber: page, PageSize: size, TotalPage: total, TotalSize: len(h.records), Records: []*rhea.HistoryRecord{}}
	start := (page - 1) * size
	for i := start; i < start+size && i < len(h.records); i++ {
		p.Records = append(p.Records, h.records[i])
	}
	return p, nil
}

type rheaTestRig struct {
	eng     *rheaEngine
	chain   *fakeRheaChain
	history *fakeRheaHistory
	db      *TDB
	ids     []rheaIdentity
	now     time.Time
}

func newRheaTestRig(t *testing.T) *rheaTestRig {
	r := &rheaTestRig{
		chain:   newFakeRheaChain(t),
		history: &fakeRheaHistory{},
		db:      &TDB{},
		ids:     []rheaIdentity{{Version: rhea.DerivationV1, Identity: rheaTestIdentity}},
		now:     rheaTestBlockTime.Add(time.Second),
	}
	r.eng = newRheaEngine(tLogger, rhea.Mainnet, r.chain, r.history, r.db, func() []rheaIdentity { return r.ids })
	r.eng.now = func() time.Time { return r.now }
	return r
}

// TestRheaIdentitiesAtLogin checks the core side of the derivation v1
// vector (app seed through AssetSeedAndPass to the identity) and that
// identities exist only while logged in.
func TestRheaIdentitiesAtLogin(t *testing.T) {
	appSeed := make([]byte, 64)
	for i := range appSeed {
		appSeed[i] = byte(i)
	}
	c := &Core{log: tLogger}
	if c.rheaIdentities() != nil {
		t.Fatalf("identities before login")
	}
	c.setRheaIdentities(appSeed)
	ids := c.rheaIdentities()
	if len(ids) != 1 || ids[0].Version != rhea.DerivationV1 || ids[0].Identity != "4500538e8c3060947e71621e9278cbde8ef387d3" {
		t.Fatalf("identities %+v", ids)
	}
	c.clearRheaIdentities()
	if c.rheaIdentities() != nil {
		t.Fatalf("identities after logout")
	}

	rig := newTestRig()
	defer rig.shutdown()
	if err := rig.core.Login(tPW); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if ids := rig.core.rheaIdentities(); len(ids) != 1 {
		t.Fatalf("identities after Login: %+v", ids)
	}
	if err := rig.core.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if rig.core.rheaIdentities() != nil {
		t.Fatalf("identities survived Logout")
	}
}

func TestRheaDiscovery(t *testing.T) {
	ctx := context.Background()
	r := newRheaTestRig(t)
	d := r.eng.discover(ctx, r.ids, rheaTestBlockHash)
	if d.Status != RheaFound || d.found == nil || d.found.MCA != rheaTestMCA || len(d.Bindings) != 1 {
		t.Fatalf("discovery %+v", d)
	}

	// An identity the account manager returned null for.
	none := []rheaIdentity{{Version: rhea.DerivationV1, Identity: "7e5f4552091a69125d5dfcb7b8c2659029395bdf"}}
	if d = r.eng.discover(ctx, none, rheaTestBlockHash); d.Status != RheaNone || d.Bindings[0].Status != RheaNone {
		t.Fatalf("null discovery %+v", d)
	}

	// Two identities bound to accounts is multiple, and a failed lookup
	// beside a found one is unavailable, never found.
	r.chain.setView("multica.near", "get_mca_by_wallet", `{"wallet":{"EVM":"7e5f4552091a69125d5dfcb7b8c2659029395bdf"}}`, `"rhea000001.multica.near"`)
	both := append([]rheaIdentity{}, r.ids...)
	both = append(both, rheaIdentity{Version: 2, Identity: "7e5f4552091a69125d5dfcb7b8c2659029395bdf"})
	if d = r.eng.discover(ctx, both, rheaTestBlockHash); d.Status != RheaMultiple || d.found != nil {
		t.Fatalf("multiple discovery %+v", d)
	}
	r.chain.viewErr["multica.near get_mca_by_wallet"] = errors.New("endpoint down")
	if d = r.eng.discover(ctx, r.ids, rheaTestBlockHash); d.Status != RheaUnavailable || d.Bindings[0].Error == "" {
		t.Fatalf("failed lookup %+v", d)
	}
}

func TestRheaRecordAccount(t *testing.T) {
	r := newRheaTestRig(t)
	b := &RheaBinding{Version: 1, Identity: string(rheaTestIdentity), AccountManager: "multica.near", Status: RheaFound, MCA: rheaTestMCA}
	block := &near.BlockHeader{Height: 10, Hash: "H10", Timestamp: time.Unix(1000, 0)}
	a, err := r.eng.recordAccount(b, block)
	if err != nil || a.FirstSeenHeight != 10 || a.FirstSeenHash != "H10" || !a.FirstSeenTime.Equal(block.Timestamp) {
		t.Fatalf("new record %+v, %v", a, err)
	}
	// A later sighting keeps the first-seen block.
	r.now = r.now.Add(time.Hour)
	later := &near.BlockHeader{Height: 20, Hash: "H20", Timestamp: time.Unix(2000, 0)}
	if a, err = r.eng.recordAccount(b, later); err != nil || a.FirstSeenHeight != 10 {
		t.Fatalf("re-sighting %+v, %v", a, err)
	}
	// A different MCA for the same identity starts over.
	a.VerifiedBelow = 5
	_ = r.db.StoreRheaAccount(a)
	b2 := *b
	b2.MCA = "rhea000001.multica.near"
	if a, err = r.eng.recordAccount(&b2, later); err != nil || a.FirstSeenHeight != 20 || a.VerifiedBelow != 0 || a.MCA != b2.MCA {
		t.Fatalf("rebound record %+v, %v", a, err)
	}
	r.db.rheaErr = errors.New("db down")
	if _, err = r.eng.recordAccount(b, later); err == nil {
		t.Fatalf("db failure hidden")
	}
}

func TestRheaAdmission(t *testing.T) {
	first := rheaTestBlockTime.Add(-time.Hour)
	acct := func(verified uint64) *db.RheaAccount {
		return &db.RheaAccount{MCA: rheaTestMCA, FirstSeenTime: first, VerifiedBelow: verified}
	}
	base := func() *rheaAdmissionInput {
		return &rheaAdmissionInput{
			discovery: RheaFound,
			mca:       rheaTestMCA,
			account:   acct(90),
			nonce:     90,
			blockTime: rheaTestBlockTime,
			now:       rheaTestBlockTime.Add(time.Second),
		}
	}
	if adm := rheaAdmission(base()); !adm.Open || len(adm.Reasons) != 0 || adm.RecoveryUnresolved {
		t.Fatalf("expected open gate: %+v", adm)
	}
	tests := []struct {
		name       string
		mutate     func(in *rheaAdmissionInput)
		unresolved bool
	}{
		{"not found", func(in *rheaAdmissionInput) { in.discovery = RheaNone }, false},
		{"unavailable", func(in *rheaAdmissionInput) { in.discovery = RheaUnavailable }, false},
		{"multiple", func(in *rheaAdmissionInput) { in.discovery = RheaMultiple }, false},
		{"no account record", func(in *rheaAdmissionInput) { in.account = nil }, false},
		{"window not passed", func(in *rheaAdmissionInput) {
			in.account.FirstSeenTime = rheaTestBlockTime.Add(-rhea.MaxDeadlineWindow - rheaWindowMargin)
		}, false},
		{"nonces unverified", func(in *rheaAdmissionInput) { in.account.VerifiedBelow = 88 }, true},
		{"nothing verified", func(in *rheaAdmissionInput) { in.account.VerifiedBelow = 0 }, true},
		{"checkpoint above nonce", func(in *rheaAdmissionInput) { in.account.VerifiedBelow = 91 }, true},
		{"clock ahead", func(in *rheaAdmissionInput) { in.now = rheaTestBlockTime.Add(rhea.MaxClockSkew + time.Second) }, false},
		{"clock behind", func(in *rheaAdmissionInput) { in.now = rheaTestBlockTime.Add(-rhea.MaxClockSkew - time.Second) }, false},
		{"pending business", func(in *rheaAdmissionInput) {
			in.activeOps = []*db.RheaOp{{ID: "op", MCA: rheaTestMCA, Kind: db.RheaOpWithdraw, State: db.RheaStateSubmitUnknown}}
		}, false},
	}
	for _, tt := range tests {
		in := base()
		tt.mutate(in)
		adm := rheaAdmission(in)
		if adm.Open || len(adm.Reasons) == 0 {
			t.Errorf("%s: gate open: %+v", tt.name, adm)
		}
		if adm.RecoveryUnresolved != tt.unresolved {
			t.Errorf("%s: recovery unresolved %t, want %t", tt.name, adm.RecoveryUnresolved, tt.unresolved)
		}
	}
	// A fresh MCA at nonce 0 needs no history.
	in := base()
	in.nonce, in.account.VerifiedBelow = 0, 0
	if adm := rheaAdmission(in); !adm.Open {
		t.Fatalf("nonce 0 account closed: %+v", adm)
	}
	// Pending businesses on another MCA, and settled ones, do not block.
	in = base()
	in.activeOps = []*db.RheaOp{
		{ID: "a", MCA: "rhea000001.multica.near", Kind: db.RheaOpWithdraw, State: db.RheaStateSigned},
		{ID: "b", MCA: rheaTestMCA, Kind: db.RheaOpWithdraw, State: db.RheaStateDelivering},
	}
	if adm := rheaAdmission(in); !adm.Open {
		t.Fatalf("unrelated ops closed the gate: %+v", adm)
	}
}

func rheaTestAccount(t *testing.T, r *rheaTestRig, verified uint64) *db.RheaAccount {
	t.Helper()
	a := &db.RheaAccount{
		AccountManager: "multica.near", Identity: string(rheaTestIdentity), DerivationVersion: 1, MCA: rheaTestMCA,
		FirstSeenTime: rheaTestBlockTime.Add(-time.Hour), VerifiedBelow: verified, LastLookup: r.now,
	}
	if err := r.db.StoreRheaAccount(a); err != nil {
		t.Fatal(err)
	}
	return a
}

func storedCheckpoint(t *testing.T, r *rheaTestRig) uint64 {
	t.Helper()
	a, err := r.db.RheaAccount("multica.near", 1, string(rheaTestIdentity))
	if err != nil {
		t.Fatal(err)
	}
	return a.VerifiedBelow
}

// marketRates is a ticker rate source with a rate for every mainnet market.
func marketRates() map[string]float64 {
	rates := make(map[string]float64)
	for _, m := range rhea.Mainnet.Markets {
		rates[m.FiatTicker] = 1
	}
	return rates
}

func TestRheaWalkNonces(t *testing.T) {
	ctx := context.Background()

	// Every consumed nonce found, final, and fully reported: the
	// checkpoint reaches the nonce. Supply records and execs on other MCAs
	// are ignored. Walks never read fiat rates.
	r := newRheaTestRig(t)
	r.eng.tickerRates = tripRates(t)
	for n := uint64(0); n < 5; n++ {
		h := fmt.Sprintf("tx%d", n)
		r.chain.execTx(h, rheaTestMCA, n, true, true)
		r.history.add(rhea.HistoryTypeRelayer, h)
		r.history.add(rhea.HistoryTypeSupply, fmt.Sprintf("supply%d", n))
	}
	r.chain.execTx("elsewhere", "rhea000001.multica.near", 2, true, true)
	r.history.add(rhea.HistoryTypeRelayer, "elsewhere")
	acct := rheaTestAccount(t, r, 0)
	if err := r.eng.walkNonces(ctx, acct, 5); err != nil {
		t.Fatalf("walkNonces: %v", err)
	}
	if storedCheckpoint(t, r) != 5 || acct.VerifiedBelow != 5 {
		t.Fatalf("checkpoint %d", storedCheckpoint(t, r))
	}

	// Later walks start at the checkpoint and stop one exec past it: the
	// two new execs, the record for the other MCA (which must be read to
	// be ruled out), and the newest older exec.
	for n := uint64(5); n < 7; n++ {
		h := fmt.Sprintf("tx%d", n)
		r.chain.execTx(h, rheaTestMCA, n, true, true)
		r.history.add(rhea.HistoryTypeRelayer, h)
	}
	r.chain.txCalls = 0
	if err := r.eng.walkNonces(ctx, acct, 7); err != nil {
		t.Fatalf("incremental walk: %v", err)
	}
	if storedCheckpoint(t, r) != 7 || r.chain.txCalls != 4 {
		t.Fatalf("incremental walk: checkpoint %d after %d lookups", storedCheckpoint(t, r), r.chain.txCalls)
	}

	// A nonce with no exec in the history leaves a gap.
	r = newRheaTestRig(t)
	for _, n := range []uint64{0, 1, 3} {
		h := fmt.Sprintf("tx%d", n)
		r.chain.execTx(h, rheaTestMCA, n, true, true)
		r.history.add(rhea.HistoryTypeRelayer, h)
	}
	acct = rheaTestAccount(t, r, 0)
	if err := r.eng.walkNonces(ctx, acct, 4); err == nil || !strings.Contains(err.Error(), "nonce 2") {
		t.Fatalf("gap: %v", err)
	}
	if storedCheckpoint(t, r) != 2 {
		t.Fatalf("gap checkpoint %d", storedCheckpoint(t, r))
	}

	// An exec admitted long ago whose child receipts are still pending
	// keeps recovery unresolved even though the latest exec is final.
	r = newRheaTestRig(t)
	for n := uint64(0); n < 3; n++ {
		h := fmt.Sprintf("tx%d", n)
		r.chain.execTx(h, rheaTestMCA, n, n != 1, true)
		r.history.add(rhea.HistoryTypeRelayer, h)
	}
	acct = rheaTestAccount(t, r, 0)
	if err := r.eng.walkNonces(ctx, acct, 3); err == nil || !strings.Contains(err.Error(), "nonce 1") {
		t.Fatalf("delayed receipts: %v", err)
	}
	if storedCheckpoint(t, r) != 1 {
		t.Fatalf("delayed receipts checkpoint %d", storedCheckpoint(t, r))
	}

	// A failed exec consumed nothing; the successful one for the same
	// nonce counts. Two successful execs for one nonce are an anomaly.
	r = newRheaTestRig(t)
	r.chain.execTx("failed0", rheaTestMCA, 0, true, false)
	r.chain.execTx("ok0", rheaTestMCA, 0, true, true)
	r.history.add(rhea.HistoryTypeRelayer, "failed0")
	r.history.add(rhea.HistoryTypeRelayer, "ok0")
	acct = rheaTestAccount(t, r, 0)
	if err := r.eng.walkNonces(ctx, acct, 1); err != nil {
		t.Fatalf("failed exec beside a good one: %v", err)
	}
	r = newRheaTestRig(t)
	r.chain.execTx("a1", rheaTestMCA, 0, true, true)
	r.chain.execTx("b1", rheaTestMCA, 0, true, true)
	r.history.add(rhea.HistoryTypeRelayer, "a1")
	r.history.add(rhea.HistoryTypeRelayer, "b1")
	acct = rheaTestAccount(t, r, 0)
	if err := r.eng.walkNonces(ctx, acct, 1); err == nil || !strings.Contains(err.Error(), "two executed") {
		t.Fatalf("duplicate nonce: %v", err)
	}
	if storedCheckpoint(t, r) != 0 {
		t.Fatalf("checkpoint advanced past an anomaly")
	}

	// Unreadable history or transactions never verify anything.
	r = newRheaTestRig(t)
	r.history.err = errors.New("relayer down")
	acct = rheaTestAccount(t, r, 0)
	if err := r.eng.walkNonces(ctx, acct, 1); err == nil {
		t.Fatalf("history failure accepted")
	}
	r = newRheaTestRig(t)
	r.history.add(rhea.HistoryTypeRelayer, "tx0")
	r.chain.txErr["tx0"] = errors.New("endpoint down")
	acct = rheaTestAccount(t, r, 0)
	if err := r.eng.walkNonces(ctx, acct, 1); err == nil || !strings.Contains(err.Error(), "could not be checked") {
		t.Fatalf("transaction failure: %v", err)
	}

	// A tree without its receipt list is not evidence.
	r = newRheaTestRig(t)
	r.chain.execTx("tx0", rheaTestMCA, 0, true, true)
	var m map[string]any
	_ = json.Unmarshal(r.chain.txs["tx0"], &m)
	delete(m, "receipts")
	r.chain.txs["tx0"], _ = json.Marshal(m)
	r.history.add(rhea.HistoryTypeRelayer, "tx0")
	acct = rheaTestAccount(t, r, 0)
	if err := r.eng.walkNonces(ctx, acct, 1); err == nil {
		t.Fatalf("receipt-less tree accepted")
	}

	// Long histories page.
	r = newRheaTestRig(t)
	for n := uint64(0); n < rheaHistoryPageSize+10; n++ {
		h := fmt.Sprintf("tx%d", n)
		r.chain.execTx(h, rheaTestMCA, n, true, true)
		r.history.add(rhea.HistoryTypeRelayer, h)
	}
	acct = rheaTestAccount(t, r, 0)
	if err := r.eng.walkNonces(ctx, acct, rheaHistoryPageSize+10); err != nil || r.history.pages != 2 {
		t.Fatalf("paged walk: %v after %d pages", err, r.history.pages)
	}
}

// TestRheaAccountView reads rhea001569 from the captured mainnet views. It
// holds plain supply, collateral, and debt, has foreign wallets bound, and
// holds tokens outside the allowlist.
func TestRheaAccountView(t *testing.T) {
	ctx := context.Background()
	r := newRheaTestRig(t)
	view, err := r.eng.account(ctx)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if view.Discovery != RheaFound || view.MCA != rheaTestMCA || view.Nonce == nil || *view.Nonce != 90 {
		t.Fatalf("account %+v", view)
	}
	if view.Block.Hash != rheaTestBlockHash || view.FirstSeen == nil || view.FirstSeen.Hash != rheaTestBlockHash {
		t.Fatalf("blocks %+v, first seen %+v", view.Block, view.FirstSeen)
	}
	if view.CodeHash != "FDX6nYTmwVVwkjSXwFZ8xRjkPsyU1BEt1QqB9TbXwD11" || !view.CodeIsLatest {
		t.Fatalf("code hash %s latest %s", view.CodeHash, view.LatestCodeHash)
	}
	var ours, foreign int
	for _, w := range view.Wallets {
		if w.Ours {
			ours++
		} else {
			foreign++
		}
	}
	if ours != 1 || foreign != 3 || !view.ForeignWallets {
		t.Fatalf("wallets %+v", view.Wallets)
	}
	if !view.HasDebt || !view.BurrowAccount {
		t.Fatalf("debt or Burrow account misread")
	}
	pos := map[string]*RheaPosition{}
	for _, p := range view.Positions {
		pos[p.Symbol] = p
	}
	if usdc := pos["USDC"]; usdc == nil || usdc.Supplied != "1387" || usdc.SuppliedInternal != "1387814608947877" ||
		usdc.Borrowed != "13004972106" || usdc.PlainBalance != "52" || usdc.Decimals != 6 || !usdc.CanWithdraw {
		t.Fatalf("USDC position %+v", pos["USDC"])
	}
	if near := pos["NEAR"]; near == nil || near.Supplied != "0" || near.Collateral != "200920577216290306447789830" {
		t.Fatalf("NEAR position %+v", pos["NEAR"])
	}
	if zec := pos["ZEC"]; zec == nil || zec.Collateral != "1500400150" {
		t.Fatalf("ZEC position %+v", pos["ZEC"])
	}
	// Without fiat rates every market is marked unavailable, not hidden.
	for _, p := range view.Positions {
		if p.SupplyUnavailable != "no current USD rate for "+p.Symbol+"; supplying is unavailable until rates refresh" {
			t.Fatalf("position without a rate %+v", p)
		}
	}
	r.eng.tickerRates = marketRates
	if view, err = r.eng.account(ctx); err != nil {
		t.Fatalf("account: %v", err)
	}
	for _, p := range view.Positions {
		if p.SupplyUnavailable != "" {
			t.Fatalf("position with a rate %+v", p)
		}
	}
	unlisted := map[string]string{}
	for _, h := range view.Unlisted {
		unlisted[h.TokenID] = h.Kind
	}
	if unlisted["eth.bridge.near"] != "collateral" || unlisted["xtoken.rhealab.near"] != "collateral" {
		t.Fatalf("unlisted holdings %+v", view.Unlisted)
	}
	// Just discovered: the signature window has not passed and no nonce
	// is verified, so the gate is closed and a walk is requested.
	adm := view.Admission
	if adm.Open || !adm.RecoveryUnresolved || adm.WindowOpensAt == nil || len(adm.Reasons) != 2 {
		t.Fatalf("admission %+v", adm)
	}
	select {
	case <-r.eng.trigger:
	default:
		t.Fatalf("no walk requested for unverified nonces")
	}

	// Outdated MCA code is reported, not hidden.
	r.chain.accounts[rheaTestMCA].GlobalContractHash = "GaLHJv6RqCfniprg6dc7K484Ga3i1qjEYPkSRk5aL31n"
	if view, err = r.eng.account(ctx); err != nil || view.CodeIsLatest || view.CodeHash == view.LatestCodeHash {
		t.Fatalf("outdated code: %+v, %v", view, err)
	}

	// No identities: no view. A failed read fails the view rather than
	// reporting zero balances.
	r.ids = nil
	if _, err = r.eng.account(ctx); err == nil {
		t.Fatalf("account view without identities")
	}
	r.ids = []rheaIdentity{{Version: rhea.DerivationV1, Identity: rheaTestIdentity}}
	r.chain.viewErr[usdcTokenForTest+" ft_balance_of"] = errors.New("endpoint down")
	if _, err = r.eng.account(ctx); err == nil {
		t.Fatalf("failed balance read hidden")
	}
	delete(r.chain.viewErr, usdcTokenForTest+" ft_balance_of")

	// An existing account lists no creation fees.
	if view, err = r.eng.account(ctx); err != nil || view.CreationFees != nil || view.CreationFeesUnavailable {
		t.Fatalf("creation fees with an account: %+v, %v", view, err)
	}

	// Discovery that cannot conclude is reported with a closed gate, and
	// the creation fees as unavailable rather than listed.
	r.chain.viewErr["multica.near get_mca_by_wallet"] = errors.New("endpoint down")
	if view, err = r.eng.account(ctx); err != nil || view.Discovery != RheaUnavailable || view.Admission.Open || view.MCA != "" ||
		!view.CreationFeesUnavailable || view.CreationFees != nil {
		t.Fatalf("unavailable discovery: %+v, %v", view, err)
	}
}

// TestRheaAccountCreationFees lists the one-time creation fee of every
// market while no account exists, each as its own line with its parts.
func TestRheaAccountCreationFees(t *testing.T) {
	r := newSupplyRig(t, true)
	view, err := r.eng.account(tCtx)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if view.Discovery != RheaNone || view.CreationFeesUnavailable || len(view.CreationFees) != len(rhea.Mainnet.Markets) {
		t.Fatalf("creation fees %+v", view)
	}
	// Each market's fee is get_create_mca_fee plus 0.1 NEAR at its near
	// value, valued at the rig's fiat rates (NEAR at 3 USD, ZEC at 900).
	// Each line's creation minimum is the larger of twice the fee and the
	// amount worth 5 USD, rounded up; 5 USD wins everywhere here.
	want := map[string]struct{ amount, fixed, funding, usd, least string }{
		usdcTokenForTest:         {"600000", "300000", "300000", "0.60", "5000000"},
		"usdt.tether-token.near": {"600000", "300000", "300000", "0.60", "5000000"},
		"wrap.near":              {"200000000000000000000000", "100000000000000000000000", "100000000000000000000000", "0.60", "1666666666666666666666667"},
		"zec.omft.near":          {"60000", "30000", "30000", "0.54", "555556"},
	}
	for _, fl := range view.CreationFees {
		w, ok := want[fl.TokenID]
		if !ok || fl.Amount != w.amount || fl.USD != w.usd || !fl.OneTime || fl.Unavailable != "" || len(fl.Components) != 2 ||
			fl.Components[0].Kind != RheaFeeFixed || fl.Components[0].Amount != w.fixed ||
			fl.Components[1].Kind != RheaFeeAccountFunding || fl.Components[1].Amount != w.funding ||
			fl.Components[1].NEAR != "100000000000000000000000" {
			t.Errorf("%s: %+v", fl.TokenID, fl)
		}
		if m := fl.CreationMinimum; m == nil || m.Amount != w.least || m.USD != "5.00" {
			t.Errorf("%s creation minimum %+v, want %s", fl.TokenID, m, w.least)
		}
	}
	// A creation fee large enough that twice it exceeds 5 USD sets the
	// minimum: 2.7 USDC fixed makes the fee 3 USDC and the minimum 6.
	feeArgs := `{"token_id":"` + usdcTokenForTest + `"}`
	r.chain.setView("multica.near", "get_create_mca_fee", feeArgs, `"2700000"`)
	if view, err = r.eng.account(tCtx); err != nil {
		t.Fatal(err)
	}
	for _, fl := range view.CreationFees {
		if fl.TokenID == usdcTokenForTest && (fl.CreationMinimum == nil || fl.CreationMinimum.Amount != "6000000" || fl.CreationMinimum.USD != "6.00") {
			t.Errorf("USDC creation minimum at twice the fee %+v", fl.CreationMinimum)
		}
	}
	r.chain.setView("multica.near", "get_create_mca_fee", feeArgs, `"300000"`)

	// The 1Click catalog plays no part in the USD values.
	r.eng.oneClick = func() (*intentsEngine, error) { return nil, errors.New("no catalog") }
	if view, err = r.eng.account(tCtx); err != nil {
		t.Fatal(err)
	}
	for _, fl := range view.CreationFees {
		if fl.USD != want[fl.TokenID].usd {
			t.Errorf("without a catalog %s: %+v", fl.TokenID, fl)
		}
	}

	// A market with no fresh fiat rate is listed as unavailable, with the
	// reason; the others stay priced.
	r.setRate("ZEC", 0)
	if view, err = r.eng.account(tCtx); err != nil {
		t.Fatal(err)
	}
	for _, fl := range view.CreationFees {
		if fl.TokenID == "zec.omft.near" {
			if fl.Amount != "" || fl.USD != "" || fl.Components != nil || !fl.OneTime || fl.CreationMinimum != nil ||
				fl.Unavailable != "no current USD rate for ZEC; supplying is unavailable until rates refresh" {
				t.Errorf("without a ZEC rate: %+v", fl)
			}
		} else if fl.USD != want[fl.TokenID].usd || fl.Unavailable != "" {
			t.Errorf("without a ZEC rate %s: %+v", fl.TokenID, fl)
		}
	}

	// A fee that cannot be read is reported per market, with no amount.
	r.chain.viewErr["multica.near get_create_mca_fee"] = errors.New("endpoint down")
	if view, err = r.eng.account(tCtx); err != nil {
		t.Fatal(err)
	}
	for _, fl := range view.CreationFees {
		if fl.Amount != "" || fl.Unavailable == "" || !fl.OneTime || fl.Components != nil {
			t.Errorf("unreadable %s: %+v", fl.TokenID, fl)
		}
	}
}

func TestRheaUSD(t *testing.T) {
	for _, tt := range []struct {
		amount   int64
		decimals int
		price    float64
		want     string
	}{
		{600000, 6, 1, "0.60"},
		{600000, 6, 0.999949, "0.60"},
		{5, 6, 1, "0.00"},
		{4999, 6, 1, "0.00"},
		{5000, 6, 1, "0.01"},
		{1_234_567_891, 6, 1, "1234.57"},
		{100, 2, 0, ""},
		{100, 2, -1, ""},
	} {
		if got := rheaUSD(big.NewInt(tt.amount), tt.decimals, tt.price); got != tt.want {
			t.Errorf("rheaUSD(%d, %d, %v) = %q, want %q", tt.amount, tt.decimals, tt.price, got, tt.want)
		}
	}
	if got := rheaUSD(big.NewInt(1), 6, math.NaN()); got != "" {
		t.Errorf("NaN price: %q", got)
	}
	// The exact rate values the amount: 3.125e12 base units at 1.6e-6 USD
	// is 5 USD, where a rate rounded to micro-USD would read 6.25.
	if got := rheaUSD(big.NewInt(3_125_000_000_000), 6, 1.6e-6); got != "5.00" {
		t.Errorf("sub-micro rate: %q", got)
	}
}

func TestRheaPrice(t *testing.T) {
	for _, price := range []float64{math.MaxFloat64, math.Inf(1), math.Inf(-1), math.NaN(), -1, 0, 1e-8, 9.99e-7, 1.0001e12} {
		if _, ok := rheaPrice(price); ok {
			t.Errorf("accepted unusable price %v", price)
		}
	}
	for _, price := range []float64{1e-6, 1.6e-6, 1, 1e12} {
		r, ok := rheaPrice(price)
		if !ok || r.Cmp(new(big.Rat).SetFloat64(price)) != 0 {
			t.Errorf("price %v: %v, %v", price, r, ok)
		}
	}
}

const usdcTokenForTest = "17208628f84f5d6ad33f0da3bbbeb27ffcb398eac501a31bd6ad2011e36133a1"

// TestRheaRefreshOpensGate runs a background refresh that verifies every
// nonce, after which the account view's gate opens once the window has
// passed.
func TestRheaRefreshOpensGate(t *testing.T) {
	ctx := context.Background()
	r := newRheaTestRig(t)
	r.chain.setView(rheaTestMCA, "get_nonce", `{}`, `"3"`)
	for n := uint64(0); n < 3; n++ {
		h := fmt.Sprintf("tx%d", n)
		r.chain.execTx(h, rheaTestMCA, n, true, true)
		r.history.add(rhea.HistoryTypeRelayer, h)
	}
	r.eng.viewed.Store(true)         // the account view was opened this session
	r.eng.tickerRates = tripRates(t) // refresh never reads fiat rates
	r.eng.refresh(ctx)
	r.eng.tickerRates = nil
	if storedCheckpoint(t, r) != 3 {
		t.Fatalf("refresh checkpoint %d", storedCheckpoint(t, r))
	}
	view, err := r.eng.account(ctx)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if view.Admission.RecoveryUnresolved || view.Admission.Open {
		t.Fatalf("admission right after discovery: %+v", view.Admission)
	}
	// Chain time moves past the window.
	r.chain.block.Timestamp = rheaTestBlockTime.Add(rhea.MaxDeadlineWindow + rheaWindowMargin + time.Second)
	r.now = r.chain.block.Timestamp
	if view, err = r.eng.account(ctx); err != nil || !view.Admission.Open {
		t.Fatalf("admission after the window: %+v, %v", view.Admission, err)
	}
	// Logged out, refresh does nothing.
	r.ids = nil
	r.history.pages = 0
	r.eng.refresh(ctx)
	if r.history.pages != 0 {
		t.Fatalf("refresh ran without identities")
	}
}

// TestRheaRefreshNeedsEngagement checks that a wallet that never used
// lending makes no lending requests in the background.
func TestRheaRefreshNeedsEngagement(t *testing.T) {
	ctx := context.Background()
	r := newRheaTestRig(t)
	calls := func() int {
		r.chain.mtx.Lock()
		defer r.chain.mtx.Unlock()
		return r.chain.txCalls
	}
	r.chain.setView(rheaTestMCA, "get_nonce", `{}`, `"1"`)
	r.chain.execTx("tx0", rheaTestMCA, 0, true, true)
	r.history.add(rhea.HistoryTypeRelayer, "tx0")

	r.chain.viewErr["multica.near get_mca_by_wallet"] = errors.New("discovery must not run")
	r.eng.refresh(ctx)
	if r.history.pages != 0 || calls() != 0 {
		t.Fatalf("background refresh ran without engagement")
	}
	delete(r.chain.viewErr, "multica.near get_mca_by_wallet")

	// A stored account record engages the engine.
	rheaTestAccount(t, r, 0)
	r.eng.refresh(ctx)
	if storedCheckpoint(t, r) != 1 {
		t.Fatalf("engaged refresh did not run")
	}

	// So does an active operation, or opening the account view.
	r = newRheaTestRig(t)
	_ = r.db.StoreRheaOp(&db.RheaOp{ID: "op", Kind: db.RheaOpSupply, State: db.IntentsStateDepositSent})
	if !r.eng.engaged(r.ids) {
		t.Fatalf("active operation did not engage")
	}
	r = newRheaTestRig(t)
	if r.eng.engaged(r.ids) {
		t.Fatalf("engaged with no history")
	}
	if _, err := r.eng.account(ctx); err != nil {
		t.Fatalf("account: %v", err)
	}
	if !r.eng.engaged(r.ids) {
		t.Fatalf("account view did not engage")
	}
	r = newRheaTestRig(t)
	r.db.rheaErr = errors.New("db down")
	if r.eng.engaged(r.ids) {
		t.Fatalf("engaged on a database error")
	}
}

func TestRheaMarkets(t *testing.T) {
	r := newRheaTestRig(t)
	view, err := r.eng.markets(context.Background())
	if err != nil {
		t.Fatalf("markets: %v", err)
	}
	if len(view.Markets) != len(rhea.Mainnet.Markets) || view.Block.Hash != rheaTestBlockHash {
		t.Fatalf("markets %+v", view)
	}
	usdc := view.Markets[0]
	if usdc.Symbol != "USDC" || usdc.Decimals != 6 || usdc.ExtraDecimals != 12 || !usdc.CanDeposit || !usdc.CanWithdraw {
		t.Fatalf("USDC market %+v", usdc)
	}
	if usdc.Supplied != "12119320531471" || usdc.Borrowed != "10647947794480" || usdc.SupplyHeadroom != "47880679468528" {
		t.Fatalf("USDC amounts %+v", usdc)
	}
	if usdc.AvailableLiquidity != "1672048105177" || usdc.Utilization < 0.87 || usdc.Utilization > 0.88 {
		t.Fatalf("USDC liquidity %s utilization %f", usdc.AvailableLiquidity, usdc.Utilization)
	}
	// Without a fiat rate a market is marked unavailable, not hidden.
	for _, mk := range view.Markets {
		if mk.SupplyUnavailable != "no current USD rate for "+mk.Symbol+"; supplying is unavailable until rates refresh" {
			t.Fatalf("market without a rate %+v", mk)
		}
	}
	r.eng.tickerRates = marketRates
	if view, err = r.eng.markets(context.Background()); err != nil {
		t.Fatalf("markets: %v", err)
	}
	for _, mk := range view.Markets {
		if mk.SupplyUnavailable != "" {
			t.Fatalf("market with a rate %+v", mk)
		}
	}
	r.chain.viewErr["zec.omft.near ft_metadata"] = errors.New("endpoint down")
	if _, err := r.eng.markets(context.Background()); err == nil {
		t.Fatalf("partial markets returned")
	}
}

func TestStartRheaEngine(t *testing.T) {
	testDep := *rhea.Mainnet
	testDep.AccountManager = "am.test.near"
	start := func(net util.Network, tc *RheaTestConfig) *Core {
		c := &Core{cfg: &Config{RheaTest: tc}, net: net, log: tLogger, db: &TDB{}}
		ctx, cancel := context.WithCancel(context.Background())
		c.startRheaEngine(ctx)
		cancel()
		c.wg.Wait()
		return c
	}
	tc := &RheaTestConfig{Deployment: &testDep, RPCEndpoints: []string{"http://127.0.0.1:1"}}

	// Mainnet ignores the test configuration.
	c := start(util.Mainnet, tc)
	eng, err := c.rheaEngine()
	if err != nil || eng.dep != rhea.Mainnet {
		t.Fatalf("mainnet engine %v, %v", eng, err)
	}
	// The engine reads the app's ticker rates: an asset's fresh fiat rate
	// under its normalized ticker, and nothing for an expired one.
	src := newCommonRateSource(nil)
	src.fiatRates[near.BipID] = &fiatRateInfo{rate: 3, lastUpdate: time.Now()}
	c.fiatRateSources = map[string]*commonRateSource{"test": src}
	if rate, ok := eng.tickerRate("NEAR"); !ok || rate != 3 {
		t.Fatalf("engine NEAR rate %v, %v", rate, ok)
	}
	src.fiatRates[near.BipID].lastUpdate = time.Now().Add(-fiatRateDataExpiry)
	if rate, ok := eng.tickerRate("NEAR"); ok {
		t.Fatalf("expired NEAR rate %v used", rate)
	}
	// Elsewhere the engine needs a test configuration.
	if _, err := start(util.Simnet, nil).rheaEngine(); err == nil {
		t.Fatalf("simnet engine without a configuration")
	}
	if _, err := start(util.Testnet, &RheaTestConfig{Deployment: &testDep}).rheaEngine(); err == nil {
		t.Fatalf("engine without endpoints")
	}
	c = start(util.Simnet, tc)
	if eng, err = c.rheaEngine(); err != nil || eng.dep != &testDep {
		t.Fatalf("simnet engine %v, %v", eng, err)
	}
	if _, err := c.RheaAccount(context.Background()); err == nil {
		t.Fatalf("account view while logged out")
	}
	if ops, err := c.RheaOps(); err != nil || len(ops) != 0 {
		t.Fatalf("RheaOps: %v, %v", ops, err)
	}
}

// funcHistory serves relayer history from a function, for malformed
// pagination.
type funcHistory struct {
	pages int
	page  func(page, size int) *rhea.HistoryPage
}

func (h *funcHistory) History(_ context.Context, _ string, page, size int) (*rhea.HistoryPage, error) {
	h.pages++
	return h.page(page, size), nil
}

func supplyRecords(first, n int) []*rhea.HistoryRecord {
	recs := make([]*rhea.HistoryRecord, n)
	for i := range recs {
		recs[i] = &rhea.HistoryRecord{ID: int64(first + i + 1), Type: rhea.HistoryTypeSupply, MCAID: rheaTestMCA, RequestHash: fmt.Sprintf("s%d", first+i)}
	}
	return recs
}

// TestRheaWalkBounds feeds the nonce walk relayer histories that would
// otherwise never end. Every one must stop within the record and page
// bounds and leave recovery admission unresolved.
func TestRheaWalkBounds(t *testing.T) {
	ctx := context.Background()
	const hugeTotal = 1_000_000

	// Supply records only, with a page count that never ends.
	r := newRheaTestRig(t)
	h := &funcHistory{page: func(page, size int) *rhea.HistoryPage {
		return &rhea.HistoryPage{Records: supplyRecords((page-1)*size, size), TotalPage: hugeTotal}
	}}
	r.eng.history = h
	acct := rheaTestAccount(t, r, 0)
	err := r.eng.walkNonces(ctx, acct, 1)
	if err == nil || !strings.Contains(err.Error(), "exceeds") || h.pages > rheaMaxHistoryPages || r.chain.txCalls != 0 {
		t.Fatalf("all-supply history: %v after %d pages, %d lookups", err, h.pages, r.chain.txCalls)
	}

	// The same page served for every page number.
	r = newRheaTestRig(t)
	h = &funcHistory{page: func(_, size int) *rhea.HistoryPage {
		return &rhea.HistoryPage{Records: supplyRecords(0, size), TotalPage: hugeTotal}
	}}
	r.eng.history = h
	acct = rheaTestAccount(t, r, 0)
	if err = r.eng.walkNonces(ctx, acct, 1); err == nil || !strings.Contains(err.Error(), "repeats") || h.pages != 2 {
		t.Fatalf("repeated page: %v after %d pages", err, h.pages)
	}

	// A page larger than requested.
	r = newRheaTestRig(t)
	h = &funcHistory{page: func(_, size int) *rhea.HistoryPage {
		return &rhea.HistoryPage{Records: supplyRecords(0, size*100), TotalPage: hugeTotal}
	}}
	r.eng.history = h
	acct = rheaTestAccount(t, r, 0)
	if err = r.eng.walkNonces(ctx, acct, 1); err == nil || !strings.Contains(err.Error(), "more than") || h.pages != 1 {
		t.Fatalf("oversized page: %v after %d pages", err, h.pages)
	}

	// Stopped early, the verified prefix still counts.
	r = newRheaTestRig(t)
	r.chain.execTx("tx1", rheaTestMCA, 1, true, true)
	r.chain.execTx("tx0", rheaTestMCA, 0, true, true)
	h = &funcHistory{page: func(page, size int) *rhea.HistoryPage {
		recs := supplyRecords(page*size, size-2)
		if page == 1 {
			recs = append([]*rhea.HistoryRecord{
				{ID: -1, Type: rhea.HistoryTypeRelayer, MCAID: rheaTestMCA, RequestHash: "tx1"},
				{ID: -2, Type: rhea.HistoryTypeRelayer, MCAID: rheaTestMCA, RequestHash: "tx0"},
			}, recs...)
		}
		return &rhea.HistoryPage{Records: recs, TotalPage: hugeTotal}
	}}
	r.eng.history = h
	acct = rheaTestAccount(t, r, 0)
	err = r.eng.walkNonces(ctx, acct, 3)
	if err == nil || !strings.Contains(err.Error(), "nonce 2") || !strings.Contains(err.Error(), "stopped") || storedCheckpoint(t, r) != 2 {
		t.Fatalf("bounded walk with a verified prefix: %v, checkpoint %d", err, storedCheckpoint(t, r))
	}

	// Cancellation ends the walk without touching the checkpoint.
	r = newRheaTestRig(t)
	cctx, cancel := context.WithCancel(ctx)
	h = &funcHistory{page: func(page, size int) *rhea.HistoryPage {
		cancel()
		return &rhea.HistoryPage{Records: supplyRecords((page-1)*size, size), TotalPage: hugeTotal}
	}}
	r.eng.history = h
	acct = rheaTestAccount(t, r, 0)
	if err = r.eng.walkNonces(cctx, acct, 1); !errors.Is(err, context.Canceled) || h.pages != 1 {
		t.Fatalf("canceled walk: %v after %d pages", err, h.pages)
	}
}

// TestRheaInferredOpEngages checks that a supply whose leg was inferred
// FAILED keeps the engine engaged until the inference is retired.
func TestRheaInferredOpEngages(t *testing.T) {
	r := newRheaTestRig(t)
	op := &db.RheaOp{ID: "op", Kind: db.RheaOpSupply, State: "FAILED",
		Leg: &db.RheaLeg{Inferred: true, InferredFrom: "PENDING_DEPOSIT"}}
	if err := r.db.StoreRheaOp(op); err != nil {
		t.Fatal(err)
	}
	if !r.eng.engaged(r.ids) {
		t.Fatalf("inferred FAILED supply did not engage")
	}
	op.RetireInference()
	if err := r.db.StoreRheaOp(op); err != nil {
		t.Fatal(err)
	}
	if r.eng.engaged(r.ids) {
		t.Fatalf("retired supply still engages")
	}
}

// TestRheaAccountViewBlockMismatch refuses an MCA account state read at a
// block other than the view's final block.
func TestRheaAccountViewBlockMismatch(t *testing.T) {
	r := newRheaTestRig(t)
	r.chain.accounts[rheaTestMCA].BlockHash = "OTHERHASH"
	if _, err := r.eng.account(context.Background()); err == nil || !strings.Contains(err.Error(), "OTHERHASH") {
		t.Fatalf("account state from another block: %v", err)
	}
}
