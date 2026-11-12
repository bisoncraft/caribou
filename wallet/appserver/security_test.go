//go:build !live

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util/encrypt"
	"github.com/gorilla/websocket"
)

// errWrongPass is how core reports a wrong app password.
var errWrongPass = fmt.Errorf("outer key deserialization error: %w", encrypt.ErrIncorrectPassword)

func TestCapabilityMiddleware(t *testing.T) {
	const token = "00112233445566778899aabbccddeeff"

	run := func(s *AppServer, req *http.Request) (*httptest.ResponseRecorder, bool) {
		t.Helper()
		nextCalled := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nextCalled = true
			w.WriteHeader(http.StatusOK)
		})
		rec := httptest.NewRecorder()
		s.capabilityMiddleware(next).ServeHTTP(rec, req)
		return rec, nextCalled
	}

	// Gate disabled (desktop): every request passes through.
	t.Run("disabled passthrough", func(t *testing.T) {
		s := &AppServer{} // empty capToken
		_, called := run(s, httptest.NewRequest(http.MethodGet, "/api/login", nil))
		if !called {
			t.Fatal("request blocked with no capability token configured")
		}
	})

	s := &AppServer{capToken: token}

	// No cookie and no token: blocked, next never runs.
	t.Run("blocks unauthorized", func(t *testing.T) {
		rec, called := run(s, httptest.NewRequest(http.MethodPost, "/api/login", nil))
		if called {
			t.Fatal("blocked request reached the handler")
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", rec.Code)
		}
	})

	// Valid capability cookie: allowed.
	t.Run("allows valid cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
		req.AddCookie(&http.Cookie{Name: capCK, Value: token})
		if _, called := run(s, req); !called {
			t.Fatal("valid cookie was rejected")
		}
	})

	// Reattach: a valid cookie but the URL still carries ?k. Strip it rather
	// than serving the page with the token in location.search.
	t.Run("strips token on reattach with cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?k="+token+"&net=simnet", nil)
		req.AddCookie(&http.Cookie{Name: capCK, Value: token})
		rec, called := run(s, req)
		if called {
			t.Fatal("served the handler instead of stripping the token")
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("want 302, got %d", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/?net=simnet" {
			t.Fatalf("token not stripped from redirect: %q", loc)
		}
	})

	// Wrong cookie value: blocked.
	t.Run("rejects wrong cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/login", nil)
		req.AddCookie(&http.Cookie{Name: capCK, Value: "deadbeef"})
		if _, called := run(s, req); called {
			t.Fatal("wrong cookie was accepted")
		}
	})

	// Bootstrap GET with the token: sets the cookie and redirects with k
	// stripped, without serving the underlying handler.
	t.Run("bootstrap grants and scrubs", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?k="+token+"&net=simnet", nil)
		rec, called := run(s, req)
		if called {
			t.Fatal("bootstrap served the handler instead of redirecting")
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("want 302, got %d", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/?net=simnet" {
			t.Fatalf("token not stripped from redirect: %q", loc)
		}
		var got string
		for _, c := range rec.Result().Cookies() {
			if c.Name == capCK {
				got = c.Value
			}
		}
		if got != token {
			t.Fatalf("capability cookie not set, got %q", got)
		}
	})

	// The token only grants via a GET bootstrap, not on a POST.
	t.Run("token does not grant on POST", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/login?k="+token, nil)
		if _, called := run(s, req); called {
			t.Fatal("POST with token in query was accepted")
		}
	})

	// securityMiddleware wraps the gate in production, so its headers must also
	// land on the gate's own rejection responses.
	t.Run("security headers cover gate rejections", func(t *testing.T) {
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
		h := s.securityMiddleware(s.capabilityMiddleware(next))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/login", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", rec.Code)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("security headers missing on gate rejection")
		}
	})
}

