#!/usr/bin/env bash
# Shared helpers for the release/package-*.sh scripts. Sourced, never run.
#
# Each package script has two halves. The host half parses arguments, exports
# the requested git ref into release/out/<version>/src, stages the XMR
# libraries next to it, and re-invokes the same script inside the
# caribou-builder image with `--stage build`. The container half does the
# build, signing and packaging. Everything the container half needs arrives as
# environment variables, so it never touches git.
#
# Directory layout (release/out is gitignored):
#   out/<version>/src/        clean export of the ref being built
#   out/<version>/COMMIT      the exact commit that was exported
#   out/<version>/<os>/       per-platform outputs and staging
#   out/<version>/dist/       collected artifacts, SHA256SUMS (release.sh)
#   out/.cache/               Go, npm, Electron and dotnet caches shared across runs
#
# Credentials are read from release/.env/env.sh (see env.example) and the
# directory is mounted read-only at /secrets in the container.

# Several variables here are consumed by the scripts that source this file.
# shellcheck disable=SC2034

set -euo pipefail

RELEASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${RELEASE_DIR}/.." && pwd)"
ENV_DIR="${RELEASE_DIR}/.env"
IMAGE=""          # set by select_image; CARIBOU_BUILDER_IMAGE overrides
BASE="${BASE:-}"  # Ubuntu release of the builder image (22.04 or 24.04); the
                  # container half receives it from run_stage's environment
APP="caribou-desktop"
GO_VERSION_SYMBOL="github.com/bisoncraft/caribou/wallet/app.Version"
HOMEPAGE="https://github.com/bisoncraft/caribou"

log() { printf '==> %s\n' "$*" >&2; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

in_container() { [[ -n "${CARIBOU_IN_CONTAINER:-}" ]]; }

# Options every package script accepts. Platform-specific flags fall through
# to EXTRA_ARGS for the caller to handle.
common_usage() {
	cat <<USAGE
  vX.Y.Z[-pre]       release tag to build; the version is derived from it
  --ref <commit>     build this commit instead of the tag (or with no tag at
                     all, in which case the version is <nearest tag>-dev.<sha>)
  --dirty            build the working tree, uncommitted changes included
                     (development only; the version gets a .dirty suffix)
  --xmr-libs <dir>   take libmonero_wallet2_api_c from <dir>/<platform>/
                     instead of wallet/asset/xmr/lib/
  --no-sign          skip code signing
  --no-notarize      skip Apple notarization (macOS only)
  --build-image      rebuild the ${IMAGE} docker image first
  --no-xmr           build without Monero support. Pipeline testing only:
                     every release carries the xmr tag
USAGE
}

parse_common_args() {
	TAG="" REF="" DIRTY=0 NO_SIGN=0 NO_NOTARIZE=0 NO_XMR=0 XMR_LIBS="" BUILD_IMAGE=0
	EXTRA_ARGS=()
	while [[ $# -gt 0 ]]; do
		case "$1" in
		-h | --help) usage; exit 0 ;;
		--ref) REF="$2"; shift 2 ;;
		--dirty) DIRTY=1; REF="${REF:-HEAD}"; shift ;;
		--xmr-libs) XMR_LIBS="$(cd "$2" && pwd)"; shift 2 ;;
		--no-sign) NO_SIGN=1; shift ;;
		--no-notarize) NO_NOTARIZE=1; shift ;;
		--no-xmr) NO_XMR=1; shift ;;
		--build-image) BUILD_IMAGE=1; shift ;;
		v[0-9]*) TAG="$1"; shift ;;
		*) EXTRA_ARGS+=("$1"); shift ;;
		esac
	done
	[[ -n "$TAG" || -n "$REF" ]] || die "a release tag (vX.Y.Z), --ref <commit> or --dirty is required"
	[[ "$DIRTY" == 1 && -n "$TAG" ]] && die "--dirty cannot be combined with a release tag"
	return 0
}

