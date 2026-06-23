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

# arm64 covers modern devices. Add other ABIs (e.g. amd64 for the emulator) to
# -target as needed: -target=android/arm64,android/amd64
# -Wl,-z,max-page-size=16384 makes the produced .so use 16 KB-aligned LOAD
# segments, required by Android 15+ (16 KB pages) and Google Play for API 35+.
# NDK r26's lld defaults to 4 KB, so it must be passed explicitly.
gomobile bind \
    -target=android/arm64 \
    -androidapi 21 \
    -tags mobile \
    -ldflags="-extldflags=-Wl,-z,max-page-size=16384" \
    -o "$out" \
    ./wallet/mobile

echo "Wrote $out"
