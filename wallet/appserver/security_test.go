//go:build !live

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
)

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

func TestLoginThrottle(t *testing.T) {
	s := &AppServer{}

	if d := s.loginThrottle(); d != 0 {
		t.Fatalf("fresh server already throttled: %v", d)
	}

	// The free budget does not lock the account.
	for i := 0; i < loginFailFree; i++ {
		s.noteLoginFailure()
	}
	if d := s.loginThrottle(); d != 0 {
		t.Fatalf("throttled within the free budget: %v", d)
	}

	// One more failure starts the lockout.
	s.noteLoginFailure()
	if d := s.loginThrottle(); d <= 0 {
		t.Fatal("no lockout after exceeding the free budget")
	}

	// A success clears it.
	s.noteLoginSuccess()
	if d := s.loginThrottle(); d != 0 {
		t.Fatalf("lockout not cleared after success: %v", d)
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
