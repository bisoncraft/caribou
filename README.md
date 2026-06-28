# Boss Wallet

[![ISC License](https://img.shields.io/badge/license-Blue_Oak-007788.svg)](https://blueoakcouncil.org/license/1.0.0)
[![GoDoc](https://img.shields.io/badge/go.dev-reference-blue.svg?logo=go&logoColor=lightblue)](https://pkg.go.dev/github.com/bisoncraft/bosswallet)

Boss Wallet is a non-custodial, multi-chain wallet, from the developers of Bison Wallet.

## Guiding principles

- **Self-custody.** Your keys, your coins. You, and only you, control your funds.
- **Non-KYC.** No accounts, no identity verification, no address whitelisting,
  no third-party KYC/AML gates. Boss Wallet does not require, collect, or
  forward identifying information to use any of its features, including
  swaps. Where a chain or swap counterparty is operated by a third party,
  Boss Wallet prefers backends that do not impose KYC and surfaces the
  trade-off plainly when a non-KYC option is unavailable.

These principles are load-bearing — features that conflict with them do not
ship. Anything that requires custody of user funds, identity verification, or
gated access is out of scope on its own merits, regardless of convenience.

## Building and testing

### Wallet binary

```bash
cd wallet/cmd/bossw && go build -o bossw
```

### Desktop wallet

```bash
cd wallet/cmd/bossw-desktop && go build -o bossw-desktop
```

### Cross-platform release packaging

```bash
./pkg.sh
```

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

Boss Wallet is under active development. Our near-term goals are:

- **May 2026**: Complete a comprehensive code refactor and achieve high test coverage for all core wallet components.
- **June - July 2026**:
    - Implement basic sync, send, and receive functionality end-to-end for Bitcoin (BTC), Litecoin (LTC), Ethereum (ETH), Polygon (POL), and stablecoin tokens (USDT, USDC).
    - Integrate Chainlink oracle feeds.
    - Go-based Electrum wallet for BTC and LTC
    - Mobile wallet infrastructure
        - Database configuration and tuning
        - Build scripts
        - Emulator documentation
        - Basic UI implementation
- **End of July 2026**:
    - Secure grant funding for light wallet implementations, with a focus on Zcash (ZEC) and Monero (XMR)
    - Implement basic wallets for Zcash and Monero
    - Add tokens for xAUT, PAXG, and other commodity tokens
- **August 2026**:
    - Implement NEAR Intents for efficient cross-chain interactions, and demonstrate basic swap functionality between supported assets
    - Add fiat onramp, e.g. Alchemy Pay

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
  `bossw` from scripts, a CLI, or an MCP server over the appserver API.
- [Simnet Development Guide](docs/simnet-dev.md) — local regtest harnesses and
  running the wallet against them.
- [Desktop UI Development Guide](docs/desktop-ui-dev.md) — developing the
  desktop wallet UI.

## Contributing

Boss Wallet is in heavy flux right now; large surfaces are being removed,
rewritten, or first introduced. Check with @buck54321 or @JoeGruffins if you're
interested in helping out.

## Heritage

Boss Wallet descends from Bison Wallet, which itself was developed alongside
DCRDEX. The non-trading wallet, atomic-swap settlement, EVM relay, and Monero
adaptor-signature code are retained from that lineage; the credit for that
work belongs to the DCRDEX / Bison Wallet contributors.
