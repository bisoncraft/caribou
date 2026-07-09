# Persisted hot/cold tiering + neutrino watch policy

Plan for persisting the hot/cold address classification to disk so that at
startup only hot addresses enter the subscribe/watch set, and for extending
the watch policy to the neutrino (BIP157/158) backend. Extends
`address-watching-plan.md` (the in-memory tiering increments 1–2, electrum) —
this document persists the tier and covers the neutrino backend.

> **Superseded for planning and status by `docs/utxosync-master-plan.md`.** Retained as a detailed design record.

## Status

Proposed. Not started. Parameters settled (see §7).

## 1. Motivation: neutrino restart is CPU-bound, not memory-bound

Restarting a neutrino-backed wallet after a long offline delay is slow and
spikes single-threaded CPU. The cause is not the in-memory address set — it
is BIP158 per-block filter matching during catch-up. For `B` gap blocks and
`W` watched scripts, catch-up costs `O(B·N)` to decode filters (`N` scripts
per block, an unavoidable floor for any cfilter scan) plus `O(B·W log W)` for
`MatchAny` over the watch set. An aged wallet's `W_cold` (every used address,
add-only forever) dwarfs `W_hot` (lookahead + recently active, ~tens), so the
watch set contributes a large avoidable CPU multiplier on the user-visible
startup path.

The hot/cold split removes the `W_cold` term from the critical-path catch-up
and moves the cold check to a throttled background sweep. The honest limit:
the `O(B·N)` filter-decode floor is paid by any real scan, so the win is
criticality (cold off the startup path), throttling (the sweep is rate-capped
so it never spins the fan), and rarity (the sweep covers only
`[last-swept, tip]`), not a reduction in total work.

## 2. Persist last-active-height, classify at load

Persist, per **scripthash**, its last-active block height — not a static
hot/cold flag. A flag goes stale because hotness is defined relative to a
moving reference; a stored height compared to a reference at load is always
correct. The index is a small side bucket (`scripthash → height`, 4 bytes),
maintained on ingestion (the watch policy already computes last-active in
memory). It is foldable into the master TxDB later; this avoids a `waddrmgr`
core-schema migration now.

At load, classification is a cheap `O(addresses)` pass, no history scan:

```text
hot ⟺ unused (gap-limit lookahead)  OR  last_active ≥ reference − quietDepth
```

### The reference is SyncedTo at last shutdown, NOT the new chain tip

The reference is `S` = the wallet's persisted `SyncedTo` block height from
before shutdown — already on disk in `waddrmgr`, so classification needs no
`GetBestBlock` and is entirely local.

Classifying against the new tip `T` (which may be months ahead) is wrong: an
address the user was expecting funds on right before shutdown (last-active
near `S`) would be demoted to cold and its receive deferred to the background
sweep. Against `S`, that address stays hot and is watched through the `[S, T]`
catch-up — where its expected activity lands — so it is detected promptly and
is not also swept. `T` only bounds the catch-up and the sweep target.

### Lifecycle

1. **Load** — classify against `S` from disk. Watch set = hot-at-`S`
   (lookahead + active-near-`S`), a small set. Cold-at-`S` set seeded with
   `upTo = S`.
2. **Catch up `[S, T]`** with the small watch set — hot-at-`S` activity found
   live; the cold set is swept over `[S, T]` in the throttled background pass.
3. **Reach `T`** — the existing reaper re-classifies against `T` (steady
   state) and demotes hot-at-`S` addresses that stayed quiet through the gap.

`S` governs the load/catch-up phase; `T` takes over for steady state once
current. The reaper performs the handoff — no new machinery for it.

### Replaces the current guard

This removes the `applyWatchFilter` guard (`wallet.go`:
`SyncedTo + watchFinalityDepth ≥ chainTip`), which disables filtering
precisely on a deep restart — the case that matters — and replaces the
`RangeTransactions`-scan `hotAddrs`. The in-memory tracker stays as the live
promote/demote layer, now seeded from disk and persisting transitions.
Watched unspent outpoints are tiered the same way (a cold address's UTXO
stays cold — we originate its spend).

