# Address-watching strategy for the utxo wallet (Electrum + Neutrino)

Planning document for replacing the current "watch every wallet address
forever" model with a tiered scheme: a small, persistently-watched set of
**user-facing external addresses**, plus **metered, on-demand scans** for
internal (change/non-displayed) addresses and cold external addresses.

The scheme is backend-agnostic in policy but differs in mechanism between the
two `chain.Backend` implementations, so this document covers both.

## Status

Proposed. Not started. The companion electrum-recovery work (electrum-native
gap-limit discovery, currently stubbed out by `recovery_window=0`) shares the
same `get_history` primitive and metering machinery and should be designed
alongside this — see "Interaction with recovery" below.

## 1. Background: how watching works today

Both the electrum and neutrino backends implement the same
`chain.Backend` interface (`wallet/utxowallet/chain/interface.go`) and are
driven by the same forked btcwallet `wallet.Wallet`
(`wallet/utxowallet/wallet/`). The set of addresses being watched is therefore
decided in the **shared** wallet layer, not per backend:

- `Wallet.activeData` (`wallet/utxowallet/wallet/wallet.go:363`) returns the
  active receiving addresses and all unspent outputs; this set feeds
  `rescanWithTarget` (`wallet.go:574`), which establishes the watch.
- On new-address generation the wallet calls
  `chainClient.NotifyReceived` per address (`wallet.go:1760`, `:3177`, `:3241`)
  and `NotifyReceived(ourAddrs)` for the active set (`wallet.go:3792`).
- `NotifyReceived([]btcutil.Address)` is a **flat address list** — the
  external/internal branch is not carried on this path.
- The branch *is* preserved on the filter path: `FilterBlocksRequest` splits
  `ExternalAddrs` and `InternalAddrs` (`chain/interface.go`), and `waddrmgr`
  knows each address's branch (external = 0, internal = 1).

The net effect: every wallet address — external and internal, hot and cold —
is added to a single watch set that only ever **grows**. `chain.Backend` has
no "unwatch" verb at all.

## 2. Why it matters (cost models differ by backend)

### Electrum: a hard cap

Electrum watching is `blockchain.scripthash.subscribe`. electrumx enforces a
per-session subscription limit (`max_session_subs`, commonly ~1000). A
long-lived wallet that subscribes every external deposit address plus every
change address will hit that ceiling and start failing subscriptions. Every
reconnect also re-subscribes the entire set, so a large watch set makes
reconnects slow and fragile. This is a **hard** constraint.

### Neutrino: a soft, bandwidth cost

Neutrino watching adds scripts to a BIP158 cfilter watch list
(`NeutrinoClient.NotifyReceived` → `rescan.Update(spv.AddAddrs(...))`,
`wallet/utxowallet/chain/neutrino.go:499`). For `W` watched scripts over `B`
blocks, expected spurious full-block downloads scale roughly as `B·W/2^M`
(M ≈ 20). It bites hardest during deep sync / rescan / recovery and is modest
in steady state, but it still scales linearly with watch-set size. This is a
**soft** constraint — beneficial to reduce, not fatal if not.

Both backends therefore benefit from a smaller persistent watch set; electrum
*requires* it.

## 3. Principle

A persistent watch only earns its cost when it observes an event whose
**timing we do not control** — i.e. inbound funds to an address we have
exposed. Everything else has no time-critical event and should be checked with
metered, on-demand scans instead.

- **External address** = an address shown in the UI; a third party may pay it
  at any time. Time-critical inbound → persistent watch *while hot*.
- **Internal address** = any output the UI never displays (change, and any
  programmatic output the user never sees). We originate every spend of it, so
  after its funding output is final there is no event we don't already control
  → metered scans only.

This generalizes the BIP44 external/internal split: the policy axis is "will a
human ever see this address," not "is this change."

## 4. Target scheme

- **Persistent watch set = hot external addresses only.** These are the
  current/next deposit address and any external address we are actively
  expecting funds on.
- **Internal addresses are never persistently watched** (strong form). The
  only event awaited on a change output is confirmation, which is block-paced;
  a metered `get_history` (electrum) or bounded rescan (neutrino) driven by tip
  changes catches the mined→final transition within one block, which is free
  relative to the multi-block finality wait it gates. Track from creation to
  finality, then forget.
  - Weaker fallback if per-tip metering proves insufficient: subscribe an
    internal address only between creation and **finality** (not "seen" —
    mempool/1-conf are inside the reorg/RBF window), then unwatch.
