// Copyright (c) 2024 The Bison Wallet developers
// Copyright (c) The go-electrum-client authors
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/decred/go-socks/socks"
)

// keepAliveInterval is the keep-alive ping cadence. ElectrumX servers
// idle-time-out around 10 minutes by default, so 10s is well within the worst
// case and gives prompt detection of dropped sockets.
const keepAliveInterval = 10 * time.Second

// readIdleTimeout bounds how long listen() will block waiting for new bytes
// from the server. It is refreshed at the top of each listen iteration, so
// the limit is "no bytes from server in this window" rather than "the entire
// read takes longer than this", which matters when a multi-megabyte header
// batch arrives in chunks on a slow link. With the keep-alive ping firing
// every keepAliveInterval, the server should never go silent for this long
// on a healthy connection.
const readIdleTimeout = 3 * keepAliveInterval

// dialTimeout is the connection timeout when dialing without a SOCKS proxy.
const dialTimeout = 5 * time.Second

// torDialTimeout is the connection timeout when dialing through Tor.
const torDialTimeout = 20 * time.Second

// requestWriteTimeout bounds a single Write call against a stalled server.
const requestWriteTimeout = 7 * time.Second

// notifyBufSize is the buffer depth on each notification channel. Unbuffered
// would deadlock the listener if the consumer is slow.
const notifyBufSize = 16

// Errors that may be returned by Server methods.
var (
	// ErrServerClosed is returned by request() when the underlying
	// connection has been closed.
	ErrServerClosed = errors.New("electrum: server connection closed")
)

// dialOptions are connection-level options for a Server.
type dialOptions struct {
	// TLSConfig, if non-nil, wraps the underlying TCP connection. For
	// insecure-skip-verify use the appropriate TLS config; the package
	// does not impose a policy.
	TLSConfig *tls.Config

	// SOCKS5Proxy, if non-empty, is the host:port of a local SOCKS5
	// proxy used for outbound connections (typically a Tor relay).
	SOCKS5Proxy string
}

// Server owns a single TCP/TLS connection to an electrumx server and provides
// generic JSON-RPC request/response and notification dispatch. It is built
// once per connection; reconnects mean a fresh Server.
type Server struct {
	addr   string
	conn   net.Conn
	cancel context.CancelFunc
	done   chan struct{}

	nextID atomic.Uint64

	// Pending request handlers, keyed by request id.
	handlersMtx sync.Mutex
	handlers    map[uint64]chan *rpcResponse

	// Notification channels. These are closed by the listen goroutine when
	// it returns, so all sends are guarded by the corresponding mutex to
	// avoid sending on a closed channel.
	headersMtx    sync.Mutex
	headersNotify chan *HeadersTip

	scripthashMtx    sync.Mutex
	scripthashNotify chan *ScripthashStatus
}

// dial connects to addr with the given options and returns a started Server.
// The Server runs background goroutines for read dispatch and keep-alive
// pings; both stop when ctx is cancelled or the connection drops.
func dial(ctx context.Context, addr string, opts *dialOptions) (*Server, error) {
	var dialer func(ctx context.Context, network, address string) (net.Conn, error)
	var dCtx context.Context
	var dCancel context.CancelFunc
	if opts.SOCKS5Proxy != "" {
		proxy := &socks.Proxy{Addr: opts.SOCKS5Proxy, TorIsolation: true}
		dialer = proxy.DialContext
		dCtx, dCancel = context.WithTimeout(ctx, torDialTimeout)
	} else {
		dialer = (&net.Dialer{}).DialContext
		dCtx, dCancel = context.WithTimeout(ctx, dialTimeout)
	}
	defer dCancel()

	conn, err := dialer(dCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("electrum: dial %s: %w", addr, err)
	}
	if opts.TLSConfig != nil {
		tlsConn := tls.Client(conn, opts.TLSConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("electrum: TLS handshake to %s: %w", addr, err)
		}
		conn = tlsConn
	}

	// Wrap the parent ctx so callers can shut us down individually.
	connCtx, cancel := context.WithCancel(ctx)

	s := &Server{
		addr:             addr,
		conn:             conn,
		cancel:           cancel,
		done:             make(chan struct{}),
		handlers:         make(map[uint64]chan *rpcResponse),
		headersNotify:    make(chan *HeadersTip, notifyBufSize),
		scripthashNotify: make(chan *ScripthashStatus, notifyBufSize),
	}

	go s.listen(connCtx)
	go s.keepAlive(connCtx)
	go func() {
		<-connCtx.Done()
		_ = conn.Close()
		close(s.done)
	}()

	return s, nil
}

