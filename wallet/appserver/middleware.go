// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import "net/http"

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
