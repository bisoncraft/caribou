#!/usr/bin/env bash
# Packages the libraries staged in lib/ and uploads them to a GitHub release,
# then rewrites lib/SHA256SUMS to match what was uploaded.
#
#   cd wallet/asset/xmr
#   ./publish-libs.sh                       # every platform present in lib/
#   ./publish-libs.sh ios-arm64             # or name platforms explicitly
#   ./publish-libs.sh --tag xmr-libs-v0.18.4.6-RC2-caribou.2
#   ./publish-libs.sh --dry-run             # package and hash, upload nothing
#   ./publish-libs.sh --publish             # publish outright instead of a draft
#   ./publish-libs.sh --drop-unpublished    # drop carried platforms this tag lacks
#
# The tag written here is the one fetch-libs.sh will use: it goes into the
# "# Release:" header of lib/SHA256SUMS, which fetch-libs.sh reads. It also
# defaults to whatever that header already says, so the tag lives in exactly one
# place and --tag moves it for both scripts at once.
#
# A release created as a draft serves nothing: draft assets are not reachable at
# releases/download/<tag>/, so fetch-libs.sh's curl path 404s for everyone and
# only its gh path works, and only for users with repo access. The draft has to
# be published before the checksums are committed. This script says so when it
# creates one.
#
# The counterpart to fetch-libs.sh. Building these in CI is not practical — each
# target compiles its own cross toolchain from source and ten of them do not
# fit in a six-hour job — so they are built on a developer machine (Dockerfile,
# build-ios.sh, build-darwin.sh) and published from there.
#
# WHAT THAT COSTS, AND WHAT REPLACES IT. A CI build would say where a binary
# came from. Publishing by hand cannot, so the claim these libraries rest on is
# a different one: anyone can rebuild from the pinned source and compare.
#
#     # build, then, from wallet/asset/xmr:
#     cd lib && sha256sum -c SHA256SUMS        # shasum -a 256 -c on macOS
#
# A match means the published bytes are the bytes that source produces. That is
# a stronger statement than provenance if it holds, and worth nothing if it does
# not, so establish it before leaning on it: monero_c makes no reproducibility
# claim, and nobody has checked this pin. See lib/README.md.
#
# Until then the honest position is that lib/SHA256SUMS pins exactly which
# binary this repo expects, and fetch-libs.sh will install no other. That is
# integrity, not provenance: it stops an asset being swapped after review, and
# does not prove what the build machine did.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"    # wallet/asset/xmr
sums="$here/lib/SHA256SUMS"

# Default to the tag lib/SHA256SUMS already names, for the same reason
# fetch-libs.sh reads it from there rather than carrying its own copy: a second
# copy is a second thing to forget. Publishing --tag ...caribou.2 from one
# machine and then running this script bare from another would otherwise create
# ...caribou.1 again and rewrite the header back to it, silently reverting the
# release pointer for everyone. The literal is only a bootstrap, for the first
# publish, when no header exists yet.
TAG="${XMR_LIBS_TAG:-}"
if [[ -z "$TAG" && -f "$sums" ]]; then
    TAG="$(awk '/^# Release:/ { print $3; exit }' "$sums")"
fi
TAG="${TAG:-xmr-libs-v0.18.4.6-RC2-caribou.1}"
REPO="${XMR_LIBS_REPO:-bisoncraft/caribou}"
PIN="3bfb3856a838f2bf6b729501837bb0295dedf25d"   # monero_c, == tag v0.18.4.6-RC2

dry_run=0
draft=1
drop_unpublished=0
declare -a want=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --tag)      TAG="$2"; shift 2 ;;
        --dry-run)  dry_run=1; shift ;;
        --publish)  draft=0; shift ;;   # opt out of --draft
        --drop-unpublished) drop_unpublished=1; shift ;;
        # Print the header comment and stop at the first line that is not one.
        # The range this replaces had already drifted onto `set -euo pipefail`.
        -h|--help)  awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; exit 0 ;;
        -*)         echo "error: unknown flag $1" >&2; exit 1 ;;
        *)          want+=("$1"); shift ;;
    esac
done

# The wallet library's filename per platform. Used to spot a built platform in
# lib/ and to reject an unknown platform argument.
libname() {
    case "$1" in
        linux-*|android-*) echo "libmonero_wallet2_api_c.so"    ;;
        windows-*)         echo "libmonero_wallet2_api_c.dll"   ;;
        darwin-*|ios-*)    echo "libmonero_wallet2_api_c.dylib" ;;
        *) return 1 ;;
    esac
}

