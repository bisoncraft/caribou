# syntax=docker/dockerfile:1
# caribou-builder: one image that cross-compiles, signs and packages the
# desktop and command-line releases for Linux, Windows and macOS from a Linux
# host. Built by release/common.sh (build_image) with wallet/asset/xmr/xcode/
# as the build context: that directory is where a staged Xcode xip is picked
# up for the macOS toolchain, and nothing else from the tree is needed at
# build time because the release scripts are bind-mounted at run time.
#
# Ubuntu 22.04 is the base so that the Linux binary links against glibc 2.35
# and WebKitGTK 2.x from that release, which is the oldest distro the .deb
# targets. Every download below is pinned by version and checksum; Go modules
# and npm packages are verified by their own lockfiles and sumdb.
#
# The Windows installer is built with NSIS (a distro package): makensis runs
# natively on Linux, and its Modern UI pages carry the installer options. The
# WiX Toolset declares its behavior undefined off Windows, and msitools' wixl,
# which does run here, has no support for installer dialogs.

ARG UBUNTU=22.04

########## base: distro packages shared by every stage ##########
FROM ubuntu:${UBUNTU} AS base
ENV DEBIAN_FRONTEND=noninteractive

# A current clang from apt.llvm.org. Ubuntu 22.04's clang 14 cannot parse the
# macOS 26 SDK headers (libc++ and even sys/_types), so every cgo build for
# darwin would fail; osxcross's own compiler test catches it at image build
# time. The repository key is verified against its fingerprint before use.
ARG LLVM_VERSION=20
ARG LLVM_APT_FINGERPRINT=6084F3CF814B57C1CF12EFD515CF4D18AF4F7421
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
    && curl -fsSL https://apt.llvm.org/llvm-snapshot.gpg.key -o /tmp/llvm.key \
    && fpr=$(gpg --batch --with-colons --show-keys /tmp/llvm.key | awk -F: '/^fpr/ { print $10; exit }') \
    && [ "$fpr" = "${LLVM_APT_FINGERPRINT}" ] || { echo "LLVM apt key fingerprint mismatch: $fpr"; exit 1; } \
    && gpg --batch --dearmor -o /usr/share/keyrings/llvm.gpg /tmp/llvm.key && rm /tmp/llvm.key \
    && codename=$(. /etc/os-release && echo "$VERSION_CODENAME") \
    && echo "deb [signed-by=/usr/share/keyrings/llvm.gpg] http://apt.llvm.org/${codename}/ llvm-toolchain-${codename}-${LLVM_VERSION} main" \
        >/etc/apt/sources.list.d/llvm.list \
    && rm -rf /var/lib/apt/lists/*

RUN apt-get update && apt-get install -y --no-install-recommends \
    git patch xz-utils bzip2 zip unzip jq cpio \
    build-essential pkg-config cmake python3 \
    clang-${LLVM_VERSION} lld-${LLVM_VERSION} llvm-${LLVM_VERSION} llvm-${LLVM_VERSION}-dev \
    libxml2-dev uuid-dev libssl-dev zlib1g-dev liblzma-dev libbz2-dev \
    libgtk-3-dev libwebkit2gtk-4.1-dev libx11-dev \
    gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64 mingw-w64-tools \
    genisoimage osslsigncode default-jre-headless dpkg-dev nsis \
    && rm -rf /var/lib/apt/lists/* \
    && for t in clang clang++ llvm-config lld ld.lld llvm-ar llvm-nm llvm-objdump; do \
        update-alternatives --install /usr/bin/$t $t /usr/bin/$t-${LLVM_VERSION} 100; done \
    && clang --version | head -1

########## osxcross: macOS SDK and toolchain, built only when a xip is staged ##########
FROM base AS osxcross
ARG OSXCROSS_COMMIT=27d21e4977c9751d01199c7a226a6faf494c3dd9
# Deployment target for everything the toolchain builds. Electron 44 needs
# macOS 13, so the Go binary is held to the same floor.
ARG OSX_VERSION_MIN=13.0
RUN git clone https://github.com/tpoechtrager/osxcross.git /opt/osxcross-src \
    && git -C /opt/osxcross-src checkout ${OSXCROSS_COMMIT}
# The xip is identified by the sha256 in xcode/PIN, never by name: Apple's
# download list has near-identical names for different SDKs. No xip is a
# notice, not an error, so the image still builds for Linux and Windows.
COPY . /opt/xcode/
RUN set -eu; \
    want=$(awk -F= '/^XCODE_XIP_SHA256=/ { print $2; exit }' /opt/xcode/PIN); \
    found=; \
    for f in $(find /opt/xcode -type f -iname '*.xip' | sort); do \
        if [ "$(sha256sum "$f" | cut -d' ' -f1)" = "$want" ]; then found=$f; break; fi; \
        echo "Apple SDK: $f does not match the pinned sha256, ignoring"; \
    done; \
    mkdir -p /opt/osxcross; \
    if [ -z "$found" ]; then \
        echo "Apple SDK: no matching xip in the build context; the image will have no macOS toolchain"; \
        touch /opt/osxcross/.no-sdk; \
        rm -rf /opt/osxcross-src /opt/xcode; \
        exit 0; \
    fi; \
    echo "Apple SDK: using $found"; \
    ver=$(awk -F= '/^XCODE_VERSION=/ { print $2; exit }' /opt/xcode/PIN); \
    cd /opt/osxcross-src; \
    ./tools/gen_sdk_package_pbzx.sh "$found"; \
    mv MacOSX*.sdk.tar.xz tarballs/; \
    # Xcode ships the SDK under both its full and major version (26.1 and 26); \
    # osxcross needs exactly one, or SDK_VERSION named explicitly. Keep the \
    # one matching the pinned Xcode version. \
    find tarballs -name 'MacOSX*.sdk.tar.xz' ! -name "MacOSX${ver}.sdk.tar.xz" -delete; \
    ls tarballs; \
    UNATTENDED=1 BUILD_FLAVOR=stable TARGET_DIR=/opt/osxcross SDK_VERSION="$ver" \
        OSX_VERSION_MIN=${OSX_VERSION_MIN} ENABLE_ARCHS="arm64 x86_64" ./build.sh; \
    rm -rf /opt/osxcross-src /opt/xcode

########## tools: Go, Node, signing tools, Windows and dmg helpers ##########
FROM base AS tools

ARG GO_VERSION=1.26.8
ARG GO_SHA256=d0f743b33e8d8945e6b1f432edd15785c70507121d6e2a723b21285eddf8b57b
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz \
    && echo "${GO_SHA256}  /tmp/go.tgz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
# GOTOOLCHAIN=local: a go.mod asking for a newer Go fails here instead of
# silently downloading a toolchain this file does not pin.
ENV PATH=/usr/local/go/bin:/usr/local/gobin:${PATH} GOTOOLCHAIN=local GOBIN=/usr/local/gobin

# go-winres stamps icon and version resources into the Windows binary.
ARG GO_WINRES_VERSION=v0.3.3
RUN go install github.com/tc-hib/go-winres@${GO_WINRES_VERSION} && rm -rf /root/go /root/.cache

ARG NODE_VERSION=22.23.3
ARG NODE_SHA256=df450af89261115ef9f9e3830c3eeb2cc9213b63c720b1af623cb5dcbe2e02de
RUN curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" -o /tmp/node.txz \
    && echo "${NODE_SHA256}  /tmp/node.txz" | sha256sum -c - \
    && tar -C /usr/local --strip-components=1 -xJf /tmp/node.txz && rm /tmp/node.txz

# rcodesign: Apple code signing, notarization and stapling without a Mac.
ARG RCODESIGN_VERSION=0.29.0
ARG RCODESIGN_SHA256=dbe85cedd8ee4217b64e9a0e4c2aef92ab8bcaaa41f20bde99781ff02e600002
RUN curl -fsSL "https://github.com/indygreg/apple-platform-rs/releases/download/apple-codesign%2F${RCODESIGN_VERSION}/apple-codesign-${RCODESIGN_VERSION}-x86_64-unknown-linux-musl.tar.gz" -o /tmp/rc.tgz \
    && echo "${RCODESIGN_SHA256}  /tmp/rc.tgz" | sha256sum -c - \
    && tar -xzf /tmp/rc.tgz -C /tmp \
    && install -m755 /tmp/apple-codesign-${RCODESIGN_VERSION}-x86_64-unknown-linux-musl/rcodesign /usr/local/bin/rcodesign \
    && rm -rf /tmp/rc.tgz /tmp/apple-codesign-*

# jsign: Authenticode signing of the .exe, the DLLs and the installer, with
# Azure Trusted Signing and PKCS#12 keystores among its backends.
ARG JSIGN_VERSION=7.5
ARG JSIGN_SHA256=602a51c3545a6dc4fb99bd2ea7152b26d1345916d0c93ddfbd5936cb735af91c
RUN curl -fsSL "https://github.com/ebourg/jsign/releases/download/${JSIGN_VERSION}/jsign-${JSIGN_VERSION}.jar" -o /usr/local/lib/jsign.jar \
    && echo "${JSIGN_SHA256}  /usr/local/lib/jsign.jar" | sha256sum -c - \
    && printf '#!/bin/sh\nexec java -jar /usr/local/lib/jsign.jar "$@"\n' >/usr/local/bin/jsign \
    && chmod 755 /usr/local/bin/jsign

# WebView2Loader.dll ships next to the Windows binary. The headers are not
# needed: webview_go bundles its own copy of the WebView2 SDK headers.
ARG WEBVIEW2_VERSION=1.0.4191.47
ARG WEBVIEW2_SHA256=f492bbf547d0da329553b6727435b677579b1e9f91cc9e4a1ad029366d5f23d0
RUN curl -fsSL "https://www.nuget.org/api/v2/package/Microsoft.Web.WebView2/${WEBVIEW2_VERSION}" -o /tmp/wv2.nupkg \
    && echo "${WEBVIEW2_SHA256}  /tmp/wv2.nupkg" | sha256sum -c - \
    && mkdir -p /opt/webview2 && unzip -q /tmp/wv2.nupkg -d /opt/webview2 && rm /tmp/wv2.nupkg
# webview_go's bundled WebView2.h includes this Windows SDK header, which
# MinGW does not provide; the one-line stub is all it needs.
RUN mkdir -p /opt/winsdk \
    && printf '#pragma once\ntypedef struct EventRegistrationToken { __int64 value; } EventRegistrationToken;\n' >/opt/winsdk/EventToken.h

# libdmg-hfsplus `dmg`: compresses a genisoimage ISO 9660 image (Rock Ridge
# plus Apple extensions) into a UDIF .dmg. The fork is the one Bitcoin Core
# used for its Linux-built dmgs.
ARG LIBDMG_COMMIT=1cc791e4173da9cb0b0cc16c5a1aaa25d5eb5efa
RUN git clone https://github.com/fanquake/libdmg-hfsplus.git /tmp/libdmg \
    && git -C /tmp/libdmg checkout ${LIBDMG_COMMIT} \
    && cmake -S /tmp/libdmg -B /tmp/libdmg/build >/dev/null \
    && make -C /tmp/libdmg/build/dmg -j"$(nproc)" >/dev/null \
    && install -m755 /tmp/libdmg/build/dmg/dmg /usr/local/bin/dmg \
    && rm -rf /tmp/libdmg

# 7-Zip unpacks the finished Windows installer so the build can check that
# every executable file it carries is signed. Installed in its own late layer
# so adding it does not rebuild the osxcross stage.
RUN apt-get update && apt-get install -y --no-install-recommends p7zip-full \
    && rm -rf /var/lib/apt/lists/*

# The linux/arm64 CLI build (release/package-cli.sh) links the arm64 XMR
# library with cgo, so it needs a cross compiler and the target's libc. Also
# in a late layer, for the same reason as 7-Zip above.
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc-aarch64-linux-gnu g++-aarch64-linux-gnu \
    && rm -rf /var/lib/apt/lists/*

# ds_store writes the Finder layout (.DS_Store) of the macOS dmg and mac_alias
# the alias record that points Finder at its background image; both are pure
# Python. They go in a venv at /opt/dmgtools, since Ubuntu 24.04 refuses pip
# installs into the system Python (PEP 668). Pinned by hash: pip installs the
# wheel, the sdist hash is listed in case a wheel is ever unavailable.
RUN apt-get update && apt-get install -y --no-install-recommends python3-venv \
    && rm -rf /var/lib/apt/lists/* \
    && python3 -m venv /opt/dmgtools \
    && printf '%s\n' \
        'mac_alias==2.2.3 --hash=sha256:7362b521d2132ef92f606a37abfed5fcd849ceb2f28b6f9743e014b02af92f0d --hash=sha256:1c7fa367687d66979f2ce4d1a8b2716cf1c9fb811741cab3cf3ca356555c2beb' \
        'ds_store==1.3.3 --hash=sha256:b92a371efbf1b4ccce2a04d1ed13fceacc4736c81ba09cf5aefb74c088160a35 --hash=sha256:a7380896ea1e880f7ba07c5cdc24bd1530fb86836cad0be531f3e80e2d721ac5' \
        >/tmp/dmg-requirements.txt \
    && /opt/dmgtools/bin/pip install --no-cache-dir --no-deps --require-hashes -r /tmp/dmg-requirements.txt \
    && rm /tmp/dmg-requirements.txt \
    && /opt/dmgtools/bin/python -c 'import ds_store, mac_alias'

########## final ##########
FROM tools
COPY --from=osxcross /opt/osxcross /opt/osxcross
ENV PATH=/opt/osxcross/bin:${PATH} LD_LIBRARY_PATH=/opt/osxcross/lib
WORKDIR /work
