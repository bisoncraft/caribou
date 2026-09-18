# monero_c Libraries

This directory holds `libmonero_wallet2_api_c` libraries for each supported
platform. The libraries themselves are gitignored: they are distributed as
release assets, or built from source with the Dockerfile in the parent
directory (see below).

## Getting the libraries

```bash
cd wallet/asset/xmr
./fetch-libs.sh                     # this host's platform
./fetch-libs.sh ios-arm64           # or name platforms explicitly
./fetch-libs.sh --all
```

**Nothing is published yet, so this errors today.** `fetch-libs.sh` takes the
release tag from the `# Release:` line of `SHA256SUMS`, and that line is written
by the first `../publish-libs.sh` run rather than by hand, so until then the
script stops and says so instead of downloading from a release with no assets.
Build from source (below) in the meantime.

`fetch-libs.sh` downloads from this repo's `xmr-libs-*` releases and verifies
every file against `SHA256SUMS` in this directory before installing it. Which
files a platform ships is read out of `SHA256SUMS`, so a platform can carry more
than one: the Android ABIs install `libc++_shared.so` alongside the wallet
library, and neither is installed unless both verify.

Those checksums, not the release, are what you are trusting. Release assets are
mutable — anyone with write access can replace one at the same URL and nothing
in git changes — so the hashes live in git, where a change to them is a diff on
a commit that someone reviewed. Nothing that fails the check is installed.

These are **not** stock monero_c builds: the patches in the parent directory add
`MONERO_Wallet_setAllowMismatchedDaemonVersion`, which `cxmr` declares `extern`
and links against, so an upstream MrCyjaneK release will not satisfy the link.

## What these libraries are, and are not, verified by

They are built on a developer machine and uploaded with `../publish-libs.sh`.
Building them in CI is not practical: each target compiles its whole cross
toolchain from source, and ten of those do not fit in a six-hour job. So there
is **no build provenance** — nothing attests to which machine produced them.

What `SHA256SUMS` gives you is integrity, not provenance. It pins exactly which
binary this repo expects and makes any later swap of a release asset fail
loudly. It says nothing about what the machine that built it was doing.

The check that would say something is rebuilding from the pinned source:

```bash
# after building (see below)
cd lib && sha256sum -c SHA256SUMS      # shasum -a 256 -c on macOS
```

A match means the published bytes are exactly what that source produces, which
is a stronger claim than provenance — anyone can make it, independently, without
trusting the publisher at all.

**It holds, if you build the way the Dockerfile does.** Checked 2026-09-17 by
rebuilding `aarch64-linux-android` from an unchanged tree: 215 objects
recompiled, the library relinked, and the result was byte-identical to the
first build. Two things that normally defeat this are already handled — monero
zeroes its own build timestamp, so every library here says
`built on: Jan  1 1970 00:00:01` rather than a wall-clock date, and the
Dockerfile always builds at `/monero_c`, so the 780-2400 build paths each
library embeds are the same for everyone.

**Reproducibility is therefore path-dependent, and that has one sharp edge.**
`../build-darwin.sh` and `../build-ios.sh` build in `.monero_c-darwin` /
`.monero_c-ios` next to the scripts, not at `/monero_c`. Those paths are
compiled into the output, so a macOS-built darwin or iOS library **cannot**
byte-match a Docker-built one even from identical source. The libraries
published now were all built in a single Docker run, so they are mutually
consistent; if you rebuild the Apple targets on a Mac, expect a mismatch and do
not read it as tampering. Rebuild via the Dockerfile to compare against
`SHA256SUMS`.

Getting here needed a fix. `apply_patches.sh` commits monero_c's patches into
the monero submodule, a git commit hash covers its committer date, and monero
compiles `git rev-parse --short HEAD` into its version string — so every build
produced a different library. Three builds of the same source gave
`0.18.4.6-5fb1e9c7c`, `0.18.4.6-fcc0f0614` and `0.18.4.6-42c66d9c7`. The
Dockerfile now pins `GIT_COMMITTER_DATE`/`GIT_AUTHOR_DATE` to the monero_c pin's
own commit date, which fixes the submodule HEAD and with it the output. Builds
from before that fix will not match, and cannot be made to.

