#!/usr/bin/env bash

set -e

cd "$(dirname "$0")"

# For release, remove pre-release info, and set metadata to "release".
VER="1.1.0-rc2" # pre, beta, rc1, etc.
META="release.local" # "release"
BOSSW_EXE="bossw"

# Parse flags.
while [[ "$#" -gt 0 ]]; do
  case $1 in
    --xmr) TAGS_BOSSW="${TAGS_BOSSW:+${TAGS_BOSSW} }xmr"; export CGO_ENABLED=1 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
  shift
done

export CGO_ENABLED=${CGO_ENABLED:-0}
export GOWORK=off

# if META set, append "+${META}", otherwise nothing.
LDFLAGS_BASE="-buildid= -s -w -X main.Version=${VER}${META:++${META}}"

#Build the webpack bundle prior to building the appserver package, which embeds
# the files.
pushd ../../../../wallet/appserver/newui
npm ci
npm run build
popd

pushd ..
rm -rf resources
mkdir -p "resources/mac"
popd

pushd ../../../../wallet/cmd/bossw
go build -trimpath ${TAGS_BOSSW:+-tags ${TAGS_BOSSW}} -o  "../bossw-desktop/resources/mac/${BOSSW_EXE}" -ldflags "${LDFLAGS_BOSSW:-${LDFLAGS_BASE}}"
popd

pushd ../src
cp bossw-16.png ../resources/mac/bossw-16.png
popd

# Bundle XMR shared library if building with XMR support.
if [[ " ${TAGS_BOSSW} " == *" xmr "* || "${TAGS_BOSSW}" == "xmr" ]]; then
  XMRARCH=$(go env GOARCH)
  cp "../../../../wallet/asset/xmr/lib/darwin-${XMRARCH}/libwallet2_api_c.dylib" "../resources/mac/"
  # macOS requires ad-hoc signing when linking against an unsigned dynamic
  # library, and downloaded files may have quarantine attributes.
  if [[ "$(uname)" == "Darwin" ]]; then
    xattr -dr com.apple.quarantine "../resources/mac/libwallet2_api_c.dylib" 2>/dev/null || true
    codesign -f -s - "../resources/mac/libwallet2_api_c.dylib"
    codesign -f -s - "../resources/mac/${BOSSW_EXE}"
  fi
fi

echo "Preparation complete. Proceed to run npm run start or npm run make."
