package nearintentsmock

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/intents"
)

var testLogger = util.StdOutLogger("NIMOCKTEST", util.LevelError)

// fakeDriver is an in-memory Driver for network-free tests.
type fakeDriver struct {
	slug string

	mtx      sync.Mutex
	addrN    int
	deposits map[string]*big.Int
	sends    map[string]*big.Int
	sendN    int
	confirms int
	sendErr  error
}

func newFakeDriver(slug string) *fakeDriver {
	return &fakeDriver{
		slug:     slug,
		deposits: make(map[string]*big.Int),
		sends:    make(map[string]*big.Int),
	}
}

func (d *fakeDriver) Slug() string { return d.slug }

func (d *fakeDriver) NewDepositAddress() (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	d.addrN++
	return fmt.Sprintf("%s-deposit-%d", d.slug, d.addrN), nil
}

func (d *fakeDriver) CheckDeposit(addr string) (*big.Int, string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	amt := d.deposits[addr]
	if amt == nil {
		return new(big.Int), "", nil
	}
	return new(big.Int).Set(amt), "", nil
}

func (d *fakeDriver) Send(addr string, amount *big.Int) (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	if d.sendErr != nil {
		return "", d.sendErr
	}
	d.sends[addr] = new(big.Int).Set(amount)
	d.sendN++
	return fmt.Sprintf("%s-tx-%d", d.slug, d.sendN), nil
}

func (d *fakeDriver) Confirm() error {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	d.confirms++
	return nil
}

func (d *fakeDriver) fund(addr string, amount *big.Int) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	d.deposits[addr] = new(big.Int).Set(amount)
}

func (d *fakeDriver) sentTo(addr string) *big.Int {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	return d.sends[addr]
}

// fakeTokenDriver extends fakeDriver with TokenDriver behavior.
type fakeTokenDriver struct {
	*fakeDriver
	tokens        []*driverToken
	tokenDeposits map[string]*big.Int // key: contract|addr
	tokenSends    map[string]*big.Int // key: addr
}

func newFakeTokenDriver(slug string, tokens []*driverToken) *fakeTokenDriver {
	return &fakeTokenDriver{
		fakeDriver:    newFakeDriver(slug),
		tokens:        tokens,
		tokenDeposits: make(map[string]*big.Int),
		tokenSends:    make(map[string]*big.Int),
	}
}

func (d *fakeTokenDriver) Tokens() []*driverToken { return d.tokens }

func (d *fakeTokenDriver) CheckTokenDeposit(contract, addr string) (*big.Int, string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	amt := d.tokenDeposits[contract+"|"+addr]
	if amt == nil {
		return new(big.Int), "", nil
	}
	return new(big.Int).Set(amt), "", nil
}

func (d *fakeTokenDriver) SendToken(script, addr string, amount *big.Int, decimals int) (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	if script == "" {
		return "", fmt.Errorf("no send script")
	}
	d.tokenSends[addr] = new(big.Int).Set(amount)
	d.sendN++
	return fmt.Sprintf("%s-token-tx-%d", d.slug, d.sendN), nil
}

func (d *fakeTokenDriver) fundToken(contract, addr string, amount *big.Int) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	d.tokenDeposits[contract+"|"+addr] = new(big.Int).Set(amount)
}

// testRig is a running mock server with fake btc and near drivers and a
// real intents client pointed at it.
type testRig struct {
	server *Server
	btc    *fakeDriver
	near   *fakeDriver
	client *intents.Client
}

