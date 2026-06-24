# Boss Wallet — iOS shell

Thin SwiftUI WebView shell that hosts the Boss Wallet mobile SPA over the
in-process loopback appserver, mirroring `wallet/android`. All app logic lives in
the Go core (`wallet/mobile`, gomobile-bound) and the React bundle
(`wallet/appserver/mobile`); this target only boots the core, hosts a full-screen
`WKWebView`, confines it to loopback, and manages lifecycle.

See `~/ocean/bossmobile/gomobile.md` for the architecture (issue #74). Session
lock / biometrics (`biometrics.md`) and XMR (`xmr.md`) are separate slices and
are **not** implemented here — `AppModel.scenePhaseChanged` marks the lifecycle
seam where the biometric idle-timer work will land.

## Prerequisites

- macOS with Xcode (the iOS toolchain is Mac-only).
- [XcodeGen](https://github.com/yonki/XcodeGen): `brew install xcodegen`
- gomobile/gobind on PATH, initialized:
  ```sh
  go install golang.org/x/mobile/cmd/gomobile@latest
  go install golang.org/x/mobile/cmd/gobind@latest
  gomobile init
  ```

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

## Layout

| Path | Role |
|------|------|
| `project.yml` | XcodeGen project spec (source of truth; the `.xcodeproj` is generated and git-ignored) |
| `bind.sh` | Builds `Frameworks/Mobile.xcframework` via `gomobile bind -target=ios,iossimulator -tags mobile` |
| `BossWallet/BossWalletApp.swift` | `@main` entry; owns the app lifecycle |
| `BossWallet/AppModel.swift` | Boots/stops the bound Go `App`; supplies dataDir + network |
| `BossWallet/ContentView.swift` | Boot / error / WebView states |
| `BossWallet/WebView.swift` | `WKWebView` wrapper + loopback navigation confinement |
| `BossWallet/Info.plist` | ATS local-networking exception, `WalletNet`, launch screen |
| `BossWallet/Config/*.xcconfig` | Per-network bundle id, display name, `WALLET_NET` |
