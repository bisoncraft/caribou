# Surgical wallet-server rotation for the electrum backend

Plan for replacing top-down teardown with in-place rotation: quarantine the
misbehaving wallet server, promote an observer into the wallet slot, backfill
the observer set — all beneath `ElectrumNetwork`, invisible to the layers
above. Extends `electrum-multi-server-plan.md` (phases C1–C4, implemented)
and supersedes its phase-1 "the wallet-level reconnect is the rotation
mechanism" description, which overstated what exists: nothing restarts the
sync after a mid-session backend death today, so rotation only occurs at
dial time.

## Status

Plan. Not started.

## 1. Why surgical

- **Self-healing removes a whole failure class.** If the Network never dies
  on server failure — every failure handled internally by rotation, retry,
  or degraded idling — then the wallet layer's unbuilt "restart the sync on
  backend death" loop is unnecessary by construction. The notification
  channels close only on real shutdown.
- **Observers are pre-validated replacements.** A live observer has already
  proven reachability, protocol compatibility, genesis, and — via the
  compare loop — chain agreement. Promotion adopts a connection that has
  been vetted for exactly the properties the wallet slot needs.
- **The expensive state survives.** The header store, the wallet's
  scripthash diff baselines, the rescan floor, and the chain client's
  tip-continuity state all remain valid across a swap; a teardown discards
  and rebuilds all of it.

## 2. What a swap actually consists of

The wallet node participates in the Network at these attachment points,
each of which the swap must handle:

1. **`n.node` (mu-guarded)** — RPC target for `getNode` callers (probes,
   sweeps, history, broadcast). Swap under the lock; in-flight RPCs against
   the old node fail into existing per-caller retry semantics (sweep
   retries next interval, probes are non-fatal to recovery, broadcast
   errors surface).
2. **`headersLoop` / `scripthashLoop`** — goroutines bound to the node's
   notify channels (network.go:686, 952). The swap must join the old
   node's loops (exit on node death, signal completion) before starting
   the new node's, so two loops never interleave header appends or status
   forwards.
3. **Server-side session state** — the old node's header + scripthash
   subscriptions die with its connection:
   - *Headers:* a promoted observer already holds a header subscription.
     Catch-up runs the existing `syncHeaders` machinery from our tip+1
     against the new node; contiguity of TipChange emissions is preserved
     because the store only advances through the validated append paths.
   - *Scripthashes:* observers deliberately carry no address
     subscriptions, so the swap replays them. The Network gains a
     subscription registry (it already brokers every
     Subscribe/UnsubscribeScripthash — network.go:426/435); replay
     re-subscribes each entry on the new node and forwards the returned
     statuses through the normal channel. The chain client's
     status/history diff then emits exactly what was missed during the
     gap — the self-healing property the subscription design was built
     around. Bounded by the watch policy's small persistent set.
4. **`watchNode`** — re-armed against the new node; its death arm calls
   the rotation entry point instead of `Stop`.
5. **Observer set** — the promoted observer leaves the set;
   `fillObservers` backfills from the pool on its next pass (existing
   machinery).

The invariant throughout: **nothing above `ElectrumNetwork` observes the
swap.** No channel closes, no re-Synchronize, no rescan restart; the chain
client's `goLive`/`lastConnected` state is untouched.

## 3. Quarantine vs backoff

Two failure classes, two consequences:

- **Connection failures** (dial, timeout, disconnect) keep the existing
  10-minute health backoff — transient by default.
- **Misbehavior** (checkpoint mismatch, retarget violation, PoW failure,
  losing side of a verified divergence) earns **quarantine**: excluded
  from every election for a persisted cooldown (proposed: 24 hours,
  recorded in the pool database with the reason), plus process-lifetime
  memory. Quarantined servers are skipped outright — no
  least-recently-failed fallback; per the established policy, loudly
  degraded beats quietly eclipsed.

## 4. Promotion policy

The C4 tier rules apply unchanged to promotion: the wallet slot prefers a
**shipped** live observer (the shipped-first observer fill makes one likely
present); with none, dial a fresh shipped candidate; a discovered observer
is promoted only under the existing gated fallback (all shipped failing
30+ minutes, aged candidates only). Among eligible observers, highest tip
wins.

Two promotion flavors:

- **Same-chain promotion** (death, lag): the observer agrees with our
  chain; catch-up is append-only.
- **Reorg promotion** (verified divergence): the dissenting chain carries
  at least our cumulative work, so promotion adopts it and the header
  store must rewind to the fork and resync — the existing reorg machinery
  (rewind + TipChange.Disconnected + wallet rollback) does this work; the
  swap only drives it. `spendSuspect` stays set from the divergence
  finding until the resync completes, then clears: the store then reflects
  the best-work chain we can see.

## 5. Degraded self-healing (no candidate at all)

