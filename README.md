<p align="center">
  <img src="docs/branding/antlers_250x250.png" width="125" alt="">
</p>

<h1 align="center">Caribou</h1>

<p align="center">
  <strong>Your personal treasury.</strong><br>
  Gold, cash and coins in one wallet, under keys that never leave your device.
</p>

<p align="center">
  <a href="https://caribou.you">Website</a> ·
  <a href="#roadmap">Roadmap</a> ·
  <a href="#building-from-source">Build from source</a> ·
  <a href="SECURITY.md">Security</a>
</p>

<p align="center">
  <a href="https://blueoakcouncil.org/license/1.0.0"><img src="https://img.shields.io/badge/license-Blue_Oak-007788.svg" alt="Blue Oak license"></a>
  <a href="https://pkg.go.dev/github.com/bisoncraft/caribou"><img src="https://img.shields.io/badge/go.dev-reference-blue.svg?logo=go&logoColor=lightblue" alt="Go reference"></a>
</p>

Caribou is a free, open-source wallet for desktop and mobile that keeps
tokenized gold, digital dollars and coins side by side in one portfolio.
There is no account to create and no ID to show. Caribou is built by
Bisoncraft LLC, the developers of Bison Wallet and DCRDEX.

> [!IMPORTANT]
> Caribou is pre-release software and has no tagged release yet. The first
> desktop alpha is planned for October 2026. Until then, build from source,
> and use small amounts.

## What Caribou holds

