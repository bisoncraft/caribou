#!/usr/bin/env bash
# Build, sign and package the Android release: an APK for direct download and
# an Android App Bundle for Google Play, both arm64 and both carrying the XMR
# library.
#
#   release/package-android.sh v1.2.0 [--build N]
#
# The two artifacts are signed with different keys. The APK carries the app
# signing key, the app's permanent identity, a copy of which Google Play holds
# and re-signs the bundle's APKs with; an install from either channel
# therefore updates from the other. The bundle carries the upload key, which
# only authenticates the upload to Play. Both certificates are pinned in
# release/android/, and a build signed by anything else fails.
#
# The work is split over two containers so that nothing fetched from the
# network runs next to a signing key. The build stage (npm, gomobile, Gradle
# and every dependency they resolve) gets no credential at all and leaves the
# two artifacts unsigned. The package stage gets the two keystores and their
# passwords and nothing else, has no network, and runs only tools from the
# builder image: it signs, checks the result, and only then puts the
# artifacts and a SHA256SUMS of them in out/<version>/android/. The build
# stage can write to the exported source and to
# out/<version>/android-unsigned/ and nowhere else under out/<version>, so
# the final directory holds what the package stage made and nothing else.
# release.sh and publish-play.sh take from it only files that SHA256SUMS
# lists and matches.

source "$(dirname "$0")/common.sh"

ANDROID_APP_ID="com.bisoncraft.caribou"
ANDROID_ABI="arm64-v8a"
ANDROID_XMR_PLATFORM="android-arm64"
ANDROID_FLAVOR="netMainnet"
# Android's ceiling for versionCode.
ANDROID_MAX_VERSION_CODE=2100000000

# ANDROID_BUILD_TOOLS and ANDROID_XMR_SUMS are read from the export on the
# host, before the build stage has had the chance to change it.
CONTAINER_VARS+=(BUILD_NUMBER ANDROID_BUILD_TOOLS ANDROID_XMR_SUMS)
# What the package stage receives, and no other stage does.
ANDROID_SIGNING_VARS=(ANDROID_APP_KEYSTORE ANDROID_APP_KEY_ALIAS ANDROID_APP_KEYSTORE_PASSWORD
	ANDROID_UPLOAD_KEYSTORE ANDROID_UPLOAD_KEY_ALIAS ANDROID_UPLOAD_KEYSTORE_PASSWORD)

usage() {
	cat <<USAGE
usage: $(basename "$0") vX.Y.Z [options]
$(common_usage)
  --build <N>        versionCode of the build. Defaults to the number of
                     commits in the history of the commit being built, the
                     same number wallet/ios/release.sh gives the iOS build.
                     Google Play accepts a versionCode once and only above
                     every one it has seen, so a rebuild of an uploaded
                     commit, or a build from a branch with fewer commits
                     than the last upload, needs this.
USAGE
}

# apksigner loads a native crypto library, which this JDK warns about unless
# native access is granted. Its wrapper script passes -J options to the JVM
# with one dash added.
APKSIGNER_JVM=(-J-enable-native-access=ALL-UNNAMED)

# pinned_cert <name>: the SHA-256 fingerprint pinned in release/android/<name>,
# as lowercase hex without separators.
pinned_cert() {
	local f="/release/android/$1"
	[[ -f "$f" ]] || die "missing ${f}"
	grep -v '^#' "$f" | tr -d ':[:space:]' | tr 'A-F' 'a-f'
}