func TestGrantMiddleware(t *testing.T) {
	const (
		grant     = "abcdef0123456789"
		authToken = "aatoken"
		pwKey     = "deadbeef"
	)

	run := func(s *AppServer, req *http.Request) (*httptest.ResponseRecorder, bool) {
		t.Helper()
		nextCalled := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nextCalled = true
			w.WriteHeader(http.StatusOK)
		})
		rec := httptest.NewRecorder()
		s.grantMiddleware(next).ServeHTTP(rec, req)
		return rec, nextCalled
	}

	newServer := func() *AppServer {
		return &AppServer{pendingGrants: map[string]*sessionGrant{
			grant: {authToken: authToken, pwKey: pwKey},
		}}
	}

	cookieVal := func(rec *httptest.ResponseRecorder, name string) string {
		for _, c := range rec.Result().Cookies() {
			if c.Name == name {
				return c.Value
			}
		}
		return ""
	}

	// No grant in the query: pass straight through (desktop no-op).
	t.Run("passthrough without grant", func(t *testing.T) {
		s := newServer()
		_, called := run(s, httptest.NewRequest(http.MethodGet, "/?net=simnet", nil))
		if !called {
			t.Fatal("request without a grant was blocked")
		}
	})

	// A valid grant swaps to session cookies, redirects with g stripped (other
	// params preserved), does not serve the handler, and is consumed.
	t.Run("valid grant sets cookies and is consumed", func(t *testing.T) {
		s := newServer()
		req := httptest.NewRequest(http.MethodGet, "/?k=tok&g="+grant+"&net=simnet", nil)
		rec, called := run(s, req)
		if called {
			t.Fatal("served the handler instead of redirecting")
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("want 302, got %d", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/?k=tok&net=simnet" {
			t.Fatalf("grant not stripped from redirect: %q", loc)
		}
		if got := cookieVal(rec, authCK); got != authToken {
			t.Fatalf("dexauth cookie = %q, want %q", got, authToken)
		}
		if got := cookieVal(rec, pwKeyCK); got != pwKey {
			t.Fatalf("sessionkey cookie = %q, want %q", got, pwKey)
		}
		if _, ok := s.pendingGrants[grant]; ok {
			t.Fatal("grant was not consumed")
		}
	})

	// A reused (already-consumed) grant sets no cookies and continues unauthed.
	t.Run("reused grant sets no cookies", func(t *testing.T) {
		s := &AppServer{pendingGrants: map[string]*sessionGrant{}}
		req := httptest.NewRequest(http.MethodGet, "/?g="+grant, nil)
		rec, called := run(s, req)
		if called {
			t.Fatal("served the handler instead of stripping")
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("want 302, got %d", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Fatalf("grant not stripped: %q", loc)
		}
		if got := cookieVal(rec, authCK); got != "" {
			t.Fatalf("unexpected dexauth cookie for unknown grant: %q", got)
		}
	})
}

func TestPasswordThrottle(t *testing.T) {
	var pt passThrottle

	if d := pt.wait(); d != 0 {
		t.Fatalf("fresh throttle already locked: %v", d)
	}

	// The free budget does not lock the account.
	for i := 0; i < passFailFree; i++ {
		pt.fail()
	}
	if d := pt.wait(); d != 0 {
		t.Fatalf("throttled within the free budget: %v", d)
	}

	// One more failure starts the lockout.
	pt.fail()
	if d := pt.wait(); d <= 0 {
		t.Fatal("no lockout after exceeding the free budget")
	}

	// A success clears it.
	pt.succeed()
	if d := pt.wait(); d != 0 {
		t.Fatalf("lockout not cleared after success: %v", d)
	}
}

// TestPasswordRoutesThrottled checks that the routes taking an explicit app
// password all count wrong guesses against the route throttle, and are refused
// while it is locked out, even with the right password. Before, only /login
// was throttled, so /exportseed was a free oracle that returned the seed.
func TestPasswordRoutesThrottled(t *testing.T) {
	post := func(h http.HandlerFunc, body string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		return rec.Body.String()
	}

	routes := []struct {
		name string
		call func(s *AppServer) string
	}{
		{"exportseed", func(s *AppServer) string { return post(s.apiExportSeed, `{"pass":"guess"}`) }},
		{"restorewalletinfo", func(s *AppServer) string { return post(s.apiRestoreWalletInfo, `{"assetID":0,"pass":"guess"}`) }},
		{"changeapppass", func(s *AppServer) string {
			return post(s.apiChangeAppPass, `{"appPW":"guess","newAppPW":"new"}`)
		}},
		{"resolvePass", func(s *AppServer) string {
			_, err := s.resolvePass([]byte("guess"), httptest.NewRequest(http.MethodPost, "/", nil))
			if err == nil {
				return "ok"
			}
			return err.Error()
		}},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			s, tCore, shutdown := newTServer(t, false)
			defer shutdown()
			prevLog := log
			log = tLogger
			t.Cleanup(func() { log = prevLog })

			tCore.verifyPasswordErr = errWrongPass
			for i := 0; i <= passFailFree; i++ {
				if resp := rt.call(s); strings.Contains(resp, "too many") {
					t.Fatalf("throttled after %d wrong guesses: %s", i, resp)
				}
			}
			if d := s.routeThrottle.wait(); d <= 0 {
				t.Fatalf("no lockout after %d wrong guesses", passFailFree+1)
			}

			// The right password is refused while locked out, so the core
			// is never reached.
			tCore.verifyPasswordErr = nil
			resp := rt.call(s)
			if !strings.Contains(resp, "too many") {
				t.Fatalf("right password accepted during lockout: %s", resp)
			}
			if strings.Contains(resp, "seed words here") {
				t.Fatalf("seed returned during lockout: %s", resp)
			}

			// Login has its own throttle, so it is not locked out.
			if resp := post(s.apiLogin, `{"pass":"right"}`); strings.Contains(resp, "too many") {
				t.Fatalf("login throttled by guesses on %s: %s", rt.name, resp)
			}
		})
	}
}