| | Assets | Details |
| --- | --- | --- |
| **Gold** | PAX Gold (PAXG), Tether Gold (XAUT) | On Ethereum. Each token is one troy ounce of an allocated bar, per its issuer. Send any fraction of an ounce. |
| **Cash** | USDC, USDT | On Ethereum, BNB Smart Chain, Polygon and Solana; USDC also on the XRP Ledger. Each shows as one balance, however many networks it sits on. |
| **Coins** | Bitcoin, Ethereum, Solana, XRP, BNB, Litecoin, Hyperliquid (HYPE), Polygon (POL), Dash, Monero | The native money of each network. Monero is part of desktop release builds. |
| **Investments** | Tokenized stocks and ETFs | Planned; see the [roadmap](#roadmap). |

## Why Caribou

- **Only you hold the keys.** Keys are generated and stored on your device.
  Lose the device, and the recovery phrase restores everything on a new one.
  Lose the phrase, and no one can restore it for you, Bisoncraft included.
- **No account, no ID check.** There is nothing to sign up for. The wallet's
  password unlocks it locally and is not an account.
- **Native wallets.** Bitcoin, Litecoin and Dash run as native wallets on
  your device, syncing through Electrum servers or compact block filters
  (BIP 157/158).
- **Swaps built in.** Swap between supported assets across chains through
  NEAR Intents. The fee and the amount received are itemized before you
  confirm. While a swap settles, the funds are held by NEAR Intents solvers
  and verifiers until delivery or refund.
- **Scriptable.** A token-authenticated API, bound to localhost, lets scripts,
  a CLI or an MCP server drive a running wallet
  ([docs/automation.md](docs/automation.md)).

### What the software contacts

Caribou runs locally and does not require a publisher account. It connects to
blockchain networks and third-party services for balances, transactions,
prices and swap availability. Those services can receive connection and
request information. Swaps also depend on external settlement infrastructure.

Bisoncraft operates no server that the wallet reports to, and the wallet sends
Bisoncraft no telemetry or crash reports.

### What it costs

Caribou is free. Sending pays only the network's transaction fee. Swaps carry
an application fee, shown in the quote breakdown before you confirm.

## Guiding principles

- **Self-custody.** Your keys, your coins. Keys are generated and kept on
  your device, and funds are held at addresses the wallet controls. Caribou
  has no custodial accounts, and no funds are kept with Bisoncraft or any
  third party. Funds leave the wallet's control only when you send them or
  swap them across chains; during a cross-chain swap, the settlement network
  holds them until delivery or refund, secured by their own blockchain
  protocol.
- **Privacy by default, disclosure by choice.** Caribou requires no account
  or identity verification to hold, send, receive, or swap supported assets,
  and it collects no identifying information. Planned opt-in fiat and
  securities integrations may require verification directly with their
  providers.

### Opt-in third-party modules

Buying crypto with a card or bank transfer, and buying tokenized securities,
depend on providers with their own verification rules. The planned Ramp
Network integration requires identity verification with Ramp, and some
issuers of tokenized securities verify buyers themselves. Caribou plans to
offer these routes as opt-in modules, and each integration must meet four
requirements:

- **Opt-in, off by default.** A module does nothing until the user turns it
  on. No default path through the wallet reaches one.
- **Nothing depends on one.** Holding, sending, receiving, and swapping remain
  available with all modules disabled.
- **Disclosed at the point of use.** A module that imposes identity
  verification, geographic restrictions, transfer restrictions, or its own
  terms says so before the user commits to it. The disclosure includes that
  the provider receives a wallet address and can link it to the identity it
  verified.
- **Delivered to the wallet.** Purchases and tokenized securities are
  delivered to addresses the wallet controls and spent from them. Providers
  that hold funds or securities on the user's behalf are not integrated.

## Platforms

| Platform | Status |
| --- | --- |
| Linux | Desktop alpha planned for October 2026: `.deb` and `.tar.gz`, amd64 |
| macOS | Desktop alpha planned for October 2026: notarized `.dmg`, Apple silicon and Intel |
| Windows | Desktop alpha planned for October 2026: signed installer and `.zip`, amd64 |
| Android | In development; alpha planned for November 2026 |
| iOS | In development; alpha planned for November 2026 |

Each release will publish a `SHA256SUMS` manifest with a GPG signature, and
the signing key's fingerprint, so a download can be verified before it is
run.

## Roadmap

Caribou is under active development. These are target milestones; timing may
shift based on testing, funding, and partner onboarding.

### October 2026 — Desktop alpha

- Make the repository public.
- Release the desktop alpha for Linux, macOS, and Windows, with a signed
  release manifest.
- Recruit alpha testers.
- Begin onboarding with Ramp Network.
- **Zcash phase 0 — Funding and FFI validation**
  - Submit a funding proposal to Zcash Community Grants.
  - Demonstrate the foreign function interface (FFI) with passing tests.
- **Dash staking phase 0 — Funding and backend**
  - Submit a funding proposal to the Dash DAO.
  - Implement the staking backend, including key handling and custom
    transaction handling.
  - Implement a simulated server for testing on simnet.

### November 2026 — Mobile alpha and integration development

- Open the alpha to Android and iOS testers through TestFlight and a Google
  Play closed test.
- Target funding decisions from Zcash Community Grants and the Dash DAO.
- **Zcash phase 1 — Go integration:** Implement the Go wrapper and its tests.
- **Dash staking phase 1 — Mainnet and user experience:** Test on mainnet and
  implement the staking UI/UX.

### December 2026 — Swap foundations

- Move swaps behind a provider interface that ranks quotes across venues,
  with NEAR Intents as the first provider.
- Add memo-bearing sends for UTXO chains, EVM chains, and XRP, as required by
  protocol-vault swap venues.

### Q1 2027 — Beta

- Release the beta on all five platforms.
- Publish to the App Store and Google Play.
- Ship the Zcash shielded light wallet.
- Ship Dash staking.
- Offer card purchases through Ramp Network as an opt-in module.
- Add THORChain as a second cross-chain swap venue for BTC, LTC, ETH, and XRP,
  with quote ranking and failover between venues. Monero follows when
  THORChain activates its Monero vault.
- Add same-chain swaps on Ethereum and BNB Smart Chain through the Uniswap
  Trading API, including PAXG.

### Q2 2027 — Equities outside the United States

- Offer tokenized stocks as an opt-in module outside the United States,
  where issuers permit, with eligibility checks by jurisdiction.
- Add same-chain swaps on Solana through Jupiter and on BNB Smart Chain
  through PancakeSwapX.
- Add Chainflip as a third cross-chain swap venue.

### Q3 2027 — Equities in the United States

- Subject to a suitable issuer partnership, offer tokenized stocks in the
  United States as an opt-in module, through an issuer that performs its own
  identity verification and delivers to the user's wallet.
- Add swap venues where coverage data shows gaps.

---

## Building from source

Requirements:

- Go 1.25 or later.
- Node.js 20.19+ or 22.13+ with npm, for the web UI.
- For the desktop app: cgo, plus the GTK and WebKitGTK development packages
  on Linux (`libgtk-3-dev libwebkit2gtk-4.1-dev`).

### Web UI

The desktop and mobile UIs are React and TypeScript bundles in one npm
workspace under `wallet/appserver` (`shared`, `desktop` and `mobile`). The Go
binaries embed the built bundle with `go:embed`, so build it before building
a binary:

```bash
cd wallet/appserver && npm ci && npm run build
```

A binary built without this step compiles against a placeholder and serves no
UI.

### Desktop app

```bash
go build -o caribou-desktop ./wallet/cmd/caribou-desktop
./caribou-desktop
```

Release builds add the `xmr` tag, which links the Monero library
`libmonero_wallet2_api_c` from `wallet/asset/xmr/lib/<platform>/`. The
libraries are not committed; [wallet/asset/xmr/lib/README.md](wallet/asset/xmr/lib/README.md)
covers obtaining or building them. With the libraries in place:

```bash
go build -tags xmr -o caribou-desktop ./wallet/cmd/caribou-desktop
```

### Command-line wallet

`caribou` runs the wallet as a local server and serves the UI to a browser.
The `systray` tag adds a tray icon.

```bash
go build -o caribou ./wallet/cmd/caribou
```

### Mobile apps

The Android and iOS apps are thin native shells that host the mobile UI over
a loopback server, around the Go core bound with gomobile from
`wallet/mobile`. See [wallet/android/README.md](wallet/android/README.md) and
[wallet/ios/README.md](wallet/ios/README.md).

### Release packaging

Desktop installers and command-line archives for Linux, Windows and macOS
are built, signed and packaged from a Linux host by the scripts in
`release/`; see [release/README.md](release/README.md).

## Testing

```bash
# Full suite
./run_tests.sh

# Fast unit tests with race detection
go test -race -short ./...

# Single asset package
go test -race -short ./wallet/asset/btc/...

# Web UI lint and tests
(cd wallet/appserver && npm run lint && npm run test)
```

Some tests need real networks or running nodes and are gated by build tags:
`live`, `rpclive` and `harness`. The
[Simnet Development Guide](docs/simnet-dev.md) covers the local harnesses.

### Lint

```bash
golangci-lint -c ./.golangci.yml run
markdownlint-cli2 "*.md" "docs/**/*.md"
```

## Architecture

- **Asset interface.** Every chain implements `asset.Wallet` from
  `wallet/asset/interface.go`. Adding a new chain means implementing the
  interface and registering a driver under `wallet/asset/`;
  `wallet/asset/importall` sets which assets a build includes.
- **Embedded UI.** The appserver serves the desktop bundle
  (`wallet/appserver/desktop`) by default and the mobile bundle
  (`wallet/appserver/mobile`) under the `mobile` tag. Both build on
  `wallet/appserver/shared`.

## Documentation

- [Local Automation with API Tokens](docs/automation.md) — drive a running
  `caribou` from scripts, a CLI, or an MCP server over the appserver API.
- [Simnet Development Guide](docs/simnet-dev.md) — local regtest harnesses and
  running the wallet against them.
- [Desktop UI Development Guide](docs/desktop-ui-dev.md) — developing the
  desktop wallet UI.
- [Desktop Release Packaging](release/README.md) — building, signing and
  packaging the desktop installers.

## Contributing

Caribou is changing quickly, and large parts of the code are being removed,
rewritten, or added. Check with @buck54321 or @JoeGruffins if you're
interested in helping out.

## Reporting issues

Found a bug or want a feature? Open an issue and fill in the form for your
case: [bug report](https://github.com/bisoncraft/caribou/issues/new?template=bug_report.yml)
or [feature request](https://github.com/bisoncraft/caribou/issues/new?template=feature_request.yml).
Include your version, platform, and steps to reproduce. For the CLI, run with
`--log=debug` and attach the relevant log lines.

Never post seed phrases, private keys, or passwords in an issue. Report
security vulnerabilities privately as described in [SECURITY.md](SECURITY.md),
not in a public issue.

## License

Caribou is released under the [Blue Oak Model License 1.0.0](LICENSE.md).

## Heritage

Caribou descends from Bison Wallet, which itself was developed alongside
DCRDEX. The multi-chain wallet code, including the Monero adaptor-signature
implementation, is carried over from that lineage; the credit for that work
belongs to the DCRDEX / Bison Wallet contributors.