# resolve_version derives every version form from the tag or ref:
#   VERSION      1.2.0-rc1            full semver, stamped into the binaries
#   VERSION_NUM  1.2.0                numeric core (Windows file and product version)
#   PRERELEASE   rc1                  empty for a final release
#   VERSION_DEB  1.2.0~rc1            dpkg orders ~rc1 before the final 1.2.0
#   COMMIT       full sha of what is exported
resolve_version() {
	if [[ -n "$TAG" ]]; then
		[[ "$TAG" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)(-[0-9A-Za-z.]+)?$ ]] ||
			die "tag must look like v1.2.0 or v1.2.0-rc1, got $TAG"
		git -C "$REPO_ROOT" rev-parse -q --verify "refs/tags/${TAG}^{commit}" >/dev/null ||
			die "tag $TAG not found in $REPO_ROOT"
		VERSION_NUM="${BASH_REMATCH[1]}"
		PRERELEASE="${BASH_REMATCH[2]#-}"
		VERSION="${TAG#v}"
		: "${REF:=$TAG}"
		COMMIT="$(git -C "$REPO_ROOT" rev-parse "${REF}^{commit}")"
		if [[ "$COMMIT" != "$(git -C "$REPO_ROOT" rev-parse "${TAG}^{commit}")" ]]; then
			# A different commit stamped with the tag's version: mark it as
			# build metadata so the binary says where it really came from.
			VERSION="${VERSION}+g${COMMIT:0:10}"
		fi
	else
		COMMIT="$(git -C "$REPO_ROOT" rev-parse "${REF}^{commit}")" || die "unknown ref $REF"
		local base
		base="$(git -C "$REPO_ROOT" describe --tags --abbrev=0 --match 'v[0-9]*' "$COMMIT" 2>/dev/null || echo v0.0.0)"
		[[ "$base" =~ ^v([0-9]+\.[0-9]+\.[0-9]+) ]] || die "cannot parse nearest tag $base"
		VERSION_NUM="${BASH_REMATCH[1]}"
		PRERELEASE="dev.${COMMIT:0:10}"
		[[ "$DIRTY" == 1 ]] && PRERELEASE+=".dirty"
		VERSION="${VERSION_NUM}-${PRERELEASE}"
	fi
	VERSION_DEB="${VERSION/-/\~}"
	OUT_DIR="${RELEASE_DIR}/out/${VERSION}"
	SRC_DIR="${OUT_DIR}/src"
	log "version ${VERSION} from ${REF} (${COMMIT:0:10})"
}

export_source() {
	# The package scripts share out/<version>/src, so two of them building the
	# same version at once would clobber each other. The lock is held by this
	# shell for the rest of the run (release.sh runs the platforms in sequence).
	mkdir -p "$OUT_DIR"
	exec 9>"${OUT_DIR}/.lock"
	flock -n 9 || die "another build of ${VERSION} is running in ${OUT_DIR}"
	rm -rf "$SRC_DIR"
	mkdir -p "$SRC_DIR"
	if [[ "$DIRTY" == 1 ]]; then
		# Tracked plus untracked-but-not-ignored files, as they are on disk.
		# A tracked file deleted from the working tree is still in the index,
		# so the list is filtered to paths that exist.
		log "exporting the working tree (dirty) to ${SRC_DIR}"
		git -C "$REPO_ROOT" ls-files -z --cached --others --exclude-standard |
			(cd "$REPO_ROOT" && while IFS= read -r -d '' f; do
				if [[ -e "$f" || -L "$f" ]]; then printf '%s\0' "$f"; fi
			done) |
			tar -C "$REPO_ROOT" --null -T - -cf - | tar -x -C "$SRC_DIR"
		printf '%s (dirty)\n' "$COMMIT" >"${OUT_DIR}/COMMIT"
		return 0
	fi
	log "exporting ${COMMIT:0:10} to ${SRC_DIR}"
	git -C "$REPO_ROOT" archive --format=tar "$COMMIT" | tar -x -C "$SRC_DIR"
	printf '%s\n' "$COMMIT" >"${OUT_DIR}/COMMIT"
}

# stage_xmr_libs <platform> <file>: copy the prebuilt monero_c library into the
# export, verifying it against the SHA256SUMS committed at the ref being built.
# That file is the trust anchor for the libraries (see wallet/asset/xmr/lib/
# README.md); a release binary must not link a library the tree does not pin.
stage_xmr_libs() {
	local platform="$1" file="$2"
	if [[ "$NO_XMR" == 1 ]]; then
		warn "building without XMR (--no-xmr)"
		return 0
	fi
	local src="${XMR_LIBS:-${REPO_ROOT}/wallet/asset/xmr/lib}/${platform}/${file}"
	[[ -f "$src" ]] || die "XMR library not found: $src
Build or fetch it (wallet/asset/xmr/lib/README.md) or pass --xmr-libs <dir>."
	local sums="${SRC_DIR}/wallet/asset/xmr/lib/SHA256SUMS"
	local want got
	want="$(awk -v p="${platform}/${file}" '$2 == p { print $1 }' "$sums")"
	got="$(sha256sum "$src" | cut -d' ' -f1)"
	if [[ -z "$want" ]]; then
		warn "SHA256SUMS at ${REF} has no entry for ${platform}/${file}; using it unverified"
	elif [[ "$want" != "$got" ]]; then
		die "${platform}/${file} does not match SHA256SUMS at ${REF}
  want ${want}
  got  ${got}"
	else
		log "XMR library ${platform}/${file} verified"
	fi
	install -Dm644 "$src" "${SRC_DIR}/wallet/asset/xmr/lib/${platform}/${file}"
}

