// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build xmr

package xmr

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The monero_c shim this wallet links against wraps no fee-estimation API: the
// shipped libmonero_wallet2_api_c.so exports fee accessors only on already
// constructed objects (PendingTransaction_fee, TransactionInfo_fee,
// UnsignedTransaction_fee), and neither wallet2_api's estimateTransactionFee
// nor any daemon-RPC passthrough. Rather than construct a throwaway
// transaction to learn a fee — which cannot be freed, see StandardSendFee —
// this talks to monerod directly over JSON-RPC, the same way the account
// assets in this repo hand-roll their own clients.

// jsonRPCPath is monerod's JSON-RPC endpoint.
const jsonRPCPath = "/json_rpc"

// digestAttempts bounds the challenge-response cycle. Two is enough for the
// ordinary case of "no challenge yet, then authenticate on the same
// connection"; the third covers a retry that lands on a different pooled
// connection carrying a nonce the daemon will not accept.
const digestAttempts = 3

// drainAndClose consumes the remainder of a response body before closing it.
//
// This is load-bearing, not hygiene. monerod scopes a digest nonce to the TCP
// connection that issued it, so the authenticated retry must travel over the
// same connection as the 401 that supplied the nonce. Go only returns a
// connection to the idle pool when its body is read to EOF and closed; closing
// early makes the transport discard the connection, and the retry then opens a
// fresh one whose nonce the daemon rejects. Verified against monerod v0.18.4.6,
// where even curl fails digest auth if forced to use Connection: close.
//
// This matters on the success path too, not just the 401 retries: a JSON
// decoder stops as soon as it has read a complete value and is not guaranteed
// to have driven the body to EOF, so the connection carrying the nonce would
// be discarded even when nothing went wrong.
//
// The body here is a small error page or an already-decoded result, but it is
// bounded anyway so a misbehaving daemon cannot make this read forever.
func drainAndClose(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) //nolint:errcheck // Best effort.
	resp.Body.Close()
}

// maxDrainBytes caps how much of an unwanted response body is read back.
const maxDrainBytes = 1 << 16

// maxRespBytes caps how much of a response body is decoded. The daemon may be
// a public third-party node, so its response length is untrusted input: an
// unbounded decode buffers whatever it sends for as long as feeRequestTimeout
// allows. The replies this client asks for run to a few hundred bytes, so this
// is orders of magnitude of headroom.
const maxRespBytes = 1 << 20

// feeEstimate is the result of monerod's get_fee_estimate.
type feeEstimate struct {
	// Fee is the dynamic base fee in atoms per weight unit, equivalent to
	// Fees[0].
	Fee uint64 `json:"fee"`
	// Fees holds the per-priority base fee, one entry each for priorities
	// low, medium, high and last. See feeForPriority.
	Fees []uint64 `json:"fees"`
	// QuantizationMask is the granularity a final fee is rounded up to.
	QuantizationMask uint64 `json:"quantization_mask"`
	Status           string `json:"status"`
	Untrusted        bool   `json:"untrusted"`
}

