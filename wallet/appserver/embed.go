// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import "embed"

// site is the common prefix for the SPA's bundled resources with respect to
// this appserver package.
const site = "newui"

//go:embed newui/dist newui/src/img newui/src/font
var staticSiteRes embed.FS
