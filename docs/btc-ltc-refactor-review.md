# BTC/LTC refactor review

Date: 2026-06-20

Scope: BTC and LTC only. This review treats the wallet as new, with no need to preserve old user data or backwards compatibility for prior wallet deployments.

## Summary

Claude's current BTC/LTC work looks coherent for this phase.

The tx history move from `wallet/asset/btc` into `wallet/utxowallet/txhistory` is directionally correct for shared UTXO wallet code. The BTC and LTC call sites I checked now depend on the shared package instead of BTC owning shared transaction-history storage.

The shared `wallet/utxowallet/coinid` package is also a good extraction. BTC and LTC both use the same 36-byte UTXO coin ID format, so moving `ToCoinID` / `DecodeCoinID` out of BTC avoids unnecessary BTC-package coupling.

## Checks performed

Reviewed the active worktree diffs around:

- BTC asset wallet changes
- BTC Electrum wallet changes
- LTC Electrum wallet changes
- shared UTXO wallet recovery/rescan hooks
- shared tx history package
- shared coin ID package

Ran targeted uncached tests:

```sh
go test -count=1 ./wallet/utxowallet/txhistory ./wallet/utxowallet/wallet ./wallet/utxowallet/chain ./wallet/asset/btc ./wallet/asset/ltc
```

Result: passed.

## BTC/LTC findings

No blocking BTC/LTC correctness issue found for the current phase.

The Electrum recovery scan code is present at the lower wallet layer, but production/core wiring for the `recoverywindow` setting is not complete yet. Given the current phase, I am treating that as expected unfinished wiring, not a defect in this review.

When that wiring becomes in scope, one behavior should be revisited: `recoverViaScan` errors are currently logged and sync continues. For an actual recovery flow, probe failure should probably be fatal so the wallet does not appear successfully recovered after only scanning already-known addresses.

## Non-scope notes

I intentionally ignored BCH and DCR concerns after scope was clarified to BTC/LTC only.

I also did not treat old tx history database location compatibility as a requirement, because this wallet can be considered new and without existing users.