load_env() {
	# shellcheck disable=SC1091
	[[ -f "${ENV_DIR}/env.sh" ]] && source "${ENV_DIR}/env.sh"
	return 0
}

version_le() { [[ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -1)" == "$1" ]]; }

# linux_base_for_lib <libmonero_wallet2_api_c.so>: the oldest Ubuntu base
# whose glibc and libstdc++ can link the prebuilt library. The library sets
# the .deb's floor, so the binary is built on the oldest base able to link
# it, and the floor falls on its own once a library built against an older
# glibc arrives. Prints the base; diagnostics go to stderr.
linux_base_for_lib() {
	local so="$1" glibc cxx
	glibc="$(objdump -T "$so" | grep -oE 'GLIBC_[0-9.]+' | sort -Vu | tail -1)"
	cxx="$(objdump -T "$so" | grep -oE 'GLIBCXX_[0-9.]+' | sort -Vu | tail -1)"
	glibc="${glibc#GLIBC_}"; cxx="${cxx#GLIBCXX_}"
	log "XMR library needs glibc ${glibc:-?} and libstdc++ ${cxx:-(none)}"
	if version_le "${glibc:-0}" 2.35 && version_le "${cxx:-0}" 3.4.30; then
		echo 22.04
	elif version_le "${glibc:-0}" 2.39 && version_le "${cxx:-0}" 3.4.33; then
		echo 24.04
	else
		die "the XMR library needs glibc ${glibc} / libstdc++ ${cxx}, newer than any supported builder base"
	fi
}

# select_image [base]: choose the builder image. The Linux package dictates
# the base (linux_base_for_lib); the other platforms take whatever labeled
# builder image already exists, or 22.04, since their toolchains do not
# depend on it. CARIBOU_BUILDER_IMAGE overrides everything.
select_image() {
	local want="${1:-}"
	if [[ -n "${CARIBOU_BUILDER_IMAGE:-}" ]]; then
		IMAGE="$CARIBOU_BUILDER_IMAGE"; BASE="${want:-22.04}"
		return 0
	fi
	if [[ -z "$want" ]]; then
		want="$(docker image ls --filter label=caribou.builder=1 --format '{{.Tag}}' 2>/dev/null | sort -V | tail -1)"
		want="${want:-22.04}"
	fi
	BASE="$want"
	IMAGE="caribou-builder:${BASE}"
}

build_image() {
	# The build context is the Xcode staging directory, not the repo: the image
	# needs nothing from the tree except a staged xip, and the scripts are
	# mounted at run time so they never go stale inside the image.
	# CARIBOU_XCODE_DIR points it at another checkout's staging directory, so
	# a worktree can build against a xip staged once in the main clone.
	local ctx="${CARIBOU_XCODE_DIR:-${REPO_ROOT}/wallet/asset/xmr/xcode}"
	# A rebuild under the same tag leaves the previous image untagged, where
	# the label listing below no longer finds it, so note it first.
	local previous
	previous="$(docker image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || true)"
	log "building docker image ${IMAGE} on ubuntu:${BASE} (Xcode staging dir: ${ctx})"
	docker build -f "${RELEASE_DIR}/Dockerfile" -t "$IMAGE" \
		--build-arg "UBUNTU=${BASE}" --label caribou.builder=1 --label "caribou.base=${BASE}" "$ctx"
	# One builder image at a time: remove the ones these scripts built on
	# other bases, and the one this build replaced, only after the new one
	# exists. The label keeps every other image on the machine out of reach.
	local id other
	id="$(docker image inspect --format '{{.Id}}' "$IMAGE")"
	for other in $(printf '%s\n' "$previous" $(docker image ls -q --no-trunc --filter label=caribou.builder=1) | grep . | sort -u); do
		[[ "$other" == "$id" ]] && continue
		log "removing superseded builder image ${other#sha256:}"
		docker rmi -f "$other" >/dev/null || warn "could not remove ${other}"
	done
}

ensure_image() {
	[[ -n "$IMAGE" ]] || select_image ""
	if [[ "$BUILD_IMAGE" == 1 ]] || ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
		build_image
	fi
}

# Variables forwarded into the container verbatim. Credentials are forwarded
# only when set.
CONTAINER_VARS=(VERSION VERSION_NUM VERSION_DEB PRERELEASE TAG REF COMMIT NO_SIGN NO_NOTARIZE NO_XMR BASE)
SECRET_VARS=(APPLE_TEAM_ID APPLE_KEY_FILE APPLE_CERT_FILE APPLE_P12 APPLE_P12_PASSWORD ASC_KEY_JSON
	AZURE_TENANT_ID AZURE_CLIENT_ID AZURE_CLIENT_SECRET ACS_ENDPOINT ACS_ACCOUNT ACS_PROFILE
	WINDOWS_PFX WINDOWS_PFX_PASSWORD)

# run_stage <name> [args...]: re-invoke the calling script inside the builder
# image. /release is the live scripts directory, /work is out/<version>,
# /cache persists toolchain caches across runs, /secrets is release/.env.
run_stage() {
	local stage="$1"; shift
	local cache="${RELEASE_DIR}/out/.cache"
	mkdir -p "$cache"/{gomod,gocache,npm,electron,home} "$OUT_DIR"
	local args=(run --rm --init
		--user "$(id -u):$(id -g)"
		-e CARIBOU_IN_CONTAINER=1
		-e HOME=/cache/home -e XDG_CACHE_HOME=/cache/home -e DOTNET_CLI_HOME=/cache/home
		-e GOMODCACHE=/cache/gomod -e GOCACHE=/cache/gocache
		-e npm_config_cache=/cache/npm -e ELECTRON_CACHE=/cache/electron
		-v "${RELEASE_DIR}:/release:ro"
		-v "${OUT_DIR}:/work"
		-v "${cache}:/cache"
		-w /work/src)
	[[ -d "$ENV_DIR" ]] && args+=(-v "${ENV_DIR}:/secrets:ro")
	local v
	for v in "${CONTAINER_VARS[@]}"; do args+=(-e "${v}=${!v:-}"); done
	for v in "${SECRET_VARS[@]}"; do [[ -n "${!v:-}" ]] && args+=(-e "${v}=${!v}"); done
	[[ -t 0 && -t 1 ]] && args+=(-t)
	docker "${args[@]}" "$IMAGE" bash "/release/$(basename "$0")" --stage "$stage" "$@"
}

# ---------------------------------------------------------------------------
# Container-side helpers. Everything below assumes cwd=/work/src.

go_ldflags() { printf -- '-s -w -buildid= -X %s=%s %s' "$GO_VERSION_SYMBOL" "$VERSION" "${1:-}"; }
# go_tags fills GO_TAGS with the build tags for this build.
go_tags() {
	GO_TAGS=()
	[[ "$NO_XMR" == 1 ]] || GO_TAGS=(-tags xmr)
}
with_xmr() { [[ "$NO_XMR" != 1 ]]; }

build_ui() {
	log "building web UI"
	(cd wallet/appserver && npm ci --no-audit --no-fund && npm run build --workspace=newui)
}

# sign_windows_file <file>: Authenticode-sign a PE file with jsign. Azure
# Artifact Signing (formerly Trusted Signing, which is still jsign's name for
# the backend) when the ACS_* variables are set, otherwise a local PKCS#12
# keystore (self-signed certificates for pipeline testing). Every signature is
# RFC 3161 timestamped: Artifact Signing certificates are valid for three
# days, so the timestamp is what keeps the signature valid.
sign_windows_file() {
	local f="$1"
	if [[ "$NO_SIGN" == 1 ]]; then
		warn "not signing $(basename "$f") (--no-sign)"
		return 0
	fi
	if [[ -n "${ACS_ACCOUNT:-}" ]]; then
		log "signing $(basename "$f") with Azure Artifact Signing ${ACS_ACCOUNT}/${ACS_PROFILE}"
		# jsign's TRUSTEDSIGNING backend takes a bearer token as the store
		# password (newer jsign releases also accept tenant|client|secret, 7.5
		# does not), so exchange the service principal's client credentials for
		# one here. Tokens last about an hour; one is fetched per file.
		local token
		token="$(curl -fsS -X POST "https://login.microsoftonline.com/${AZURE_TENANT_ID}/oauth2/v2.0/token" \
			-d client_id="${AZURE_CLIENT_ID}" --data-urlencode client_secret="${AZURE_CLIENT_SECRET}" \
			-d scope="https://codesigning.azure.net/.default" -d grant_type=client_credentials |
			jq -r '.access_token // empty')"
		[[ -n "$token" ]] || die "could not obtain an Azure access token for Artifact Signing (check AZURE_TENANT_ID / AZURE_CLIENT_ID / AZURE_CLIENT_SECRET)"
		jsign --storetype TRUSTEDSIGNING --keystore "$ACS_ENDPOINT" \
			--storepass "$token" \
			--alias "${ACS_ACCOUNT}/${ACS_PROFILE}" \
			--tsaurl http://timestamp.acs.microsoft.com --tsmode RFC3161 --alg SHA-256 \
			--name Caribou --url "$HOMEPAGE" "$f"
	elif [[ -n "${WINDOWS_PFX:-}" ]]; then
		log "signing $(basename "$f") with PKCS#12 keystore ${WINDOWS_PFX}"
		jsign --storetype PKCS12 --keystore "/secrets/${WINDOWS_PFX}" \
			--storepass "${WINDOWS_PFX_PASSWORD:-}" \
			--tsaurl http://timestamp.digicert.com --tsmode RFC3161 --alg SHA-256 \
			--name Caribou --url "$HOMEPAGE" "$f"
	else
		die "no Windows signing credentials (ACS_* or WINDOWS_PFX in release/.env/env.sh); pass --no-sign to build unsigned"
	fi
}

# verify_windows_file <file>: check an Authenticode signature and its
# timestamp with osslsigncode. Only meaningful for Artifact Signing output,
# whose chain roots at the pinned Microsoft root; a self-signed test
# certificate has no anchor to verify against and is skipped.
verify_windows_file() {
	local f="$1"
	if [[ "$NO_SIGN" == 1 || -z "${ACS_ACCOUNT:-}" ]]; then
		return 0
	fi
	local root=/release/windows/microsoft-identity-verification-root-2020.pem
	log "verifying signature on $(basename "$f")"
	osslsigncode verify -CAfile "$root" -TSA-CAfile "$root" "$f" >/dev/null 2>&1 ||
		die "signature verification failed for $f (osslsigncode verify -CAfile $root -TSA-CAfile $root)"
}

# apple_key_args: fills APPLE_KEY_ARGS with rcodesign credential flags from
# the Apple identity in /secrets: a PEM key+certificate pair, or a PKCS#12
# export from a Mac keychain.
apple_key_args() {
	if [[ -n "${APPLE_P12:-}" ]]; then
		APPLE_KEY_ARGS=(--p12-file "/secrets/${APPLE_P12}" --p12-password "${APPLE_P12_PASSWORD:-}")
	elif [[ -n "${APPLE_KEY_FILE:-}" && -n "${APPLE_CERT_FILE:-}" ]]; then
		APPLE_KEY_ARGS=(--pem-file "/secrets/${APPLE_KEY_FILE}" --pem-file "/secrets/${APPLE_CERT_FILE}")
	else
		die "no Apple signing identity (APPLE_KEY_FILE+APPLE_CERT_FILE or APPLE_P12 in release/.env/env.sh); pass --no-sign to build unsigned"
	fi
}

# macos_nested_scopes <bundle>: bundle-relative paths of every nested bundle
# and every loose Mach-O executable inside an .app, one per line. rcodesign
# applies scoped settings only to the main entity unless a setting is scoped
# to a nested path explicitly (its `@main` scope reaches the main executable
# alone, whatever the help text says), so signing a bundle with the hardened
# runtime means naming each helper, framework and embedded binary.
macos_nested_scopes() {
	local bundle="$1"
	(
		cd "$bundle"
		find Contents/Frameworks -mindepth 1 -maxdepth 1 \( -name '*.app' -o -name '*.framework' \) 2>/dev/null
		find Contents/Frameworks -mindepth 3 -maxdepth 3 -path '*.framework/Versions/*' -type d 2>/dev/null
		# Loose Mach-O executables; a nested bundle's own Contents/MacOS is
		# covered by the bundle scope above. Identified by magic number.
		find Contents -type f -perm -u+x ! -path '*/Contents/MacOS/*' ! -name '*.dylib' -print0 2>/dev/null |
			while IFS= read -r -d '' f; do
				case "$(head -c4 "$f" | od -An -tx1 | tr -d ' \n')" in
				cffaedfe | cefaedfe | cafebabe | bebafeca) printf '%s\n' "$f" ;;
				esac
			done
	) | sort -u
}

