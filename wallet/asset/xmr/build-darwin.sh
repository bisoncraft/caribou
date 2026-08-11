#!/usr/bin/env bash
# Builds libmonero_wallet2_api_c for the two macOS desktop triples and stages the
# dylibs into wallet/asset/xmr/lib/darwin-amd64 and lib/darwin-arm64. This is the
# native-macOS analogue of the Dockerfile's darwin targets: the Dockerfile builds
# them in Debian, which forces simplybs to download Xcode (2.7 GB) and build a
# native `xar` from source to unpack the .xip. On macOS the system `xar`/`pbzx`
# unpack the xip directly, and a locally-downloaded xip is reused, so this path is
# faster and avoids the Linux-only xar/zlib snag. Pinned to monero_c v0.18.4.6-RC2
# (Monero 0.18.4.6), same as the Dockerfile and build-ios.sh.
#
#   cd wallet/asset/xmr && XCODE_XIP=~/Downloads/Xcode_26.1_Universal.xip ./build-darwin.sh
#
# Output (SHARED dylibs, install-name rewritten to @executable_path so the
# caribou binary finds the lib next to the executable — cgo adds no LC_RPATH):
#   lib/darwin-amd64/libmonero_wallet2_api_c.dylib  (Intel)
#   lib/darwin-arm64/libmonero_wallet2_api_c.dylib  (Apple Silicon)
#
# ============================================================================
# APPLE SDK REQUIREMENT (read before running)
# ----------------------------------------------------------------------------
# Like build-ios.sh, simplybs does NOT use this Mac's installed Xcode. Its
# native/apple-sdk package wants a specific, sha256-pinned Xcode_26.1 xip and
# builds an osxcross toolchain from it. Provide the xip you already have with
# XCODE_XIP=/path/to/Xcode_26.1_Universal.xip; it is staged into simplybs' source
# cache (reused if already present, so you download it from Apple only once).
# Without it the build fails in the native/apple-sdk step.
# ============================================================================
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"     # wallet/asset/xmr
repo_root="$(cd "$here/../../.." && pwd)"

if [[ "$(uname)" != "Darwin" ]]; then
    echo "error: build-darwin.sh must run on macOS (the native-SDK path)." >&2
    echo "  Use the Dockerfile's darwin targets on Linux instead." >&2
    exit 1
fi
command -v git  >/dev/null || { echo "error: git not found." >&2; exit 1; }
command -v go   >/dev/null || { echo "error: go not found (simplybs runs Go)." >&2; exit 1; }
command -v install_name_tool >/dev/null || { echo "error: install_name_tool not found (need Xcode CLT)." >&2; exit 1; }

PIN="3bfb3856a838f2bf6b729501837bb0295dedf25d"   # == tag v0.18.4.6-RC2
TRIPLES=(x86_64-apple-darwin aarch64-apple-darwin)
# Where the .dylib for each triple is staged in-repo (matches the lib/ layout).
declare -A LIBDIR=(
    [x86_64-apple-darwin]="$here/lib/darwin-amd64"
    [aarch64-apple-darwin]="$here/lib/darwin-arm64"
)

# monero_c checkout. Override with MONERO_C_DIR; defaults to a gitignored build
# dir under this folder. To skip a fresh (large) clone you can reuse the iOS
# checkout:  MONERO_C_DIR="$here/.monero_c-ios" ./build-darwin.sh
MONERO_C_DIR="${MONERO_C_DIR:-$here/.monero_c-darwin}"
JOBS="${JOBS:--j$(sysctl -n hw.physicalcpu)}"

# apply a patch only if it is not already applied (so re-runs are safe).
apply_once() { # $1=dir-to-run-in  $2=patch-file
    local dir="$1" patch="$2"
    ( cd "$dir"
      if git apply --reverse --check "$patch" >/dev/null 2>&1; then
          echo "  (already applied: $(basename "$patch"))"
      else
          git apply "$patch"
          echo "  applied: $(basename "$patch")"
      fi )
}

# Prepare (clone + submodule + patch), gated on a success sentinel so an
# interrupted prepare re-runs cleanly. The heavy build below is outside the gate;
# simplybs caches its own work, so re-running resumes it.
if [[ ! -f "$MONERO_C_DIR/.caribou-prepared" ]]; then
    if [[ ! -d "$MONERO_C_DIR/.git" ]]; then
        echo "==> cloning monero_c into $MONERO_C_DIR"
        git clone https://github.com/MrCyjaneK/monero_c.git "$MONERO_C_DIR"
        ( cd "$MONERO_C_DIR" && git checkout "$PIN" )
    else
        echo "==> resuming interrupted prepare; resetting tracked files to pristine"
        ( cd "$MONERO_C_DIR"
          git checkout "$PIN" --force
          git reset --hard
          git submodule foreach --recursive 'git reset --hard' 2>/dev/null || true )
    fi

    echo "==> fetching monero submodule (recursive)"
    ( cd "$MONERO_C_DIR" && git submodule update --init --recursive --force monero )

    echo "==> applying source patches"
    ( cd "$MONERO_C_DIR" && ./apply_patches.sh monero )
    apply_once "$MONERO_C_DIR/monero" "$here/0001-add-allow_mismatched_daemon_version.patch"
    apply_once "$MONERO_C_DIR"        "$here/0002-add-wallet2_api_c-wrapper.patch"

    # Apple builds are SHARED and gate exported symbols on the .exp list. Add our
    # wrapper symbol so MONERO_Wallet_setAllowMismatchedDaemonVersion is visible
    # in the dylib, else the Go binding's call is undefined at the app link.
    exp="$MONERO_C_DIR/monero_libwallet2_api_c/monero_libwallet2_api_c.exp"
    if ! grep -q '_MONERO_Wallet_setAllowMismatchedDaemonVersion' "$exp"; then
        sed -i.bak -e '/_MONERO_Wallet_setTrustedDaemon/a\
_MONERO_Wallet_setAllowMismatchedDaemonVersion' "$exp"
        rm -f "$exp.bak"
        echo "  added export symbol to $(basename "$exp")"
    fi

    echo "==> fetching simplybs + robustness patches"
    ( cd "$MONERO_C_DIR/contrib/depends" && make simplybs )
    apply_once "$MONERO_C_DIR/contrib/depends" "$here/0004-simplybs-retry-mirrors-only.patch"
    apply_once "$MONERO_C_DIR/contrib/depends" "$here/0005-depends-keep-simplybs-checkout.patch"
    apply_once "$MONERO_C_DIR/contrib/depends" "$here/0006-simplybs-android-16k-pages.patch"

    touch "$MONERO_C_DIR/.caribou-prepared"
