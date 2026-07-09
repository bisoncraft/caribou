# monero_c Libraries

This directory holds `libmonero_wallet2_api_c` libraries for each supported platform.
Libraries must be built using the Dockerfile in the parent directory (see below).

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

The desktop libraries are built with the Dockerfile (below). Mobile libraries
are built and packaged differently: Android `.so`s come from the Dockerfile's
`aarch64-linux-android` target and ship as `jniLibs` (see `wallet/android/bind.sh`);
iOS `.dylib`s are built by `../build-ios.sh` on macOS (the Apple SDK cannot live
in the Debian image) and packaged into `Monero.xcframework` by
`wallet/ios/bind.sh` with `CARIBOU_XMR=1`. Mobile libs are embedded in the app
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
```

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