func newTestRig(t *testing.T) *testRig {
	t.Helper()
	btc, near := newFakeDriver("btc"), newFakeDriver("near")
	s, err := New(&Config{
		Drivers:         map[string]Driver{"btc": btc, "near": near},
		ProcessingDelay: time.Millisecond,
		Logger:          nil, // default
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.Run(ctx)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	c, err := intents.NewClient(&intents.Config{
		BaseURL:    srv.URL,
		SigningKey: s.PubKey(),
		AppFees:    []intents.AppFee{{Recipient: "0xfeeaccount", Fee: 20}},
	}, testLogger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return &testRig{server: s, btc: btc, near: near, client: c}
}

func testDeadline(d time.Duration) string {
	return time.Now().Add(d).UTC().Format(timestampFormat)
}

func testQuoteRequest(dry bool) *intents.QuoteRequest {
	return &intents.QuoteRequest{
		Dry:               dry,
		SwapType:          intents.SwapTypeExactInput,
		SlippageTolerance: 100,
		OriginAsset:       "nep141:btc.omft.near",
		DepositType:       intents.LocationOriginChain,
		DestinationAsset:  "nep141:wrap.near",
		Amount:            "100000000", // 1 BTC
		RefundTo:          "bc1qrefund",
		RefundType:        intents.LocationOriginChain,
		Recipient:         "nearrecipient.near",
		RecipientType:     intents.LocationDestinationChain,
		Deadline:          testDeadline(time.Hour),
	}
}

// TestDevKeySync asserts the mock's deterministic signing key matches the
// constant pinned in the wallet.
func TestDevKeySync(t *testing.T) {
	if _, pub := DevSigningKey(); pub != intents.SimnetSigningKey {
		t.Fatalf("DevSigningKey %s != intents.SimnetSigningKey %s", pub, intents.SimnetSigningKey)
	}
}

// TestCatalog asserts the published tokens resolve as chain natives through
// the wallet's own catalog logic.
func TestCatalog(t *testing.T) {
	rig := newTestRig(t)
	cat, err := rig.client.Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	for slug, bipID := range map[string]uint32{"btc": 0, "near": 397} {
		native := cat.Native(bipID)
		if native == nil {
			t.Fatalf("no native for %s (bip %d)", slug, bipID)
		}
		if native.Decimals != chainSpecs[slug].decimals {
			t.Fatalf("%s decimals = %d, want %d", slug, native.Decimals, chainSpecs[slug].decimals)
		}
	}
}

// TestQuoteMath verifies quote arithmetic through the real client (which
// verifies the signature and the request echo) with the static fallback
// prices: 1 BTC at $60,000 -> 12,000 NEAR at $5, minus 30 bps fees (20
// configured appFee + 10 keyless surcharge), minus 100 bps slippage for
// the minimum.
func TestQuoteMath(t *testing.T) {
	rig := newTestRig(t)
	resp, err := rig.client.Quote(context.Background(), testQuoteRequest(true))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	q := &resp.Quote
	if q.DepositAddress != "" {
		t.Fatalf("dry quote has deposit address %q", q.DepositAddress)
	}
	// 12000e24 * 0.997
	if want := "11964000000000000000000000000"; q.AmountOut != want {
		t.Fatalf("amountOut = %s, want %s", q.AmountOut, want)
	}
	// amountOut * 0.99
	if want := "11844360000000000000000000000"; q.MinAmountOut != want {
		t.Fatalf("minAmountOut = %s, want %s", q.MinAmountOut, want)
	}
	if q.AmountInUsd != "60000.00" {
		t.Fatalf("amountInUsd = %s, want 60000.00", q.AmountInUsd)
	}
	if q.AmountOutUsd != "59820.00" {
		t.Fatalf("amountOutUsd = %s, want 59820.00", q.AmountOutUsd)
	}
	if q.AmountInFormatted != "1" {
		t.Fatalf("amountInFormatted = %s, want 1", q.AmountInFormatted)
	}

	// Determinism: a committed quote must price identically.
	resp2, err := rig.client.Quote(context.Background(), testQuoteRequest(false))
	if err != nil {
		t.Fatalf("committed Quote: %v", err)
	}
	if resp2.Quote.MinAmountOut != q.MinAmountOut || resp2.Quote.AmountOut != q.AmountOut {
		t.Fatalf("committed quote priced differently: %s/%s vs %s/%s",
			resp2.Quote.AmountOut, resp2.Quote.MinAmountOut, q.AmountOut, q.MinAmountOut)
	}
	if resp2.Quote.DepositAddress == "" {
		t.Fatal("committed quote has no deposit address")
	}
	if resp2.Quote.DepositMemo != "" {
		t.Fatalf("committed quote has deposit memo %q", resp2.Quote.DepositMemo)
	}
}

// TestAppFeeRewrite asserts the echoed appFees array models the service's
// keyless mechanics: the rig's configured 20 bps is split 10 to its
// recipient and 10 to the operator, the 10 bps surcharge lands on the
// operator's entry, and the array sums to the priced 30 bps haircut.
func TestAppFeeRewrite(t *testing.T) {
	rig := newTestRig(t)
	resp, err := rig.client.Quote(context.Background(), testQuoteRequest(true))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	want := []intents.AppFee{
		{Recipient: "0xfeeaccount", Fee: 10},
		{Recipient: operatorFeeAccount, Fee: 20},
	}
	if !reflect.DeepEqual(resp.Request.AppFees, want) {
		t.Fatalf("echoed appFees = %+v, want %+v", resp.Request.AppFees, want)
	}

	// With no configured fees, only the surcharge entry is injected.
	if fees := rewriteAppFees(nil); !reflect.DeepEqual(fees, []intents.AppFee{{Recipient: operatorFeeAccount, Fee: 10}}) {
		t.Fatalf("rewriteAppFees(nil) = %+v", fees)
	}
}

func TestQuoteMathExactOutput(t *testing.T) {
	rig := newTestRig(t)
	req := testQuoteRequest(true)
	req.SwapType = intents.SwapTypeExactOutput
	// The exact-input case quotes 1 BTC -> 11964 NEAR (TestQuoteMath);
	// requesting that output must invert to exactly the 1 BTC input.
	req.Amount = "11964000000000000000000000000"
	resp, err := rig.client.Quote(context.Background(), req)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	q := &resp.Quote
	if want := "100000000"; q.AmountIn != want {
		t.Fatalf("amountIn = %s, want %s", q.AmountIn, want)
	}
	if q.AmountOut != req.Amount {
		t.Fatalf("amountOut = %s, want the requested %s", q.AmountOut, req.Amount)
	}
	// The output is exact; slippage erodes nothing.
	if q.MinAmountOut != req.Amount {
		t.Fatalf("minAmountOut = %s, want the requested %s", q.MinAmountOut, req.Amount)
	}

	// A requested output needing a fractional-atom input rounds the input
	// up, never down: one yoctoNEAR more must price one more satoshi.
	req.Amount = "11964000000000000000000000001"
	resp, err = rig.client.Quote(context.Background(), req)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if want := "100000001"; resp.Quote.AmountIn != want {
		t.Fatalf("amountIn = %s, want %s (rounded up)", resp.Quote.AmountIn, want)
	}
}

// waitStatus polls until the swap reaches the wanted status.
func waitStatus(t *testing.T, rig *testRig, addr, want string) *intents.StatusResponse {
	t.Helper()
	var last string
	for range 200 {
		st, err := rig.client.Status(context.Background(), addr)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Status == want {
			return st
		}
		last = st.Status
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("swap never reached %s (last %s)", want, last)
	return nil
}

func TestLifecycleSuccess(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	resp, err := rig.client.Quote(ctx, testQuoteRequest(false))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	addr := resp.Quote.DepositAddress

	st, err := rig.client.Status(ctx, addr)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Status != intents.StatusPendingDeposit {
		t.Fatalf("initial status = %s", st.Status)
	}
	if st.QuoteResponse == nil {
		t.Fatal("status carries no quote envelope")
	}

	// Deposit the full amount and submit the hint; the poke makes
	// detection immediate.
	amountIn, _ := new(big.Int).SetString(resp.Quote.AmountIn, 10)
	rig.btc.fund(addr, amountIn)
	if err := rig.client.SubmitDeposit(ctx, addr, "userdeposittx"); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}

	st = waitStatus(t, rig, addr, intents.StatusSuccess)
	if st.SwapDetails.AmountOut != resp.Quote.AmountOut {
		t.Fatalf("realized amountOut = %s, want %s", st.SwapDetails.AmountOut, resp.Quote.AmountOut)
	}
	if len(st.SwapDetails.DestinationChainTxs) == 0 {
		t.Fatal("no destination tx hashes")
	}
	if len(st.SwapDetails.NearTxHashes) == 0 {
		t.Fatal("no near tx hashes")
	}
	if len(st.SwapDetails.OriginChainTxs) == 0 || st.SwapDetails.OriginChainTxs[0].Hash != "userdeposittx" {
		t.Fatalf("origin txs = %+v, want the submitted hint first", st.SwapDetails.OriginChainTxs)
	}
	wantOut, _ := new(big.Int).SetString(resp.Quote.AmountOut, 10)
	if got := rig.near.sentTo("nearrecipient.near"); got == nil || got.Cmp(wantOut) != 0 {
		t.Fatalf("near payout = %v, want %s", got, wantOut)
	}
	if rig.btc.confirms == 0 {
		t.Fatal("origin chain never confirmed")
	}
}

// TestArmedOutcome covers /test/force with no deposit address: the outcome
// is armed for the next swap created, consumed one-shot, and cleared by an
// address-less RESUME.
func TestArmedOutcome(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	force := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/test/force", strings.NewReader(body))
		rig.server.handleForce(w, r)
		return w
	}
	pending := func() string {
		rig.server.swapMtx.RLock()
		defer rig.server.swapMtx.RUnlock()
		return rig.server.pendingForce
	}

	// An armed outcome can be cleared before any swap consumes it.
	if w := force(`{"outcome":"FAIL"}`); w.Code != 200 {
		t.Fatalf("arm FAIL: status %d", w.Code)
	}
	if got := pending(); got != forceFail {
		t.Fatalf("pendingForce = %q, want %q", got, forceFail)
	}
	if w := force(`{"outcome":"RESUME"}`); w.Code != 200 {
		t.Fatalf("clear arm: status %d", w.Code)
	}
	if got := pending(); got != "" {
		t.Fatalf("pendingForce = %q after clearing", got)
	}

	// A dry quote must not consume the arm; the next committed quote does.
	if w := force(`{"outcome":"REFUND"}`); w.Code != 200 {
		t.Fatalf("arm REFUND: status %d", w.Code)
	}
	if _, err := rig.client.Quote(ctx, testQuoteRequest(true)); err != nil {
		t.Fatalf("dry Quote: %v", err)
	}
	if got := pending(); got != forceRefund {
		t.Fatalf("dry quote consumed the arm: pendingForce = %q", got)
	}
	resp, err := rig.client.Quote(ctx, testQuoteRequest(false))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	addr := resp.Quote.DepositAddress
	if got := pending(); got != "" {
		t.Fatalf("arm not consumed: pendingForce = %q", got)
	}

	amountIn, _ := new(big.Int).SetString(resp.Quote.AmountIn, 10)
	rig.btc.fund(addr, amountIn)
	if err := rig.client.SubmitDeposit(ctx, addr, "userdeposittx"); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}
	waitStatus(t, rig, addr, intents.StatusRefunded)

	// One-shot: a second swap runs unforced.
	rig.server.swapMtx.RLock()
	if forced := rig.server.swaps[addr].forcedOutcome(); forced != forceRefund {
		rig.server.swapMtx.RUnlock()
		t.Fatalf("swap forced = %q, want %q", forced, forceRefund)
	}
	rig.server.swapMtx.RUnlock()
	resp2, err := rig.client.Quote(ctx, testQuoteRequest(false))
	if err != nil {
		t.Fatalf("second Quote: %v", err)
	}
	rig.server.swapMtx.RLock()
	forced := rig.server.swaps[resp2.Quote.DepositAddress].forcedOutcome()
	rig.server.swapMtx.RUnlock()
	if forced != "" {
		t.Fatalf("second swap born forced: %q", forced)
	}
}

func TestForcedRefund(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	resp, err := rig.client.Quote(ctx, testQuoteRequest(false))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	addr := resp.Quote.DepositAddress

	rig.server.swapMtx.RLock()
	sw := rig.server.swaps[addr]
	rig.server.swapMtx.RUnlock()
	sw.setForced(forceRefund)

	amountIn, _ := new(big.Int).SetString(resp.Quote.AmountIn, 10)
	rig.btc.fund(addr, amountIn)
	if err := rig.client.SubmitDeposit(ctx, addr, "userdeposittx"); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}

	st := waitStatus(t, rig, addr, intents.StatusRefunded)
	refundFee, _ := new(big.Int).SetString(resp.Quote.RefundFee, 10)
	wantRefund := new(big.Int).Sub(amountIn, refundFee)
	if st.SwapDetails.RefundedAmount != wantRefund.String() {
		t.Fatalf("refundedAmount = %s, want %s", st.SwapDetails.RefundedAmount, wantRefund)
	}
	if st.SwapDetails.RefundReason == "" {
		t.Fatal("no refund reason")
	}
	if got := rig.btc.sentTo("bc1qrefund"); got == nil || got.Cmp(wantRefund) != 0 {
		t.Fatalf("refund payout = %v, want %s", got, wantRefund)
	}
	if len(st.SwapDetails.OriginChainTxs) < 2 {
		t.Fatalf("origin txs = %+v, want deposit + refund", st.SwapDetails.OriginChainTxs)
	}
	// No destination payout on refund.
	if got := rig.near.sentTo("nearrecipient.near"); got != nil {
		t.Fatalf("unexpected destination payout %s", got)
	}
}

