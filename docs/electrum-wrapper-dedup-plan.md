# Deduplicating the BTC/LTC electrum wallet wrappers

Plan to collapse the two near-identical electrum `asset.Wallet` wrappers
(`wallet/asset/btc/electrum.go`, `wallet/asset/ltc/electrum.go`) into one shared
implementation in `wallet/utxowallet`, parameterized by a small per-chain
config. Companion to the electrum sync/recovery and address-watching plans;
this is a structural refactor, not a behavior change.

## Status

Proposed. Not started. Phase 1a + 1b of the recovery work
(`electrum-sync-recovery-plan.md`) are implemented in `btc` only; this dedup is
what makes LTC inherit recovery (and everything else) automatically instead of
needing the change mirrored.

This is an **architecture cleanup with intentional behavior consolidation**, not
a compatibility-preserving refactor. Framing per a second review and the
project's new-wallet assumptions (no mainnet history to preserve, no foreign
seeds, legacy multi-scope behavior is not a contract):

- It **intentionally ports the BTC electrum recovery path to LTC** by moving
  recovery-window handling into the shared wrapper. LTC currently passes `0` to
  `NewLoader` unconditionally; afterward an LTC restore can opt in via
  `recoverywindow` exactly like BTC. That is a behavior addition, stated plainly,
  not hidden under "no behavior change."
- It makes **BIP84-only the product recovery model** (see §5), not a perf hack.

## 1. The problem

`electrumWallet` (btc, 840 lines) and `ltcElectrumWallet` (ltc, 805 lines)
expose the **identical** 24-method set — `Balance`, `Connect`, `DepositAddress`,
`Send`, `SyncStatus`, `syncTxHistory`, `scanNewTransactions`,
`updatePendingTransactions`, `Unlock`/`Lock`/`Locked`, … — and the bodies are
byte-for-byte parallel. They differ only in a small chain-config surface. So a
change like the `recoverywindow` plumbing has to be written twice, and "remember
to mirror it to LTC" is a recurring tax.

Two related smells fall out of the same history (this tree was forked from
DCRDEX):

- **`ltc` depends on `btc`.** `ltc/electrum.go` reuses `btc`'s `BadgerTxDB`,
  `ExtendedWalletTx`, tx-size consts, and `ToCoinID` via
  `dexbtc "…/wallet/asset/btc"`. The `btc` package is the de-facto shared
  utxo-electrum library — an accident, not a design.
- **`ltc.go` imports `btc` twice** — line 18 as `btc`, line 19 as `dexbtc` —
  same path, two names, referenced both ways across the package. Pure cruft.

## 2. What actually differs (the chain-config surface)

Everything chain-specific reduces to a handful of values:

| Concern | btc | ltc |
| --- | --- | --- |
| bisonwire chain | `ChainBTC` | `ChainLTC` |
| electrumx servers | `btcElectrumServers` | `ltcElectrumServers` |
| chain params | btcsuite `chaincfg.Params` | LTC params |
| netparams | `btcNetParams` | `ltcNetParams` |
| wallet info | `WalletInfo` (Bitcoin) | `WalletInfo` (Litecoin) |
| BIP id | 0 | LTC `BipID` |
| address / recovery scope | `ScopeForPurpose(84)` | `ScopeForPurpose(84)` |

Note the scope is **BIP84 in both** — so scope restriction (see §5) is uniform.
MWEB is *not* in this table: it lives in the `bisonwire` decoder under the chain
client, transparent to the wrapper.

## 3. Home: `wallet/utxowallet`

`utxowallet` is the neutral utxo-wallet library — it already hosts `chain`,
`wallet`, `electrum`, `bisonwire`, `spv`, `waddrmgr`, `wtxmgr`. The shared
electrum asset-wallet belongs alongside them, not in a new package and not in
`btc`.

Verified cycle-free: `wallet/asset` does not import `utxowallet`, and
`utxowallet` does not import `wallet/asset` today, so a new
`utxowallet → wallet/asset` edge (for `asset.Wallet`, `WalletConfig`, `Coin`) is
clean.

Proposed package: **`wallet/utxowallet/electrumwallet`** (name TBD).

## 4. Design

A single shared electrum `asset.Wallet`, composition over a per-chain config.
The config must carry enough chain-specific state that the shared package never
imports `asset/btc` or `asset/ltc`:

```go
type ChainConfig struct {
    Chain         bisonwire.Chain                 // ChainBTC / ChainLTC (also yields the symbol)
    Servers       map[string]*electrum.ServerAddr // per-net electrumx endpoints
    ChainParams   *chaincfg.Params
    NetParams     *netparams.ChainParams
    Info          *asset.WalletInfo               // name, units
    BipID         uint32
    DBTimeout     time.Duration
    Confirmations uint32                          // confTxFinality
    LogWallet     func(walletDir string) error    // per-asset logger setup (logNeutrino)
}
```

Deliberately *not* in the config:

- **Symbol** — already encoded by `Chain`; derive it from there for logging.
- **DefaultNet / server-map fallback** — the network is always provided, so a
  missing server entry is a real error, not a silent mainnet fallback.
