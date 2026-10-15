// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import (
	_ "embed"
	"errors"
	"fmt"
	"net/http"
)

// termsVersion identifies the current Terms of Use and Privacy Policy. Bump it
// whenever either document changes, so that every user is asked to accept the
// new text.
const termsVersion = "2026-09-21"

//go:embed legal/terms.md
var termsText string

//go:embed legal/privacy.md
var privacyText string

// errTermsNotAccepted is returned when the wallet would be initialized before
// the user accepted the current terms.
var errTermsNotAccepted = errors.New("the terms of use must be accepted before the wallet can be created")

// termsAccepted reports whether the current terms version has been accepted.
// Only a positive answer is cached: the version is a compile-time constant, so
// once accepted it stays accepted for the life of the process, while a negative
// answer is re-read until the user accepts.
func (s *AppServer) termsAccepted() (bool, error) {
	if s.termsAcceptedCache.Load() {
		return true, nil
	}
	accepted, err := s.core.AcceptedTermsVersion()
	if err != nil {
		return false, err
	}
	if accepted != termsVersion {
		return false, nil
	}
	s.termsAcceptedCache.Store(true)
	return true, nil
}

// apiTerms serves the current Terms of Use and Privacy Policy. It is
// unauthenticated because the init wizard shows it before an account exists.
func (s *AppServer) apiTerms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, &struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Terms   string `json:"terms"`
		Privacy string `json:"privacy"`
	}{
		OK:      true,
		Version: termsVersion,
		Terms:   termsText,
		Privacy: privacyText,
	})
}

// apiAcceptTerms records that the user accepted the current terms. Before the
// wallet is initialized there is no login to check and the caller is trusted:
// the same local caller could drive the UI, and apiInit is what requires the
// acceptance. Afterwards the caller must be logged in.
func (s *AppServer) apiAcceptTerms(w http.ResponseWriter, r *http.Request) {
	if s.core.IsInitialized() && !s.isAuthed(r) {
		http.Error(w, "not authorized - login first", http.StatusUnauthorized)
		return
	}
	var form struct {
		Version string `json:"version"`
	}
	if !readPost(w, r, &form) {
		return
	}
	// Reject any version but the current one, so a stale client cannot record
	// acceptance of text the user was not shown.
	if form.Version != termsVersion {
		s.writeAPIError(w, fmt.Errorf("terms version %q is not the current version", form.Version))
		return
	}
	if err := s.core.SetAcceptedTermsVersion(termsVersion); err != nil {
		s.writeAPIError(w, fmt.Errorf("error saving terms acceptance: %w", err))
		return
	}
	s.termsAcceptedCache.Store(true)
	writeJSON(w, simpleAck())
}
