# Dockerfile for building libmonero_wallet2_api_c (Monero's wallet2 C wrapper)
# for Linux, Windows, and macOS.
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
# Or extract everything at once:
#   docker run --rm monero_c_builder tar -cf - -C /monero_c/release . | tar -xf -

FROM golang:1.26.0-bookworm

ENV DEBIAN_FRONTEND=noninteractive

# simplybs builds its own cmake/make/autoconf/etc. from source; the host only
# needs a compiler toolchain (clang is simplybs's default native CC) plus git.
RUN apt-get update && apt-get install -y \
    git clang llvm gcc g++ \
    && rm -rf /var/lib/apt/lists/*

RUN git config --global user.name "bossw" \
    && git config --global user.email "bossw@nowhere" \
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
# the library's MONERO_* symbols by default).
RUN sed -i '/_MONERO_Wallet_setTrustedDaemon/a _MONERO_Wallet_setAllowMismatchedDaemonVersion' \
    monero_libwallet2_api_c/monero_libwallet2_api_c.exp

# Build robustness for simplybs source downloads. simplybs fetches sources via
# ftpmirror.gnu.org, which load-balances to random mirrors that occasionally
# 403/time out, and it aborts on the first 0-byte response instead of using its
# own 15-retry loop. Two fixes:
#   1) limit that early give-up to mirror lookups, so the origin URL keeps
#      retrying through the redirect roulette until it lands on a working mirror;
#   2) stop the depends Makefile from git-resetting simplybs on every build,
#      which would otherwise revert fix (1).
RUN cd contrib/depends && make simplybs \
    && sed -i 's/bytesReceived == 0 && attempt == 0 {/bytesReceived == 0 \&\& attempt == 0 \&\& isMirror {/' \
        simplybs/utils/download/download.go \
    && sed -i 's|^\(\tcd simplybs && git fetch\)|#\1|; s|^\(\tcd simplybs && git checkout\)|#\1|; s|^\(\tcd simplybs && git reset\)|#\1|' \
        Makefile

# Build libmonero_wallet2_api_c for each desktop target. Each target builds its
# full cross toolchain + deps from source (long); sbs-cleanup frees the
# intermediate build trees between targets.
RUN ./build_single.sh monero x86_64-linux-gnu   -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero aarch64-linux-gnu  -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero x86_64-w64-mingw32 -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero x86_64-apple-darwin  -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero aarch64-apple-darwin -j$(nproc) && (cd contrib/depends && make sbs-cleanup)

# macOS: simplybs links the dylib with install_name @rpath/libmonero_wallet2_api_c.dylib.
# cgo rejects -Wl,-rpath, so the bossw binary carries no LC_RPATH for @rpath to
# resolve against. Rewrite the id to @executable_path/... so the loader finds
# the dylib next to the executable (matches the runtime layout in lib/README.md).
RUN INT=$(command -v llvm-install-name-tool || ls /usr/bin/llvm-install-name-tool-* | head -1) \
    && for dy in release/*/x86_64-apple-darwin/libmonero_wallet2_api_c.dylib \
                 release/*/aarch64-apple-darwin/libmonero_wallet2_api_c.dylib; do \
        "$INT" --id "@executable_path/libmonero_wallet2_api_c.dylib" "$dy"; \
    done

RUN echo "=== Built libraries ===" && find release -name 'libmonero_wallet2_api_c.*' -print

CMD ["bash"]