// Addr returns the server endpoint.
func (s *Server) Addr() string { return s.addr }

// Done returns a channel that is closed once the Server has fully shut down
// (read goroutine exited, connection closed, notification channels closed).
func (s *Server) Done() <-chan struct{} { return s.done }

// Close initiates shutdown. The Done channel will close once shutdown
// finishes. Safe to call multiple times.
func (s *Server) Close() { s.cancel() }

// nextRequestID returns a fresh JSON-RPC request id.
func (s *Server) nextRequestID() uint64 { return s.nextID.Add(1) }

// request issues a JSON-RPC request and decodes the response result into out.
// If out is nil, the response body is discarded. The call returns whenever
// ctx is cancelled or the connection dies, even if the server never replies.
func (s *Server) request(ctx context.Context, method string, args, out any) error {
	id := s.nextRequestID()
	msg, err := prepareRequest(id, method, args)
	if err != nil {
		return err
	}
	msg = append(msg, '\n')

	ch := s.registerHandler(id)

	if err := s.write(msg); err != nil {
		s.unregisterHandler(id)
		// A failed write means the connection is gone; tear down so
		// the listen goroutine doesn't sit on a half-dead socket.
		s.cancel()
		return err
	}

	select {
	case <-ctx.Done():
		s.unregisterHandler(id)
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return ErrServerClosed
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, out)
	}
}

// write sends raw bytes to the server with a bounded write deadline.
func (s *Server) write(b []byte) error {
	if err := s.conn.SetWriteDeadline(time.Now().Add(requestWriteTimeout)); err != nil {
		return err
	}
	_, err := s.conn.Write(b)
	return err
}

func (s *Server) registerHandler(id uint64) chan *rpcResponse {
	ch := make(chan *rpcResponse, 1)
	s.handlersMtx.Lock()
	s.handlers[id] = ch
	s.handlersMtx.Unlock()
	return ch
}

func (s *Server) unregisterHandler(id uint64) {
	s.handlersMtx.Lock()
	delete(s.handlers, id)
	s.handlersMtx.Unlock()
}

func (s *Server) deliverResponse(id uint64, resp *rpcResponse) {
	s.handlersMtx.Lock()
	ch, ok := s.handlers[id]
	delete(s.handlers, id)
	s.handlersMtx.Unlock()
	if !ok {
		log.Tracef("electrum %s: response for unknown id %d", s.addr, id)
		return
	}
	ch <- resp // buffered with capacity 1; safe.
}

// listen reads frames from the connection and dispatches them to either
// pending request handlers or the notification channels. It exits on read
// error or context cancellation, then drains pending handlers so any waiting
// callers unblock with ErrServerClosed.
func (s *Server) listen(ctx context.Context) {
	defer s.closeNotifyChannels()
	defer s.failPendingHandlers()

	// Buffer big enough for an initial 2016-header sync chunk plus
	// padding. 2016 * 80 bytes per header * 16x safety = ~2.5 MiB.
	reader := bufio.NewReaderSize(s.conn, 2016*80*16)

	for {
		if ctx.Err() != nil {
			return
		}
		// Refresh the read deadline per iteration so a single slow read
		// can't be cut off by a stale deadline set earlier. The window
		// resets every time we successfully receive a frame, so a
		// chunky multi-MB header batch trickling in over a slow link
		// stays alive as long as bytes keep arriving.
		if err := s.conn.SetReadDeadline(time.Now().Add(readIdleTimeout)); err != nil {
			if ctx.Err() == nil {
				log.Tracef("electrum %s: set read deadline: %v", s.addr, err)
			}
			s.cancel()
			return
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if ctx.Err() == nil {
				log.Tracef("electrum %s: read: %v", s.addr, err)
			}
			s.cancel()
			return
		}

		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			log.Tracef("electrum %s: bad frame: %v", s.addr, err)
			continue
		}

		if resp.Method != "" {
			s.dispatchNotification(ctx, resp.Method, line)
			continue
		}
		s.deliverResponse(resp.ID, &resp)
	}
}

