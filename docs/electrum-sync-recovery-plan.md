# Electrum sync & recovery architecture

Architectural deep-dive and plan for the electrum-backed utxo wallet's initial
sync and recovery. Companion to `address-watching-plan.md` (which covers the
steady-state subscription set); this document covers initial catch-up,
seed-restore recovery, and `SyncedTo` advancement.

## Status

Analysis + plan. Not started. Supersedes the ad-hoc recovery attempt (reverted)
that bolted gap-limit discovery synchronously onto `Connect`.

## 1. The conundrum, restated

Adding seed-restore recovery to the electrum wallet was awkward: btcwallet's
block-walk recovery is O(chain length) and never finishes on a long chain, so
it's disabled (`recovery_window=0`), which loses gap-limit fund discovery. Every
attempt to re-add discovery fought the wallet's sync machinery.

The recovery pain is a *symptom*. The root cause is structural, and the
highest-leverage fixes are not even "recovery."

## 2. Root cause: an impedance mismatch

btcwallet's `chain.Backend` contract and its `syncWithChain` driver are
**block-oriented** — designed for neutrino / btcd / bitcoind, which serve blocks
and compact filters. The wallet catches up by walking blocks and asking "does
this block touch me."

Electrum is **address-oriented**: `blockchain.scripthash.get_history` returns an
address's *entire* history (txids + heights) in one call, and headers arrive on
a separate subscription. There is no block to walk and no per-block question to
ask.

`chain.ElectrumClient` bridges the two by *emulating a block backend* on top of
electrum — implementing `Rescan`/`FilterBlocks` by fetching per-address
`get_history`, bucketing by height, and replaying per-block notifications. That
emulation is correct, but it forces address-shaped data back through a
block-shaped pipe, and that is where the costs and the recovery awkwardness come
from.

## 3. Four concerns the design entangles

For an electrum wallet, "sync" decomposes into four independent things. The
current design threads all four through the block-oriented pipe:

1. **Header sync** → chain height + confirmation depth. Already clean
   (`electrum.Network` owns headers and the tip).
2. **Address discovery** → which derived addresses are mine. Should be a
   `get_history` gap-limit scan; currently tied to btcwallet's block-walk
   recovery (disabled).
3. **Transaction ingestion** → populate `wtxmgr` (balance, UTXOs). Currently the
   block-walk emulation (`Rescan`/`FilterBlocks`).
4. **`SyncedTo` advancement** → the wallet's "caught up to height H." Currently
   advanced **one block at a time** from the birthday block.

These are separable. Discovery needs no blocks. Ingestion needs only the txs
`get_history` already named. `SyncedTo` can jump — the header store has every
header. Only header sync is inherently sequential, and it's already handled.

## 4. Grounded inefficiencies

- **A. O(chain) `SyncedTo` catch-up — affects the *normal* path, not just
  recovery.** On every `RescanProgress`/`RescanFinished`, the wallet runs
  `catchUpHashes` (`wallet/chainntfns.go:193,202`), which loops
  `for i := startBlock.Height+1; i <= height; i++` doing `GetBlockHash` +
  `GetBlockHeader` + `SetSyncedTo` **per block** (`chainntfns.go:48-84`,
  `connectBlock` at `:248`). With the 2021 birthday on a ~76k-block regtest
  chain, `SyncedTo` starts near genesis and walks the whole chain — this is the
  bulk of the observed ~16s sync, and on mainnet from an old birthday it is
  minutes.
- **B. Block-walk recovery + per-batch re-fetch.** btcwallet's `recovery()`
  (`RecoveryManager`) calls `FilterBlocks` per block-batch; the electrum
  `FilterBlocks` re-fetches `get_history` for every lookahead address on every
  batch (see D). O(chain × lookahead). Disabled via `recovery_window=0`; this is
  why restore-discovery is off.
- **C. `runRescan` iterates every height** birthday→tip
  (`chain/electrum_rescan.go`), emitting only for matching heights. Local and
  cheap on the harness; O(chain) on mainnet.
- **D. `get_history` caching gap.** `fetchAndCacheHistory`
  (`chain/electrum_rescan.go`) caches only when an `addrCache` entry already
  exists — i.e. only for *subscribed* addresses. Unsubscribed probes and rescan
  lookahead re-fetch every time.
- **E. Duplicate transaction stores.** The asset layer keeps its own
  `BadgerTxDB` and polls `wtxmgr` via `ListSinceBlock`
  (`asset/btc/electrum.go` `syncTxHistory`/`scanNewTransactions`) to populate
  it. Every tx is stored twice with a polling bridge between them; this bridge
  is where the wtxid-vs-txid bug surfaced.