**What is still unchecked:** only one of the ten targets has been built twice,
and that rebuild reused the cached toolchain and dependencies, so it establishes
that monero's own compile and link are deterministic rather than that the whole
toolchain is. Nobody has reproduced these bytes on a second machine.

If you do rebuild, please report the result either way — an independent
reproduction, especially on another machine, is worth more here than any amount
of tooling.

## Platforms

| Directory | Platform | Library |
|-----------|----------|---------|
| `linux-amd64/` | Linux x86_64 | `libmonero_wallet2_api_c.so` |
| `linux-arm64/` | Linux ARM64 | `libmonero_wallet2_api_c.so` |
| `windows-amd64/` | Windows x86_64 | `libmonero_wallet2_api_c.dll` |
| `darwin-amd64/` | macOS x86_64 | `libmonero_wallet2_api_c.dylib` |
| `darwin-arm64/` | macOS ARM64 | `libmonero_wallet2_api_c.dylib` |
| `android-arm64/` | Android arm64-v8a | `libmonero_wallet2_api_c.so` |
| `android-amd64/` | Android x86_64 (emulator) | `libmonero_wallet2_api_c.so` |
| `android-arm/` | Android armeabi-v7a | `libmonero_wallet2_api_c.so` |
| `ios-arm64/` | iOS device | `libmonero_wallet2_api_c.dylib` |
| `ios-arm64-simulator/` | iOS simulator | `libmonero_wallet2_api_c.dylib` |

The Android directories hold a second file, `libc++_shared.so`. monero_c links
libc++ dynamically, so the wallet library does not load without it, and it has
to be the copy from the NDK that built the library: a local NDK's prebuilt may
be a different libc++ ABI, and before r27 it is 4 KB-aligned, which Android 15+
and Play API 35+ reject. `wallet/android/bind.sh` stages the co-located copy
into `jniLibs` when it is there and falls back to the local NDK with a warning
when it is not, so a fetched or extracted Android platform is only complete with
both files.

The desktop libraries are built with the Dockerfile (below). The two macOS
desktop `.dylib`s (`darwin-amd64/`, `darwin-arm64/`) can instead be built
natively on a Mac with `../build-darwin.sh`, which is faster there. Mobile
libraries are built by the same Dockerfile but packaged differently: Android
`.so`s come from its three `*-linux-android*` targets and ship as `jniLibs`
(see `wallet/android/bind.sh`); iOS `.dylib`s come from its `aarch64-apple-ios`
and `aarch64-apple-ios-simulator` targets and are packaged into
`Monero.xcframework` by `wallet/ios/bind.sh` with `CARIBOU_XMR=1`.

The Apple targets, iOS included, cross-compile from Linux: simplybs' Apple SDK
package is osxcross, and it unpacks whichever SDK the target names out of the
one Xcode xip.

**That xip has to be supplied, and no build path avoids it.** simplybs pins
`Xcode_26.1_Universal.xip` by sha256 — a pin it enforces only on its own
download, so whichever path you take is the one that checks the file you staged;
both do. Nothing can fetch it unattended:
Apple answers the pinned URL with a 302 to a developer login, and the simplybs
mirror returns 451 for that one path. Put it in `../xcode/` for the Dockerfile,
or pass `XCODE_XIP=` to `../build-darwin.sh` / `../build-ios.sh`. See
[../xcode/README.md](../xcode/README.md).

Building on a Mac does not get you out of it. simplybs does not use the Mac's
installed Xcode — every rule in `native/apple-sdk` is scoped `*:*-apple-*:`,
where the leading `*` is the build machine, so the package builds osxcross from
the xip whatever you are running on. A Mac only saves compiling `xar` and
`pbzx` from source, since the system ones work. `../build-darwin.sh` says the
same thing in its header.

