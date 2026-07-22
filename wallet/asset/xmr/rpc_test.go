// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build xmr

package xmr

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/xmr/cxmr"
)

func TestParseDigestChallenge(t *testing.T) {
	// A verbatim challenge from monerod v0.18.4.6.
	const hdr = `Digest qop="auth",algorithm=MD5,realm="monero-rpc",nonce="shueNdIeXXS9bRFBjrt2qQ==",stale=false`

	params := parseDigestChallenge(hdr)
	for k, want := range map[string]string{
		"qop":       "auth",
		"algorithm": "MD5",
		"realm":     "monero-rpc",
		// The nonce is base64 and contains '=' padding, which must survive the
		// key/value split intact.
		"nonce": "shueNdIeXXS9bRFBjrt2qQ==",
		"stale": "false",
	} {
		if got := params[k]; got != want {
			t.Errorf("param %q = %q, want %q", k, got, want)
		}
	}
}

func TestSplitDigestParamsQuotedComma(t *testing.T) {
	// A comma inside a quoted value must not split the parameter.
	parts := splitDigestParams(`qop="auth,auth-int",realm="a,b"`)
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2: %q", len(parts), parts)
	}
	params := parseDigestChallenge("Digest " + `qop="auth,auth-int",realm="a,b"`)
	if params["realm"] != "a,b" {
		t.Errorf("realm = %q, want %q", params["realm"], "a,b")
	}
}

func TestFeeForPriority(t *testing.T) {
	// Values as returned by the mainnet daemon in July 2026. The fees array is
	// the base fee times monerod's per-priority multipliers, 1x, 4x, 16x and
	// 200x, so the entries are exact multiples of the first.
	est := &feeEstimate{
		Fee:              20000,
		Fees:             []uint64{20000, 80000, 320000, 4000000},
		QuantizationMask: 10000,
	}

	// The fees array covers priorities Low(1) through Last(4). Default(0)
	// resolves to Low, so it shares the first entry.
	for priority, want := range map[int]uint64{
		0: 20000,
		1: 20000,
		2: 80000,
		3: 320000,
		4: 4000000,
	} {
		if got := est.feeForPriority(priority); got != want {
			t.Errorf("feeForPriority(%d) = %d, want %d", priority, got, want)
		}
	}

	// Out of range must clamp rather than panic.
	if got := est.feeForPriority(99); got != 4000000 {
		t.Errorf("feeForPriority(99) = %d, want clamp to 4000000", got)
	}

	// A daemon that omits the array falls back to the scalar.
	noArray := &feeEstimate{Fee: 555}
	if got := noArray.feeForPriority(3); got != 555 {
		t.Errorf("feeForPriority with no array = %d, want 555", got)
	}
}

// TestFeeEstimateCeiling checks getFeeEstimate's validation of the numbers a
// daemon reports, which are untrusted: the mainnet default is a public
// third-party node.
func TestFeeEstimateCeiling(t *testing.T) {
	for name, est := range map[string]*feeEstimate{
		"scalar fee":        {Status: "OK", Fee: maxBaseFee + 1},
		"per-priority fee":  {Status: "OK", Fee: 20000, Fees: []uint64{20000, maxBaseFee + 1}},
		"quantization mask": {Status: "OK", Fee: 20000, QuantizationMask: maxBaseFee + 1},
	} {
		if err := validateFeeEstimate(est); err == nil {
			t.Errorf("%s above the ceiling was accepted", name)
		}
	}

	// A realistic mainnet estimate must still pass.
	ok := &feeEstimate{
		Status:           "OK",
		Fee:              20000,
		Fees:             []uint64{20000, 80000, 320000, 4000000},
		QuantizationMask: 10000,
	}
	if err := validateFeeEstimate(ok); err != nil {
		t.Errorf("a real mainnet estimate was rejected: %v", err)
	}
}

