# Key-scope model: one scope per chain

Design note for how the in-tree UTXO wallet (`wallet/utxowallet`) creates and
uses HD key scopes. It applies to the electrum and neutrino backends via the
shared `wallet.Wallet` and the `btc`/`ltc`/`dash` asset packages.

## Model

Each chain declares the BIP-43 key purposes it uses in
`netparams.ChainParams.KeyPurposes []uint32`, and the wallet creates exactly
one key scope per configured purpose — nothing else. The zeroth entry is the
primary scope, used for receiving, change, and gap-limit recovery.

- BTC and LTC — `KeyPurposes = {84}`: BIP-84 native segwit (P2WPKH).
- Dash — `KeyPurposes = {44}`: BIP-44 legacy P2PKH (no SegWit).

This replaces the upstream btcwallet behavior of creating the full
BIP-44/49/84/86 default scope set on every wallet, and it replaces the earlier
single-`AddrPurpose` field.

## Rationale

Per-asset wallet seeds are derived as `BLAKE256(appSeed || assetID)`
(`core.AssetSeedAndPass`). That digest is not a BIP-39/SLIP-0132 seed any other
wallet could produce, so the wallet only ever restores wallets it created
itself, and only ever derives the one address type each chain is configured
for. The other three default scopes could therefore never hold funds. Creating
them was pure cost:

- unused scoped-key managers and account rows persisted in every wallet
  database, and
- on the neutrino backend, `recoveryWindow`-sized lookahead address sets per
  unused scope loaded into the compact-filter match set during a recovery scan
  — filter-matching work that can never produce a hit.

`KeyPurposes` is a slice so a chain can list more than one purpose when it
genuinely uses more than one address type (e.g. adding BIP-86 taproot to BTC
later); today every chain lists exactly one.

## Coherence invariant

One configured set governs every stage of an address's lifecycle, so the stages
cannot drift:

- **Created** — `waddrmgr.Create` builds one scope per `KeyPurposes` entry at
  the chain's `HDCoinType`.
- **Derived** — `Wallet.PrimaryScope()` returns `KeyPurposes[0]` at the coin
  type; deposit addresses, change addresses, and the gap-limit recovery scan
  (`recoverViaScan`) all derive under it.
- **Classified** — `Manager.IsDefaultScope(scope)` is
  `scope.Coin == HDCoinType && KeyPurposes contains scope.Purpose`.
- **Credited** — `addRelevantTx` (`wallet/chainntfns.go`) only adds a credit
  for an output whose scope satisfies `IsDefaultScope`.

Because deposit, change, and recovery all use the primary scope, and crediting
admits exactly the `KeyPurposes` scopes, the outputs that are created, watched,
credited, and recovered are the same set. Opening a database created before this
change is safe: `loadManager` loads all persisted scopes, and non-configured
scopes fall through the `ForEachRelevantActiveAddress` internal-only branch, so
nothing already tracked becomes invisible.

## Change scope

Transaction construction defaults the change scope to `PrimaryScope()` rather
than a hardcoded scope. Previously the default was BIP-86 taproot (purpose 86),
which no chain creates under this model; on a single-scope wallet that default
produced `scope m/86'/…' not found`, which surfaced in max/subtract fee
estimation on Dash (`EstimateSendAllFee` → `changeScriptSize(nil, …)`). The
change-source helpers (`changeScriptSize`, `addrMgrWithChangeSource`, and the
nil-coin-select watch-only check in `txToOutputs`) and `EstimateSendTxFee` all
now resolve to the primary scope, which always exists. The change output's
address type and the size used to estimate its fee both come from the same
`ScopeAddrMapForCoin(HDCoinType)[PrimaryScope]` schema that `Create` persisted,
so estimate and on-chain output agree (P2WPKH for BTC/LTC, P2PKH for Dash).

## Removed and relocated surface

Removed from the `waddrmgr` package (they encoded a fixed coin-0 four-scope
assumption that no longer holds): the `KeyScopeBIP0044/0049Plus/0084/0086`
vars, `DefaultKeyScopes`, `ScopeAddrMap`, the package-level `IsDefaultScope`,
and `DefaultKeyScopesForCoin`. Retained: `ScopeAddrMapForCoin` (the
purpose→schema table, consulted by `Create` and change sizing) and
`KeyScopeBIP0049AddrSchema` (imported-key schema).

The coin-0 scope constants were legitimately BTC test fixtures, so they were
relocated to test scope (`waddrmgr/scopes_test.go`, `wallet/scopes_test.go`)
rather than deleted outright.

## Testing

The address-manager machinery still supports N scopes; only the default
creation set shrank. To keep coverage of the multi-scope machinery independent
of production's single-scope configuration, the `waddrmgr` and `wallet` test
binaries set `assets.BTCParams[*].KeyPurposes = {84, 49, 86, 44}` in an
`init()` (BIP-84 first, so `PrimaryScope` stays segwit). The single-scope
production path is exercised by `TestScopeForPurposeLTC` (coin type 2) and the
harness-gated Dash suites, including a send-fee estimate and a
restore-from-seed that must recover a spent-change balance.

## Known follow-ups

- `PrimaryScope()` indexes `KeyPurposes[0]` without a bounds guard; the previous
  `AddrPurpose == 0 → 84` fallback is gone. A chain added to
  `utxowallet/assets` that omits `KeyPurposes` panics at wallet open rather than
  failing cleanly. All shipped entries set it.
- `Create` looks a purpose's schema up in `ScopeAddrMapForCoin(...)`; a purpose
  outside {44,49,84,86} yields the zero-value schema (`PubKeyHash`) rather than
  an error. Only 44 and 84 are configured.
- `psbt.go`, `multisig.go`, and `import.go` still hardcode scope purposes 84/86
  (and 49). These are not wired into the shipped `asset.Wallet` surface, so they
  are unreachable today, but they would fail with `scope … not found` on a
  single-scope chain (Dash) if PSBT, multisig, or key import were exposed for
  it; they should move to `PrimaryScope()` when that happens.
- Moving the default change scope off taproot left the PSBT taproot-change
  verification path (`TaprootInternalKey`/`TaprootBip32Derivation`) unexercised;
  it should regain a case with an explicit taproot change scope when a chain
  configures BIP-86.
- Electrum gap-limit discovery (`recoverViaScan`) scans only `PrimaryScope()`,
  not every entry in `KeyPurposes`. This is correct while each chain configures
  a single purpose, but adding a second purpose (e.g. BIP-86) without extending
  discovery to scan every configured scope would silently fail to recover funds
  held under the non-primary scope.

## Related

The Dash electrum backend's missing gap-limit `RecoveryWindow`, discovered while
validating single-scope recovery, is covered in `utxosync-master-plan.md` §3.
