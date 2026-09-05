#!/usr/bin/env bash
# Downloads prebuilt libmonero_wallet2_api_c libraries into lib/<platform>/,
# verifying each against lib/SHA256SUMS before it is installed.
#
#   cd wallet/asset/xmr
#   ./fetch-libs.sh                     # this host's platform
#   ./fetch-libs.sh ios-arm64 ios-arm64-simulator
#   ./fetch-libs.sh --all               # every platform in SHA256SUMS
#   ./fetch-libs.sh --force             # re-download even if present and valid
#
# These libraries are NOT stock monero_c. Patches 0001/0002 add
# MONERO_Wallet_setAllowMismatchedDaemonVersion, which cxmr declares extern and
# links against, so an upstream MrCyjaneK release will not satisfy the link.
# They come from this repo's own release assets, built from the Dockerfile in
# this directory and uploaded with ./publish-libs.sh.
#
# A platform can ship more than one file. The Android targets also carry
# libc++_shared.so, which the wallet library links against and which has to be
# the copy from the NDK simplybs built it with — wallet/android/bind.sh stages
# it into jniLibs beside the wallet lib, and falls back to a possibly
# 4 KB-aligned NDK copy when it is absent. Which files a platform ships is read
# out of lib/SHA256SUMS, not listed here.
#
# TRUST MODEL. Release assets are mutable: anyone with write access can replace
# one at the same URL and nothing in git changes. So the trust anchor is
# lib/SHA256SUMS, which IS in git — reviewed in a diff, tied to a commit, and
# immutable once merged. A swapped asset fails here instead of being linked into
# a binary that handles private keys. Nothing is installed unverified.
#
# The assets are built on a developer machine, not in CI, so nothing here
# attests to where they came from. Rebuilding is what replaces that: build from
# the pinned source and run `cd lib && sha256sum -c SHA256SUMS`. Whether that
# reproduces bit for bit is not yet established — see lib/README.md.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"    # wallet/asset/xmr
sums="$here/lib/SHA256SUMS"

# The release these assets come from. Bump the caribou suffix when the patches
# in this directory change, not only when the monero_c pin moves: the patches
# change the ABI, so a lib built before them is the wrong artifact even though
# Monero's version is identical.
TAG="${XMR_LIBS_TAG:-xmr-libs-v0.18.4.6-RC2-caribou.1}"
REPO="${XMR_LIBS_REPO:-bisoncraft/caribou}"

# The wallet library's filename per platform. monero_c emits a shared library
# everywhere; only the extension differs. This names one file, so it is used to
# validate a platform argument and nothing else — what a platform actually
# ships is files_in_sums() below.
libname() {
    case "$1" in
        linux-*)   echo "libmonero_wallet2_api_c.so"    ;;
        android-*) echo "libmonero_wallet2_api_c.so"    ;;
        windows-*) echo "libmonero_wallet2_api_c.dll"   ;;
        darwin-*)  echo "libmonero_wallet2_api_c.dylib" ;;
        ios-*)     echo "libmonero_wallet2_api_c.dylib" ;;
        *) return 1 ;;
    esac
}

# The platform matching this machine. Only the desktop platforms can be guessed
# — a mobile lib is a cross-compile target, never the host — so Android and iOS
# must always be named explicitly.
host_platform() {
    local os arch
    case "$(uname -s)" in
        Linux)  os=linux  ;;
        Darwin) os=darwin ;;
        MINGW*|MSYS*|CYGWIN*) os=windows ;;
        *) echo "error: unsupported host OS $(uname -s); name a platform explicitly." >&2; return 1 ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        arm64|aarch64) arch=arm64 ;;
        *) echo "error: unsupported host arch $(uname -m); name a platform explicitly." >&2; return 1 ;;
    esac
    echo "$os-$arch"
}

# sha256sum is coreutils (Linux), shasum is perl (macOS); neither is on both.
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# Every file SHA256SUMS lists for a platform, as "<platform>/<file>" paths.
# Read from the file rather than from a table here, so that a platform which
# grows a second library needs no matching edit, and so that what gets verified
# is exactly what the committed anchor names.
files_in_sums() {
    awk -v p="$1/" '!/^#/ && NF == 2 && substr($2, 1, length(p)) == p { print $2 }' "$sums"
}

# The expected hash for one "<platform>/<file>" path, from the committed
# SHA256SUMS. Lines are "<sha256>  <platform>/<file>", the format sha256sum -c
# expects, so the file can also be checked directly from lib/ with
# `sha256sum -c SHA256SUMS`.
expected_sha() {
    awk -v p="$1" '$2 == p { print $1 }' "$sums"
}

# Platforms named in SHA256SUMS, once each: a platform with two files is still
# one download.
platforms_in_sums() {
    awk '!/^#/ && NF == 2 { sub("/.*", "", $2); if (!seen[$2]++) print $2 }' "$sums"
}

force=0
declare -a want=()
for arg in "$@"; do
    case "$arg" in
        # Not mapfile: macOS ships bash 3.2, where it does not exist.
        --all)   while read -r p; do want+=("$p"); done < <(platforms_in_sums) ;;
        --force) force=1 ;;
        -h|--help)
            sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        -*) echo "error: unknown flag $arg" >&2; exit 1 ;;
        *)  want+=("$arg") ;;
    esac
