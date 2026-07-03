// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build xmr && android && cgo

package cxmr

// On Android, libmonero_wallet2_api_c.so is built per ABI by monero_c/simplybs
// (triples aarch64-linux-android, x86_64-linux-android,
// armv7a-linux-androideabi) and dropped into wallet/asset/xmr/lib/android-arm64/
// (arm64-v8a), android-amd64/ (x86_64 emulator), or android-arm/ (armeabi-v7a).
// The cgo LDFLAGS in wallet.go select the dir by GOARCH at build time.
//
// simplybs targets NDK API level 21 and links libc++, so the app's minSdk must
// be >= 21 and libc++_shared.so must ship alongside the wallet lib.
//
// Packaging (see xmr.md Slice 3): ship libmonero_wallet2_api_c.so and
// libc++_shared.so as jniLibs/<abi>/ in the app module, next to the gomobile
// .aar. Android's loader resolves them from the installed APK's per-ABI lib dir
// at runtime, so no rpath is needed (and cgo rejects -Wl,-rpath anyway).
//
// Release note: every native .so (the gomobile Go .so, this wallet lib, and
// libc++_shared.so) must be linked 16 KB-page aligned
// (-Wl,-z,max-page-size=16384) for Google Play API 35+ submission. simplybs
// does not set this by default; it must be injected in the Slice 1 build.