func TestLateDepositRefund(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	req := testQuoteRequest(false)
	req.Deadline = testDeadline(-time.Minute) // already expired
	resp, err := rig.client.Quote(ctx, req)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	addr := resp.Quote.DepositAddress
	amountIn, _ := new(big.Int).SetString(resp.Quote.AmountIn, 10)
	rig.btc.fund(addr, amountIn)
	if err := rig.client.SubmitDeposit(ctx, addr, "latetx"); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}
	st := waitStatus(t, rig, addr, intents.StatusRefunded)
	if st.SwapDetails.RefundReason == "" {
		t.Fatal("no refund reason")
	}
}

func TestIncompleteDeposit(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	resp, err := rig.client.Quote(ctx, testQuoteRequest(false))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	addr := resp.Quote.DepositAddress
	amountIn, _ := new(big.Int).SetString(resp.Quote.AmountIn, 10)
	short := new(big.Int).Quo(amountIn, big.NewInt(2))
	rig.btc.fund(addr, short)
	if err := rig.client.SubmitDeposit(ctx, addr, "shorttx"); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}
	waitStatus(t, rig, addr, intents.StatusIncompleteDeposit)

	// Completing the deposit converts it to a success.
	rig.btc.fund(addr, amountIn)
	if err := rig.client.SubmitDeposit(ctx, addr, "resttx"); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}
	waitStatus(t, rig, addr, intents.StatusSuccess)
}

