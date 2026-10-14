// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import "embed"

// sharedAssetsDir is the prefix of the shared assets within sharedAssets, and
// the directory name on disk (a sibling of the desktop/mobile bundles).
const sharedAssetsDir = "shared"

// sharedAssets holds static assets (images, fonts) referenced by the SPAs as
// runtime URLs (/img/..., /font/...) and served as-is. They are common to both
// the desktop and mobile bundles, so they live in one place rather than
// being duplicated per bundle. The per-bundle built output (dist) is embedded
// separately by the build-tagged embed_desktop.go / embed_mobile.go.
//
//go:embed shared/img shared/font
var sharedAssets embed.FS
