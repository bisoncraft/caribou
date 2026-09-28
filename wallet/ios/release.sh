#!/usr/bin/env bash
# Archives the mainnet Release build and exports a signed .ipa for TestFlight /
# the App Store, optionally uploading it.
#
#   ./release.sh                 # archive + export to build/Caribou.ipa
#   ./release.sh --upload        # ... and upload to App Store Connect
#   ./release.sh --build 7       # override CFBundleVersion (see below)
#
# Prereqs, in order:
#   1. ./bind.sh (or CARIBOU_XMR=1 ./bind.sh) — builds Frameworks/*.xcframework
#   2. xcodegen generate [--spec project.xmr.yml]
#   3. Caribou/Config/Local.xcconfig with DEVELOPMENT_TEAM
#   4. Credentials — an App Store Connect API key, or an Apple ID signed into
#      Xcode plus an app-specific password for --upload. See CREDENTIALS below.
#
# Always archives the *mainnet* scheme: testnet/simnet use different bundle ids
# that are not registered for distribution, and shipping a testnet build to the
# store would be a bad day.
#
# ---------------------------------------------------------------------------
# CREDENTIALS — two ways, pick whichever your App Store Connect role allows.
#
# (A) App Store Connect API key. Best for CI: no 2FA, no GUI session, and it
#     covers provisioning as well as upload. But ONLY the team's Account Holder
#     can create one (Users and Access > Integrations), so an Admin cannot set
#     this up for themselves. Download the .p8 once — Apple will not show it
#     again — and put it where the tools look:
#
#         mkdir -p ~/.appstoreconnect/private_keys
#         mv ~/Downloads/AuthKey_ABC123XYZ.p8 ~/.appstoreconnect/private_keys/
#         export ASC_KEY_ID=ABC123XYZ
#         export ASC_ISSUER_ID=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee
#
#     Never commit the .p8.
#
# (B) Apple ID + app-specific password. Any Admin or App Manager can do this
#     without involving the Account Holder. Generate the password at
#     appleid.apple.com > Sign-In and Security > App-Specific Passwords:
#
#         export APPLE_ID=you@example.com
#         read -rs -p "App-specific password: " APPLE_PASSWORD && export APPLE_PASSWORD && echo
#
#     An app-specific password is bound to the Apple ID, not to an app or a
#     team. ASC_APPLE_ID / ASC_APP_PASSWORD override them if the two need to
#     differ. (The desktop release notarizes with the API key of option (A)
#     instead, via rcodesign; see release/README.md.)
#
# (C) Apple ID + the password in the login keychain. Same as (B), but store it
#     once instead of exporting it, so no shell, history, or transcript ever
#     holds the secret:
#
#         export APPLE_ID=you@example.com
#         security add-generic-password -s caribou-appstore -a "$APPLE_ID" -w
#
#     (-w with no value prompts, without echoing.) Then --upload just works,
#     with only the non-secret APPLE_ID in the environment.
#
# (B) and (C) cover the *upload* only. Provisioning then needs an Apple ID
#     signed into Xcode (Xcode > Settings > Accounts), which is the same
#     credential, entered once in the GUI.
#
#     If your Apple ID belongs to more than one team, altool cannot guess which
#     to upload to and fails with "multiple providers". List them with
#         xcrun altool --list-providers -u "$APPLE_ID" -p "@env:APPLE_PASSWORD"
#     and export the ProviderShortname of the org:
#         export ASC_PROVIDER=Bisoncraft
# ---------------------------------------------------------------------------
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

SCHEME="Caribou (mainnet)"
CONFIG="Release-mainnet"
builddir="$here/build"
archive="$builddir/Caribou.xcarchive"
export_dir="$builddir/export"

upload=0
build_number=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --upload)  upload=1; shift ;;
        --build)   build_number="${2:?--build needs a number}"; shift 2 ;;
        # Print the whole header comment block, not a fixed line range: the
        # range drifts every time CREDENTIALS grows, and --help is exactly what
        # the failure path at the bottom tells you to read.
        -h|--help) awk 'NR==1{next} /^#/{print; next} {exit}' "$0"; exit 0 ;;
        *) echo "error: unknown argument '$1'" >&2; exit 1 ;;
    esac
done

[[ -d "$here/Caribou.xcodeproj" ]] || {
    echo "error: Caribou.xcodeproj missing. Run: xcodegen generate --spec project.xmr.yml" >&2; exit 1; }
[[ -d "$here/Frameworks/Mobile.xcframework" ]] || {
    echo "error: Frameworks/Mobile.xcframework missing. Run: ./bind.sh" >&2; exit 1; }
# A project generated from project.xmr.yml embeds Monero.xcframework too. Check
# it here rather than letting xcodebuild discover it several minutes into the
# archive: it is the same pure configuration error, knowable up front.
if grep -q 'Monero\.xcframework' "$here/Caribou.xcodeproj/project.pbxproj" 2>/dev/null; then
    [[ -d "$here/Frameworks/Monero.xcframework" ]] || {
        echo "error: the project embeds Frameworks/Monero.xcframework but it is missing." >&2
        echo "       Run: CARIBOU_XMR=1 ./bind.sh" >&2; exit 1; }
