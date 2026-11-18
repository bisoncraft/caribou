// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

	// CertSPKISHA256, CertNotAfter and CertPinnedAt are the server's learned
	// certificate pin (see certPin), recorded by address and never on the
	// ServerAddr, so a shipped server's current configuration and its
	// learned identity stay separate. Empty when nothing is pinned.
	CertSPKISHA256 string    `json:"certSpkiSha256,omitempty"`
	CertNotAfter   time.Time `json:"certNotAfter,omitempty"`
	CertPinnedAt   time.Time `json:"certPinnedAt,omitempty"`
}

// pin returns the entry's learned certificate pin. Records are validated at
// load (see validatePin), so a stored value always decodes.
func (e *poolEntry) pin() certPin {
	if e.CertPinnedAt.IsZero() {
		return certPin{}
	}
	spki, err := decodePinHex(e.CertSPKISHA256)
	if err != nil {
		return certPin{}
	}
	return certPin{spki: spki, notAfter: e.CertNotAfter, pinnedAt: e.CertPinnedAt}
}

// setPin stores c as the entry's learned pin; a zero c clears it.
func (e *poolEntry) setPin(c certPin) {
	if c.isZero() {
		e.CertSPKISHA256, e.CertNotAfter, e.CertPinnedAt = "", time.Time{}, time.Time{}
		return
	}
	e.CertSPKISHA256 = encodePinHex(c.spki)
	e.CertNotAfter = c.notAfter
	e.CertPinnedAt = c.pinnedAt
}

// validatePin reports whether a loaded record's pin fields form a pin: all
// empty, or a 32-byte hex key with non-zero expiry and pin times.
func (e *poolEntry) validatePin() error {
	if e.CertSPKISHA256 == "" && e.CertNotAfter.IsZero() && e.CertPinnedAt.IsZero() {
		return nil
	}
	if _, err := decodePinHex(e.CertSPKISHA256); err != nil {
		return fmt.Errorf("key hash: %w", err)
	}
	if e.CertNotAfter.IsZero() || e.CertPinnedAt.IsZero() {
		return errors.New("missing expiry or pin time")
	}
	return nil
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

	// pinsDirty is set when a pin's expiry was extended in memory but the
	// write failed; the next successful save clears it, and the next
	// connection to a pinned server retries the write. The in-memory value
	// stays authoritative for this process, so no other dial reopens first
	// use on the stale expiry. If the process restarts before a write
	// succeeds, the older expiry on disk can reopen first use earlier than
	// the observed renewal would allow.
	pinsDirty bool

	// writeFile persists the database (atomicWriteFile; tests substitute a
	// failing writer), and now is the clock for pin expiry (time.Now when
	// nil).
	writeFile func(path string, data []byte) error
	now       func() time.Time

	// learnable reports whether learned pins apply to an endpoint, for
	// ranking observers by the policy a dial would apply (see
	// tofuEndpoint). Nil treats every endpoint as learnable.
	learnable func(addr string) bool

	// shippedDownSince is set when every shipped server is observed in
	// failure backoff, and cleared by any shipped success. It gates the
	// discovered-fallback tier.
	shippedDownSince time.Time
}