# Everything a platform's asset carries. Android adds libc++_shared.so: Monero
# links it dynamically, and wallet/android/bind.sh wants the copy from the NDK
# simplybs built the wallet library with, because that one has a matching libc++
# ABI and 16 KB-aligned LOAD segments (Play API 35+). Without it in the asset a
# fetched Android platform is not usable, and bind.sh silently falls back to
# whatever the local NDK has.
libfiles() {
    case "$1" in
        android-*)      echo "libmonero_wallet2_api_c.so libc++_shared.so" ;;
        linux-*)        echo "libmonero_wallet2_api_c.so"    ;;
        windows-*)      echo "libmonero_wallet2_api_c.dll"   ;;
        darwin-*|ios-*) echo "libmonero_wallet2_api_c.dylib" ;;
        *) return 1 ;;
    esac
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# Default to whatever is actually built. A platform with no library is skipped
# rather than failing: not every machine can build every target, and publishing
# the subset you have is the normal case.
discovered=0
if [[ ${#want[@]} -eq 0 ]]; then
    discovered=1
    for d in "$here"/lib/*/; do
        p="$(basename "$d")"
        lib="$(libname "$p" 2>/dev/null)" || continue
        [[ -f "$d$lib" ]] && want+=("$p")
    done
fi
[[ ${#want[@]} -gt 0 ]] || { echo "error: no libraries found in lib/." >&2; exit 1; }

command -v tar >/dev/null || { echo "error: tar not found." >&2; exit 1; }
# GNU tar shells out to the xz binary for -J; bsdtar, which is what `tar` is on
# macOS, links liblzma and needs nothing. Check only the tar that needs it, and
# check here rather than at the `tar -cJf` below: that one runs after every
# platform has been hashed and size-accounted, so a missing xz would otherwise
# fail at the end of the work instead of before any of it.
if tar --version 2>/dev/null | head -1 | grep -q 'GNU tar'; then
    command -v xz >/dev/null || {
        echo "error: xz not found, and GNU tar needs it to write a .tar.xz." >&2
        echo "  Install xz-utils (Debian/Ubuntu) or xz (Fedora/Arch)." >&2
        exit 1
    }
fi
if [[ $dry_run -eq 0 ]]; then
    command -v gh >/dev/null 2>&1 || { echo "error: gh not found (needed to upload)." >&2; exit 1; }
    gh auth status >/dev/null 2>&1 || { echo "error: gh is not authenticated." >&2; exit 1; }
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/assets"

# Why a missing libc++_shared.so makes an Android platform unpublishable rather
# than merely incomplete.
libcxx_note() {
    echo "  An Android asset ships it alongside the wallet library, and it has to" >&2
    echo "  be the NDK copy simplybs built that library with. Extract it from the" >&2
    echo "  builder image (see lib/README.md) rather than publishing without it:" >&2
    echo "  bind.sh would then fall back to a local NDK libc++ that may be 4 KB" >&2
    echo "  aligned, which Android 15+ rejects." >&2
}

echo "==> packaging"
new_sums="$work/SHA256SUMS.body"
: > "$new_sums"
declare -a packaged=()
for platform in "${want[@]}"; do
    files="$(libfiles "$platform")" || { echo "error: unknown platform '$platform'." >&2; exit 1; }
    raw=0
    skip=0
    declare -a members=()
    for f in $files; do
        src="$here/lib/$platform/$f"
        if [[ ! -f "$src" ]]; then
            # Discovery above admits a platform on its wallet library alone, so an
            # Android directory with no libc++_shared.so lands here on a bare run.
            # Skip it, for the same reason a platform with no library at all is
            # skipped: publishing the subset you have is the normal case, and
            # failing here publishes nothing. Naming a platform on the command
            # line is the opposite statement -- you asked for that one -- so an
            # incomplete one stays fatal there.
            if [[ $discovered -eq 1 ]]; then
                echo "warning: $platform: $f not present, skipping this platform." >&2
                if [[ "$f" == "libc++_shared.so" ]]; then libcxx_note; fi
                skip=1
                break
            fi
            echo "error: $src not found." >&2
            if [[ "$f" == "libc++_shared.so" ]]; then libcxx_note; fi
            exit 1
        fi
        members+=("$f")
        raw=$(( raw + $(wc -c < "$src") ))
    done
    if [[ $skip -eq 1 ]]; then
        continue
    fi
    packaged+=("$platform")

    # -C so the archive holds bare libraries with no leading path; fetch-libs.sh
    # extracts into a scratch dir and expects them there.
    tar -cJf "$work/assets/libmonero_wallet2_api_c-$platform.tar.xz" \
        -C "$here/lib/$platform" "${members[@]}"

    # Hash the uncompressed libraries: those are what get linked and loaded, and
    # the hashes stay stable if the assets are ever recompressed.
    for f in "${members[@]}"; do
        printf '%s  %s/%s\n' "$(sha256_of "$here/lib/$platform/$f")" "$platform" "$f" >> "$new_sums"
    done
    printf '    %-20s %6s MiB -> %s MiB\n' "$platform" \
        "$(( raw / 1048576 ))" \
        "$(( $(wc -c < "$work/assets/libmonero_wallet2_api_c-$platform.tar.xz") / 1048576 ))"
done
[[ ${#packaged[@]} -gt 0 ]] || {
    echo "error: every platform found in lib/ was incomplete; nothing to publish." >&2
    exit 1
}
sort -k2 -o "$new_sums" "$new_sums"

# Platforms already in SHA256SUMS that this run did not package. They are kept
# so publishing one target does not silently drop the others -- but only if the
# target release actually holds their asset. The header this file is about to
# get names $TAG, so carrying a platform that was never uploaded to $TAG makes
# the file claim a release it is not in, and fetch-libs.sh then 404s on it with
# "the asset may not exist for this platform", which reads as an unbuilt target
# rather than a bad anchor.
declare -a carry=()
if [[ -f "$sums" ]]; then
    while read -r p; do
        printf '%s\n' "${packaged[@]}" | grep -qx "$p" || carry+=("$p")
    done < <(awk '!/^#/ && NF == 2 { sub("/.*", "", $2); if (!seen[$2]++) print $2 }' "$sums")
fi

declare -a unpublished=()
if [[ ${#carry[@]} -gt 0 ]]; then
    have_assets=1
    if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
        gh release view "$TAG" --repo "$REPO" --json assets \
            --jq '.assets[].name' > "$work/release-assets" 2>/dev/null || : > "$work/release-assets"
    else
        # Only reachable under --dry-run, which does not require gh.
        have_assets=0
        : > "$work/release-assets"
    fi
    for p in "${carry[@]}"; do
        grep -qx "libmonero_wallet2_api_c-$p.tar.xz" "$work/release-assets" || unpublished+=("$p")
    done
    if [[ $have_assets -eq 0 && ${#unpublished[@]} -gt 0 ]]; then
        echo "note: gh unavailable, so carried platforms were not checked against $TAG:" >&2
        printf '  %s\n' "${unpublished[@]}" >&2
        unpublished=()
    fi
fi

if [[ ${#unpublished[@]} -gt 0 ]]; then
    if [[ $drop_unpublished -eq 1 ]]; then
        echo "==> dropping platforms not present in $TAG: ${unpublished[*]}"
        for p in "${unpublished[@]}"; do
            for i in "${!carry[@]}"; do
                [[ "${carry[$i]}" == "$p" ]] && unset 'carry[i]'
            done
        done
    elif [[ $dry_run -eq 1 ]]; then
        # A dry run rewrites no file and uploads nothing, so there is no claim
        # to be false yet. Refusing here made --dry-run unusable for the case it
        # is most wanted in -- the first publish, where the tag does not exist
        # and so holds none of the carried assets.
        echo "note: these platforms are listed in lib/SHA256SUMS but have no asset in" >&2
        echo "  $REPO $TAG, so a real run would refuse to carry them:" >&2
        printf '    %s\n' "${unpublished[@]}" >&2
    else
        echo "error: these platforms are listed in lib/SHA256SUMS but have no asset in" >&2
        echo "  $REPO $TAG, so their hashes cannot honestly be carried into a file" >&2
        echo "  headed '# Release: $TAG':" >&2
        printf '    %s\n' "${unpublished[@]}" >&2
        echo "  Either package them in this run (build them, or name them as arguments)," >&2
        echo "  publish to the tag they do belong to, or pass --drop-unpublished to remove" >&2
        echo "  them from the file. Dropping them means fetch-libs.sh will refuse those" >&2
        echo "  platforms until they are published to $TAG." >&2
        exit 1
    fi
fi

echo "==> assembling SHA256SUMS"
{
    cat <<EOF
# sha256 of each prebuilt libmonero_wallet2_api_c, by platform directory.
#
# This file is the trust anchor for ./fetch-libs.sh. The libraries themselves
# are gitignored and distributed as release assets, which are mutable — anyone
# with write access can replace an asset at the same URL and nothing in git
# changes. These hashes are in git, so a swap shows up as a diff on a commit
# instead of as a silently different binary linked into a wallet.
#
# The hash is of the UNCOMPRESSED library, the artifact that actually gets
# linked. That keeps it stable if the assets are ever recompressed, and the
# .tar.xz carries its own integrity check for transport.
#
# Written by ./publish-libs.sh. Verify a rebuild against it with:
#     cd lib && sha256sum -c SHA256SUMS      # shasum -a 256 -c on macOS
#
# Release: $TAG
# monero_c pin: $PIN
EOF
    # Carry forward the platforms validated above, and only those.
    if [[ -f "$sums" && ${#carry[@]} -gt 0 ]]; then
        awk '!/^#/ && NF == 2 { print }' "$sums" | while read -r h path; do
            p="${path%%/*}"
            if printf '%s\n' "${carry[@]}" | grep -qx "$p"; then
                printf '%s  %s\n' "$h" "$path"
            fi
        done
    fi
    cat "$new_sums"
} > "$work/SHA256SUMS.new"
# Sort the body (everything after the header) so order stays stable.
{ grep '^#' "$work/SHA256SUMS.new"; echo; grep -v '^#' "$work/SHA256SUMS.new" | grep -v '^$' | sort -k2; } \
    > "$work/assets/SHA256SUMS"
echo
cat "$work/assets/SHA256SUMS"
echo

if [[ $dry_run -eq 1 ]]; then
    echo "==> dry run: lib/SHA256SUMS not touched, nothing uploaded"
    echo "    assets that would go to $TAG:"
    ls -1 "$work/assets"
    exit 0
fi

echo "==> uploading to $REPO $TAG"
notes="$(cat <<EOF
Prebuilt \`libmonero_wallet2_api_c\` for the Caribou wallet.

Not stock monero_c: the patches in \`wallet/asset/xmr/\` add
\`MONERO_Wallet_setAllowMismatchedDaemonVersion\`, which the cgo bindings link
against, so an upstream MrCyjaneK asset will not satisfy the link.

monero_c pin: \`$PIN\` (v0.18.4.6-RC2)

Install with verification rather than by hand:

    cd wallet/asset/xmr && ./fetch-libs.sh

It checks every download against \`lib/SHA256SUMS\` in git and installs nothing
that does not match.

These were built on a developer machine, not in CI, so there is no build
provenance attached. What can be checked instead is the source: rebuild from
the pin above and compare against \`lib/SHA256SUMS\`. If you do this, please
report the result either way — whether the build reproduces is not yet
established.
EOF
)"

created_draft=0
if gh release view "$TAG" --repo "$REPO" >/dev/null 2>&1; then
    gh release upload "$TAG" "$work"/assets/* --repo "$REPO" --clobber
else
    args=(--title "monero_c libraries $TAG" --notes "$notes")
    [[ $draft -eq 1 ]] && { args+=(--draft); created_draft=1; }
    gh release create "$TAG" "$work"/assets/* --repo "$REPO" "${args[@]}"
fi

# Only now. lib/SHA256SUMS is what fetch-libs.sh trusts, and it names the
# release the hashes belong to, so writing it before the upload means a failed
# upload leaves the working tree pointing at assets that do not exist. `set -e`
# takes the exit above, and the tree is left untouched for a retry.
cp "$work/assets/SHA256SUMS" "$sums"

echo
echo "Uploaded."
if [[ $created_draft -eq 1 ]]; then
    echo
    echo "$TAG is a DRAFT. Its assets are not served from"
    echo "releases/download/$TAG/, so fetch-libs.sh's curl path cannot reach them and"
    echo "its gh path works only for accounts with access to $REPO. Publish it first:"
    echo "    gh release edit '$TAG' --repo '$REPO' --draft=false"
    echo
fi
echo "Then commit the checksums:"
echo "    git add wallet/asset/xmr/lib/SHA256SUMS"
echo "    git commit -m 'xmr: publish monero_c libs $TAG'"
echo
echo "fetch-libs.sh will not install these assets until that commit lands."