# sign_apk <unsigned> <signed>: sign with the app signing key and check the
# signer against the pin.
sign_apk() {
	local in="$1" out="$2"
	local ks="/secrets/${ANDROID_APP_KEYSTORE:-}"
	[[ -n "${ANDROID_APP_KEYSTORE:-}" && -f "$ks" ]] ||
		die "ANDROID_APP_KEYSTORE is not set or not found under release/.env (see env.example), or pass --no-sign"
	log "signing $(basename "$out") with the app signing key"
	# No v4 signature: it is a separate .idsig file that only incremental
	# installs over adb use.
	apksigner "${APKSIGNER_JVM[@]}" sign --ks "$ks" --ks-type PKCS12 --ks-key-alias "$ANDROID_APP_KEY_ALIAS" \
		--ks-pass env:ANDROID_APP_KEYSTORE_PASSWORD --key-pass env:ANDROID_APP_KEYSTORE_PASSWORD \
		--v4-signing-enabled false --out "$out" "$in"
	# The report names the signer once per signature scheme, with a prefix
	# that differs between apksigner versions, so the digests are collected
	# wherever they appear: every one of them has to be the pinned one.
	local report got want
	report="$(apksigner "${APKSIGNER_JVM[@]}" verify --verbose --print-certs "$out")" || die "apksigner rejected $(basename "$out")"
	got="$(grep -oE 'certificate SHA-256 digest: [0-9a-f]{64}' <<<"$report" | awk '{ print $NF }' | sort -u)"
	want="$(pinned_cert app-signing-cert.sha256)"
	[[ "$got" == "$want" ]] || die "$(basename "$out") is signed by ${got:-nothing},
not by the pinned app signing certificate ${want}"
	grep -qx 'Number of signers: 1' <<<"$report" || die "$(basename "$out") does not have exactly one signer"
	log "APK signer matches release/android/app-signing-cert.sha256 ($(sed -n 's/^Verified using \(v[0-9.]*\) scheme.*: true$/\1/p' <<<"$report" | tr '\n' ' ')signatures)"
}

# sign_bundle <unsigned> <signed>: sign with the upload key and check the
# signer against the pin. A bundle takes a plain JAR signature; apksigner
# does not handle the format.
sign_bundle() {
	local in="$1" out="$2"
	local ks="/secrets/${ANDROID_UPLOAD_KEYSTORE:-}"
	[[ -n "${ANDROID_UPLOAD_KEYSTORE:-}" && -f "$ks" ]] ||
		die "ANDROID_UPLOAD_KEYSTORE is not set or not found under release/.env (see env.example), or pass --no-sign"
	log "signing $(basename "$out") with the upload key"
	jarsigner -keystore "$ks" -storetype PKCS12 -storepass:env ANDROID_UPLOAD_KEYSTORE_PASSWORD \
		-sigalg SHA256withRSA -digestalg SHA-256 -signedjar "$out" "$in" "$ANDROID_UPLOAD_KEY_ALIAS" >/dev/null
	jarsigner -verify "$out" | grep -q '^jar verified\.' || die "jarsigner rejected $(basename "$out")"
	local got want
	got="$(keytool -printcert -jarfile "$out" | sed -n 's/^[[:space:]]*SHA256: //p' | head -1 | tr -d ':' | tr 'A-F' 'a-f')"
	want="$(pinned_cert upload-cert.sha256)"
	[[ "$got" == "$want" ]] || die "$(basename "$out") is signed by ${got:-nothing},
not by the pinned upload certificate ${want}"
	log "bundle signer matches release/android/upload-cert.sha256"
}