func TestFallbackFeeScalesWithPriority(t *testing.T) {
	// fallbackSendFee is quoted at the default priority, so a wallet
	// configured higher has to scale it. Unscaled, a cold start against an
	// unreachable daemon would report a fee up to 200x below what the send
	// costs. The multipliers match the fees array in TestFeeForPriority.
	for priority, want := range map[cxmr.Priority]uint64{
		cxmr.PriorityDefault: fallbackSendFee,
		cxmr.PriorityLow:     fallbackSendFee,
		cxmr.PriorityMedium:  fallbackSendFee * 4,
		cxmr.PriorityHigh:    fallbackSendFee * 16,
		cxmr.PriorityLast:    fallbackSendFee * 200,
	} {
		w := &ExchangeWallet{feePriority: priority}
		if got := w.fallbackFee(); got != want {
			t.Errorf("fallbackFee at priority %d = %d, want %d", priority, got, want)
		}
	}
}

func TestQuantize(t *testing.T) {
	est := &feeEstimate{QuantizationMask: 10000}
	for _, tt := range []struct{ in, want uint64 }{
		{0, 0},
		{1, 10000},
		{10000, 10000},
		{10001, 20000},
	} {
		if got := est.quantize(tt.in); got != tt.want {
			t.Errorf("quantize(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}

	// A mask of 0 or 1 must not divide by zero or alter the fee.
	for _, mask := range []uint64{0, 1} {
		est := &feeEstimate{QuantizationMask: mask}
		if got := est.quantize(1234); got != 1234 {
			t.Errorf("quantize(1234) with mask %d = %d, want 1234", mask, got)
		}
	}

	// Rounding a fee near the top of the range must not wrap to a tiny one.
	// getFeeEstimate's ceiling keeps real inputs far from here; this only
	// pins the behavior if one ever arrives.
	big := &feeEstimate{QuantizationMask: 10000}
	if got := big.quantize(math.MaxUint64 - 5); got != math.MaxUint64-5 {
		t.Errorf("quantize near MaxUint64 = %d, want it returned unrounded", got)
	}
}

func TestNewDaemonRPCURL(t *testing.T) {
	for _, tt := range []struct{ addr, wantURL, wantPath string }{
		{"http://127.0.0.1:18081", "http://127.0.0.1:18081/json_rpc", "/json_rpc"},
		{"http://127.0.0.1:18081/", "http://127.0.0.1:18081/json_rpc", "/json_rpc"},
		{"127.0.0.1:18081", "http://127.0.0.1:18081/json_rpc", "/json_rpc"},
		{"https://node.example.com:18089", "https://node.example.com:18089/json_rpc", "/json_rpc"},
		// A daemon behind a reverse proxy on a path prefix is requested at
		// the prefixed path, and digest hashes the URI, so the path has to
		// follow the URL rather than stay at the bare /json_rpc.
		{"https://node.example.com/monero", "https://node.example.com/monero/json_rpc", "/monero/json_rpc"},
	} {
		d := newDaemonRPC(tt.addr, "", "", nil)
		if d.url != tt.wantURL {
			t.Errorf("newDaemonRPC(%q).url = %q, want %q", tt.addr, d.url, tt.wantURL)
		}
		if d.path != tt.wantPath {
			t.Errorf("newDaemonRPC(%q).path = %q, want %q", tt.addr, d.path, tt.wantPath)
		}
	}
}

// TestStandardSendFeeRejectsAbsurdRate checks the ceiling on the rate side.
// The default mainnet daemon is a public third-party node, so an absurd rate
// is reachable input, and multiplying it by standardTxWeight would overflow
// into a fee that is not merely wrong but small.
func TestStandardSendFeeRejectsAbsurdRate(t *testing.T) {
	w := &ExchangeWallet{log: util.NewLogger("T", util.LevelError, os.Stdout)}
	w.observedFee.Store(42)
	// Park the cache in its cooldown so feeEstimate answers nil from memory
	// rather than reaching for the nil feeRPC.
	w.feeCache.fail(time.Now().UnixNano())

	// A rate whose product with standardTxWeight wraps must not be used.
	if got := w.StandardSendFee(math.MaxUint64 / 2); got != 42 {
		t.Errorf("StandardSendFee with a wrapping rate = %d, want the observed fee 42", got)
	}
	if got := w.StandardSendFee(maxBaseFee + 1); got != 42 {
		t.Errorf("StandardSendFee above the ceiling = %d, want the observed fee 42", got)
	}
	// A rate at the ceiling is still priced, so the bound is not accidentally
	// rejecting the range it is meant to admit.
	if got := w.StandardSendFee(maxBaseFee); got != maxBaseFee*standardTxWeight {
		t.Errorf("StandardSendFee at the ceiling = %d, want %d", got, uint64(maxBaseFee)*standardTxWeight)
	}
}

// TestFlushPendingFee covers the handoff commitTx uses to keep the meta-file
// write out of the walletMtx critical section.
func TestFlushPendingFee(t *testing.T) {
	dir := t.TempDir()
	w := &ExchangeWallet{
		dataDir:     dir,
		log:         util.NewLogger("T", util.LevelError, os.Stdout),
		feePriority: cxmr.PriorityMedium,
	}

	// With nothing pending there is nothing to write, so a send that never
	// reached a broadcast must not touch the file at all.
	w.flushPendingFee()
	meta, err := loadWalletMeta(dir)
	if err != nil {
		t.Fatalf("loadWalletMeta: %v", err)
	}
	if meta != nil {
		t.Fatalf("an empty flush wrote a meta file: %+v", meta)
	}

	w.pendingFee.Store(12345)
	w.flushPendingFee()

	meta, err = loadWalletMeta(dir)
	if err != nil {
		t.Fatalf("loadWalletMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("no meta file written")
	}
	if meta.LastFee != 12345 {
		t.Errorf("LastFee = %d, want 12345", meta.LastFee)
	}
	if meta.LastFeePriority != int(cxmr.PriorityMedium) {
		t.Errorf("LastFeePriority = %d, want %d", meta.LastFeePriority, cxmr.PriorityMedium)
	}

	// The fee is taken rather than read, so a broadcast is persisted once no
	// matter how many flushes follow it.
	if pending := w.pendingFee.Load(); pending != 0 {
		t.Errorf("pendingFee = %d after a flush, want it cleared", pending)
	}
}

func TestFeeCacheCooldown(t *testing.T) {
	var c feeEstimateCache
	now := time.Now()

	// A cache that has never failed must not be cooling down, or the very
	// first lookup would be skipped.
	if c.coolingDown(now, time.Minute) {
		t.Error("fresh cache reports cooling down")
	}

	c.fail(now.UnixNano())
	if !c.coolingDown(now, time.Minute) {
		t.Error("cache does not report cooling down right after a failure")
	}
	if c.coolingDown(now.Add(2*time.Minute), time.Minute) {
		t.Error("cache still cooling down after the interval elapsed")
	}

	// A success must clear the cooldown so the next lookup is not skipped
	// because of a failure that has since been superseded.
	c.set(&feeEstimate{Fee: 1}, now.UnixNano())
	if c.coolingDown(now, time.Minute) {
		t.Error("cooldown survived a successful fetch")
	}
}

// singleConnClient mirrors the transport Connect builds: one pooled
// connection, because monerod scopes its digest nonce to the connection that
// issued it. See drainAndClose.
func singleConnClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        1,
			MaxIdleConnsPerHost: 1,
		},
	}
}

const feeEstimateJSON = `{"jsonrpc":"2.0","id":"0","result":` +
	`{"fee":20000,"fees":[20000,80000,320000,4000000],"quantization_mask":10000,"status":"OK"}}`

// TestCallReusesConnection checks that a response this client does not want
// still leaves the connection reusable. Every exit path has to drain the body:
// Go only returns a connection to the idle pool once the body is read to EOF
// and closed, and a discarded connection costs the daemon-scoped digest nonce
// with it, so the next call has to re-challenge from scratch.
func TestCallReusesConnection(t *testing.T) {
	var mtx sync.Mutex
	var addrs []string
	var failing bool

	srv := httptest.NewServer(http.HandlerFunc(func(wr http.ResponseWriter, r *http.Request) {
		mtx.Lock()
		addrs = append(addrs, r.RemoteAddr)
		fail := failing
		mtx.Unlock()

		if fail {
			// A daemon error carries a body, same as the 401 challenge does.
			wr.WriteHeader(http.StatusInternalServerError)
			wr.Write(bytes.Repeat([]byte("x"), 4096)) //nolint:errcheck // Test server.
			return
		}
		wr.Write([]byte(feeEstimateJSON)) //nolint:errcheck // Test server.
	}))
	defer srv.Close()

	d := newDaemonRPC(srv.URL, "", "", singleConnClient())
	ctx := context.Background()

	if _, err := d.getFeeEstimate(ctx); err != nil {
		t.Fatalf("first getFeeEstimate: %v", err)
	}

	mtx.Lock()
	failing = true
	mtx.Unlock()
	if _, err := d.getFeeEstimate(ctx); err == nil {
		t.Fatal("a 500 response was not reported as an error")
	}

	mtx.Lock()
	failing = false
	mtx.Unlock()
	if _, err := d.getFeeEstimate(ctx); err != nil {
		t.Fatalf("getFeeEstimate after a 500: %v", err)
	}

	mtx.Lock()
	defer mtx.Unlock()
	if len(addrs) != 3 {
		t.Fatalf("got %d requests, want 3", len(addrs))
	}
	for i, addr := range addrs {
		if addr != addrs[0] {
			t.Errorf("request %d came from %s, want the pooled connection %s", i, addr, addrs[0])
		}
	}
}

// TestCallBoundsResponseBody checks that a daemon cannot make this client
// buffer an unbounded response. The mainnet default is a public third-party
// node, so the length of what it sends is untrusted input.
func TestCallBoundsResponseBody(t *testing.T) {
	// backstop keeps a client that reads without limit from running until the
	// deadline instead of failing the test. It is far above what a bounded
	// client can pull, and reaching it is itself the failure.
	const backstop = 64 << 20

	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(wr http.ResponseWriter, r *http.Request) {
		// A reply that never terminates: valid JSON so far, then filler for as
		// long as the client keeps reading.
		wr.Write([]byte(`{"jsonrpc":"2.0","id":"0","result":{"status":"`)) //nolint:errcheck // Test server.
		chunk := bytes.Repeat([]byte("A"), 1<<16)
		for served.Load() < backstop {
			n, err := wr.Write(chunk)
			served.Add(int64(n))
			if err != nil {
				return // Client hung up, which is the point of the test.
			}
		}
	}))
	defer srv.Close()

	d := newDaemonRPC(srv.URL, "", "", singleConnClient())

	// Comfortably longer than reading maxRespBytes off a loopback socket, so a
	// failure here is the decode giving up rather than the deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := d.getFeeEstimate(ctx); err == nil {
		t.Fatal("an oversized response was accepted")
	} else if ctx.Err() != nil {
		t.Fatalf("the request ran out the clock rather than the byte limit: %v", err)
	}

	// The client stops at maxRespBytes plus the drain, but the server can run
	// ahead of it by whatever the socket buffers hold, so this checks the order
	// of magnitude rather than an exact figure.
	if got := served.Load(); got > 8*maxRespBytes {
		t.Errorf("the daemon pushed %d bytes before the client gave up, want it bounded near %d",
			got, maxRespBytes)
	}
}

