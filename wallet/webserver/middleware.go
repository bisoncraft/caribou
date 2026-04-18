// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package webserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type ctxID int

const (
	ctxProposalToken ctxID = iota
)

// securityMiddleware adds security headers to the server responses.
func (s *WebServer) securityMiddleware(next http.Handler) http.Handler {
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

// authMiddleware checks incoming requests for cookie-based information
// including the auth token. Use extractUserInfo to access the *userInfo in
// downstream handlers. This should be used with care since it involves a call
// to (*Core).User, which can be expensive.
func (s *WebServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxKeyUserInfo, &userInfo{
			Authed:           s.isAuthed(r),
			PasswordIsCached: s.isPasswordCached(r),
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tokenAuthMiddleware checks for a dexauth query parameter.
// If the token is valid it sets the dexauth cookie, allowing passwordless
// login in the context of mobile companion apps. When the token matches
// the current companion token it is marked as claimed and a SHA-256 hash
// is persisted to the database so it survives restarts. Invalid tokens
// are rejected so that a revoked companion app cannot fall through to
// password login. After a server restart, an unknown token is checked
// against the persisted hash to re-establish the pairing.
func (s *WebServer) tokenAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries := r.URL.Query()
		authToken := queries.Get(authCK)
		if authToken != "" {
			s.authMtx.Lock()
			valid := s.authTokens[authToken]
			newlyClaimed := valid && authToken == s.companionToken && !s.companionTokenClaimed
			if newlyClaimed {
				s.companionTokenClaimed = true
			}
			s.authMtx.Unlock()
			if !valid {
				// Token not in memory. Try to restore from
				// the persisted hash (e.g. after restart).
				if !s.restoreCompanionFromHash(authToken) {
					http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
					return
				}
			}
			setCookie(authCK, authToken, w)
			if newlyClaimed {
				h := sha256.Sum256([]byte(authToken))
				hashStr := hex.EncodeToString(h[:])
				s.authMtx.Lock()
				s.companionTokenHash = hashStr
				s.authMtx.Unlock()
				if err := s.core.SetCompanionToken(hashStr); err != nil {
					log.Errorf("Error persisting claimed companion token hash: %v", err)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// extractBooleanCookie extracts the cookie value with key k from the Request,
// and interprets the value as true only if it's equal to the string "1".
func extractBooleanCookie(r *http.Request, k string, defaultVal bool) bool {
	cookie, err := r.Cookie(k)
	switch {
	// Dark mode is the default
	case err == nil:
		return cookie.Value == "1"
	case errors.Is(err, http.ErrNoCookie):
	default:
		log.Errorf("Cookie %q retrieval error: %v", k, err)
	}
	return defaultVal
}

// requireInit ensures that the core app is initialized before allowing the
// incoming request to proceed. Redirects to the register page if the app is
// not initialized.
func (s *WebServer) requireInit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.core.IsInitialized() {
			http.Redirect(w, r, initRoute, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireNotInit ensures that the core app is not initialized before allowing
// the incoming request to proceed. Redirects to the login page if the app is
// initialized and the user is not logged in. If logged in, directs to the
// wallets page.
func (s *WebServer) requireNotInit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.core.IsInitialized() {
			route := loginRoute
			if extractUserInfo(r).Authed {
				route = walletsRoute
			}
			http.Redirect(w, r, route, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rejectUninited is like requireInit except that it responds with an error
// instead of redirecting to the register path.
func (s *WebServer) rejectUninited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.core.IsInitialized() {
			http.Error(w, http.StatusText(http.StatusPreconditionRequired), http.StatusPreconditionRequired)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireLogin ensures that the user is authenticated (has logged in) before
// allowing the incoming request to proceed. Redirects to login page if user is
// not logged in. This check should typically be performed after checking that
// the app is initialized.
func (s *WebServer) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthed(r) {
			http.Redirect(w, r, loginRoute, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rejectUnauthed is like requireLogin except that it responds with an error
// instead of redirecting to the login path.
func (s *WebServer) rejectUnauthed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthed(r) {
			http.Error(w, "not authorized - login first", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// proposalTokenCtx embeds the proposal token into the request context.
func proposalTokenCtx(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		ctx := context.WithValue(r.Context(), ctxProposalToken, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// getProposalTokenCtx interprets the context value at ctxProposalToken as a string.
func getProposalTokenCtx(r *http.Request) (string, error) {
	untypedToken := r.Context().Value(ctxProposalToken)
	if untypedToken == nil {
		return "", errors.New("proposal token not set in request")
	}
	token, ok := untypedToken.(string)
	if !ok {
		return "", errors.New("type assertion failed")
	}
	return token, nil
}