func (s *Server) dispatchNotification(ctx context.Context, method string, raw []byte) {
	var n ntfnPayload
	if err := json.Unmarshal(raw, &n); err != nil {
		log.Tracef("electrum %s: notification payload decode: %v", s.addr, err)
		return
	}
	switch method {
	case "blockchain.headers.subscribe":
		// Params is an array of {height, hex} objects.
		var tips []*HeadersTip
		if err := json.Unmarshal(n.Params, &tips); err != nil {
			log.Tracef("electrum %s: headers notify decode: %v", s.addr, err)
			return
		}
		for _, t := range tips {
			s.sendHeadersTip(t)
		}
	case "blockchain.scripthash.subscribe":
		// Params is a 2-element string array [scripthash, status].
		var pair [2]string
		if err := json.Unmarshal(n.Params, &pair); err != nil {
			log.Tracef("electrum %s: scripthash notify decode: %v", s.addr, err)
			return
		}
		s.sendScripthashStatus(ctx, &ScripthashStatus{Scripthash: pair[0], Status: pair[1]})
	default:
		log.Tracef("electrum %s: unknown notification method %q", s.addr, method)
	}
}

func (s *Server) sendHeadersTip(t *HeadersTip) {
	s.headersMtx.Lock()
	defer s.headersMtx.Unlock()
	select {
	case s.headersNotify <- t:
	default:
		log.Warnf("electrum %s: headers notify channel full, dropping tip @ %d", s.addr, t.Height)
	}
}

func (s *Server) sendScripthashStatus(ctx context.Context, st *ScripthashStatus) {
	s.scripthashMtx.Lock()
	defer s.scripthashMtx.Unlock()
	// Block until delivered (or shutdown) rather than dropping. A scripthash
	// status is the address's latest state; dropping the last one would leave
	// an incoming tx invisible until unrelated future activity. This runs on
	// the listen goroutine, but the network-layer forwarder drains
	// scripthashNotify continuously into a coalescing map with no network I/O,
	// so the send returns promptly and listen is never deadlocked behind it.
	// ctx is the connection context, cancelled on Close, so shutdown unblocks
	// this before listen returns to run closeNotifyChannels.
	select {
	case s.scripthashNotify <- st:
	case <-ctx.Done():
	}
}

func (s *Server) closeNotifyChannels() {
	s.headersMtx.Lock()
	close(s.headersNotify)
	s.headersMtx.Unlock()
	s.scripthashMtx.Lock()
	close(s.scripthashNotify)
	s.scripthashMtx.Unlock()
}

func (s *Server) failPendingHandlers() {
	s.handlersMtx.Lock()
	defer s.handlersMtx.Unlock()
	for id, ch := range s.handlers {
		close(ch)
		delete(s.handlers, id)
	}
}

// keepAlive pings the server on a fixed cadence so the connection's read side
// has a guaranteed signal to wake it up. listen() handles the read deadline
// itself; we deliberately do NOT bounce the deadline here, because doing so
// from a separate goroutine races with whatever read listen() is currently
// blocked on (e.g. a large in-flight header batch). keepAlive exits when ctx
// is cancelled or ping fails; failure cascades through listen and surfaces to
// in-flight callers via ErrServerClosed.
func (s *Server) keepAlive(ctx context.Context) {
	t := time.NewTicker(keepAliveInterval)
	defer t.Stop()
	for {
		if err := s.ping(ctx); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ping issues `server.ping` and waits for the reply.
func (s *Server) ping(ctx context.Context) error {
	return s.request(ctx, "server.ping", nil, nil)
}

// HeadersNotifyChan returns the channel that receives tip-change notifications
// after the caller has issued a `blockchain.headers.subscribe`. The channel is
// closed when the Server shuts down.
func (s *Server) HeadersNotifyChan() <-chan *HeadersTip { return s.headersNotify }

// ScripthashNotifyChan returns the channel that receives scripthash status
// notifications after the caller has issued `blockchain.scripthash.subscribe`
// for one or more scripthashes. The channel is closed when the Server shuts
// down.
func (s *Server) ScripthashNotifyChan() <-chan *ScripthashStatus {
	return s.scripthashNotify
}
