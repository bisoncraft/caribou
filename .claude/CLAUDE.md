# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a new wallet based on **Bison Wallet**, without the DCRDEX trading/exchange functionality. The goal is a non-custodial, multi-chain wallet that retains atomic swap capability (for peer-to-peer trades) but removes the centralized exchange server, order books, market-making, and DEX protocol layer.

**Tatanka Mesh** has moved to a separate repository and is not part of this project.

### In scope
- Per-chain wallet implementations (`client/asset/`)
- Atomic swap execution: init, audit, redeem, refund (the settlement mechanics, not the trading layer)
- Wallet UI (`client/webserver/`)
- Wallet database (`client/db/`)
- EVM relay service (`evmrelay/`)
- Shared cryptographic utilities and asset abstractions (`dex/encode`, `dex/encrypt`, `dex/keygen`, `dex/networks`, `dex/asset.go`)
- Monero adaptor signature primitives (`internal/adaptorsigs/`)

### Out of scope — do not modify or extend
- `server/` — DCRDEX server (epoch management, order matching, PostgreSQL DB)
- `tatanka/` — Mesh network (separate repo)
- `client/mm/` — Market-making and arbitrage
- `client/orderbook/` — DEX order book management
- `client/comms/` — WebSocket connectivity to DEX servers
- `dex/order/` — DEX order types
- `dex/msgjson/` — DEX wire protocol messages
- `dex/market.go` — Market/lot-size definitions

## Common Commands

### Build
```bash
# Build the wallet binary
cd client/cmd/bisonw && go build -o bisonw

# Build the desktop wallet
cd client/cmd/bisonw-desktop && go build -o bisonw-desktop

# Build the EVM relay service
cd evmrelay/cmd/evmrelay && go build -o evmrelay

# Cross-platform release packaging
./pkg.sh
```

### Test
```bash
# Run all tests
./run_tests.sh

# Fast unit tests with race detection
go test -race -short ./...

# Test a specific asset package
go test -race -short ./client/asset/btc/...

# Build with special tags to verify compilation
go test -c -o /dev/null -tags live ./client/webserver
go test -c -o /dev/null -tags harness ./client/asset/eth
```

### Lint
```bash
golangci-lint -c ./.golangci.yml run
```

### Web UI (required before testing webserver package)
```bash
cd client/webserver/site && npm ci && npm run build
```

## Architecture

### `client/` — Wallet Application
Entry point: `client/cmd/bisonw/main.go`.

- **`client/asset/`** — Per-chain wallet implementations (btc, eth, dcr, ltc, zec, xmr, bch, doge, firo, polygon, dash, dgb, zcl). Each implements `asset.Wallet` from `dex/asset.go`. **Primary development area.**
- **`client/core/`** — Central wallet logic: swap lifecycle (init/audit/redeem/refund), account and key management, send/receive. DEX connectivity portions are being removed.
- **`client/webserver/`** — Embedded web UI (HTML/CSS/JS under `site/`)
- **`client/db/`** — BoltDB-backed wallet database

### `dex/` — Shared Utilities (subset)
Only the non-trading parts are in scope: `asset.go` (wallet interface), `encode/`, `encrypt/`, `keygen/`, `networks/`, and general-purpose utilities. Avoid changes to `order/`, `msgjson/`, and `market.go`.

### `evmrelay/` — EVM Relay Service
Manages Ethereum/EVM atomic swap contract interactions, fee estimation, and batch redemptions. Entry point: `evmrelay/cmd/evmrelay/main.go`.

### `internal/` — Internal Utilities
- `adaptorsigs/` — Adaptor signature primitives (used for Monero atomic swaps)
- `cmd/xmrswap/` — XMR atomic swap tooling

## Key Design Patterns

- **Asset interface**: Every supported chain implements `asset.Wallet` from `dex/asset.go`. Adding a new chain means implementing this interface and registering it under `client/asset/`.
- **Atomic swaps**: Settlement uses on-chain atomic swaps (init → audit → redeem → refund). This mechanism is retained; what is removed is the DEX trading layer that drives it.
- **Build tags**: `live`, `harness`, `electrumlive`, `rpclive`, `systray` gate tests requiring real network access, running nodes, or platform-specific support.
- **Embedded web UI**: The frontend is bundled via `go:embed`; run `npm run build` in `client/webserver/site/` before running webserver tests.
