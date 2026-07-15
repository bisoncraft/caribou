// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// serverHealth is a process-lifetime record of per-server connection health,
// keyed by server address. It deliberately lives at package level: the wallet
// rebuilds the Network on every (re)connect, and rotation only works if the
// record of which servers just failed survives that rebuild. In-memory is
// enough — a fresh process re-learning a dead server costs one dial timeout.
var serverHealth = struct {
	mtx sync.Mutex
	m   map[string]*serverRecord
	// quarantined is the process-lifetime misbehavior blacklist. Unlike
	// failure backoff (transient, retried), quarantine is earned by
	// protocol violations and never expires within the process; the
	// persisted 24-hour term (poolEntry.QuarantinedUntil) covers restarts.
	quarantined map[string]bool
}{m: make(map[string]*serverRecord), quarantined: make(map[string]bool)}

type serverRecord struct {
	failures    int
	lastFailure time.Time
}

// serverFailureBackoff is how long a failed server is deprioritized before
// being retried. Scored, not banned: if every candidate is inside the
// backoff window the least-recently-failed one is tried anyway.
const serverFailureBackoff = 10 * time.Minute

func recordServerFailure(addr string) {
	serverHealth.mtx.Lock()
	defer serverHealth.mtx.Unlock()
	r := serverHealth.m[addr]
	if r == nil {
		r = &serverRecord{}
		serverHealth.m[addr] = r
	}
	r.failures++
	r.lastFailure = time.Now()
}

func recordServerSuccess(addr string) {
	serverHealth.mtx.Lock()
	defer serverHealth.mtx.Unlock()
	delete(serverHealth.m, addr)
}

// orderCandidates returns the candidate servers in election order: healthy
// ones first in their configured order (the shipped list is preference-
// ordered), then backed-off ones from least- to most-recently failed. The
// wallet-server election is sticky by construction elsewhere — this ordering
// only decides who is dialed next after a failure or restart.
func orderCandidates(servers []*ServerAddr) []*ServerAddr {
	healthy, backedOff := partitionBackoff(servers)
	return append(healthy, backedOff...)
}

// partitionBackoff splits servers into those with no recent dial failure and
// those still inside the failure backoff, the latter ordered
// least-recently-failed first.
func partitionBackoff(servers []*ServerAddr) (healthy, backedOff []*ServerAddr) {
	now := time.Now()
	serverHealth.mtx.Lock()
	defer serverHealth.mtx.Unlock()

	for _, s := range servers {
		r := serverHealth.m[s.Addr]
		if r == nil || now.Sub(r.lastFailure) >= serverFailureBackoff {
			healthy = append(healthy, s)
		} else {
			backedOff = append(backedOff, s)
		}
	}
	// Least-recently-failed first among the backed-off.
	for i := 1; i < len(backedOff); i++ {
		for j := i; j > 0; j-- {
			a := serverHealth.m[backedOff[j-1].Addr]
			b := serverHealth.m[backedOff[j].Addr]
			if a != nil && b != nil && a.lastFailure.After(b.lastFailure) {
				backedOff[j-1], backedOff[j] = backedOff[j], backedOff[j-1]
			}
		}
	}
	return healthy, backedOff
}

// ─── Persistent server pool (multi-server phase 4) ──────────────────────────

