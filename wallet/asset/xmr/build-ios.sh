#!/usr/bin/env bash
# Builds libmonero_wallet2_api_c for the two iOS triples and stages the dylibs
# into wallet/asset/xmr/lib/ios-arm64{,-simulator}/. Pinned to monero_c
# v0.18.4.6-RC2 (Monero 0.18.4.6), same as the Dockerfile.
#
# This is a convenience path, not the only one. The Dockerfile builds these two
# triples too: simplybs' Apple SDK package is osxcross, a Linux-hosted Apple
# cross toolchain, and it is scoped to any build machine.
#
# What this script buys you is not the xip. Both paths need one supplied, since
# neither can fetch it (see the SDK box below); this one takes it from
# XCODE_XIP=, the Dockerfile from xcode/. What it saves is compiling xar and
# pbzx from source, because macOS already has them. Use it when you are on a
# Mac with the xip on hand. No CI builds these today.
#
#   cd wallet/asset/xmr && ./build-ios.sh
#
# Output (SHARED dylibs; iOS embeds a signed dynamic framework, which is allowed
# — only dlopen of loose dylibs is barred — matching what Cake Wallet ships):
#   lib/ios-arm64/libmonero_wallet2_api_c.dylib            (device)
#   lib/ios-arm64-simulator/libmonero_wallet2_api_c.dylib  (simulator)
#
# Then package + wire into the app:
#   cd ../../ios && CARIBOU_XMR=1 ./bind.sh          # assembles Monero.xcframework
#   xcodegen generate --spec project.xmr.yml       # links it into the app
#
# ============================================================================
# APPLE SDK REQUIREMENT (read before running)
# ----------------------------------------------------------------------------
# monero_c builds via simplybs, which does NOT use this Mac's installed Xcode.
# Its native/apple-sdk package wants a specific, sha256-pinned
# Xcode_26.1_Universal.xip from download.developer.apple.com (Apple-login-gated,
# 2.8 GB) and builds an osxcross toolchain from it; SDK_VERSION is pinned to
# 26.1 and the iOS compile uses -isysroot .../SDK/iPhoneOS26.1.sdk.
#
# So before this can build the Apple targets you must make that xip available to
# simplybs' source cache (it is reused if already present, so an authenticated
# machine can download it once and drop it in). Without it, the build fails in
# the native/apple-sdk step. This is why the iOS lib build lives in a macOS CI
# job with Apple credentials rather than in the Dockerfile.
#
# NOTE: this script has been validated only for its patch/staging plumbing and
# the downstream Go/cgo compile (which is green). The full simplybs iOS build
# itself is unproven at this pin — the residual open item from xmr.md.
# ============================================================================
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"     # wallet/asset/xmr
repo_root="$(cd "$here/../../.." && pwd)"

if [[ "$(uname)" != "Darwin" ]]; then
    echo "error: build-ios.sh must run on macOS (needs the Apple/iOS toolchain)." >&2
    exit 1
fi
command -v git >/dev/null || { echo "error: git not found." >&2; exit 1; }
command -v go  >/dev/null || { echo "error: go not found (simplybs runs Go)." >&2; exit 1; }

PIN="3bfb3856a838f2bf6b729501837bb0295dedf25d"   # == tag v0.18.4.6-RC2
TRIPLES=(aarch64-apple-ios aarch64-apple-ios-simulator)
# Where the .dylib for each triple is staged in-repo (matches the lib/ layout).
# A case, not `declare -A`: associative arrays are bash 4, stock macOS ships
# bash 3.2, and this script runs nowhere but a Mac -- so the map aborted it on
# line one unless a Homebrew bash happened to precede /bin/bash in PATH.
# fetch-libs.sh avoids mapfile for the same reason, and says so.
libdir() {
    case "$1" in
        aarch64-apple-ios)           echo "$here/lib/ios-arm64" ;;
        aarch64-apple-ios-simulator) echo "$here/lib/ios-arm64-simulator" ;;
        *) echo "error: no lib/ directory mapped for triple '$1'." >&2; return 1 ;;
    esac
}

