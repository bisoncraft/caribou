#!/usr/bin/env bash
# Build every desktop platform for a release tag, collect the artifacts,
# write a GPG-signed SHA256SUMS, and optionally open a draft GitHub release.
#
#   release/release.sh v1.2.0 [--skip-linux] [--skip-windows] [--skip-darwin] [--publish]
#
# Common options (--ref, --no-sign, --no-notarize, --xmr-libs, --build-image)
# are forwarded to every package script. GPG_KEY_ID in release/.env/env.sh
# selects the manifest signing key; without it, gpg uses its default key.

source "$(dirname "$0")/common.sh"

usage() {
	cat <<USAGE
usage: $(basename "$0") vX.Y.Z [options]
$(common_usage)
  --skip-linux, --skip-windows, --skip-darwin
                     leave that platform out
  --publish          create a draft GitHub release for the tag and upload dist/
USAGE
}

parse_common_args "$@"
skip_linux=0 skip_windows=0 skip_darwin=0 publish=0
for a in "${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}"; do
	case "$a" in
	--skip-linux) skip_linux=1 ;;
	--skip-windows) skip_windows=1 ;;
	--skip-darwin) skip_darwin=1 ;;
	--publish) publish=1 ;;
	*) die "unknown option: $a" ;;
	esac
done
resolve_version
load_env

# Forward the common options verbatim; the package scripts parse them again.
# --build-image goes only to the first platform that runs: package-linux.sh
# picks the builder base from the XMR library, and the others reuse it.
fwd=()
[[ -n "$TAG" ]] && fwd+=("$TAG")
[[ -n "$REF" && "$REF" != "$TAG" ]] && fwd+=(--ref "$REF")
[[ "$DIRTY" == 1 ]] && fwd+=(--dirty)
[[ -n "$XMR_LIBS" ]] && fwd+=(--xmr-libs "$XMR_LIBS")
[[ "$NO_SIGN" == 1 ]] && fwd+=(--no-sign)
[[ "$NO_NOTARIZE" == 1 ]] && fwd+=(--no-notarize)
[[ "$NO_XMR" == 1 ]] && fwd+=(--no-xmr)

first=()
[[ "$BUILD_IMAGE" == 1 ]] && first=(--build-image)
[[ "$skip_linux" == 1 ]] || { "${RELEASE_DIR}/package-linux.sh" "${fwd[@]}" "${first[@]}"; first=(); }
[[ "$skip_windows" == 1 ]] || { "${RELEASE_DIR}/package-windows.sh" "${fwd[@]}" "${first[@]}"; first=(); }
[[ "$skip_darwin" == 1 ]] || "${RELEASE_DIR}/package-darwin.sh" "${fwd[@]}" "${first[@]}"

dist="${OUT_DIR}/dist"
rm -rf "$dist"
mkdir -p "$dist"
shopt -s nullglob
cp "${OUT_DIR}"/linux/*.{deb,tar.gz} "${OUT_DIR}"/windows/*.{msi,zip} "${OUT_DIR}"/darwin/*.{dmg,zip} "$dist/" 2>/dev/null || true
shopt -u nullglob
[[ -n "$(ls -A "$dist")" ]] || die "no artifacts found under ${OUT_DIR}"

(cd "$dist" && sha256sum -- * >SHA256SUMS)
if [[ "$NO_SIGN" == 1 ]]; then
	warn "not signing SHA256SUMS (--no-sign)"
elif [[ -z "${GPG_KEY_ID:-}" ]] && ! gpg --list-secret-keys 2>/dev/null | grep -q '^sec'; then
	warn "no GPG signing key (set GPG_KEY_ID in release/.env/env.sh); SHA256SUMS left unsigned"
else
	log "signing SHA256SUMS"
	gpg --armor --detach-sign ${GPG_KEY_ID:+--local-user "$GPG_KEY_ID"} --output "${dist}/SHA256SUMS.asc" "${dist}/SHA256SUMS"
fi

log "release ${VERSION} in ${dist}:"
ls -1 "$dist" >&2

if [[ "$publish" == 1 ]]; then
	[[ -n "$TAG" ]] || die "--publish needs a tag"
	log "creating draft GitHub release ${TAG}"
	gh release create "$TAG" --draft --verify-tag --title "Caribou ${VERSION}" \
		${PRERELEASE:+--prerelease} --generate-notes "${dist}"/*
fi
