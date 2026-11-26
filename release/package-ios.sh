#!/usr/bin/env bash
# Build and sign the iOS release on a Mac: an App Store .ipa of the mainnet
# app, carrying the XMR library, optionally uploaded to App Store Connect.
#
#   release/package-ios.sh v1.2.0 [--build N] [--upload]
#
# Unlike the other package scripts this one does not run in the builder image
# and shares no code with them. Compiling the Swift shell and its asset
# catalog, signing for the App Store and uploading are all done by tools that
# exist only in Xcode, so the script runs natively on macOS and is written for
# what macOS ships: bash 3.2 and a BSD userland.
#
# What it does:
#   1. Resolves the tag to a commit and exports that commit with git archive
#      into release/out/<version>/ios/src. The working tree is never built.
#   2. Checks the toolchain against the pins below and in release/Dockerfile.
#   3. Verifies the two iOS XMR libraries against the SHA256SUMS at the tag.
#   4. Builds the web bundle, binds the Go core (wallet/ios/bind.sh), generates
#      the Xcode project and archives, exports and signs the app
#      (wallet/ios/release.sh).
#   5. Checks the signed .ipa: signature, provisioning profile, entitlements,
#      identity and version.
#   6. With --upload, validates and uploads it.
#
# Output, in release/out/<version>/ios/:
#   Caribou-<version>-<build>.ipa          the signed app
#   Caribou-<version>-<build>-dSYMs.zip    debug symbols of the archive
#
# Signing uses Xcode's automatic signing with a distribution certificate that
# Apple holds, so no private key is kept on the Mac. That needs an Apple ID
# with the Admin or Account Holder role signed into Xcode (Settings >
# Accounts), or an App Store Connect API key with the Admin role. The team's
# key that the desktop release notarizes with has the Developer role: it can
# upload but not provision, which is what ASC_KEY_NO_PROVISIONING=1 is for.
# wallet/ios/release.sh documents every credential option; in short:
#
#   export APPLE_TEAM_ID=XXXXXXXXXX        or set it in release/.env/env.sh
#   export ASC_KEY_ID=... ASC_ISSUER_ID=...   with AuthKey_<id>.p8 in
#                                          ~/.appstoreconnect/private_keys/
#   export ASC_KEY_NO_PROVISIONING=1       when that key is Developer-role
#
# Options:
#   vX.Y.Z[-pre]       release tag to build
#   --ref <commit>     build this commit instead of the tag (or with no tag at
#                      all, as <nearest tag>-dev.<sha>)
#   --build <N>        CFBundleVersion. Defaults to the number of commits in
#                      the history of the commit being built, the number
#                      package-android.sh gives the Android build. App Store
#                      Connect accepts a build number once per version and
#                      only above the last one it has seen for that version.
#   --xmr-libs <dir>   take the XMR libraries from <dir>/ios-arm64{,-simulator}/
#                      instead of wallet/asset/xmr/lib/
#   --no-xmr           build without Monero support. Pipeline testing only.
#   --upload           validate and upload the .ipa after the checks pass
#   --allow-toolchain-mismatch
#                      warn instead of stopping when Xcode, XcodeGen, Go or
#                      Node is not the pinned version

set -euo pipefail

# The Xcode that builds the app and the XcodeGen that generates its project.
# This is not the Xcode pinned in wallet/asset/xmr/xcode/PIN, which supplies
# the SDK the XMR libraries and the macOS desktop app are cross-compiled with.
IOS_XCODE_VERSION="27.2"
IOS_XCODEGEN_VERSION="2.48.0"

BUNDLE_ID="com.bisoncraft.caribou"
APP_NAME="Caribou"
GO_VERSION_SYMBOL="github.com/bisoncraft/caribou/wallet/app.Version"

RELEASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${RELEASE_DIR}/.." && pwd)"

log() { printf '==> %s\n' "$*" >&2; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
usage() { awk 'NR==1{next} /^#/{sub(/^# ?/,""); print; next} {exit}' "$0"; }

TAG="" REF="" BUILD="" XMR_LIBS="" NO_XMR=0 UPLOAD=0 ALLOW_MISMATCH=0
while [[ $# -gt 0 ]]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--ref) REF="${2:?--ref needs a commit}"; shift 2 ;;
	--build) BUILD="${2:?--build needs a number}"; shift 2 ;;
	--xmr-libs) XMR_LIBS="$(cd "${2:?--xmr-libs needs a directory}" && pwd)"; shift 2 ;;
	--no-xmr) NO_XMR=1; shift ;;
	--upload) UPLOAD=1; shift ;;
	--allow-toolchain-mismatch) ALLOW_MISMATCH=1; shift ;;
	v[0-9]*) [[ -z "$TAG" ]] || die "more than one tag given"; TAG="$1"; shift ;;
	*) die "unknown option: $1" ;;
	esac
