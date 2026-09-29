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
# The four Apple targets additionally need an Xcode xip staged in xcode/, which
# the image cannot download for itself. See xcode/README.md. Without it those
# four are skipped and the image still builds and tags, carrying the Linux,
# Windows and Android libraries; only the Apple extraction commands below then
# have nothing to read.
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
#   # Android (each ABI also ships libc++_shared.so, see below)
#   docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-linux-android/libmonero_wallet2_api_c.so > libmonero_wallet2_api_c.so
#   docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-linux-android/libc++_shared.so > libc++_shared.so
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

# Build as a non-root user. This is not hygiene, it is a hard requirement: two
# of the packages simplybs builds refuse to run as root outright. crosstool-NG
# aborts with "You must NOT be root to run crosstool-NG" (native/ct-tools, the
# package that produces every cross toolchain), and GNU coreutils' configure
# aborts with "you should not run configure as root". Neither can be talked out
# of it from the Dockerfile, because simplybs executes build steps with a
# hermetic environment -- pack/build.go starts cmd.Env from nil -- and the
# ct-tools step additionally runs `env -i`. Upstream's own builder/Dockerfile
# does exactly this.
#
# /monero_c is created and handed over here because git clone below runs as
# user and cannot create a directory at /. GOPATH (/go) is already mode 1777 in
# the golang image, so `go run` needs nothing further.
RUN useradd -ms /bin/bash user \
    && mkdir -p /monero_c \
    && chown user:user /monero_c
USER user

# lowSpeedLimit/lowSpeedTime: git ships no transfer timeout, so a clone that
# stalls mid-transfer hangs forever rather than failing -- and simplybs clones
# llvm-project, a 2.2 GiB fetch with plenty of chances to stall. Under 1 KB/s
# for 60s now aborts, which turns a hang into an error the build can report.
RUN git config --global user.name "caribou" \
    && git config --global user.email "caribou@nowhere" \
    && git config --global --add safe.directory '*' \
    && git config --global http.lowSpeedLimit 1000 \
    && git config --global http.lowSpeedTime 60

# Clone monero_c at our pin (== tag v0.18.4.6-RC2), without --recursive so
# submodules match the pinned commit.
RUN git clone https://github.com/MrCyjaneK/monero_c.git /monero_c
WORKDIR /monero_c
RUN git checkout 3bfb3856a838f2bf6b729501837bb0295dedf25d \
    && git submodule update --init --recursive --force

# Apply monero_c's own patches, then our local ones:
#   0001 adds the C++ Wallet::setAllowMismatchedDaemonVersion API (needed for
#        regtest/fakechain, where the daemon starts at the latest fork).
#   0002 exposes it through the wallet2_api_c C wrapper as
#        MONERO_Wallet_setAllowMismatchedDaemonVersion.
#   0010 recovers HOST_ABI from the environment when CMake deletes its own
#        cache mid-build and re-runs configure without the -D that carried it.
#        Intermittent, and fatal to a target: a RUN below has no retry, so the
#        layer and its toolchain work are discarded.
#   0013 links the desktop Linux library with a static libstdc++/libgcc, a
#        version script that exports only MONERO_*, and -z relro -z now. With
#        0014 below it is what lets the library load on distributions older
#        than the toolchain; publish-libs.sh refuses a Linux library without it.
#   0015 adds -static to the Windows link, so winpthread is linked in rather
#        than imported from a libwinpthread-1.dll the release does not ship.
#        publish-libs.sh refuses a Windows library that imports one.
#   0016 makes monero's util.cpp declare strsafe.h's functions rather than
#        define them inline, which 0015 needs: the static winpthread brings
#        libmingwex's own definitions in, and ld rejects the pair.
# Pin the patch commits' dates. apply_patches.sh applies monero_c's patches to
# the monero submodule with `git am` and `git commit`, so the submodule's HEAD is
# a commit this build creates -- and a commit hash covers its committer date, so
# HEAD changed on every build. monero embeds `git rev-parse --short HEAD` in its
# version string, which lands in the binary: the September libraries say
# 0.18.4.6-5fb1e9c7c and a rebuild said 0.18.4.6-fcc0f0614.
#
# That was the ONLY difference between the two. 34,971,831 of 34,971,840 bytes
# matched; the nine that did not were those hex digits. Pinning both dates to the
# monero_c pin's own commit date makes HEAD deterministic (42c66d9c7) and, with
# it, the libraries -- which is what lets `cd lib && sha256sum -c SHA256SUMS`
# actually mean something for a rebuilder. `git am` takes each patch's author
# date from the patch itself, so only the committer date really floats, but both
# are set because the .note.GNU-stack step uses a plain `git commit`.
ENV GIT_COMMITTER_DATE="2026-05-28T12:56:53+02:00" \
    GIT_AUTHOR_DATE="2026-05-28T12:56:53+02:00"
