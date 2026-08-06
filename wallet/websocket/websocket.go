// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package websocket

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/msgjson"
	"github.com/bisoncraft/caribou/util/ws"
)

var (
	// Time allowed to read the next pong message from the peer. The
	// default is intended for production, but leaving as a var instead of const
	// to facilitate testing.
	pongWait = 60 * time.Second
	// Send pings to peer with this period. Must be less than pongWait. The
	// default is intended for production, but leaving as a var instead of const
	// to facilitate testing.
	pingPeriod = (pongWait * 9) / 10
	// A client id counter.
	cidCounter int32
)

// wsClient is a persistent websocket connection to a client.
type wsClient struct {
	*ws.WSLink
	cid int32
}

func newWSClient(addr string, conn ws.Connection, hndlr func(msg *msgjson.Message) *msgjson.Error, logger util.Logger) *wsClient {
	return &wsClient{
		WSLink: ws.NewWSLink(addr, conn, pingPeriod, hndlr, logger),
		cid:    atomic.AddInt32(&cidCounter, 1),
	}
}

// Core specifies the needed methods for Server to operate. Satisfied by *core.Core.
type Core interface {
	AckNotes([]util.Bytes)
}

// Server is a websocket hub that tracks all running websocket clients, allows
// sending notifications to all of them, and manages per-client subscriptions.
type Server struct {
	core Core
	log  util.Logger
	wg   sync.WaitGroup

	clientsMtx sync.RWMutex
	clients    map[int32]*wsClient
	// shuttingDown is set by Shutdown under clientsMtx. HandleConnect adds to
	// wg under the same lock, so a connection arriving as Shutdown runs cannot
	// race wg.Wait, which sync.WaitGroup panics on.
	shuttingDown bool
	// epoch increments on every DisconnectAll. A client registers itself from
	// its own goroutine, so without this a connection accepted just before a
	// session teardown would land in the map after the sweep and survive it.
	epoch uint64
}

// New returns a new websocket Server.
func New(core Core, log util.Logger) *Server {
	return &Server{
		core:    core,
		log:     log,
		clients: make(map[int32]*wsClient),
	}
}

// Shutdown gracefully shuts down all connected clients, waiting for them to
// disconnect and any running goroutines and message handlers to return.
func (s *Server) Shutdown() {
	s.clientsMtx.Lock()
	s.shuttingDown = true
	for _, cl := range s.clients {
		cl.Disconnect()
	}
	s.clientsMtx.Unlock()
	// Each upgraded connection handler must return. This also waits for
	// response handlers as long as dex/ws.(*WSLink) operates as designed and
	// each (*Server).connect goroutine waits for the link's WaitGroup before
	// returning.
	s.wg.Wait()
}

// DisconnectAll drops every connected client without waiting for their
// handlers to return. A link is authorized only at upgrade and is never
// re-checked, so a socket established under a session that has since been torn
// down would otherwise keep receiving notifications. Each client removes itself
// from the map as its connect goroutine unwinds.
func (s *Server) DisconnectAll() {
	s.clientsMtx.Lock()
	defer s.clientsMtx.Unlock()
	s.epoch++
	for _, cl := range s.clients {
		cl.Disconnect()
	}
}

// epochKey keys the epoch StampEpoch records on a request context.
type epochKey struct{}

// StampEpoch records the current epoch on the request context. Every route
// reaching HandleConnect must be wrapped in it, and it must run ahead of
// whatever authorizes the upgrade: the epoch HandleConnect captures
// has to predate the auth check, or a session teardown landing between the two
// leaves the connection holding the already-incremented epoch and surviving the
// sweep. Stamped first, a teardown either completes before the auth check —
// which then refuses the upgrade — or after the stamp, which makes the
// connection stale.
func (s *Server) StampEpoch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.clientsMtx.RLock()
		epoch := s.epoch
		s.clientsMtx.RUnlock()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), epochKey{}, epoch)))
	})
}

// HandleConnect handles the websocket connection request, creating a
// ws.Connection and a connect thread. Since the http.Request's Context is
// canceled after ServerHTTP returns, a separate context must be provided to be
// able to cancel the hijacked connection handler at a later time since this
// function is not blocking.
func (s *Server) HandleConnect(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// The epoch has to be captured ahead of whatever authorized this request,
	// which only StampEpoch can do. Falling back to the current epoch here
	// would silently reinstate the race it exists to close, so refuse instead:
	// a route wired without the middleware fails outright rather than serving
	// sockets that no session teardown can reliably drop.
	epoch, ok := r.Context().Value(epochKey{}).(uint64)
	if !ok {
		s.log.Errorf("Refusing websocket connection from %s: the route is not wrapped in StampEpoch", r.RemoteAddr)
		http.Error(w, "not authorized", http.StatusUnauthorized)
		return
	}

	wsConn, err := ws.NewConnection(w, r, pongWait)
	if err != nil {
		s.log.Errorf("ws connection error: %v", err)
		return
	}

	// Launch the handler for the upgraded connection. Shutdown will wait for
	// these to return, so the Add has to be ordered against its Wait: take
	// clientsMtx and refuse the connection outright once shutdown has begun,
	// rather than adding to a WaitGroup that is already being waited on.
	s.clientsMtx.Lock()
	if s.shuttingDown {
		s.clientsMtx.Unlock()
		s.log.Debugf("Refusing websocket connection from %s during shutdown", r.RemoteAddr)
		if err := wsConn.Close(); err != nil {
			s.log.Errorf("Error closing refused websocket connection: %v", err)
		}
		return
	}
	s.wg.Add(1)
	s.clientsMtx.Unlock()

	go func() {
		defer s.wg.Done()
		s.connect(ctx, wsConn, r.RemoteAddr, epoch)
	}()
}

