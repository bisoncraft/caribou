// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package websocket

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/ws"
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

func newWSClient(addr string, conn ws.Connection, hndlr func(msg *msgjson.Message) *msgjson.Error, logger dex.Logger) *wsClient {
	return &wsClient{
		WSLink: ws.NewWSLink(addr, conn, pingPeriod, hndlr, logger),
		cid:    atomic.AddInt32(&cidCounter, 1),
	}
}

// Core specifies the needed methods for Server to operate. Satisfied by *core.Core.
type Core interface {
	AckNotes([]dex.Bytes)
}

// Server is a websocket hub that tracks all running websocket clients, allows
// sending notifications to all of them, and manages per-client subscriptions.
type Server struct {
	core Core
	log  dex.Logger
	wg   sync.WaitGroup

	clientsMtx sync.RWMutex
	clients    map[int32]*wsClient
}

// New returns a new websocket Server.
func New(core Core, log dex.Logger) *Server {
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

// HandleConnect handles the websocket connection request, creating a
// ws.Connection and a connect thread. Since the http.Request's Context is
// canceled after ServerHTTP returns, a separate context must be provided to be
// able to cancel the hijacked connection handler at a later time since this
// function is not blocking.
func (s *Server) HandleConnect(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	wsConn, err := ws.NewConnection(w, r, pongWait)
	if err != nil {
		s.log.Errorf("ws connection error: %v", err)
		return
	}

	// Launch the handler for the upgraded connection. Shutdown will wait for
	// these to return.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.connect(ctx, wsConn, r.RemoteAddr)
	}()
}

// connect handles a new websocket client by creating a new wsClient, starting
// it, and blocking until the connection closes. This method should be
// run as a goroutine.
func (s *Server) connect(ctx context.Context, conn ws.Connection, addr string) {
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
	cm := dex.NewConnectionMaster(cl)
	err := cm.ConnectOnce(ctx) // we discard the cm anyway, but good practice
	if err != nil {
		s.clientsMtx.Unlock()
		s.log.Errorf("websocketHandler client connect: %v", err)
		return
	}

	// Add the client to the map only after it is connected so that notify does
	// not attempt to send to non-existent connection.
	s.clients[cl.cid] = cl
	s.clientsMtx.Unlock()

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

type ackNoteIDs []dex.Bytes

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
