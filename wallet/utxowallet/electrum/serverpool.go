// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	now := time.Now()
	serverHealth.mtx.Lock()
	defer serverHealth.mtx.Unlock()

	var healthy, backedOff []*ServerAddr
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
	return append(healthy, backedOff...)
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

	// quarantineTerm is the persisted exclusion for misbehavior (checkpoint
	// or retarget violations, invalid work, losing a verified divergence).
	// In-process the exclusion never expires; the term covers restarts.
	quarantineTerm = 24 * time.Hour

	// serverDBName is the pool database filename, beside the header store.
	serverDBName = "servers.json"
)

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
	return p
}

// save persists the pool; best-effort (logged, never fatal — the pool
// rebuilds from shipped seeds and discovery).
func (p *serverPool) save() {
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

// addDiscovered folds peers advertised by recommender into the pool.
func (p *serverPool) addDiscovered(recommender string, peers []*ServerAddr) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	now := time.Now()
	changed := false
	for _, sv := range peers {
		e := p.entries[sv.Addr]
		if e == nil {
			e = &poolEntry{Server: sv, FirstSeen: now}
			p.entries[sv.Addr] = e
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
		p.save()
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
		p.save()
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
		p.save()
	}
}

func (p *serverPool) recordFailure(addr string) {
	recordServerFailure(addr)
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if e := p.entries[addr]; e != nil {
		e.Failures++
		p.save()
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
// one per recommender before seconds, limiting how far a single
// recommending operator can stack the observer panel.
func (p *serverPool) observerCandidates() []*ServerAddr {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	now := time.Now()
	out := orderCandidates(p.notQuarantinedLocked(p.shipped, now))
	var discovered []*poolEntry
	for _, e := range p.entries {
		if !e.Shipped && !quarantinedLocked(e, now) {
			discovered = append(discovered, e)
		}
	}
	seenRec := make(map[string]int)
	// Stable pass: take servers whose first recommender is least used so
	// far; repeat until exhausted.
	for len(discovered) > 0 {
		best := -1
		for i, e := range discovered {
			rec := ""
			if len(e.Recommenders) > 0 {
				rec = e.Recommenders[0]
			}
			if best == -1 || seenRec[rec] < seenRec[firstRec(discovered[best])] {
				best = i
			}
		}
		e := discovered[best]
		seenRec[firstRec(e)]++
		out = append(out, e.Server)
		discovered = append(discovered[:best], discovered[best+1:]...)
	}
	return out
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
