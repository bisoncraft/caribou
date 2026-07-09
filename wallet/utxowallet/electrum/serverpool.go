// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
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
}{m: make(map[string]*serverRecord)}

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