# monero_c checkout. Override with MONERO_C_DIR; defaults to a gitignored build
# dir under this folder. Reused across runs (kept out of git).
MONERO_C_DIR="${MONERO_C_DIR:-$here/.monero_c-ios}"
JOBS="${JOBS:--j$(sysctl -n hw.physicalcpu)}"

# Apply a patch once, from the repository ROOT with --directory, and fail loudly
# if it does not land.
#
# Running `cd contrib/depends && git apply 0011-...patch` looks like it works and
# does not. git apply treats a patch carrying a `diff --git a/x b/x` header as
# toplevel-relative, so it does not prepend the "contrib/depends/" prefix; the
# path then sits outside the current directory, git drops the patch, prints
# nothing without -v, and EXITS 0. That is what this function used to do, and
# 0007 and 0011 -- the only two of our patches carrying that header -- were
# silently never applied on any run. The other three have no `diff --git` line,
# get the prefix prepended, and did apply, which is what hid it.
#
# --directory gives every patch the same prefix regardless of its header, and the
# "Skipped patch" check is what makes a recurrence loud: git apply has no flag
# that turns a skip into a nonzero exit, so the message has to be read.
apply_once() { # $1=repo-root  $2=subdir the patch paths are relative to (or .)  $3=patch
    local root="$1" sub="$2" patch="$3" out
    local dirflag=""
    [[ "$sub" != "." ]] && dirflag="--directory=$sub"
    if git -C "$root" apply --reverse --check ${dirflag:+"$dirflag"} "$patch" >/dev/null 2>&1; then
        echo "  (already applied: $(basename "$patch"))"
        return 0
    fi
    if ! out="$(git -C "$root" apply -v ${dirflag:+"$dirflag"} "$patch" 2>&1)"; then
        echo "$out" >&2
        echo "error: failed to apply $(basename "$patch")" >&2
        exit 1
    fi
    case "$out" in
        *"Skipped patch"*)
            echo "$out" >&2
            echo "error: git apply SKIPPED $(basename "$patch") rather than applying it." >&2
            exit 1
            ;;
    esac
    echo "  applied: $(basename "$patch")"
}

# Prepare (clone + submodule + monero_c's own patches) is gated on a success
# sentinel rather than just ".git exists", so an interrupted prepare (e.g. a
# submodule fetch that got a connection reset) re-runs cleanly instead of being
# skipped with patches unapplied. Only the expensive or non-idempotent steps are
# inside the gate; our own patches are applied below it on every run. The heavy
# build further down is outside it too; simplybs caches its own work, so
# re-running resumes it.
# The sentinel records the pin it was prepared at, not merely that a prepare
# happened. `git checkout "$PIN"` runs only inside this gate, so with a bare
# touch an existing checkout silently stayed on the old commit after a PIN
# bump -- while publish-libs.sh carries its own PIN and would have labelled
# the resulting binaries with the new one in SHA256SUMS and the release notes.
prepared_pin=""
[[ -f "$MONERO_C_DIR/.caribou-prepared" ]] && prepared_pin="$(cat "$MONERO_C_DIR/.caribou-prepared")"
if [[ "$prepared_pin" != "$PIN" ]]; then
    if [[ -n "$prepared_pin" ]]; then
        echo "==> checkout was prepared at $prepared_pin, pin is now $PIN; re-preparing"
        rm -f "$MONERO_C_DIR/.caribou-prepared"
    fi
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

    # We only build the monero flavor, so init just the monero submodule (and its
    # nested externals). This skips wownero (codeberg) and zano, which are not
    # needed for `build_single.sh monero` and were the network-flake failure
    # point of `git submodule update --init --recursive` over all three.
    echo "==> fetching monero submodule (recursive)"
    ( cd "$MONERO_C_DIR" && git submodule update --init --recursive --force monero )

    echo "==> applying monero_c's own patches"
    ( cd "$MONERO_C_DIR" && ./apply_patches.sh monero )
    printf '%s\n' "$PIN" > "$MONERO_C_DIR/.caribou-prepared"