const (
	// discoveredAgeMin is how long a discovered server must have been known
	// before it is eligible as a fallback wallet server — freshly appeared
	// servers (an attacker's sybils arriving just in time) never qualify.
	discoveredAgeMin = 24 * time.Hour

	// shippedDownWindow is how long every shipped server must have been
	// failing continuously before the fallback tier opens.
	shippedDownWindow = 30 * time.Minute

	// discoveredExpiry ages a discovered server out of the database when it
	// has neither been seen advertised nor answered for this long.
	discoveredExpiry = 30 * 24 * time.Hour

	// maxRecommenders caps the recommender list per entry.
	maxRecommenders = 5

	// maxDiscoveredPerRecommender caps how many pool entries a single
	// recommender may introduce (be the first to advertise).
	// maxRecommenders bounds the list per entry, not entries per
	// recommender, so without this cap one hostile server could flood the
	// pool with thousands of sybils in a single server.peers reply. 16
	// comfortably exceeds what observer election ever consumes from one
	// operator while keeping a lone recommender's footprint small.
	maxDiscoveredPerRecommender = 16

	// maxDiscovered caps the total discovered (non-shipped) entries. 256
	// is far more fallback depth than elections ever walk, and it bounds
	// both the persisted file size and the cost of any full scan of the
	// entry set. When the cap is hit, existing entries are preferred over
	// new ones: an established entry has age working toward fallback
	// eligibility (discoveredAgeMin), and evicting it for an unknown would
	// let an attacker churn honest servers out of the pool.
	maxDiscovered = 256

	// quarantineTerm is the persisted exclusion for misbehavior (checkpoint
	// or retarget violations, invalid work, losing a verified divergence).
	// In-process the exclusion never expires; the term covers restarts.
	quarantineTerm = 24 * time.Hour

	// serverDBName is the pool database filename, beside the header store.
	serverDBName = "servers.json"
)

// saveDelay coalesces routine pool writes. A save rewrites the whole file,
// and reputation updates arrive one per dial outcome — a flooded candidate
// list would otherwise turn every failed dial into a full-file rewrite.
// Losing the final delay's worth of health scores in a crash is acceptable;
// quarantines bypass the delay (written immediately) because a misbehavior
// verdict must survive a crash. A variable so tests can tighten it.
var saveDelay = 2 * time.Second

// poolEntry is one known server: a shipped seed or a discovered peer, with
// its durable reputation.
type poolEntry struct {
	Server       *ServerAddr `json:"server"`
	Shipped      bool        `json:"shipped"`
	Recommenders []string    `json:"recommenders,omitempty"`
	FirstSeen    time.Time   `json:"firstSeen"`
	LastSeen     time.Time   `json:"lastSeen"`
	LastSuccess  time.Time   `json:"lastSuccess,omitempty"`
	Successes    int         `json:"successes"`
	Failures     int         `json:"failures"`

	// QuarantinedUntil/QuarantineReason record a persisted misbehavior
	// exclusion; see quarantineTerm.
	QuarantinedUntil time.Time `json:"quarantinedUntil,omitempty"`
	QuarantineReason string    `json:"quarantineReason,omitempty"`
}

// quarantinedLocked reports whether the entry is currently excluded for
// misbehavior — by the process-lifetime blacklist or the persisted term.
func quarantinedLocked(e *poolEntry, now time.Time) bool {
	serverHealth.mtx.Lock()
	inProc := serverHealth.quarantined[e.Server.Addr]
	serverHealth.mtx.Unlock()
	return inProc || now.Before(e.QuarantinedUntil)
}

// serverPool is the per-chain server database: the shipped seeds plus
// discovered peers, persisted beside the header store. Election ordering
// within a tier still uses the process-lifetime health registry (fast
// backoff); the pool carries the durable, cross-restart reputation and the
// shipped/discovered tiering.
type serverPool struct {
	mtx     sync.Mutex
	path    string
	shipped []*ServerAddr
	entries map[string]*poolEntry

	// dirty and saveTimer implement the coalesced save (see saveDelay).
	// saveCount counts completed writes; test instrumentation.
	dirty     bool
	saveTimer *time.Timer
	saveCount int

	// shippedDownSince is set when every shipped server is observed in
	// failure backoff, and cleared by any shipped success. It gates the
	// discovered-fallback tier.
	shippedDownSince time.Time
}