else
    echo "==> reusing prepared checkout at $MONERO_C_DIR (skipping clone/patch)"
fi

# Stage the Apple SDK xip into simplybs' source cache. simplybs reuses any blob
# already at its cache path and only sha256-verifies on its own download, so a
# pre-placed file is trusted as-is; we warn (not fail) on a hash mismatch in case
# Apple re-released.
XIP_URLPATH="download.developer.apple.com/Developer_Tools/Xcode_26.1/XcodeXIP_26.1_Universal.xip"
XIP_SHA256="434287e1e4c6ecbd6d5ea07e9f408fffcad2b82916b0a3fdfefa7fe60db4de93"
XIP_CACHE="$MONERO_C_DIR/contrib/depends/simplybs/.buildlib/source/$XIP_URLPATH"
if [[ -n "${XCODE_XIP:-}" ]]; then
    if [[ ! -f "$XCODE_XIP" ]]; then
        echo "error: XCODE_XIP=$XCODE_XIP not found." >&2
        exit 1
    fi
    got="$(shasum -a 256 "$XCODE_XIP" | awk '{print $1}')"
    if [[ "$got" != "$XIP_SHA256" ]]; then
        echo "  warning: XCODE_XIP sha256 $got != pinned $XIP_SHA256" >&2
        echo "  (proceeding; Apple may have re-released, or this is a different build)" >&2
    fi
    # (Re)stage into the cache whenever it is absent OR a different size. simplybs
    # only checks a source blob's *existence* before reusing it, so a truncated
    # xip left by an interrupted download would otherwise be reused as-is and fail
    # to extract — replacing it here is what keeps the build from getting stuck at
    # the Apple SDK step on a re-run.
    if [[ ! -f "$XIP_CACHE" ]] || [[ "$(stat -f%z "$XIP_CACHE")" != "$(stat -f%z "$XCODE_XIP")" ]]; then
        echo "==> staging Apple SDK xip into simplybs source cache"
        mkdir -p "$(dirname "$XIP_CACHE")"
        cp "$XCODE_XIP" "$XIP_CACHE"
        echo "  staged -> ${XIP_CACHE#$MONERO_C_DIR/}"
    else
        echo "==> Apple SDK xip already staged (matching size), reusing"
    fi
elif [[ ! -f "$XIP_CACHE" ]]; then
    echo "warning: no XCODE_XIP set and no cached Apple SDK xip at" >&2
    echo "  $XIP_CACHE" >&2
    echo "Pass XCODE_XIP=/path/to/Xcode_26.1_Universal.xip (see the header)." >&2
fi

# Build each desktop triple. Each builds its full cross toolchain + deps from
# source via simplybs (long, tens of GB); sbs-cleanup frees intermediates.
for triple in "${TRIPLES[@]}"; do
    echo "==> build_single.sh monero $triple"
    ( cd "$MONERO_C_DIR" && ./build_single.sh monero "$triple" "$JOBS" )
    ( cd "$MONERO_C_DIR/contrib/depends" && make sbs-cleanup )

    dylib="$(find "$MONERO_C_DIR/release" -path "*/$triple/libmonero_wallet2_api_c.dylib" -print -quit)"
    if [[ -z "$dylib" ]]; then
        echo "error: no libmonero_wallet2_api_c.dylib produced for $triple" >&2
        echo "(looked under $MONERO_C_DIR/release/*/$triple/)" >&2
        exit 1
    fi

    # simplybs links the dylib with install_name @rpath/libmonero_wallet2_api_c.dylib.
    # cgo rejects -Wl,-rpath, so the caribou binary carries no LC_RPATH for @rpath
    # to resolve against. Rewrite the id to @executable_path/... so the loader
    # finds the dylib next to the executable (matches the desktop runtime layout
    # and what the Dockerfile does for these targets).
    install_name_tool -id "@executable_path/libmonero_wallet2_api_c.dylib" "$dylib"

    dest="${LIBDIR[$triple]}"
    mkdir -p "$dest"
    cp "$dylib" "$dest/"
    echo "  staged $(basename "$dylib") -> ${dest#$repo_root/}/"
done

echo
echo "Done. Staged desktop monero dylibs:"
for triple in "${TRIPLES[@]}"; do
    echo "  ${LIBDIR[$triple]#$repo_root/}/libmonero_wallet2_api_c.dylib"
done