- **Cold external addresses are demoted to periodic metered scans.** An
  external address that has been quiet for a long time and is not the
  current/next deposit address moves from persistent watch to periodic
  `get_history`. A metered hit re-promotes it to the hot set.
- **Restore/recovery** uses the same metered primitive as a gap-limit sweep —
  separate concern, shared machinery (see §8).

## 5. Architecture: where the policy lives

The central design decision. The watch set is currently built in the shared
btcwallet layer, branch-agnostically, and that layer will happily re-add
internal addresses behind any chain-client-level pruning (address-pool
maintenance, new-address generation). So pruning purely inside a backend would
fight the wallet layer.

Because we own the btcwallet fork, the recommended home is the **shared layer**,
made branch-aware, delegating mechanism to the backend through a small
capability interface:

```text
            wallet.Wallet (shared, fork)
                    |
       watch policy (branch-aware): external -> Watch (persistent)
                                    internal -> finality tracker -> Scan + Unwatch
                                    cold ext -> demote: Unwatch + periodic Scan
                    |
        +-----------+------------------------+
        |                                    |
  ElectrumClient                       NeutrinoClient
  Watch   = subscribe                  Watch   = AddAddrs (rescan.Update)
  Unwatch = unsubscribe                Unwatch = NOT SUPPORTED today (see §6)
  Scan    = get_history                Scan    = bounded rescan over [from,tip]
```

Proposed backend capability (an optional interface alongside `chain.Backend`,
not a breaking change to it):

```go
// AddressWatcher is implemented by chain.Backends that support a tiered
// watch set. Backends that don't implement it fall back to today's
// add-only NotifyReceived behaviour.
type AddressWatcher interface {
    Watch(addrs []btcutil.Address) error             // persistent
    Unwatch(addrs []btcutil.Address) error           // drop persistent watch
    Scan(addrs []btcutil.Address, fromHeight int32) (map[OutPoint]*ScanHit, error) // metered
}
```

The shared layer keeps `NotifyReceived`/`Rescan` for compatibility but, when
the backend implements `AddressWatcher`, routes external vs internal
differently and runs the finality/demotion logic. Branch information is already
available in `waddrmgr` and `FilterBlocksRequest`; the missing plumbing is to
carry it to the watch decision instead of flattening it through
`NotifyReceived`.

Alternative homes considered:

1. **Entirely inside each chain client.** Rejected as the primary approach: the
   client would have to reconstruct branch + finality it isn't handed, and the
   wallet layer would re-add internals anyway.
2. **Extend `chain.Backend` itself** with branch-aware watch verbs. Cleanest
   conceptually but the most invasive to the btcwallet-derived contract and to
   every consumer. Hold unless the optional-interface approach proves leaky.

## 6. Per-backend mechanics

### Electrum

All primitives already exist:

- `Watch` → `Network.SubscribeScripthash` (`electrum/network.go:279`).
- `Unwatch` → `Network.UnsubscribeScripthash` (`network.go:288`); wired through
  to the protocol's `blockchain.scripthash.unsubscribe`.
- `Scan` → `Network.GetHistory` (`network.go:297`). Already used on
  *unsubscribed* addresses today: `FilterBlocks` calls `fetchHistory` →
  `GetHistory` without subscribing (`chain/electrum_rescan.go`). Use
  `get_history` non-empty as the "used" test, **not** `get_balance`/`listunspent`
  (a used-then-emptied address has zero balance/UTXOs but non-empty history).

So the electrum side is mostly wiring plus the policy layer; no new protocol
work.

### Neutrino

Two gaps:

- **No `Unwatch`.** `NeutrinoClient.NotifyReceived` only adds
  (`rescan.Update(spv.AddAddrs(...))`); the spv rescan exposes `AddAddrs`
  (`spv/rescan.go:1431`) but **no removal** option. Implementing `Unwatch`
  requires either (a) adding a `RemoveAddrs`/filter-rebuild path to the forked
  spv rescan, or (b) tearing down and restarting the rescan with the reduced
  set. Option (a) is more work but avoids resync churn; option (b) is simpler
  but re-walks from the new start height.
- **No `get_history` equivalent.** A metered internal-address check is a
  **bounded rescan** over `[fromHeight, tip]` (cfilter match over the range,
  fetch only matching blocks). Cost scales with the block range, not address
  count, so meter at the granularity of "the change output's active window"
  (creation height → finality), which is small. This reuses the existing
  `FilterBlocks` machinery, scoped to a range.