// newServerPool loads (or initializes) the pool database at dir, merging the
// shipped servers as permanent seeds.
func newServerPool(dir string, shipped []*ServerAddr) *serverPool {
	p := &serverPool{
		path:    filepath.Join(dir, serverDBName),
		shipped: shipped,
		entries: make(map[string]*poolEntry),
	}
	if raw, err := os.ReadFile(p.path); err == nil {
		var stored []*poolEntry
		if err := json.Unmarshal(raw, &stored); err != nil {
			log.Warnf("electrum: unreadable server database %s: %v", p.path, err)
		} else {
			now := time.Now()
			for _, e := range stored {
				if e.Server == nil || e.Server.Addr == "" {
					continue
				}
				if !e.Shipped && now.Sub(e.LastSeen) > discoveredExpiry &&
					now.Sub(e.LastSuccess) > discoveredExpiry {
					continue // aged out
				}
				p.entries[e.Server.Addr] = e
			}
		}
	}
	now := time.Now()
	for _, sv := range shipped {
		e := p.entries[sv.Addr]
		if e == nil {
			e = &poolEntry{Server: sv, FirstSeen: now}
			p.entries[sv.Addr] = e
		}
		e.Shipped = true
		e.LastSeen = now
	}
	// Enforce the discovered cap on load too: a database written before the
	// caps existed (or flooded on disk by an attacker) must not carry an
	// oversized set into the election scans. Actively quarantined entries
	// are kept first — dropping one would erase its persisted exclusion —
	// then previously successful ones, then the oldest.
	var discovered []*poolEntry
	for _, e := range p.entries {
		if !e.Shipped {
			discovered = append(discovered, e)
		}
	}
	if len(discovered) > maxDiscovered {
		sort.Slice(discovered, func(i, j int) bool {
			a, b := discovered[i], discovered[j]
			if aq, bq := now.Before(a.QuarantinedUntil), now.Before(b.QuarantinedUntil); aq != bq {
				return aq
			}
			if as, bs := a.Successes > 0, b.Successes > 0; as != bs {
				return as
			}
			return a.FirstSeen.Before(b.FirstSeen)
		})
		for _, e := range discovered[maxDiscovered:] {
			delete(p.entries, e.Server.Addr)
		}
	}
	return p
}

// saveLocked persists the pool immediately; best-effort (logged, never fatal
// — the pool rebuilds from shipped seeds and discovery). Must be called with
// mtx held. Routine reputation updates should go through scheduleSaveLocked
// instead; only writes that must survive a crash (quarantines) belong here.
func (p *serverPool) saveLocked() {
	p.dirty = false
	p.saveCount++
	entries := make([]*poolEntry, 0, len(p.entries))
	for _, e := range p.entries {
		entries = append(entries, e)
	}
	raw, err := json.MarshalIndent(entries, "", "\t")
	if err == nil {
		err = os.WriteFile(p.path, raw, 0o600)
	}
	if err != nil {
		log.Warnf("electrum: persist server database: %v", err)
	}
}

// scheduleSaveLocked marks the pool dirty and arms the coalescing timer if it
// is not already armed; the pending flush picks up whatever is dirty when it
// fires. Must be called with mtx held.
func (p *serverPool) scheduleSaveLocked() {
	p.dirty = true
	if p.saveTimer == nil {
		p.saveTimer = time.AfterFunc(saveDelay, p.flush)
	}
}

// flush disarms the coalescing timer and persists the pool if it is dirty.
// Runs as the timer callback and at network teardown.
func (p *serverPool) flush() {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	p.flushLocked()
}

func (p *serverPool) flushLocked() {
	if p.saveTimer != nil {
		p.saveTimer.Stop()
		p.saveTimer = nil
	}
	if p.dirty {
		p.saveLocked()
	}
}

// pruneExpiredLocked drops discovered entries neither seen advertised nor
// successful within discoveredExpiry. newServerPool applies the same filter
// at load, but a long-lived process never reloads, so expiry must also run
// at runtime; addDiscovered calls this before admitting new peers, which
// keeps the discovered set's turnover paced by real discovery traffic.
// Returns whether anything was pruned. Must be called with mtx held.
func (p *serverPool) pruneExpiredLocked(now time.Time) bool {
	changed := false
	for addr, e := range p.entries {
		if e.Shipped {
			continue
		}
		if quarantinedLocked(e, now) {
			// Dropping an actively quarantined entry would erase its
			// persisted exclusion (the load trim protects these for the
			// same reason). The quarantine term is far shorter than the
			// expiry, so the entry prunes on a later pass.
			continue
		}
		if now.Sub(e.LastSeen) > discoveredExpiry &&
			now.Sub(e.LastSuccess) > discoveredExpiry {
			delete(p.entries, addr)
			changed = true
		}
	}
	return changed
}

