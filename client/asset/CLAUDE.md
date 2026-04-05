# client/asset — Per-Chain Wallet Implementations

This is the primary development area for the wallet. Each subdirectory implements the `asset.Wallet` interface defined in `dex/asset.go` for a specific blockchain.

## What belongs here
- Send, receive, balance, fee estimation for each chain
- Atomic swap contract interactions: `Initiate`, `Audit`, `Redeem`, `Refund`, `FindRedemption`
- Transaction broadcasting and confirmation tracking
- Address generation and key management per chain

## Supported chains
btc, eth (and polygon), dcr, ltc, zec, xmr, bch, doge, firo, dash, dgb, zcl

## What does NOT belong here
- Order management, lot sizes, or DEX rate logic — those live in the (out-of-scope) DEX layer
- Server-side asset backends — those are in `server/asset/` which is out of scope

## Interface contract
Every wallet must satisfy `asset.Wallet` (and optionally the extended interfaces like `asset.Sweeper`, `asset.Accelerator`, etc.) from `dex/asset.go`. When adding methods, check `dex/asset.go` first for existing optional interfaces before defining new ones.
