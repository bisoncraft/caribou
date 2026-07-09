# Multi-server strategy for the electrum backend

Planning document for moving the electrum network layer from a single trusted
server to a server pool. Companion to `electrum-sync-recovery-plan.md` (initial
sync) and `address-watching-plan.md` (steady-state subscriptions); this
document covers connection topology, failover, and tip integrity.

## Status

Phase 1 (failover) implemented on `utxosync`: the wallet passes the full
built-in list, `Start` elects one connection in health-scored order
(`serverpool.go` — process-lifetime failure records, 10-minute backoff,
least-recently-failed retry ordering), and every failure path (dial,
handshake, header-sync, unexpected disconnect) demotes the server so the
next election — including the wallet-level reconnect that follows a
mid-session death — rotates instead of retrying it. In-place rotation
without the network rebuild was considered and deliberately deferred: the
rebuild path is cheap post-anchor (seconds) and reuses hardened machinery,
while an in-place node swap would rewire the delicate loop/teardown
ownership for little gain. Phases 2–4 remain proposed.

## 1. Why

- **Availability.** A server outage is currently a hard failure: the node
  watcher tears the network down and the wallet's reconnect loop rebuilds the
  whole client — always against the same server. `electrum.blockstream.info`
  (the shipped BTC mainnet entry) was observed unreachable on both its
  electrum ports for an extended period while other public servers answered.
- **Tip integrity.** Checkpoint anchors pin the past, but a single server can
  still eclipse the wallet at the tip or lie by omission about address
  history. Electrum proper holds ~10 connections primarily to compare tips.
- **Broadcast censorship.** A lone server can silently drop submitted
  transactions.

## 2. Topology: one wallet server, several observers

Electing a single **wallet server** for all address-level traffic
(subscriptions, `get_history`, probes, sweeps) preserves two invariants:

- The `chain.ElectrumClient` layer assumes one continuous notification
  stream (rescan handoff, `goLive`, tip delivery). A pool that load-balances
  wallet traffic would break it; a pool that elects one stream leaves it
  untouched.
- Address queries reveal the wallet to the server they are sent to.
  Spreading them across the pool would multiply the privacy exposure, not
  reduce it. One wallet server keeps the exposure surface exactly as today.

**Observer servers** (2–3) carry only header subscriptions and broadcast
fan-out — no address-level traffic, so no added privacy cost.

Existing machinery makes the switch-over cheap:

- Headers are server-agnostic: checkpoint anchor + retarget + PoW-window
  validation apply regardless of source, and the anchor bounds re-validation
  against a new server to a few thousand headers.
- Scripthash subscriptions self-heal across a switch by construction:
  `addrCache` holds the full set, and the `lastStatus`/`lastHistory` diff
  means a replayed subscribe emits exactly what was missed during the gap.
- The watch policy bounds the persistent subscription set, keeping the
  replay (and per-session server caps) small regardless of wallet age.

## 3. Phases

### Phase 1 — Failover (availability)

Consume the full `ElectrumServers` list and maintain a health-scored
candidate set. `Start` elects one connection, dialing candidates in scored
order; every failure path demotes the failing server. As built, the
wallet-level reconnect IS the rotation mechanism for mid-session failures:
the death demotes the server, teardown proceeds as before, and the rebuilt
Network elects the next candidate — whose chain is re-validated against the
header store (anchor-bounded) and whose subscriptions are re-established by
the normal sync. An in-place swap (no rebuild) is a possible later
refinement if reconnect cost ever matters.

### Phase 2 — Observer tips (integrity)

Hold header subscriptions on 2–3 observers (the next candidates after the
wallet server; an observer death demotes it and elects a replacement without
touching the wallet stream). Compare tips against the wallet server
continuously. Policy (decided):

- **Lag → rotate, silently.** The wallet server is lagging when the best
  observer tip is at least 20 minutes' worth of blocks ahead at the chain's
  target spacing (BTC 2 blocks, LTC 8), and that state has persisted
  through a time buffer (10 minutes) — two blocks mined seconds apart and
  reported by an observer first is not a reason to switch. Rotation is the
  C1 mechanism: demote and re-elect.
