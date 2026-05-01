# HD key derivation in Boss Wallet's vendored `utxowallet`

This note documents two places where Boss Wallet's vendored `utxowallet` deliberately departs from the behaviour of upstream `btcsuite/btcwallet` (and `dcrlabs/ltcwallet`, which Bison Wallet previously consumed for Litecoin). Both changes are intentional. Both are user-visible. If you are wiring up a new chain or auditing how addresses get derived, read this first.

## TL;DR

- Boss Wallet uses standards-compliant BIP-32 derivation (`hdkeychain.ExtendedKey.Derive`) at every level of the path. Bison Wallet inherited `btcwallet`'s legacy buggy derivation (`DeriveNonStandard`) for backwards compatibility with itself; we do not.
- Boss Wallet's Litecoin wallet derives at SLIP-44 coin type `2'` (`m/{44,49,84,86}'/2'/...`). Bison Wallet derived Litecoin at coin type `0'`, the same path Bitcoin uses, as a workaround for an unrelated bug in `btcwallet`'s coin-type plumbing. We do not.

The implication of both changes is the same: a seed that is valid in *both* wallets does not necessarily address the same coins on disk in both wallets. Anyone migrating funds between the two should read the rest of this document.

---

## Issue 1: BIP-32 derivation (`Derive` vs. `DeriveNonStandard`)

### What BIP-32 says

When you derive a child key at index `i` from an extended parent key, BIP-32 specifies a check: the resulting child key may be invalid (the intermediate value `IL` is greater than or equal to the secp256k1 curve order, or the resulting key is zero). When that happens, the implementation must skip the index — increment `i` and try again — and the parent of that path is responsible for advertising the skipped child as undefined.

The check fires very rarely in practice. In hardened derivation it is the leading-zero-byte case in `IL` that is the empirically observable failure mode; the documented frequency is roughly **1 in 256 hardened derivations**.

### What `btcwallet` did

The `btcsuite/btcd/btcutil/hdkeychain` package originally shipped a `Child` method (and later a `Derive` alias for it) that did *not* perform the BIP-32 skip. When the bad path was hit, it returned the malformed child key as if nothing had happened. This was identified, the broken method was renamed to `DeriveNonStandard` and marked deprecated, and a new, correct `Derive` method was added.

`btcwallet` could not adopt the corrected `Derive` because doing so would change the keys produced by every existing seed-on-disk for a path landing on the affected indices. ~1 in 256 hardened derivations would silently start pointing at different addresses, which from a user's perspective looks identical to "the wallet lost my money." So upstream `btcwallet` continues to call `DeriveNonStandard` everywhere and suppresses the deprecation lint.

Bison Wallet inherited that decision wholesale.

### What Boss Wallet does

Every site that walks the BIP-32 hierarchy in the vendored tree (purpose, coin type, account, branch, address index) uses the standards-compliant `hdkeychain.ExtendedKey.Derive`. Sites:

- `wallet/utxowallet/waddrmgr/manager.go` — `deriveCoinTypeKey`, `deriveAccountKey`, `checkBranchKeys`
- `wallet/utxowallet/waddrmgr/scoped_manager.go` — `deriveKey`, `nextAddresses`, `extendAddresses`

There is no `nolint:staticcheck` suppression in the tree. If you see one being added during a future re-vendor pass from upstream, remove it and convert the call to `Derive`.

### Implications

A seed produced by a wallet that depended on `btcsuite/btcwallet` or `dcrlabs/ltcwallet` (this includes Bison Wallet) may not properly restore in Boss Wallet: the buggy derivation will, on a small fraction of seeds, place funds at addresses Boss Wallet does not derive. Boss Wallet does not raise an error in that case — it simply reports a zero (or partial) balance. The funds are not lost; they are still recoverable from a wallet that uses the legacy derivation. To recover them, either restore the seed in the originating wallet (Bison Wallet, or another `btcwallet`/`ltcwallet`-derived client) and send the funds to a fresh Boss Wallet address, or use a recovery tool that walks both the standard and `DeriveNonStandard` paths.

---

## Issue 2: Litecoin coin type (`m/.../2'/...` vs. `m/.../0'/...`)

### What SLIP-44 says

SLIP-44 assigns each chain its own BIP-44 "coin type" index, used as the second hardened path component. Bitcoin is `0'`, Bitcoin testnet is `1'` (shared by all testnets), Litecoin mainnet is `2'`. A standards-compliant Litecoin wallet derives at `m/{44,49,84,86}'/2'/account'/branch/index`.

### What `btcwallet` did, and what `dcrlabs/ltcwallet` did about it