# check_libs <dir> <from>: <dir> holds the native libraries taken out of the
# artifact <from>. The ones the app needs are there, every one is linked with
# 16 KB segment alignment, and the XMR pair is the one the host verified.
check_libs() {
	local dir="$1" from="$2" so want=(libgojni.so)
	with_xmr && want+=(libmonero_wallet2_api_c.so libc++_shared.so)
	for so in "${want[@]}"; do
		[[ -f "${dir}/${so}" ]] || die "${from} has no ${so}"
	done
	local align segments
	for so in "$dir"/*.so; do
		# Every LOAD segment's alignment, the last column of readelf -lW, in
		# hex. bash does the comparison: the image's awk has no strtonum.
		segments=0
		while read -r align; do
			segments=$((segments + 1))
			((align >= 16384)) || die "$(basename "$so") in ${from} has a LOAD segment aligned to ${align}, below 16 KB"
		done < <(readelf -lW "$so" | awk '$1 == "LOAD" { print $NF }')
		((segments > 0)) || die "readelf found no LOAD segments in $(basename "$so") from ${from}"
	done
	if with_xmr; then
		[[ -n "${ANDROID_XMR_SUMS:-}" ]] || die "no checksums for the XMR libraries were passed in"
		(cd "$dir" && sha256sum --check --strict --quiet <<<"$ANDROID_XMR_SUMS") ||
			die "the XMR libraries in ${from} are not the verified ones"
	fi
	log "${from}: $(printf '%s ' "${want[@]}")present, 16 KB segment alignment"
}

# extract_libs <archive> <prefix> <dir>: unpack the native libraries stored
# under <prefix> in an APK or a bundle, and insist they are for one ABI only.
extract_libs() {
	local archive="$1" prefix="$2" dir="$3"
	unzip -q "$archive" "${prefix}/*" -d "$dir" || die "$(basename "$archive") has no ${prefix}/"
	[[ "$(ls "${dir}/${prefix}")" == "$ANDROID_ABI" ]] ||
		die "$(basename "$archive") carries ABIs other than ${ANDROID_ABI}: $(ls "${dir}/${prefix}" | tr '\n' ' ')"
}

# check_apk <apk>: the package identity is the mainnet one, the version is the
# one asked for, and the native libraries are stored uncompressed and
# page-aligned in the archive. Android 15 devices with 16 KB pages refuse
# anything less, and so does Google Play.
check_apk() {
	local apk="$1" badging tmp
	badging="$(aapt2 dump badging "$apk")"
	grep -q "^package: name='${ANDROID_APP_ID}' versionCode='${BUILD_NUMBER}' versionName='${VERSION}' " <<<"$badging" ||
		die "unexpected package line in $(basename "$apk"): $(grep '^package:' <<<"$badging")"
	log "$(basename "$apk"): $(grep -E "^(minSdkVersion|targetSdkVersion)" <<<"$badging" | tr -d "'" | tr '\n' ' ')"
	zipalign -c -P 16 4 "$apk" || die "$(basename "$apk") is not zip-aligned to 16 KB"
	tmp="$(mktemp -d)"
	extract_libs "$apk" lib "$tmp"
	check_libs "${tmp}/lib/${ANDROID_ABI}" "$(basename "$apk")"
	rm -rf "$tmp"
}

# check_bundle <aab>: the same for the bundle. Google Play builds the APKs it
# serves from the bundle, so the page alignment of those is a setting in the
# bundle's configuration, not a property of the archive.
check_bundle() {
	local aab="$1" tmp got attr
	bundletool validate --bundle "$aab" >/dev/null || die "bundletool rejected $(basename "$aab")"
	for attr in "package=${ANDROID_APP_ID}" "android:versionCode=${BUILD_NUMBER}" "android:versionName=${VERSION}"; do
		got="$(bundletool dump manifest --bundle "$aab" --xpath "/manifest/@${attr%%=*}")"
		[[ "$got" == "${attr#*=}" ]] || die "$(basename "$aab") has ${attr%%=*} ${got:-(none)}, expected ${attr#*=}"
	done
	got="$(bundletool dump config --bundle "$aab" | jq -r '.optimizations.uncompressNativeLibraries | "\(.enabled) \(.alignment)"')"
	[[ "$got" == "true PAGE_ALIGNMENT_16K" ]] ||
		die "$(basename "$aab") does not ask for uncompressed native libraries aligned to 16 KB (enabled and alignment: ${got})"
	tmp="$(mktemp -d)"
	extract_libs "$aab" base/lib "$tmp"
	check_libs "${tmp}/base/lib/${ANDROID_ABI}" "$(basename "$aab")"
	rm -rf "$tmp"
}

# android_tools: put the JDK and the build tools the Gradle build names on
# PATH. The version reaches both stages from the host, which read it out of
# the export, so wallet/android/app/build.gradle stays the one place it is
# stated.
android_tools() {
	[[ -d "${ANDROID_HOME:-/nonexistent}" && -x /opt/android-jdk/bin/java ]] ||
		die "the builder image has no Android toolchain; rerun with --build-image"
	[[ -d "${ANDROID_HOME}/build-tools/${ANDROID_BUILD_TOOLS}" ]] ||
		die "wallet/android/app/build.gradle names build tools ${ANDROID_BUILD_TOOLS}, which the builder image does not carry; update release/Dockerfile and rerun with --build-image"
	export JAVA_HOME=/opt/android-jdk
	# osxcross puts an xcrun on PATH. gomobile takes any xcrun to mean Xcode is
	# installed and fails probing for an iOS SDK, so that directory is dropped.
	PATH="${ANDROID_HOME}/build-tools/${ANDROID_BUILD_TOOLS}:${JAVA_HOME}/bin:${PATH//\/opt\/osxcross\/bin:/}"
}

# ---------------------------------------------------------------------------
# stage_build runs without credentials: see the top of this file.
stage_build() {
	local out=/work/android-unsigned
	android_tools
	export GRADLE_USER_HOME=/cache/gradle

	log "building mobile web UI"
	(cd wallet/appserver && npm ci --no-audit --no-fund && npm run build --workspace=mobile)

	log "binding the Go library for android/${ANDROID_ABI}"
	local xmr=""
	with_xmr && xmr=1
	CARIBOU_SKIP_WEB_BUILD=1 CARIBOU_XMR="$xmr" CARIBOU_GO_LDFLAGS="$(go_ldflags)" wallet/android/bind.sh

	log "building the ${ANDROID_FLAVOR} release APK and bundle, versionCode ${BUILD_NUMBER}"
	local task="${ANDROID_FLAVOR^}Release"
	(cd wallet/android && ./gradlew --no-daemon --console=plain \
		-Pcaribou.versionCode="$BUILD_NUMBER" -Pcaribou.versionName="$VERSION" \
		":app:assemble${task}" ":app:bundle${task}")
	local built=wallet/android/app/build/outputs
	local apk_in="${built}/apk/${ANDROID_FLAVOR}/release/app-${ANDROID_FLAVOR}-release-unsigned.apk"
	local aab_in="${built}/bundle/${ANDROID_FLAVOR}Release/app-${ANDROID_FLAVOR}-release.aab"
	[[ -f "$apk_in" && -f "$aab_in" ]] || die "Gradle did not produce ${apk_in} and ${aab_in}"
	cp "$apk_in" "${out}/app.apk"
	cp "$aab_in" "${out}/app.aab"
}

# stage_package signs what stage_build left and checks the result. It holds
# the signing keys, so it has no network, does not see the shared caches, and
# runs nothing out of /work: the build stage could write there.
stage_package() {
	local out=/work/android unsigned=/work/android-unsigned staging=/work/android-staging
	[[ -f "${unsigned}/app.apk" && -f "${unsigned}/app.aab" ]] || die "the build stage left nothing to package"
	android_tools
	cd /
	# Whatever is at the final path now was not put there by this stage.
	rm -rf "$out" "$staging"
	mkdir "$staging"
	# A signed artifact that fails a check is not kept, even under a
	# temporary name.
	trap 'rm -rf "'"$staging"'"' EXIT

	local apk="caribou-${VERSION}-android-arm64.apk" aab="caribou-${VERSION}.aab"
	if [[ "$NO_SIGN" == 1 ]]; then
		warn "not signing the APK or the bundle (--no-sign); neither can be installed or uploaded"
		apk="${apk%.apk}-unsigned.apk" aab="${aab%.aab}-unsigned.aab"
		cp "${unsigned}/app.apk" "${staging}/${apk}"
		cp "${unsigned}/app.aab" "${staging}/${aab}"
	else
		sign_apk "${unsigned}/app.apk" "${staging}/${apk}"
		sign_bundle "${unsigned}/app.aab" "${staging}/${aab}"
	fi
	check_apk "${staging}/${apk}"
	check_bundle "${staging}/${aab}"

	# The directory gets its final name, in one step, only when every check
	# has passed.
	(cd "$staging" && sha256sum -- "$apk" "$aab" >SHA256SUMS)
	mv "$staging" "$out"
	trap - EXIT
	rm -rf "$unsigned"

	log "artifacts:"
	ls -1 "$out" >&2
}

# ---------------------------------------------------------------------------
host_main() {
	parse_common_args "$@"
	local build=""
	set -- "${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}"
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--build) build="${2:-}"; shift 2 || die "--build needs a number" ;;
		*) die "unknown option: $1" ;;
		esac
	done
	resolve_version
	load_env
	local v
	if [[ "$NO_SIGN" != 1 ]]; then
		for v in "${ANDROID_SIGNING_VARS[@]}"; do
			[[ -n "${!v:-}" ]] || die "${v} is not set in release/.env/env.sh (see env.example), or pass --no-sign"
		done
	fi
	export_source
	stage_xmr_libs "$ANDROID_XMR_PLATFORM" libmonero_wallet2_api_c.so
	# monero_c needs the libc++ of the NDK it was built with at run time.
	stage_xmr_libs "$ANDROID_XMR_PLATFORM" libc++_shared.so
	ANDROID_XMR_SUMS=""
	if [[ "$NO_XMR" != 1 ]]; then
		ANDROID_XMR_SUMS="$(cd "${SRC_DIR}/wallet/asset/xmr/lib/${ANDROID_XMR_PLATFORM}" &&
			sha256sum libmonero_wallet2_api_c.so libc++_shared.so)"
	fi

	BUILD_NUMBER="${build:-$(git -C "$REPO_ROOT" rev-list --count "$COMMIT")}"
	[[ "$BUILD_NUMBER" =~ ^[1-9][0-9]*$ && "$BUILD_NUMBER" -le "$ANDROID_MAX_VERSION_CODE" ]] ||
		die "the build number must be an integer from 1 to ${ANDROID_MAX_VERSION_CODE}, got ${BUILD_NUMBER}"
	log "build number ${BUILD_NUMBER}"

	local gradle_file="${SRC_DIR}/wallet/android/app/build.gradle"
	ANDROID_BUILD_TOOLS="$(sed -n "s/.*buildToolsVersion = '\(.*\)'.*/\1/p" "$gradle_file")"
	[[ "$ANDROID_BUILD_TOOLS" =~ ^[0-9]+(\.[0-9]+){2}$ ]] ||
		die "cannot read buildToolsVersion from ${gradle_file} (got '${ANDROID_BUILD_TOOLS}')"

	select_image ""
	ensure_image
	mkdir -p "${RELEASE_DIR}/out/.cache/gradle"

	# Results of an earlier run do not outlive the start of this one, and the
	# build stage cannot reach the place the final ones go.
	rm -rf "${OUT_DIR}/android" "${OUT_DIR}/android-staging" "${OUT_DIR}/android-unsigned"
	stage_credentials none
	stage_workspace src android-unsigned
	run_stage build

	stage_workspace
	stage_private
	stage_offline
	if [[ "$NO_SIGN" != 1 ]]; then
		stage_credentials only "${ANDROID_SIGNING_VARS[@]}" -- "$ANDROID_APP_KEYSTORE" "$ANDROID_UPLOAD_KEYSTORE"
	fi
	run_stage package
}

if [[ "${1:-}" == "--stage" ]]; then
	"stage_$2"
else
	host_main "$@"
fi
