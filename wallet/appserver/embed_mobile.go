// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build mobile

package appserver

import "embed"

// site is the bundle directory whose dist is served (js/css/index.html). The
// mobile build serves the mobile bundle. Mirrors the storage-engine build-tag
// split (util/kvstore/engine_mobile.go).
const site = "mobile"

// printBrowserBanner prints the "open in your browser" banner to stdout on
// startup. Useful for the desktop CLI; pointless on mobile, where there is no
// console and the WebView loads the URL directly.
const printBrowserBanner = false

//go:embed mobile/dist
var staticSiteRes embed.FS
