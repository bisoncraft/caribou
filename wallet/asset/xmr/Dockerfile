# Dockerfile for building libmonero_wallet2_api_c (Monero's wallet2 C wrapper)
# for Linux, Windows, macOS, Android and iOS.
#
# Pinned to monero_c v0.18.4.6-RC2 (Monero 0.18.4.6 "Fluorine Fermi"). monero_c
# builds via its "simplybs" system, which compiles the full cross toolchain and
# dependencies from source per target, so the host only needs a compiler + git.
#
# Build:
#   docker build -t monero_c_builder .
#
# Extract libraries (output lives at release/<tag>/<triple>/):
#   # Linux x86_64
#   docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/x86_64-linux-gnu/libmonero_wallet2_api_c.so > libmonero_wallet2_api_c.so
#
#   # Linux ARM64
#   docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-linux-gnu/libmonero_wallet2_api_c.so > libmonero_wallet2_api_c.so
#
#   # Windows x86_64
#   docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/x86_64-w64-mingw32/libmonero_wallet2_api_c.dll > libmonero_wallet2_api_c.dll
#
#   # macOS x86_64 / ARM64
#   docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/x86_64-apple-darwin/libmonero_wallet2_api_c.dylib > libmonero_wallet2_api_c.dylib
#   docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-apple-darwin/libmonero_wallet2_api_c.dylib > libmonero_wallet2_api_c.dylib
#
#   # iOS device / simulator
#   docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-apple-ios/libmonero_wallet2_api_c.dylib > libmonero_wallet2_api_c.dylib
#   docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-apple-ios-simulator/libmonero_wallet2_api_c.dylib > libmonero_wallet2_api_c.dylib
#
# Or extract everything at once:
#   docker run --rm monero_c_builder tar -cf - -C /monero_c/release . | tar -xf -

FROM golang:1.26.0-bookworm

ENV DEBIAN_FRONTEND=noninteractive