// newServerPool loads (or initializes) the pool database at dir, merging the
// shipped servers as permanent seeds. Every address is normalized
// (normalizeAddr), and stored entries whose addresses collide after
// normalization are merged.
//
// A missing database starts a fresh pool. A database that exists but cannot
// be read (permissions, I/O) is an error: it may be intact, and starting
// fresh would overwrite its learned certificate pins on the next save. A
// database whose content does not parse is moved aside to
// servers.json.corrupt-<unix time> and the pool starts fresh, logged at error
// level: that is a deliberate trust reset, since every learned pin it held is
// lost and those servers re-enroll on first use. A record that does not parse,
// or whose pin fields are malformed, loses only that record (or that pin),
// also logged at error level.
func newServerPool(dir string, shipped []*ServerAddr) (*serverPool, error) {
	p := &serverPool{
		path:      filepath.Join(dir, serverDBName),
		entries:   make(map[string]*poolEntry),
		writeFile: atomicWriteFile,
	}
	now := time.Now()
	raw, err := os.ReadFile(p.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("electrum: read server database %s: %w", p.path, err)
	default:
		var records []json.RawMessage
		if err := json.Unmarshal(raw, &records); err != nil {
			aside := p.path + ".corrupt-" + strconv.FormatInt(now.Unix(), 10)
			if rerr := os.Rename(p.path, aside); rerr != nil {
				return nil, fmt.Errorf("electrum: move corrupt server database %s aside: %w", p.path, rerr)
			}
			log.Errorf("electrum: server database %s does not parse (%v); moved to %s. "+
				"Learned certificate pins and server reputation are lost, and servers "+
				"re-enroll their certificates on first use", p.path, err, aside)
			records = nil
		}
		for i, rec := range records {
			e := new(poolEntry)
			if err := json.Unmarshal(rec, e); err != nil {
				log.Errorf("electrum: server database %s record %d does not parse (%v); "+
					"dropped, with any certificate pin it held", p.path, i, err)
				continue
			}
			if e.Server == nil || e.Server.Addr == "" {
				continue
			}
			if err := e.validatePin(); err != nil {
				log.Errorf("electrum: server database entry %s has a malformed certificate "+
					"pin (%v); the pin is dropped and the server re-enrolls on first use",
					e.Server.Addr, err)
				e.setPin(certPin{})
			}
			if !e.Shipped && now.Sub(e.LastSeen) > discoveredExpiry &&
				now.Sub(e.LastSuccess) > discoveredExpiry {
				continue // aged out
			}
			sv := *e.Server
			sv.Addr = normalizeAddr(sv.Addr)
			e.Server = &sv
			if prev := p.entries[sv.Addr]; prev != nil {
				e = mergeEntries(prev, e)
			}
			p.entries[sv.Addr] = e
		}
	}
	// Shipped servers take their configuration from the current release,
	// not from the stored ServerAddr, so a release that pins or unpins a
	// server reaches existing installs; the stored reputation and learned
	// pin carry over.
	seen := make(map[string]bool, len(shipped))
	for _, cfg := range shipped {
		sv := *cfg
		sv.Addr = normalizeAddr(sv.Addr)
		if seen[sv.Addr] {
			continue // a second spelling of a shipped server
		}
		seen[sv.Addr] = true
		p.shipped = append(p.shipped, &sv)
		e := p.entries[sv.Addr]
		if e == nil {
			e = &poolEntry{FirstSeen: now}
			p.entries[sv.Addr] = e
		}
		e.Server = &sv
		e.Shipped = true
		e.LastSeen = now
	}
	// Enforce the discovered cap on load too: a database written before the
	// caps existed (or flooded on disk by an attacker) must not carry an
	// oversized set into the election scans. Actively quarantined entries
	// are kept first (dropping one would erase its persisted exclusion),
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
	return p, nil
}