- **Divergence → verify work, then suspend spends, silently.** When an
  observer's chain conflicts beyond the reorg window, first make the
  dissenter demonstrate work: fetch the divergent headers and PoW-check
  them. Unverified dissent just demotes the observer (a lone malicious
  observer must not be able to freeze the wallet — work is unforgeable,
  majorities of three are not). Verified divergence suspends
  spend-affecting operations and attempts resolution by rotation.
- **No proactive UI; full logging.** Degraded states (lag, divergence,
  observer loss, refused broadcasts) surface nothing in the UI but are
  always logged — verified divergence and refused broadcasts at error
  level. The user sees an error only when they attempt an operation that
  cannot be performed safely — including the case where every connection
  is down.

### Phase 3 — Broadcast fan-out

Implemented. The wallet server's answer stays authoritative: on its accept,
the observers receive the transaction asynchronously (censorship then
requires every connected operator to collude); on its rejection, the
observers are tried synchronously and any accept both publishes the
transaction and logs the wallet server's refusal as suspect. Responses
meaning "already known" — worded differently across server implementations —
count as success, with the txid derived locally when no server returns one.
No privacy change (the transaction is being published).

### Phase 4 — Peer discovery + list hygiene

Policy (decided):

- **Discovery.** Poll `server.peers.subscribe` on connected servers at low
  cadence; accept candidates with an SSL port and protocol >= 1.4, onion
  addresses only when a Tor proxy is configured. A discovered server proves
  itself the same way shipped ones do — genesis at handshake, header
  validation if it ever serves headers; the recommendation itself conveys
  no trust.
- **Persistence.** A per-chain server database lives beside the header
  store (`DataDir/electrum/<chain>/servers.json`) — NOT in the wallet
  database: server reputation is environmental, chain-scoped knowledge with
  the header store's lifecycle, while wallet.db is seed-derived state that
  rebuilds (RecoverWallet) exactly when server memory matters most. Entries
  carry source (shipped or discovered, with recommenders), first/last seen,
  and success/failure history; dead discovered entries age out. The shipped
  list merges in as permanent seeds on every load.
- **Tiered election (Sybil defense).** Discovery is an eclipse vector — an
  attacker recommends its own clones. The wallet server is elected from the
  shipped tier; at least one observer is always shipped; remaining observer
  slots fill from discovered peers preferring recommender/network
  diversity; broadcast fan-out accepts anyone (a bad server can only fail
  to relay).
- **Gated fallback.** When every shipped server has been failing
  continuously for a sustained window (30 minutes), an aged discovered
  server — prior successes or multiple independent recommenders, not
  freshly appeared — may be elected wallet server, logged at error level.
  Shipped servers keep being re-dialed each election and win back the slot
  as soon as one answers. Spends stay enabled: UTXO validity comes from the
  wallet's own validated history, and checkpoint anchors plus the PoW
  window bound what any wallet server can fabricate.

The shipped lists become bootstrap seeds rather than the universe; the
release-time probe of the lists stays on the release checklist.

## 4. Interactions

- **Session caps**: each server enforces its own subscription cap; the
  bounded watch set from `address-watching-plan.md` is what makes both
  failover replay and any future multi-server subscription scheme viable.
- **Checkpoint anchors**: `electrum-sync-recovery-plan.md` work bounds the
  cost of validating any new server's chain; anchors must keep pace with
  `DefaultWalletBirthday` at release.
- **Tor**: the single `TorProxy` config applies per-connection today and
  extends to the pool unchanged.

## 5. Risks and open questions

- **Connection budget.** 3–4 sockets per asset is trivial on desktop;
  confirm acceptable for mobile battery/network profiles before defaulting
  observers on there.
- **Observer set quality.** Observers drawn from one operator (or one
  hosting provider) give false confidence; selection should prefer operator
  diversity once discovery (Phase 4) lands. LTC is the acute case: only
  three known-live public mainnet servers exist (two tied to the
  electrum-ltc project), so LTC observation is one observer with no spares
  until discovery lands or a project-operated server ships.
- **Wallet-server stickiness.** Rotating the wallet server changes which
  operator sees the wallet's addresses. Prefer sticky election with health
  hysteresis over frequent rotation.
