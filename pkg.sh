#!/usr/bin/env bash

set -e

# For release, remove pre-release info, and set metadata to "release".
VER="1.1.0-rc2" # pre, beta, rc1, etc.
META="release.local" # "release"

export CGO_ENABLED=0
export GOWORK=off

# if META set, append "+${META}", otherwise nothing.
LDFLAGS_BASE="-buildid= -s -w -X main.Version=${VER}${META:++${META}}"

# Build the webpack bundle prior to building the appserver package, which embeds
# the files.
pushd wallet/appserver/newui
npm ci
npm run build
popd

rm -rf bin

build_targets (){
  for TARGET in ${TARGETS}; do
    OS=${TARGET%%/*}
    ARCH=${TARGET##*/}
    echo "Building for ${OS}-${ARCH}"

    mkdir -p "bin/bossw-${OS}-${ARCH}-v${VER}"

    pushd wallet/cmd/bossw
    GOOS=${OS} GOARCH=${ARCH} go build -trimpath ${TAGS_BOSSW:+-tags ${TAGS_BOSSW}} -o "../../../bin/bossw-${OS}-${ARCH}-v${VER}/${BOSSW_EXE}" -ldflags "${LDFLAGS_BOSSW:-${LDFLAGS_BASE}}"
    popd

    pushd wallet/cmd/bwctl
    GOOS=${OS} GOARCH=${ARCH} go build -trimpath -o "../../../bin/bossw-${OS}-${ARCH}-v${VER}" -ldflags "${LDFLAGS_BASE}"
    popd

    pushd bin
    if [[ "$OS" == "windows" ]]; then
      zip -9 -r -q "bossw-${OS}-${ARCH}-v${VER}.zip" "bossw-${OS}-${ARCH}-v${VER}"
    else
      tar -I 'gzip -9' --owner=0 --group=0 -cf "bossw-${OS}-${ARCH}-v${VER}.tar.gz" "bossw-${OS}-${ARCH}-v${VER}"
    fi
    popd
  done
}

# Vanilla builds on all supported os/arch targets
TARGETS="linux/amd64 linux/arm64 linux/riscv64 darwin/amd64 darwin/arm64 freebsd/amd64 openbsd/amd64 openbsd/arm64"
build_targets

# Only Windows gets the systray build
TARGETS="windows/amd64"
TAGS_BOSSW="systray"
BOSSW_EXE="bossw.exe"
LDFLAGS_BOSSW="${LDFLAGS_BASE} -H=windowsgui"
build_targets

echo "Files embedded in the Go appserver package:"
go list -f '{{ .EmbedFiles }}' github.com/bisoncraft/caribou/wallet/appserver
# NOTE: before embedding, we needed to grab: dist, src/font, src/html, src/img.

pushd bin
sha256sum *.gz *.zip *.deb *.msi *.dmg > bossw-v${VER}-manifest.txt
popd
