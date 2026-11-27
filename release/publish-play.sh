#!/usr/bin/env bash
# Upload the Android App Bundle that package-android.sh built to Google Play
# and attach it to a release on a testing track.
#
#   release/publish-play.sh v1.2.0 [--track internal] [--rollout] [--check]
#
# By default the release is created as a draft on the internal testing track:
# nothing reaches a tester until someone rolls the draft out in the Play
# Console, and promotion to production always happens there. This mirrors
# release.sh --publish, which opens a draft GitHub release.
#
# The Google Play Developer API works in "edits": a private copy of the app's
# state that changes are staged in and that takes effect only when committed.
# This script opens an edit, uploads the bundle into it, points the track at
# the new versionCode, has Google validate the result and commits. A failure
# at any step deletes the edit, which leaves the app as it was.
#
# The API cannot create an app or accept its first build. The app record and
# the first bundle, along with the Play App Signing enrollment that happens
# on that upload, are done by hand in the Play Console.
#
# Authentication is the service account key named by PLAY_SERVICE_ACCOUNT_JSON
# in release/.env/env.sh (see env.example). The container this runs in gets
# that key and no other credential, and a home directory of its own: the one
# the builds share is writable by them, and curl would read a .curlrc from it.
# Every curl call also starts with -q, which turns that file off.

source "$(dirname "$0")/common.sh"

ANDROID_APP_ID="com.bisoncraft.caribou"
PLAY_API="https://androidpublisher.googleapis.com/androidpublisher/v3/applications/${ANDROID_APP_ID}"
PLAY_UPLOAD_API="https://androidpublisher.googleapis.com/upload/androidpublisher/v3/applications/${ANDROID_APP_ID}"
PLAY_SCOPE="https://www.googleapis.com/auth/androidpublisher"

CONTAINER_VARS+=(PLAY_TRACK PLAY_STATUS PLAY_CHECK)

usage() {
	cat <<USAGE
usage: $(basename "$0") vX.Y.Z [options]
  vX.Y.Z             release tag whose bundle, out/<version>/android/*.aab, is
                     uploaded
  --ref <commit>     the same --ref the bundle was built with, if any
  --track <name>     internal (default), alpha (closed testing) or beta (open
                     testing). production is refused: promote in the Console.
  --rollout          make the release available to the track's testers at
                     once, instead of leaving a draft
  --check            upload and have Google validate the release, then discard
                     the edit without committing. Nothing changes in the Play
                     Console.
USAGE
}

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

# play_token <service-account.json>: exchange the service account key for a
# short-lived access token (RFC 7523). The private key is piped to openssl and
# never written to disk. The signed assertion can be exchanged for a token by
# anyone who sees it until it expires, so it reaches curl in a config on
# stdin, not as an argument.
play_token() {
	local sa="$1" email now header claims sig
	email="$(jq -er .client_email "$sa")" || die "$(basename "$sa") is not a service account key"
	now="$(date +%s)"
	header="$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)"
	claims="$(jq -cn --arg iss "$email" --arg scope "$PLAY_SCOPE" --argjson iat "$now" \
		'{iss: $iss, scope: $scope, aud: "https://oauth2.googleapis.com/token", iat: $iat, exp: ($iat + 600)}' | b64url)"
	sig="$(printf '%s.%s' "$header" "$claims" | openssl dgst -sha256 -sign <(jq -r .private_key "$sa") | b64url)"
	curl -q -fsS --max-time 60 -K - https://oauth2.googleapis.com/token \
		--data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer' \
		<<<"data-urlencode = \"assertion=${header}.${claims}.${sig}\"" | jq -er .access_token
}

# play <method> <url> [curl args...]: an authenticated API call. Prints the
# response body. Returns 1 when Google refused the request (HTTP 4xx, with its
# message as a warning) and 2 when there is no telling whether the request
# took effect: no response, or a server error.
play() {
	local method="$1" url="$2"; shift 2
	local body code
	body="$(mktemp)"
	# The token goes in through a config on stdin, not an argument, so it does
	# not appear in the process list.
	if ! code="$(curl -q -sS --max-time 900 -X "$method" -o "$body" -w '%{http_code}' -K - "$@" "$url" \
		<<<"header = \"Authorization: Bearer ${PLAY_TOKEN}\"")"; then
		rm -f "$body"
		warn "no response to ${method} ${url##*/applications/}"
		return 2
	fi
	if [[ "$code" != 2* ]]; then
		warn "${method} ${url##*/applications/} returned HTTP ${code}: $(jq -r '.error.message // empty' "$body" 2>/dev/null || head -c 300 "$body")"
		rm -f "$body"
		[[ "$code" == 4* ]] && return 1
		return 2
	fi
	cat "$body"
	rm -f "$body"
}

# discard_edit <id>: delete an edit that will not be committed.
discard_edit() {
	play DELETE "${PLAY_API}/edits/$1" >/dev/null
}

