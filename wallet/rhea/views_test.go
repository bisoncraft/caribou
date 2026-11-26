package rhea

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
)

// capturedViews is testdata/views.json: mainnet view results all read at
// one final block.
type capturedViews struct {
	Block struct {
		Height uint64 `json:"height"`
		Hash   string `json:"hash"`
	} `json:"block"`
	Views []struct {
		Contract string          `json:"contract"`
		Method   string          `json:"method"`
		Args     json.RawMessage `json:"args"`
		Result   *string         `json:"result"`
		Error    string          `json:"error"`
	} `json:"views"`
}

func loadViews(t *testing.T) *capturedViews {
	t.Helper()
	var cv capturedViews
	if err := json.Unmarshal(readTestdata(t, "views.json"), &cv); err != nil {
		t.Fatalf("error decoding views: %v", err)
	}
	return &cv
}

// viewResult returns the captured result of the first view of method on
// contract.
func viewResult(t *testing.T, contract, method string) []byte {
	t.Helper()
	for _, v := range loadViews(t).Views {
		if v.Contract == contract && v.Method == method && v.Result != nil {
			return []byte(*v.Result)
		}
	}
	t.Fatalf("no captured %s.%s", contract, method)
	return nil
}

// canonicalArgs re-encodes JSON args with sorted keys for lookup.
func canonicalArgs(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("bad args %s: %v", b, err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// fakeViewer serves captured views. Lookups that were not captured, or
// were captured as errors, fail.
type fakeViewer struct {
	t       *testing.T
	height  uint64
	hash    string
	results map[string][]byte
	// override replaces results by key; err fails every call.
	override map[string][]byte
	err      error
	// replyHash, when set, is reported instead of the requested block.
	replyHash string
}

func viewKey(contract, method, args string) string {
	return contract + " " + method + " " + args
}

func newFakeViewer(t *testing.T) *fakeViewer {
	cv := loadViews(t)
	f := &fakeViewer{t: t, height: cv.Block.Height, hash: cv.Block.Hash, results: make(map[string][]byte), override: make(map[string][]byte)}
	for _, v := range cv.Views {
		if v.Result == nil {
			continue
		}
		f.results[viewKey(v.Contract, v.Method, canonicalArgs(t, v.Args))] = []byte(*v.Result)
	}
	return f
}

func (f *fakeViewer) CallFunction(_ context.Context, contract, method string, args []byte, blockHash string) (*ViewResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	if blockHash != "" && blockHash != f.hash {
		return nil, errors.New("unknown block")
	}
	key := viewKey(contract, method, canonicalArgs(f.t, args))
	res, ok := f.override[key]
	if !ok {
		if res, ok = f.results[key]; !ok {
			return nil, errors.New("not captured: " + key)
		}
	}
	hash := f.hash
	if f.replyHash != "" {
		hash = f.replyHash
	}
	return &ViewResult{Result: res, BlockHeight: f.height, BlockHash: hash}, nil
}

func TestReaderAccountManager(t *testing.T) {
	f := newFakeViewer(t)
	r := NewReader(f, Mainnet)
	ctx := context.Background()
	B := f.hash

	mca, at, err := r.MCAByWallet(ctx, "242c9fc3162321d326892cfe044fc0d9ffc3e9b0", B)
	if err != nil || mca != "rhea001569.multica.near" {
		t.Fatalf("MCAByWallet = %q, %v", mca, err)
	}
	if at.Hash != B || at.Height != f.height {
		t.Fatalf("wrong block reference %+v", at)
	}
	// An authoritative null is absence; the unregistered identity was
	// captured returning null.
	mca, at, err = r.MCAByWallet(ctx, "7e5f4552091a69125d5dfcb7b8c2659029395bdf", B)
	if err != nil || mca != "" || at.Hash != B {
		t.Fatalf("unregistered identity: %q, %+v, %v", mca, at, err)
	}
	// A failure to answer is never absence.
	f.err = errors.New("endpoint down")
	if mca, _, err = r.MCAByWallet(ctx, "242c9fc3162321d326892cfe044fc0d9ffc3e9b0", B); err == nil || mca != "" {
		t.Fatalf("RPC failure reported as %q, %v", mca, err)
	}
	f.err = nil
	// A result for a different block than requested is refused.
	f.replyHash = "OTHER"
	if _, _, err = r.MCAByWallet(ctx, "242c9fc3162321d326892cfe044fc0d9ffc3e9b0", B); err == nil {
		t.Fatalf("result from another block accepted")
	}
	f.replyHash = ""
	// A returned account that is not an MCA of the deployment is refused.
	key := viewKey("multica.near", "get_mca_by_wallet", `{"wallet":{"EVM":"242c9fc3162321d326892cfe044fc0d9ffc3e9b0"}}`)
	for _, bad := range []string{`"evil.near"`, `7`, ``, `{"a":1}`} {
		f.override[key] = []byte(bad)
		if _, _, err = r.MCAByWallet(ctx, "242c9fc3162321d326892cfe044fc0d9ffc3e9b0", B); err == nil {
			t.Fatalf("result %q accepted", bad)
		}
	}
	delete(f.override, key)

	ws, _, err := r.WalletsByMCA(ctx, "rhea001569.multica.near", B)
	if err != nil || len(ws) != 4 {
		t.Fatalf("WalletsByMCA = %v, %v", ws, err)
	}
	kinds := map[string]string{}
	for _, w := range ws {
		kinds[w.Kind] = w.ID
	}
	if kinds[WalletKindEVM] != "242c9fc3162321d326892cfe044fc0d9ffc3e9b0" || kinds["Near"] != "bonjupiglet.near" || kinds["WebAuthn"] == "" {
		t.Fatalf("wallets %+v", ws)
	}

	n, _, err := r.Nonce(ctx, "rhea001569.multica.near", B)
	if err != nil || n != 90 {
		t.Fatalf("Nonce = %d, %v", n, err)
	}
	fee, _, err := r.CreateMCAFee(ctx, usdcToken, B)
	if err != nil || fee.Int64() != 300_000 {
		t.Fatalf("CreateMCAFee = %v, %v", fee, err)
	}
	nv, _, err := r.NearValue(ctx, "zec.omft.near", B)
	if err != nil || nv.Int64() != 300_000 {
		t.Fatalf("NearValue = %v, %v", nv, err)
	}
	ib, _, err := r.MCAInitBalance(ctx, B)
	if err != nil || ib.String() != "100000000000000000000000" {
		t.Fatalf("MCAInitBalance = %v, %v", ib, err)
	}
	h, _, err := r.LatestMCACodeHash(ctx, B)
	if err != nil || h != "FDX6nYTmwVVwkjSXwFZ8xRjkPsyU1BEt1QqB9TbXwD11" {
		t.Fatalf("LatestMCACodeHash = %s, %v", h, err)
	}
	if _, err := r.CheckSupplyTemplates(ctx, B); err != nil {
		t.Fatalf("CheckSupplyTemplates: %v", err)
	}
	tkey := viewKey("multica.near", "get_reliable_business_template_paged", `{"from_index":0,"limit":100}`)
	f.override[tkey] = []byte(`null`)
	if _, err := r.CheckSupplyTemplates(ctx, B); err == nil {
		t.Fatalf("null templates accepted")
	}
	delete(f.override, tkey)

	// Malformed amounts are errors.
	nkey := viewKey("multica.near", "get_near_value", `{"token_id":"zec.omft.near"}`)
	for _, bad := range []string{`null`, `"-1"`, `"1.5"`, `300000`, `""`} {
		f.override[nkey] = []byte(bad)
		if _, _, err := r.NearValue(ctx, "zec.omft.near", B); err == nil {
			t.Fatalf("near value %s accepted", bad)
		}
	}
}

func TestReaderBurrow(t *testing.T) {
	f := newFakeViewer(t)
	r := NewReader(f, Mainnet)
	ctx := context.Background()
	B := f.hash

	a, _, err := r.Asset(ctx, usdcToken, B)
	if err != nil {
		t.Fatalf("Asset: %v", err)
	}
	if a.Config.ExtraDecimals != 12 || !a.Config.CanDeposit || !a.Config.CanWithdraw || a.SupplyAPR == "" {
		t.Fatalf("asset config %+v", a.Config)
	}
	if a.Config.SuppliedLimit == nil || a.Config.SuppliedLimit.String() != "60000000000000000000000000" {
		t.Fatalf("supplied limit %v", a.Config.SuppliedLimit)
	}
	// supplied + reserved + prot_fee − borrowed − margin debt − pending.
	want := mustBig("12119320531471864368726346")
	want.Add(want, mustBig("200002373581382714624455"))
	want.Add(want, mustBig("672994604193599068728"))
	want.Sub(want, mustBig("10647947794480010453769794"))
	if got := a.AvailableLiquidity(); got.Cmp(want) != 0 {
		t.Fatalf("available liquidity %s, want %s", got, want)
	}
	headroom := new(big.Int).Sub(mustBig("60000000000000000000000000"), mustBig("12119320531471864368726346"))
	if got := a.SupplyHeadroom(); got.Cmp(headroom) != 0 {
		t.Fatalf("supply headroom %s, want %s", got, headroom)
	}
	for _, tok := range []string{"usdt.tether-token.near", "wrap.near", "zec.omft.near"} {
		if _, _, err := r.Asset(ctx, tok, B); err != nil {
			t.Fatalf("Asset(%s): %v", tok, err)
		}
	}

	// rhea001569 holds plain supply, collateral, and debt.
	p, _, err := r.AccountPositions(ctx, "rhea001569.multica.near", B)
	if err != nil || p == nil {
		t.Fatalf("AccountPositions: %v", err)
	}
	if got := p.SuppliedBalance(usdcToken); got.String() != "1387814608947877" {
		t.Fatalf("supplied %s", got)
	}
	if got := p.CollateralBalance("wrap.near"); got.String() != "200920577216290306447789830" {
		t.Fatalf("collateral %s", got)
	}
	if got := p.BorrowedBalance(usdcToken); got.String() != "13004972106821978168687" {
		t.Fatalf("borrowed %s", got)
	}
	if !p.HasDebt() || p.SuppliedBalance("wrap.near").Sign() != 0 {
		t.Fatalf("debt or supply misread")
	}
	// rhea001594 (created by the captured creation supply) holds plain
	// supply only.
	p, _, err = r.AccountPositions(ctx, "rhea001594.multica.near", B)
	if err != nil || p.SuppliedBalance("usdt.tether-token.near").String() != "29998854986533613" || p.HasDebt() {
		t.Fatalf("rhea001594 positions %+v, %v", p, err)
	}
	if p.CollateralBalance("usdt.tether-token.near").Sign() != 0 {
		t.Fatalf("rhea001594 collateral misread")
	}
	pkey := viewKey("contract.main.burrow.near", "get_account_all_positions", `{"account_id":"rhea001625.multica.near"}`)
	f.override[pkey] = []byte(`null`)
	if p, at, err := r.AccountPositions(ctx, "rhea001625.multica.near", B); err != nil || p != nil || at.Hash != B {
		t.Fatalf("unregistered account: %+v, %v", p, err)
	}

	m, _, err := r.FTMetadata(ctx, "wrap.near", B)
	if err != nil || m.Decimals != 24 || m.Symbol != "wNEAR" {
		t.Fatalf("FTMetadata = %+v, %v", m, err)
	}
	bal, _, err := r.FTBalance(ctx, usdcToken, "rhea001569.multica.near", B)
	if err != nil || bal.Int64() != 52 {
		t.Fatalf("FTBalance = %v, %v", bal, err)
	}
	sb, _, err := r.StorageBalanceOf(ctx, usdcToken, "rhea001569.multica.near", B)
	if err != nil || sb == nil || sb.Total.String() != StorageDeposit {
		t.Fatalf("StorageBalanceOf = %+v, %v", sb, err)
	}
	sb, _, err = r.StorageBalanceOf(ctx, "usdt.tether-token.near", "rhea001569.multica.near", B)
	if err != nil || sb != nil {
		t.Fatalf("unregistered storage = %+v, %v", sb, err)
	}
}
