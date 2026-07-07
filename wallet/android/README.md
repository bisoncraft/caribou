# Boss Wallet Android shell

The Android app is a thin native shell (`MainActivity`) hosting the SPA in a
WebView over a loopback server, plus a keystore-backed biometric unlock
(`BiometricAuth`). The wallet logic is the gomobile-bound Go library
(`app/libs/mobile.aar`, built with `./bind.sh`).

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

- Android SDK (`local.properties` points `sdk.dir` at it) and a Gradle 8.x.
- A connected `arm64-v8a` device or emulator (`adb devices` lists it). Network
  adb (`adb connect <ip>:<port>`) works.

Run them:

```sh
cd wallet/android
gradle :app:connectedNetSimnetDebugAndroidTest
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
