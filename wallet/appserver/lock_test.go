//go:build !live

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bisoncraft/bosswallet/util/encode"
)

// grantCookie returns the value of the named response cookie, or "".
func grantCookie(rec *httptest.ResponseRecorder, name string) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// TestUnlockMintsConsumableGrant covers the biometric-unlock session-restore
// path end to end: Unlock logs core back in, mints exactly one pending grant
// bound to a fresh auth token and cached password, and grantMiddleware then
// swaps that grant for the session cookies and consumes it.
func TestUnlockMintsConsumableGrant(t *testing.T) {
	s, _, shutdown := newTServer(t, false)
	defer shutdown()

	grant, err := s.Unlock([]byte("apppass"))
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if grant == "" {
		t.Fatal("Unlock returned an empty grant")
	}

	s.authMtx.RLock()
	sg := s.pendingGrants[grant]
	nGrants := len(s.pendingGrants)
	var boundToken, cached bool
	if sg != nil {
		boundToken = s.authTokens[sg.authToken]
		_, cached = s.cachedPasswords[sg.authToken]
	}
	s.authMtx.RUnlock()

	if nGrants != 1 {
		t.Fatalf("want exactly 1 pending grant, got %d", nGrants)
	}
	if sg == nil {
		t.Fatal("returned grant is not in pendingGrants")
	}
	if !boundToken {
		t.Fatal("grant not bound to a valid auth token")
	}
	if !cached {
		t.Fatal("no cached password for the grant's auth token")
	}

	// grantMiddleware consumes it: sets the session cookies and removes the grant.
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?g="+grant, nil)
	s.grantMiddleware(next).ServeHTTP(rec, req)

	if got := grantCookie(rec, authCK); got != sg.authToken {
		t.Fatalf("dexauth cookie = %q, want %q", got, sg.authToken)
	}
	if got := grantCookie(rec, pwKeyCK); got != sg.pwKey {
		t.Fatalf("sessionkey cookie = %q, want %q", got, sg.pwKey)
	}
	s.authMtx.RLock()
	_, still := s.pendingGrants[grant]
	s.authMtx.RUnlock()
	if still {
		t.Fatal("grant was not consumed")
	}
}

// TestUnlockLoginErrorLeavesNoSession covers the failed-login path: Unlock
// returns the error and mints no session, so a wrong password cannot leave a
// half-open session behind.
func TestUnlockLoginErrorLeavesNoSession(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()
	c.loginErr = errors.New("bad password")

	grant, err := s.Unlock([]byte("wrong"))
	if err == nil {
		t.Fatal("Unlock succeeded on a login error")
	}
	if grant != "" {
		t.Fatalf("Unlock returned a grant on failure: %q", grant)
	}
	s.authMtx.RLock()
	defer s.authMtx.RUnlock()
	if len(s.pendingGrants) != 0 || len(s.authTokens) != 0 || len(s.cachedPasswords) != 0 {
		t.Fatalf("session state leaked after a failed unlock: grants=%d tokens=%d cached=%d",
			len(s.pendingGrants), len(s.authTokens), len(s.cachedPasswords))
	}
}

// TestUnlockRollsBackAfterLogin covers the security-critical rollback: if a step
// after core.Login fails, Unlock must re-lock the wallets (core.Logout) and leave
// no partial session behind, rather than stranding decrypted wallets in memory
// with no way to reach them. The grant-token step is forced to fail via the
// newGrantToken seam, which runs after authorize and cacheAppPassword have
// already populated an auth token and cached password.
func TestUnlockRollsBackAfterLogin(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()

	orig := newGrantToken
	newGrantToken = func() (string, error) { return "", errors.New("no entropy") }
	defer func() { newGrantToken = orig }()

	grant, err := s.Unlock([]byte("apppass"))
	if err == nil {
		t.Fatal("Unlock succeeded despite a post-login failure")
	}
	if grant != "" {
		t.Fatalf("Unlock returned a grant on failure: %q", grant)
	}
	// The login must have been rolled back with a Logout (wallets re-locked).
	if c.logoutCalls != 1 {
		t.Fatalf("want 1 rollback Logout, got %d", c.logoutCalls)
	}
	// And no partial session may survive.
	s.authMtx.RLock()
	defer s.authMtx.RUnlock()
	if len(s.authTokens) != 0 || len(s.cachedPasswords) != 0 || len(s.pendingGrants) != 0 {
		t.Fatalf("rollback left session state: tokens=%d cached=%d grants=%d",
			len(s.authTokens), len(s.cachedPasswords), len(s.pendingGrants))
	}
}

// TestLockClearsSession guards that Lock tears down everything a prior Unlock
// established, including the pending grant, so the password key it carries
// cannot outlive the locked session.
func TestLockClearsSession(t *testing.T) {
	s, _, shutdown := newTServer(t, false)
	defer shutdown()
	if _, err := s.Unlock([]byte("apppass")); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	if err := s.Lock(); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	s.authMtx.RLock()
	defer s.authMtx.RUnlock()
	if len(s.pendingGrants) != 0 {
		t.Fatalf("Lock left %d pending grants", len(s.pendingGrants))
	}
	if len(s.authTokens) != 0 || len(s.cachedPasswords) != 0 {
		t.Fatalf("Lock left session credentials resident: tokens=%d cached=%d",
			len(s.authTokens), len(s.cachedPasswords))
	}
}

// TestLockLogoutError surfaces a Core.Logout failure rather than reporting a
// lock that did not happen.
func TestLockLogoutError(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()
	c.logoutErr = errors.New("logout boom")
	if err := s.Lock(); err == nil {
		t.Fatal("Lock did not surface the Logout error")
	}
}

// TestDeauthClearsPendingGrants guards the fix that deauth (used by logout and
// change-password) drops pending grants, so a grant minted against a token that
// deauth is removing cannot be consumed afterward.
func TestDeauthClearsPendingGrants(t *testing.T) {
	s, _, shutdown := newTServer(t, false)
	defer shutdown()
	if _, err := s.Unlock([]byte("apppass")); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	s.deauth()

	s.authMtx.RLock()
	defer s.authMtx.RUnlock()
	if len(s.pendingGrants) != 0 {
		t.Fatalf("deauth left %d pending grants", len(s.pendingGrants))
	}
}

// TestChangeAppPassClearsPendingGrants exercises the change-password handler
// end to end: it runs under sessionMtx without deadlocking and drops any grant
// left by a prior Unlock.
func TestChangeAppPassClearsPendingGrants(t *testing.T) {
	s, _, shutdown := newTServer(t, false)
	defer shutdown()
	if _, err := s.Unlock([]byte("oldpass")); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	body, err := json.Marshal(map[string]encode.PassBytes{
		"appPW":    encode.PassBytes("oldpass"),
		"newAppPW": encode.PassBytes("newpass"),
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", newTestBody(body))
	s.apiChangeAppPass(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("change app pass failed: %d %s", rec.Code, rec.Body.String())
	}
	s.authMtx.RLock()
	defer s.authMtx.RUnlock()
	if len(s.pendingGrants) != 0 {
		t.Fatalf("change-password left %d pending grants", len(s.pendingGrants))
	}
}

// newTestBody wraps a byte slice as an http request body.
func newTestBody(b []byte) *TReader { return &TReader{msg: b} }