## 3. Neutrino AddressWatcher implementation

`NeutrinoClient` implements `chain.Backend` but not `chain.AddressWatcher`
(`Unwatch` + `Sweep`), so the policy runs and no-ops there today. Two gaps:

- **Unwatch.** `NotifyReceived → rescan.Update(spv.AddAddrs(...))` is
  add-only; the forked `spv/rescan.go` `updateFilter` only appends to
  `watchAddrs`/`watchList`. Add a symmetric `RemoveAddrs` UpdateOption
  (set-difference on the watch list, under the existing `Update` lock), or
  tear down and restart the rescan with the reduced set. `RemoveAddrs` is the
  cleaner path and avoids resync churn since we own the fork.
- **Sweep.** No `get_history` equivalent; the metered check is a **bounded
  range cfilter rescan** over `[fromHeight, tip]` reusing the `GetBlock`/
  rescan machinery. It **must be throttled** (rate-limit filter fetch/match)
  so the background cold pass caps CPU and never spins the fan — this is
  neutrino-specific (electrum's per-address `get_history` needed no throttle).
  The `(hits, failed)` contract is all-or-nothing per range (a network error
  fails the batch), simpler than electrum's per-address failures.

### What does NOT translate

- **R7 merkle inclusion verification** — neutrino downloads real blocks via
  BIP157/158 and validates against cfheaders; the fabrication gap R7 closes
  does not exist there.
- **The surgical rotation program (R0–R7)** — electrumx single-connection
  machinery; neutrino connects to multiple peers with its own management.

## 4. quietDepth is chain-derived

`quietDepth = 24h / chain.TargetTimePerBlock` — BTC 144, LTC 576. This is
both the load-time threshold and a correction to the existing
`externalQuietDepth = 144` global constant, which is silently BTC-centric
(144 LTC blocks is 6 hours, not a day). `watchFinalityDepth = 6` stays a
fixed confirmation count — a reorg-safety margin, not a wall-clock duration.

## 5. Effort and sequencing

- **Persisted-tier refactor** (scripthash last-active-height index +
  maintain-on-ingest + load-time classification replacing the guard and
  `hotAddrs` + chain-derived quietDepth): ~1 week. Benefits both backends;
  partly a rebuild of `watch_policy.go` sync-time code. Sequence FIRST — it
  also lets electrum's bounded subscription set survive a restart without a
  scan.
- **Neutrino `Unwatch` + throttled `Sweep`**: ~1 week. The `RemoveAddrs`
  fork addition and the range-scoped throttled rescan, plus lifecycle tests
  on the neutrino simnet suites (the slower/flakier harness is the schedule
  risk). Verify neutrino's block-walk recovery (`recoveryWindow = 250`,
  waddrmgr lookahead) is undisturbed by the load-time filter.

~2 weeks total. SPV is a live shipping target (the motivating problem is a
field report), resolving the earlier question of whether the neutrino watch
work is worth building.

## 6. Verification

- Persisted index round-trips; classification at load matches the in-memory
  policy for a caught-up wallet.
- Deep-restart harness: an aged neutrino wallet restarted far behind the tip
  builds a watch set of only the hot-at-`S` addresses (not the full used
  set); catch-up CPU/time is bounded by the small set; a payment to a
  cold address during the offline gap is credited by the background sweep.
- A payment to a hot-at-`S` address during the gap is detected during
  catch-up (not deferred to the sweep).
- Neutrino Unwatch/Sweep lifecycle mirrors the electrum harness tests
  (change watched → finality → unwatched → still spendable; cold external
  demoted → paid → swept → re-promoted).
- Sweep throttling holds CPU below a target during a large-gap cold pass.

## 7. Settled parameters

- Reference for load classification: on-disk `SyncedTo` (last shutdown), not
  the new tip.
- Persisted primitive: per-scripthash last-active-height (not a flag).
- `quietDepth = 24h / TargetTimePerBlock` (chain-derived); also fixes the
  existing `externalQuietDepth` constant. `watchFinalityDepth = 6` unchanged.
