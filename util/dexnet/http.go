// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dexnet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/decred/go-socks/socks"
)

const defaultResponseSizeLimit = 1 << 20 // 1 MiB = 1,048,576 bytes

// proxyDialTimeout bounds a dial through the proxy whose context carries no
// deadline. The socks library stops waiting on a stalled proxy only at the
// context's deadline, so without it an abandoned dial would hold its goroutine
// and socket for as long as the proxy stays silent. A context that already
// carries a deadline is left alone. A variable so tests can shorten it.
var proxyDialTimeout = 30 * time.Second

// Client is the default HTTP client used for requests.
// torproxy: the shared client; SetProxy installs the proxy transport on it
var Client = &http.Client{
	Timeout: 20 * time.Second, // 20 seconds
}

// ProxyDialContext returns a DialContext function that routes connections
// through a SOCKS5 proxy at addr (host:port). If addr is empty, nil is
// returned. Local targets (see ProxyFor) are dialed direct, since Tor cannot
// reach them, so a node on this machine or LAN keeps working. The dial returns
// as soon as ctx is done, and gives up on a proxy that never answers after
// proxyDialTimeout. The socks library only honors a context's deadline, so
// neither would hold otherwise.
func ProxyDialContext(addr string) func(ctx context.Context, network, address string) (net.Conn, error) {
	if addr == "" {
		return nil
	}
	// TorIsolation gives each connection fresh credentials, so Tor puts every
	// connection, and therefore unrelated wallets, on separate circuits.
	proxy := &socks.Proxy{Addr: addr, TorIsolation: true}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if isLocalTarget(address) && !proxyLocal.Load() {
			// torproxy: local IP literals and localhost only, which Tor rejects
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
		type result struct {
			conn net.Conn
			err  error
		}
		res := make(chan result, 1)
		go func() {
			dctx := ctx
			if _, ok := ctx.Deadline(); !ok {
				var cancel context.CancelFunc
				dctx, cancel = context.WithTimeout(ctx, proxyDialTimeout)
				defer cancel()
			}
			conn, err := proxy.DialContext(dctx, network, address)
			res <- result{conn, err}
		}()
		select {
		case r := <-res:
			// socks returns a nil *proxiedConn inside a non-nil net.Conn on
			// error, so trust err rather than a nil check on conn.
			if r.err != nil {
				return nil, r.err
			}
			return r.conn, nil
		case <-ctx.Done():
			// Close a connection the abandoned dial may still complete.
			go func() {
				if r := <-res; r.err == nil {
					r.conn.Close()
				}
			}()
			return nil, ctx.Err()
		}
	}
}

// cgnat is the carrier-grade NAT range (100.64.0.0/10), used by Tailscale.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// isLocalTarget reports whether hostAddr (a host, host:port or URL) is a
// loopback, private, link-local or unspecified IP, or localhost, which Tor
// refuses to reach. The only local hostname is localhost. Other hostnames are never local, even if they resolve to
// a private address, because resolving them locally would leak the lookup.
func isLocalTarget(hostAddr string) bool {
	host := hostAddr
	if u, err := url.Parse(hostAddr); err == nil && u.Host != "" {
		host = u.Hostname()
	} else if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	// Only the exact name: the system resolver would look up any other
	// *.localhost name, which could leak it outside Tor.
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap().WithZone("")
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

// ProxyFor returns torProxy unless hostAddr is a local target, in which case
// it returns "" so the connection goes direct. It is for clients that take a
// proxy setting rather than a dialer; see ProxyDialContext.
func ProxyFor(hostAddr, torProxy string) string {
	if torProxy == "" || isLocalTarget(hostAddr) {
		return ""
	}
	return torProxy
}

// proxyLocal makes ProxyDialContext proxy local targets too.
var proxyLocal atomic.Bool

// ProxyLocalForTest makes ProxyDialContext route loopback and private targets
// through the proxy until the test ends, so tests can use loopback listeners
// as stand-ins for remote servers. Tests only. It is process-wide, so it must
// not be used from parallel tests or subtests.
func ProxyLocalForTest(t interface {
	Helper()
	Cleanup(func())
}) {
	t.Helper()
	proxyLocal.Store(true)
	t.Cleanup(func() { proxyLocal.Store(false) })
}

// IsolationCreds returns random SOCKS credentials. Tor keeps connections with
// distinct credentials on separate circuits, for clients that dial through a
// library which takes proxy credentials rather than using ProxyDialContext.
func IsolationCreds() (user, pass string) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:8]), hex.EncodeToString(b[8:])
}