// addDiscovered folds peers advertised by recommender into the pool, bounded
// two ways: a recommender may introduce at most maxDiscoveredPerRecommender
// entries, and the discovered set never grows past maxDiscovered. Both
// bounds prefer keeping existing entries over inserting new ones (see the
// constants' comments); an existing entry advertised again still refreshes
// its LastSeen and may gain the recommender.
func (p *serverPool) addDiscovered(recommender string, peers []*ServerAddr) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	now := time.Now()
	changed := p.pruneExpiredLocked(now)

	// Current bound usage. An entry's introducer is its first recommender:
	// addDiscovered always appends the recommender to a freshly created
	// entry (the list is empty, under maxRecommenders), so Recommenders[0]
	// is whoever first advertised it.
	discovered, introduced := 0, 0
	for _, e := range p.entries {
		if e.Shipped {
			continue
		}
		discovered++
		if firstRec(e) == recommender {
			introduced++
		}
	}

	for _, sv := range peers {
		e := p.entries[sv.Addr]
		if e == nil {
			if discovered >= maxDiscovered ||
				introduced >= maxDiscoveredPerRecommender {
				continue
			}
			e = &poolEntry{Server: sv, FirstSeen: now}
			p.entries[sv.Addr] = e
			discovered++
			introduced++
			changed = true
		}
		e.LastSeen = now
		found := false
		for _, r := range e.Recommenders {
			if r == recommender {
				found = true
				break
			}
		}
		if !found && len(e.Recommenders) < maxRecommenders {
			e.Recommenders = append(e.Recommenders, recommender)
			changed = true
		}
	}
	if changed {
		p.scheduleSaveLocked()
	}
}

// recordSuccess/recordFailure update both the fast health registry and the
// durable reputation.
func (p *serverPool) recordSuccess(addr string) {
	recordServerSuccess(addr)
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if e := p.entries[addr]; e != nil {
		e.Successes++
		e.LastSuccess = time.Now()
		e.LastSeen = e.LastSuccess
		if e.Shipped {
			p.shippedDownSince = time.Time{}
		}
		p.scheduleSaveLocked()
	}
}

// quarantine excludes a misbehaving server from every election: process-
// lifetime in memory, quarantineTerm on disk. Slowness and connection
// failures do NOT belong here — they take the backoff path (recordFailure).
func (p *serverPool) quarantine(addr, reason string) {
	serverHealth.mtx.Lock()
	serverHealth.quarantined[addr] = true
	serverHealth.mtx.Unlock()
	log.Warnf("electrum: quarantining %s for %v: %s", addr, quarantineTerm, reason)
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if e := p.entries[addr]; e != nil {
		e.QuarantinedUntil = time.Now().Add(quarantineTerm)
		e.QuarantineReason = reason
		e.Failures++
		// Immediate write, bypassing the coalescing delay: losing a few
		// seconds of health scores in a crash is fine, losing a
		// quarantine is not.
		p.dirty = true
		p.flushLocked()
	}
}

func (p *serverPool) recordFailure(addr string) {
	recordServerFailure(addr)
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if e := p.entries[addr]; e != nil {
		e.Failures++
		p.scheduleSaveLocked()
	}
}

// fallbackEligible reports whether the discovered tier may serve as wallet
// server: every shipped server has been failing continuously for
// shippedDownWindow. Must be called with mtx held.
func (p *serverPool) fallbackEligibleLocked(now time.Time) bool {
	serverHealth.mtx.Lock()
	allDown := true
	for _, sv := range p.shipped {
		r := serverHealth.m[sv.Addr]
		if r == nil || now.Sub(r.lastFailure) >= serverFailureBackoff {
			allDown = false
			break
		}
	}
	serverHealth.mtx.Unlock()
	if !allDown {
		p.shippedDownSince = time.Time{}
		return false
	}
	if p.shippedDownSince.IsZero() {
		p.shippedDownSince = now
	}
	return now.Sub(p.shippedDownSince) >= shippedDownWindow
}