# sign_macos <path> [entitlements.plist]: Developer ID signature with the
# hardened runtime on the target and, for a bundle, on every nested bundle
# and executable (see macos_nested_scopes). The entitlements, when given, are
# applied at the same scopes. A .dmg gets a plain signature.
#
# With --no-sign the target is ad-hoc signed instead of left alone: fuse
# flipping and the integrity digest patch the Electron Framework binary, and
# Apple Silicon refuses to launch a binary whose signature no longer matches.
# An ad-hoc signature is enough to run a test build locally (after clearing
# the quarantine attribute); it is not enough for Gatekeeper.
sign_macos() {
	local target="$1" entitlements="${2:-}"
	local -a args=()
	if [[ "$target" != *.dmg ]]; then
		args+=(--code-signature-flags runtime)
		[[ -n "$entitlements" ]] && args+=(--entitlements-xml-file "$entitlements")
		if [[ -d "$target" ]]; then
			local p
			while IFS= read -r p; do
				args+=(--code-signature-flags "${p}:runtime")
				[[ -n "$entitlements" ]] && args+=(--entitlements-xml-file "${p}:${entitlements}")
			done < <(macos_nested_scopes "$target")
		fi
	fi
	if [[ "$NO_SIGN" == 1 ]]; then
		warn "ad-hoc signing $(basename "$target") (--no-sign)"
		rcodesign sign "${args[@]}" "$target"
		return 0
	fi
	apple_key_args
	local -a team=()
	[[ -n "${APPLE_TEAM_ID:-}" ]] && team=(--team-name "$APPLE_TEAM_ID")
	log "signing $(basename "$target")"
	rcodesign sign "${APPLE_KEY_ARGS[@]}" "${team[@]}" "${args[@]}" "$target"
}