done
[[ ${#want[@]} -gt 0 ]] || want=("$(host_platform)")

[[ -f "$sums" ]] || { echo "error: $sums not found." >&2; exit 1; }

# Prefer gh: it carries the viewer's credentials, which a private repo needs and
# a public one does not mind. curl covers machines without gh, and works only
# once the repo is public.
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    downloader=gh
elif command -v curl >/dev/null 2>&1; then
    downloader=curl
else
    echo "error: need gh (authenticated) or curl to download." >&2
    exit 1
fi

# GNU tar shells out to the xz binary to read a .tar.xz; bsdtar, which is what
# `tar` is on macOS, links liblzma and needs nothing. Check only the tar that
# needs it, and check it before anything is downloaded, so a host without
# xz-utils is told what is missing instead of reporting every platform as an
# archive that "could not be extracted".
command -v tar >/dev/null || { echo "error: tar not found." >&2; exit 1; }
if tar --version 2>/dev/null | head -1 | grep -q 'GNU tar'; then
    command -v xz >/dev/null || {
        echo "error: xz not found, and GNU tar needs it to read a .tar.xz." >&2
        echo "  Install xz-utils (Debian/Ubuntu) or xz (Fedora/Arch)." >&2
        exit 1
    }
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

failed=0
for platform in "${want[@]}"; do
    libname "$platform" >/dev/null || { echo "error: unknown platform '$platform'." >&2; failed=1; continue; }

    declare -a paths=()
    while read -r p; do paths+=("$p"); done < <(files_in_sums "$platform")
    if [[ ${#paths[@]} -eq 0 ]]; then
        echo "error: $platform is not listed in lib/SHA256SUMS." >&2
        echo "  It may not be published yet; see README.md to build it." >&2
        failed=1
        continue
    fi

    # Skip the download only when *every* file the platform lists is present and
    # matching. A platform that ships two libraries and has one of them is not
    # done, which is the state an Android checkout is left in by a fetch from
    # before libc++_shared.so was distributed.
    if [[ $force -eq 0 ]]; then
        have=1 any=0
        for p in "${paths[@]}"; do
            f="$here/lib/$p"
            [[ -f "$f" ]] && any=1
            [[ -f "$f" && "$(sha256_of "$f")" == "$(expected_sha "$p")" ]] || have=0
        done
        if [[ $have -eq 1 ]]; then
            echo "==> $platform: already present and verified"
            continue
        fi
        # A local build, a partial copy, or a stale lib from an earlier tag.
        # Replacing it is the point of the run, so say so rather than silently
        # overwriting something the user may have built on purpose.
        if [[ $any -eq 1 ]]; then
            echo "==> $platform: present but does not match SHA256SUMS, replacing"
        fi
    fi

    asset="libmonero_wallet2_api_c-$platform.tar.xz"
    echo "==> $platform: downloading $asset"
    if [[ "$downloader" == gh ]]; then
        gh release download "$TAG" --repo "$REPO" --pattern "$asset" --dir "$work" --clobber
    else
        curl -fsSL -o "$work/$asset" \
            "https://github.com/$REPO/releases/download/$TAG/$asset"
    fi

    # Extract into a scratch dir so a bad archive cannot leave a half-written
    # library where the linker would find it. The preflight above has already
    # established that this tar can read xz.
    mkdir -p "$work/$platform"
    tar -xJf "$work/$asset" -C "$work/$platform"

    # Verify every file before installing any of them. Installing as we go
    # would leave a platform whose second library failed the check holding one
    # new file and one stale one, which is a mismatched pair rather than a
    # rejected download.
    ok=1
    for p in "${paths[@]}"; do
        got="$work/$platform/${p#*/}"
        if [[ ! -f "$got" ]]; then
            echo "error: $asset did not contain ${p#*/}." >&2
            ok=0
            break
        fi
        got_sha="$(sha256_of "$got")"
        want_sha="$(expected_sha "$p")"
        if [[ "$got_sha" != "$want_sha" ]]; then
            echo "error: $p checksum mismatch — NOT installed." >&2
            echo "  expected $want_sha (lib/SHA256SUMS)" >&2
            echo "  got      $got_sha (release $TAG)" >&2
            echo "  The release asset does not match what this commit expects." >&2
            ok=0
            break
        fi
    done
    if [[ $ok -eq 0 ]]; then
        failed=1
        continue
    fi

    for p in "${paths[@]}"; do
        dest="$here/lib/$p"
        if ! { mkdir -p "$(dirname "$dest")" && mv "$work/$platform/${p#*/}" "$dest" && chmod 755 "$dest"; }; then
            echo "error: $platform: verified, but could not install to lib/$platform/." >&2
            ok=0
            break
        fi
        echo "    verified and installed lib/$p"
    done
    [[ $ok -eq 1 ]] || failed=1
done

if [[ $failed -ne 0 ]]; then
    echo >&2
    echo "One or more libraries were not installed. See the errors above." >&2
    exit 1
fi
