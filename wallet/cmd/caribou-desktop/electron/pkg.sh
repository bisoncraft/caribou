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
pushd ../../../../wallet/appserver/newui
npm ci
npm run build
popd

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

    npm run make --platform=${OS} --arch=${ARCH}

    cp -R "./out/make/Caribou.dmg" "../installers/caribou-desktop-${OS}-${ARCH}-v${VER}.dmg"

    rm -rf "./out"

  done
}

TARGETS="darwin/amd64 darwin/arm64"
build_targets

rm -rf "../resources"
