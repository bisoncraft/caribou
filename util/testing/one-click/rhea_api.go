// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package oneclickmock

import (
	"math/big"
	"strings"
	"time"
)

// RheaWorld is the programmatic handle on the simulated RHEA world, for
// tests in other packages. The HTTP control API under /test/rhea/ offers
// the same operations to a harness.
type RheaWorld struct {
	w *rheaWorld
}

// Warp advances the simulated chain clock by d (cumulative), for example
// past an admission window.
func (r *RheaWorld) Warp(d time.Duration) { r.w.Warp(d) }

// ChainTime is the simulated chain clock now: the host clock plus the
// warp.
func (r *RheaWorld) ChainTime() time.Time { return time.Now().Add(r.w.WarpOffset()) }

// Fund credits account's plain balance of token ("near" for native NEAR).
func (r *RheaWorld) Fund(token, account string, amt *big.Int) error {
	return r.w.Fund(token, account, amt)
}

// SetFaults replaces the fault injections.
func (r *RheaWorld) SetFaults(f RheaFaults) { r.w.SetFaults(f) }

// Faults returns the current fault injections.
func (r *RheaWorld) Faults() RheaFaults { return r.w.Faults() }

// SetNonce sets an MCA's nonce, as a business executed elsewhere would.
func (r *RheaWorld) SetNonce(mca string, nonce uint64) {
	r.w.mtx.Lock()
	defer r.w.mtx.Unlock()
	r.w.st.nonces[mca] = nonce
}

// MCA returns the MCA bound to an EVM identity (hex, no 0x), or "".
func (r *RheaWorld) MCA(identity string) string {
	r.w.mtx.Lock()
	defer r.w.mtx.Unlock()
	return r.w.st.mcaByID[strings.ToLower(identity)]
}

// Nonce returns an MCA's nonce at the head.
func (r *RheaWorld) Nonce(mca string) uint64 {
	r.w.mtx.Lock()
	defer r.w.mtx.Unlock()
	return r.w.st.nonces[mca]
}

// Supplied returns an account's Burrow supply of token in token units.
func (r *RheaWorld) Supplied(account, token string) *big.Int {
	r.w.mtx.Lock()
	defer r.w.mtx.Unlock()
	return new(big.Int).Div(r.w.st.suppliedOf(account, token), tenPow(r.w.extraDecimals(token)))
}

// Balance returns an account's plain balance of token.
func (r *RheaWorld) Balance(token, account string) *big.Int {
	r.w.mtx.Lock()
	defer r.w.mtx.Unlock()
	return r.w.st.balance(token, account)
}

// Settled reports whether every scheduled transaction and receipt has
// executed.
func (r *RheaWorld) Settled() bool {
	r.w.mtx.Lock()
	defer r.w.mtx.Unlock()
	return len(r.w.queue) == 0 && len(r.w.pendingTx) == 0
}
