# Electrum wrapper dedup recommendations

Recommendations after reviewing `docs/electrum-wrapper-dedup-plan.md` against
the current BTC/LTC Electrum wrapper code, with the explicit assumption that this
is a ground-up wallet architecture cleanup:

- No mainnet wallet history needs to be preserved.
- No restore needs to support birthdays older than the new wallet's launch
  window.
- Legacy scope behavior is not a compatibility contract.
- Compile/test fallout from moving misplaced BTC package helpers is acceptable
  if it drives a cleaner package boundary.

## Summary

Proceed with the shared Electrum wrapper plan. The architecture problem is real:
`wallet/asset/btc/electrum.go` and `wallet/asset/ltc/electrum.go` duplicate the
same asset-wallet wrapper, while LTC imports BTC as an accidental shared UTXO
library.

The plan should be framed as an architecture cleanup with intentional behavior
consolidation, not as a strict no-behavior-change refactor. In particular, the
shared implementation should make LTC inherit the BTC Electrum recovery path.

## Recommended adjustments

### 1. State the behavior change plainly

The existing BTC Electrum wrapper parses `recoverywindow` and passes it to
`wallet.NewLoader`. The LTC Electrum wrapper does not; it still passes `0`.

The plan should say:

> This refactor intentionally ports the BTC Electrum recovery path to LTC by
> moving recovery-window handling into the shared wrapper.

That is a good outcome, but it should not be hidden under "not a behavior
change."

### 2. Make BIP84-only recovery the product model

Given the new-wallet assumptions, BIP84-only recovery is the right simplification.
There is no need to preserve broad legacy scanning over every active scope.

Recommended direction:

- Treat BIP84 as the only generation/recovery scope for these Electrum wallets.
- Remove or bypass multi-scope scan behavior for the Electrum recovery path.
- Keep both external and internal branches.
- Prefer explicit configuration in the shared wrapper: `Purpose: 84` or
  `RecoveryScope: wallet.ScopeForPurpose(84)`.

This turns recovery from "scan every historical thing btcwallet might know
about" into "recover the only address family this product generates."

### 3. Move neutral UTXO infrastructure out of `asset/btc`

The BTC package is currently acting as a shared UTXO library. That should end.

Move these to a neutral package under `wallet/utxowallet`:

- `BadgerTxDB`
- `ExtendedWalletTx`
- `ToCoinID`
- exported coin ID decode helper

The exact package names matter less than the dependency direction. LTC should
not import BTC for Electrum tx history, coin IDs, or fee sizing.

Suggested homes:

- `wallet/utxowallet/txhistory` for `BadgerTxDB` and `ExtendedWalletTx`
- `wallet/utxowallet/coinid` or `wallet/utxowallet/utxoid` for UTXO coin ID
  encoding/decoding

### 4. Use the existing tx size package

Do not create a second home for transaction size constants. There is already
`wallet/utxowallet/wallet/txsizes`.

For the shared Electrum wrapper's standard send fee calculation, import from
`txsizes` or extend it with any missing constants. Keep BTC-specific swap sizing
in BTC only if it is still specific to the BTC asset wrapper.

### 5. Expand `ChainConfig`

The proposed `ChainConfig` is close, but too small for a clean shared wrapper.
It should carry enough chain-specific state that the new package does not need
to import `asset/btc` or `asset/ltc`.

Recommended shape:

```go
type ChainConfig struct {
    Symbol       string
    Chain        bisonwire.Chain
    Servers      map[string]*electrum.ServerAddr
    DefaultNet   string
    ChainParams  *chaincfg.Params
    NetParams    *netparams.ChainParams
    Info         *asset.WalletInfo
    Purpose      uint32
    DBTimeout    time.Duration
    LogWallet    func(walletDir string) error
    ConfirmationsRequired uint32
}
```

`Purpose` can be turned into `wallet.ScopeForPurpose(Purpose)` after the wallet
is opened, which keeps the config simple and coin-type-aware.

### 6. Keep thin asset packages

After extraction, `wallet/asset/btc` and `wallet/asset/ltc` should only own:

- asset registration
- chain params and net params
- wallet definitions
- Electrum server defaults
- seeded wallet creation entry point
- construction of the shared Electrum wrapper config

The shared wrapper should own:

- `asset.Wallet` implementation
- `asset.Authenticator` implementation
- connection lifecycle
- Electrum network/client lifecycle
- tx history syncing
- pending tx tracking
- recovery-window parsing/use
- deposit/send/address validation logic

### 7. Sequence the migration to keep failures useful

Suggested implementation order:

1. Consolidate the duplicate BTC import in `wallet/asset/ltc/ltc.go`.
2. Move coin ID encode/decode to a neutral UTXO package and update callers.
3. Move `BadgerTxDB` and `ExtendedWalletTx` to a neutral tx-history package and
   update callers.
4. Switch Electrum standard send fee sizing to `wallet/utxowallet/wallet/txsizes`.
5. Create the shared Electrum wrapper package.
6. Rebuild BTC Electrum as a thin config wrapper.
7. Rebuild LTC Electrum as a thin config wrapper.
8. Delete duplicated wrapper code.
9. Collapse Electrum recovery to the BIP84 product model.

This order separates mechanical relocation from behavioral wrapper extraction,
while still allowing aggressive cleanup.

### 8. Verification

Minimum verification:

- BTC Electrum harness
- LTC Electrum harness
- BTC Electrum recovery test
- New LTC Electrum recovery test using the same shared path
- Non-harness package tests for:
  - `wallet/asset/btc`
  - `wallet/asset/ltc`
  - `wallet/utxowallet/...`

Because this is a package-boundary refactor, compiler errors are expected and
useful. Treat them as the checklist for removing accidental BTC dependencies.

## Bottom line

The existing plan is directionally right. The main change is framing: this is not
a careful compatibility-preserving dedup. It is an opportunity to make the
Electrum wallet architecture match the intended product:

- one shared UTXO Electrum asset-wallet wrapper,
- BTC and LTC as thin chain configs,
- BIP84-only generation and recovery,
- no LTC dependency on BTC for Electrum internals.
