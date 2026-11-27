// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import "time"

// onChangeInterval is the shortest time between two calls of
// Config.OnChange during a cycle.
const onChangeInterval = time.Second

// noteChange records that the sync loop wrote to the wallet database in a way
// that can change balances or the history, and calls Config.OnChange if the
// last call was at least onChangeInterval ago. Otherwise the change waits
// for the next note or for the end of the cycle.
func (p *processor) noteChange() {
	if p.opts.onChange == nil {
		return
	}
	p.changePending = true
	if now := p.opts.now(); !now.Before(p.lastChange.Add(onChangeInterval)) {
		p.fireChange(now)
	}
}

// flushChange calls Config.OnChange for a change that is still waiting. It
// runs at the end of every cycle, whatever the time since the last call.
func (p *processor) flushChange() {
	if p.changePending {
		p.fireChange(p.opts.now())
	}
}

func (p *processor) fireChange(now time.Time) {
	p.changePending = false
	p.lastChange = now
	p.opts.onChange()
}
