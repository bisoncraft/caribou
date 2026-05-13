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

The following work is intended but not yet (or only partially) in the tree.
Each line corresponds to a Claude agent definition under `.claude/agents/`,
which is the day-to-day specification for the work.

- **Desktop UI rewrite.** A new React + TypeScript desktop UI lives at
  `wallet/appserver/desktop/` and is served by `wallet/appserver/` (Go).
  Builds only into the `!mobile` binary. Responsive down to 1024px wide.
  Tracked by the `desktop-ui` agent. The directory is not yet checked in.
- **Mobile app.** A React Native app under `wallet/mobile/` that embeds the
  Go wallet core in-process via `gomobile bind`, exposes it over a
  localhost-only token-authenticated HTTP listener, and reuses the same
  appserver REST/WebSocket API as the desktop UI. iOS and Android. Builds
  only into the `mobile` binary. Tracked by the `mobile-ui` agent. The
  directory is not yet checked in.
- **NEAR wallet.** `wallet/asset/near/` is in early-stage development.
  Account-based, ed25519, Borsh-encoded transactions, NEAR JSON-RPC. Swap
  support is intended to be implemented via NEAR Intents — signed intent
  messages submitted to the `intents.near` contract, fulfilled by off-chain
  solvers — mapped onto the existing `Swap` / `AuditContract` / `Redeem` /
  `Refund` surface of `asset.Wallet`. Tracked by the `near-wallet-dev` agent.
- **Pure-Go Electrum backend.** A new `chain.Interface` implementation under
  `wallet/utxowallet/chain/` backed by an Electrum protocol client, as an
  alternative to the existing Neutrino SPV backend. Tracked by the
  `utxowallet-dev` agent.

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

## Contributing

Boss Wallet is in heavy flux right now; large surfaces are being removed,
rewritten, or first introduced. Check with @buck54321 or @JoeGruffins if you're
interested in helping out.

## Heritage

Boss Wallet descends from Bison Wallet, which itself was developed alongside
DCRDEX. The non-trading wallet, atomic-swap settlement, EVM relay, and Monero
adaptor-signature code are retained from that lineage; the credit for that
work belongs to the DCRDEX / Bison Wallet contributors.