// agedDiscoveredLocked returns discovered servers eligible for wallet-server
// fallback: known for at least discoveredAgeMin AND either previously
// successful or advertised by multiple independent recommenders.
func (p *serverPool) agedDiscoveredLocked(now time.Time) []*ServerAddr {
	var out []*ServerAddr
	for _, e := range p.entries {
		if e.Shipped || now.Sub(e.FirstSeen) < discoveredAgeMin {
			continue
		}
		if e.Successes > 0 || len(e.Recommenders) >= 2 {
			out = append(out, e.Server)
		}
	}
	return out
}

// walletCandidates returns the wallet-server election order: the shipped
// tier (health-ordered), extended by aged discovered servers only when the
// gated fallback is open. Entering fallback is logged at error level; the
// UI stays quiet per policy.
func (p *serverPool) walletCandidates() []*ServerAddr {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	now := time.Now()
	out := orderCandidates(p.notQuarantinedLocked(p.shipped, now))
	if p.fallbackEligibleLocked(now) {
		aged := orderCandidates(p.notQuarantinedLocked(p.agedDiscoveredLocked(now), now))
		if len(aged) > 0 {
			log.Errorf("electrum: every shipped server failing for %v; opening "+
				"wallet-server election to %d aged discovered servers",
				time.Since(p.shippedDownSince).Round(time.Second), len(aged))
			out = append(out, aged...)
		}
	}
	return out
}

// observerCandidates returns the observer election order: shipped first (so
// the observer set always prefers, and with slots available includes, a
// shipped server), then discovered peers ordered for recommender diversity —
// one per introducing recommender before seconds, limiting how far a single
// recommending operator can stack the observer panel. The interleave is
// group-then-round-robin (linear in the discovered set, which maxDiscovered
// also bounds) rather than a per-pick rescan.
func (p *serverPool) observerCandidates() []*ServerAddr {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	now := time.Now()
	shipped := p.notQuarantinedLocked(p.shipped, now)
	groups := make(map[string][]*poolEntry)
	var order []string
	for _, e := range p.entries {
		if e.Shipped || quarantinedLocked(e, now) {
			continue
		}
		rec := firstRec(e)
		if _, seen := groups[rec]; !seen {
			order = append(order, rec)
		}
		groups[rec] = append(groups[rec], e)
	}
	var disc []*ServerAddr
	for round := 0; ; round++ {
		took := false
		for _, rec := range order {
			if g := groups[rec]; round < len(g) {
				disc = append(disc, g[round].Server)
				took = true
			}
		}
		if !took {
			break
		}
	}
	// Backoff partitioning spans BOTH tiers: shipped servers are preferred
	// among candidates in equal standing, but a recently-failed shipped
	// server sorts after a fresh discovered one. fillObservers dials a
	// bounded number of candidates per pass, so keeping dead shipped
	// servers at the front would burn the whole budget re-dialing them
	// every tick — with a shipped list at least as long as the budget (LTC
	// ships eight, matching maxObserverDialsPerPass), discovered candidates
	// would never be dialed at all, and the all-shipped-down case is
	// exactly when the wallet is running on a discovered fallback server
	// and observer oversight matters most.
	shippedHealthy, shippedBacked := partitionBackoff(shipped)
	discHealthy, discBacked := partitionBackoff(disc)
	out := make([]*ServerAddr, 0,
		len(shippedHealthy)+len(discHealthy)+len(shippedBacked)+len(discBacked))
	out = append(out, shippedHealthy...)
	out = append(out, discHealthy...)
	out = append(out, shippedBacked...)
	return append(out, discBacked...)
}

func firstRec(e *poolEntry) string {
	if len(e.Recommenders) > 0 {
		return e.Recommenders[0]
	}
	return ""
}

// notQuarantinedLocked filters servers currently excluded for misbehavior.
// Must be called with p.mtx held.
func (p *serverPool) notQuarantinedLocked(servers []*ServerAddr, now time.Time) []*ServerAddr {
	// Fresh slice: filtering in place would corrupt the caller's backing
	// array (p.shipped is long-lived).
	out := make([]*ServerAddr, 0, len(servers))
	for _, sv := range servers {
		if e := p.entries[sv.Addr]; e != nil && quarantinedLocked(e, now) {
			continue
		}
		out = append(out, sv)
	}
	return out
}