- **F. `RecoverWallet` drops the birthday.** Fresh-seed creation is actually
  correct: `core.go:2388` sets `bday = time.Now()` for a new seed and decodes it
  from the mnemonic for a restore. But `RecoverWallet` (`core.go:1655`) omits
  `Birthday` from `CreateWalletParams`, so a rebuild falls to
  `DefaultWalletBirthday` (2021, `asset/btc/btc.go:566`) and re-scans from 2021
  even for a wallet created today — triggering inefficiency A. (An earlier
  draft overstated this as a fresh-wallet default; the create path is fine, the
  recover path is not.)
- **G. Unbounded subscription set.** See `address-watching-plan.md`.

A and B trace back to §2: the wallet treats catch-up as a block walk from a
birthday, when electrum needs neither. F is a separate plumbing bug that
needlessly triggers A.

## 5. Design principle: an address-native initial sync

Keep btcwallet's `wtxmgr`/`waddrmgr` and the sanctioned
`RelevantTx`/`FilteredBlockConnected` ingestion path — but feed them from
address history and stop walking blocks:

```text
initial sync (electrum) =
    header sync            (electrum.Network — already done)
  + address discovery      (get_history gap-limit per scope/branch)
  + tx ingestion           (get_history -> fetch txs -> wtxmgr, via RelevantTx)
  + SyncedTo := tip        (jump, from the header store)
```

`Connect` stays fast regardless: `Wallet.Synchronize` is non-blocking
(`wallet/wallet.go:211` — it only launches `handleChainNotifications` et al.),
so all of the above runs in the background sync goroutine, off the connect path.
The earlier attempt blocked `Connect` only because it called discovery
*synchronously in `Connect` before `Synchronize`* — neither necessary nor
correct.

## 6. Plan (EV-ordered)

### Phase 1 — Birthday + recovery (the immediate win)

- **1a. Pass the birthday through `RecoverWallet`.** The create path is already
  correct (F); the fix is the one-line omission in `RecoverWallet`
  (`core.go:1655`): pass `creds.Birthday` in `CreateWalletParams` (it exists and
  is the right source — `core.go:1239`, `:1805`). Near-zero code; stops the
  rebuild path re-scanning from 2021.
- **1b. Address-native discovery.** Add a `get_history` gap-limit scan
  (the reverted `RecoverViaScan` + chain-client `AddressHasHistory` probe),
  hooked **inside `syncWithChain`** just before `rescanWithTarget` (so the
  rescan that follows ingests the discovered addresses — no separate
  re-trigger), gated on an optional `AddressProber` backend capability
  (electrum implements it; neutrino keeps the block-walk). Three requirements
  that fell out of review:
  - **Bound probe concurrency** with a semaphore (~10–20). Probing a gap window
    concurrently is necessary so a fresh wallet's ~120 empty probes don't
    serialize, but *unbounded* concurrency risks rate-limiting / disconnection
    on public mainnet servers.
  - **Make cache-sharing part of the prober contract (folds in fix D).** The
    probe and the immediately-following `rescanWithTarget` both `get_history`
    the same addresses, but `fetchAndCacheHistory`
    (`chain/electrum_rescan.go:647`) only writes `lastHistory` for *subscribed*
    scripthashes — unsubscribed probes are fetched and discarded. So probing
    must prime the backend's history cache (or the prober must return the
    history for the caller to thread through). Design this into the
    `AddressProber` shape, not as a side cache, or the probes double-fetch.
  - **Gate discovery correctly — the main design risk.** `syncWithChain` fires
    on every `ClientConnected`, so discovery must distinguish four cases and run
    only for the first:
    - **mnemonic restore** — seed present, no derived-count info → run discovery.
    - **`RecoverWallet` rebuild** — derived address counts are *already*
      transferred via `RecoveryCfg` (`asset/btc/btc.go:847`
      `NumExternal/InternalAddresses` → `extendAddresses`, electrum.go:84), so
      discovery is redundant and must not override it.
    - **ordinary reconnect** — full prior state present → skip.
    - **fresh create** — nothing to find → skip.

    The gate must *compose* with the existing `RecoveryCfg` count-transfer, not
    duplicate it — a persistent "discovery complete" marker plus "no counts were
    supplied." Caveat: `GetRecoveryCfg` currently lives on the old
    `*ExchangeWalletSPV` stack; confirm whether the lean electrum wallet emits
    counts on rebuild — if not, the rebuild case *also* needs discovery.

Result: the rebuild path stops re-scanning from 2021 (1a); restores discover
gap-limit funds (1b); both off the connect path. (Fresh wallets are already
fast — the create path passes a now() birthday; the harness's ~16s sync is the
*test helper* passing `DefaultWalletBirthday` directly, a separate test-config
fix.)

### Phase 2 — Jump `SyncedTo` (deferred; make long-lived restores scale)