done
[[ -n "$TAG" || -n "$REF" ]] || die "a release tag (vX.Y.Z) or --ref <commit> is required"
[[ "$(uname)" == "Darwin" ]] || die "this script runs on macOS only; the iOS toolchain is part of Xcode"

# ---------------------------------------------------------------------------
# Version. The same rules as resolve_version in common.sh, plus the two forms
# iOS needs: MARKETING is CFBundleShortVersionString, which allows three
# integers and nothing else, so a pre-release suffix lives only in VERSION,
# which the Go core reports.
if [[ -n "$TAG" ]]; then
	[[ "$TAG" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)(-[0-9A-Za-z.]+)?$ ]] || die "tag must look like v1.2.0 or v1.2.0-rc1, got $TAG"
	MARKETING="${BASH_REMATCH[1]}"
	git -C "$REPO_ROOT" rev-parse -q --verify "refs/tags/${TAG}^{commit}" >/dev/null || die "tag $TAG not found in $REPO_ROOT"
	VERSION="${TAG#v}"
	: "${REF:=$TAG}"
	COMMIT="$(git -C "$REPO_ROOT" rev-parse "${REF}^{commit}")"
	if [[ "$COMMIT" != "$(git -C "$REPO_ROOT" rev-parse "${TAG}^{commit}")" ]]; then
		VERSION="${VERSION}+g${COMMIT:0:10}"
	fi
else
	COMMIT="$(git -C "$REPO_ROOT" rev-parse "${REF}^{commit}")" || die "unknown ref $REF"
	base="$(git -C "$REPO_ROOT" describe --tags --abbrev=0 --match 'v[0-9]*' "$COMMIT" 2>/dev/null || echo v0.0.0)"
	[[ "$base" =~ ^v([0-9]+\.[0-9]+\.[0-9]+) ]] || die "cannot parse nearest tag $base"
	MARKETING="${BASH_REMATCH[1]}"
	VERSION="${MARKETING}-dev.${COMMIT:0:10}"
fi
: "${BUILD:=$(git -C "$REPO_ROOT" rev-list --count "$COMMIT")}"
[[ "$BUILD" =~ ^[1-9][0-9]*$ ]] || die "the build number must be a positive integer, got $BUILD"
log "version ${VERSION} (App Store ${MARKETING}, build ${BUILD}) from ${REF} (${COMMIT:0:10})"

OUT="${RELEASE_DIR}/out/${VERSION}/ios"
SRC="${OUT}/src"

# ---------------------------------------------------------------------------
# Credentials that are not secret. The team id comes from the environment or
# from release/.env/env.sh; that file is read, not sourced, so that nothing
# else in it has to be valid on this machine.
if [[ -z "${APPLE_TEAM_ID:-}" && -f "${RELEASE_DIR}/.env/env.sh" ]]; then
	APPLE_TEAM_ID="$(sed -n 's/^APPLE_TEAM_ID=//p' "${RELEASE_DIR}/.env/env.sh" | tail -1)"
fi
[[ "${APPLE_TEAM_ID:-}" =~ ^[A-Z0-9]{10}$ ]] || die "APPLE_TEAM_ID is not set (environment or release/.env/env.sh); it is the ten-character team id"

# ---------------------------------------------------------------------------
# Toolchain.
mismatch() {
	if [[ "$ALLOW_MISMATCH" == 1 ]]; then warn "$*"; else die "$* (--allow-toolchain-mismatch to build anyway)"; fi
}
for tool in git go node npm xcodebuild xcodegen codesign security plutil shasum ditto; do
	command -v "$tool" >/dev/null || die "$tool not found on PATH"
done
have="$(xcodebuild -version | awk 'NR==1 { print $2 }')"
[[ "$have" == "$IOS_XCODE_VERSION" ]] || mismatch "Xcode is ${have:-unknown}, the release is pinned to ${IOS_XCODE_VERSION}"
have="$(xcodegen --version | awk '{ print $NF }')"
[[ "$have" == "$IOS_XCODEGEN_VERSION" ]] || mismatch "XcodeGen is ${have:-unknown}, the release is pinned to ${IOS_XCODEGEN_VERSION}"
# Go and Node follow the versions the builder image pins, so that the Go core
# and the web bundle in the iOS app come from the same compilers as the other
# platforms'. Patch releases may differ; the minor version may not.
pin() { sed -n "s/^ARG $1=//p" "${RELEASE_DIR}/Dockerfile"; }
want="$(pin GO_VERSION)"
have="$(go env GOVERSION)"; have="${have#go}"
[[ "${have%.*}" == "${want%.*}" ]] || mismatch "Go is ${have}, the release is pinned to ${want}"
want="$(pin NODE_VERSION)"
have="$(node --version)"; have="${have#v}"
[[ "${have%%.*}" == "${want%%.*}" ]] || mismatch "Node is ${have}, the release is pinned to ${want}"

