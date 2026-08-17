// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/gorilla/websocket"
)

const (
	// accountSubscribe is subscribed per watched account, and
	// accountNotification is the method name the server pushes changes under.
	//
	// Deliberately not slotSubscribe. Slots are the obvious way to learn the
	// tip, but Solana produces one roughly every 400ms, so subscribing to them
	// would push ~2.5 frames per second forever. That is worse for a phone than
	// the 2s poll this replaces: the byte count is trivial either way, and what
	// costs battery is never letting the modem leave its high-power state.
	// Account notifications instead arrive only when this wallet's balance or
	// history actually changes, which is exactly when there is work to do, and
	// a slow tip poll covers what they do not.
	accountSubscribe    = "accountSubscribe"
	accountUnsubscribe  = "accountUnsubscribe"
	accountNotification = "accountNotification"

	// wsDialTimeout bounds one dial attempt, wsWriteWait one frame write.
	wsDialTimeout = 15 * time.Second
	wsWriteWait   = 10 * time.Second

	// wsPingInterval is the control-ping cadence and wsPongWait how long a read
	// may block before the link is declared dead. This pair is the only traffic
	// an idle subscription generates, so it sets the idle cost of the whole
	// mechanism: one exchange per minute, against the 30 HTTP requests a minute
	// of 2s polling. wsPongWait must exceed wsPingInterval, or a healthy link
	// times out in the gap between pings.
	wsPingInterval = time.Minute
	wsPongWait     = wsPingInterval + 30*time.Second

	// wsReconnectMin and wsReconnectMax bound the reconnect backoff. Polling
	// continues at its unsubscribed cadence throughout, so a long backoff
	// costs freshness only for the notifications, never correctness.
	wsReconnectMin = time.Second
	wsReconnectMax = time.Minute

	// wsSessionSettled is how long a session must survive before its
	// reconnect backoff is forgiven. Without it, a link that dies after an
	// hour and one that is refused instantly share a backoff, so an endpoint
	// that drops a connection nightly would creep to the maximum delay and
	// stay there.
	wsSessionSettled = 2 * time.Minute
)

// wsClient keeps a Solana JSON-RPC websocket subscribed to a set of accounts,
// reconnecting for as long as its run context lives. Callers register accounts
// with watch and read live to decide how hard to lean on HTTP polling instead.
//
// The client is a notification transport, not a data source: a callback says
// "this account changed", never what it changed to. Everything the wallet needs
// still comes from the HTTP RPC. That keeps the subscription free to drop,
// reconnect, or miss a frame without any of it becoming a correctness problem,
// since the fallback poll re-reads the same state anyway.
type wsClient struct {
	url string
	log util.Logger

	// live reports a connection that is up with every watched account
	// confirmed subscribed. It is the wallet's cue to slow its poll, so it is
	// deliberately strict: a half-subscribed connection reads as not live.
	live atomic.Bool

	// mtx guards every map below and conn, and serializes frame writes.
	// gorilla's Conn permits only one concurrent writer, and the volume here
	// (a few subscribes per session, one ping a minute) is far too low for
	// holding the lock across a write to matter.
	mtx     sync.Mutex
	conn    *websocket.Conn
	nextID  uint64
	watched map[string]func() // account -> change callback
	reqAcct map[uint64]string // in-flight request id -> account
	subAcct map[uint64]string // server subscription id -> account
	acctSub map[string]uint64 // account -> server subscription id
}

func newWSClient(url string, log util.Logger) *wsClient {
	return &wsClient{
		url:     url,
		log:     log,
		watched: make(map[string]func()),
		reqAcct: make(map[uint64]string),
		subAcct: make(map[uint64]string),
		acctSub: make(map[string]uint64),
	}
}

// watch registers onChange to be called whenever account changes on chain, and
// subscribes immediately if a connection is up. Registering is durable across
// reconnects: every watched account is re-subscribed on each new session.
// onChange runs on the read loop, so it must not block.
func (c *wsClient) watch(account string, onChange func()) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	c.watched[account] = onChange
	c.refreshLive()
	if c.conn == nil {
		return
	}
	if _, subscribed := c.acctSub[account]; subscribed {
		return
	}
	if c.subscribePending(account) {
		// A subscribe from an earlier watch is still awaiting its reply. A
		// second one would have the server open two subscriptions for the same
		// account, and only the later id would end up in acctSub, so the
		// earlier one could never be released.
		return
	}
	if err := c.sendSubscribe(account); err != nil {
		c.log.Debugf("solana websocket: subscribe %s: %v", account, err)
	}
}