- **Purpose / scope** — these wallets are **BIP84-only**. The shared wrapper
  uses `ScopeForPurpose(84)` directly. If a future asset needs another scope we
  add it then; we don't carry a knob for a single value.

The 24 methods move onto the shared type once. `btc` and `ltc` shrink to: build
a `ChainConfig`, a `createElectrumWallet` entry point, and driver registration.
`recoveryWindow` (from the `recoverywindow` setting) is read in exactly one
place, so LTC gets recovery for free.

## 5. Relocations and fold-ins

- **Relocate the genuinely-shared helpers out of `btc` into neutral utxowallet
  sub-packages** (exact names matter less than the dependency direction —
  nothing in LTC's electrum path should import `asset/btc`):
  - `BadgerTxDB` + `ExtendedWalletTx` → e.g. `wallet/utxowallet/txhistory`.
  - `ToCoinID` + the coin-ID decode helper → e.g. `wallet/utxowallet/coinid`.
  - This touches `btc`'s SPV/RPC wallet too (`btc.go`, `txdb.go`, `types.go`,
    `redemption_finder.go` use these); the compiler is the checklist.
- **Use the existing tx-size package, don't relocate a copy.**
  `wallet/utxowallet/wallet/txsizes` already defines the P2WPKH/RedeemP2WPKH
  sizes; `btc/script.go` carries a *second, duplicate* copy
  (`MinimumTxOverhead`, `P2WPKHOutputSize`, `RedeemP2WPKHInputSize`, …). The
  shared wrapper's `StandardSendFee`/`Send` sizing imports from `txsizes`;
  extend `txsizes` with any missing consts (`MinimumTxOverhead`, the overhead
  primitives). BTC-only *swap* sizing stays in `btc` (it's swap baggage being
  shed anyway).
- **BIP84-only recovery is the product model, folded in here.** Given no foreign
  seeds, recovery scans only the generation scope — BIP84 — both branches.
  `recoverViaScan` (shared wallet pkg) uses `ScopeForPurpose(84)` directly
  instead of iterating every `ActiveScopedKeyManagers()` scope; the
  non-default-scope/internal-only legacy guard goes away. This is not "scan
  everything btcwallet might know" — it's "recover the one address family this
  product generates." Drops the ~200-probe scan (mostly empty BIP44/49/86) to
  BIP84-only, and lands once for both chains.
- **Relocate `defaultWalletBirthdayUnix` / `DefaultWalletBirthday` to
  utxowallet and set it to the launch date.** It's currently in `btc` (LTC uses
  `dexbtc.DefaultWalletBirthday`), so it's another accidental btc-shared const.
  Moving it neutral fits the dependency cleanup; setting it to ~launch (rather
  than 2021) means a default-birthday wallet never walks the whole chain on
  first sync (see the sync/recovery plan — this is what defers Phase 2). Once
  it's recent, the recovery harness test can drop its `now()-1h` birthday hack.

## 6. Migration

Ordered to separate mechanical relocation from behavioral extraction, so a
broken step is a small, obvious one:

1. **Independent quick win:** consolidate `ltc.go`'s double `btc` import to one
   name; standardize the alias across the ltc package. No behavior change.
2. Move the coin-ID encode/decode helpers to a neutral utxowallet package;
   update callers.
3. Move `BadgerTxDB` + `ExtendedWalletTx` to a neutral tx-history package;
   update callers (incl. btc SPV).
4. Switch the electrum wrappers' send-fee sizing to
   `wallet/utxowallet/wallet/txsizes` (extend it as needed); drop btc's
   duplicate sizing from the electrum path.
5. Create `utxowallet/electrumwallet` with the shared type + `ChainConfig`.
6. Rebuild `btc` electrum as a thin config wrapper over it.
7. Rebuild `ltc` electrum as a thin config wrapper; delete its `dexbtc` electrum
   usage.
8. Delete the duplicated wrapper code.
9. Collapse electrum recovery to the BIP84 product model (`ScopeForPurpose(84)`
   in `recoverViaScan`).
10. Verify (see §8).

## 7. Scope caveat — `ltc → btc` is not fully severed

This removes the *electrum* contribution to the `ltc → btc` dependency, but
`ltc/params.go` and `ltc/spv.go` still import `btc` for the *SPV* wallet's shared
code. Fully cutting `ltc → btc` also wants the SPV wrappers deduped the same way
(shared SPV wallet in utxowallet) — a separate, larger effort. The double-import
consolidation in §6.1 is still worth doing now because the SPV path keeps that
import alive.

## 8. Risks / verification

- **Params type:** confirm btc (btcsuite `chaincfg`) and LTC params unify under
  one `*chaincfg.Params` (the `netparams` abstraction suggests yes); reconcile
  if not.
- **MWEB:** confined to `bisonwire`/chain-client; the wrapper stays
  chain-agnostic — but re-run the LTC suite to be sure the MWEB-stripped decode
  path is unaffected.
- **Helper relocation blast radius:** `BadgerTxDB` et al. are referenced by
  `btc`'s SPV wallet too; the move must update those, kept honest by the
  compiler.
- **Verification:** BTC harness 8/8, LTC harness 7/7, plus a new LTC recovery
  test (now trivial — same shared path), non-harness suites, golangci-lint.