const testUSDCContract = "0xDb7d6ab1f17c6b31909aE466702703dAEf9269cf"

// newTokenRig builds a rig whose eth driver reports a deployed USDC test
// token, alongside a native btc driver.
func newTokenRig(t *testing.T) (*testRig, *fakeTokenDriver, string) {
	t.Helper()
	usdc := &driverToken{
		symbol: "USDC", decimals: 6, contract: testUSDCContract,
		sendScript: "sendUSDC", walletBipID: 1966, fallbackMicroUSD: 1_000_000,
	}
	eth := newFakeTokenDriver("eth", []*driverToken{usdc})
	btc := newFakeDriver("btc")
	s, err := New(&Config{
		Drivers:         map[string]Driver{"btc": btc, "eth": eth},
		ProcessingDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.Run(ctx)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	c, err := intents.NewClient(&intents.Config{
		BaseURL:    srv.URL,
		SigningKey: s.PubKey(),
	}, testLogger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	usdcID := "nep141:eth-" + strings.ToLower(testUSDCContract) + ".omft.near"
	return &testRig{server: s, btc: btc, client: c}, eth, usdcID
}

// TestTokenCatalogAndQuote verifies a harness token is published with its
// contract address, resolves as a non-native through the wallet's catalog
// logic, and prices deterministically (1 BTC at $60,000 -> 60,000 USDC at
// $1, minus 30 bps fees with the default 20 bps appFee absent here: 10 bps
// protocol fee only).
func TestTokenCatalogAndQuote(t *testing.T) {
	rig, _, usdcID := newTokenRig(t)
	ctx := context.Background()

	cat, err := rig.client.Catalog(ctx)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	entry := cat.FindContract(60, testUSDCContract)
	if entry == nil || entry.AssetID != usdcID {
		t.Fatalf("USDC not found by contract: %+v", entry)
	}
	if chainID, native, ok := cat.AssetChain(usdcID); !ok || native || chainID != 60 {
		t.Fatalf("AssetChain(%s) = %d %v %v", usdcID, chainID, native, ok)
	}

	req := testQuoteRequest(true)
	req.DestinationAsset = usdcID
	resp, err := rig.client.Quote(ctx, req)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	// 1 BTC * $60,000 -> USDC(6 dec) = 60000e6, minus 10 bps protocol fee.
	if want := "59940000000"; resp.Quote.AmountOut != want {
		t.Fatalf("amountOut = %s, want %s", resp.Quote.AmountOut, want)
	}
}

// TestTokenLifecycle swaps token->native: a USDC deposit is detected via
// the token path and the BTC payout lands.
func TestTokenLifecycle(t *testing.T) {
	rig, eth, usdcID := newTokenRig(t)
	ctx := context.Background()

	req := testQuoteRequest(false)
	req.OriginAsset = usdcID
	req.DestinationAsset = "nep141:btc.omft.near"
	req.Amount = "60000000000" // 60,000 USDC
	req.RefundTo = "0xrefund"
	req.Recipient = "bc1qtokenrecipient"
	resp, err := rig.client.Quote(ctx, req)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	addr := resp.Quote.DepositAddress
	if addr == "" {
		t.Fatal("no deposit address")
	}

	amountIn, _ := new(big.Int).SetString(resp.Quote.AmountIn, 10)
	eth.fundToken(testUSDCContract, addr, amountIn)
	if err := rig.client.SubmitDeposit(ctx, addr, "tokendeposittx"); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}

	st := waitStatus(t, rig, addr, intents.StatusSuccess)
	wantOut, _ := new(big.Int).SetString(resp.Quote.AmountOut, 10)
	if got := rig.btc.sentTo("bc1qtokenrecipient"); got == nil || got.Cmp(wantOut) != 0 {
		t.Fatalf("btc payout = %v, want %s", got, wantOut)
	}
	if st.SwapDetails.AmountOut != resp.Quote.AmountOut {
		t.Fatalf("realized amountOut = %s, want %s", st.SwapDetails.AmountOut, resp.Quote.AmountOut)
	}
}

// TestTokenRefund verifies the refund path returns tokens via SendToken.
func TestTokenRefund(t *testing.T) {
	rig, eth, usdcID := newTokenRig(t)
	ctx := context.Background()

	req := testQuoteRequest(false)
	req.OriginAsset = usdcID
	req.DestinationAsset = "nep141:btc.omft.near"
	req.Amount = "60000000000"
	req.RefundTo = "0xrefund"
	req.Recipient = "bc1qtokenrecipient"
	resp, err := rig.client.Quote(ctx, req)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	addr := resp.Quote.DepositAddress

	rig.server.swapMtx.RLock()
	rig.server.swaps[addr].setForced(forceRefund)
	rig.server.swapMtx.RUnlock()

	amountIn, _ := new(big.Int).SetString(resp.Quote.AmountIn, 10)
	eth.fundToken(testUSDCContract, addr, amountIn)
	if err := rig.client.SubmitDeposit(ctx, addr, "tokendeposittx"); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}

	waitStatus(t, rig, addr, intents.StatusRefunded)
	refundFee, _ := new(big.Int).SetString(resp.Quote.RefundFee, 10)
	wantRefund := new(big.Int).Sub(amountIn, refundFee)
	eth.mtx.Lock()
	got := eth.tokenSends["0xrefund"]
	eth.mtx.Unlock()
	if got == nil || got.Cmp(wantRefund) != 0 {
		t.Fatalf("token refund = %v, want %s", got, wantRefund)
	}
}

func TestStatusNotFound(t *testing.T) {
	rig := newTestRig(t)
	_, err := rig.client.Status(context.Background(), "nosuchaddress")
	if !errors.Is(err, intents.ErrSwapNotFound) {
		t.Fatalf("err = %v, want ErrSwapNotFound", err)
	}
}

func TestQuoteRejections(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	for name, mut := range map[string]func(*intents.QuoteRequest){
		"bad swap type":    func(r *intents.QuoteRequest) { r.SwapType = intents.SwapTypeFlexInput },
		"unknown origin":   func(r *intents.QuoteRequest) { r.OriginAsset = "nep141:xtz.omft.near" },
		"same asset":       func(r *intents.QuoteRequest) { r.DestinationAsset = r.OriginAsset },
		"bad amount":       func(r *intents.QuoteRequest) { r.Amount = "zero" },
		"missing deadline": func(r *intents.QuoteRequest) { r.Deadline = "" },
	} {
		req := testQuoteRequest(true)
		mut(req)
		if _, err := rig.client.Quote(ctx, req); err == nil {
			t.Fatalf("%s: quote unexpectedly accepted", name)
		}
	}
}
