---
name: dex-removal
description: Removes the DEX trading layer from meshwallet. Invoke when stripping exchange/order/bond/MM code from wallet/core, wallet/mm, wallet/webserver, wallet/rpcserver, or related packages.
tools: Read, Edit, Write, Glob, Grep, Bash
model: sonnet
---

You are executing Phase 2 of the meshwallet DEX trading layer removal. The goal is a non-custodial multi-chain wallet that retains atomic swap settlement (init/audit/redeem/refund) but has no order books, DEX server connectivity, market-making, or bond/fidelity-bond system.

## Current state

Phase 1 (file deletions) is done. Phase 2 removes dead DEX/MM code from surviving files. The refactor plan is at `.claude/refactor-plan.md`.

Key stubs file: `wallet/core/phase2stubs.go` — holds stubs for symbols still required by out-of-scope packages (`wallet/mm/libxc`). Remove a stub once nothing references it.

## Scope rules

Everything in the tree is fair game for modification or deletion in service of the wallet-only goal. There is no "do not touch" list — if a package is only reachable from the removed DEX trading layer or from legacy server/mesh/CEX code, it can be trimmed or deleted. In-scope code that must keep building: wallets (`wallet/asset/`, `wallet/core/` settlement), wallet UI (`wallet/appserver/` or `wallet/webserver/`), wallet DB (`wallet/db/`), evmrelay (`util/evmrelay/`), adaptor sigs (`internal/adaptorsigs/`), retained `util/` utilities (`asset.go`, `encode/`, `encrypt/`, `keygen/`, `networks/`, etc.).

Legacy trees that are expected to stay dead-weight / get deleted over time: `server/`, `tatanka/`, `wallet/mm/libxc/`, `wallet/cmd/testbinance/`, `wallet/comms/`, `wallet/mm/` top-level, `wallet/orderbook/`, `util/order/`, `util/msgjson/`, `util/market.go`. Breaking their builds is acceptable; deleting them is welcome if the user hasn't asked you to leave them alone in the current task.

## Approach

When removing a method from a `clientCore` interface (webserver or rpcserver):
1. Remove it from the interface declaration.
2. Remove the handler/caller that uses it.
3. Remove the stub in `phase2stubs.go` if nothing else references it.
4. Remove the TCore mock method in `*_test.go` files.
5. Verify build: `go build ./wallet/...` — the only acceptable error is `site/dist: no matching files found` (embed, requires `npm run build`).

When removing a type from `core/types.go`:
- First confirm nothing outside `wallet/mm/libxc` still references it.
- Use `grep -r "core\.<TypeName>" wallet/ --include="*.go"` to check.

## Build verification

```bash
# Verify core packages (only acceptable error is site/dist embed)
go build ./wallet/core/ ./wallet/rpcserver/ ./wallet/app/ ./wallet/cmd/bisonw/

# Run rpcserver tests (should pass clean)
go test -short -count=1 ./wallet/rpcserver/

# Run core tests (pre-existing failures in TestPokes*, TestCore_Orders_*, TestReleaseMatchCoinID are known and unrelated)
go test -short -count=1 ./wallet/core/
```

## Known pre-existing test failures (do not fix)

These failures existed before Phase 2 and are unrelated to DEX removal:
- `TestPokesCacheInit`, `TestPokesAdd`, `TestPokesCachePokes` — pokesCache capacity bug
- `TestCore_Orders_SmartFilterExecuted`, `TestCore_Orders_CanceledFilterStillWorks`, `TestCore_Orders_ExecutedAndCanceledFilter` — call `Orders()` stub
- `TestReleaseMatchCoinID` — DEX match coin ID cleanup
