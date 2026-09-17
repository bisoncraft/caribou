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
# A case, not `declare -A`: associative arrays are bash 4, stock macOS ships
# bash 3.2, and this script runs nowhere but a Mac -- so the map aborted it on
# line one unless a Homebrew bash happened to precede /bin/bash in PATH.
# fetch-libs.sh avoids mapfile for the same reason, and says so.
libdir() {
    case "$1" in
        x86_64-apple-darwin)  echo "$here/lib/darwin-amd64" ;;
        aarch64-apple-darwin) echo "$here/lib/darwin-arm64" ;;
        *) echo "error: no lib/ directory mapped for triple '$1'." >&2; return 1 ;;
    esac
}

# monero_c checkout. Override with MONERO_C_DIR; defaults to a gitignored build
# dir under this folder. To skip a fresh (large) clone you can reuse the iOS
# checkout:  MONERO_C_DIR="$here/.monero_c-ios" ./build-darwin.sh
MONERO_C_DIR="${MONERO_C_DIR:-$here/.monero_c-darwin}"
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

# Prepare (clone + submodule + monero_c's own patches), gated on a success
# sentinel so an interrupted prepare re-runs cleanly. Only the steps that are
# expensive or not idempotent live inside the gate; our own patches are applied
# below it on every run. The heavy build further down is outside it too, since
# simplybs caches its own work and re-running resumes it.
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

    echo "==> fetching monero submodule (recursive)"
    ( cd "$MONERO_C_DIR" && git submodule update --init --recursive --force monero )

    echo "==> applying monero_c's own patches"
    # Pin the patch commits' dates: apply_patches.sh commits monero_c's patches
    # into the monero submodule, a commit hash covers its committer date, and
    # monero embeds `git rev-parse --short HEAD` in the version string that ends
    # up in the binary. Without this the same source produced a different
    # library on every run -- nine bytes, but enough to break sha256sum -c for
    # anyone rebuilding. The date is the monero_c pin's own commit date.
    ( cd "$MONERO_C_DIR" \
      && GIT_COMMITTER_DATE="2026-05-28T12:56:53+02:00" \
         GIT_AUTHOR_DATE="2026-05-28T12:56:53+02:00" \
         ./apply_patches.sh monero )
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
apply_once "$MONERO_C_DIR" contrib/depends "$here/0004-simplybs-retry-mirrors-only.patch"
apply_once "$MONERO_C_DIR" contrib/depends "$here/0005-depends-keep-simplybs-checkout.patch"
apply_once "$MONERO_C_DIR" contrib/depends "$here/0007-simplybs-gnu-origin-fallback.patch"
apply_once "$MONERO_C_DIR" contrib/depends "$here/0009-depends-keep-simplybs-source-cache.patch"
apply_once "$MONERO_C_DIR" contrib/depends "$here/0011-simplybs-recheck-cached-sources.patch"
apply_once "$MONERO_C_DIR" contrib/depends "$here/0012-simplybs-retry-git-clone.patch"

# Stage the Apple SDK xip into simplybs' source cache. simplybs reuses any blob
# already at its cache path and only sha256-verifies on its own download -- its
# "blob" extract path is a plain copy with no hash check -- so a pre-placed file
# is trusted as-is unless we check it here, which is what the block below does.
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
    # Hash it rather than compare sizes. The check above applies to $XCODE_XIP,
    # which is not necessarily the file that gets consumed: a cached blob of the
    # same size was reused on the strength of a hash computed over a different
    # file. 2.8 GB is a few seconds of sha256 against a build that runs for hours,
    # and this is the only thing standing between an arbitrary file and the Apple
    # toolchain that compiles the wallet library.
    #
    # Compared against $got, not $XIP_SHA256: with XCODE_XIP_ALLOW_MISMATCH the
    # supplied file is deliberately not the pinned one, and comparing against the
    # pin would re-copy 2.8 GB on every run forever.
    if [[ ! -f "$XIP_CACHE" ]] || [[ "$(shasum -a 256 "$XIP_CACHE" | awk '{print $1}')" != "$got" ]]; then
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
    echo "Pass XCODE_XIP=/path/to/Xcode_${XCODE_VERSION}_Universal.xip (see the header)." >&2
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

    dest="$(libdir "$triple")"
    mkdir -p "$dest"
    cp "$dylib" "$dest/"
    echo "  staged $(basename "$dylib") -> ${dest#$repo_root/}/"
done

echo
echo "Done. Staged desktop monero dylibs:"
for triple in "${TRIPLES[@]}"; do
    echo "  $(libdir "$triple" | sed "s|^$repo_root/||")/libmonero_wallet2_api_c.dylib"
done