fi

# -allowProvisioningUpdates has to authenticate to the developer portal to
# register the App ID and mint the provisioning profile. It can do that from an
# Apple ID signed into Xcode's GUI, or from an App Store Connect API key. Prefer
# the key: it is the same credential the upload uses, it survives on a machine
# with no Xcode GUI session, and it does not trip 2FA. If the key is absent, fall
# through with no auth flags so a signed-in Xcode account still works.
#
# The ${auth[@]+"${auth[@]}"} expansion at the use sites is not decoration: under
# `set -u`, bash 3.2 -- still /bin/bash on macOS -- treats a plain "${auth[@]}" on
# an empty array as an unbound variable and aborts.
auth=()
if [[ -n "${ASC_KEY_ID:-}" && -n "${ASC_ISSUER_ID:-}" ]]; then
    keyfile=""
    for d in "$HOME/.appstoreconnect/private_keys" "$HOME/private_keys" \
             "$HOME/.private_keys" "./private_keys"; do
        [[ -f "$d/AuthKey_${ASC_KEY_ID}.p8" ]] && { keyfile="$d/AuthKey_${ASC_KEY_ID}.p8"; break; }
    done
    if [[ -n "$keyfile" ]]; then
        echo "Provisioning with App Store Connect API key $ASC_KEY_ID"
        auth=(-authenticationKeyPath "$keyfile"
              -authenticationKeyID "$ASC_KEY_ID"
              -authenticationKeyIssuerID "$ASC_ISSUER_ID")
    else
        echo "warning: ASC_KEY_ID is set but AuthKey_${ASC_KEY_ID}.p8 was not found." >&2
        echo "         Looked in ~/.appstoreconnect/private_keys and ~/private_keys." >&2
        echo "         Falling back to an Xcode-signed-in Apple ID for provisioning." >&2
    fi
fi

# Pull the team id out of the resolved build settings rather than duplicating it
# here, so it keeps living only in the git-ignored Local.xcconfig.
team="$(xcodebuild -project Caribou.xcodeproj -target Caribou -configuration "$CONFIG" \
        -showBuildSettings 2>/dev/null | awk -F' = ' '/ DEVELOPMENT_TEAM = /{print $2; exit}')"
[[ -n "$team" ]] || {
    echo "error: DEVELOPMENT_TEAM is empty. Create Caribou/Config/Local.xcconfig:" >&2
    echo "         DEVELOPMENT_TEAM = YOURTEAMID" >&2; exit 1; }
echo "Signing with team $team"

# CFBundleVersion must strictly increase for every upload to a given
# CFBundleShortVersionString; App Store Connect rejects a duplicate outright and
# the build is burned (you cannot reuse the number even after deleting it). The
# commit count needs no state committed to the repo, and it rises monotonically
# along a single history — which holds as long as releases are cut from master.
# It does NOT hold across branches: a release from a feature branch followed by
# one from a master that squash-merged it yields a *lower* number the second
# time, as does a rebase that drops or squashes commits. --validate-app runs
# before --upload-app below and catches that, so the cost is a wasted archive
# rather than a burned build number; --build N is the override.
if [[ -z "$build_number" ]]; then
    build_number="$(git -C "$here" rev-list --count HEAD 2>/dev/null || echo 1)"
fi
echo "Build number (CFBundleVersion): $build_number"