// connect handles a new websocket client by creating a new wsClient, starting
// it, and blocking until the connection closes. This method should be
// run as a goroutine.
func (s *Server) connect(ctx context.Context, conn ws.Connection, addr string, epoch uint64) {
	s.log.Debugf("New websocket client %s", addr)
	// Create a new websocket client to handle the new websocket connection
	// and wait for it to shut down.  Once it has shutdown (and hence
	// disconnected), remove it.
	var cl *wsClient
	cl = newWSClient(addr, conn, func(msg *msgjson.Message) *msgjson.Error {
		return s.handleMessage(cl, msg)
	}, s.log.SubLogger(addr))

	// Lock the clients map before starting the connection listening so that
	// synchronized map accesses are guaranteed to reflect this connection.
	// Also, ensuring only live connections are in the clients map notify from
	// sending before it is connected.
	s.clientsMtx.Lock()
	cm := util.NewConnectionMaster(cl)
	err := cm.ConnectOnce(ctx) // we discard the cm anyway, but good practice
	if err != nil {
		s.clientsMtx.Unlock()
		s.log.Errorf("websocketHandler client connect: %v", err)
		return
	}

	// Add the client to the map only after it is connected so that notify does
	// not attempt to send to non-existent connection.
	s.clients[cl.cid] = cl
	// A session teardown or a shutdown that swept the map while this connection
	// was being established would have missed it. Drop it rather than let it
	// outlive the session it authenticated under, or leave Shutdown's wg.Wait
	// blocked on a link nothing has disconnected.
	stale := epoch != s.epoch || s.shuttingDown
	s.clientsMtx.Unlock()
	if stale {
		s.log.Debugf("Dropping websocket client %s accepted across a session teardown", addr)
		cl.Disconnect()
	}

	defer func() {
		s.clientsMtx.Lock()
		delete(s.clients, cl.cid)
		s.clientsMtx.Unlock()
	}()

	cm.Wait() // also waits for any handleMessage calls in (*WSLink).inHandler
	s.log.Tracef("Disconnected websocket client %s", addr)
}

// Notify sends a notification to the websocket client.
func (s *Server) Notify(route string, payload any) {
	msg, err := msgjson.NewNotification(route, payload)
	if err != nil {
		s.log.Errorf("%q notification encoding error: %v", route, err)
		return
	}
	s.clientsMtx.RLock()
	defer s.clientsMtx.RUnlock()
	for _, cl := range s.clients {
		if err = cl.Send(msg); err != nil {
			s.log.Warnf("Failed to send %v notification to client %v at %v: %v",
				msg.Route, cl.cid, cl.Addr(), err)
		}
	}
}

// handleMessage handles the websocket message, calling the right handler for
// the route.
func (s *Server) handleMessage(conn *wsClient, msg *msgjson.Message) *msgjson.Error {
	s.log.Tracef("message of type %d received for route %s", msg.Type, msg.Route)
	if msg.Type == msgjson.Request {
		handler, found := wsHandlers[msg.Route]
		if !found {
			return msgjson.NewError(msgjson.UnknownMessageType, "unknown route %q", msg.Route)
		}
		return handler(s, conn, msg)
	}
	// Web server doesn't send requests, only responses and notifications, so
	// a response-type message from a client is an error.
	return msgjson.NewError(msgjson.UnknownMessageType, "web server only handles requests")
}

// All request handlers must be defined with this signature.
type wsHandler func(*Server, *wsClient, *msgjson.Message) *msgjson.Error

// wsHandlers is the map used by the server to locate the router handler for a
// request.
var wsHandlers = map[string]wsHandler{
	"acknotes": wsAckNotes,
}

type ackNoteIDs []util.Bytes

// wsAckNotes is the handler for the 'acknotes' websocket route. It informs the
// Core that the user has seen the specified notifications.
func wsAckNotes(s *Server, _ *wsClient, msg *msgjson.Message) *msgjson.Error {
	ids := make(ackNoteIDs, 0)
	err := msg.Unmarshal(&ids)
	if err != nil {
		s.log.Errorf("error acking notifications: %v", err)
		return nil
	}
	s.core.AckNotes(ids)
	return nil
}
