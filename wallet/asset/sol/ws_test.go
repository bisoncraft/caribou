// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/gorilla/websocket"
)

// testWSServer is a minimal stand-in for a Solana websocket endpoint: it
// answers accountSubscribe and accountUnsubscribe, reports what it was asked
// for, and can push notifications on demand.
type testWSServer struct {
	t   *testing.T
	srv *httptest.Server
	url string

	reqd     chan string // account, as each subscribe is read off the wire
	subbed   chan string // account, as each subscribe is answered
	unsubbed chan uint64 // server subscription id, as each unsubscribe is answered
	accepted chan struct{}

	badSubbed chan string // account, as each subscribe is answered badly

	mtx      sync.Mutex
	conn     *websocket.Conn
	nextSub  uint64
	subIDs   map[string]uint64 // account -> subscription id on the live conn
	holdSubs chan struct{}     // gates subscribe replies; see holdSubscribes

	badSubs    int  // remaining subscribe replies to answer badly
	badSubNull bool // answer with a null result rather than an error object
}

func newTestWSServer(t *testing.T) *testWSServer {
	s := &testWSServer{
		t: t,
		// Generously buffered so the server never blocks on a test that is not
		// reading, which would stall its read loop and deadlock the client.
		reqd:      make(chan string, 16),
		subbed:    make(chan string, 16),
		unsubbed:  make(chan uint64, 16),
		accepted:  make(chan struct{}, 16),
		badSubbed: make(chan string, 16),
		subIDs:    make(map[string]uint64),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.url = "ws" + strings.TrimPrefix(s.srv.URL, "http")
	t.Cleanup(s.srv.Close)
	return s
}

func (s *testWSServer) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.mtx.Lock()
	s.conn = conn
	// Subscription ids are per-connection, so a reconnect starts clean. The
	// client must re-subscribe and re-learn the ids.
	s.subIDs = make(map[string]uint64)
	s.mtx.Unlock()
	select {
	case s.accepted <- struct{}{}:
	default:
	}
	defer conn.Close()

	// Answer control pings so the client's read deadline never expires.
	conn.SetPingHandler(func(string) error {
		s.mtx.Lock()
		defer s.mtx.Unlock()
		return conn.WriteMessage(websocket.PongMessage, nil)
	})

	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			ID     uint64            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &req); err != nil {
			s.t.Errorf("server: undecodable request: %v", err)
			continue
		}
		switch req.Method {
		case accountSubscribe:
			var account string
			if len(req.Params) == 0 {
				s.t.Errorf("server: %s with no params", accountSubscribe)
				continue
			}
			if err := json.Unmarshal(req.Params[0], &account); err != nil {
				s.t.Errorf("server: undecodable account: %v", err)
				continue
			}
			s.reqd <- account
			s.mtx.Lock()
			hold := s.holdSubs
			s.mtx.Unlock()
			if hold != nil {
				// Stall the reply so the test can act while the subscribe is
				// still in flight. Blocking the read loop is fine: nothing else
				// arrives until the reply does, and pings are answered on it
				// well inside the client's read deadline.
				<-hold
			}
			s.mtx.Lock()
			if s.badSubs > 0 {
				s.badSubs--
				reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
				if s.badSubNull {
					reply["result"] = nil
				} else {
					reply["error"] = map[string]any{"code": -32602, "message": "invalid account"}
				}
				err := conn.WriteJSON(reply)
				s.mtx.Unlock()
				if err != nil {
					return
				}
				s.badSubbed <- account
				continue
			}
			s.nextSub++
			subID := s.nextSub
			s.subIDs[account] = subID
			err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": subID})
			s.mtx.Unlock()
			if err != nil {
				return
			}
			s.subbed <- account
		case accountUnsubscribe:
			var subID uint64
			if len(req.Params) > 0 {
				json.Unmarshal(req.Params[0], &subID)
			}
			s.mtx.Lock()
			err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": true})
			s.mtx.Unlock()
			if err != nil {
				return
			}
			s.unsubbed <- subID
		}
	}
}

// notify pushes an accountNotification for account. It reports false if the
// account is not currently subscribed.
func (s *testWSServer) notify(account string) bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	subID, ok := s.subIDs[account]
	if !ok || s.conn == nil {
		return false
	}
	err := s.conn.WriteJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  accountNotification,
		"params": map[string]any{
			"subscription": subID,
			"result":       map[string]any{"context": map[string]any{"slot": 1}},
		},
	})
	if err != nil {
		s.t.Errorf("notify: %v", err)
		return false
	}
	return true
}