// unwatch drops an account's registration and tells the server to stop sending
// for it. A token wallet calls this when it disconnects, so the parent's
// connection does not keep carrying notifications nobody handles.
//
// An in-flight subscribe for the account is deliberately left in reqAcct: its
// reply carries the only copy of the subscription id, so handle needs the entry
// to recognize the reply and release it.
func (c *wsClient) unwatch(account string) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	delete(c.watched, account)
	subID, subscribed := c.acctSub[account]
	if subscribed {
		delete(c.acctSub, account)
		delete(c.subAcct, subID)
	}
	c.refreshLive()
	if c.conn == nil || !subscribed {
		return
	}
	if err := c.sendUnsubscribe(subID); err != nil {
		c.log.Debugf("solana websocket: unsubscribe %s: %v", account, err)
	}
}

// run dials and re-dials until ctx is done, and is the client's only goroutine
// owner. It blocks, so callers run it under the WaitGroup their Connect
// returns.
func (c *wsClient) run(ctx context.Context) {
	backoff := wsReconnectMin
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.log.Debugf("solana websocket session ended: %v", err)
		}
		if time.Since(started) >= wsSessionSettled {
			backoff = wsReconnectMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > wsReconnectMax {
			backoff = wsReconnectMax
		}
	}
}

// session runs one connection from dial to death, returning the error that
// ended it.
func (c *wsClient) session(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: wsDialTimeout}
	conn, resp, err := dialer.DialContext(ctx, c.url, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.url, err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	// ctx cancellation has to unblock the read loop, and no read deadline
	// shorter than wsPongWait would do it. Closing the connection is what
	// interrupts ReadMessage, so a watchdog does that; closing done on the way
	// out retires the watchdog and the ping loop together.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	defer func() {
		// Drop all subscription state with the connection that carried it.
		// Server subscription ids are per-connection, so keeping them would
		// mean routing the next session's notifications by stale ids. The
		// watched set deliberately survives: it is the caller's registration,
		// and the next session re-subscribes from it.
		//
		// live is cleared inside the lock, and after conn: refreshLive reads
		// conn, so an unwatch interleaving between a bare Store and the clear
		// below would recompute live as true against a connection already on
		// its way out, and nothing would recompute it again until the next
		// session came up.
		c.mtx.Lock()
		c.conn = nil
		c.reqAcct = make(map[uint64]string)
		c.subAcct = make(map[uint64]string)
		c.acctSub = make(map[string]uint64)
		c.live.Store(false)
		c.mtx.Unlock()
		conn.Close()
	}()

	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	if err := conn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
		return err
	}

	// Re-subscribe every registered account. sendSubscribe touches reqAcct, not
	// watched, so ranging over watched here is safe.
	c.mtx.Lock()
	c.conn = conn
	for account := range c.watched {
		if err := c.sendSubscribe(account); err != nil {
			c.mtx.Unlock()
			return fmt.Errorf("subscribe %s: %w", account, err)
		}
	}
	c.mtx.Unlock()

	go c.pingLoop(conn, done)

	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if err := conn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
			return err
		}
		c.handle(frame)
	}
}

