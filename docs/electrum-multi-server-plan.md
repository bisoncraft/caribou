# Multi-server strategy for the electrum backend

Planning document for moving the electrum network layer from a single trusted
server to a server pool. Companion to `electrum-sync-recovery-plan.md` (initial
sync) and `address-watching-plan.md` (steady-state subscriptions); this
document covers connection topology, failover, and tip integrity.

## Status

Proposed. Not started. `electrum.Network` is explicitly phase-1 single-server
("multi-peer failover will be added in a later phase" — `network.go`), and the
wallet consumes only the first entry of the built-in server lists
(`wallet/backends.go`: `np.ElectrumServers[0]`).

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

Consume the full `ElectrumServers` list. Maintain a health-scored candidate
set; on wallet-server failure, rotate in place: dial the next candidate,
re-validate its chain against the header store (anchor-bounded), replay the
subscription set, and resume the notification stream. The wallet's existing
reconnect loop remains the backstop, not the mechanism. No `chain` package
changes — the rotation happens beneath `ElectrumNetwork`.

### Phase 2 — Observer tips (integrity)

Hold header subscriptions on 2–3 observers. Compare tips against the wallet
server continuously:

- **Lagging** wallet server (observers several blocks ahead for longer than a
  threshold) → rotate to the best observer.
- **Diverging** chains past the reorg window → surface a wallet alert and
  suspend spend-affecting operations until resolved; this is the
  eclipse/fork signal a single connection cannot produce.

Policy for thresholds and the user-facing alert shape is the main design
work; the mechanics reuse the existing header validation.

### Phase 3 — Broadcast fan-out

Submit transactions through the wallet server and all observers; success on
any one counts. Cheap censorship insurance, no privacy change worth noting
(the transaction is being published).

### Phase 4 — Peer discovery + list hygiene

Seed from the shipped lists, expand via `server.peers.subscribe`, persist a
scored server database across restarts. The shipped lists become bootstrap
rather than the universe. Refresh the shipped lists as part of this work
(the BTC mainnet list currently leads with a server that was observed down;
each network should ship several vetted entries).

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

- **Divergence policy.** What exactly suspends on a fork signal, and what
  does the user see? Needs a decision before Phase 2 ships.
- **Connection budget.** 3–4 sockets per asset is trivial on desktop;
  confirm acceptable for mobile battery/network profiles before defaulting
  observers on there.
- **Observer set quality.** Observers drawn from one operator (or one
  hosting provider) give false confidence; selection should prefer operator
  diversity once discovery (Phase 4) lands.
- **Wallet-server stickiness.** Rotating the wallet server changes which
  operator sees the wallet's addresses. Prefer sticky election with health
  hysteresis over frequent rotation.
