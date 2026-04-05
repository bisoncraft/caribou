# Refactor Plan: Remove DEX Trading Layer

Goal: strip DCRDEX exchange/trading functionality from Bison Wallet, leaving a
non-custodial multi-chain wallet that retains atomic swap execution
(init/audit/redeem/refund) but has no order books, DEX server connectivity,
market-making, or bond/fidelity-bond system.

---

## Phase 1 — Delete out-of-scope files, keep build green (DONE)

Deleted from `client/core/`:

| File | What it contained |
|------|-------------------|
| `bond.go` | DEX fidelity bond rotation, PostBond, WithdrawBond, bond key management |
| `bookie.go` | Order book subscriber, price feed, market suspension handlers |
| `account.go` | DEX account import/export/management |
| `mesh.go` | Tatanka mesh network integration |
| `simnet_trade.go` | Simnet harness test helpers (build tag `harness`) |
| `certs.go` | TLS certificates for known DEX servers |
| `account_test.go` | Tests for account.go |

Compensating changes:

- **`client/core/phase2stubs.go`** (new): stub implementations for every symbol
  removed above that is still referenced by surviving code. Stubs return errors
  or zero values; they exist only to satisfy the compiler while callers are
  removed in Phase 2.
- **`client/core/core.go`**: removed struct fields, imports, and call-sites that
  directly referenced the deleted files (books map, mesh fields, bondXPriv,
  bookie note handlers, watchBonds goroutine, etc.).
- **`client/core/core_test.go`**: gutted ~15 test functions with `t.Skip` where
  the test body depended on deleted functionality; removed book-setup code from
  shared helpers.

---

## Phase 2 — Remove DEX trading code from core.go

`phase2stubs.go` lists the surviving callers. Work through them package by
package, removing dead call-sites until each stub can be deleted.

### 2a. `client/core/core.go` — trading functions

These functions exist purely to support DEX order matching and can be deleted
outright (or reduced to error stubs if the webserver/rpcserver interface still
requires them):

- `Trade`, `TradeAsync`, `prepareForTradeRequestPrep`, `createTradeRequest`,
  `prepareTradeRequest`, `prepareMultiTradeRequests`, `sendTradeRequest`
- `MultiTrade`
- `PreOrder`
- `Cancel`, `sendCancelOrder`, `tryCancel`, `tryCancelTrade`
- `MaxBuy`, `MaxSell`, `MaxFundingFees`
- `AccelerateOrder`, `AccelerationEstimate`, `PreAccelerateOrder`
- `GaslessRedeemCalldata`, `ValidateGaslessRedeem`, `SubmitGaslessRedeem`
- `TradingLimits`
- `Orders`, `Order` (DEX order history — separate from swap/tx history)
- `dbTrackers`, `loadDBTrades`, `resumeTrade`, `resumeTrades`,
  `schedTradeTick`, `tryFastSwap`, `tryFastRedeem`, `findTrade`,
  `findActiveOrder`
- `authDEX` (bond-posting sections; the auth handshake itself may be needed if
  DEX connectivity is kept for swap coordination — evaluate)
- `startDexConnection`, `handleReconnect` (book re-subscription sections)
- `upgradeConnection` (already removed `subPriceFeed` call)
- `checkEpochResolution` (already removed bookie.send block)

### 2b. `client/core/core.go` — DEX connection / server management

Evaluate whether any of these are needed for atomic swap coordination without
a full trading session:

- `GetDEXConfig`, `AddDEX`, `DiscoverAccount` — needed only if users still
  register with a DEX server to initiate swaps; remove if swaps are driven
  purely P2P
- `PostBond`, `BondsFeeBuffer`, `UpdateBondOptions`, `WithdrawBond` — remove
  (bonds are a trading-layer concept)
- `UpdateCert`, `UpdateDEXHost` — remove if DEX server connections are dropped

### 2c. `client/core/types.go` — dead types

After trading functions are removed, delete or trim:

- `TradeForm`, `MultiTradeForm`, `MultiTradeResult`, `InFlightOrder`
- `OrderEstimate`, `MaxOrderEstimate`
- `PostBondForm`, `PostBondResult`, `BondOptionsForm`
- `PreAccelerate`, `GaslessRedeemCalldataResult`
- `OrderFilter`, `Order` (DEX order type — distinct from swap records)
- Bond-related fields in `Exchange`, `Account`, `User`

### 2d. `client/webserver` — remove trading routes

HTTP/WebSocket handlers that drive the trading UI:

- `/api/trade`, `/api/tradeasync`, `/api/cancel`, `/api/multiorder`
- `/api/preorder`, `/api/maxbuy`, `/api/maxsell`
- `/api/accelerateorder`, `/api/preaccelerate`, `/api/accelerationestimate`
- `/api/postbond`, `/api/updatebondoptions`, `/api/withdrawbond`,
  `/api/bondsfeeBuffer`
- `/api/discoveracct`, `/api/getdexconfig`, `/api/adddex`
- `/api/orders`, `/api/order/:oid`
- Order-book WebSocket subscription handler in `client/websocket/`
- Update `clientCore` interface to remove the methods above

### 2e. `client/rpcserver` — remove trading RPC commands

Mirror of 2d for the JSON-RPC server; remove command handlers and trim the
`clientCore` interface.

### 2f. `client/db` — evaluate order/bond tables

The BoltDB schema stores DEX orders and bonds. Decide whether to:
- Drop those buckets entirely (breaking change for existing users)
- Keep the schema but stop writing new records
- Migrate to a simpler swap-record-only schema

### 2g. Delete phase2stubs.go

Once all callers in 2a–2f are removed, `phase2stubs.go` should have no
remaining references and can be deleted. Confirm with:

```
grep -rn "phase2" client/
```

(The file has no special marker; verify by attempting to delete it and
checking that `go build ./client/...` still passes.)

---

## Out of scope (do not modify)

- `server/` — DEX matching server
- `tatanka/` — Mesh network (separate repository)
- `client/mm/` — Market-making
- `client/orderbook/` — Order book management
- `client/comms/` — DEX WebSocket connectivity
- `dex/order/`, `dex/msgjson/`, `dex/market.go`