COPY 0001-add-allow_mismatched_daemon_version.patch /tmp/
COPY 0002-add-wallet2_api_c-wrapper.patch /tmp/
COPY 0010-cmake-recover-host-abi.patch /tmp/
COPY 0013-cmake-linux-static-runtime.patch /tmp/
COPY 0015-cmake-mingw-static-winpthread.patch /tmp/
COPY 0016-monero-strsafe-declarations-only.patch /tmp/
RUN ./apply_patches.sh monero \
    && (cd monero && git apply /tmp/0001-add-allow_mismatched_daemon_version.patch) \
    && (cd monero && git apply /tmp/0016-monero-strsafe-declarations-only.patch) \
    && git apply /tmp/0002-add-wallet2_api_c-wrapper.patch \
    && git apply /tmp/0010-cmake-recover-host-abi.patch \
    && git apply /tmp/0013-cmake-linux-static-runtime.patch \
    && git apply /tmp/0015-cmake-mingw-static-winpthread.patch

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
# own 15-retry loop. `make simplybs` clones it at SIMPLYBS_HASH; then these patches:
#   0004 limits that early give-up to mirror lookups, so the origin URL keeps
#        retrying through the redirect roulette until it lands on a working mirror;
#   0005 stops the depends Makefile from git-resetting simplybs on every build,
#        which would otherwise revert 0004 before the next target builds.
#   0007 sends the origin attempt to ftp.gnu.org instead of the redirector,
#        which has host-wide outages that 0004's retries cannot ride out. It
#        matches both schemes (36 of the 41 GNU URLs in packages/ are http://)
#        and restores the /gnu/ path component the redirector supplies, which
#        native/bash and native/cpio do not spell out for themselves.
#   0009 stops sbs-cleanup from deleting the sha256-verified source cache
#        between targets, which made all ten targets re-download everything
#        and would discard the staged Xcode xip after the first target.
#   0011 re-checks a cached archive against its pinned sha256 before reusing
#        it, so the cache 0009 keeps cannot preserve a truncated download
#        from an interrupted run for good.
#   0012 retries the git clone that builds a source bundle. Nothing on that
#        path retried, and a dropped connection on the 2.2 GiB llvm-project
#        clone failed the whole target -- which under `docker build` discards
#        the layer and throws away that target's toolchain build.
# And one that changes what gets built rather than how reliably:
#   0014 builds the two linux-gnu cross toolchains against glibc 2.31 instead
#        of 2.42, so the library needs GLIBC_2.31 at most (Ubuntu 20.04,
#        Debian 11, RHEL 9) rather than 2.38. It also rehashes every linux-gnu
#        target package, which would otherwise be reused from a cache built
#        against the old headers. publish-libs.sh checks the result.
COPY 0004-simplybs-retry-mirrors-only.patch /tmp/
COPY 0005-depends-keep-simplybs-checkout.patch /tmp/
COPY 0007-simplybs-gnu-origin-fallback.patch /tmp/
COPY 0009-depends-keep-simplybs-source-cache.patch /tmp/
COPY 0011-simplybs-recheck-cached-sources.patch /tmp/
COPY 0012-simplybs-retry-git-clone.patch /tmp/
COPY 0014-simplybs-linux-glibc-2.31.patch /tmp/
RUN cd contrib/depends && make simplybs