Mobile libs are embedded in the app package rather than copied next to an
executable, so the runtime notes below apply to the desktop builds only.

## Building the Libraries

From `wallet/asset/xmr/`:

```bash
# Build the Docker image
docker build -t monero_c_builder .

# Extract libraries (monero_c emits release/<tag>/<triple>/, uncompressed)
docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/x86_64-linux-gnu/libmonero_wallet2_api_c.so > lib/linux-amd64/libmonero_wallet2_api_c.so

docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-linux-gnu/libmonero_wallet2_api_c.so > lib/linux-arm64/libmonero_wallet2_api_c.so

docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/x86_64-w64-mingw32/libmonero_wallet2_api_c.dll > lib/windows-amd64/libmonero_wallet2_api_c.dll

docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/x86_64-apple-darwin/libmonero_wallet2_api_c.dylib > lib/darwin-amd64/libmonero_wallet2_api_c.dylib

docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-apple-darwin/libmonero_wallet2_api_c.dylib > lib/darwin-arm64/libmonero_wallet2_api_c.dylib

docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-linux-android/libmonero_wallet2_api_c.so > lib/android-arm64/libmonero_wallet2_api_c.so
docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-linux-android/libc++_shared.so > lib/android-arm64/libc++_shared.so

docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/x86_64-linux-android/libmonero_wallet2_api_c.so > lib/android-amd64/libmonero_wallet2_api_c.so
docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/x86_64-linux-android/libc++_shared.so > lib/android-amd64/libc++_shared.so

docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/armv7a-linux-androideabi/libmonero_wallet2_api_c.so > lib/android-arm/libmonero_wallet2_api_c.so
docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/armv7a-linux-androideabi/libc++_shared.so > lib/android-arm/libc++_shared.so

docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-apple-ios/libmonero_wallet2_api_c.dylib > lib/ios-arm64/libmonero_wallet2_api_c.dylib

docker run --rm monero_c_builder cat /monero_c/release/v0.18.4.6-RC2/aarch64-apple-ios-simulator/libmonero_wallet2_api_c.dylib > lib/ios-arm64-simulator/libmonero_wallet2_api_c.dylib
```

Or pull the whole tree out at once:

```bash
docker run --rm monero_c_builder tar -cf - -C /monero_c/release . | tar -xf - -C out
```

Then publish what you built:

```bash
./publish-libs.sh --dry-run     # package and hash, upload nothing
./publish-libs.sh               # upload, then commit the rewritten lib/SHA256SUMS
```

A bare run takes every platform in `lib/` that has all of its files and skips
the rest with a warning, since publishing the subset you built is the normal
case; the skipped platforms keep whatever hashes `SHA256SUMS` already had.
Naming platforms as arguments is the stricter statement — you asked for those —
so an incomplete one is an error there rather than a warning.

## Runtime Requirements

After building caribou with XMR support, the library must be available at runtime:

**Option 1: Copy library next to executable (recommended for development)**
```bash
# Linux x86_64
cp lib/linux-amd64/libmonero_wallet2_api_c.so ./

# Linux ARM64
cp lib/linux-arm64/libmonero_wallet2_api_c.so ./

# macOS
cp lib/darwin-arm64/libmonero_wallet2_api_c.dylib ./  # or darwin-amd64

# Windows
copy lib\windows-amd64\libmonero_wallet2_api_c.dll .\
```

**Option 2: Install to application library directory (for system packaging)**
```bash
# Linux - use /usr/lib/caribou to avoid conflicts with other apps using monero_c
sudo mkdir -p /usr/lib/caribou
sudo cp lib/linux-amd64/libmonero_wallet2_api_c.so /usr/lib/caribou/
```

The binary's rpath is configured to search both the executable's directory and
`/usr/lib/caribou`, so either location works without setting `LD_LIBRARY_PATH`.
