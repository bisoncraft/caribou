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
	"time"

	"github.com/bisoncraft/caribou/util/encode"
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

// respCookie returns the named response cookie, or nil if it was not set. Unlike
// grantCookie it distinguishes an absent cookie from a cleared one, which is set
// with an empty value.
func respCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
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
// lock that did not happen, but still tears the session down: the native callers
// show the locked UI regardless of the return, so a "locked" screen must never
// sit over a still-valid server-side session.
func TestLockLogoutError(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()
	if _, err := s.Unlock([]byte("apppass")); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	c.logoutErr = errors.New("logout boom")
	if err := s.Lock(); err == nil {
		t.Fatal("Lock did not surface the Logout error")
	}

	// deauth must have run despite the Logout error: no session state may survive.
	s.authMtx.RLock()
	defer s.authMtx.RUnlock()
	if len(s.authTokens) != 0 || len(s.cachedPasswords) != 0 || len(s.pendingGrants) != 0 {
		t.Fatalf("Lock left session state after a Logout error: tokens=%d cached=%d grants=%d",
			len(s.authTokens), len(s.cachedPasswords), len(s.pendingGrants))
	}
}

// TestLogoutClearsCookiesOnLogoutError guards that apiLogout expires the session
// cookies even when core.Logout errors. Lock deauths unconditionally, so the
// session those cookies name is gone either way; leaving them set would point the
// client at a destroyed session, turning a retry into a confusing auth failure
// instead of a clean re-login. The Logout error is still reported.
func TestLogoutClearsCookiesOnLogoutError(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()
	if _, err := s.Unlock([]byte("apppass")); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	c.logoutErr = errors.New("logout boom")

	rec := httptest.NewRecorder()
	s.apiLogout(rec, httptest.NewRequest(http.MethodPost, "/api/logout", nil))

	for _, name := range []string{authCK, pwKeyCK} {
		ck := respCookie(rec, name)
		if ck == nil {
			t.Fatalf("%s cookie was not cleared after a Logout error", name)
		}
		if ck.Value != "" || !ck.Expires.Before(time.Now()) {
			t.Fatalf("%s cookie not expired: value=%q expires=%v", name, ck.Value, ck.Expires)
		}
	}

	// The Logout error must still reach the caller.
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OK {
		t.Fatal("apiLogout reported success despite a Logout error")
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

// postResetAppPass drives apiResetAppPassword with a new password and seed.
func postResetAppPass(t *testing.T, s *AppServer) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"newPass": encode.PassBytes("newpass"),
		"seed":    "seed words",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", newTestBody(body))
	s.apiResetAppPassword(rec, req)
	return rec
}

// sessionState reports how many auth tokens, cached passwords and pending
// grants the server holds.
func sessionState(s *AppServer) (tokens, cached, grants int) {
	s.authMtx.RLock()
	defer s.authMtx.RUnlock()
	return len(s.authTokens), len(s.cachedPasswords), len(s.pendingGrants)
}

// TestResetAppPassDeauths checks that a successful seed reset tears down every
// session minted under the old password, rather than leaving one authenticated
// with a cached password that no longer decrypts anything.
func TestResetAppPassDeauths(t *testing.T) {
	s, _, shutdown := newTServer(t, false)
	defer shutdown()
	if _, err := s.Unlock([]byte("oldpass")); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if tokens, cached, grants := sessionState(s); tokens == 0 || cached == 0 || grants == 0 {
		t.Fatalf("Unlock left no session to tear down: %d tokens, %d cached, %d grants", tokens, cached, grants)
	}

	if rec := postResetAppPass(t, s); rec.Code != http.StatusOK {
		t.Fatalf("reset app pass failed: %d %s", rec.Code, rec.Body.String())
	}
	if tokens, cached, grants := sessionState(s); tokens != 0 || cached != 0 || grants != 0 {
		t.Fatalf("reset left %d tokens, %d cached passwords, %d pending grants", tokens, cached, grants)
	}
}

// TestResetAppPassFailureKeepsSession checks that a rejected reset (a wrong
// seed) leaves the live session alone: the password did not change, so the
// session is still valid.
func TestResetAppPassFailureKeepsSession(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()
	if _, err := s.Unlock([]byte("oldpass")); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	wantTokens, wantCached, wantGrants := sessionState(s)

	tCore.resetAppPassErr = errors.New("incorrect seed")
	// writeAPIError answers 200 with ok=false.
	rec := postResetAppPass(t, s)
	var resp struct{ OK bool }
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	if resp.OK {
		t.Fatalf("reset with a failing core reported success")
	}
	if tokens, cached, grants := sessionState(s); tokens != wantTokens || cached != wantCached || grants != wantGrants {
		t.Fatalf("failed reset changed the session: %d/%d/%d tokens/cached/grants, want %d/%d/%d",
			tokens, cached, grants, wantTokens, wantCached, wantGrants)
	}
}

// newTestBody wraps a byte slice as an http request body.
func newTestBody(b []byte) *TReader { return &TReader{msg: b} }
