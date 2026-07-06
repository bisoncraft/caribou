// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import (
	"crypto/subtle"
	"net/http"
)

// capabilityMiddleware gates every request on a capability token when one is
// configured (Config.CapabilityToken, set by the mobile build). The token is
// handed to the WebView in the bootstrap URL (?k=...) and exchanged here for an
// HttpOnly cookie on first load; thereafter the cookie authorizes requests.
//
// On Android the loopback interface is shared by every app on the device, so a
// listening 127.0.0.1 server is reachable by any app with the INTERNET
// permission. This gate makes the server answer only the WebView we launched:
// other apps cannot present the token, so they cannot reach the SPA, the
// unauthenticated API routes (e.g. /api/login), or the notification websocket.
//
// When no token is configured (desktop) the gate is a no-op.
func (s *AppServer) capabilityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.capToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Already granted: a valid capability cookie. The reattach URL still
		// carries ?k=<token>, so strip it rather than serving the page with the
		// token sitting in location.search.
		if c, err := r.Cookie(capCK); err == nil &&
			subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.capToken)) == 1 {
			if r.URL.Query().Has("k") {
				// Strip only the capability token; a session grant (?g=) may ride
				// along on the same reattach URL and is consumed by grantMiddleware.
				stripTokenRedirect(w, r, "k")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		// Bootstrap: the WebView's first navigation carries ?k=<token>. Grant
		// the cookie, then redirect to the same URL with the token stripped so
		// it does not linger in the document URL.
		if r.Method == http.MethodGet &&
			subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("k")), []byte(s.capToken)) == 1 {
			http.SetCookie(w, &http.Cookie{
				Name:     capCK,
				Value:    s.capToken,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				// Secure is omitted intentionally: the loopback server is
				// HTTP-only, and a Secure cookie would never be sent over
				// http://, breaking the WebView bootstrap.
			})
			// Strip only "k"; any ?g= grant is preserved for grantMiddleware on
			// the redirected request (which now carries the capability cookie).
			stripTokenRedirect(w, r, "k")
			return
		}
		// Anything else (port scanners, other local apps) gets nothing useful.
		http.NotFound(w, r)
	})
}

// grantMiddleware swaps a one-time session grant (?g=<grant>, minted by Unlock)
// for the dexauth and sessionkey cookies, then redirects with the token stripped
// so it does not linger in the document URL. It runs after capabilityMiddleware,
// so the capability gate still protects it, and before the request logger, so
// the grant never reaches the log. A request without ?g= passes straight
// through, making this a no-op on desktop (which never mints grants).
func (s *AppServer) grantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grant := r.URL.Query().Get("g")
		if grant == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Consume the grant. An unknown or already-used grant just gets stripped;
		// the request continues unauthed and the SPA falls back to its login gate.
		s.authMtx.Lock()
		sg := s.pendingGrants[grant]
		delete(s.pendingGrants, grant)
		s.authMtx.Unlock()

		if sg != nil {
			if sg.ttlTimer != nil {
				sg.ttlTimer.Stop()
			}
			setCookie(authCK, sg.authToken, w)
			setCookie(pwKeyCK, sg.pwKey, w)
		}
		stripTokenRedirect(w, r, "g")
	})
}

// stripTokenRedirect 302s back to the same path with the named query params
// removed, so tokens do not linger in the document URL.
func stripTokenRedirect(w http.ResponseWriter, r *http.Request, params ...string) {
	q := r.URL.Query()
	for _, p := range params {
		q.Del(p)
	}
	dest := r.URL.Path
	if enc := q.Encode(); enc != "" {
		dest += "?" + enc
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// securityMiddleware adds security headers to the server responses.
func (s *AppServer) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-frame-options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if s.csp != "" {
			w.Header().Set("Content-Security-Policy", s.csp)
		}
		w.Header().Set("Permissions-Policy", "geolocation=(), midi=(), sync-xhr=(self), microphone=(), camera=(), magnetometer=(), gyroscope=(), fullscreen=(self), payment=()")
		next.ServeHTTP(w, r)
	})
}

// rejectUninited responds with HTTP 428 Precondition Required if the core
// app is not initialized. Used to gate API endpoints that require an
// initialized core.
func (s *AppServer) rejectUninited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.core.IsInitialized() {
			http.Error(w, http.StatusText(http.StatusPreconditionRequired), http.StatusPreconditionRequired)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rejectUnauthed responds with HTTP 401 Unauthorized if the user is not
// logged in. Used to gate API endpoints that require an authenticated user.
func (s *AppServer) rejectUnauthed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthed(r) {
			http.Error(w, "not authorized - login first", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