# ---------------------------------------------------------------------------
# Export. A second build of the same version starts from nothing.
log "exporting ${COMMIT:0:10} to ${SRC}"
rm -rf "$OUT"
mkdir -p "$SRC"
git -C "$REPO_ROOT" archive --format=tar "$COMMIT" | tar -x -C "$SRC"
printf '%s\n' "$COMMIT" >"${OUT}/COMMIT"

# ---------------------------------------------------------------------------
# XMR libraries, verified against the SHA256SUMS committed at the ref being
# built. bind.sh packs the device and simulator slices into one xcframework,
# so both are needed even though only the device slice ships.
if [[ "$NO_XMR" == 1 ]]; then
	warn "building without XMR (--no-xmr)"
else
	sums="${SRC}/wallet/asset/xmr/lib/SHA256SUMS"
	for platform in ios-arm64 ios-arm64-simulator; do
		lib="${XMR_LIBS:-${REPO_ROOT}/wallet/asset/xmr/lib}/${platform}/libmonero_wallet2_api_c.dylib"
		[[ -f "$lib" ]] || die "XMR library not found: $lib
Fetch it with wallet/asset/xmr/fetch-libs.sh or pass --xmr-libs <dir>."
		want="$(awk -v p="${platform}/libmonero_wallet2_api_c.dylib" '$2 == p { print $1 }' "$sums")"
		got="$(shasum -a 256 "$lib" | awk '{ print $1 }')"
		[[ -n "$want" ]] || die "SHA256SUMS at ${REF} has no entry for ${platform}/libmonero_wallet2_api_c.dylib"
		[[ "$want" == "$got" ]] || die "${platform}/libmonero_wallet2_api_c.dylib does not match SHA256SUMS at ${REF}
  want ${want}
  got  ${got}"
		mkdir -p "${SRC}/wallet/asset/xmr/lib/${platform}"
		cp "$lib" "${SRC}/wallet/asset/xmr/lib/${platform}/"
		log "XMR library ${platform} verified"
	done
fi

# ---------------------------------------------------------------------------
# Build. The npm cache stays beside the other release caches; Go uses its
# usual module and build caches.
cache="${RELEASE_DIR}/out/.cache"
mkdir -p "${cache}/npm-darwin"
export npm_config_cache="${cache}/npm-darwin"

log "building mobile web UI"
(cd "${SRC}/wallet/appserver" && npm ci --no-audit --no-fund && npm run build --workspace=mobile)

log "binding the Go core"
xmr=""
spec="project.yml"
if [[ "$NO_XMR" != 1 ]]; then
	xmr=1
	spec="project.xmr.yml"
fi
(cd "${SRC}/wallet/ios" &&
	CARIBOU_SKIP_WEB_BUILD=1 CARIBOU_XMR="$xmr" CARIBOU_GO_LDFLAGS="-X ${GO_VERSION_SYMBOL}=${VERSION}" ./bind.sh)

# The team id reaches Xcode through the git-ignored Local.xcconfig, which an
# export never contains.
printf 'DEVELOPMENT_TEAM = %s\n' "$APPLE_TEAM_ID" >"${SRC}/wallet/ios/Caribou/Config/Local.xcconfig"

log "generating the Xcode project from ${spec}"
(cd "${SRC}/wallet/ios" && xcodegen generate --spec "$spec")

log "archiving, exporting and signing"
(cd "${SRC}/wallet/ios" && ./release.sh --build "$BUILD" --version "$MARKETING")

built="${SRC}/wallet/ios/build"
ipa_in="$(find "${built}/export" -maxdepth 1 -name '*.ipa' -print -quit)"
[[ -n "$ipa_in" ]] || die "wallet/ios/release.sh produced no .ipa"

# ---------------------------------------------------------------------------
# Checks on the signed app, before anything is uploaded. App Store Connect
# validates a build only after it has taken the build number, and reports some
# failures by email minutes later.
log "checking the signed app"
check="$(mktemp -d)"
trap 'rm -rf "$check"' EXIT
(cd "$check" && unzip -q "$ipa_in")
app="${check}/Payload/${APP_NAME}.app"
[[ -d "$app" ]] || die "the .ipa has no Payload/${APP_NAME}.app"

