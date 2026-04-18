# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a new wallet based on **Bison Wallet**, without the DCRDEX trading/exchange functionality. The goal is a non-custodial, multi-chain wallet that retains atomic swap capability (for peer-to-peer trades) but removes the centralized exchange server, order books, market-making, and DEX protocol layer.

**Tatanka Mesh** has moved to a separate repository and is not part of this project.

### In scope
- Per-chain wallet implementations (`wallet/asset/`)
- Atomic swap execution: init, audit, redeem, refund (the settlement mechanics, not the trading layer)
- Wallet UI (`wallet/appserver/`)
- Wallet database (`wallet/db/`)
- EVM relay service (`dex/evmrelay/`)
- Shared cryptographic utilities and asset abstractions (`dex/encode`, `dex/encrypt`, `dex/keygen`, `dex/networks`, `dex/asset.go`)
- Monero adaptor signature primitives (`internal/adaptorsigs/`)

### Out of scope — do not modify or extend
- `server/` — DCRDEX server (epoch management, order matching, PostgreSQL DB)
- `tatanka/` — Mesh network (separate repo)
- `wallet/mm/libxc/` — CEX exchange connectors (Binance, Coinbase, MEXC, Bitget); do not touch
- `wallet/orderbook/` — DEX order book management
- `wallet/comms/` — WebSocket connectivity to DEX servers
- `dex/order/` — DEX order types
- `dex/msgjson/` — DEX wire protocol messages
- `dex/market.go` — Market/lot-size definitions

Note: `wallet/mm/` top-level files (mm.go, config.go, exchange_adaptor.go, event_log.go, etc.) are **in scope for removal** as part of the DEX trading layer cleanup. Delete or gut them to unblock `core/types.go` cleanup.

## Common Commands

### Build
```bash
# Build the wallet binary
cd wallet/cmd/bisonw && go build -o bisonw

# Build the desktop wallet
cd wallet/cmd/bisonw-desktop && go build -o bisonw-desktop

# Build the EVM relay service
cd dex/evmrelay/cmd/evmrelay && go build -o evmrelay

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
go test -race -short ./wallet/asset/btc/...

# Build with special tags to verify compilation
go test -c -o /dev/null -tags live ./wallet/webserver
go test -c -o /dev/null -tags harness ./wallet/asset/eth
```

### Lint
```bash
golangci-lint -c ./.golangci.yml run
```

### Web UI (required before testing webserver package)
```bash
cd wallet/webserver/site && npm ci && npm run build
```

## Architecture

### `wallet/` — Wallet Application
Entry point: `wallet/cmd/bisonw/main.go`.

- **`wallet/asset/`** — Per-chain wallet implementations (btc, eth, dcr, ltc, zec, xmr, bch, doge, firo, polygon, dash, dgb, zcl). Each implements `asset.Wallet` from `dex/asset.go`. **Primary development area.**
- **`wallet/core/`** — Central wallet logic: swap lifecycle (init/audit/redeem/refund), account and key management, send/receive. DEX connectivity portions are being removed.
- **`wallet/webserver/`** — Embedded web UI (HTML/CSS/JS under `site/`)
- **`wallet/db/`** — BoltDB-backed wallet database

### `dex/` — Shared Utilities (subset)
Only the non-trading parts are in scope: `asset.go` (wallet interface), `encode/`, `encrypt/`, `keygen/`, `networks/`, and general-purpose utilities. Avoid changes to `order/`, `msgjson/`, and `market.go`.

### `dex/evmrelay/` — EVM Relay Service
Manages Ethereum/EVM atomic swap contract interactions, fee estimation, and batch redemptions. Entry point: `dex/evmrelay/cmd/evmrelay/main.go`.

### `internal/` — Internal Utilities
- `adaptorsigs/` — Adaptor signature primitives (used for Monero atomic swaps)
- `cmd/xmrswap/` — XMR atomic swap tooling

## Key Design Patterns

- **Asset interface**: Every supported chain implements `asset.Wallet` from `dex/asset.go`. Adding a new chain means implementing this interface and registering it under `wallet/asset/`.
- **Atomic swaps**: Settlement uses on-chain atomic swaps (init → audit → redeem → refund). This mechanism is retained; what is removed is the DEX trading layer that drives it.
- **Build tags**: `live`, `harness`, `electrumlive`, `rpclive`, `systray` gate tests requiring real network access, running nodes, or platform-specific support.
- **Embedded web UI**: The frontend is bundled via `go:embed`; run `npm run build` in `wallet/webserver/site/` before running webserver tests.