// holdSubscribes makes every accountSubscribe reply wait for a send on the
// returned channel, so a test can hold one in flight.
func (s *testWSServer) holdSubscribes() chan<- struct{} {
	hold := make(chan struct{})
	s.mtx.Lock()
	s.holdSubs = hold
	s.mtx.Unlock()
	return hold
}

// failSubscribes makes the next n accountSubscribe replies unusable: an error
// object, or a null subscription id when null is set. Later subscribes are
// answered normally, so a test can watch the client recover.
func (s *testWSServer) failSubscribes(n int, null bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.badSubs = n
	s.badSubNull = null
}

// dropConn kills the live connection from the server side, so the client sees
// its session end and reconnects.
func (s *testWSServer) dropConn() {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.conn != nil {
		s.conn.Close()
	}
}

func recvString(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return ""
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestWSClientNotifies covers the mainline: an account watched before the
// client is ever run gets subscribed on connect, live flips once the server
// confirms, and a pushed notification reaches the callback.
func TestWSClientNotifies(t *testing.T) {
	srv := newTestWSServer(t)
	c := newWSClient(srv.url, util.Disabled)

	const account = "watched-account"
	changed := make(chan struct{}, 8)
	c.watch(account, func() { signalRefresh(changed) })

	// Nothing is subscribed before run, so nothing is live.
	if c.live.Load() {
		t.Fatal("live before run")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.run(ctx) }()

	if got := recvString(t, srv.subbed, "subscribe"); got != account {
		t.Fatalf("subscribed to %q, want %q", got, account)
	}
	waitFor(t, "live", func() bool { return c.live.Load() })

	if !srv.notify(account) {
		t.Fatal("server had no subscription to notify")
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for change callback")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return on context cancellation")
	}
	if c.live.Load() {
		t.Fatal("still live after shutdown")
	}
}