// mergeEntries combines two stored records for one normalized address. A
// pinned record's pin wins. Two pins for the same key merge to the latest
// expiry and the earliest pin time. Between pins for different keys, the one
// pinned earlier wins (the later one was learned while the earlier was
// already established under another spelling), and both are logged. A quarantine is kept,
// counters add, and the time bounds widen.
func mergeEntries(a, b *poolEntry) *poolEntry {
	out := *a
	pa, pb := a.pin(), b.pin()
	switch {
	case pa.isZero():
		out.setPin(pb)
	case pb.isZero():
	case pa.spki == pb.spki:
		// One key under two spellings: the latest expiry seen and the
		// first pin time both stand.
		merged := pa
		if pb.notAfter.After(merged.notAfter) {
			merged.notAfter = pb.notAfter
		}
		if pb.pinnedAt.Before(merged.pinnedAt) {
			merged.pinnedAt = pb.pinnedAt
		}
		out.setPin(merged)
	default:
		if pb.pinnedAt.Before(pa.pinnedAt) {
			out.setPin(pb)
		}
		log.Errorf("electrum: server database holds two certificate pins for %s under "+
			"different spellings (%x pinned %s, %x pinned %s); keeping the one pinned first",
			a.Server.Addr, pa.spki, pa.pinnedAt.UTC().Format(time.RFC3339),
			pb.spki, pb.pinnedAt.UTC().Format(time.RFC3339))
	}
	out.Shipped = a.Shipped || b.Shipped
	out.Successes += b.Successes
	out.Failures += b.Failures
	if b.FirstSeen.Before(out.FirstSeen) || out.FirstSeen.IsZero() {
		out.FirstSeen = b.FirstSeen
	}
	if b.LastSeen.After(out.LastSeen) {
		out.LastSeen = b.LastSeen
	}
	if b.LastSuccess.After(out.LastSuccess) {
		out.LastSuccess = b.LastSuccess
	}
	if b.QuarantinedUntil.After(out.QuarantinedUntil) {
		out.QuarantinedUntil, out.QuarantineReason = b.QuarantinedUntil, b.QuarantineReason
	}
	for _, r := range b.Recommenders {
		known := false
		for _, have := range out.Recommenders {
			known = known || have == r
		}
		if !known && len(out.Recommenders) < maxRecommenders {
			out.Recommenders = append(out.Recommenders, r)
		}
	}
	return &out
}