# Apply them from the repository root with --directory, not from contrib/depends,
# and fail on a skip.
#
# `cd contrib/depends && git apply 0011-...patch` looks like it works and does
# not. git apply treats a patch carrying a `diff --git a/x b/x` header as
# toplevel-relative, so it does not prepend the "contrib/depends/" prefix; the
# path then sits outside the current directory, git drops the patch, prints
# nothing without -v, and EXITS 0. The `&&` chain this replaces therefore ran
# green while 0007 and 0011 -- the only two of the five carrying that header --
# were never applied. 0004, 0005 and 0009 have no `diff --git` line, get the
# prefix prepended, and did apply, which is what made the failure look like
# nothing was wrong.
#
# --directory gives every patch the same prefix regardless of its header, and
# the "Skipped patch" check below is what makes a recurrence loud: git apply has
# no flag that turns a skip into a nonzero exit.
RUN set -eu; \
    for p in 0004-simplybs-retry-mirrors-only \
             0005-depends-keep-simplybs-checkout \
             0007-simplybs-gnu-origin-fallback \
             0009-depends-keep-simplybs-source-cache \
             0011-simplybs-recheck-cached-sources \
             0012-simplybs-retry-git-clone \
             0014-simplybs-linux-glibc-2.31; do \
        out=$(git apply -v --directory=contrib/depends "/tmp/$p.patch" 2>&1) \
            || { echo "$out" >&2; echo "error: $p failed to apply" >&2; exit 1; }; \
        case "$out" in \
            *"Skipped patch"*) \
                echo "$out" >&2; \
                echo "error: git apply SKIPPED $p rather than applying it." >&2; \
                exit 1 ;; \
        esac; \
        echo "applied: $p"; \
    done

# Apple SDK. native/apple-sdk pins the Xcode xip as a "blob" download, but its
# URL is not reachable unattended: Apple 302s to developer.apple.com/unauthorized/
# without a signed-in session, and the simplybs LFS mirror answers 451 for that
# one path. The file therefore has to come from the build context: drop it in
# xcode/ (see xcode/README.md) and the four Apple targets build.
#
# The xip is identified by its sha256, not by its name. Naming is a genuine
# trap here -- Apple serves the file as Xcode_26.1_Universal.xip while the
# pinned URL calls it XcodeXIP_26.1_Universal.xip, and Xcode_26.1.1 sits right
# next to 26.1 in the download list looking almost identical. So take any *.xip
# in xcode/, hash it, and wire up the one that matches.
#
# Wiring it up is a symlink rather than a copy or a rename. Every one of those
# would force overlayfs to copy the 2.8 GB up into a fresh layer to change a
# name; a symlink costs an inode. pack.DownloadSource() only os.Stat()s the
# path before deciding the source is already present, and os.Stat follows
# symlinks, so it never notices the difference.
#
# Which is exactly why the RUN below hashes the file itself. simplybs does NOT
# check a source it did not download: DownloadSource passes the pinned sha256 to
# DownloadFile only on the branch where the file is absent, and ExtractSource's
# "blob" case -- the xip's kind -- is a plain copy with no hash check. So a
# staged xip is verified here or nowhere.
#
# Every xip present is hashed before giving up, so staging both 26.1 and 26.1.1
# works -- the right one is picked whichever way they sort. Only if none of them
# match is it an error, and it is a hard one: someone went to the trouble of
# staging a 2.8 GB file and got the wrong one, so say which and say it now
# rather than failing three hours into the first Apple target. No xip at all is
# a notice, not an error: that is the supported way to build only the Linux,
# Windows and Android targets.
#
# --chown because the Apple targets read the xip as user, through the symlink,
# and COPY otherwise leaves it root-owned with whatever mode it had on the host.
# The chown -R at the end of the RUN cannot fix that: it does not follow
# symlinks, so it changes the link and never touches the file.
USER root
COPY --chown=user:user xcode/ /opt/xcode/
RUN set -eu; \
    want=$(awk -F= '/^XCODE_XIP_SHA256=/ { print $2; exit }' /opt/xcode/PIN); \
    ver=$(awk -F= '/^XCODE_VERSION=/ { print $2; exit }' /opt/xcode/PIN); \
    [ -n "$want" ] && [ -n "$ver" ] || { echo "xcode/PIN does not define XCODE_VERSION and XCODE_XIP_SHA256"; exit 1; }; \
    dest="/monero_c/contrib/depends/simplybs/.buildlib/source/download.developer.apple.com/Developer_Tools/Xcode_$ver"; \
    mkdir -p "$dest"; \
    find /opt/xcode -type f -iname '*.xip' | sort > /tmp/xips; \
    found=; rejected=; \
    while IFS= read -r f; do \
        got=$(sha256sum "$f" | cut -d' ' -f1); \
        if [ "$got" = "$want" ]; then found=$f; break; fi; \
        rejected="$rejected  $got  $f\n"; \
    done < /tmp/xips; \
    rm -f /tmp/xips; \
    if [ -n "$found" ]; then \
        ln -s "$found" "$dest/XcodeXIP_${ver}_Universal.xip"; \
        touch /monero_c/.apple-sdk-staged; \
        echo "Apple SDK: using $found"; \
    elif [ -n "$rejected" ]; then \
        echo "Apple SDK: none of the xips in xcode/ match the pinned sha256."; \
        echo "  wanted $want  (Xcode $ver)"; \
        printf "%b" "$rejected"; \
        echo "  Xcode 26.1.1 hashes ed55d55f... and is NOT interchangeable."; \
        exit 1; \
    else \
        rm -f /monero_c/.apple-sdk-staged; \
        echo "Apple SDK: no xip in xcode/, so the four Apple targets are skipped."; \
        echo "           Linux, Windows and Android are unaffected. See xcode/README.md."; \
    fi; \
    chown -R user:user /monero_c/contrib/depends/simplybs/.buildlib
