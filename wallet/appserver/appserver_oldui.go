//go:build oldui

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/appserver/locales"
	"github.com/go-chi/chi/v5"
	"golang.org/x/text/language"
)

// contextKey is the key param type used when saving values to a context using
// context.WithValue. A custom type is defined because built-in types are
// discouraged.
type contextKey string

type ctxID int

const (
	ctxProposalToken ctxID = iota
)

const ctxKeyUserInfo contextKey = "userinfo"

// userInfo is information about the connected user. This type embeds the
// core.User type, adding fields specific to the users server authentication
// and cookies.
type userInfo struct {
	Authed           bool
	PasswordIsCached bool
}

// extractUserInfo extracts the userInfo from the request context. This should
// be used with authMiddleware.
func extractUserInfo(r *http.Request) *userInfo {
	user, ok := r.Context().Value(ctxKeyUserInfo).(*userInfo)
	if !ok {
		log.Errorf("no auth info retrieved from client")
		return &userInfo{}
	}
	return user
}

// authMiddleware checks incoming requests for cookie-based information
// including the auth token. Use extractUserInfo to access the *userInfo in
// downstream handlers. This should be used with care since it involves a call
// to (*Core).User, which can be expensive.
func (s *AppServer) authMiddleware(next http.Handler) http.Handler {
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
func (s *AppServer) tokenAuthMiddleware(next http.Handler) http.Handler {
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

// requireInit ensures that the core app is initialized before allowing the
// incoming request to proceed. Redirects to the register page if the app is
// not initialized.
func (s *AppServer) requireInit(next http.Handler) http.Handler {
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
func (s *AppServer) requireNotInit(next http.Handler) http.Handler {
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

// requireLogin ensures that the user is authenticated (has logged in) before
// allowing the incoming request to proceed. Redirects to login page if user is
// not logged in. This check should typically be performed after checking that
// the app is initialized.
func (s *AppServer) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthed(r) {
			http.Redirect(w, r, loginRoute, http.StatusSeeOther)
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

// buildTemplates prepares the HTML templates, which are executed and served in
// sendTemplate. An empty siteDir indicates that the embedded templates in the
// htmlTmplSub FS should be used. If siteDir is set, the templates will be
// loaded from disk.
func (s *AppServer) buildTemplates(lang string) error {
	// Try to identify language.
	acceptLang, err := language.Parse(lang)
	if err != nil {
		return fmt.Errorf("unable to parse requested language: %v", err)
	}

	// Find acceptable match with available locales.
	langTags := make([]language.Tag, 0, len(locales.Locales))
	localeNames := make([]string, 0, len(locales.Locales))
	for localeName := range locales.Locales {
		lang, _ := language.Parse(localeName) // checked in init()
		langTags = append(langTags, lang)
		localeNames = append(localeNames, localeName)
	}
	_, idx, conf := language.NewMatcher(langTags).Match(acceptLang)
	localeName := localeNames[idx] // use index because tag may end up as something hyper specific like zh-Hans-u-rg-cnzzzz
	switch conf {
	case language.Exact, language.High, language.Low:
		log.Infof("Using language %v", localeName)
	case language.No:
		return fmt.Errorf("no match for %q in recognized languages %v", lang, localeNames)
	}

	var htmlDir string
	if s.siteDir == "" {
		log.Infof("Using embedded HTML templates")
	} else {
		htmlDir = filepath.Join(s.siteDir, "src", "html")
		log.Infof("Using HTML templates in %s", htmlDir)
	}

	bb := "bodybuilder"
	html := newTemplates(htmlDir, localeName).
		addTemplate("login", bb, "forms").
		addTemplate("wallets", bb, "forms", "docs").
		addTemplate("settings", bb, "forms").
		addTemplate("init", bb).
		addTemplate("proposals", bb).
		addTemplate("proposal", bb)
	s.html.Store(html)

	return html.buildErr()
}

// companionTokenTTL is the time-to-live for an unclaimed companion token.
// If the companion app does not scan the QR code within this window the
// token is automatically revoked.
const companionTokenTTL = 10 * time.Minute

// authorizeCompanion mints a new companion-app auth token, revoking any
// previous one. The token is unclaimed until the companion app scans the
// QR code and opens the URL. An unclaimed token expires after
// companionTokenTTL.
func (s *AppServer) authorizeCompanion() string {
	b := make([]byte, 32)
	crand.Read(b)
	token := hex.EncodeToString(b)
	zero(b)

	s.authMtx.Lock()
	// Revoke previous companion token.
	if s.companionToken != "" {
		delete(s.authTokens, s.companionToken)
	}
	// Stop any previous expiry timer.
	if s.companionExpiryTimer != nil {
		s.companionExpiryTimer.Stop()
	}
	s.authTokens[token] = true
	s.companionToken = token
	s.companionTokenHash = ""
	s.companionTokenExpiry = time.Now().Add(companionTokenTTL)
	s.companionTokenClaimed = false
	s.companionExpiryTimer = time.AfterFunc(companionTokenTTL, func() {
		s.authMtx.Lock()
		defer s.authMtx.Unlock()
		if s.companionToken == token && !s.companionTokenClaimed {
			delete(s.authTokens, token)
			s.companionToken = ""
			s.companionTokenExpiry = time.Time{}
			s.companionExpiryTimer = nil
		}
	})
	s.authMtx.Unlock()

	// Clear any previously persisted token. The new token is only
	// persisted when claimed (see tokenAuthMiddleware) so that a
	// crash before claim doesn't leave a stale unclaimed token that
	// gets treated as claimed on restart.
	if err := s.core.SetCompanionToken(""); err != nil {
		log.Errorf("Error clearing persisted companion token: %v", err)
	}

	return token
}

// registerLegacyWebRoutes installs the server-rendered page routes used by the
// pre-SPA UI. The new SPA UI (default build) installs no per-page routes —
// every page is served from the SPA shell via the mux.NotFound fallback in
// New.
func (s *AppServer) registerLegacyWebRoutes(mux *chi.Mux) {
	mux.Group(func(web chi.Router) {
		web.Use(s.tokenAuthMiddleware)
		// Inject user info for handlers that use extractUserInfo, which
		// includes most of the page handlers that use commonArgs to
		// inject the User object for page template execution.
		web.Use(s.authMiddleware)
		web.Get(settingsRoute, s.handleSettings)

		web.Get("/generateqrcode", s.handleGenerateQRCode)

		web.Group(func(notInit chi.Router) {
			notInit.Use(s.requireNotInit)
			notInit.Get(initRoute, s.handleInit)
		})

		// The rest of the web handlers require initialization.
		web.Group(func(webInit chi.Router) {
			webInit.Use(s.requireInit)

			webInit.Group(func(webNoAuth chi.Router) {
				// The login handler requires init but not auth since
				// it performs the auth.
				webNoAuth.Get(loginRoute, s.handleLogin)

				// The rest of these handlers require both init and auth.
				webNoAuth.Group(func(webAuth chi.Router) {
					webAuth.Use(s.requireLogin)
					webAuth.Get(homeRoute, s.handleHome)
					webAuth.Get(walletsRoute, s.handleWallets)
					webAuth.Get(walletLogRoute, s.handleWalletLogFile)
					webAuth.With(proposalTokenCtx).Get("/proposal/{token}", s.handleProposal)
					webAuth.Get(proposalsRoute, s.handleProposals)
					webAuth.Get("/generatecompanionappqrcode", s.handleGenerateCompanionAppQRCode)
				})
			})

		})
	})
}
