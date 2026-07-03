#!/usr/bin/env bash
# Builds the gomobile-bound library from wallet/mobile into app/libs/mobile.aar.
#
# Prereqs:
#   - Android SDK + NDK installed; ANDROID_HOME (or ANDROID_SDK_ROOT) set.
#   - gomobile + gobind on PATH:  go install golang.org/x/mobile/cmd/gomobile@latest
#                                 go install golang.org/x/mobile/cmd/gobind@latest
#                                 gomobile init
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
out="$here/app/libs/mobile.aar"

# gomobile shells out to `go` for `go mod tidy` and the build. The root go.mod
# requires a recent Go; with the default GOTOOLCHAIN=auto, a `go` on PATH older
# than that requirement tries to *download* the matching toolchain, which fails
# with a confusing "toolchain not available" error when offline. Pin
# GOTOOLCHAIN=local (unless the caller set one) so the bind uses the installed
# toolchain directly, and fail early with a clear message if it is too old.
: "${GOTOOLCHAIN:=local}"
export GOTOOLCHAIN
need="$(awk '/^go [0-9]/{print $2; exit}' "$repo_root/go.mod")"
have="$(go env GOVERSION)"; have="${have#go}"
if [[ -n "$need" && "$(printf '%s\n%s\n' "$need" "$have" | sort -V | head -1)" != "$need" ]]; then
    echo "error: go.mod requires Go >= $need but 'go' is ${have:-unknown}." >&2
    echo "Install/select a newer Go, or set GOTOOLCHAIN=auto to fetch it (needs network)." >&2
    exit 1
fi

# The Go binary embeds wallet/appserver/mobile/dist (the built JS/CSS bundle).
# This script does not run webpack, so build the web bundle first; otherwise the
# embed picks up only the committed `placeholder` file and the app serves a
# blank page. Set BOSSW_SKIP_WEB_BUILD=1 to skip (e.g. you just built it).
if [[ "${BOSSW_SKIP_WEB_BUILD:-}" != "1" ]]; then
    echo "Building web bundle (npm run build in wallet/appserver)..." >&2
    (cd "$repo_root/wallet/appserver" && npm run build)
fi

mkdir -p "$here/app/libs"

cd "$repo_root"

# XMR is opt-in: it links the native libmonero_wallet2_api_c.so, which must be
# built for android first (wallet/asset/xmr/Dockerfile, aarch64-linux-android
# target) and staged as a jniLib. Default builds omit it so a plain ./bind.sh
# always works. Enable with BOSSW_XMR=1.
tags="mobile"
if [[ "${BOSSW_XMR:-}" == "1" ]]; then
    tags="mobile xmr"
    abi="arm64-v8a"
    xmrlib="$repo_root/wallet/asset/xmr/lib/android-arm64/libmonero_wallet2_api_c.so"
    jni="$here/app/src/main/jniLibs/$abi"
    if [[ ! -f "$xmrlib" ]]; then
        echo "error: BOSSW_XMR=1 but $xmrlib is missing." >&2
        echo "Build it first: the aarch64-linux-android target in" >&2
        echo "wallet/asset/xmr/Dockerfile, then drop the .so into that dir." >&2
        exit 1
    fi
    # cgo links -lmonero_wallet2_api_c from lib/android-arm64 at bind time; the
    # produced Go .so then carries a NEEDED entry the loader resolves from the
    # APK's jniLibs/<abi>/ at runtime. Stage the wallet lib there.
    mkdir -p "$jni"
    cp "$xmrlib" "$jni/"
    # Monero needs libc++_shared.so at runtime too. Pull it from the NDK that
    # gomobile links against so the ABI matches. Warn (do not fail) if it cannot
    # be located; the build still links, but the APK will crash without it.
    ndk="${ANDROID_NDK_HOME:-${ANDROID_NDK_ROOT:-}}"
    libcxx="$(find "$ndk" -path '*aarch64-linux-android/libc++_shared.so' 2>/dev/null | head -1)"
    if [[ -n "$libcxx" ]]; then
        cp "$libcxx" "$jni/"
    else
        echo "warning: libc++_shared.so not found under NDK ($ndk); the XMR" >&2
        echo "APK will need it in jniLibs/$abi/ or it will crash on launch." >&2
    fi
    echo "XMR enabled: staged $(basename "$xmrlib") into jniLibs/$abi/" >&2
fi

# arm64 covers modern devices. Add other ABIs (e.g. amd64 for the emulator) to
# -target as needed: -target=android/arm64,android/amd64
# -Wl,-z,max-page-size=16384 makes the produced .so use 16 KB-aligned LOAD
# segments, required by Android 15+ (16 KB pages) and Google Play for API 35+.
# NDK r26's lld defaults to 4 KB, so it must be passed explicitly.
gomobile bind \
    -target=android/arm64 \
    -androidapi 21 \
    -tags "$tags" \
    -ldflags="-extldflags=-Wl,-z,max-page-size=16384" \
    -o "$out" \
    ./wallet/mobile

echo "Wrote $out"