# Resolve upload credentials BEFORE the archive: discovering there are none
# after several minutes of building is a waste, and the failure is a pure
# configuration error that is knowable up front.
if [[ "$upload" == "1" ]]; then
    # Either credential works for the upload; see CREDENTIALS at the top.
    #
    # The Apple ID pair falls back to APPLE_ID / APPLE_PASSWORD. An app-specific
    # password belongs to the Apple ID, not to an app or a team, so one password
    # serves every upload. ASC_* wins when both are set, so the two can still be
    # pointed at different accounts.
    altool_auth=()
    apple_id="${ASC_APPLE_ID:-${APPLE_ID:-}}"
    # Pass the *name* of the variable holding the password, never its value: altool's
    # @env: indirection keeps the secret out of the process list, where a plain -p
    # would be readable by every other process on the machine.
    if   [[ -n "${ASC_APP_PASSWORD:-}" ]]; then apple_pw_var=ASC_APP_PASSWORD
    elif [[ -n "${APPLE_PASSWORD:-}"   ]]; then apple_pw_var=APPLE_PASSWORD
    else apple_pw_var=""
    fi

    # Last resort: the login keychain. Store the app-specific password once with
    #
    #     security add-generic-password -s caribou-appstore -a "$APPLE_ID" -w
    #
    # (-w with no value prompts for it, without echoing) and no shell here ever needs
    # to hold it. Worth preferring over an exported variable: an app-specific
    # password typed at a prompt stays out of shell history, out of any transcript,
    # and out of `ps` and /proc for the whole process tree.
    KEYCHAIN_SERVICE="caribou-appstore"
    if [[ -z "$apple_pw_var" && -n "$apple_id" ]]; then
        if pw="$(security find-generic-password -s "$KEYCHAIN_SERVICE" -a "$apple_id" -w 2>/dev/null)"; then
            echo "Using app-specific password from keychain ($KEYCHAIN_SERVICE/$apple_id)"
            export CARIBOU_ASC_PASSWORD="$pw"
            apple_pw_var=CARIBOU_ASC_PASSWORD
            unset pw
        fi
    fi

    if [[ -n "${ASC_KEY_ID:-}" && -n "${ASC_ISSUER_ID:-}" ]]; then
        altool_auth=(--apiKey "$ASC_KEY_ID" --apiIssuer "$ASC_ISSUER_ID")
    elif [[ -n "$apple_id" && -n "$apple_pw_var" ]]; then
        echo "Uploading as $apple_id (password from \$$apple_pw_var)"
        altool_auth=(-u "$apple_id" -p "@env:$apple_pw_var")
        # An Apple ID in more than one team is ambiguous to altool, and it errors out
        # rather than picking one. ASC_PROVIDER names the org. Deliberately NOT
        # falling back to the desktop's APPLE_TEAM_ID: that is a team id, while
        # altool wants a provider *shortname*, and the desktop signs under a
        # different team anyway. Harmless to omit on a single-team Apple ID.
        [[ -n "${ASC_PROVIDER:-}" ]] && altool_auth+=(--asc-provider "$ASC_PROVIDER")
    else
        echo "error: no upload credentials. Use one of:" >&2
        echo "  ASC_KEY_ID + ASC_ISSUER_ID    App Store Connect API key" >&2
        echo "  APPLE_ID + APPLE_PASSWORD     Apple ID + app-specific password" >&2
        echo "  APPLE_ID + login keychain     store the password once, no env var needed:" >&2
        echo "                                  security add-generic-password \\" >&2
        echo "                                    -s $KEYCHAIN_SERVICE -a \"\$APPLE_ID\" -w" >&2
        if [[ -n "$apple_id" ]]; then
            echo >&2
            echo "APPLE_ID is set ($apple_id) but no password was found for it." >&2
        fi
        echo "See CREDENTIALS at the top of this script." >&2
        exit 1
    fi
fi

rm -rf "$archive" "$export_dir"
mkdir -p "$builddir"

# Archive UNSIGNED, and let the export step below do the distribution signing.
#
# This is not a shortcut, it is the only route that works here. Under automatic
# signing, xcodebuild signs the archive itself for *development* and only
# re-signs for distribution at export. Minting that development profile needs an
# Apple Development certificate for the team and at least one registered device,
# and a team that only ever ships through the App Store has neither:
#
#   error: Your team has no devices from which to generate a provisioning
#          profile. ... No profiles for 'com.bisoncraft.caribou' were found
#
# Forcing CODE_SIGN_IDENTITY=Apple Distribution instead does not help; automatic
# signing rejects a manually specified identity outright ("conflicting
# provisioning settings"). Skipping signing here sidesteps the development
# profile entirely, and -exportArchive then signs with the App Store profile,
# which requires no devices.
echo "==> Archiving $SCHEME ($CONFIG), unsigned"
xcodebuild archive \
    -project Caribou.xcodeproj \
    -scheme "$SCHEME" \
    -configuration "$CONFIG" \
    -destination "generic/platform=iOS" \
    -archivePath "$archive" \
    CODE_SIGNING_ALLOWED=NO \
    CURRENT_PROJECT_VERSION="$build_number"

# Inject the team id into a copy of the export options so the committed plist
# stays team-agnostic.
opts="$builddir/ExportOptions.resolved.plist"
cp "$here/ExportOptions.plist" "$opts"
/usr/libexec/PlistBuddy -c "Add :teamID string $team" "$opts" >/dev/null 2>&1 \
    || /usr/libexec/PlistBuddy -c "Set :teamID $team" "$opts"

# This is where signing actually happens, and the only step that talks to the
# developer portal: it registers the App ID if new, mints the App Store
# provisioning profile, and signs the app and both embedded frameworks.
echo "==> Exporting and signing .ipa"
xcodebuild -exportArchive \
    -archivePath "$archive" \
    -exportOptionsPlist "$opts" \
    -exportPath "$export_dir" \
    -allowProvisioningUpdates ${auth[@]+"${auth[@]}"}

ipa="$(find "$export_dir" -maxdepth 1 -name '*.ipa' -print -quit)"
[[ -n "$ipa" ]] || { echo "error: no .ipa produced in $export_dir" >&2; exit 1; }
echo "Exported $ipa"

if [[ "$upload" != "1" ]]; then
    echo
    echo "Not uploading (pass --upload to send it to App Store Connect)."
    exit 0
fi


# Validate before uploading: a rejected upload burns the build number, a failed
# validation does not.
echo "==> Validating"
xcrun altool --validate-app -f "$ipa" -t ios "${altool_auth[@]}"

echo "==> Uploading"
xcrun altool --upload-app -f "$ipa" -t ios "${altool_auth[@]}"

echo
echo "Uploaded. It takes a few minutes to finish processing before it shows up"
echo "in TestFlight; App Store Connect emails if processing fails."
