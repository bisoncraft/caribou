#!/usr/bin/env bash
# Build every desktop platform and the command-line archives for a release
# tag, collect the artifacts, write one GPG-signed SHA256SUMS covering all of
# them, and optionally open a draft GitHub release.
#
#   release/release.sh v1.2.0 [--skip-linux] [--skip-windows] [--skip-darwin] [--skip-cli]
#                             [--publish [--notes <file>]]
#
# Common options (--ref, --no-sign, --no-notarize, --xmr-libs, --build-image)
# are forwarded to every package script. GPG_KEY_ID in release/.env/env.sh
# selects the manifest signing key; without it, gpg uses its default key.

source "$(dirname "$0")/common.sh"

usage() {
	cat <<USAGE
usage: $(basename "$0") vX.Y.Z [options]
$(common_usage)
  --skip-linux, --skip-windows, --skip-darwin, --skip-cli
                     leave that platform, or the command-line archives, out
  --publish          create a draft GitHub release for the tag and upload dist/
  --notes <file>     release notes for the draft, instead of the notes GitHub
                     generates from the pull requests since the previous
                     release (every pull request, for the first one)
USAGE
}

parse_common_args "$@"
skip_linux=0 skip_windows=0 skip_darwin=0 skip_cli=0 publish=0 notes=""
set -- "${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}"
while [[ $# -gt 0 ]]; do
	case "$1" in
	--skip-linux) skip_linux=1 ;;
	--skip-windows) skip_windows=1 ;;
	--skip-darwin) skip_darwin=1 ;;
	--skip-cli) skip_cli=1 ;;
	--publish) publish=1 ;;
	--notes)
		[[ $# -ge 2 ]] || die "--notes needs a file"
		notes="$(realpath "$2")"
		[[ -f "$notes" ]] || die "no such notes file: $2"
		shift
		;;
	*) die "unknown option: $1" ;;
	esac
	shift
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
[[ "$skip_darwin" == 1 ]] || { "${RELEASE_DIR}/package-darwin.sh" "${fwd[@]}" "${first[@]}"; first=(); }
[[ "$skip_cli" == 1 ]] || "${RELEASE_DIR}/package-cli.sh" "${fwd[@]}" "${first[@]}"

dist="${OUT_DIR}/dist"
rm -rf "$dist"
mkdir -p "$dist"
shopt -s nullglob
cp "${OUT_DIR}"/linux/*.{deb,tar.gz} "${OUT_DIR}"/windows/*.{exe,zip} "${OUT_DIR}"/darwin/*.{dmg,zip} \
	"${OUT_DIR}"/cli/*.{tar.gz,zip} "$dist/" 2>/dev/null || true
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
	notes_args=(--generate-notes)
	[[ -n "$notes" ]] && notes_args=(--notes-file "$notes")
	gh release create "$TAG" --draft --verify-tag --title "Caribou ${VERSION}" \
		${PRERELEASE:+--prerelease} "${notes_args[@]}" "${dist}"/*

	# GitHub renames some characters in uploaded asset names without
	# documenting which ("~" becomes ".", which is why the .deb file name
	# carries VERSION rather than VERSION_DEB). A renamed asset no longer
	# matches its SHA256SUMS line or a download link built from the file name,
	# so stop before the draft is published.
	stored="$(gh release view "$TAG" --json assets --jq '.assets[].name' | sort)"
	built="$(ls -1 "$dist" | sort)"
	if [[ "$stored" != "$built" ]]; then
		diff <(printf '%s\n' "$built") <(printf '%s\n' "$stored") >&2 || true
		die "the draft release's asset names (>) differ from ${dist} (<)"
	fi
	log "every asset name in the draft matches ${dist}"
fi
