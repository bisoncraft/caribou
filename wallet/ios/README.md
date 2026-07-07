# Boss Wallet — iOS shell

Thin SwiftUI WebView shell that hosts the Boss Wallet mobile SPA over the
in-process loopback appserver, mirroring `wallet/android`. All app logic lives in
the Go core (`wallet/mobile`, gomobile-bound) and the React bundle
(`wallet/appserver/mobile`); this target only boots the core, hosts a full-screen
`WKWebView`, confines it to loopback, and manages lifecycle.

See `~/ocean/bossmobile/gomobile.md` for the architecture (issue #74). Session
lock / biometrics (`biometrics.md`) is a separate slice and is **not**
implemented here — `AppModel.scenePhaseChanged` marks the lifecycle seam where
the biometric idle-timer work will land. XMR (`xmr.md`) is opt-in and wired via
`BOSSW_XMR=1 ./bind.sh` + `project.xmr.yml` (see [XMR (Monero)](#xmr-monero)).

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
  `BossWallet/Config/Local.xcconfig` with your Apple Developer team:
  ```
  DEVELOPMENT_TEAM = YOURTEAMID
  ```
  (or set the team in Xcode's Signing & Capabilities). Simulator builds need none.

## Build & run

```sh
cd wallet/ios
./bind.sh              # builds Frameworks/Mobile.xcframework (runs webpack first)
xcodegen generate      # generates BossWallet.xcodeproj from project.yml
open BossWallet.xcodeproj
```

Pick a scheme and Run:

| Scheme                  | Bundle id                              |
|-------------------------|----------------------------------------|
| `BossWallet (mainnet)`  | `com.bisoncraft.bosswallet`            |
| `BossWallet (testnet)`  | `com.bisoncraft.bosswallet.testnet`    |
| `BossWallet (simnet)`   | `com.bisoncraft.bosswallet.simnet`     |

Each scheme selects a network the same way Android's product flavors do: the
`WALLET_NET` build setting (from `BossWallet/Config/<net>.xcconfig`) is injected
into `Info.plist` as `WalletNet` and read at runtime by `AppModel`, which passes
it to `App.start`. The three bundle ids install side by side.

`./bind.sh` rebuilds the web bundle each run; set `BOSSW_SKIP_WEB_BUILD=1` to
skip if you just built it. Re-run `./bind.sh` whenever the Go core or the mobile
bundle changes; re-run `xcodegen generate` only when `project.yml` or the file
layout changes.

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
BOSSW_XMR=1 ./bind.sh

# 3. Generate the project with the XMR overlay so Xcode embeds + links monero.
xcodegen generate --spec project.xmr.yml
```

How it fits together (details in `../asset/xmr/cxmr/cgo_ios.go`):

- gomobile binds the Go core as a **c-archive** (static), so the monero symbols
  are left undefined and resolved when **Xcode links the app** — nothing about
  monero is linked at `gomobile bind` time.
- `BOSSW_XMR=1 ./bind.sh` wraps each triple's dylib as a `Monero.framework` and
  packs the device + simulator slices into `Frameworks/Monero.xcframework`.
- `project.xmr.yml` includes `project.yml` and adds that xcframework as an
  embedded, signed app dependency. Xcode picks the device or simulator slice per
  destination, so there is no arm64 device/simulator ambiguity to resolve in cgo.

The native library build (step 1) is the one piece that cannot run from the
Debian Dockerfile that produces the desktop/Android libs: it needs the Apple SDK.
It realistically runs in a macOS CI job with Apple credentials.

## Layout

| Path | Role |
|------|------|
| `project.yml` | XcodeGen project spec (source of truth; the `.xcodeproj` is generated and git-ignored) |
| `project.xmr.yml` | XcodeGen overlay (`include`s `project.yml`) that adds `Monero.xcframework`; used for XMR builds |
| `bind.sh` | Builds `Frameworks/Mobile.xcframework` via `gomobile bind`; `BOSSW_XMR=1` adds the xmr tag and assembles `Monero.xcframework` |
| `../asset/xmr/build-ios.sh` | Builds the native `libmonero_wallet2_api_c` dylibs for the two iOS triples (macOS; needs the Apple SDK) |
| `BossWallet/BossWalletApp.swift` | `@main` entry; owns the app lifecycle |
| `BossWallet/AppModel.swift` | Boots/stops the bound Go `App`; supplies dataDir + network |
| `BossWallet/ContentView.swift` | Boot / error / WebView states |
| `BossWallet/WebView.swift` | `WKWebView` wrapper + loopback navigation confinement |
| `BossWallet/Info.plist` | ATS local-networking exception, `WalletNet`, launch screen |
| `BossWallet/Config/*.xcconfig` | Per-network bundle id, display name, `WALLET_NET` |
