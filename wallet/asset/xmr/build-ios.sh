#!/usr/bin/env bash
# Builds libmonero_wallet2_api_c for the two iOS triples and stages the dylibs
# into wallet/asset/xmr/lib/ios-arm64{,-simulator}/. This is the iOS analogue of
# the Dockerfile (which builds the Linux/Windows/macOS/Android libs); iOS cannot
# build in the Debian image because it needs the Apple SDK, so it runs here, on
# macOS. Pinned to monero_c v0.18.4.6-RC2 (Monero 0.18.4.6), same as the
# Dockerfile.
#
#   cd wallet/asset/xmr && ./build-ios.sh
#
# Output (SHARED dylibs; iOS embeds a signed dynamic framework, which is allowed
# — only dlopen of loose dylibs is barred — matching what Cake Wallet ships):
#   lib/ios-arm64/libmonero_wallet2_api_c.dylib            (device)
#   lib/ios-arm64-simulator/libmonero_wallet2_api_c.dylib  (simulator)
#
# Then package + wire into the app:
#   cd ../../ios && BOSSW_XMR=1 ./bind.sh          # assembles Monero.xcframework
#   xcodegen generate --spec project.xmr.yml       # links it into the app
#
# ============================================================================
# APPLE SDK REQUIREMENT (read before running)
# ----------------------------------------------------------------------------
# monero_c builds via simplybs, which does NOT use this Mac's installed Xcode.
# Its native/apple-sdk package downloads a specific, sha256-pinned
# Xcode_26.1_Universal.xip from download.developer.apple.com (Apple-login-gated,
# ~13 GB) and builds an osxcross toolchain from it; SDK_VERSION is pinned to
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
declare -A LIBDIR=(
    [aarch64-apple-ios]="$here/lib/ios-arm64"
    [aarch64-apple-ios-simulator]="$here/lib/ios-arm64-simulator"
)

# monero_c checkout. Override with MONERO_C_DIR; defaults to a gitignored build
# dir under this folder. Reused across runs (kept out of git).
MONERO_C_DIR="${MONERO_C_DIR:-$here/.monero_c-ios}"
JOBS="${JOBS:--j$(sysctl -n hw.physicalcpu)}"

# apply a patch only if it is not already applied (so re-runs on an existing
# checkout are safe).
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

# Prepare (clone + submodule + patch) is gated on a success sentinel rather than
# just ".git exists", so an interrupted prepare (e.g. a submodule fetch that got
# a connection reset) re-runs cleanly instead of being skipped with patches
# unapplied. The heavy build below is outside this gate; simplybs caches its own
# work, so re-running resumes it.
if [[ ! -f "$MONERO_C_DIR/.bossw-prepared" ]]; then
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

    echo "==> applying source patches"
    # 0001 patches the monero submodule; 0002 the wallet2_api_c C wrapper.
    ( cd "$MONERO_C_DIR" && ./apply_patches.sh monero )
    apply_once "$MONERO_C_DIR/monero" "$here/0001-add-allow_mismatched_daemon_version.patch"
    apply_once "$MONERO_C_DIR"        "$here/0002-add-wallet2_api_c-wrapper.patch"

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

    echo "==> fetching simplybs + robustness patches"
    ( cd "$MONERO_C_DIR/contrib/depends" && make simplybs )
    apply_once "$MONERO_C_DIR/contrib/depends" "$here/0004-simplybs-retry-mirrors-only.patch"
    apply_once "$MONERO_C_DIR/contrib/depends" "$here/0005-depends-keep-simplybs-checkout.patch"
    # 0006 is android-only (16 KB page align); harmless to carry so the same
    # checkout can build android too.
    apply_once "$MONERO_C_DIR/contrib/depends" "$here/0006-simplybs-android-16k-pages.patch"

    touch "$MONERO_C_DIR/.bossw-prepared"
else
    echo "==> reusing prepared checkout at $MONERO_C_DIR (skipping clone/patch)"