# simplybs builds its own cmake/make/autoconf/etc. from source; the host only
# needs a compiler toolchain (clang is simplybs's default native CC) plus git.
# git clang llvm gcc g++: the native toolchain simplybs bootstraps from.
# zlib1g-dev/libbz2-dev/liblzma-dev/libxml2-dev/libssl-dev: headers for the
# native `xar` tool simplybs builds to unpack the Apple SDK .xip (Darwin
# targets). Without them the xar build fails ("zlib.h/bzlib.h/lzma.h not
# found") and the build gets stuck at the Apple SDK extraction step.
RUN apt-get update && apt-get install -y \
    git clang llvm gcc g++ \
    zlib1g-dev libbz2-dev liblzma-dev libxml2-dev libssl-dev \
    && rm -rf /var/lib/apt/lists/*

RUN git config --global user.name "caribou" \
    && git config --global user.email "caribou@nowhere" \
    && git config --global --add safe.directory '*'

# Clone monero_c at our pin (== tag v0.18.4.6-RC2), without --recursive so
# submodules match the pinned commit.
RUN git clone https://github.com/MrCyjaneK/monero_c.git /monero_c
WORKDIR /monero_c
RUN git checkout 3bfb3856a838f2bf6b729501837bb0295dedf25d \
    && git submodule update --init --recursive --force

# Apply monero_c's own patches, then our two local patches:
#   0001 adds the C++ Wallet::setAllowMismatchedDaemonVersion API (needed for
#        regtest/fakechain, where the daemon starts at the latest fork).
#   0002 exposes it through the wallet2_api_c C wrapper as
#        MONERO_Wallet_setAllowMismatchedDaemonVersion.
COPY 0001-add-allow_mismatched_daemon_version.patch /tmp/
COPY 0002-add-wallet2_api_c-wrapper.patch /tmp/
RUN ./apply_patches.sh monero \
    && (cd monero && git apply /tmp/0001-add-allow_mismatched_daemon_version.patch) \
    && git apply /tmp/0002-add-wallet2_api_c-wrapper.patch

# macOS only: the .exp symbol-export list gates exported symbols on Darwin.
# Add our new wrapper symbol so it is visible in the dylib (Linux/Windows export
# the library's MONERO_* symbols by default). A sed append after an existing
# entry is deliberate here rather than a patch: it survives upstream reordering
# of the export list, whereas a unified diff would need the neighbours unchanged.
RUN sed -i '/_MONERO_Wallet_setTrustedDaemon/a _MONERO_Wallet_setAllowMismatchedDaemonVersion' \
    monero_libwallet2_api_c/monero_libwallet2_api_c.exp

# Build robustness for simplybs source downloads. simplybs fetches sources via
# ftpmirror.gnu.org, which load-balances to random mirrors that occasionally
# 403/time out, and it aborts on the first 0-byte response instead of using its
# own 15-retry loop. `make simplybs` clones it at SIMPLYBS_HASH; then two patches:
#   0004 limits that early give-up to mirror lookups, so the origin URL keeps
#        retrying through the redirect roulette until it lands on a working mirror;
#   0005 stops the depends Makefile from git-resetting simplybs on every build,
#        which would otherwise revert 0004 before the next target builds.
#   0006 16 KB-aligns the aarch64-linux-android shared lib (Play API 35+).
COPY 0004-simplybs-retry-mirrors-only.patch /tmp/
COPY 0005-depends-keep-simplybs-checkout.patch /tmp/
COPY 0006-simplybs-android-16k-pages.patch /tmp/
RUN cd contrib/depends && make simplybs \
    && git apply /tmp/0004-simplybs-retry-mirrors-only.patch \
    && git apply /tmp/0005-depends-keep-simplybs-checkout.patch \
    && git apply /tmp/0006-simplybs-android-16k-pages.patch

# Build libmonero_wallet2_api_c for each desktop target. Each target builds its
# full cross toolchain + deps from source (long); sbs-cleanup frees the
# intermediate build trees between targets.
RUN ./build_single.sh monero x86_64-linux-gnu   -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero aarch64-linux-gnu  -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero x86_64-w64-mingw32 -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero x86_64-apple-darwin  -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero aarch64-apple-darwin -j$(nproc) && (cd contrib/depends && make sbs-cleanup)

# Mobile: Android arm64-v8a. simplybs supplies its own NDK toolchain (targets
# API 21, links libc++), so this builds in the Debian image with no host NDK.
# The .so is 16 KB-aligned by patch 0006 (Play API 35+). It ships as a jniLib in
# the APK (see wallet/android/bind.sh, CARIBOU_XMR=1).
RUN ./build_single.sh monero aarch64-linux-android -j$(nproc) && (cd contrib/depends && make sbs-cleanup)

# Mobile: iOS device + simulator. These build here, not on a Mac. simplybs'
# native/apple-sdk package is MrCyjaneK's osxcross — a Linux-hosted Apple cross
# toolchain — and every rule in it is scoped `*:<target>:`, where the first
# field is the BUILD machine (contrib/depends/simplybs/utils/ifstring). So the
# builder is unconstrained, and the package maps each Apple target to the SDK it
# unpacks from the same Xcode xip:
#
#     *:*-apple-darwin:SDKNAME=MacOSX
#     *:*-apple-ios:SDKNAME=iPhoneOS
#     *:*-apple-ios-simulator:SDKNAME=iPhoneSimulator
#
# The darwin targets above already exercise that path in this image, xar and
# pbzx included, so iOS differs only in which SDK comes out of the xip.
#
# No install_name fixup here, unlike the darwin dylibs below: the iOS libs are
# wrapped into Monero.framework by wallet/ios/bind.sh, which retargets the id to
# @rpath/Monero.framework/Monero itself.
RUN ./build_single.sh monero aarch64-apple-ios           -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero aarch64-apple-ios-simulator -j$(nproc) && (cd contrib/depends && make sbs-cleanup)

# macOS: simplybs links the dylib with install_name @rpath/libmonero_wallet2_api_c.dylib.
# cgo rejects -Wl,-rpath, so the caribou binary carries no LC_RPATH for @rpath to
# resolve against. Rewrite the id to @executable_path/... so the loader finds
# the dylib next to the executable (matches the runtime layout in lib/README.md).
RUN INT=$(command -v llvm-install-name-tool || ls /usr/bin/llvm-install-name-tool-* | head -1) \
    && for dy in release/*/x86_64-apple-darwin/libmonero_wallet2_api_c.dylib \
                 release/*/aarch64-apple-darwin/libmonero_wallet2_api_c.dylib; do \
        "$INT" --id "@executable_path/libmonero_wallet2_api_c.dylib" "$dy"; \
    done

RUN echo "=== Built libraries ===" && find release -name 'libmonero_wallet2_api_c.*' -print

CMD ["bash"]