// TestLoginLockoutSparesRoutes checks that wrong guesses at /api/login, which
// is reachable before authentication, lock out login but not the password
// checks of a user who is already logged in.
func TestLoginLockoutSparesRoutes(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()
	prevLog := log
	log = tLogger
	t.Cleanup(func() { log = prevLog })

	login := func(pass string) string {
		rec := httptest.NewRecorder()
		s.apiLogin(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"pass":"`+pass+`"}`)))
		return rec.Body.String()
	}

	tCore.loginErr = errWrongPass
	for i := 0; i <= passFailFree; i++ {
		if resp := login("guess"); strings.Contains(resp, "too many") {
			t.Fatalf("throttled after %d wrong logins: %s", i, resp)
		}
	}
	if resp := login("guess"); !strings.Contains(resp, "too many") {
		t.Fatalf("login not locked out after %d wrong guesses: %s", passFailFree+1, resp)
	}

	tCore.verifyPasswordErr = nil
	if err := s.checkAppPassword([]byte("right")); err != nil {
		t.Fatalf("logged-in password check refused after login guesses: %v", err)
	}
}

// TestLoginOtherErrorsNotCounted checks that a login error other than a wrong
// password is returned but does not count toward the login throttle.
func TestLoginOtherErrorsNotCounted(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()
	prevLog := log
	log = tLogger
	t.Cleanup(func() { log = prevLog })

	tCore.loginErr = errors.New("app not initialized")
	for i := 0; i <= passFailFree; i++ {
		rec := httptest.NewRecorder()
		s.apiLogin(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"pass":"right"}`)))
		if !strings.Contains(rec.Body.String(), "not initialized") {
			t.Fatalf("core error not returned: %s", rec.Body.String())
		}
	}
	if s.loginThrottle.fails != 0 {
		t.Fatalf("%d non-password login errors counted as wrong guesses", s.loginThrottle.fails)
	}
}