// refuseLocalRedirect stops a proxied request from being redirected to a local
// address, which would be dialed direct. Redirects between local addresses of
// a request that was local to begin with are left alone.
func refuseLocalRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if isLocalTarget(req.URL.Host) && !isLocalTarget(via[0].URL.Host) {
		return fmt.Errorf("refusing redirect from %s to local address %s",
			via[0].URL.Host, req.URL.Host)
	}
	return nil
}

// ProxyTransport returns an *http.Transport that routes all connections through
// a SOCKS5 proxy at addr (host:port).
func ProxyTransport(addr string) *http.Transport {
	// torproxy: this is the proxy transport
	return &http.Transport{
		DialContext:           ProxyDialContext(addr),
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// ProxyHTTPClient returns an *http.Client that routes all requests through a
// SOCKS5 proxy at addr (host:port).
func ProxyHTTPClient(addr string) *http.Client {
	// torproxy: this is the proxy client
	return &http.Client{
		Transport:     ProxyTransport(addr),
		CheckRedirect: refuseLocalRedirect,
		Timeout:       20 * time.Second,
	}
}

var setProxyOnce sync.Once

// SetProxy configures the global Client to route all requests through a SOCKS5
// proxy at the given address (host:port). Only the first call takes effect;
// subsequent calls are no-ops.
func SetProxy(addr string) {
	setProxyOnce.Do(func() {
		Client.Transport = ProxyTransport(addr)
		Client.CheckRedirect = refuseLocalRedirect
	})
}

// RequestOption are optional arguments to Get, Post, or Do.
type RequestOption struct {
	responseSizeLimit int64
	statusFunc        func(int)
	header            *[2]string
	errThing          any
}

// WithSizeLimit sets a size limit for a response. See defaultResponseSizeLimit
// for the default.
func WithSizeLimit(limit int64) *RequestOption {
	return &RequestOption{responseSizeLimit: limit}
}

// WithStatusFunc calls a function with the status code after the request is
// performed.
func WithStatusFunc(f func(int)) *RequestOption {
	return &RequestOption{statusFunc: f}
}

// WithRequestHeader adds a header entry to the request.
func WithRequestHeader(k, v string) *RequestOption {
	h := [2]string{k, v}
	return &RequestOption{header: &h}
}

// WithErrorParsing adds parsing of response bodies for HTTP error responses.
func WithErrorParsing(thing any) *RequestOption {
	return &RequestOption{errThing: thing}
}

// Post performs an HTTP POST request. If thing is non-nil, the response will
// be JSON-unmarshaled into thing.
func Post(ctx context.Context, uri string, thing any, body []byte, opts ...*RequestOption) error {
	var r io.Reader
	if len(body) > 0 {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uri, r)
	if err != nil {
		return fmt.Errorf("error constructing request: %w", err)
	}
	return Do(req, thing, opts...)
}

// Post performs an HTTP GET request. If thing is non-nil, the response will
// be JSON-unmarshaled into thing.
func Get(ctx context.Context, uri string, thing any, opts ...*RequestOption) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return fmt.Errorf("error constructing request: %w", err)
	}
	return Do(req, thing, opts...)
}

// Do does the request and JSON-marshals the result into thing, if non-nil.
func Do(req *http.Request, thing any, opts ...*RequestOption) error {
	var sizeLimit int64 = defaultResponseSizeLimit
	var statusFunc func(int)
	var errThing any
	for _, opt := range opts {
		switch {
		case opt.responseSizeLimit > 0:
			sizeLimit = opt.responseSizeLimit
		case opt.statusFunc != nil:
			statusFunc = opt.statusFunc
		case opt.header != nil:
			h := *opt.header
			k, v := h[0], h[1]
			req.Header.Add(k, v)
		case opt.errThing != nil:
			errThing = opt.errThing
		}
	}
	resp, err := Client.Do(req)
	if err != nil {
		return fmt.Errorf("error performing request: %w", err)
	}
	defer resp.Body.Close()
	if statusFunc != nil {
		statusFunc(resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if errThing != nil {
			reader := io.LimitReader(resp.Body, sizeLimit)
			if err = json.NewDecoder(reader).Decode(errThing); err != nil {
				return fmt.Errorf("HTTP error: %q (code %d). error encountered parsing error body: %w", resp.Status, resp.StatusCode, err)
			}
		}
		return fmt.Errorf("HTTP error: %q (code %d)", resp.Status, resp.StatusCode)
	}
	if thing == nil {
		return nil
	}
	reader := io.LimitReader(resp.Body, sizeLimit)
	if err = json.NewDecoder(reader).Decode(thing); err != nil {
		return fmt.Errorf("error decoding request: %w", err)
	}
	return nil
}