// handle dispatches one inbound frame: either a change notification for a
// watched account, or the reply to one of our own subscribe requests.
func (c *wsClient) handle(frame []byte) {
	var msg struct {
		ID     uint64          `json:"id"`
		Result json.RawMessage `json:"result"`
		Method string          `json:"method"`
		Params *struct {
			Subscription uint64 `json:"subscription"`
		} `json:"params"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		c.log.Debugf("solana websocket: undecodable frame: %v", err)
		return
	}

	if msg.Method == accountNotification {
		if msg.Params == nil {
			return
		}
		// Resolve and release before calling out: the callback is the wallet's,
		// and holding mtx across it would let a callback that touched this
		// client deadlock the read loop.
		c.mtx.Lock()
		onChange := c.watched[c.subAcct[msg.Params.Subscription]]
		c.mtx.Unlock()
		if onChange != nil {
			onChange()
		}
		return
	}

	if msg.ID == 0 {
		return
	}

	c.mtx.Lock()
	defer c.mtx.Unlock()
	account, ours := c.reqAcct[msg.ID]
	if !ours {
		return
	}
	delete(c.reqAcct, msg.ID)

	// subscribeFailed retires the connection so run redials and re-subscribes
	// from scratch. Nothing else would ever retry: watch subscribes only at
	// registration and session only at connect, and ping/pong keeps an
	// otherwise healthy link up indefinitely, so one unanswered subscribe would
	// hold live false for the life of the connection. Not just for its own
	// account either — live is all-or-nothing, so a token wallet's rejected ATA
	// would drag the native wallet back to unsubscribed polling too, which is
	// the entire cost this client exists to avoid. An endpoint that rejects
	// every time costs redials on run's growing backoff and nothing else; the
	// wallet polls at its unsubscribed cadence throughout either way.
	subscribeFailed := func(format string, args ...any) {
		c.log.Warnf(format, args...)
		if _, stillWatched := c.watched[account]; !stillWatched {
			// Unwatched while the subscribe was in flight, so nobody is waiting
			// on it and a redial has nothing to fix. Dropping the connection
			// here would only interrupt the accounts that did subscribe.
			// refreshLive already counts this one out.
			return
		}
		if c.conn != nil {
			c.conn.Close()
		}
	}

	if msg.Error != nil {
		subscribeFailed("solana websocket: %s rejected for %s: %s (%d)",
			accountSubscribe, account, msg.Error.Message, msg.Error.Code)
		return
	}
	var subID uint64
	if err := json.Unmarshal(msg.Result, &subID); err != nil {
		subscribeFailed("solana websocket: undecodable subscription id for %s: %v", account, err)
		return
	}
	if subID == 0 {
		// A null result decodes into zero without error, so the check above does
		// not catch it. Solana numbers subscriptions from one, and zero is also
		// what an accountNotification with no subscription field decodes to, so
		// recording it would route those malformed frames to this account.
		subscribeFailed("solana websocket: no subscription id in the %s reply for %s",
			accountSubscribe, account)
		return
	}
	if _, stillWatched := c.watched[account]; !stillWatched {
		// Unwatched while this subscribe was in flight. Recording it would put
		// acctSub permanently out of step with watched, which refreshLive reads
		// as a half-subscribed connection: live then either sticks false for the
		// rest of the session, undoing the whole point of subscribing, or worse,
		// matches by count while the account the wallet actually cares about is
		// the one missing. This reply is also the first and only sight of the
		// subscription id, so it is the one chance to release it server side.
		if c.conn != nil {
			if err := c.sendUnsubscribe(subID); err != nil {
				c.log.Debugf("solana websocket: unsubscribe unwatched %s: %v", account, err)
			}
		}
		return
	}
	c.subAcct[subID] = account
	c.acctSub[account] = subID
	c.refreshLive()
}

// pingLoop keeps the connection warm and, more to the point, detects a link
// that has gone away without a close frame: the peer stops answering, the read
// deadline expires, and the session ends so polling speeds back up.
func (c *wsClient) pingLoop(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			c.mtx.Lock()
			var err error
			// Only ping while this is still the live connection, so a ping for
			// a retired session cannot land on its replacement.
			if c.conn == conn {
				if err = conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err == nil {
					err = conn.WriteMessage(websocket.PingMessage, nil)
				}
			}
			c.mtx.Unlock()
			if err != nil {
				// End the session now rather than waiting out wsPongWait.
				conn.Close()
				return
			}
		}
	}
}

// sendSubscribe writes one accountSubscribe and records the request so the
// reply can be matched back to the account. Caller must hold mtx and have
// checked that conn is non-nil.
func (c *wsClient) sendSubscribe(account string) error {
	c.nextID++
	c.reqAcct[c.nextID] = account
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID,
		"method":  accountSubscribe,
		"params": []any{account, map[string]any{
			"commitment": "confirmed",
			// The payload is discarded, so ask for the cheapest encoding
			// rather than jsonParsed. Only the fact of the change matters.
			"encoding": "base64",
		}},
	})
}

// sendUnsubscribe writes one accountUnsubscribe. The reply is ignored: nothing
// downstream depends on it, and the subscription state it would confirm is
// already gone. Caller must hold mtx and have checked that conn is non-nil.
func (c *wsClient) sendUnsubscribe(subID uint64) error {
	c.nextID++
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID,
		"method":  accountUnsubscribe,
		"params":  []any{subID},
	})
}

// subscribePending reports whether a subscribe for account is still awaiting
// its reply. Caller must hold mtx.
func (c *wsClient) subscribePending(account string) bool {
	for _, acct := range c.reqAcct {
		if acct == account {
			return true
		}
	}
	return false
}

// write sends one JSON frame. Caller must hold mtx and have checked conn.
func (c *wsClient) write(v any) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
		return err
	}
	return c.conn.WriteJSON(v)
}

// refreshLive recomputes live from subscription state. Caller must hold mtx.
func (c *wsClient) refreshLive() {
	c.live.Store(c.conn != nil && len(c.watched) > 0 && len(c.acctSub) == len(c.watched))
}

// signalRefresh delivers a non-blocking wake on a coalescing channel. The
// channel holds one pending wake, so a burst of notifications collapses into a
// single refresh instead of queueing one apiece.
func signalRefresh(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
