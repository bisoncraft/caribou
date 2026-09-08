#!/usr/bin/env bash
# Builds libmonero_wallet2_api_c for the two macOS desktop triples and stages the
# dylibs into wallet/asset/xmr/lib/darwin-amd64 and lib/darwin-arm64. This is the
# native-macOS analogue of the Dockerfile's darwin targets, and it is a
# convenience, not a requirement: the Dockerfile builds these two on Linux and
# that is the reference path. Pinned to monero_c v0.18.4.6-RC2 (Monero
# 0.18.4.6), same as the Dockerfile and build-ios.sh.
#
# What running here actually saves is compiling `xar` and `pbzx` from source to
# unpack the xip, since macOS ships both. It does NOT save you the xip itself.
# Neither path can download that (see the SDK box below), so both need you to
# supply it; this one takes it from XCODE_XIP=, the Dockerfile from xcode/.
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

# Prepare (clone + submodule + monero_c's own patches), gated on a success
# sentinel so an interrupted prepare re-runs cleanly. Only the steps that are
# expensive or not idempotent live inside the gate; our own patches are applied
# below it on every run. The heavy build further down is outside it too, since
# simplybs caches its own work and re-running resumes it.
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

    echo "==> applying monero_c's own patches"
    ( cd "$MONERO_C_DIR" && ./apply_patches.sh monero )
    touch "$MONERO_C_DIR/.caribou-prepared"
else
    echo "==> reusing prepared checkout at $MONERO_C_DIR (skipping clone/patch)"
fi

# Our patches are applied on every run, not only on a fresh prepare. apply_once
# is a no-op once a patch is in, and gating them on the sentinel meant a
# checkout prepared by an earlier revision of this script silently never picked
# up a patch added to the list afterwards.
#   0001 adds Wallet::setAllowMismatchedDaemonVersion in the monero submodule;
#   0002 exposes it through the wallet2_api_c C wrapper.
#   0010 recovers HOST_ABI when CMake throws its own cache away mid-build. Not
#        Docker-specific: this script drives the same build_single.sh over the
#        same CMakeLists.txt, so it hits the same intermittent syntax error.
echo "==> applying caribou patches"
apply_once "$MONERO_C_DIR/monero" "$here/0001-add-allow_mismatched_daemon_version.patch"
apply_once "$MONERO_C_DIR"        "$here/0002-add-wallet2_api_c-wrapper.patch"
apply_once "$MONERO_C_DIR"        "$here/0010-cmake-recover-host-abi.patch"

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

#   0004/0007 keep GNU source downloads working through mirror flakes and
#        ftpmirror.gnu.org's host-wide outages;
#   0005 stops the depends Makefile reverting those on the next build;
#   0009 stops `make sbs-cleanup` deleting simplybs' source cache. That one is
#        load-bearing here, not just a speed-up: the loop below runs sbs-cleanup
#        between triples, and the cache is where the xip staged just below
#        lives, so without 0009 the second triple finds no xip and fails in
#        native/apple-sdk.
#   0011 re-checks a cached archive against its pinned sha256 before reusing it.
#        With 0009 keeping the cache, a Ctrl-C mid-download would otherwise
#        leave a truncated tarball that every later run reuses and fails to
#        extract, with no way out but deleting it by hand.
echo "==> fetching simplybs + robustness patches"
( cd "$MONERO_C_DIR/contrib/depends" && make simplybs )
apply_once "$MONERO_C_DIR/contrib/depends" "$here/0004-simplybs-retry-mirrors-only.patch"
apply_once "$MONERO_C_DIR/contrib/depends" "$here/0005-depends-keep-simplybs-checkout.patch"
apply_once "$MONERO_C_DIR/contrib/depends" "$here/0007-simplybs-gnu-origin-fallback.patch"
apply_once "$MONERO_C_DIR/contrib/depends" "$here/0009-depends-keep-simplybs-source-cache.patch"
apply_once "$MONERO_C_DIR/contrib/depends" "$here/0011-simplybs-recheck-cached-sources.patch"

# Stage the Apple SDK xip into simplybs' source cache. simplybs reuses any blob
# already at its cache path and only sha256-verifies on its own download -- its
# "blob" extract path is a plain copy with no hash check -- so a pre-placed file
# is trusted as-is unless we check it here, which is what the block below does.
XIP_URLPATH="download.developer.apple.com/Developer_Tools/Xcode_26.1/XcodeXIP_26.1_Universal.xip"
XIP_SHA256="434287e1e4c6ecbd6d5ea07e9f408fffcad2b82916b0a3fdfefa7fe60db4de93"
XIP_CACHE="$MONERO_C_DIR/contrib/depends/simplybs/.buildlib/source/$XIP_URLPATH"
if [[ -n "${XCODE_XIP:-}" ]]; then
    if [[ ! -f "$XCODE_XIP" ]]; then
        echo "error: XCODE_XIP=$XCODE_XIP not found." >&2
        exit 1
    fi
    # Refuse a mismatch rather than warn past it. Nothing downstream will catch
    # it: simplybs verifies only what it downloaded itself, so this hash is the
    # single check standing between an arbitrary 2.8 GB file and the Apple
    # toolchain that compiles a wallet library. Override deliberately if Apple
    # really has re-released and the pin is being moved.
    got="$(shasum -a 256 "$XCODE_XIP" | awk '{print $1}')"
    if [[ "$got" != "$XIP_SHA256" ]]; then
        echo "error: XCODE_XIP sha256 does not match the pin." >&2
        echo "  got    $got" >&2
        echo "  wanted $XIP_SHA256  (Xcode 26.1)" >&2
        echo "  Xcode 26.1.1 hashes ed55d55f... and is NOT interchangeable." >&2
        echo "  Set XCODE_XIP_ALLOW_MISMATCH=1 to build from it anyway." >&2
        [[ "${XCODE_XIP_ALLOW_MISMATCH:-}" == "1" ]] || exit 1
        echo "  XCODE_XIP_ALLOW_MISMATCH=1 set, proceeding." >&2
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
# source via simplybs (long, tens of GB); sbs-cleanup frees the intermediates
# between them and, with 0009 applied above, leaves the source cache and the
# staged xip alone.
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