`btcwallet` was originally Bitcoin-only. Its `KeyScopeBIP00xx` constants were declared at package init time as `KeyScope{Purpose: xx, Coin: 0}` — coin type baked in at compile time. Several callers in `btcwallet` itself look up scopes by these globals.

When `dcrlabs` forked `btcwallet` to support Litecoin, they took the path of least resistance: they left the `Coin: 0` globals alone, derived Litecoin addresses at `m/.../0'/...` (Bitcoin's path, not Litecoin's), and exposed an explicit constant called `KeyScopeBIP0084WithBitcoinCoinID` to let callers be clear about what was happening. Bison Wallet's `wallet/asset/ltc` consumed that constant on every address-creation, change-address, and account-properties call to keep its existing on-disk Litecoin wallets working.

The result: Litecoin wallets in the Bison Wallet ecosystem have funds at `m/{44,49,84,86}'/0'/...`, which no third-party Litecoin wallet recognises as Litecoin paths. They are SLIP-44-incompatible.

### What Boss Wallet does

The vendored `utxowallet`'s key-scope helpers are coin-type-aware. `waddrmgr.DefaultKeyScopesForCoin(coinType)` and `waddrmgr.ScopeAddrMapForCoin(coinType)` build scopes using the *chain's* coin type, not a hardcoded `0`. The convenience helper `Wallet.ScopeForPurpose(purpose)` returns `KeyScope{Purpose: purpose, Coin: w.chainParams.HDCoinType}`.

Litecoin's chain params at `wallet/utxowallet/assets/ltc.go` set:

- `mainnet`: `HDCoinType: 2`
- `testnet4`: `HDCoinType: 1`
- `simnet`: `HDCoinType: 1`

The Litecoin asset wrapper at `wallet/asset/ltc/spv.go` calls `w.Wallet.ScopeForPurpose(84)` everywhere it used to call `KeyScopeBIP0084WithBitcoinCoinID`. Mainnet Litecoin wallets created by Boss Wallet derive at `m/{44,49,84,86}'/2'/...`; testnet4 and regtest at `m/.../1'/...`. There is no `KeyScopeBIP0084WithBitcoinCoinID` in the vendored tree, intentionally.

The Bitcoin asset wrapper at `wallet/asset/btc/spv_wrapper.go` follows the same pattern: `NewChangeAddress`, `NewAddress`, `AccountProperties`, and `extendAddresses` all look up scopes via `wallet.ScopeForPurpose(84)` rather than the package-level `waddrmgr.KeyScopeBIP0084` constant. The `BTCWallet` interface includes `ScopeForPurpose(purpose uint32) waddrmgr.KeyScope` so non-`*wallet.Wallet` implementations (BCH, the in-tree test fakes) can answer for themselves; BCH always returns `{Purpose: 44, Coin: 0}` because the BCH SPV wallet derives through a separately-vendored bchwallet stack. Bitcoin's chain params follow `btcd`'s `chaincfg`: mainnet uses `HDCoinType: 0`, testnet3/regtest use `HDCoinType: 1` per BIP-44, and the asset wrapper now picks up the correct coin type for the network rather than forcing `0`.

The same coin-type plumbing is what makes a future Dogecoin, Digibyte, etc. derive at their respective SLIP-44 coin types without a per-chain workaround.

### Implications

A Litecoin seed produced by a wallet that depended on `dcrlabs/ltcwallet` (this includes Bison Wallet) will not properly restore in Boss Wallet: Boss Wallet looks at `m/.../2'/...` while the source wallet's funds sit at `m/.../0'/...`. Boss Wallet will report a zero balance. To recover, either restore the seed in the originating wallet and send the funds to a fresh Boss Wallet Litecoin address, or import the seed into a third-party wallet that lets you override the derivation path (Electrum-LTC, Sparrow's "advanced" derivation field, etc.), specify `m/84'/0'/...`, and forward the funds from there.

---

## When you would re-introduce these workarounds

- **Multi-wallet recovery tooling.** A "rescue funds from a Bison Wallet seed" utility might want to compute *both* derivations — `Derive` *and* `DeriveNonStandard`, `Coin: 0` *and* `Coin: 2` — and report any non-empty paths it finds. That's a reasonable place to bring the legacy paths back, scoped to the recovery tool. Do not bring them back into the main wallet.
- **Hardware-signer compatibility.** Some hardware signers historically aligned with `btcwallet`'s coin-type-zero behaviour for Litecoin. If we add hardware-signer support for Litecoin and the device only supports the legacy path, we have a real choice to make. Re-open this document at that point.

If you find yourself adding `nolint:staticcheck` next to a `Derive` call, or hardcoding `Coin: 0` in an LTC code path, stop and re-read this file. Either you have a legitimate reason that should be documented here, or you are about to undo a deliberate choice.
