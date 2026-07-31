#!/usr/bin/env bash

set -e

cd "$(dirname "$0")"

# For release, remove pre-release info, and set metadata to "release".
VER="1.1.0-rc2" # pre, beta, rc1, etc.
META="release.local" # "release"
CARIBOU_EXE="caribou"

# Parse flags.
while [[ "$#" -gt 0 ]]; do
  case $1 in
    --xmr) TAGS_CARIBOU="${TAGS_CARIBOU:+${TAGS_CARIBOU} }xmr"; export CGO_ENABLED=1 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
  shift
done

export CGO_ENABLED=${CGO_ENABLED:-0}
export GOWORK=off

# if META set, append "+${META}", otherwise nothing.
LDFLAGS_BASE="-buildid= -s -w -X main.Version=${VER}${META:++${META}}"

# Build the webpack bundle prior to building the appserver package, which embeds
# the files.
pushd ../../../../wallet/appserver
npm ci
npm run build --workspace=newui
popd

# Install this electron project's own deps (electron-forge + the makers).
# --ignore-scripts skips the package's "prepare" lifecycle (prepare.sh), since
# pkg.sh does its own resource setup below with the correct --xmr handling.
npm ci --ignore-scripts

rm -rf out # This is the default output dir for electron-forge make

# Use folders outside of pwd to prevent electron-forge from adding them to build.
# The "ignore" packaging config seems to malfunction: https://github.com/electron/forge/issues/673#issuecomment-697180654
pushd ..
rm -rf resources
rm -rf installers
mkdir -p installers
popd

build_targets (){
  for TARGET in ${TARGETS}; do
    OS=${TARGET%%/*}
    ARCH=${TARGET##*/}
    echo "Building for ${OS}-${ARCH}"

    pushd ..
    mkdir -p "resources/mac"
    popd

    pushd ../../../../wallet/cmd/caribou
    GOOS=${OS} GOARCH=${ARCH} go build -trimpath ${TAGS_CARIBOU:+-tags ${TAGS_CARIBOU}} -o  "../caribou-desktop/resources/mac/${CARIBOU_EXE}" -ldflags "${LDFLAGS_CARIBOU:-${LDFLAGS_BASE}}"
    popd

    pushd ../src
    cp caribou-16.png ../resources/mac/caribou-16.png
    popd

    # Bundle XMR shared library if building with XMR support.
    if [[ " ${TAGS_CARIBOU} " == *" xmr "* || "${TAGS_CARIBOU}" == "xmr" ]]; then
      cp "../../../../wallet/asset/xmr/lib/${OS}-${ARCH}/libmonero_wallet2_api_c.dylib" "../resources/mac/"
      if [[ "$(uname)" == "Darwin" ]]; then
        xattr -dr com.apple.quarantine "../resources/mac/libmonero_wallet2_api_c.dylib" 2>/dev/null || true
        if [[ -n "${APPLE_ID}" && -n "${APPLE_PASSWORD}" && -n "${APPLE_TEAM_ID}" ]]; then
          # Developer ID release: `npm run package` re-signs every nested Mach-O
          # (this dylib + the caribou binary) with the Developer ID identity and
          # Hardened Runtime via @electron/osx-sign (see forge.config.js). Ad-hoc
          # signing here would be overwritten and is rejected by notarization, so
          # leave signing to the package step.
          :
        else
          # Unsigned local build: ad-hoc sign so macOS will run the app on this
          # machine (linking an unsigned dylib otherwise fails to load).
          codesign -f -s - "../resources/mac/libmonero_wallet2_api_c.dylib"
          codesign -f -s - "../resources/mac/${CARIBOU_EXE}"
        fi
      fi
    fi

    # Package (not make): electron-forge package signs + notarizes + staples the
    # .app via forge.config.js (osxSign/osxNotarize). The .dmg is then assembled
    # by hdiutil, because maker-dmg's native module no longer builds on modern
    # Node (see forge.config.js). electron uses x64 where Go uses amd64.
    case "${ARCH}" in amd64) ELECTRON_ARCH="x64" ;; *) ELECTRON_ARCH="${ARCH}" ;; esac
    npm run package -- --platform=${OS} --arch=${ELECTRON_ARCH}

    APP="./out/Caribou-${OS}-${ELECTRON_ARCH}/Caribou.app"
    DMG="../installers/caribou-desktop-${OS}-${ARCH}-v${VER}.dmg"

    # Assemble the .dmg with a styled Finder window: the app icon on the left and
    # an /Applications drop target on the right, so opening the dmg shows the usual
    # "drag to install" layout. hdiutil alone makes a bare image with no window
    # layout (maker-dmg used to do this styling); we reproduce it by laying out a
    # writable image with Finder/AppleScript, then compressing to the final
    # read-only ULFO image. Driving Finder needs a GUI login session on the build
    # Mac (as does the signing/notarization this script already does).
    STAGING="$(mktemp -d)"
    cp -R "${APP}" "${STAGING}/Caribou.app"
    ln -s /Applications "${STAGING}/Applications"

    # Unmount any stale volume of this name so the layout below targets ours.
    hdiutil detach "/Volumes/Caribou" >/dev/null 2>&1 || true

    # Writable image, sized to the content plus slack so Finder can write .DS_Store.
    RW_DMG="$(mktemp -u).dmg"
    SIZE_MB=$(( $(du -sm "${STAGING}" | awk '{print $1}') + 50 ))
    hdiutil create -volname "Caribou" -srcfolder "${STAGING}" \
      -fs HFS+ -format UDRW -size "${SIZE_MB}m" -ov "${RW_DMG}"
    rm -rf "${STAGING}"

    hdiutil attach "${RW_DMG}" -readwrite -noverify -noautoopen
    osascript <<'OSA'
tell application "Finder"
  tell disk "Caribou"
    open
    set current view of container window to icon view
    set toolbar visible of container window to false
    set statusbar visible of container window to false
    set the bounds of container window to {200, 120, 720, 460}
    set opts to the icon view options of container window
    set arrangement of opts to not arranged
    set icon size of opts to 100
    set position of item "Caribou.app" of container window to {150, 170}
    set position of item "Applications" of container window to {390, 170}
    update without registering applications
    delay 1
    close
  end tell
end tell
OSA
    sync
    hdiutil detach "/Volumes/Caribou"

    # Compress the laid-out image to the final read-only .dmg.
    rm -f "${DMG}"
    hdiutil convert "${RW_DMG}" -format ULFO -ov -o "${DMG}"
    rm -f "${RW_DMG}"

    # Sign the .dmg, and with credentials also notarize + staple it (the app
    # inside is already notarized; a notarized dmg passes Gatekeeper on download).
    if [[ -n "${APPLE_ID}" && -n "${APPLE_PASSWORD}" && -n "${APPLE_TEAM_ID}" ]]; then
      codesign -s "Developer ID Application" "${DMG}"
      xcrun notarytool submit "${DMG}" --apple-id "${APPLE_ID}" \
        --password "${APPLE_PASSWORD}" --team-id "${APPLE_TEAM_ID}" --wait
      xcrun stapler staple "${DMG}"
    fi

    rm -rf "./out"

  done
}

TARGETS="darwin/amd64 darwin/arm64"
build_targets

rm -rf "../resources"
