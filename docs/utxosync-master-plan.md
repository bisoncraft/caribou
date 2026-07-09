# utxosync: light-wallet sync, watching, recovery, and trust master plan

Master planning document for the `utxosync` branch. It subsumes the five
workstream docs listed in §10 — they remain as detailed design records; this
document is the single source of truth for scope, status, decisions, and
remaining work.

## 1. Through-line

The branch hardens the in-tree light UTXO wallet (`wallet/utxowallet`), shared
by the electrum and neutrino backends and driven by the forked btcwallet
`wallet.Wallet`. Every workstream serves one goal: make light-wallet initial
sync, steady-state watching, recovery, and server trust **correct, cheap, and
trust-minimized** — without leaking the block-oriented full-node model through
an address-oriented backend, and without unbounded resource growth as a wallet
ages.

Recurring principles, applied throughout:

- **Address-native, not block-native.** Discovery, ingestion, and watching are
  driven by address history / cfilters, not by re-walking the chain.
- **Bounded state as the wallet ages.** The persistent watch set, the header
  store, and the server pool are all bounded or tiered so an old wallet is no
  more expensive than a young one.
- **Trust minimized at the boundary.** A single server should not be able to
  lie by fabrication, omission, eclipse, or censorship without detection.
- **Self-healing over teardown.** Failures rotate or degrade in place; the
  layers above the backend do not observe them.
- **No proactive UI for degraded states.** Errors surface only when the user
  attempts an operation that cannot be performed safely (§ multi-server).

## 2. Status at a glance

| Workstream | Status | Detail |
| --- | --- | --- |
| A. Recovery & initial sync (electrum) | Phase 1 done; 2–5 deferred | §3 |
| B. Checkpoint anchors | Done | §4 |
| C. Address watching / hot-cold tiering | Increments 1–2 + persisted tier done (electrum); neutrino backend TODO | §5 |
| D. wtxmgr same-block ordering correctness | Done | §6 |
| E. Multi-server (electrum) | C1–C4 done | §7 |
| F. Surgical wallet-server rotation | R0–R7 done | §8 |
| G. Neutrino watch-policy translation | Not started | §9 |

Everything marked done is committed on `utxosync` and verified (unit + race +
harness, plus opt-in livenet where noted). The branch is not yet pushed or
PR'd.

## 3. Recovery & initial sync (electrum)

Goal: seed-restore recovery and initial catch-up that are address-native and
O(used + gap-limit), not O(chain length).