else
    echo "==> reusing prepared checkout at $MONERO_C_DIR, pin $PIN (skipping clone/patch)"
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
apply_once "$MONERO_C_DIR/monero" . "$here/0001-add-allow_mismatched_daemon_version.patch"
apply_once "$MONERO_C_DIR" . "$here/0002-add-wallet2_api_c-wrapper.patch"
apply_once "$MONERO_C_DIR" . "$here/0010-cmake-recover-host-abi.patch"

# Apple builds are SHARED and gate exported symbols on the .exp list (see
# monero_libwallet2_api_c/CMakeLists.txt: -exported_symbols_list). Add our
# wrapper symbol so it is visible in the iOS dylib, else the Go binding's
# call to MONERO_Wallet_setAllowMismatchedDaemonVersion is undefined at the
# app link. BSD sed (macOS) append syntax; also valid on GNU sed.
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
apply_once "$MONERO_C_DIR" contrib/depends "$here/0004-simplybs-retry-mirrors-only.patch"
apply_once "$MONERO_C_DIR" contrib/depends "$here/0005-depends-keep-simplybs-checkout.patch"
apply_once "$MONERO_C_DIR" contrib/depends "$here/0007-simplybs-gnu-origin-fallback.patch"
apply_once "$MONERO_C_DIR" contrib/depends "$here/0009-depends-keep-simplybs-source-cache.patch"
apply_once "$MONERO_C_DIR" contrib/depends "$here/0011-simplybs-recheck-cached-sources.patch"

# Stage the Apple SDK xip into simplybs' source cache (see the header). simplybs
# reuses any blob already at its cache path and only sha256-verifies on its own
# download -- its "blob" extract path is a plain copy with no hash check -- so a
# pre-placed file is trusted as-is unless we check it here, which is what the
# block below does. simplybs derives the cache
# path from the download URL host+path, and DataDirRoot defaults to
# <cwd>/.buildlib with cwd == the simplybs dir (contrib/depends/simplybs).
#   Provide the file with:  XCODE_XIP=/path/to/Xcode_26.1.xip ./build-ios.sh
# Read the pin rather than carry a fourth copy of it: the Dockerfile,
# xcode/README.md and the sibling build script all read the same file, so an
# Xcode bump is one edit instead of four that have to agree.
XIP_PIN_FILE="$here/xcode/PIN"
[[ -f "$XIP_PIN_FILE" ]] || { echo "error: $XIP_PIN_FILE not found." >&2; exit 1; }
XIP_SHA256="$(awk -F= '/^XCODE_XIP_SHA256=/ { print $2; exit }' "$XIP_PIN_FILE")"
XCODE_VERSION="$(awk -F= '/^XCODE_VERSION=/ { print $2; exit }' "$XIP_PIN_FILE")"
[[ -n "$XIP_SHA256" && -n "$XCODE_VERSION" ]] || {
    echo "error: $XIP_PIN_FILE does not define XCODE_VERSION and XCODE_XIP_SHA256." >&2
    exit 1
}
XIP_URLPATH="download.developer.apple.com/Developer_Tools/Xcode_$XCODE_VERSION/XcodeXIP_${XCODE_VERSION}_Universal.xip"
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
        echo "  wanted $XIP_SHA256  (Xcode $XCODE_VERSION)" >&2
        echo "  Xcode 26.1.1 hashes ed55d55f... and is NOT interchangeable." >&2
        echo "  Set XCODE_XIP_ALLOW_MISMATCH=1 to build from it anyway." >&2
        [[ "${XCODE_XIP_ALLOW_MISMATCH:-}" == "1" ]] || exit 1
        echo "  XCODE_XIP_ALLOW_MISMATCH=1 set, proceeding." >&2
    fi
    # (Re)stage into the cache whenever it is absent OR is not the file that was
    # just verified.
    # simplybs only checks a source blob's *existence* before reusing it, so a
    # truncated xip left by an interrupted download would otherwise be reused
    # as-is and fail to extract, and the build would stick at the Apple SDK step
    # on every re-run.
    #
    # Hash it rather than compare sizes. The hash computed above is of $XCODE_XIP,
    # which is not necessarily the file that gets consumed: a same-size blob
    # already in the cache was reused on the strength of a check that applied to a
    # different file. 2.8 GB is a few seconds of sha256 against a build that runs
    # for hours, and it is the only check standing between an arbitrary file and
    # the Apple toolchain that compiles the wallet library.
    if [[ ! -f "$XIP_CACHE" ]] || [[ "$(shasum -a 256 "$XIP_CACHE" | awk '{print $1}')" != "$XIP_SHA256" ]]; then
        echo "==> staging Apple SDK xip into simplybs source cache"
        mkdir -p "$(dirname "$XIP_CACHE")"
        cp "$XCODE_XIP" "$XIP_CACHE"
        echo "  staged -> ${XIP_CACHE#$MONERO_C_DIR/}"
    else
        echo "==> Apple SDK xip already staged (hash matches the pin), reusing"
    fi
