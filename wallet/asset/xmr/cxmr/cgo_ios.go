// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build xmr && ios && cgo

package cxmr

// iOS linking model (differs from every other platform, so read this before
// touching the cgo LDFLAGS in wallet.go).
//
// gomobile binds the Go core as a c-archive: the framework binary inside
// Mobile.xcframework is a static ar archive, not a linked dylib. A c-archive
// does NOT resolve -l libraries at build time; the monero wrapper symbols
// (MONERO_*) stay undefined in the archive and are resolved only when Xcode
// links the final app. So, unlike desktop/Android, nothing about the monero
// library is linked at `gomobile bind` time, and wallet.go intentionally adds
// no iOS -L/-l of its own for it.
//
// The monero library is therefore supplied to the *app* link, not the bind:
//
//   - monero_c/simplybs builds libmonero_wallet2_api_c per iOS triple
//     (aarch64-apple-ios for device, aarch64-apple-ios-simulator for the
//     Simulator). build_single.sh emits a SHARED library (.dylib); iOS allows
//     an embedded, signed dynamic framework (it only bars dlopen of arbitrary
//     loose .dylibs), which is what Cake Wallet ships too.
//   - wallet/asset/xmr/build-ios.sh wraps each triple's dylib as a
//     Monero.framework and packs the device + simulator slices into
//     Monero.xcframework, staging it under wallet/ios/Frameworks/.
//   - wallet/ios/project.xmr.yml adds that xcframework as an embedded, signed
//     app dependency. Xcode selects the right slice per destination, so the
//     arm64 device/simulator ambiguity that would defeat a cgo -L never arises.
//   - libc++ is a system dylib on iOS; the dynamic monero framework links it
//     itself (simplybs passes -lc++ -lc++abi), so nothing extra is bundled.
//
// Build with: cd wallet/ios && BOSSW_XMR=1 ./bind.sh  (see that script and
// wallet/ios/README.md). The library build needs the Apple SDK provisioned into
// simplybs' native prefix, so it runs on macOS, not the Debian Dockerfile that
// produces the desktop/Android libs.
