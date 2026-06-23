// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