// TestLiveFeeEstimate exercises the client against a real monerod. Set
// XMR_DAEMON to the daemon address, and XMR_DAEMON_USER / XMR_DAEMON_PASS if
// it was started with --rpc-login.
//
//	monerod --regtest --offline --data-dir=/tmp/x --rpc-bind-port=18391
//	XMR_DAEMON=127.0.0.1:18391 go test -tags xmr -run TestLiveFeeEstimate ./wallet/asset/xmr/
func TestLiveFeeEstimate(t *testing.T) {
	addr := os.Getenv("XMR_DAEMON")
	if addr == "" {
		t.Skip("set XMR_DAEMON to run")
	}

	d := newDaemonRPC(addr, os.Getenv("XMR_DAEMON_USER"), os.Getenv("XMR_DAEMON_PASS"),
		&http.Client{Timeout: 10 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	est, err := d.getFeeEstimate(ctx)
	if err != nil {
		t.Fatalf("getFeeEstimate: %v", err)
	}
	if est.Fee == 0 {
		t.Error("fee is zero")
	}
	if len(est.Fees) != 4 {
		t.Errorf("got %d per-priority fees, want 4: %v", len(est.Fees), est.Fees)
	}
	if est.QuantizationMask == 0 {
		t.Error("quantization mask is zero")
	}
	t.Logf("fee=%d fees=%v mask=%d", est.Fee, est.Fees, est.QuantizationMask)

	// A second call must also succeed. With credentials configured this is the
	// path that reuses the cached digest challenge with an incremented nonce
	// count, which is where a broken digest implementation shows up.
	if _, err := d.getFeeEstimate(ctx); err != nil {
		t.Fatalf("second getFeeEstimate: %v", err)
	}
}