// TestWSClientResubscribes covers the reconnect path, which is what makes the
// registration durable: after the server drops the connection the client must
// dial again and re-subscribe every watched account under fresh ids, since
// subscription ids do not survive a connection.
func TestWSClientResubscribes(t *testing.T) {
	srv := newTestWSServer(t)
	c := newWSClient(srv.url, util.Disabled)

	const account = "watched-account"
	changed := make(chan struct{}, 8)
	c.watch(account, func() { signalRefresh(changed) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)

	recvString(t, srv.subbed, "first subscribe")
	waitFor(t, "live", func() bool { return c.live.Load() })

	srv.dropConn()
	// Liveness must drop with the connection: that is the wallet's cue to
	// resume polling at the unsubscribed cadence while the link is down.
	waitFor(t, "live to clear", func() bool { return !c.live.Load() })

	if got := recvString(t, srv.subbed, "re-subscribe"); got != account {
		t.Fatalf("re-subscribed to %q, want %q", got, account)
	}
	waitFor(t, "live again", func() bool { return c.live.Load() })

	// The re-subscription must be routable, not merely sent: the new session's
	// subscription id has to be mapped to the account or notifications land
	// nowhere.
	if !srv.notify(account) {
		t.Fatal("server had no subscription to notify after reconnect")
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for change callback after reconnect")
	}
}

// TestWSClientLiveRequiresAllAccounts pins the strictness of live: a second
// watched account leaves it false until that account is confirmed too, so a
// token wallet whose subscription has not landed keeps polling briskly.
func TestWSClientLiveRequiresAllAccounts(t *testing.T) {
	srv := newTestWSServer(t)
	c := newWSClient(srv.url, util.Disabled)

	const native, ata = "native-account", "token-account"
	c.watch(native, func() {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)

	recvString(t, srv.subbed, "native subscribe")
	waitFor(t, "live", func() bool { return c.live.Load() })

	// Registering a second account immediately un-lives the client, since one
	// of two watched accounts is now unconfirmed. Hold the reply so the
	// assertion below can't race the server's round trip: without this, a
	// slow scheduler tick can let the subscribe reply land and re-live the
	// client before c.live.Load() is checked.
	hold := srv.holdSubscribes()
	c.watch(ata, func() {})
	if c.live.Load() {
		t.Fatal("live with an unconfirmed subscription")
	}
	hold <- struct{}{}
	recvString(t, srv.subbed, "token subscribe")
	waitFor(t, "live with both", func() bool { return c.live.Load() })

	// Dropping the token wallet releases the server subscription and leaves the
	// remaining account live on its own.
	c.unwatch(ata)
	select {
	case <-srv.unsubbed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for unsubscribe")
	}
	waitFor(t, "live after unwatch", func() bool { return c.live.Load() })
}

// TestWSClientUnwatchDuringSubscribe covers a token wallet disconnecting while
// its subscribe is still in flight. The reply carries the only copy of the
// subscription id, so it is the one chance to release it server side, and it
// must not be recorded: an acctSub entry for an account no longer watched puts
// the two counts permanently out of step, which refreshLive reads as a
// half-subscribed connection for the rest of the session.
func TestWSClientUnwatchDuringSubscribe(t *testing.T) {
	srv := newTestWSServer(t)
	hold := srv.holdSubscribes()
	c := newWSClient(srv.url, util.Disabled)

	const native, ata = "native-account", "token-account"
	c.watch(native, func() {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)

	recvString(t, srv.reqd, "native subscribe request")
	hold <- struct{}{}
	recvString(t, srv.subbed, "native subscribe")
	waitFor(t, "live", func() bool { return c.live.Load() })

	// Register the token account, then drop it before the server answers.
	c.watch(ata, func() {})
	recvString(t, srv.reqd, "token subscribe request")
	c.unwatch(ata)
	hold <- struct{}{}
	recvString(t, srv.subbed, "token subscribe")

	select {
	case <-srv.unsubbed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the orphaned subscription to be released")
	}

	// The native account is the only one watched and the only one subscribed,
	// so the client stays live. It would not if the orphan had landed.
	waitFor(t, "live after the orphaned reply", func() bool { return c.live.Load() })
	c.mtx.Lock()
	subs, watched := len(c.acctSub), len(c.watched)
	c.mtx.Unlock()
	if subs != 1 || watched != 1 {
		t.Fatalf("acctSub holds %d, watched holds %d, want 1 and 1", subs, watched)
	}
}

// TestWSClientRewatchDuringSubscribe is the same race resolved the other way:
// the account is watched again before its first reply lands. The client must
// not send a second subscribe, since only the later id would reach acctSub and
// the earlier subscription could then never be released; the in-flight reply
// covers the new registration.
func TestWSClientRewatchDuringSubscribe(t *testing.T) {
	srv := newTestWSServer(t)
	hold := srv.holdSubscribes()
	c := newWSClient(srv.url, util.Disabled)

	const account = "token-account"
	c.watch(account, func() {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)

	recvString(t, srv.reqd, "subscribe request")
	c.unwatch(account)
	c.watch(account, func() {})
	hold <- struct{}{}
	recvString(t, srv.subbed, "subscribe")

	waitFor(t, "live on the in-flight reply", func() bool { return c.live.Load() })

	// A second subscribe would have shown up as another request by now, and the
	// notification would be routed by an id nothing released.
	select {
	case acct := <-srv.reqd:
		t.Fatalf("client sent a redundant subscribe for %q", acct)
	case <-time.After(200 * time.Millisecond):
	}
	if !srv.notify(account) {
		t.Fatal("server had no subscription to notify")
	}
}

// TestWSClientRedialsAfterBadSubscribeReply covers the two ways a subscribe can
// come back without leaving the account subscribed: an error object, and a null
// subscription id, which decodes to zero without an unmarshal error. Nothing
// retries a subscribe on its own — watch sends one only at registration and
// session only at connect, and ping/pong holds an otherwise healthy link up
// indefinitely — so the client has to retire the connection and let run redial.
// Without that, live would stay false for the rest of the session, and because
// live is all-or-nothing, one rejected token account would drag the native
// wallet back to unsubscribed polling with it.
func TestWSClientRedialsAfterBadSubscribeReply(t *testing.T) {
	for _, tt := range []struct {
		name string
		null bool
	}{{
		name: "error reply",
	}, {
		name: "null subscription id",
		null: true,
	}} {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestWSServer(t)
			srv.failSubscribes(1, tt.null)
			c := newWSClient(srv.url, util.Disabled)

			const account = "watched-account"
			changed := make(chan struct{}, 8)
			c.watch(account, func() { signalRefresh(changed) })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go c.run(ctx)

			if got := recvString(t, srv.badSubbed, "bad subscribe reply"); got != account {
				t.Fatalf("answered badly for %q, want %q", got, account)
			}
			// The account is not subscribed, so nothing may report otherwise.
			if c.live.Load() {
				t.Fatal("live on a subscribe that never landed")
			}

			// The redial is the whole point: a fresh connection, a fresh
			// subscribe, and this time the server answers it properly.
			if got := recvString(t, srv.subbed, "subscribe after redial"); got != account {
				t.Fatalf("re-subscribed to %q, want %q", got, account)
			}
			waitFor(t, "live after redial", func() bool { return c.live.Load() })

			// Routable, not merely sent: the second session's id has to be
			// mapped to the account or notifications land nowhere.
			if !srv.notify(account) {
				t.Fatal("server had no subscription to notify after redial")
			}
			select {
			case <-changed:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for change callback after redial")
			}
		})
	}
}

// TestWSClientKeepsConnectionOnUnwatchedRejection is the limit of the teardown
// above: a rejected subscribe for an account nobody watches any more must leave
// the connection alone. Nothing is waiting on it, so a redial has nothing to
// fix, and dropping the link would interrupt the accounts that did subscribe.
func TestWSClientKeepsConnectionOnUnwatchedRejection(t *testing.T) {
	srv := newTestWSServer(t)
	hold := srv.holdSubscribes()
	c := newWSClient(srv.url, util.Disabled)

	const native, ata = "native-account", "token-account"
	changed := make(chan struct{}, 8)
	c.watch(native, func() { signalRefresh(changed) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)

	recvString(t, srv.reqd, "native subscribe request")
	hold <- struct{}{}
	recvString(t, srv.subbed, "native subscribe")
	waitFor(t, "live", func() bool { return c.live.Load() })
	<-srv.accepted // the only connection so far

	// Register the token account and drop it again, then let the server reject
	// the subscribe that is still in flight for it.
	srv.failSubscribes(1, false)
	c.watch(ata, func() {})
	recvString(t, srv.reqd, "token subscribe request")
	c.unwatch(ata)
	hold <- struct{}{}
	if got := recvString(t, srv.badSubbed, "rejected token subscribe"); got != ata {
		t.Fatalf("rejected %q, want %q", got, ata)
	}

	// No redial: a second upgrade would show up on accepted well inside this
	// window, since run's first backoff is wsReconnectMin.
	select {
	case <-srv.accepted:
		t.Fatal("client redialed over a rejection nobody was waiting on")
	case <-time.After(2 * wsReconnectMin):
	}

	// And the native account's subscription survived it, still live and still
	// routing notifications on the original connection.
	if !c.live.Load() {
		t.Fatal("live cleared by a rejection for an unwatched account")
	}
	if !srv.notify(native) {
		t.Fatal("server had no subscription to notify")
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for change callback")
	}
}

// TestWSClientUnreachable checks the case the wallet leans on most: an endpoint
// that never answers must leave live false rather than block or panic, so the
// poll loop stays at its unsubscribed cadence.
func TestWSClientUnreachable(t *testing.T) {
	srv := newTestWSServer(t)
	url := srv.url
	srv.srv.Close()

	c := newWSClient(url, util.Disabled)
	c.watch("account", func() {})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.run(ctx) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return for an unreachable endpoint")
	}
	if c.live.Load() {
		t.Fatal("live against an unreachable endpoint")
	}
}

// TestSignalRefreshCoalesces confirms the wake channel collapses a burst, which
// is what keeps a busy account from queueing one refreshState per notification.
func TestSignalRefreshCoalesces(t *testing.T) {
	ch := make(chan struct{}, 1)
	for range 10 {
		signalRefresh(ch)
	}
	if len(ch) != 1 {
		t.Fatalf("channel holds %d wakes, want 1", len(ch))
	}
	<-ch
	select {
	case <-ch:
		t.Fatal("more than one wake queued")
	default:
	}
}

// TestRefreshInterval pins when the loop is allowed to relax its cadence. The
// case that matters is the last one: refreshState stops at its first error, so
// a failed pass leaves tipTime untouched, and holding the subscribed cadence
// would put the retry exactly at the MaxBlockInterval mark SyncStatus reads as
// unsynced. One dropped getSlot must not report the wallet out of sync.
func TestRefreshInterval(t *testing.T) {
	live := &wsClient{}
	live.live.Store(true)
	down := &wsClient{}

	// The relationship the failure case exists to protect. If these constants
	// ever move, the reasoning above has to be revisited, not just the numbers.
	if 2*subscribedPollInterval < time.Duration(MaxBlockInterval)*time.Second {
		t.Fatalf("two subscribed intervals (%v) no longer reach MaxBlockInterval (%ds); "+
			"re-derive the cadence", 2*subscribedPollInterval, MaxBlockInterval)
	}

	tests := []struct {
		name       string
		ws         *wsClient
		lastFailed bool
		want       time.Duration
	}{
		{"no subscription", nil, false, pollInterval},
		{"subscription down", down, false, pollInterval},
		{"subscription live", live, false, subscribedPollInterval},
		{"live but the last refresh failed", live, true, pollInterval},
		{"down and the last refresh failed", down, true, pollInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refreshInterval(tt.ws, tt.lastFailed); got != tt.want {
				t.Fatalf("refreshInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}

// runRefreshLoop starts refreshLoop in the background and returns a stop
// function that cancels it and waits for it to return, so a leaked loop shows up
// as a test failure rather than as noise in a later test.
func runRefreshLoop(t *testing.T, wake <-chan struct{}, minGap time.Duration,
	interval func(bool) time.Duration, pass func(context.Context) error) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshLoop(ctx, time.Now(), wake, minGap, interval, pass)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("refreshLoop did not return on context cancellation")
		}
	}
}

// TestRefreshLoopWakes covers the notification path: a wake runs a pass without
// waiting out the interval, and the loop goes back to waiting afterwards rather
// than falling out or spinning. The interval is an hour, so only a wake can
// drive this at all.
func TestRefreshLoopWakes(t *testing.T) {
	wake := make(chan struct{}, 1)
	passes := make(chan struct{}, 4)
	stop := runRefreshLoop(t, wake, 0,
		func(bool) time.Duration { return time.Hour },
		func(context.Context) error {
			passes <- struct{}{}
			return nil
		})
	defer stop()

	for i := range 2 {
		signalRefresh(wake)
		select {
		case <-passes:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for pass %d", i+1)
		}
	}
	// Nothing else may run: the hour has not elapsed and no wake is pending.
	select {
	case <-passes:
		t.Fatal("loop ran a pass with no wake and no elapsed interval")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestRefreshLoopWakeSpacing covers the floor the wake path is held to. A
// notification that lands while a pass is running cannot be coalesced away — the
// loop has already drained the channel — so without a floor it would run the
// next pass with no gap at all, and a steadily notified account would keep doing
// that indefinitely. The floor must not cost anything in the ordinary case: a
// wake arriving with the loop idle still runs at once.
func TestRefreshLoopWakeSpacing(t *testing.T) {
	const minGap = 300 * time.Millisecond
	wake := make(chan struct{}, 1)
	starts := make(chan time.Time, 4)
	var passNum int
	stop := runRefreshLoop(t, wake, minGap,
		// An hour, so only a wake can drive this.
		func(bool) time.Duration { return time.Hour },
		func(context.Context) error {
			starts <- time.Now()
			passNum++
			if passNum == 1 {
				signalRefresh(wake)
				time.Sleep(20 * time.Millisecond)
			}
			return nil
		})
	defer stop()

	nextStart := func(within time.Duration, what string) time.Time {
		t.Helper()
		select {
		case s := <-starts:
			return s
		case <-time.After(within):
			t.Fatalf("timed out waiting for %s", what)
			return time.Time{}
		}
	}

	signalRefresh(wake)
	first := nextStart(5*time.Second, "the first pass")
	second := nextStart(5*time.Second, "the pass the mid-pass wake asked for")
	if gap := second.Sub(first); gap < minGap {
		t.Fatalf("passes started %v apart, want at least %v; a wake taken during "+
			"a pass is running the next one back to back", gap, minGap)
	}
	select {
	case <-starts:
		t.Fatal("loop ran a pass with no wake and no elapsed interval")
	case <-time.After(100 * time.Millisecond):
	}

	// Idle for longer than the floor, so the next wake owes it nothing.
	time.Sleep(minGap)
	signaled := time.Now()
	signalRefresh(wake)
	if delay := nextStart(5*time.Second, "the idle wake").Sub(signaled); delay >= minGap {
		t.Fatalf("wake took %v to run with the loop idle, want immediate; the "+
			"floor is being charged from the wake rather than from the last pass", delay)
	}
}

// TestRefreshLoopTimesFromPassStart is the property the tip age depends on. Each
// pass here runs longer than the interval, so timing from the start means the
// deadline is always already past and passes run back to back, while timing from
// the end would add an interval of idle to every cycle.
func TestRefreshLoopTimesFromPassStart(t *testing.T) {
	const (
		interval = 200 * time.Millisecond
		passTime = 300 * time.Millisecond
		passes   = 3
	)
	done := make(chan struct{}, passes)
	start := time.Now()
	stop := runRefreshLoop(t, nil, 0,
		func(bool) time.Duration { return interval },
		func(context.Context) error {
			time.Sleep(passTime)
			select {
			case done <- struct{}{}:
			default:
			}
			return nil
		})
	defer stop()

	for i := range passes {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for pass %d", i+1)
		}
	}
	elapsed := time.Since(start)
	// Start-timed is passes*passTime, end-timed would be
	// passes*(passTime+interval). The threshold sits between them with room for
	// scheduling jitter on either side.
	if limit := passes * (passTime + interval/2); elapsed >= limit {
		t.Fatalf("%d passes took %v, want under %v; the interval looks to be "+
			"timed from the end of a pass rather than its start", passes, elapsed, limit)
	}
}

// TestRefreshLoopReportsFailure checks the wiring the SyncStatus margin rests
// on: whatever a pass returns has to reach the next interval decision, or
// refreshInterval never sees a failure to back off from.
func TestRefreshLoopReportsFailure(t *testing.T) {
	const want = 3
	seen := make(chan bool, want)
	var passNum int
	stop := runRefreshLoop(t, nil, 0,
		func(lastFailed bool) time.Duration {
			select {
			case seen <- lastFailed:
			default:
			}
			return time.Millisecond
		},
		func(context.Context) error {
			// Only the first pass fails, so the report has to both arrive and
			// clear again.
			passNum++
			if passNum == 1 {
				return errors.New("pass failed")
			}
			return nil
		})
	defer stop()

	got := make([]bool, 0, want)
	for len(got) < want {
		select {
		case v := <-seen:
			got = append(got, v)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out with %v", got)
		}
	}
	if wantSeq := []bool{false, true, false}; !slices.Equal(got, wantSeq) {
		t.Fatalf("interval was told %v, want %v (nothing yet, then the failed "+
			"pass, then recovered)", got, wantSeq)
	}
}

// TestSubscriptionEndpoint pins which configurations get a subscription. A
// custom RPC provider with no websocket provider must not silently pair the
// user's own node with a public endpoint.
func TestSubscriptionEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]string
		want     string
	}{{
		name:     "defaults subscribe to the default endpoint",
		settings: map[string]string{},
		want:     DefaultWSEndpoints[util.Mainnet],
	}, {
		name:     "explicit ws provider is honored",
		settings: map[string]string{"wsprovider": "wss://example.com"},
		want:     "wss://example.com",
	}, {
		name:     "explicit ws provider is honored alongside a custom rpc provider",
		settings: map[string]string{"rpcprovider": "https://node.example.com", "wsprovider": "wss://node.example.com"},
		want:     "wss://node.example.com",
	}, {
		name:     "custom rpc provider alone gets no subscription",
		settings: map[string]string{"rpcprovider": "https://node.example.com"},
		want:     "",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Wallet{net: util.Mainnet, settings: tt.settings}
			if got := w.subscriptionEndpoint(); got != tt.want {
				t.Fatalf("subscriptionEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSubscribeStale covers the aging rule on its own. The unwatched case is
// the one worth pinning: unwatch deliberately leaves an in-flight subscribe in
// reqAcct so a late reply can still be recognized and released, so those
// entries never get a reply removing them on the wallet's behalf and would
// otherwise age forever and retire a connection nobody needed retired.
func TestSubscribeStale(t *testing.T) {
	const account = "watched-account"
	now := time.Now()
	fresh := now.Add(-wsSubscribeTimeout / 2)
	old := now.Add(-wsSubscribeTimeout - time.Second)

	for _, tt := range []struct {
		name    string
		watched bool
		sent    time.Time
		pending bool
		want    bool
	}{{
		name:    "nothing in flight",
		watched: true,
	}, {
		name:    "in flight but young",
		watched: true,
		sent:    fresh,
		pending: true,
	}, {
		name:    "in flight and overdue",
		watched: true,
		sent:    old,
		pending: true,
		want:    true,
	}, {
		name:    "overdue but unwatched",
		sent:    old,
		pending: true,
	}, {
		name:    "exactly at the deadline",
		watched: true,
		sent:    now.Add(-wsSubscribeTimeout),
		pending: true,
		want:    true,
	}} {
		t.Run(tt.name, func(t *testing.T) {
			c := newWSClient("", util.Disabled)
			if tt.watched {
				c.watched[account] = func() {}
			}
			if tt.pending {
				c.reqAcct[1] = pendingSub{account: account, sent: tt.sent}
			}
			if got := c.subscribeStale(now); got != tt.want {
				t.Fatalf("subscribeStale = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWSClientRetiresOnUnansweredSubscribe covers the failure the rejection
// path above cannot see: a server that accepts a subscribe and simply never
// answers it, which is the usual shape of RPC rate limiting. Nothing on the
// connection notices. Ping/pong keeps reporting a healthy link, no reply
// arrives to run the subscribeFailed teardown, and refreshLive holds live false
// for as long as the connection lasts — so the wallet stays on the 2s
// unsubscribed poll, with the socket open, indefinitely. The ping tick has to
// age the request out and retire the connection itself.
func TestWSClientRetiresOnUnansweredSubscribe(t *testing.T) {
	srv := newTestWSServer(t)
	hold := srv.holdSubscribes()
	c := newWSClient(srv.url, util.Disabled)

	const account = "watched-account"
	changed := make(chan struct{}, 8)
	c.watch(account, func() { signalRefresh(changed) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)

	// The server has the subscribe and is sitting on it, so the client is
	// waiting on a reply that this test never sends.
	if got := recvString(t, srv.reqd, "subscribe request"); got != account {
		t.Fatalf("subscribe request for %q, want %q", got, account)
	}
	waitFor(t, "the subscribe to be recorded in flight", func() bool {
		c.mtx.Lock()
		defer c.mtx.Unlock()
		return c.conn != nil && len(c.reqAcct) == 1
	})

	c.mtx.Lock()
	conn := c.conn
	c.mtx.Unlock()

	// Slow is not lost. A subscribe still inside its window must ping as
	// normal, or every connect would retire itself before the first reply.
	if c.pingTick(conn) {
		t.Fatal("retired a connection whose subscribe is still within wsSubscribeTimeout")
	}
	if c.live.Load() {
		t.Fatal("live with a subscribe still in flight")
	}

	// Age it past the deadline. Reaching into reqAcct rather than waiting out
	// wsSubscribeTimeout keeps the test to the aging rule and off the clock.
	c.mtx.Lock()
	for id, pending := range c.reqAcct {
		pending.sent = pending.sent.Add(-wsSubscribeTimeout - time.Second)
		c.reqAcct[id] = pending
	}
	c.mtx.Unlock()

	if !c.pingTick(conn) {
		t.Fatal("an unanswered subscribe past wsSubscribeTimeout did not retire the connection")
	}
	// What pingLoop does with that verdict.
	conn.Close()

	// Release the abandoned subscribe now, while the retired handler is the
	// only reader: its reply lands on a closed connection and goes nowhere.
	// Doing it after the redial would race the two handlers over the server's
	// subscription-id map.
	hold <- struct{}{}

	// The redial is the point of retiring: a fresh connection, a fresh
	// subscribe, and this time an answer.
	if got := recvString(t, srv.reqd, "subscribe request after redial"); got != account {
		t.Fatalf("re-subscribed to %q, want %q", got, account)
	}
	hold <- struct{}{}
	if got := recvString(t, srv.subbed, "subscribe after redial"); got != account {
		t.Fatalf("subscribed to %q after redial, want %q", got, account)
	}
	waitFor(t, "live after redial", func() bool { return c.live.Load() })

	// Routable, not merely sent: the second session's id has to be mapped to
	// the account or notifications land nowhere.
	if !srv.notify(account) {
		t.Fatal("server had no subscription to notify after redial")
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for change callback after redial")
	}
}