Because neutrino's watch-set pressure is soft, a reasonable first neutrino
increment is: stop *persistently* adding internal addresses to the cfilter
watch list, and instead bounded-rescan them to finality. `Unwatch` of already
finalized addresses can come second.

## 7. Policy and parameters to define

- **Finality threshold** for unwatch / "forget internal" — reuse the wallet's
  `confTxFinality`.
- **Hot → cold demotion** for external: criteria (e.g. quiet for N blocks and
  not the current/next deposit address) and whether the current gap-limit
  lookahead window stays hot.
- **Metering cadence**: electrum can meter per-tip cheaply; neutrino metering is
  a bounded rescan, so meter less often / batch internal addresses into one
  range scan.
- **Re-promotion**: a metered hit on a cold external (or an unexpected hit on an
  internal) promotes back to persistent watch and emits a tx.
- **Persistence + reconnect**: the hot/cold/finalized tiering must survive
  restart; on (re)connect, re-establish only the hot persistent set and run a
  metered sweep for the rest. Keeping the hot set small is what makes electrum
  reconnect cheap.

## 8. Interaction with recovery

This scheme governs **steady-state** watching; restore-time discovery is the
recovery problem. They compose:

- Electrum recovery = gap-limit sweep of `get_history` over derived address
  batches (the deferred work behind `recovery_window=0`). Same primitive as
  `Scan`; same metering/throttling machinery.
- Neutrino recovery = the existing block-walk recovery, which is the right
  model there.

Design the electrum `Scan`/metering layer so the recovery sweep and the
steady-state cold-address checks share it.

## 9. Phased implementation plan

1. **Phase 0 — measure.** Instrument current subscription / watch-list size as
   a wallet ages (harness + a long-running soak). Confirms the electrum cap is
   actually reached and quantifies the neutrino bandwidth cost. Cheap; prevents
   building against an imagined problem.
2. **Phase 1 — electrum, policy + interface.** Define `AddressWatcher`; make the
   shared layer branch-aware; route external → `Watch`, internal → finality
   tracker using `Scan` + `Unwatch`. Electrum first because all primitives
   exist. Land cold-external demotion here or in Phase 3.
3. **Phase 2 — neutrino mechanics.** Add `Unwatch` (removal or rescan-restart)
   and range-scoped `Scan` (bounded rescan) to `NeutrinoClient`; implement the
   same policy via the same shared layer.
4. **Phase 3 — cold external + recovery integration.** Demotion of quiet
   external addresses to periodic `Scan`; fold the electrum gap-limit recovery
   sweep onto the shared metering layer.
5. **Phase 4 — persistence + reconnect hardening.** Tiering state across
   restart; minimal re-subscribe on reconnect; metered catch-up for the cold
   set.

## 10. Risks and open questions

- **btcwallet re-adds internals.** The shared layer's address-pool maintenance
  and new-address paths currently feed internals into the watch set; the policy
  must intercept these, not just prune after the fact. Confirm every
  `NotifyReceived` / `activeData` feed is routed through the new policy.
- **Neutrino removal cost.** Add `RemoveAddrs` to the forked spv rescan vs
  restart-rescan: pick based on resync churn vs implementation complexity
  (Phase 0/2).
- **Detection-latency budget.** Demoting addresses to metering trades instant
  notification for poll-interval latency. Acceptable for a wallet (a delayed
  balance update, recovered next poll); it is **not** acceptable for the
  swap/redemption engine — but the lean wallet is shedding that, so the budget
  is generous here. State the budget explicitly per address tier.
- **Reorg past finality.** Once an internal address is forgotten, a reorg deeper
  than `confTxFinality` would be missed; the periodic metered sweep is the
  cheap safety net. Decide whether cold internals get any sweep at all or are
  truly fire-and-forget.
- **Address reuse / leaked internal address.** A third party paying a change
  address is caught only by a metered sweep, not instantly. Negligible in
  practice (change isn't shared) but note it.

## 11. Verification (harness)

- Persistent watch-set size stays bounded as the wallet ages and sends many
  transactions (the core regression this prevents).
- External receive detected while hot (instant) and while cold (within the
  metering interval).
- Change output: created → watched to finality → dropped; a subsequent spend of
  that change UTXO still succeeds (it is in the wallet UTXO set independent of
  the watch).
- Restored seed discovers funds at gap-limit addresses (recovery sweep) —
  shared with the recovery work.
- Both backends exercised by the same scenario suite to keep policy parity.