USER user

# Build libmonero_wallet2_api_c for each target. Each target builds its full
# cross toolchain + deps from source (long); sbs-cleanup frees the intermediate
# build trees between targets, keeping the source cache (0009).
#
# THE APPLE TARGETS COME LAST, AND THEY SKIP THEMSELVES WITHOUT THE XIP. They are
# the only four that need it, and each is guarded on the .apple-sdk-staged marker
# written above. Ordering alone is not enough: this is one stage, so a failing
# Apple RUN ends the build and `docker build -t monero_c_builder .` then tags no
# image at all -- which would leave the Linux, Windows and Android libraries
# built but unreachable, since every extraction command is a `docker run` against
# that tag. Skipping instead of failing is what makes the "Linux, Windows and
# Android are unaffected" notice above true rather than aspirational. Keep the
# Apple targets last anyway, so a genuine failure in one of them still leaves the
# other six cached.
RUN ./build_single.sh monero x86_64-linux-gnu   -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero aarch64-linux-gnu  -j$(nproc) && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero x86_64-w64-mingw32 -j$(nproc) && (cd contrib/depends && make sbs-cleanup)

# Mobile: Android. simplybs supplies its own NDK toolchain (targets API 21,
# links libc++), so these build in the Debian image with no host NDK. They ship
# as jniLibs in the APK (see wallet/android/bind.sh, CARIBOU_XMR=1).
#
# arm64-v8a is the device ABI and the only one bind.sh stages today. x86_64 is
# the emulator, which is what bind.sh's own comment points at when it says to
# add ABIs to -target, and building it here is what makes that instruction
# work. armeabi-v7a is 32-bit, built for completeness on old devices.
#
# All three come out with 16 KB-aligned LOAD segments (Play API 35+) without
# anything from us: monero_libwallet2_api_c/CMakeLists.txt already passes
# -Wl,-z,max-page-size=16384 for every `-linux-` HOST_ABI, and NDK r28c
# defaults to it for the 64-bit ABIs on top of that. Verified on all three.
#
# Each ABI also ships libc++_shared.so. monero_c links libc++ dynamically, so
# the .so alone does not load: wallet/android/bind.sh stages this file into
# jniLibs beside it, and it has to be the copy from the NDK that built the
# library -- a local NDK's prebuilt may be a different libc++ ABI, and before
# r27 it is 4 KB-aligned, which Android 15+ and Play API 35+ reject. Copy it
# into release/ in the same RUN as the build: sbs-cleanup removes
# contrib/depends/_native, so by the next line it is gone. armv7a's sysroot
# directory is spelled arm-linux-androideabi, unlike its target triple.
RUN ./build_single.sh monero aarch64-linux-android    -j$(nproc) \
    && cp contrib/depends/_native/sysroot/usr/lib/aarch64-linux-android/libc++_shared.so release/*/aarch64-linux-android/ \
    && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero x86_64-linux-android     -j$(nproc) \
    && cp contrib/depends/_native/sysroot/usr/lib/x86_64-linux-android/libc++_shared.so release/*/x86_64-linux-android/ \
    && (cd contrib/depends && make sbs-cleanup)
