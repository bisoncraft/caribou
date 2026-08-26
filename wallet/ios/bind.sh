#!/usr/bin/env bash
# Builds the gomobile-bound framework from wallet/mobile into
# Frameworks/Mobile.xcframework, consumed by the Xcode project (project.yml).
#
# Must run on macOS (the iOS toolchain is Xcode-only).
#
# Prereqs:
#   - Xcode + command line tools.
#   - gomobile + gobind on PATH:  go install golang.org/x/mobile/cmd/gomobile@latest
#                                 go install golang.org/x/mobile/cmd/gobind@latest
#                                 gomobile init
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
out="$here/Frameworks/Mobile.xcframework"

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
# blank page. Set CARIBOU_SKIP_WEB_BUILD=1 to skip (e.g. you just built it).
if [[ "${CARIBOU_SKIP_WEB_BUILD:-}" != "1" ]]; then
    echo "Building web bundle (npm run build in wallet/appserver)..." >&2
    (cd "$repo_root/wallet/appserver" && npm run build)
fi

mkdir -p "$here/Frameworks"
# gomobile refuses to overwrite an existing xcframework directory; clear it.
rm -rf "$out"

cd "$repo_root"

# XMR is opt-in: it links the native libmonero_wallet2_api_c, which must be
# built for the two iOS triples first (wallet/asset/xmr/build-ios.sh) and staged
# as per-triple dylibs. Default builds omit it so a plain ./bind.sh always works.
# Enable with CARIBOU_XMR=1.
#
# Note the split from Android: gomobile binds the Go core as a c-archive, which
# does not resolve -l libraries at bind time (the monero symbols are resolved by
# Xcode when it links the app). So bind.sh does NOT hand the monero library to
# gomobile; it only (a) adds the xmr build tag and (b) assembles the monero
# dylibs into Monero.xcframework so the Xcode project (project.xmr.yml) can embed
# and sign it. See wallet/asset/xmr/cxmr/cgo_ios.go.
tags="mobile"
if [[ "${CARIBOU_XMR:-}" == "1" ]]; then
    tags="mobile xmr"
    xmrlibdir="$repo_root/wallet/asset/xmr/lib"
    dev="$xmrlibdir/ios-arm64/libmonero_wallet2_api_c.dylib"
    sim="$xmrlibdir/ios-arm64-simulator/libmonero_wallet2_api_c.dylib"
    for f in "$dev" "$sim"; do
        if [[ ! -f "$f" ]]; then
            echo "error: CARIBOU_XMR=1 but $f is missing." >&2
            echo "Build the iOS monero libs first: wallet/asset/xmr/build-ios.sh" >&2
            echo "(needs macOS + the Apple SDK provisioned into simplybs)." >&2
            exit 1
        fi
    done

    # Wrap a dylib as a minimal Monero.framework: copy the dylib to the bundle's
    # executable name, retarget its install id to @rpath so the embedded copy is
    # found at runtime, and drop a minimal Info.plist. xcodebuild -create-
    # xcframework then packs the device + simulator frameworks into one bundle
    # and Xcode selects the right slice per destination.
    wrap_framework() { # $1=dylib  $2=out framework dir  $3=SupportedPlatform
        local dylib="$1" fw="$2" platform="$3"
        rm -rf "$fw"; mkdir -p "$fw"
        cp "$dylib" "$fw/Monero"
        install_name_tool -id "@rpath/Monero.framework/Monero" "$fw/Monero"
        cat > "$fw/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleDevelopmentRegion</key><string>en</string>
  <key>CFBundleExecutable</key><string>Monero</string>
  <key>CFBundleIdentifier</key><string>com.bisoncraft.Monero</string>
  <key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
  <key>CFBundleName</key><string>Monero</string>
  <key>CFBundlePackageType</key><string>FMWK</string>
  <!-- Three components maximum. Upstream monero is 0.18.4.6, but App Store
       validation rejects the fourth: "The value for key
       CFBundleShortVersionString '0.18.4.6' ... must be a period-separated list
       of at most three non-negative integers" (409, VALIDATION_ERROR). The
       exact upstream version is preserved in MoneroUpstreamVersion below. -->
  <key>CFBundleShortVersionString</key><string>0.18.4</string>
  <key>CFBundleVersion</key><string>1</string>
  <!-- Informational; not read by iOS, but keeps the real pin discoverable in
       the shipped bundle. Matches the PIN in ../asset/xmr/build-ios.sh. -->
  <key>MoneroUpstreamVersion</key><string>0.18.4.6</string>
  <key>MinimumOSVersion</key><string>15.0</string>
  <key>CFBundleSupportedPlatforms</key><array><string>$platform</string></array>
</dict></plist>
PLIST
    }

    stage="$here/Frameworks/.monero-staging"
    rm -rf "$stage"; mkdir -p "$stage/device" "$stage/sim"
    wrap_framework "$dev" "$stage/device/Monero.framework" "iPhoneOS"
    wrap_framework "$sim" "$stage/sim/Monero.framework" "iPhoneSimulator"

    monoxc="$here/Frameworks/Monero.xcframework"
    rm -rf "$monoxc"
    xcodebuild -create-xcframework \
        -framework "$stage/device/Monero.framework" \
        -framework "$stage/sim/Monero.framework" \
        -output "$monoxc"
    rm -rf "$stage"
    echo "XMR enabled: assembled Monero.xcframework (device + simulator)." >&2
fi

# Device (ios) + Simulator (iossimulator) so the dev loop works on both real
# devices and the Simulator; gomobile packs the slices into one .xcframework.
# arm64 covers modern devices and Apple-silicon simulators.
gomobile bind \
    -target=ios,iossimulator \
    -tags "$tags" \
    -o "$out" \
    ./wallet/mobile

echo "Wrote $out"
if [[ "${CARIBOU_XMR:-}" == "1" ]]; then
    echo "XMR: generate the project with the XMR overlay so Xcode links monero:" >&2
    echo "  xcodegen generate --spec project.xmr.yml" >&2
fi