# ---------------------------------------------------------------------------
stage_publish() {
	# package-android.sh gives the bundle its name and writes SHA256SUMS only
	# once every check on the signed artifacts has passed, so a bundle is
	# uploaded only by that name and only if it still matches that file.
	cd /
	local name="caribou-${VERSION}.aab"
	local aab="/work/android/${name}" sums=/work/android/SHA256SUMS
	[[ -f "$aab" && -f "$sums" ]] || die "no finished Android build in out/${VERSION}/android; run package-android.sh first"
	awk -v n="$name" '$2 == n { found = 1 } END { exit !found }' "$sums" || die "SHA256SUMS in out/${VERSION}/android does not list ${name}"
	(cd /work/android && sha256sum --check --strict --quiet SHA256SUMS) ||
		die "out/${VERSION}/android is not as package-android.sh left it; run it again"
	local sa="/secrets/${PLAY_SERVICE_ACCOUNT_JSON:-}"
	[[ -n "${PLAY_SERVICE_ACCOUNT_JSON:-}" && -f "$sa" ]] ||
		die "PLAY_SERVICE_ACCOUNT_JSON is not set or not found under release/.env (see env.example)"

	PLAY_TOKEN="$(play_token "$sa")" || die "Google refused the service account key"

	local edit
	edit="$(play POST "${PLAY_API}/edits" | jq -er .id)" || die "could not open an edit"
	# From here on, leaving without a commit deletes the edit. An edit that
	# cannot be deleted changes nothing in the app and expires on its own.
	trap 'discard_edit "'"$edit"'" 2>/dev/null || warn "could not delete edit '"$edit"'; it was not committed and expires on its own"' EXIT
	log "opened edit ${edit}"

	log "uploading ${name} ($(du -h "$aab" | cut -f1))"
	local uploaded code sum
	uploaded="$(play POST "${PLAY_UPLOAD_API}/edits/${edit}/bundles?uploadType=media" \
		-H 'Content-Type: application/octet-stream' --data-binary "@${aab}")" || die "the bundle upload failed"
	code="$(jq -er .versionCode <<<"$uploaded")"
	sum="$(jq -r .sha256 <<<"$uploaded")"
	[[ "$sum" == "$(sha256sum "$aab" | cut -d' ' -f1)" ]] || die "Google reports a different sha256 for the bundle than the local file has"
	log "Google accepted the bundle as versionCode ${code}"

	local release
	release="$(jq -cn --arg track "$PLAY_TRACK" --arg name "$VERSION" --arg code "$code" --arg status "$PLAY_STATUS" \
		'{track: $track, releases: [{name: $name, versionCodes: [$code], status: $status}]}')"
	play PUT "${PLAY_API}/edits/${edit}/tracks/${PLAY_TRACK}" -H 'Content-Type: application/json' -d "$release" >/dev/null ||
		die "could not set the ${PLAY_TRACK} track"

	play POST "${PLAY_API}/edits/${edit}:validate" >/dev/null || die "Google rejected the release"
	if [[ "$PLAY_CHECK" == 1 ]]; then
		trap - EXIT
		discard_edit "$edit" ||
			die "Google validated the release, but edit ${edit} could not be deleted. It was not committed, so the app is unchanged, and it expires on its own."
		log "Google validated a ${PLAY_STATUS} release of ${VERSION} (${code}) on ${PLAY_TRACK}; the edit is deleted (--check)"
		return 0
	fi

	local rc=0
	play POST "${PLAY_API}/edits/${edit}:commit" >/dev/null || rc=$?
	case "$rc" in
	0) trap - EXIT ;;
	1) die "Google refused the commit; nothing was published" ;;
	*)
		# The commit may have been carried out before the answer was lost.
		# Deleting the edit now would be a guess, so it is left alone.
		trap - EXIT
		die "the commit got no clear answer: the release may or may not be on the ${PLAY_TRACK} track.
Look at the track in the Play Console before running this again. If versionCode ${code} is there, the
release went through; Google refuses a second upload of the same versionCode."
		;;
	esac
	log "${PLAY_STATUS} release ${VERSION} (${code}) is on the ${PLAY_TRACK} track"
}

# ---------------------------------------------------------------------------
host_main() {
	PLAY_TRACK=internal PLAY_STATUS=draft PLAY_CHECK=0
	parse_common_args "$@"
	set -- "${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}"
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--track)
			[[ $# -ge 2 ]] || die "--track needs a name"
			PLAY_TRACK="$2"
			shift
			;;
		--rollout) PLAY_STATUS=completed ;;
		--check) PLAY_CHECK=1 ;;
		*) die "unknown option: $1" ;;
		esac
		shift
	done
	case "$PLAY_TRACK" in
	internal | alpha | beta) ;;
	production) die "this script does not release to production; promote the release in the Play Console" ;;
	*) die "unknown track: ${PLAY_TRACK}" ;;
	esac
	[[ "$DIRTY" == 1 ]] && die "a working-tree build is not uploaded to Google Play"
	resolve_version
	load_env
	[[ -f "${OUT_DIR}/android/SHA256SUMS" ]] || die "no finished Android build for ${VERSION}; run package-android.sh first"
	# Held so that a package-android.sh run cannot replace the bundle while it
	# is being read. The container checks the bundle again under the lock.
	lock_version
	[[ -n "${PLAY_SERVICE_ACCOUNT_JSON:-}" ]] || die "PLAY_SERVICE_ACCOUNT_JSON is not set in release/.env/env.sh (see env.example)"
	select_image ""
	ensure_image
	stage_credentials only PLAY_SERVICE_ACCOUNT_JSON -- "$PLAY_SERVICE_ACCOUNT_JSON"
	stage_private
	stage_workspace android
	run_stage publish
}

if [[ "${1:-}" == "--stage" ]]; then
	"stage_$2"
else
	host_main "$@"
fi
