# Caribou Android shell

The Android app is a thin native shell (`MainActivity`) hosting the SPA in a
WebView over a loopback server, plus a keystore-backed biometric unlock
(`BiometricAuth`). The wallet logic is the gomobile-bound Go library
(`app/libs/mobile.aar`, built with `./bind.sh`).

## Build

Prerequisites: Go, Node, a JDK (17 or later), and the Android SDK with the
platform, build tools and NDK that `app/build.gradle` and `release/Dockerfile`
name. `local.properties` or `ANDROID_HOME` points at the SDK.

```sh
cd wallet/android
./bind.sh                           # web bundle + app/libs/mobile.aar
./gradlew :app:assembleNetSimnetDebug
```

`bind.sh` builds gomobile and gobind from the versions pinned in `go.mod`, so
neither needs installing. `CARIBOU_XMR=1 ./bind.sh` includes Monero, given
the libraries from `wallet/asset/xmr/fetch-libs.sh`.

Release builds are made by `release/package-android.sh`, which runs this
build in the release builder image, signs the APK and the bundle, and checks
the result; see `release/README.md`. A release build from Gradle alone is
unsigned.

### Emulator

On an Intel or AMD host the emulator runs an x86_64 system image and
translates ARM code. That translation cannot run the Go runtime: the default
arm64 build installs, then dies at launch with `SIGILL` in
`internal/cpu.getMIDR`. The emulator therefore needs a native build, of the
library and of the app:

```sh
CARIBOU_ANDROID_ABI=x86_64 ./bind.sh
./gradlew -Pcaribou.abi=x86_64 :app:assembleNetTestnetDebug
adb install -r app/build/outputs/apk/netTestnet/debug/app-netTestnet-debug.apk
```

With `CARIBOU_XMR=1`, the `android-amd64` libraries are needed as well. The
emulator itself needs KVM (`/dev/kvm`).

`MainActivity` marks its window secure, so `adb shell screencap` and screen
recording over adb show the app as black. `adb emu screenrecord screenshot
<dir>` captures through the emulator and shows it.

### Dependencies

Gradle itself is pinned by the wrapper (`gradle/wrapper/gradle-wrapper.properties`,
with `distributionSha256Sum`), and every plugin and library by its sha256 in
`gradle/verification-metadata.xml`: a build that resolves an artifact not
listed there, or one whose content differs, fails. After changing a
dependency or a plugin version, regenerate the file with a build that
exercises every variant, and review the diff:

```sh
./gradlew --write-verification-metadata sha256 assemble bundle assembleAndroidTest lint
```

Gradle records only what the build host resolves. `aapt2` is the one
artifact with a variant per host: the file lists the Linux jar from the run
above plus, added by hand, the `osx` and `windows` jars of the same version,
whose hashes are those of the files under
`https://dl.google.com/dl/android/maven2/com/android/tools/build/aapt2/<version>/`.
A new Android Gradle plugin version brings a new `aapt2`, so those two
entries are replaced with it.

The compile SDK, build tools and NDK versions are also pinned in
`release/Dockerfile`, by archive checksum; a bump changes both files.

### Launcher icon

`icon/make-icons.py` regenerates the adaptive icon's foreground and the
raster fallbacks from `wallet/ios/icon/appicon.svg`, the vector source of the
antler mark.

## Tests

Test coverage for this feature is split by what it needs to run:

| Layer | Where | How it runs |
| --- | --- | --- |
| Session lock/unlock, grant flow | `wallet/appserver` (Go) | `./run_tests.sh` (via `go test ./...`) |
| Mobile bridge (`Lock`/`Unlock`/`VerifyPassword`) | `wallet/mobile` (Go, `mobile` tag) | `./run_tests.sh` (explicit `-tags mobile` line) |
| Keystore / cipher (`BiometricAuth`) | `app/src/androidTest` (Kotlin) | **on a device/emulator only** (see below) |

### Instrumented tests (device or emulator required)

`BiometricAuth` drives the Android Keystore and `Cipher`, which only exist on a
real device or emulator, so these tests are **not** part of the headless
`run_tests.sh` and must be run separately.

Prerequisites:

- Android SDK (`local.properties` points `sdk.dir` at it).
- A connected `arm64-v8a` device or emulator (`adb devices` lists it). Network
  adb (`adb connect <ip>:<port>`) works.

Run them:

```sh
cd wallet/android
./gradlew :app:connectedNetSimnetDebugAndroidTest
```

Notes:

- The `netSimnet` debug variant has its own `applicationId` suffix (`.simnet`),
  so it installs alongside a real build without touching it.
- The tests use a throwaway keystore alias and prefs file (via test-only
  constructor parameters on `BiometricAuth`), so they never touch a user's real
  biometric enrollment.
- They cover the keystore/cipher layer beneath the prompt (key lifecycle,
  AES/GCM round-trip, GCM IV binding). The `enroll`/`unlock` flows go through a
  live `BiometricPrompt` (a real fingerprint/credential touch) and are out of
  scope for automated runs.

HTML report after a run:
`app/build/reports/androidTests/connected/index.html`.
