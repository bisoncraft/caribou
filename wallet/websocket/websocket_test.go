//go:build !live

package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/msgjson"
	"github.com/gorilla/websocket"
)

var tCtx context.Context

type TCore struct{}

func (c *TCore) AckNotes(ids []util.Bytes) {}

type TConn struct {
	msg       []byte
	reads     [][]byte      // data for ReadMessage
	respReady chan []byte   // signal from WriteMessage
	close     chan struct{} // Close tells ReadMessage to return with error
}

var readTimeout = 10 * time.Second // ReadMessage must not return constantly with nothing

func (c *TConn) ReadMessage() (int, []byte, error) {
	if len(c.reads) > 0 {
		var read []byte
		// pop front
		read, c.reads = c.reads[0], c.reads[1:]
		return len(read), read, nil
	}

	select {
	case <-c.close: // receive from nil channel blocks
		return 0, nil, fmt.Errorf("closed")
	case <-time.After(readTimeout):
		return 0, nil, fmt.Errorf("read timeout")
	}
}

func (c *TConn) addRead(read []byte) {
	// push back
	c.reads = append(c.reads, read)
}

func (c *TConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (c *TConn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	return nil
}

func (c *TConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *TConn) SetReadLimit(int64) {}

func (c *TConn) WriteMessage(_ int, msg []byte) error {
	c.msg = msg
	select {
	case c.respReady <- msg:
	default:
	}
	return nil
}

func (c *TConn) Close() error {
	// If the test has a non-nil close channel, signal close.
	select {
	case c.close <- struct{}{}:
	default:
	}
	return nil
}

type tLink struct {
	cl   *wsClient
	conn *TConn
}

func newLink() *tLink {
	conn := &TConn{
		respReady: make(chan []byte, 1),
		close:     make(chan struct{}, 1),
	}
	ipk := util.IPKey{16, 16, 120, 120 /* ipv6 1010:7878:: */}
	cl := newWSClient(ipk.String(), conn, func(*msgjson.Message) *msgjson.Error { return nil }, util.StdOutLogger("ws_TEST", util.LevelTrace))
	return &tLink{
		cl:   cl,
		conn: conn,
	}
}

func newTServer() (*Server, *TCore) {
	c := &TCore{}
	return New(c, util.StdOutLogger("TEST", util.LevelTrace)), c
}

func TestMain(m *testing.M) {
	var shutdown func()
	tCtx, shutdown = context.WithCancel(context.Background())
	doIt := func() int {
		// Not counted as coverage, must test Archiver constructor explicitly.
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

func TestHandleMessage(t *testing.T) {
	link := newLink()
	srv, _ := newTServer()

	// NOTE: link is not started because the handlers in this test do not
	// actually use it.

	var msg *msgjson.Message

	ensureErr := func(name string, wantCode int) {
		got := srv.handleMessage(link.cl, msg)
		if got == nil {
			t.Fatalf("%s: no error", name)
		}
		if wantCode != got.Code {
			t.Fatalf("%s, wanted %d, got %d", name, wantCode, got.Code)
		}
	}

	// Send a response, which is unsupported on the web server.
	msg, _ = msgjson.NewResponse(1, nil, nil)
	ensureErr("bad route", msgjson.UnknownMessageType)

	// Unknown route.
	msg, _ = msgjson.NewRequest(1, "123", nil)
	ensureErr("bad route", msgjson.UnknownMessageType)

	// Set the route correctly.
	wsHandlers["123"] = func(*Server, *wsClient, *msgjson.Message) *msgjson.Error {
		return nil
	}

	rpcErr := srv.handleMessage(link.cl, msg)
	if rpcErr != nil {
		t.Fatalf("error for good message: %d: %s", rpcErr.Code, rpcErr.Message)
	}
}

// TestStampedEpochPredatesTeardown asserts a connection carries the epoch
// captured before whatever authorized it, not the one current at upgrade.
// deauth clears the session tokens and only then bumps the epoch via
// DisconnectAll, so a teardown landing between the auth check and the upgrade
// would otherwise hand the new link the already-incremented epoch, leaving a
// socket authorized under a dead session live.
func TestStampedEpochPredatesTeardown(t *testing.T) {
	srv, _ := newTServer()

	// Stands in for a session teardown completing after the epoch is stamped
	// but before the connection is upgraded.
	teardown := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			srv.DisconnectAll()
			next.ServeHTTP(w, r)
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts := httptest.NewServer(srv.StampEpoch(teardown(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			srv.HandleConnect(ctx, w, r)
		}))))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// The peer must observe the close. An idle-but-open socket is the bug.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("a socket authorized before a teardown survived it")
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatalf("socket left open across a teardown; read timed out instead: %v", err)
		}
	}
}

