// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !mobile

package appserver

import "embed"

// site is the bundle directory whose dist is served (js/css/index.html). The
// desktop build serves the newui bundle. Mirrors the storage-engine build-tag
// split (util/kvstore/engine_desktop.go).
const site = "newui"

// checkAppVersion enables the periodic GitHub release poll. The desktop build
// keeps it on; the mobile build turns it off (store-installed, so the poll is
// just wasted battery/radio).
const checkAppVersion = true

// printBrowserBanner prints the "open in your browser" banner to stdout on
// startup. Useful for the desktop CLI; pointless on mobile, where there is no
// console and the WebView loads the URL directly.
const printBrowserBanner = true

//go:embed newui/dist
var staticSiteRes embed.FS