When rotation finds no eligible replacement, the Network enters a degraded
state instead of dying: wallet slot empty, `getNode` callers error, an
internal timer re-runs the election until a candidate answers. Observers
(if any) keep their subscriptions. The UI stays quiet per policy; the user
sees errors only on action. `Stop` remains the only path that closes the
notification channels.

## 6. Synchronization design

The headaches, named and contained:

- **One rotation at a time**: a rotation mutex serializes
  swap/degraded-retry/Stop; Stop sets a terminal flag the rotation checks
  at each step, aborting into teardown cleanly.
- **Loop lifecycle**: refactor `headersLoop`/`scripthashLoop`/`watchNode`
  into a per-node "loop group" with a join handle — start(node) returns
  done; rotation joins before starting the next group. This is the
  foundation commit and changes no behavior on its own.
- **Initial sync excluded**: failures before the first `goLive` use the
  existing simple path (fail Start / re-elect within Start). The surgical
  machinery applies only to the live phase, where the state worth
  preserving exists.
- **Lock order**: rotation mutex → n.mu → obsMtx → pool.mtx, documented
  at declaration.

## 7. Increments

- **R0 — fake electrumx server (test infra).** In-package scriptable fake
  speaking the JSON-RPC subset (version/features handshake, header
  subscribe+notify, block.headers, scripthash subscribe/get_history,
  broadcast), with fault injection: kill connection, lag the tip, serve a
  forked chain. Prerequisite for every test below; also unlocks the
  broadcast fan-out unit tests from the coverage review.
- **R1 — loop-group refactor.** Per-node lifecycle with join semantics;
  behavior unchanged; existing tests must stay green.
- **R2 — subscription registry + replay.** Registry maintained in
  Subscribe/Unsubscribe; replay function tested against the fake (miss a
  payment during the gap → replayed subscribe's status diff delivers it).
- **R3 — quarantine tier.** Pool field + election exclusion + persistence;
  unit tests.
- **R4 — same-chain promotion.** rotate(reason): quarantine/demote old,
  promote per policy, join loops, catch up headers, replay subscriptions,
  re-arm watchNode, backfill observers. Wire death + lag triggers. Fake
  tests: mid-session kill → wallet stream continues, missed tx delivered,
  no channel close.
- **R5 — reorg promotion.** Verified divergence quarantines the wallet
  server and promotes onto the heavier chain; fork test against the fake;
  spendSuspect lifecycle.
- **R6 — degraded self-healing + doc corrections.** Internal re-election
  timer; remove the dead top-down assumptions from
  `electrum-multi-server-plan.md` phase 1; harness kill-server test via
  the dextest electrumx.

## 8. Replay reconciliation: what the swap guarantees about missed activity

The subscription push is a cache-invalidation hint, not the source of
truth. The electrum status is a hash commitment over an address's entire
history (confirmed + mempool), and a subscribe returns the current status
immediately — so the replay does not wait for future activity to close the
gap:

- **Status equal** to the stored baseline ⇒ the address's history is
  byte-identical to what was already processed; nothing was missed while
  blind. The equality is meaningful because the status commits to the full
  history.
- **Status differs** ⇒ the handler fetches the full history from the NEW
  server and diffs against the processed baseline by (txid, height):
  missed mempool arrivals, missed confirmations, and reorged entries
  (height changes produce new pairs) are all delivered at promotion time.
  Suppressed notifications from a misbehaving old server are therefore
  recovered — omission self-heals.

Preconditions already in place: the header catch-up precedes the replay
(so confirmed entries are not parked awaiting headers), and the wtxmgr
same-block ordering fixes make the per-address replay order safe at the
store level.

**The honest asymmetry — fabrication.** A hostile old wallet server that
INVENTED history (a fake confirmed transaction) is not unwound by the
replay: the diff drops fabricated entries from the subscription baseline,
but the transaction already ingested into wtxmgr persists. This is the
standing single-server electrum trust gap (transactions are ingested
without merkle inclusion proofs) and is not introduced by rotation — but
rotation is where it becomes visible. The structural fix is **R7**.

## 9. Increment R7 — merkle inclusion verification (proposed)

Verify confirmed transactions at ingestion with
`blockchain.transaction.get_merkle` against the validated local header's
merkle root, at least for transactions crossing a trust boundary (initial
sync from a new server, sweep re-promotion, post-rotation replay deltas).
Closes fabrication at the source; makes every confirmed entry in wtxmgr
provably committed to by a header the store validated. Sized and sequenced
after R6.

## 10. Decisions (settled)

- Quarantine: 24 hours, persisted in the pool database with the reason;
  process-lifetime exclusion regardless.
- Lag-triggered rotation demotes only — slowness is not hostility.
- Reorg promotion requires the dissenting observer to serve the full fork
  segment (anchor-bounded) before adoption.
