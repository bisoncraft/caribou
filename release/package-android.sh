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
# Gradle never sees a keystore: it emits both artifacts unsigned and they are
# signed here, so the passwords stay out of the Gradle daemon and its caches.

source "$(dirname "$0")/common.sh"

ANDROID_APP_ID="com.bisoncraft.caribou"
ANDROID_ABI="arm64-v8a"
ANDROID_XMR_PLATFORM="android-arm64"
ANDROID_FLAVOR="netMainnet"
# Android's ceiling for versionCode.
ANDROID_MAX_VERSION_CODE=2100000000

CONTAINER_VARS+=(BUILD_NUMBER)
SECRET_VARS+=(ANDROID_APP_KEYSTORE ANDROID_APP_KEY_ALIAS ANDROID_APP_KEYSTORE_PASSWORD
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

# check_native_libs <apk>: the libraries the APK must carry are present,
# stored uncompressed and page-aligned in the archive, and linked with 16 KB
# segment alignment. Android 15 devices with 16 KB pages refuse anything less,
# and so does Google Play.
check_native_libs() {
	local apk="$1" tmp so want=(libgojni.so)
	with_xmr && want+=(libmonero_wallet2_api_c.so libc++_shared.so)
	zipalign -c -P 16 4 "$apk" || die "$(basename "$apk") is not zip-aligned to 16 KB"
	tmp="$(mktemp -d)"
	unzip -q "$apk" 'lib/*' -d "$tmp"
	[[ "$(ls "${tmp}/lib")" == "$ANDROID_ABI" ]] || die "$(basename "$apk") carries ABIs other than ${ANDROID_ABI}: $(ls "${tmp}/lib" | tr '\n' ' ')"
	for so in "${want[@]}"; do
		[[ -f "${tmp}/lib/${ANDROID_ABI}/${so}" ]] || die "$(basename "$apk") has no ${so}"
	done
	local align segments
	for so in "${tmp}/lib/${ANDROID_ABI}"/*.so; do
		# Every LOAD segment's alignment, the last column of readelf -lW, in
		# hex. bash does the comparison: the image's awk has no strtonum.
		segments=0
		while read -r align; do
			segments=$((segments + 1))
			((align >= 16384)) || die "$(basename "$so") has a LOAD segment aligned to ${align}, below 16 KB"
		done < <(readelf -lW "$so" | awk '$1 == "LOAD" { print $NF }')
		((segments > 0)) || die "readelf found no LOAD segments in $(basename "$so")"
	done
	if with_xmr; then
		cmp -s "${tmp}/lib/${ANDROID_ABI}/libmonero_wallet2_api_c.so" "wallet/asset/xmr/lib/${ANDROID_XMR_PLATFORM}/libmonero_wallet2_api_c.so" ||
			die "the XMR library in $(basename "$apk") is not the verified one"
	fi
	rm -rf "$tmp"
	log "native libraries: $(printf '%s ' "${want[@]}")present, 16 KB aligned"
}

# check_manifest <apk>: the package identity is the mainnet one and the
# version is the one asked for.
check_manifest() {
	local apk="$1" badging
	badging="$(aapt2 dump badging "$apk")"
	grep -q "^package: name='${ANDROID_APP_ID}' versionCode='${BUILD_NUMBER}' versionName='${VERSION}' " <<<"$badging" ||
		die "unexpected package line in $(basename "$apk"): $(grep '^package:' <<<"$badging")"
	log "$(grep -E "^(minSdkVersion|targetSdkVersion)" <<<"$badging" | tr -d "'" | tr '\n' ' ')"
}

# ---------------------------------------------------------------------------
stage_build() {
	local out=/work/android
	rm -rf "$out"
	mkdir -p "$out"

	[[ -d "${ANDROID_HOME:-/nonexistent}" && -x /opt/android-jdk/bin/java ]] ||
		die "the builder image has no Android toolchain; rerun with --build-image"
	export JAVA_HOME=/opt/android-jdk
	# osxcross puts an xcrun on PATH. gomobile takes any xcrun to mean Xcode is
	# installed and fails probing for an iOS SDK, so that directory is dropped
	# for this build.
	PATH="${JAVA_HOME}/bin:${PATH//\/opt\/osxcross\/bin:/}"
	export GRADLE_USER_HOME=/cache/gradle

	# The versions of the SDK pieces come from the Gradle build, so that file
	# stays the one place they are stated.
	local gradle_file=wallet/android/app/build.gradle build_tools
	build_tools="$(sed -n "s/.*buildToolsVersion = '\(.*\)'.*/\1/p" "$gradle_file")"
	[[ -d "${ANDROID_HOME}/build-tools/${build_tools}" ]] ||
		die "${gradle_file} names build tools ${build_tools:-(none)}, which the builder image does not carry; update release/Dockerfile and rerun with --build-image"
	PATH="${ANDROID_HOME}/build-tools/${build_tools}:${PATH}"

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

	local apk="${out}/caribou-${VERSION}-android-arm64.apk" aab="${out}/caribou-${VERSION}.aab"
	if [[ "$NO_SIGN" == 1 ]]; then
		warn "not signing the APK or the bundle (--no-sign); neither can be installed or uploaded"
		apk="${apk%.apk}-unsigned.apk" aab="${aab%.aab}-unsigned.aab"
		cp "$apk_in" "$apk"
		cp "$aab_in" "$aab"
	else
		sign_apk "$apk_in" "$apk"
		sign_bundle "$aab_in" "$aab"
	fi

	check_manifest "$apk"
	check_native_libs "$apk"
	bundletool validate --bundle "$aab" >/dev/null || die "bundletool rejected $(basename "$aab")"
	log "bundletool accepts $(basename "$aab")"

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
	export_source
	stage_xmr_libs "$ANDROID_XMR_PLATFORM" libmonero_wallet2_api_c.so
	# monero_c needs the libc++ of the NDK it was built with at run time.
	stage_xmr_libs "$ANDROID_XMR_PLATFORM" libc++_shared.so

	BUILD_NUMBER="${build:-$(git -C "$REPO_ROOT" rev-list --count "$COMMIT")}"
	[[ "$BUILD_NUMBER" =~ ^[1-9][0-9]*$ && "$BUILD_NUMBER" -le "$ANDROID_MAX_VERSION_CODE" ]] ||
		die "the build number must be an integer from 1 to ${ANDROID_MAX_VERSION_CODE}, got ${BUILD_NUMBER}"
	log "build number ${BUILD_NUMBER}"

	select_image ""
	ensure_image
	mkdir -p "${RELEASE_DIR}/out/.cache/gradle"
	run_stage build
}

if [[ "${1:-}" == "--stage" ]]; then
	"stage_$2"
else
	host_main "$@"
fi