fi

# Stage the Apple SDK xip into simplybs' source cache (see the header). simplybs
# reuses any blob already at its cache path and only sha256-verifies on its own
# download, so a pre-placed file is trusted as-is. simplybs derives the cache
# path from the download URL host+path, and DataDirRoot defaults to
# <cwd>/.buildlib with cwd == the simplybs dir (contrib/depends/simplybs).
#   Provide the file with:  XCODE_XIP=/path/to/Xcode_26.1.xip ./build-ios.sh
XIP_URLPATH="download.developer.apple.com/Developer_Tools/Xcode_26.1/XcodeXIP_26.1_Universal.xip"
XIP_SHA256="434287e1e4c6ecbd6d5ea07e9f408fffcad2b82916b0a3fdfefa7fe60db4de93"
XIP_CACHE="$MONERO_C_DIR/contrib/depends/simplybs/.buildlib/source/$XIP_URLPATH"
if [[ -n "${XCODE_XIP:-}" ]]; then
    if [[ ! -f "$XCODE_XIP" ]]; then
        echo "error: XCODE_XIP=$XCODE_XIP not found." >&2
        exit 1
    fi
    if [[ ! -f "$XIP_CACHE" ]]; then
        echo "==> staging Apple SDK xip into simplybs source cache"
        got="$(shasum -a 256 "$XCODE_XIP" | awk '{print $1}')"
        if [[ "$got" != "$XIP_SHA256" ]]; then
            echo "  warning: sha256 $got != pinned $XIP_SHA256" >&2
            echo "  (proceeding; Apple may have re-released, or this is a different build)" >&2
        fi
        mkdir -p "$(dirname "$XIP_CACHE")"
        cp "$XCODE_XIP" "$XIP_CACHE"
        echo "  staged -> ${XIP_CACHE#$MONERO_C_DIR/}"
    else
        echo "==> Apple SDK xip already cached (skipping stage)"
    fi
elif [[ ! -f "$XIP_CACHE" ]]; then
    echo "warning: no XCODE_XIP set and no cached Apple SDK xip at" >&2
    echo "  $XIP_CACHE" >&2
    echo "The build will fail in the native/apple-sdk step. Download" >&2
    echo "Xcode 26.1.xip from https://developer.apple.com/download/all/ and pass" >&2
    echo "XCODE_XIP=/path/to/Xcode_26.1.xip (see the header of this script)." >&2
fi

# Build each iOS triple. Each builds its full cross toolchain + deps from source
# via simplybs (long, tens of GB); sbs-cleanup frees intermediates between them.
for triple in "${TRIPLES[@]}"; do
    echo "==> build_single.sh monero $triple"
    ( cd "$MONERO_C_DIR" && ./build_single.sh monero "$triple" "$JOBS" )
    ( cd "$MONERO_C_DIR/contrib/depends" && make sbs-cleanup )

    dylib="$(find "$MONERO_C_DIR/release" -path "*/$triple/libmonero_wallet2_api_c.dylib" | head -1)"
    if [[ -z "$dylib" ]]; then
        echo "error: no libmonero_wallet2_api_c.dylib produced for $triple" >&2
        echo "(looked under $MONERO_C_DIR/release/*/$triple/)" >&2
        exit 1
    fi
    dest="${LIBDIR[$triple]}"
    mkdir -p "$dest"
    cp "$dylib" "$dest/"
    echo "  staged $(basename "$dylib") -> ${dest#$repo_root/}/"
done

echo
echo "Done. Staged iOS monero dylibs:"
for triple in "${TRIPLES[@]}"; do
    echo "  ${LIBDIR[$triple]#$repo_root/}/libmonero_wallet2_api_c.dylib"
done
echo
echo "Next: cd $repo_root/wallet/ios && BOSSW_XMR=1 ./bind.sh"
echo "      xcodegen generate --spec project.xmr.yml"