// priorityIndex maps a Monero priority onto an index into a per-priority fee
// table of n entries.
//
// These tables carry four entries, for priorities Low(1) through Last(4).
// Default(0) is not represented because wallet2 resolves it to Low before
// computing a multiplier, so it shares the first entry. Out-of-range
// priorities clamp rather than panic.
func priorityIndex(p, n int) int {
	idx := p - 1
	if idx < 0 {
		idx = 0 // Default resolves to Low.
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}

// feeForPriority returns the base fee for the given priority, in atoms per
// weight unit.
func (f *feeEstimate) feeForPriority(p int) uint64 {
	if len(f.Fees) == 0 {
		return f.Fee
	}
	return f.Fees[priorityIndex(p, len(f.Fees))]
}

// quantize rounds a fee up to the next multiple of the quantization mask, as
// the daemon requires. A fee close enough to the top of the range that the
// rounding would wrap is returned unrounded; getFeeEstimate keeps real inputs
// far away from that, so this only stops a wrap from being worse than a
// missed rounding.
func (f *feeEstimate) quantize(fee uint64) uint64 {
	mask := f.QuantizationMask
	if mask <= 1 {
		return fee
	}
	if fee > math.MaxUint64-(mask-1) {
		return fee
	}
	return (fee + mask - 1) / mask * mask
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// daemonRPC is a minimal JSON-RPC client for monerod. It exists only to ask
// the daemon questions monero_c will not relay.
type daemonRPC struct {
	url string
	// path is the request-URI of url. Digest hashes the URI, so it has to be
	// the path actually sent rather than jsonRPCPath: a daemon reached through
	// a reverse proxy on a path prefix is requested at /prefix/json_rpc, and
	// signing /json_rpc instead fails auth with a message that blames the
	// credentials.
	path       string
	user, pass string
	cl         *http.Client

	// authMtx guards the cached digest challenge. monerod authenticates with
	// HTTP digest and rejects basic auth outright, so a challenge is cached
	// and reused with an incrementing nonce count rather than paying an extra
	// round trip per request.
	authMtx   sync.Mutex
	challenge map[string]string
	nonceCnt  uint64
}

// newDaemonRPC builds a client for the given daemon address. The address may
// omit a scheme, in which case http is assumed, matching monerod's default.
func newDaemonRPC(addr, user, pass string, cl *http.Client) *daemonRPC {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	rawURL := strings.TrimSuffix(addr, "/") + jsonRPCPath

	// Recover the request-URI for the digest hash. A bare monerod serves at
	// the root, so this is jsonRPCPath, but an address carrying a path prefix
	// is not. If the address will not parse, leave the URL alone and let the
	// request itself report the problem.
	path := jsonRPCPath
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		path = u.RequestURI()
	}

	return &daemonRPC{
		url:  rawURL,
		path: path,
		user: user,
		pass: pass,
		cl:   cl,
	}
}

// maxBaseFee is the largest per-weight-unit base fee this wallet will accept
// from a daemon, in atoms. The default mainnet daemon is a public third-party
// node, so its numbers are untrusted input: they are multiplied by a
// transaction weight and quantized, and an absurd value would overflow those
// arithmetic and report a fee that is not merely wrong but small.
//
// Mainnet in July 2026 quotes 20000 at default priority and 4000000 at Last,
// so this leaves better than two orders of magnitude of headroom above the
// most expensive priority. Rejecting the estimate outright is the right
// response: a daemon quoting a fee this far out is not one to price against,
// and the caller falls back to the last fee actually paid.
const maxBaseFee = 1_000_000_000

// getFeeEstimate asks the daemon for the current dynamic base fee.
func (d *daemonRPC) getFeeEstimate(ctx context.Context) (*feeEstimate, error) {
	var est feeEstimate
	if err := d.call(ctx, "get_fee_estimate", &est); err != nil {
		return nil, err
	}
	if err := validateFeeEstimate(&est); err != nil {
		return nil, err
	}
	return &est, nil
}

// validateFeeEstimate rejects an estimate this wallet should not price
// against.
func validateFeeEstimate(est *feeEstimate) error {
	if est.Status != "OK" {
		return fmt.Errorf("get_fee_estimate status %q", est.Status)
	}
	if est.Fee == 0 && len(est.Fees) == 0 {
		return fmt.Errorf("get_fee_estimate returned no fee")
	}
	for _, fee := range append([]uint64{est.Fee}, est.Fees...) {
		if fee > maxBaseFee {
			return fmt.Errorf("daemon quoted a base fee of %d atoms per weight unit, above the %d sanity ceiling",
				fee, maxBaseFee)
		}
	}
	// The mask multiplies into the reported fee the same way the base fee
	// does, so it needs the same bound.
	if est.QuantizationMask > maxBaseFee {
		return fmt.Errorf("daemon quoted a quantization mask of %d, above the %d sanity ceiling",
			est.QuantizationMask, maxBaseFee)
	}
	return nil
}

// call performs a JSON-RPC request, retrying once with digest credentials if
// the daemon challenges.
func (d *daemonRPC) call(ctx context.Context, method string, result any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "0",
		"method":  method,
	})
	if err != nil {
		return err
	}

	// A cached challenge can go stale, and the first request of a session has
	// none at all. Either way the daemon replies 401 with a fresh challenge:
	// adopt it and retry. More than one retry is needed because monerod binds
	// the nonce to the TCP connection (see drainAndClose), so a retry that
	// happens to land on a different pooled connection is rejected and must
	// re-challenge on that one.
	var resp *http.Response
	for attempt := 0; attempt < digestAttempts; attempt++ {
		if resp, err = d.do(ctx, body); err != nil {
			return err
		}
		if resp.StatusCode != http.StatusUnauthorized {
			break
		}
		if d.user == "" && d.pass == "" {
			drainAndClose(resp)
			return fmt.Errorf("daemon requires authentication but no credentials are configured")
		}
		d.adoptChallenge(resp)
		drainAndClose(resp)
		resp = nil
	}
	if resp == nil {
		return fmt.Errorf("daemon rejected credentials after %d attempts", digestAttempts)
	}
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %s", resp.Status)
	}

	var rpcResp rpcResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRespBytes)).Decode(&rpcResp); err != nil {
		return fmt.Errorf("failed to decode %s response: %w", method, err)
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("daemon error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return json.Unmarshal(rpcResp.Result, result)
}

// do issues a single request, attaching a digest header when a challenge is
// known.
func (d *daemonRPC) do(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	if auth := d.authHeader(); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := d.cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon request failed: %w", err)
	}
	return resp, nil
}