plist() { /usr/libexec/PlistBuddy -c "Print :$2" "$1" 2>/dev/null; }
expect() { [[ "$2" == "$3" ]] || die "$1 is '${2}', expected '${3}'"; }

expect CFBundleIdentifier "$(plist "${app}/Info.plist" CFBundleIdentifier)" "$BUNDLE_ID"
expect CFBundleShortVersionString "$(plist "${app}/Info.plist" CFBundleShortVersionString)" "$MARKETING"
expect CFBundleVersion "$(plist "${app}/Info.plist" CFBundleVersion)" "$BUILD"
expect WalletNet "$(plist "${app}/Info.plist" WalletNet)" mainnet
[[ -f "${app}/PrivacyInfo.xcprivacy" ]] || die "the app has no privacy manifest (PrivacyInfo.xcprivacy)"

codesign --verify --deep --strict "$app" || die "codesign rejects the app"
frameworks="Mobile"
[[ "$NO_XMR" == 1 ]] || frameworks="Mobile Monero"
for fw in $frameworks; do
	[[ -d "${app}/Frameworks/${fw}.framework" ]] || die "the app does not embed ${fw}.framework"
	codesign --verify --strict "${app}/Frameworks/${fw}.framework" || die "codesign rejects ${fw}.framework"
done
if [[ "$NO_XMR" == 1 && -d "${app}/Frameworks/Monero.framework" ]]; then
	die "the app embeds Monero.framework in a --no-xmr build"
fi

# The signing identity: an Apple Distribution certificate of the right team.
signer="$(codesign -dvv "$app" 2>&1)"
grep -q "^TeamIdentifier=${APPLE_TEAM_ID}\$" <<<"$signer" || die "the app is not signed by team ${APPLE_TEAM_ID}:
$(grep -E '^(Authority|TeamIdentifier)=' <<<"$signer")"
grep -q -E '^Authority=(Apple|iPhone) Distribution: ' <<<"$signer" || die "the app is not signed with a distribution certificate:
$(grep '^Authority=' <<<"$signer")"

# Entitlements: the app's identifier under the team's prefix, and no debugger
# attachment, which is what separates a distribution build from a development
# one.
ent="${check}/entitlements.plist"
codesign -d --entitlements - --xml "$app" >"$ent" 2>/dev/null || die "cannot read the app's entitlements"
expect application-identifier "$(plist "$ent" application-identifier)" "${APPLE_TEAM_ID}.${BUNDLE_ID}"
[[ "$(plist "$ent" get-task-allow)" != "true" ]] || die "the app is signed with get-task-allow, a development entitlement"

# Provisioning profile: an App Store profile lists no devices and does not
# provision all devices; a development or ad hoc profile does the first and an
# enterprise profile the second.
profile="${check}/profile.plist"
security cms -D -i "${app}/embedded.mobileprovision" >"$profile" 2>/dev/null || die "cannot decode embedded.mobileprovision"
expect "profile team" "$(plist "$profile" TeamIdentifier:0)" "$APPLE_TEAM_ID"
expect "profile application-identifier" "$(plist "$profile" Entitlements:application-identifier)" "${APPLE_TEAM_ID}.${BUNDLE_ID}"
if plist "$profile" ProvisionedDevices >/dev/null; then die "the provisioning profile lists devices; it is not an App Store profile"; fi
[[ "$(plist "$profile" ProvisionsAllDevices)" != "true" ]] || die "the provisioning profile is an enterprise profile"
log "signed by $(sed -n 's/^Authority=//p' <<<"$signer" | head -1), profile '$(plist "$profile" Name)'"

# ---------------------------------------------------------------------------
ipa="${OUT}/${APP_NAME}-${VERSION}-${BUILD}.ipa"
cp "$ipa_in" "$ipa"
if [[ -d "${built}/${APP_NAME}.xcarchive/dSYMs" ]]; then
	ditto -c -k --keepParent "${built}/${APP_NAME}.xcarchive/dSYMs" "${OUT}/${APP_NAME}-${VERSION}-${BUILD}-dSYMs.zip"
fi
log "artifacts in ${OUT}:"
(cd "$OUT" && ls -1 -- *.ipa *.zip 2>/dev/null) >&2

if [[ "$UPLOAD" != 1 ]]; then
	log "not uploading (pass --upload to send the build to App Store Connect)"
	exit 0
fi
log "validating and uploading build ${BUILD}"
(cd "${SRC}/wallet/ios" && ./release.sh --build "$BUILD" --version "$MARKETING" --upload-only)
