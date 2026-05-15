//go:build !oldui

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import "github.com/go-chi/chi/v5"

// registerLegacyWebRoutes is a no-op under the new UI: every page is served
// from the SPA shell via the mux.NotFound fallback in New.
func (s *AppServer) registerLegacyWebRoutes(_ *chi.Mux) {}

// buildTemplates is a no-op under the new UI: the SPA bundle replaces the
// server-rendered HTML templates used by the legacy UI.
func (s *AppServer) buildTemplates(_ string) error { return nil }