// adoptChallenge records the digest challenge from a 401 response. monerod
// offers both MD5 and MD5-sess; the plain MD5 challenge is preferred.
func (d *daemonRPC) adoptChallenge(resp *http.Response) {
	var chosen map[string]string
	for _, hdr := range resp.Header.Values("Www-Authenticate") {
		if !strings.HasPrefix(strings.TrimSpace(hdr), "Digest ") {
			continue
		}
		params := parseDigestChallenge(hdr)
		if params["nonce"] == "" {
			continue
		}
		alg := strings.ToUpper(params["algorithm"])
		if alg == "" || alg == "MD5" {
			chosen = params
			break
		}
		if chosen == nil {
			chosen = params // Fall back to whatever was offered.
		}
	}
	if chosen == nil {
		return
	}

	d.authMtx.Lock()
	defer d.authMtx.Unlock()
	d.challenge = chosen
	d.nonceCnt = 0
}

// parseDigestChallenge splits a Digest challenge into its parameters.
func parseDigestChallenge(hdr string) map[string]string {
	params := make(map[string]string)
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(hdr), "Digest"))
	for _, part := range splitDigestParams(rest) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return params
}

// splitDigestParams splits on commas that are not inside a quoted value.
func splitDigestParams(s string) []string {
	var parts []string
	var cur strings.Builder
	var inQuote bool
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// authHeader builds an RFC 7616 digest Authorization header from the cached
// challenge, or returns empty if no challenge is known yet.
//
// MD5 is not a choice made here: monerod's digest implementation offers only
// MD5 and MD5-sess, so the algorithm is dictated by the daemon.
func (d *daemonRPC) authHeader() string {
	d.authMtx.Lock()
	defer d.authMtx.Unlock()

	if d.challenge == nil {
		return ""
	}
	realm, nonce := d.challenge["realm"], d.challenge["nonce"]
	opaque, qop := d.challenge["opaque"], d.challenge["qop"]
	sess := strings.EqualFold(d.challenge["algorithm"], "MD5-sess")

	d.nonceCnt++
	nc := fmt.Sprintf("%08x", d.nonceCnt)
	cnonce, err := randomHex(8)
	if err != nil {
		return ""
	}

	ha1 := md5Hex(d.user + ":" + realm + ":" + d.pass)
	if sess {
		ha1 = md5Hex(ha1 + ":" + nonce + ":" + cnonce)
	}
	ha2 := md5Hex(http.MethodPost + ":" + d.path)

	// qop is advertised as a comma-separated list; monerod offers only "auth".
	useQop := strings.Contains(qop, "auth")
	var response string
	if useQop {
		response = md5Hex(strings.Join([]string{ha1, nonce, nc, cnonce, "auth", ha2}, ":"))
	} else {
		response = md5Hex(ha1 + ":" + nonce + ":" + ha2)
	}

	parts := []string{
		fmt.Sprintf("username=%q", d.user),
		fmt.Sprintf("realm=%q", realm),
		fmt.Sprintf("nonce=%q", nonce),
		fmt.Sprintf("uri=%q", d.path),
		fmt.Sprintf("response=%q", response),
	}
	if alg := d.challenge["algorithm"]; alg != "" {
		parts = append(parts, "algorithm="+alg)
	}
	if useQop {
		parts = append(parts, "qop=auth", "nc="+nc, fmt.Sprintf("cnonce=%q", cnonce))
	}
	if opaque != "" {
		parts = append(parts, fmt.Sprintf("opaque=%q", opaque))
	}
	return "Digest " + strings.Join(parts, ", ")
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // Dictated by monerod's digest auth.
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// cachedFeeEstimate holds a fee estimate and the time it was fetched, stored
// as a unit so readers never see a rate paired with the wrong timestamp.
type cachedFeeEstimate struct {
	est     *feeEstimate
	fetched int64 // Unix nanoseconds.
}

// feeEstimateCache caches the daemon's fee estimate. The base fee moves with
// median block weight, so it is stable across the ~2 minute block interval and
// does not warrant a request per lookup.
type feeEstimateCache struct {
	v atomic.Value // *cachedFeeEstimate
	// lastFail is the Unix nanosecond time of the last failed fetch, or zero.
	// Without it an unreachable daemon would be retried on every lookup, and
	// these lookups sit behind a debounced send form: each attempt would stall
	// a caller for the request timeout.
	lastFail atomic.Int64
}

func (c *feeEstimateCache) get() *cachedFeeEstimate {
	cached, _ := c.v.Load().(*cachedFeeEstimate)
	return cached
}

func (c *feeEstimateCache) set(est *feeEstimate, nowNano int64) {
	c.v.Store(&cachedFeeEstimate{est: est, fetched: nowNano})
	c.lastFail.Store(0)
}

// fail records a failed fetch.
func (c *feeEstimateCache) fail(nowNano int64) {
	c.lastFail.Store(nowNano)
}

// coolingDown reports whether a fetch failed too recently to try again.
func (c *feeEstimateCache) coolingDown(now time.Time, retryAfter time.Duration) bool {
	last := c.lastFail.Load()
	return last != 0 && now.Sub(time.Unix(0, last)) < retryAfter
}
