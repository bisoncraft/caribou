// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build xmr && ios && cgo

package cxmr

// On iOS, monero_c/simplybs builds a static archive (.a) rather than a shared
// library (triples aarch64-apple-ios for device and
// aarch64-apple-ios-simulator for the simulator). The device archive lives in
// wallet/asset/xmr/lib/ios-arm64/; the cgo LDFLAGS in wallet.go add its dir and
// the global -lmonero_wallet2_api_c resolves the archive.
//
// Static linking is deliberate: iOS bars dlopen of arbitrary loose .dylibs, so
// a static archive linked into the gomobile .xcframework (or the app target)
// sidesteps that. libc++ is linked into the framework.
//
// The simulator slice (aarch64-apple-ios-simulator) is a separate archive; both
// device and simulator slices are combined into the xcframework at package time
// (see xmr.md Slice 3). Any transitive deps monero_c pulls in (boost, openssl,
// unbound, etc.) link from the same archive set the build emits.
//
// The iOS lib build needs the Apple SDK provisioned into simplybs' native
// prefix, so it runs in a macOS CI job, not the Debian Dockerfile.