// TestUnstampedUpgradeRefused asserts a route that forgot StampEpoch is
// refused rather than served. Falling back to the epoch current at upgrade
// would reinstate the teardown race silently — the socket would look healthy
// and no test would fail — so the contract is enforced here instead.
func TestUnstampedUpgradeRefused(t *testing.T) {
	srv, _ := newTServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.HandleConnect(ctx, w, r)
	}))
	defer ts.Close()

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("an unstamped websocket upgrade succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 for an unstamped upgrade, got %v (err %v)", resp, err)
	}
}

// TestConnectAcrossShutdown asserts that a connection registering itself after
// Shutdown has already swept the client map is dropped. Shutdown blocks in
// wg.Wait until every connect goroutine returns, so a link nothing disconnects
// holds shutdown open until the peer happens to go away on its own. The
// connect context is deliberately never canceled here: production only avoids
// the hang because AppServer calls Shutdown from its ctx.Done goroutine, which
// is a coupling this must not depend on.
func TestConnectAcrossShutdown(t *testing.T) {
	srv, _ := newTServer()
	conn := &TConn{
		respReady: make(chan []byte, 1),
		close:     make(chan struct{}, 1),
	}

	// Shutdown has swept the map; this connection was not in it yet, so
	// nothing disconnected it.
	srv.clientsMtx.Lock()
	srv.shuttingDown = true
	epoch := srv.epoch
	srv.clientsMtx.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ipk := util.IPKey{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 127, 0, 0, 1}
		srv.connect(context.Background(), conn, ipk.String(), epoch)
	}()

	// Well inside readTimeout, so returning proves the link was disconnected
	// rather than that the fake connection's read gave up.
	select {
	case <-done:
	case <-time.After(readTimeout / 2):
		t.Fatal("connect did not return: a client registered across shutdown was left live")
	}
}

func TestClientMap(t *testing.T) {
	srv, _ := newTServer()
	resp := make(chan []byte, 1)
	conn := &TConn{
		respReady: resp,
		close:     make(chan struct{}, 1),
	}
	// msg.ID == 0 gets an error response, which can be discarded.
	read, _ := json.Marshal(msgjson.Message{ID: 0})
	conn.addRead(read)

	// Create the context that the http request handler would receive.
	ctx, shutdown := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		ipk := util.IPKey{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255 /* ipv4 */, 127, 0, 0, 1}
		srv.connect(ctx, conn, ipk.String(), srv.epoch)
		wg.Done()
	}()

	// When a response to our dummy message is received, the client should be in
	// RPCServer's client map.
	<-resp

	var cl *wsClient
	srv.clientsMtx.Lock()
	i := len(srv.clients)
	if i != 1 {
		t.Fatalf("expected 1 client in server map, found %d", i)
	}
	for _, c := range srv.clients {
		cl = c
		break
	}
	srv.clientsMtx.Unlock()

	// Close the server and make sure the connection is closed.
	shutdown()
	wg.Wait() // websocketHandler since it's using log
	if !cl.Off() {
		t.Fatal("connection not closed on server shutdown")
	}
}
