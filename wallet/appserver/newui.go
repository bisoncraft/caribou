//go:build !oldui

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import "embed"

const (
	newUI = true
	// site is the common prefix for the site resources with respect to this
	// appserver package.
	site = "newui"
)

//go:embed newui/dist newui/src/img newui/src/font
var staticSiteRes embed.FS