// TestPasswordCheckOtherErrorsNotCounted checks that an error other than a
// wrong password, from a route that hands the password to core, is returned
// but does not count toward the throttle.
func TestPasswordCheckOtherErrorsNotCounted(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()
	prevLog := log
	log = tLogger
	t.Cleanup(func() { log = prevLog })

	tCore.verifyPasswordErr = errors.New("db closed")
	for i := 0; i <= passFailFree; i++ {
		rec := httptest.NewRecorder()
		s.apiExportSeed(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"pass":"right"}`)))
		if !strings.Contains(rec.Body.String(), "db closed") {
			t.Fatalf("core error not returned: %s", rec.Body.String())
		}
	}
	if s.routeThrottle.fails != 0 {
		t.Fatalf("%d non-password errors counted as wrong guesses", s.routeThrottle.fails)
	}
}

// TestPasswordThrottleConcurrent checks that concurrent wrong guesses cannot
// all get past the throttle check before any failure is counted.
func TestPasswordThrottleConcurrent(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()
	tCore.verifyPasswordErr = errWrongPass
	tCore.verifyPasswordDelay = 10 * time.Millisecond

	const n = 20
	var wg sync.WaitGroup
	var mtx sync.Mutex
	var reachedCore int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.checkAppPassword([]byte("guess"))
			if errors.Is(err, errBadCredentials) {
				mtx.Lock()
				reachedCore++
				mtx.Unlock()
			}
		}()
	}
	wg.Wait()
	if reachedCore != passFailFree+1 {
		t.Fatalf("%d of %d concurrent guesses were checked, want %d", reachedCore, n, passFailFree+1)
	}
}

// TestResolvePassWrongPassword checks that a wrong explicit password is
// reported as bad credentials, and that a correct one resets the counter.
func TestResolvePassWrongPassword(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	tCore.verifyPasswordErr = errWrongPass
	if _, err := s.resolvePass([]byte("guess"), req); !errors.Is(err, errBadCredentials) {
		t.Fatalf("wrong password not reported as bad credentials: %v", err)
	}
	if s.routeThrottle.fails != 1 {
		t.Fatalf("wrong password counted %d times, want 1", s.routeThrottle.fails)
	}

	tCore.verifyPasswordErr = nil
	if _, err := s.resolvePass([]byte("right"), req); err != nil {
		t.Fatalf("right password rejected: %v", err)
	}
	if s.routeThrottle.fails != 0 {
		t.Fatalf("right password left %d failures counted", s.routeThrottle.fails)
	}
}

// An environmental resolvePass failure (e.g. a cached-password lookup that
// fails on the mobile flow) must not be tagged badCredentials, so it does not
// feed the brute-force throttle and lock out the legitimate client.
func TestResolvePassFailureDoesNotThrottle(t *testing.T) {
	s := &AppServer{}

	// Empty password plus cookies that point at a non-existent cached
	// password forces resolvePass down the retrieval-error path.
	req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	req.AddCookie(&http.Cookie{Name: authCK, Value: "deadbeef"})
	req.AddCookie(&http.Cookie{Name: pwKeyCK, Value: "00112233445566778899aabbccddeeff"})

	err := s.actuallyLogin(httptest.NewRecorder(), req, &loginForm{})
	if err == nil {
		t.Fatal("expected resolvePass failure, got nil")
	}
	if errors.Is(err, errBadCredentials) {
		t.Fatalf("resolvePass failure tagged as bad credentials: %v", err)
	}
}

// TestOnionRequestNotHostDerived asserts that onion classification comes from
// the listener the connection arrived on, not from anything the remote peer
// sends. The hidden service relays over loopback, so a Host header (the only
// other candidate signal) would let a remote Tor peer present itself as a
// same-machine client and walk past every gate keyed on it: the
// companion-pairing requirement in isAuthed and actuallyLogin, and the
// loopback-only restriction on minting and using API tokens.
func TestOnionRequestNotHostDerived(t *testing.T) {
	s := &AppServer{onion: "http://caribou.onion"}

	// A request off the tor listener, claiming to be loopback.
	spoofed := httptest.NewRequest(http.MethodPost, "/api/generateapitoken", nil)
	spoofed.Host = "127.0.0.1:5758"
	spoofed.RemoteAddr = "127.0.0.1:44444" // the relay's loopback connection
	spoofed = spoofed.WithContext(context.WithValue(spoofed.Context(), onionConnKey{}, true))

	if !s.isOnionRequest(spoofed) {
		t.Fatal("a tor-listener request escaped onion classification via the Host header")
	}
	if s.isLocalRequest(spoofed) {
		t.Fatal("a tor-listener request was treated as same-machine")
	}

	// A genuine same-machine request, claiming to be the onion host. It must
	// not be able to shed local status either, since that is what a local
	// browser reaching the server by its onion name would look like.
	direct := httptest.NewRequest(http.MethodPost, "/api/generateapitoken", nil)
	direct.Host = "caribou.onion"
	direct.RemoteAddr = "127.0.0.1:44445"

	if s.isOnionRequest(direct) {
		t.Fatal("a direct-listener request was classified as onion via the Host header")
	}
	if !s.isLocalRequest(direct) {
		t.Fatal("a direct loopback request was not treated as same-machine")
	}
}

// TestOnionConnContextIdentifiesTheListener exercises the comparison
// TestOnionRequestNotHostDerived stops short of: the one that decides whether a
// connection is tagged at all. Everything downstream reads a context value that
// is only ever set here, so a silent mismatch between the listener's address
// and an accepted connection's local address disables the whole protection with
// no visible symptom — onion traffic would simply be classified as local.
func TestOnionConnContextIdentifiesTheListener(t *testing.T) {
	torLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer torLn.Close()
	directLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer directLn.Close()

	connCtx := onionConnContext(torLn.Addr().String())

	tagged := func(ln net.Listener) bool {
		client, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer client.Close()
		server, err := ln.Accept()
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		defer server.Close()
		return connCtx(context.Background(), server).Value(onionConnKey{}) != nil
	}

	if !tagged(torLn) {
		t.Fatal("a connection accepted on the hidden service's listener was not tagged as onion")
	}
	if tagged(directLn) {
		t.Fatal("a connection accepted on the direct listener was tagged as onion")
	}
}

// TestWSRequiresAuth asserts that the notification websocket is gated by the
// same session check as the /api routes. /ws streams every balance, wallet
// state, deposit address and send notification for the life of the process, so
// an unauthenticated upgrade hands all of it to any local process — on desktop
// capabilityMiddleware is a no-op, so nothing else stands in the way.
func TestWSRequiresAuth(t *testing.T) {
	s, _, shutdown := newTServer(t, true)
	defer shutdown()

	dial := func(cookies []*http.Cookie) (*http.Response, error) {
		hdr := http.Header{}
		for _, c := range cookies {
			hdr.Add("Cookie", c.String())
		}
		conn, resp, err := websocket.DefaultDialer.Dial("ws://"+s.addr+"/ws", hdr)
		if conn != nil {
			conn.Close()
		}
		return resp, err
	}

	// No session cookie: the upgrade must be refused outright, not merely
	// starved of notifications.
	resp, err := dial(nil)
	if err == nil {
		t.Fatal("unauthenticated /ws upgrade succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 for unauthenticated /ws, got %v (err %v)", resp, err)
	}

	// A live session token authorizes the upgrade, so the WebView still
	// receives notifications after logging in.
	const authToken = "00112233445566778899aabbccddeeff"
	s.authMtx.Lock()
	s.authTokens[authToken] = true
	s.authMtx.Unlock()

	if _, err := dial([]*http.Cookie{{Name: authCK, Value: authToken}}); err != nil {
		t.Fatalf("authenticated /ws upgrade rejected: %v", err)
	}
}

// TestSetCookieHttpOnly asserts the session cookies are not readable from
// document.cookie. sessionkey decrypts the cached app password, and the
// capability cookie in this same package already sets HttpOnly, so leaving it
// off here is an inconsistency rather than a deliberate trade-off.
func TestSetCookieHttpOnly(t *testing.T) {
	for _, name := range []string{authCK, pwKeyCK} {
		rec := httptest.NewRecorder()
		setCookie(name, "value", rec)

		cookies := rec.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("%s: want 1 cookie, got %d", name, len(cookies))
		}
		if !cookies[0].HttpOnly {
			t.Errorf("%s is not HttpOnly, so script can read it", name)
		}
		// Secure stays off deliberately: the loopback server is HTTP-only and
		// a Secure cookie would never be sent, breaking the WebView bootstrap.
		if cookies[0].Secure {
			t.Errorf("%s is Secure, which breaks the HTTP-only loopback server", name)
		}
	}
}

// TestReadPostRejectsOversizedBody asserts readPost caps the request body it
// will read, so a single request cannot force an arbitrary-size allocation
// before any handler validation runs.
func TestReadPostRejectsOversizedBody(t *testing.T) {
	// readPost logs via the package-level log var, which is otherwise unset
	// outside a constructed AppServer. Restored on cleanup so the mutation
	// does not persist for the rest of the test binary; this does not by
	// itself make the assignment safe under -race with t.Parallel(), since
	// readPost is a free function reading the same unsynchronized package
	// var — no test in this package uses t.Parallel() today, which is what
	// keeps this dormant.
	prevLog := log
	log = tLogger
	t.Cleanup(func() { log = prevLog })

	t.Run("within cap", func(t *testing.T) {
		body := `{"pass":"a"}`
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(body))
		rec := httptest.NewRecorder()
		var form struct {
			Pass string `json:"pass"`
		}
		if !readPost(rec, req, &form) {
			t.Fatalf("readPost rejected a body within the cap: %d", rec.Code)
		}
		if form.Pass != "a" {
			t.Fatalf("unexpected decoded form: %+v", form)
		}
	})

	t.Run("over cap", func(t *testing.T) {
		oversized := strings.Repeat("a", maxRequestBody+1)
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(oversized))
		rec := httptest.NewRecorder()
		var form struct {
			Pass string `json:"pass"`
		}
		if readPost(rec, req, &form) {
			t.Fatal("readPost accepted a body over the cap")
		}
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("want %d, got %d", http.StatusRequestEntityTooLarge, rec.Code)
		}
	})
}

// TestLogoutDisconnectsWS asserts that ending a session drops the notification
// sockets opened under it. /ws is authorized at upgrade and never re-checked,
// so a link that outlived its session would keep streaming balances, wallet
// state, deposit addresses and sends to a peer that can no longer
// authenticate — which is exactly what Lock claims to prevent when it
// invalidates the auth tokens.
func TestLogoutDisconnectsWS(t *testing.T) {
	s, _, shutdown := newTServer(t, true)
	defer shutdown()

	const authToken = "00112233445566778899aabbccddeeff"
	s.authMtx.Lock()
	s.authTokens[authToken] = true
	s.authMtx.Unlock()

	hdr := http.Header{}
	hdr.Add("Cookie", (&http.Cookie{Name: authCK, Value: authToken}).String())
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+s.addr+"/ws", hdr)
	if err != nil {
		t.Fatalf("authenticated /ws upgrade rejected: %v", err)
	}
	defer conn.Close()

	if err := s.Lock(); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	assertWSClosed(t, conn, "logout")
}

// assertWSClosed fails unless the peer observes the socket close. An idle
// socket is not enough: a read deadline expiring means the link is still up,
// which is the bug every caller of this is written to catch.
func assertWSClosed(t *testing.T, conn *websocket.Conn, after string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatalf("socket still live after %s", after)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("socket left open after %s; read timed out instead of closing: %v", after, err)
	}
}

// TestRevokeAPITokenDisconnectsWS covers the credential the notification socket
// can carry that no cookie-bearing session teardown touches. isAuthed
// short-circuits on the API token before it looks at any session, and a
// non-browser automation client can set that header on a /ws upgrade, so
// revoking the token has to drop the sockets opened under it or the handler's
// promise of immediate invalidation holds for every request except the one that
// streams balances, wallet state, deposit addresses and sends continuously.
func TestRevokeAPITokenDisconnectsWS(t *testing.T) {
	s, _, shutdown := newTServer(t, true)
	defer shutdown()

	const token = "0123456789abcdef0123456789abcdef"
	s.authMtx.Lock()
	s.apiTokenHash = hashToken(token)
	s.authMtx.Unlock()

	hdr := http.Header{}
	hdr.Set(apiTokenHeader, token)
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+s.addr+"/ws", hdr)
	if err != nil {
		t.Fatalf("/ws upgrade bearing an API token rejected: %v", err)
	}
	defer conn.Close()

	// Revoke, which the handler only accepts from the same machine.
	req := httptest.NewRequest("POST", "/api/revokeapitoken", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.apiRevokeAPIToken(rec, req)
	if body := rec.Body.String(); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("revoke failed: %s", body)
	}

	assertWSClosed(t, conn, "the API token was revoked")
}

// TestUnpairCompanionDisconnectsWS covers the other credential a socket can
// outlive. Unpairing refuses every later companion request twice over - the
// token is gone and an onion request needs a pairing - but an upgrade is
// authorized once and never re-checked, so the peer this call exists to cut
// off, which is remote over the hidden service, keeps its stream.
func TestUnpairCompanionDisconnectsWS(t *testing.T) {
	s, _, shutdown := newTServer(t, true)
	defer shutdown()

	const companionToken = "00112233445566778899aabbccddeeff"
	s.authMtx.Lock()
	s.authTokens[companionToken] = true
	s.companionToken = companionToken
	s.companionTokenHash = hashToken(companionToken)
	s.companionTokenClaimed = true
	s.authMtx.Unlock()

	hdr := http.Header{}
	hdr.Add("Cookie", (&http.Cookie{Name: authCK, Value: companionToken}).String())
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+s.addr+"/ws", hdr)
	if err != nil {
		t.Fatalf("/ws upgrade bearing the companion token rejected: %v", err)
	}
	defer conn.Close()

	rec := httptest.NewRecorder()
	s.apiUnpairCompanionApp(rec, httptest.NewRequest("POST", "/api/unpaircompanionapp", nil))
	if body := rec.Body.String(); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("unpair failed: %s", body)
	}

	assertWSClosed(t, conn, "the companion app was unpaired")
}

// TestHostAllowlist checks hostMiddleware, the DNS rebinding defense. A page
// that rebinds its own name to 127.0.0.1 still sends that name as the Host
// header, and must get nothing, not even the unauthenticated routes.
func TestHostAllowlist(t *testing.T) {
	s, _, shutdown := newTServer(t, false)
	defer shutdown()
	s.webHost = hostOnly("mywallet.lan:5758")

	allowed := []string{
		"127.0.0.1:5758",
		"127.0.0.1",
		"[::1]:5758",
		"192.168.1.20:5758", // bound to 0.0.0.0 or a LAN IP
		"localhost:5758",
		"LOCALHOST:5758",
		"mywallet.lan:5758", // the configured --webaddr host
		"mywallet.lan.",
	}
	refused := []string{
		"evil.example:5758", // the rebinding case
		"evil.example",
		"localhost.evil.example:5758",
		"127.0.0.1.evil.example:5758",
		"sub.localhost:5758",
		// An onion name is only trusted on the onion listener.
		"abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwx.onion",
		"",
	}

	serve := func(host string, onion bool) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/login", strings.NewReader(`{"pass":"x"}`))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		if onion {
			req = req.WithContext(context.WithValue(req.Context(), onionConnKey{}, true))
		}
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, h := range allowed {
		if code := serve(h, false); code == http.StatusMisdirectedRequest {
			t.Errorf("Host %q refused, want allowed", h)
		}
	}
	for _, h := range refused {
		if code := serve(h, false); code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q got %d, want %d", h, code, http.StatusMisdirectedRequest)
		}
	}

	// On the onion listener the Host header is the remote Tor client's, in
	// whatever form it sends the onion name, so it is not checked.
	for _, h := range []string{
		"abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwx.onion",
		"abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwx.onion:80",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTUVWX.ONION",
	} {
		if code := serve(h, true); code == http.StatusMisdirectedRequest {
			t.Errorf("Host %q on the onion listener refused, want allowed", h)
		}
	}
}

// TestCapabilityGate checks Config.CapabilityGate, which the desktop shells
// set: New generates a token, BootstrapURL carries it, and a request without
// it (a DNS rebinding page, say) gets nothing, while the window that loaded
// BootstrapURL gets in.
func TestCapabilityGate(t *testing.T) {
	newServer := func(gate bool) *AppServer {
		t.Helper()
		s, err := New(&Config{
			Core:           &TCore{},
			Addr:           "127.0.0.1:0",
			Logger:         tLogger,
			AppVersion:     testAppVersion,
			CapabilityGate: gate,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		s.addr = "127.0.0.1:5758" // set by Connect
		return s
	}

	// Off: no token, and the bootstrap URL is the bare address.
	if s := newServer(false); s.capToken != "" || s.BootstrapURL() != "http://127.0.0.1:5758" {
		t.Fatalf("gate off: token %q, bootstrap URL %q", s.capToken, s.BootstrapURL())
	}

	s := newServer(true)
	if len(s.capToken) != 64 {
		t.Fatalf("gate on: token %q, want 32 random bytes as hex", s.capToken)
	}
	if other := newServer(true); other.capToken == s.capToken {
		t.Fatal("two servers got the same token")
	}
	bootstrap := s.BootstrapURL()
	if bootstrap != "http://127.0.0.1:5758/?k="+s.capToken {
		t.Fatalf("bootstrap URL %q does not carry the token", bootstrap)
	}

	serve := func(req *http.Request) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		return rec
	}

	// No token: nothing, including the unauthenticated login route.
	for _, path := range []string{"/", "/api/user", "/api/login"} {
		method := http.MethodGet
		if path == "/api/login" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, "http://127.0.0.1:5758"+path, strings.NewReader(`{"pass":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		if rec := serve(req); rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s without the token: %d, want 404", method, path, rec.Code)
		}
	}

	// The bootstrap URL swaps the token for a cookie, which then gets in.
	rec := serve(httptest.NewRequest(http.MethodGet, bootstrap, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("bootstrap URL: %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, s.capToken) {
		t.Fatalf("redirect %q still carries the token", loc)
	}
	var capCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == capCK {
			capCookie = c
		}
	}
	if capCookie == nil {
		t.Fatal("bootstrap URL set no capability cookie")
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5758/api/user", nil)
	req.AddCookie(capCookie)
	if rec := serve(req); rec.Code == http.StatusNotFound {
		t.Fatal("request with the capability cookie got 404")
	}

	// A local API token gets in without the cookie, so automation still works.
	const apiToken = "automation-token"
	s.apiTokenHash = hashToken(apiToken)
	tokenReq := func(token, remote string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5758/api/user", nil)
		req.Header.Set(apiTokenHeader, token)
		req.RemoteAddr = remote
		return req
	}
	if rec := serve(tokenReq(apiToken, "127.0.0.1:40000")); rec.Code == http.StatusNotFound {
		t.Fatal("request with a valid API token got 404")
	}
	if rec := serve(tokenReq("wrong", "127.0.0.1:40000")); rec.Code != http.StatusNotFound {
		t.Fatalf("request with a wrong API token: %d, want 404", rec.Code)
	}
	if rec := serve(tokenReq(apiToken, "192.0.2.1:40000")); rec.Code != http.StatusNotFound {
		t.Fatalf("API token from a remote peer: %d, want 404", rec.Code)
	}

	// The onion listener skips both the Host check and the gate. A Host header
	// naming an onion does not, since only the listener marks a request as
	// onion.
	req = httptest.NewRequest(http.MethodGet, "http://abc.onion/api/user", nil)
	if rec := serve(req); rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("onion Host header without the onion listener: %d, want 421", rec.Code)
	}
	req = req.WithContext(context.WithValue(req.Context(), onionConnKey{}, true))
	if rec := serve(req); rec.Code == http.StatusNotFound || rec.Code == http.StatusMisdirectedRequest {
		t.Fatalf("request on the onion listener: %d, want it past the Host check and the gate", rec.Code)
	}
}

// TestNoBrowserBanner checks that a shell opening its own windows can turn off
// the banner, which carries the capability token when the gate is on.
func TestNoBrowserBanner(t *testing.T) {
	for _, off := range []bool{false, true} {
		s, err := New(&Config{
			Core:            &TCore{},
			Addr:            "127.0.0.1:0",
			Logger:          tLogger,
			AppVersion:      testAppVersion,
			CapabilityGate:  true,
			NoBrowserBanner: off,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if want := printBrowserBanner && !off; s.banner != want {
			t.Errorf("NoBrowserBanner %v: banner = %v, want %v", off, s.banner, want)
		}
	}
}
