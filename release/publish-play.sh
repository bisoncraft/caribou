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
# in release/.env/env.sh (see env.example).

source "$(dirname "$0")/common.sh"

ANDROID_APP_ID="com.bisoncraft.caribou"
PLAY_API="https://androidpublisher.googleapis.com/androidpublisher/v3/applications/${ANDROID_APP_ID}"
PLAY_UPLOAD_API="https://androidpublisher.googleapis.com/upload/androidpublisher/v3/applications/${ANDROID_APP_ID}"
PLAY_SCOPE="https://www.googleapis.com/auth/androidpublisher"

CONTAINER_VARS+=(PLAY_TRACK PLAY_STATUS PLAY_CHECK)
SECRET_VARS+=(PLAY_SERVICE_ACCOUNT_JSON)

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
# never written to disk.
play_token() {
	local sa="$1" email now header claims sig
	email="$(jq -er .client_email "$sa")" || die "$(basename "$sa") is not a service account key"
	now="$(date +%s)"
	header="$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)"
	claims="$(jq -cn --arg iss "$email" --arg scope "$PLAY_SCOPE" --argjson iat "$now" \
		'{iss: $iss, scope: $scope, aud: "https://oauth2.googleapis.com/token", iat: $iat, exp: ($iat + 600)}' | b64url)"
	sig="$(printf '%s.%s' "$header" "$claims" | openssl dgst -sha256 -sign <(jq -r .private_key "$sa") | b64url)"
	curl -fsS --max-time 60 https://oauth2.googleapis.com/token \
		--data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer' \
		--data-urlencode "assertion=${header}.${claims}.${sig}" | jq -er .access_token
}

# play <method> <url> [curl args...]: an authenticated API call. Prints the
# response body; on an HTTP error, prints Google's message and fails.
play() {
	local method="$1" url="$2"; shift 2
	local body code
	body="$(mktemp)"
	# The token goes in through a config on stdin, not an argument, so it does
	# not appear in the process list.
	code="$(curl -sS --max-time 900 -X "$method" -o "$body" -w '%{http_code}' -K - "$@" "$url" \
		<<<"header = \"Authorization: Bearer ${PLAY_TOKEN}\"")" || { rm -f "$body"; die "request failed: ${method} ${url}"; }
	if [[ "$code" != 2* ]]; then
		warn "${method} ${url##*/applications/} returned HTTP ${code}: $(jq -r '.error.message // empty' "$body" 2>/dev/null || head -c 300 "$body")"
		rm -f "$body"
		return 1
	fi
	cat "$body"
	rm -f "$body"
}

# ---------------------------------------------------------------------------
stage_publish() {
	local aab="" f
	for f in /work/android/*.aab; do
		if [[ -f "$f" && "$f" != *-unsigned.aab ]]; then aab="$f"; break; fi
	done
	[[ -n "$aab" ]] || die "no signed bundle in out/${VERSION}/android; run package-android.sh first"
	local sa="/secrets/${PLAY_SERVICE_ACCOUNT_JSON:-}"
	[[ -n "${PLAY_SERVICE_ACCOUNT_JSON:-}" && -f "$sa" ]] ||
		die "PLAY_SERVICE_ACCOUNT_JSON is not set or not found under release/.env (see env.example)"

	PLAY_TOKEN="$(play_token "$sa")" || die "Google refused the service account key"

	local edit
	edit="$(play POST "${PLAY_API}/edits" | jq -er .id)" || die "could not open an edit"
	# From here on, leaving without a commit deletes the edit.
	trap 'play DELETE "${PLAY_API}/edits/'"$edit"'" >/dev/null 2>&1 || true' EXIT
	log "opened edit ${edit}"

	log "uploading $(basename "$aab") ($(du -h "$aab" | cut -f1))"
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
		log "Google validated a ${PLAY_STATUS} release of ${VERSION} (${code}) on ${PLAY_TRACK}; discarding the edit (--check)"
		return 0
	fi
	play POST "${PLAY_API}/edits/${edit}:commit" >/dev/null || die "the commit failed; nothing was published"
	trap - EXIT
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
	[[ -d "${OUT_DIR}/android" ]] || die "nothing built for ${VERSION}; run package-android.sh first"
	select_image ""
	ensure_image
	run_stage publish
}

if [[ "${1:-}" == "--stage" ]]; then
	"stage_$2"
else
	host_main "$@"
fi