**Largely deferred by a launch-date default birthday.** These are new wallets
with no pre-launch history, so before launch `defaultWalletBirthdayUnix` will be
moved to utxowallet and set to ~the launch date (it's 2021 today). Then the
oldest birthday any wallet can have is the launch date, so `catchUpHashes` walks
at most `now − launch` blocks — tiny at launch, growing only as the chain ages.
That removes the O(chain-from-2021) cost outright; this phase becomes a *later*
optimization for the day a launch-era wallet is restored after the post-launch
chain has grown long, not a launch blocker. (It also lets the recovery harness
test drop its `now()−1h` birthday hack once the const is recent.)

When it's worth doing: fast-forward `SyncedTo` to the tip directly from the
header store instead of `catchUpHashes`' per-block walk.

The hurdle (confirmed in `waddrmgr/db.go:2201-2212`): once the birthday block is
set, `PutSyncedTo` requires `fetchBlockHash(bs.Height-1)`. A naive jump to tip
`T` fails (`T-1` was never stored), and — subtler — you can't even *start* a
contiguous walk at `T-K`, because `PutSyncedTo(T-K)` needs `T-K-1`, also never
stored. So the primitive is:

> **Seed the last K block hashes** from the electrum header store directly into
> the block-hash bucket (bypassing the `Height-1` check for the seed writes),
> then `SetSyncedTo(tip)`. O(K), one low-level seed.

K is a deliberate reorg-safety margin, not arbitrary: the db only ever retains
`MaxReorgDepth = 10000` hashes (`staleHeight` prunes older), and rollback works
within that, so `K ≤ 10000` — pick a `confTxFinality`-sized value for cost. Both
"bypass" and "finality-buffer-walk" framings need this seed write; there is no
zero-helper version.

### Phase 3 — Trim the emulation (optional; mainnet scale)

Have `runRescan` (`chain/electrum_rescan.go`) iterate the *sorted keys of the
`matches` map* — the heights that actually have txs — and emit a
`FilteredBlockConnected` for each, then a single `RescanFinished` at tip.
Converts the rescan from O(chain length) to O(blocks-with-txs) (fix C). (The
history-cache fix D moved up into Phase 1b.)

**Depends on Phase 2.** That terminal `RescanFinished` at tip drives
`catchUpHashes(tip)` — the exact O(chain) walk Phase 2 removes — so Phase 3 only
pays off once the `SyncedTo` jump is in place. Sequence 2 before 3.

### Phase 4 — Collapse the duplicate tx stores (strategic)

Remove the asset `BadgerTxDB` ↔ `wtxmgr` `ListSinceBlock` polling bridge (E) in
favor of a single source of truth. Ties into the master-TxDB direction; out of
scope for recovery but the strategic endpoint.

### Phase 5 — Bounded subscriptions

Per `address-watching-plan.md` (G).

## 7. Recommendation

The real near-term work is **1b** (done) plus **moving the default birthday to
the launch date**: 1b makes seed restores *work* (discover gap-limit funds), and
the recent default birthday makes them *scale for free* — `catchUpHashes` only
walks `now − launch`, so the O(chain) jump (**Phase 2**) is deferred until the
post-launch chain is old enough to matter. Recovery sits where btcwallet's own
recovery sits (background sync flow), not bolted onto `Connect`. **1a** is a
cheap, independent correctness fix for the rebuild path (fresh wallets are
already correct). Phases 2–5 are deferred/strategic — pursue when chain age or
the TxDB consolidation makes them pay.

The framing that matters: **the recovery problem is the block-oriented sync
contract leaking through an address-oriented backend.** Fix it by making the
electrum initial sync address-native (discover + ingest + jump `SyncedTo`), not
by making the block-walk faster.

## 8. Open questions / verification

- **Birthday (1a):** the create path is verified correct and `RecoverWallet`'s
  omission is the fix; still worth confirming `creds.Birthday` is populated on
  the recover path and that no other `CreateWallet` caller drops `Birthday`.
- **Jump margin K (2):** pin the value (a `confTxFinality`-sized margin, bounded
  by `MaxReorgDepth = 10000`) and the seed-write helper (a low-level
  `addBlockHash` for the boundary, or a `PutSyncedToJump` that skips the
  `Height-1` check).
- **Discovery gating (1b) — the main design risk:** distinguish mnemonic
  restore (run) from rebuild (counts already via `RecoveryCfg`) from reconnect
  (skip). Where the "discovery complete" marker lives (waddrmgr? asset txdb?),
  how it composes with `RecoveryCfg`, whether the lean electrum wallet emits
  recovery counts at all, and whether a later "re-scan on demand" path is wanted
  (composes with cold-address metering in `address-watching-plan.md`).
- **`AddressProber` shape (1b):** the capability interface, shared with the
  neutrino fallback; must make discovery→ingestion cache-priming part of its
  contract (electrum_rescan.go:647); composes with the `AddressWatcher`
  interface in `address-watching-plan.md`.
- **Recovery test:** fund a gap-limit address, create a fresh wallet from the
  same seed, assert discovery + balance (the harness test drafted during the
  reverted attempt).