// clock returns the pool's notion of now for pin expiry.
func (p *serverPool) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// saveLocked persists the pool immediately and atomically (see
// atomicWriteFile), returning any error. Must be called with mtx held.
// Routine reputation updates should go through scheduleSaveLocked instead;
// only writes that must survive a crash (quarantines, certificate pins)
// belong here.
func (p *serverPool) saveLocked() error {
	p.dirty = false
	p.saveCount++
	entries := make([]*poolEntry, 0, len(p.entries))
	for _, e := range p.entries {
		entries = append(entries, e)
	}
	raw, err := json.MarshalIndent(entries, "", "\t")
	if err == nil {
		err = p.writeFile(p.path, raw)
	}
	if err != nil {
		p.dirty = true
		return fmt.Errorf("electrum: persist server database: %w", err)
	}
	p.pinsDirty = false
	return nil
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

// flushLocked persists the pool if it is dirty, logging a failure. A failed
// write leaves the pool dirty for the next attempt.
func (p *serverPool) flushLocked() {
	if p.saveTimer != nil {
		p.saveTimer.Stop()
		p.saveTimer = nil
	}
	if p.dirty {
		if err := p.saveLocked(); err != nil {
			log.Warnf("%v", err)
		}
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

	for _, adv := range peers {
		norm := *adv
		norm.Addr = normalizeAddr(adv.Addr)
		sv := &norm
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

// tlsPolicy returns the certificate check for a dial to sv and snapshots the
// learned pin the dial starts from (see certpin.go for the precedence).
// learnable reports whether trust on first use applies to the endpoint (see
// tofuEndpoint). A shipped pin past its expiry falls back to the learned pin
// rules where they apply, and stays in force where they do not.
func (p *serverPool) tlsPolicy(sv *ServerAddr, learnable bool) *tlsPolicy {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	return p.tlsPolicyLocked(sv, learnable, p.clock())
}

// tlsPolicyLocked is tlsPolicy at now for a caller holding mtx.
func (p *serverPool) tlsPolicyLocked(sv *ServerAddr, learnable bool, now time.Time) *tlsPolicy {
	if sv.Proto != "ssl" {
		return &tlsPolicy{mode: pinNone}
	}
	var learned certPin
	if e := p.entries[sv.Addr]; e != nil {
		learned = e.pin()
	}
	pol := &tlsPolicy{snapshot: learned, now: now, learnable: learnable}
	switch {
	case len(sv.Cert) > 0:
		shipped := shippedPolicy(sv)
		if !learnable || !now.After(shipped.wantNotAfter) {
			shipped.snapshot, shipped.now, shipped.learnable = learned, now, learnable
			return shipped
		}
	case !sv.SkipVerify || !learnable:
		pol.mode = pinNone
		return pol
	}
	if learned.activeAt(now) {
		pol.mode, pol.want, pol.wantNotAfter = pinLearned, learned.spki, learned.notAfter
	} else {
		pol.mode = pinEnroll
	}
	return pol
}

// recordPin commits what a dial under pol learned from leaf, the certificate
// the server presented, after the server passed the genesis check.
//
// Under a shipped pin on a learnable endpoint, the shipped key is recorded as
// the server's learned pin, replacing a stale pin for any other key, so that
// a certificate renewed for the shipped key extends the server's identity
// past the shipped certificate's own expiry. The commit is checked against
// the state at commit time, since the shipped pin can expire during the
// handshake: a pin for the shipped key is extended as a continuation; a pin
// for another key is replaced only if it is unchanged since the dial began
// and, once the shipped pin has expired, no longer active. Otherwise the
// learned rules now in force bind another key and the dial gets
// errCertPinMismatch.
//
// Otherwise it is a compare-and-set against the pin state the dial started
// from (pol.snapshot):
//
//   - the pinned key, whether the state is unchanged or a concurrent
//     connection pinned the same key meanwhile: a continuation. The pin's
//     expiry becomes the later of its own and the certificate's, never
//     earlier;
//   - unchanged state and a new key (first use, or a replacement once the
//     pinned expiry has passed): the key is pinned and written before the
//     connection is accepted, and a failed write refuses the connection;
//   - state changed by a concurrent connection to a different key, or to a
//     pin no longer active: errCertPinMismatch.
//
// A continuation whose write fails keeps the connection, since the server's
// key is already authenticated, holds the new expiry in memory and retries
// the write on the next connection. The caller closes the connection on any
// error and hands out no node before recordPin returns nil.
func (p *serverPool) recordPin(addr string, pol *tlsPolicy, leaf *x509.Certificate) error {
	switch {
	case pol.mode == pinShipped && pol.learnable:
	case pol.mode == pinLearned, pol.mode == pinEnroll:
	default:
		return nil
	}
	if leaf == nil {
		return fmt.Errorf("electrum: %s presented no certificate to pin", addr)
	}
	got := spkiHash(leaf)
	p.mtx.Lock()
	defer p.mtx.Unlock()
	now := p.clock()
	e := p.entries[addr]
	if e == nil {
		e = &poolEntry{Server: &ServerAddr{Proto: "ssl", Addr: addr}, FirstSeen: now}
		p.entries[addr] = e
	}
	cur := e.pin()
	switch {
	case pol.mode == pinShipped:
		if !cur.isZero() && cur.spki == got {
			break // the shipped key's continuation
		}
		lapsed := now.After(pol.wantNotAfter)
		if !cur.equal(pol.snapshot) || (lapsed && cur.activeAt(now)) {
			return fmt.Errorf("%w: %s presented shipped key %x, but its shipped pin expired "+
				"%s and key %x is pinned until %s", errCertPinMismatch, addr, got,
				pol.wantNotAfter.UTC().Format(time.RFC3339), cur.spki,
				cur.notAfter.UTC().Format(time.RFC3339))
		}
		e.setPin(certPin{spki: got, notAfter: leaf.NotAfter, pinnedAt: now})
		p.pinsDirty = true
		return p.persistContinuation(addr, leaf.NotAfter)
	case !cur.equal(pol.snapshot) && !(cur.activeAt(now) && cur.spki == got):
		return fmt.Errorf("%w: %s presented key %x, but a concurrent connection pinned %x",
			errCertPinMismatch, addr, got, cur.spki)
	case cur.isZero() || cur.spki != got:
		e.setPin(certPin{spki: got, notAfter: leaf.NotAfter, pinnedAt: now})
		if err := p.saveLocked(); err != nil {
			e.setPin(cur)
			return fmt.Errorf("electrum: pin certificate key for %s: %w", addr, err)
		}
		if cur.isZero() {
			log.Infof("electrum: pinned %s to certificate key %x (certificate valid until %s)",
				addr, got, leaf.NotAfter.UTC().Format(time.RFC3339))
		} else {
			log.Warnf("electrum: %s presented a new certificate key after its pinned "+
				"certificate expired; re-pinned from %x (expired %s) to %x (valid until %s)",
				addr, cur.spki, cur.notAfter.UTC().Format(time.RFC3339), got,
				leaf.NotAfter.UTC().Format(time.RFC3339))
		}
		return nil
	}
	// The pinned key: extend the expiry, never shorten it.
	if leaf.NotAfter.After(cur.notAfter) {
		cur.notAfter = leaf.NotAfter
		e.setPin(cur)
		p.pinsDirty = true
	}
	return p.persistContinuation(addr, cur.notAfter)
}

// persistContinuation writes the pool when a pin's expiry is held only in
// memory. A failure is logged and retried on the next connection; the
// connection that extended the pin stands. Must be called with mtx held.
func (p *serverPool) persistContinuation(addr string, notAfter time.Time) error {
	if !p.pinsDirty {
		return nil
	}
	if err := p.saveLocked(); err != nil {
		log.Warnf("electrum: %s presented its pinned key on a certificate valid until %s; "+
			"recording the expiry failed and is retried on the next connection: %v",
			addr, notAfter.UTC().Format(time.RFC3339), err)
	}
	return nil
}

// authenticatedLocked reports whether a dial to sv now authenticates the
// server, by the same policy the dial would use: an unexpired shipped pin
// (or a shipped pin where learned pins do not apply), CA verification, or an
// active learned pin. An endpoint that would accept a first-use certificate
// is not authenticated. Must be called with mtx held.
func (p *serverPool) authenticatedLocked(sv *ServerAddr, now time.Time) bool {
	switch p.tlsPolicyLocked(sv, p.isLearnable(sv.Addr), now).mode {
	case pinShipped, pinLearned:
		return true
	case pinNone:
		return sv.Proto == "ssl" && !sv.SkipVerify && len(sv.Cert) == 0
	}
	return false
}

// isLearnable reports whether learned pins apply to addr (see learnable).
func (p *serverPool) isLearnable(addr string) bool {
	if p.learnable == nil {
		return true
	}
	return p.learnable(addr)
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
// server: every shipped server is quarantined, or every shipped server has
// been quarantined or failing continuously for shippedDownWindow. A
// quarantined server counts as down — it is never re-dialed, so its failure
// record cannot stay fresh — and an all-quarantined shipped list opens the
// fallback at once: quarantine is a durable misbehavior verdict, not the
// transient outage the window exists to wait out. Must be called with mtx
// held.
func (p *serverPool) fallbackEligibleLocked(now time.Time) bool {
	serverHealth.mtx.Lock()
	allDown, allQuarantined := true, true
	for _, sv := range p.shipped {
		if e := p.entries[sv.Addr]; e != nil &&
			(serverHealth.quarantined[sv.Addr] || now.Before(e.QuarantinedUntil)) {
			continue
		}
		allQuarantined = false
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
	return allQuarantined || now.Sub(p.shippedDownSince) >= shippedDownWindow
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
			log.Errorf("electrum: every shipped server quarantined or failing "+
				"for %v; opening wallet-server election to %d aged discovered "+
				"servers", time.Since(p.shippedDownSince).Round(time.Second), len(aged))
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
	// Within each partition, servers whose identity a dial authenticates
	// (shipped pin, CA, or an active learned pin) come first, so a phone's
	// single observer is an authenticated one whenever one is available. A
	// learned pin only authenticates continuity with its first connection,
	// so this does not defend against sybils present at first use.
	for _, part := range [][]*ServerAddr{shippedHealthy, discHealthy, shippedBacked, discBacked} {
		sort.SliceStable(part, func(i, j int) bool {
			return p.authenticatedLocked(part[i], now) && !p.authenticatedLocked(part[j], now)
		})
	}
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