# verify_macos_runtime <bundle>: fail unless every Mach-O executable in the
# bundle carries the hardened-runtime flag, the check the notary service
# makes first. Catches a scoping mistake before a submission is spent on it.
verify_macos_runtime() {
	local bundle="$1" f flags bad=0
	while IFS= read -r -d '' f; do
		case "$(head -c4 "$f" | od -An -tx1 | tr -d ' \n')" in
		cffaedfe | cefaedfe | cafebabe | bebafeca) ;;
		*) continue ;;
		esac
		flags="$(rcodesign print-signature-info "$f" 2>/dev/null | grep -E '^\s+flags:' | head -1)"
		if [[ "$flags" != *RUNTIME* ]]; then
			warn "no hardened runtime on ${f#"$bundle"/}: ${flags:-unsigned}"
			bad=1
		fi
	done < <(find "$bundle/Contents" -type f -perm -u+x ! -name '*.dylib' -print0)
	[[ "$bad" == 0 ]] || die "hardened runtime missing on executables in $(basename "$bundle")"
	log "hardened runtime present on every executable in $(basename "$bundle")"
}

# notarize_macos <path>: submit to Apple's notary service and staple the
# ticket. Needs the App Store Connect API key encoded by
# `rcodesign encode-app-store-connect-api-key` (ASC_KEY_JSON, default
# apple/asc-key.json under release/.env).
notarize_macos() {
	local target="$1"
	if [[ "$NO_SIGN" == 1 || "$NO_NOTARIZE" == 1 ]]; then
		warn "not notarizing $(basename "$target")"
		return 0
	fi
	local key="/secrets/${ASC_KEY_JSON:-apple/asc-key.json}"
	[[ -f "$key" ]] || die "App Store Connect API key not found at $key (see release/env.example)"
	log "notarizing $(basename "$target")"
	# rcodesign gives up after 600 s by default. Apple usually answers within
	# minutes, but a team's first submissions have sat in the queue for over
	# an hour, and giving up leaves the build unstapled even if the ticket is
	# issued later. Wait up to six hours; NOTARY_WAIT_SECONDS overrides.
	rcodesign notary-submit --api-key-file "$key" \
		--max-wait-seconds "${NOTARY_WAIT_SECONDS:-21600}" --staple "$target"
}