RUN ./build_single.sh monero armv7a-linux-androideabi -j$(nproc) \
    && cp contrib/depends/_native/sysroot/usr/lib/arm-linux-androideabi/libc++_shared.so release/*/armv7a-linux-androideabi/ \
    && (cd contrib/depends && make sbs-cleanup)

# Check the 16 KB alignment claimed above rather than trust it. Nothing else
# does before Play ingestion, and a monero_c pin bump could quietly drop the
# CMakeLists.txt flag. The wallet lib is checked on all three ABIs, since that
# flag covers all of them; libc++_shared.so only on the 64-bit two, which are
# all Play requires and all the NDK default covers. Its own RUN so a failure
# here keeps the three builds above cached.
RUN set -eu; \
    for f in release/*/aarch64-linux-android/libmonero_wallet2_api_c.so \
             release/*/aarch64-linux-android/libc++_shared.so \
             release/*/x86_64-linux-android/libmonero_wallet2_api_c.so \
             release/*/x86_64-linux-android/libc++_shared.so \
             release/*/armv7a-linux-androideabi/libmonero_wallet2_api_c.so; do \
        [ -f "$f" ] || { echo "16 KB check: $f not found"; exit 1; }; \
        aligns=$(readelf -lW "$f" | awk '$1 == "LOAD" { print $NF }'); \
        [ -n "$aligns" ] || { echo "16 KB check: no LOAD segments read from $f"; exit 1; }; \
        for a in $aligns; do \
            [ $((a)) -ge 16384 ] || { echo "16 KB check: $f has a LOAD segment aligned to $a, need >= 0x4000"; exit 1; }; \
        done; \
        echo "16 KB check: ok  $f"; \
    done

# Desktop: macOS. Here rather than with the other desktop targets above because
# it needs the xip -- see the ordering note there.
RUN if [ -e /monero_c/.apple-sdk-staged ]; then \
        ./build_single.sh monero x86_64-apple-darwin -j$(nproc) && (cd contrib/depends && make sbs-cleanup); \
    else echo "SKIP x86_64-apple-darwin: no Apple SDK xip staged, see xcode/README.md"; fi
RUN if [ -e /monero_c/.apple-sdk-staged ]; then \
        ./build_single.sh monero aarch64-apple-darwin -j$(nproc) && (cd contrib/depends && make sbs-cleanup); \
    else echo "SKIP aarch64-apple-darwin: no Apple SDK xip staged, see xcode/README.md"; fi

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
# pbzx included, so iOS differs only in which SDK comes out of the xip -- the
# same staged xip, which is why 0009 has to keep the source cache alive across
# all four Apple targets rather than let sbs-cleanup delete it after the first.
#
# No install_name fixup here, unlike the darwin dylibs below: the iOS libs are
# wrapped into Monero.framework by wallet/ios/bind.sh, which retargets the id to
# @rpath/Monero.framework/Monero itself.
RUN if [ -e /monero_c/.apple-sdk-staged ]; then \
        ./build_single.sh monero aarch64-apple-ios -j$(nproc) && (cd contrib/depends && make sbs-cleanup); \
    else echo "SKIP aarch64-apple-ios: no Apple SDK xip staged, see xcode/README.md"; fi
RUN if [ -e /monero_c/.apple-sdk-staged ]; then \
        ./build_single.sh monero aarch64-apple-ios-simulator -j$(nproc) && (cd contrib/depends && make sbs-cleanup); \
    else echo "SKIP aarch64-apple-ios-simulator: no Apple SDK xip staged, see xcode/README.md"; fi

# macOS: simplybs links the dylib with install_name @rpath/libmonero_wallet2_api_c.dylib.
# cgo rejects -Wl,-rpath, so the caribou binary carries no LC_RPATH for @rpath to
# resolve against. Rewrite the id to @executable_path/... so the loader finds
# the dylib next to the executable (matches the runtime layout in lib/README.md).
RUN if [ -e /monero_c/.apple-sdk-staged ]; then \
        INT=$(command -v llvm-install-name-tool || ls /usr/bin/llvm-install-name-tool-* | head -1); \
        for dy in release/*/x86_64-apple-darwin/libmonero_wallet2_api_c.dylib \
                  release/*/aarch64-apple-darwin/libmonero_wallet2_api_c.dylib; do \
            "$INT" --id "@executable_path/libmonero_wallet2_api_c.dylib" "$dy"; \
        done; \
    else echo "SKIP install_name fixup: no macOS dylibs were built"; fi

RUN echo "=== Built libraries ===" \
    && find release \( -name 'libmonero_wallet2_api_c.*' -o -name 'libc++_shared.so' \) -print

CMD ["bash"]