**Done (Phase 1).** Gap-limit discovery on by default via the `AddressProber`
capability, scanning each branch from its pool top; probe replies parked for
the follow-on rescan; confirmed ingestion is rescan-only (the fix for the
`bad-txns-inputs-missingorspent` ordering hazard); launch-date default
birthday moved to `utxowallet/wallet`. Commits: `389a8c09e`, `db48040ef`,
`35b874406`, `ec887ee27` (Phase 1a/1b landed with #91 before the branch).

**Deferred.** Phase 2 (jump `SyncedTo` to tip from the header store instead of
the per-block `catchUpHashes` walk) — held off by the launch-date birthday,
which bounds the walk to `now − launch`; revisit when the post-launch chain is
old enough to matter. Phase 3 (trim the rescan to blocks-with-txs) depends on
Phase 2. Phase 4 (collapse the duplicate `BadgerTxDB` ↔ `wtxmgr` stores) ties
into the master-TxDB direction.

## 4. Checkpoint anchors

Goal: bound header sync without carrying stale multi-decade checkpoint lists.

**Done.** Each network's checkpoint list is a single post-launch anchor — the
last retarget boundary before `DefaultWalletBirthday` — and the electrum
header store starts there instead of genesis. Anchoring exposed and fixed
three latent bugs: the non-genesis anchor linkage base case; Litecoin's
retarget timespan sampling (H−2017, not Bitcoin's H−2016); and the
birthday-block bisection floor. `TestAnchorHeaderSync` (livenet, opt-in)
verifies all four networks. Commit `da4f30c3d`; server lists `ddcdbdb5b`,
`aa313df21`.

**Release coupling.** Anchors and `DefaultWalletBirthday` must be refreshed
together at release (§ Release couplings).

## 5. Address watching / hot-cold tiering

Goal: a bounded persistent watch set — hot (real-time) for what a third party
may pay unpredictably, cold (metered sweep) for everything else — that
survives restart without a history scan.

**Done (electrum).**

- Increment 1: internal (change) addresses unwatched at finality; the
  `AddressWatcher` capability (`Unwatch`, later `Sweep`). Commit `af4f12913`.
- Increment 2: cold-external demotion (`6a2338acc`), metered sweep with
  re-promotion (`13a25f392`), per-entry sweep floors + lifecycle test
  (`0d32540ad`).
- Persisted tier: a scripthash → last-active-height index maintained on
  ingestion; load-time classification keeps only hot addresses, judged against
  the on-disk `SyncedTo` (the header at last shutdown), not the new tip, so an
  address expecting funds at shutdown stays watched through catch-up while
  quiet addresses drop to the sweep; one-time marker-gated backfill; empty
  index (fresh/restore) watches all. `quietDepth` is chain-derived
  (`24h / TargetTimePerBlock`), correcting the BTC-centric `144` constant.
  Commit `1ab4dbece`.

**Motivation for the persisted tier (field report).** Neutrino restart after a
long delay is slow and CPU-heavy — BIP158 per-block matching is `O(B·W)` in
watched scripts. Loading only hot addresses removes the accumulated cold set
from the catch-up. The honest limit: cfilter decode over the gap is an
`O(B·N)` floor; the win is criticality (cold off the startup path), throttling,
and rarity, not less total work.

**Decisions.** Reference = on-disk `SyncedTo` (not the new tip). Persisted
primitive = last-active-height (not a static flag — a flag goes stale as the
tip advances). Index keyed by scripthash. `quietDepth` chain-derived;
`watchFinalityDepth = 6` stays a fixed conf count.

**TODO.** The neutrino backend (§9), and a deep-restart harness test proving
the watch set collapses to the lookahead while the offline gap is swept.

## 6. wtxmgr same-block ordering correctness

Goal: correct UTXO tracking when a spend and its funding confirm in the same
block out of order — a fund-loss / phantom-UTXO class, surfaced by the
address-native ingestion work.

**Done.** A spender confirmed before its funding tx now records its debit
retroactively: via the preserved unmined-input record when the spender passed
through mempool (`54088906e`), and via a same-block sibling scan in
`addCredit`'s mined branch when it did not (`c193696fe`). Both pinned by
store-level tests verified failing pre-fix (`f3343060e` + the block-only
test). This hardened both the rescan and live ingestion paths.

## 7. Multi-server (electrum)

Goal: availability, tip integrity, and censorship resistance beyond a single
electrumx connection — one wallet server plus header-only observers, never a
full connect-to-everything.

**Done (C1–C4).**

- C1 failover: full server list consumed; one connection elected in
  health-scored order; every failure path demotes so the next election
  rotates. Server lists expanded and live-verified. `a8967934d`, `ddcdbdb5b`,
  `aa313df21`.
- C2 observer tips: 2–3 header-only observers; lag → rotate (demote only,
  slowness is not hostility); divergence → work-audited, then suspend
  broadcasts + rotate. `7e96bbebd`, `9837749d3`.
- C3 broadcast fan-out: wallet server authoritative, observers receive too;
  duplicate responses count as success. `c9848deab`.
- C4 peer discovery + persistent scored pool: `server.peers.subscribe`;
  per-chain `servers.json`; tiered election (wallet from shipped tier;
  observers shipped-first then recommender-diverse; gated discovered
  fallback). `690f9e7d2`, `fa2fee13c`.

**Policy (settled).** Lag = 20 min of blocks at target spacing, sustained
10 min. Divergence must demonstrate work before quarantine+suspend;
unverified dissent just demotes the observer. Quarantine 24 h persisted +
process-lifetime. No proactive UI; all decisions logged (error level for
verified divergence and refused broadcasts).

## 8. Surgical wallet-server rotation

Goal: replace top-down teardown with in-place rotation, so the `Network` is
self-healing and nothing above `ElectrumNetwork` observes a server failure.

**Done (R0–R7).** Fake scriptable server (R0); per-node loop groups (R1);
subscription registry + replay (R2); quarantine tier (R3); same-chain
promotion on death/lag (R4); reorg promotion on verified divergence with
forced chain reconciliation (R5); degraded self-healing when no candidate
answers, channels close only on `Stop` (R6); merkle inclusion verification at
ingestion, closing the fabrication gap (R7). Commits `adfb2bfd3` … `f48db4ff9`.

**This subsumed the earlier "restart loop" gap** — nothing restarts the sync
after a backend death because rotation handles every mid-session failure in
place.

**Three-agent review + fixes (`2f793a141`).** Fixed: R7 merkle proof was
defeated at the fetch hop (the fetched bytes were not bound to the proven
txid — HIGH); the lag path tore the network down instead of rotating (a
regression — `lagRotate` was dead code); and `handleDivergence` read the
observer tip twice, spuriously demoting honest observers. See §11 for the two
open decisions the review surfaced.

## 9. Neutrino watch-policy translation (not started)

Goal: turn the hot/cold policy on for the neutrino backend, which pays the
real `O(B·W)` catch-up CPU cost. The shared policy already runs on neutrino
and no-ops; implementing the capability activates it with zero policy code.

**Plan.**

- `Unwatch`: the forked `spv/rescan.go` filter is add-only; add a symmetric
  `RemoveAddrs` UpdateOption (set-difference), or rescan-restart. ~1.5 d.
- `Sweep`: a bounded range cfilter rescan over `[fromHeight, tip]`, **throttled**
  (rate-limited fetch/match) so the background cold pass caps CPU and never
  spins the fan — neutrino-specific. ~2 d.
- Verify neutrino block-walk recovery (`recoveryWindow = 250`, waddrmgr
  lookahead) is undisturbed by the load-time filter; lifecycle tests on the
  neutrino simnet suites (the flakier harness is the schedule risk).

**Does not translate.** R7 merkle (neutrino gets real blocks via BIP157/158)
and the rotation program R0–R7 (neutrino manages multiple peers itself).

**Justification.** SPV is a live shipping target — the persisted-tier work was
motivated by a field report of slow high-CPU neutrino restart.

## 10. Superseded workstream docs

Retained as detailed design records; this document is authoritative for
status and roadmap.

- `electrum-sync-recovery-plan.md` → §3, §4
- `address-watching-plan.md` → §5
- `neutrino-watching-plan.md` → §5, §9
- `electrum-multi-server-plan.md` → §7
- `electrum-surgical-rotation-plan.md` → §8

## 11. Open decisions

1. **Divergence griefing (chain-difficulty-dependent).** A single lying
   observer with a cheap `reorgRewindDepth`-length fork can suspend broadcasts
   and 24 h-quarantine the honest shipped wallet server, chainable toward a
   degraded-state DoS. Infeasible on BTC/LTC mainnet (real work); cheap on
   testnet/regtest/low-difficulty. Candidate: require a second independent
   observer to corroborate before quarantine+suspend; a lone divergence
   demotes+rotates transiently. Trade-off: LTC testnet may have < 2 observers.
2. **Mempool fabrication persistence (bounded).** Subscription replay's diff
   only adds, never removes, so a prior server's fabricated *unconfirmed*
   entry survives rotation as a phantom-pending tx (confirmed fabrications are
   blocked by R7). Pre-existing electrum-trust limitation; could matter only
   if the wallet ever spends unconfirmed. Fix or accept-and-document.

## 12. Remaining work, sequenced

1. Neutrino `Unwatch` + throttled `Sweep` (§9) — the outstanding half of the
   hot/cold win, on the backend that needs it most.
2. Deep-restart harness test (§5) — the end-to-end proof: watch set collapses
   to the lookahead, offline gap swept.
3. Open decisions §11 — a call on divergence corroboration and mempool
   fabrication.
4. Deferred recovery Phases 2–5 (§3) — age- and TxDB-driven; no urgency.

## 13. Release-checklist couplings

Bump together at release:

1. `DefaultWalletBirthday` (`utxowallet/wallet/loader.go`).
2. Checkpoint anchors (`assets/{btc,ltc}.go`) — re-derive with the two-source
   cross-check; `TestAnchorHeaderSync` verifies.
3. Shipped electrum server lists — re-probe (handshake + genesis); or fold into
   multi-server C4 discovery.

## 14. Verification posture

- **Unit** — pure logic and store-level (wtxmgr ordering, watch policy, server
  pool, header anchors, merkle audit). Run with `-race`.
- **Fake server** (`electrum/fakeserver_test.go`) — scriptable in-process
  electrumx for rotation scenarios (kill, lag, fork, all-down) and broadcast
  fan-out, deterministic and fast.
- **Harness** (build tag `harness`) — dextest simnet electrumx/neutrino; the
  wallet lifecycle suites.
- **Livenet** (build tag `livenet`, opt-in) — public servers; anchor sync and
  observer election against real infrastructure.