elif [[ ! -f "$XIP_CACHE" ]]; then
    echo "warning: no XCODE_XIP set and no cached Apple SDK xip at" >&2
    echo "  $XIP_CACHE" >&2
    echo "The build will fail in the native/apple-sdk step. Download" >&2
    echo "Xcode 26.1.xip from https://developer.apple.com/download/all/ and pass" >&2
    echo "XCODE_XIP=/path/to/Xcode_26.1.xip (see the header of this script)." >&2
else
    # A cached xip with no XCODE_XIP to check it against. simplybs verifies only
    # what it downloaded itself and this blob was placed by hand, so without the
    # check here nothing between an arbitrary 2.8 GB file and the Apple toolchain
    # ever looks at it. Do not hard-error: this is the ordinary re-run path, and
    # the mismatch may be a deliberate XCODE_XIP_ALLOW_MISMATCH staged earlier.
    cached="$(shasum -a 256 "$XIP_CACHE" | awk '{print $1}')"
    if [[ "$cached" == "$XIP_SHA256" ]]; then
        echo "==> using cached Apple SDK xip (hash matches the pin)"
    else
        echo "warning: the cached Apple SDK xip does not match the pin." >&2
        echo "  got    $cached" >&2
        echo "  wanted $XIP_SHA256  (Xcode $XCODE_VERSION)" >&2
        echo "  $XIP_CACHE" >&2
        echo "  Pass XCODE_XIP=/path/to/Xcode_${XCODE_VERSION}_Universal.xip to re-stage it," >&2
        echo "  or delete that file to start over. Building on it anyway." >&2
    fi
fi

# Build each iOS triple. Each builds its full cross toolchain + deps from source
# via simplybs (long, tens of GB); sbs-cleanup frees the intermediates between
# them and, with 0009 applied above, leaves the source cache and the staged xip
# alone -- both triples unpack the same xip.
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
    dest="$(libdir "$triple")"
    mkdir -p "$dest"
    cp "$dylib" "$dest/"
    echo "  staged $(basename "$dylib") -> ${dest#$repo_root/}/"
done

echo
echo "Done. Staged iOS monero dylibs:"
for triple in "${TRIPLES[@]}"; do
    echo "  $(libdir "$triple" | sed "s|^$repo_root/||")/libmonero_wallet2_api_c.dylib"
done
echo
echo "Next: cd $repo_root/wallet/ios && CARIBOU_XMR=1 ./bind.sh"
echo "      xcodegen generate --spec project.xmr.yml"
