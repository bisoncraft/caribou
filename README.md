# Caribou

[![ISC License](https://img.shields.io/badge/license-Blue_Oak-007788.svg)](https://blueoakcouncil.org/license/1.0.0)
[![GoDoc](https://img.shields.io/badge/go.dev-reference-blue.svg?logo=go&logoColor=lightblue)](https://pkg.go.dev/github.com/bisoncraft/caribou)

Caribou is a non-custodial, multi-chain wallet, from the developers of Bison Wallet.

## Guiding principles

- **Self-custody.** Your keys, your coins. You, and only you, control your funds.
- **Non-KYC.** No accounts, no identity verification, no address whitelisting,
  no third-party KYC/AML gates. Caribou does not require, collect, or
  forward identifying information to use any of its features, including
  swaps. Where a chain or swap counterparty is operated by a third party,
  Caribou prefers backends that do not impose KYC and surfaces the
  trade-off plainly when a non-KYC option is unavailable.

These principles are load-bearing — features that conflict with them do not
ship. Anything that requires custody of user funds is out of scope on its own
merits, regardless of convenience. Identity verification and gated access are
out of scope wherever an alternative exists. Fiat ramps and tokenized
securities have none, and opt-in third-party modules are the single bounded
exception described below.

### Opt-in third-party modules

Some routes have no non-KYC form. There is no non-KYC path from a bank
account to a blockchain: every fiat ramp performs identity verification as a
regulatory matter. Tokenized securities can be bought only where their
issuers permit, and some issuers verify identity themselves. The choice is
not between a KYC'd route and a non-KYC one — it is between offering the
route on strict terms and offering none. Caribou offers these routes as
third-party modules, bounded as follows:

- **Opt-in, off by default.** A module is inert unless the user turns it on.
  No default path through the wallet reaches one.
- **Nothing depends on one.** Every wallet capability works with all modules
  disabled. They add a route to fiat or to securities; they are never a
  prerequisite.
- **Disclosed at the point of use.** A module that imposes identity
  verification, geographic restriction, or its own terms says so before the
  user commits to it — including that an address is disclosed to the provider
  and thereby linked to a verified identity in its records.
- **Self-custody is not on the table.** Funds and tokenized securities are
  delivered to, and spent from, wallet-controlled addresses. A provider
  requiring custody is rejected outright, whatever else it offers.

The exception is scoped to fiat ramps and tokenized securities alone. The
non-KYC principle governs the wallet's own operation and its chain and swap
backends without exception, because there the alternatives exist and are
chosen.

## Building and testing

### Wallet binary

```bash
cd wallet/cmd/caribou && go build -o caribou
```

### Desktop wallet

The desktop app needs cgo plus the GTK and WebKitGTK development packages on
Linux (`libgtk-3-dev libwebkit2gtk-4.1-dev`). Release builds always carry the
`xmr` tag, which links `libmonero_wallet2_api_c` from
`wallet/asset/xmr/lib/<platform>/` (see `wallet/asset/xmr/lib/README.md`).

```bash
go build -tags xmr -o caribou-desktop ./wallet/cmd/caribou-desktop
./caribou-desktop
```

### Cross-platform release packaging

Desktop installers for Linux, Windows and macOS are built, signed and
packaged from a Linux host by the scripts in `release/`; see
[release/README.md](release/README.md). `./pkg.sh` builds the CLI/server
tarballs.

### Tests

```bash
# Full suite
./run_tests.sh

# Fast unit tests with race detection
go test -race -short ./...

# Single asset package
go test -race -short ./wallet/asset/btc/...
```

Some test files require build tags for tests that need real networks or
running nodes: `live`, `harness`, `electrumlive`, `rpclive`, `systray`.

### Lint

```bash
golangci-lint -c ./.golangci.yml run
```

### Web UI build (required before appserver tests)

```bash
cd wallet/appserver/newui && npm ci && npm run build
```

The `wallet/appserver/newui/dist/` directory is embedded via `go:embed`;
`go test ./wallet/appserver/...` will fail with `no matching files found`
if the bundle has not been built.

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
- Secure funding from Zcash Community Grants and the Dash DAO.
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

- Offer tokenized stocks in the United States as an opt-in module, through an
  issuer that performs its own identity verification and settles to the
  user's wallet.
- Add swap venues where coverage data shows gaps.

## Design notes

- **Asset interface.** Every chain implements `asset.Wallet` from
  `wallet/asset/interface.go`. Adding a new chain means implementing the
  interface and registering a driver under `wallet/asset/`.
- **Embedded web UI.** The current frontend bundle is included via
  `go:embed` from `wallet/appserver/newui/dist/`. The planned React desktop UI
  will follow the same embed pattern from `wallet/appserver/desktop/dist/`.
- **Build tags.** `live`, `harness`, `electrumlive`, `rpclive`, and `systray`
  gate tests requiring real network access, running nodes, or platform
  features. `mobile` / `!mobile` will gate the mobile vs. desktop binary.

## Documentation

- [Local Automation with API Tokens](docs/automation.md) — drive a running
  `caribou` from scripts, a CLI, or an MCP server over the appserver API.
- [Simnet Development Guide](docs/simnet-dev.md) — local regtest harnesses and
  running the wallet against them.
- [Desktop UI Development Guide](docs/desktop-ui-dev.md) — developing the
  desktop wallet UI.

## Contributing

Caribou is in heavy flux right now; large surfaces are being removed,
rewritten, or first introduced. Check with @buck54321 or @JoeGruffins if you're
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

## Heritage

Caribou descends from Bison Wallet, which itself was developed alongside
DCRDEX. The non-trading wallet, atomic-swap settlement, EVM relay, and Monero
adaptor-signature code are retained from that lineage; the credit for that
work belongs to the DCRDEX / Bison Wallet contributors.
