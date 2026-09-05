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

`fetch-libs.sh` downloads from this repo's `xmr-libs-*` releases and verifies
every file against `SHA256SUMS` in this directory before installing it.

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

**Whether that holds here is unverified.** monero_c makes no reproducibility
claim; its `generate_checksum.sh` hashes the wrapper *source* files, not build
outputs, and nobody has rebuilt this pin twice and compared. Monero's own
`contrib/depends` lineage is built for deterministic output, so it is plausible,
but plausible is not established. Until someone checks, treat the download as
convenience backed by an integrity pin, not as a verified build.

If you do rebuild, please report the result either way — a confirmed
reproduction is worth more here than any amount of tooling.

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

The desktop libraries are built with the Dockerfile (below). The two macOS
desktop `.dylib`s (`darwin-amd64/`, `darwin-arm64/`) can instead be built
natively on a Mac with `../build-darwin.sh` — faster there and it reuses a
locally-provided Xcode xip (`XCODE_XIP=/path/to/Xcode_26.1_Universal.xip`)
instead of re-downloading the ~2.7 GB SDK inside the Debian image. Mobile
libraries are built by the same Dockerfile but packaged differently: Android
`.so`s come from its `aarch64-linux-android` target and ship as `jniLibs` (see
`wallet/android/bind.sh`); iOS `.dylib`s come from its `aarch64-apple-ios` and
`aarch64-apple-ios-simulator` targets and are packaged into
`Monero.xcframework` by `wallet/ios/bind.sh` with `CARIBOU_XMR=1`.

The Apple targets, iOS included, cross-compile from Linux: simplybs' Apple SDK
package is osxcross, and it unpacks whichever SDK the target names out of the
one Xcode xip. `../build-ios.sh` builds the same two libraries natively on a
Mac, which is convenient if you already have the xip locally, but it is not
required. Mobile libs are embedded in the app
package rather than copied next to an executable, so the runtime notes below
apply to the desktop builds only.

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
