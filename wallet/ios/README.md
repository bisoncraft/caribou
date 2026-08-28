# Caribou — iOS shell

Thin SwiftUI WebView shell that hosts the Caribou mobile SPA over the
in-process loopback appserver, mirroring `wallet/android`. All app logic lives in
the Go core (`wallet/mobile`, gomobile-bound) and the React bundle
(`wallet/appserver/mobile`); this target only boots the core, hosts a full-screen
`WKWebView`, confines it to loopback, and manages lifecycle.

See `~/ocean/bossmobile/gomobile.md` for the architecture (issue #74). Session
lock / biometrics (`biometrics.md`) is implemented here, mirroring the Android
shell — `AppModel` drives the idle-gate / key-eviction tiers off the SwiftUI
scene lifecycle, with `BiometricAuth` backing the Keychain re-auth (see
[Session lock & biometrics](#session-lock--biometrics)). XMR (`xmr.md`) is opt-in
and wired via `CARIBOU_XMR=1 ./bind.sh` + `project.xmr.yml` (see
[XMR (Monero)](#xmr-monero)).

## Prerequisites

- macOS with Xcode (the iOS toolchain is Mac-only).
- [XcodeGen](https://github.com/yonki/XcodeGen): `brew install xcodegen`
- gomobile/gobind on PATH, initialized:
  ```sh
  go install golang.org/x/mobile/cmd/gomobile@latest
  go install golang.org/x/mobile/cmd/gobind@latest
  gomobile init
  ```
- For device builds, a signing team. Create the git-ignored
  `Caribou/Config/Local.xcconfig` with your Apple Developer team:
  ```
  DEVELOPMENT_TEAM = YOURTEAMID
  ```
  (or set the team in Xcode's Signing & Capabilities). Simulator builds need none.

## Build & run

```sh
cd wallet/ios
./bind.sh              # builds Frameworks/Mobile.xcframework (runs webpack first)
xcodegen generate      # generates Caribou.xcodeproj from project.yml
open Caribou.xcodeproj
```

Pick a scheme and Run:

| Scheme                  | Bundle id                              |
|-------------------------|----------------------------------------|
| `Caribou (mainnet)`  | `com.bisoncraft.caribou`            |
| `Caribou (testnet)`  | `com.bisoncraft.caribou.testnet`    |
| `Caribou (simnet)`   | `com.bisoncraft.caribou.simnet`     |

Each scheme selects a network the same way Android's product flavors do: the
`WALLET_NET` build setting (from `Caribou/Config/<net>.xcconfig`) is injected
into `Info.plist` as `WalletNet` and read at runtime by `AppModel`, which passes
it to `App.start`. The three bundle ids install side by side.

`./bind.sh` rebuilds the web bundle each run; set `CARIBOU_SKIP_WEB_BUILD=1` to
skip if you just built it. Re-run `./bind.sh` whenever the Go core or the mobile
bundle changes; re-run `xcodegen generate` only when `project.yml` or the file
layout changes.

## Session lock & biometrics

The shell locks the wallet when the user is away and re-authenticates on return,
per `~/ocean/bossmobile/biometrics.md`. The shared halves (the `App.Lock` /
`App.Unlock` / `App.VerifyPassword` bound surface, the appserver's grant-URL
session restore) are platform-neutral and already built; this shell adds the iOS
native pieces, mirroring Android's `MainActivity` + `BiometricAuth`:

- **Two idle tiers, fed by the scene lifecycle.** `AppModel.scenePhaseChanged`
  stamps a "backgrounded at" time on `.background` and, on `.active`, picks a
  tier from how long the app was away: under the gate → resume untouched;
  past the gate → a cheap biometric **UI gate** (an opaque overlay over the still
  logged-in WebView, cleared by a presence check, no re-login); past the eviction
  window → **key eviction** (`App.Lock`: wallets locked, keys zeroed, session
  torn down), then a biometric-assisted `App.Unlock`. Normal app switching stays
  under the gate and never re-auths. Both thresholds are user-configurable from
  the SPA Settings pane (defaults: 10 min gate, 1 hour eviction).
- **Background pause, below both tiers.** After a short grace on `.background`
  (15s; Android uses 30s — see below), `App.Pause` drops every wallet's network
  connection so a backgrounded phone stops polling and the radio can sleep. Keys
  and the session stay resident, so `App.Resume` on `.active` reconnects with no
  re-auth; a quick app switch cancels the still-pending grace and never
  disconnects. Orthogonal to the two tiers: this restores connections, they
  restore the session.
- **Keychain-backed re-auth (`BiometricAuth.swift`).** The app password is stored
  as a `.biometryCurrentSet` Keychain item — released only behind Face ID /
  Touch ID, invalidated on a biometric enrollment change, device-bound and
  non-syncing. `Info.plist` carries the required `NSFaceIDUsageDescription`.
- **`window.caribou` bridge.** The SPA's biometric opt-in and lock-timing controls
  call a native bridge. iOS message handlers are async/void, but the SPA reads
  `biometricAvailable()` / `getLockConfig()` synchronously, so the injected
  `window.caribou` getters read a native-pushed `window.__caribou_lock` object while
  the void actions (enroll/disable/setLockConfig) post messages. State is
  re-pushed and a `caribou-biometric-changed` event fired on every change and page
  load.
- **Privacy cover.** An opaque brand screen covers the WebView whenever the app
  is not active, so wallet content stays out of the OS app-switcher snapshot (the
  closest iOS analog of Android's `FLAG_SECURE` for the recents thumbnail; iOS
  has no API to block user-initiated screenshots).

Two deliberate platform differences, both from the same root: iOS suspends a
backgrounded process within seconds and only a `beginBackgroundTask` assertion
holds it off, for ~25-30s.

Unlike Android's `AlarmManager`, iOS cannot run a timer to evict keys at an
arbitrary future time while suspended, so eviction is primarily the
**resume-time backstop** (plus a best-effort background-task timer for short
windows) — exactly the "best-effort, resume re-checks" policy the design doc
calls for; an OS process-kill evicts the keys for free.

The pause grace runs inside an assertion of its own (separate from eviction's,
which does not exist at all in the gate-only configuration) and so must fit
inside that window with room for `App.Pause` to finish under it — hence 15s
rather than Android's 30s. The window cannot be measured and clamped to instead:
`backgroundTimeRemaining` still reads `greatestFiniteMagnitude` when the scene
phase turns `.background`, even with the assertion already begun. If the system
reclaims the window first anyway, the expiration handler pauses immediately
rather than losing the work.

The native lock strings are English constants (`LockStrings`); an `.xcstrings`
catalog can follow the same i18n slice Android did.

## XMR (Monero)

XMR is opt-in because it links the native `libmonero_wallet2_api_c` library.
Default builds omit it (`-tags mobile`); everything else still works. To include
it:

```sh
# 1. Build the native monero libs for the two iOS triples (macOS only; needs the
#    Apple SDK provisioned into simplybs — see the header of build-ios.sh).
#    Produces the per-triple dylibs under wallet/asset/xmr/lib/ios-arm64{,-simulator}/.
../asset/xmr/build-ios.sh

# 2. Bind with the xmr tag and assemble Monero.xcframework from those dylibs.
CARIBOU_XMR=1 ./bind.sh

# 3. Generate the project with the XMR overlay so Xcode embeds + links monero.
xcodegen generate --spec project.xmr.yml
```

How it fits together (details in `../asset/xmr/cxmr/cgo_ios.go`):

- gomobile binds the Go core as a **c-archive** (static), so the monero symbols
  are left undefined and resolved when **Xcode links the app** — nothing about
  monero is linked at `gomobile bind` time.
- `CARIBOU_XMR=1 ./bind.sh` wraps each triple's dylib as a `Monero.framework` and
  packs the device + simulator slices into `Frameworks/Monero.xcframework`.
- `project.xmr.yml` includes `project.yml` and adds that xcframework as an
  embedded, signed app dependency. Xcode picks the device or simulator slice per
  destination, so there is no arm64 device/simulator ambiguity to resolve in cgo.

The native library build (step 1) is the one piece that cannot run from the
Debian Dockerfile that produces the desktop/Android libs: it needs the Apple SDK.
It realistically runs in a macOS CI job with Apple credentials.

## Release (TestFlight / App Store)

```sh
CARIBOU_XMR=1 ./bind.sh
xcodegen generate --spec project.xmr.yml
./release.sh --upload
```

`release.sh` archives the **mainnet** scheme only (testnet/simnet bundle ids are
not registered for distribution), exports a signed `.ipa`, validates it, and
uploads. It derives `CFBundleVersion` from the commit count, because App Store
Connect permanently rejects a build number it has already seen for a given
`MARKETING_VERSION` — override with `--build N` if you need to.

The commit count is only *locally* monotonic: it counts the current branch, so
uploading from a long topic branch and then from `master` can go backwards and
the upload is rejected with "the bundle version must be higher than the
previously uploaded version". That fails loudly at upload, costing a minute, not
a review cycle — pass `--build N` above the last number the app has seen.

## Checking whether a build actually made it

A successful upload is not a successful build. Apple validates the package
synchronously (`VERIFY SUCCEEDED`) but only *processes* it minutes later, and a
build that fails processing silently never appears in TestFlight — the tab keeps
saying "submit a build to start testing". `release.sh` prints the delivery UUID;
ask what became of it:

```sh
xcrun altool --build-status --delivery-id <uuid> \
    --provider-public-id <uuid from --list-providers> --wait \
    -u "$APPLE_ID" -p @env:APPLE_PASSWORD
```

`BUILD-STATUS: VALID_BINARY` means it is in TestFlight; `FAILED` prints the
processing errors, which are the only place the real reason is stated.

Credentials come in two flavours, because only the team's **Account Holder** can
create an App Store Connect API key:

| | Provisioning | Upload |
|---|---|---|
| API key (`ASC_KEY_ID` + `ASC_ISSUER_ID`) | ✅ | ✅ |
| Apple ID signed into Xcode + app-specific password (`APPLE_ID` + `APPLE_PASSWORD`) | ✅ | ✅ |

An Admin who cannot mint an API key uses the second row: sign in once at Xcode →
Settings → Accounts for provisioning, and generate an app-specific password at
appleid.apple.com for the upload. Those are the same two variables the desktop
build already exports for notarytool, and **the same password works** — an
app-specific password belongs to the Apple ID, not to an app or a team — so a
shell set up to package the desktop app needs nothing new. (`ASC_APPLE_ID` /
`ASC_APP_PASSWORD` override them if the two must differ.)

Better still, keep the password out of the environment entirely by storing it in
the login keychain once — `release.sh` looks there when no password variable is
set, so only the non-secret `APPLE_ID` need ever be exported:

```sh
export APPLE_ID=you@example.com
security add-generic-password -s caribou-appstore -a "$APPLE_ID" -w
```

`-w` with no value prompts without echoing, so the password stays out of shell
history and out of `ps` for the whole process tree.

If the Apple ID belongs to more than one team, also set `ASC_PROVIDER` to the
org's provider *shortname* — altool refuses to guess, and this is not the same
thing as the desktop's `APPLE_TEAM_ID`. List them with:

```sh
xcrun altool --list-providers -u "$APPLE_ID" -p "@env:APPLE_PASSWORD"
```

Full details in the header of `release.sh`.

Store-submission specifics that live in the repo:

- **App icon.** `Caribou/Assets.xcassets` holds a single 1024×1024 master, with
  Xcode deriving the rest. Regenerate it from the brand mark with
  `icon/make-appicon.sh` — that script also strips the alpha channel, which App
  Store Connect rejects an icon for carrying.
- **Privacy manifest.** `Caribou/PrivacyInfo.xcprivacy` declares no data
  collection and four required-reason API categories. Those are derived from the
  symbols actually present in the embedded binaries, not guessed; re-check with
  `nm -u` after any `bind.sh` change that pulls in new native code, or Apple
  mails an ITMS-91053 rejection after the upload appears to succeed.
- **Export compliance.** `ITSAppUsesNonExemptEncryption` is `false` in
  `Info.plist` so the questionnaire does not reappear on every upload. That
  answer rests on Caribou's source being published and on the BIS/NSA
  notification for License Exception TSU having been filed — see the note in
  `Info.plist`.
- **iPhone only.** `TARGETED_DEVICE_FAMILY` is `1`. Restore `1,2` once the SPA
  has an iPad layout worth shipping.

## Layout

| Path | Role |
|------|------|
| `project.yml` | XcodeGen project spec (source of truth; the `.xcodeproj` is generated and git-ignored) |
| `project.xmr.yml` | XcodeGen overlay (`include`s `project.yml`) that adds `Monero.xcframework`; used for XMR builds |
| `bind.sh` | Builds `Frameworks/Mobile.xcframework` via `gomobile bind`; `CARIBOU_XMR=1` adds the xmr tag and assembles `Monero.xcframework` |
| `../asset/xmr/build-ios.sh` | Builds the native `libmonero_wallet2_api_c` dylibs for the two iOS triples (macOS; needs the Apple SDK) |
| `Caribou/CaribouApp.swift` | `@main` entry; owns the app lifecycle |
| `Caribou/AppModel.swift` | Boots/stops the bound Go `App`; supplies dataDir + network; drives the session-lock tiers and the `window.caribou` bridge |
| `Caribou/BiometricAuth.swift` | Keychain-backed app-password store, released behind Face ID / Touch ID |
| `Caribou/LockOverlay.swift` | UI-gate overlay + app-switcher privacy cover + native lock strings |
| `Caribou/ContentView.swift` | Boot / error / WebView states, with the lock + privacy overlays on top |
| `Caribou/WebView.swift` | `WKWebView` wrapper + loopback navigation confinement + bridge wiring |
| `Caribou/Info.plist` | ATS local-networking exception, `WalletNet`, launch screen, export compliance |
| `Caribou/Config/*.xcconfig` | Per-network bundle id, display name, `WALLET_NET` |
| `Caribou/Assets.xcassets` | App icon (single 1024 master; other sizes derived) |
| `Caribou/PrivacyInfo.xcprivacy` | Privacy manifest: no data collected, required-reason API declarations |
| `icon/` | App icon source SVG + `make-appicon.sh` regenerator |
| `release.sh` | Archive → export `.ipa` → validate → upload to App Store Connect |
| `ExportOptions.plist` | Export settings for the App Store archive (team id injected at run time) |
