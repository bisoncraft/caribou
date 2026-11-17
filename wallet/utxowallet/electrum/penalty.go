// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"errors"
	"fmt"
)

// serverPenalty is what a header-path error costs the server that caused it.
type serverPenalty int

const (
	// penaltyNone records nothing against the server.
	penaltyNone serverPenalty = iota
	// penaltyFailure records a failure, which deprioritizes the server for
	// serverFailureBackoff and counts against its durable reputation.
	penaltyFailure
	// penaltyQuarantine excludes the server from every election, for the
	// process lifetime in memory and for quarantineTerm on disk.
	penaltyQuarantine
)

// headerErrorPenalty classifies an error from a server's headers. A RuleError
// is evidence the server misbehaved and quarantines it. errInsufficientWork
// (a branch lighter than the stored chain) records a failure: the server is
// behind or on a losing fork, which is not misbehavior. errTimeTooNew costs
// nothing, since local clock skew produces it as well; errForkNotFound costs
// nothing, since an honest server that reorgs back to the store's tip between
// two requests produces it; and no error costs anything while the network is
// shutting down. Anything else gets fallback: penaltyNone on the live tip
// path, where fetch and persistence errors prove nothing about the server,
// and penaltyFailure for a failed initial sync or attach, which has always
// recorded one. Availability failures (timeouts, dropped connections) never
// quarantine.
func (n *Network) headerErrorPenalty(err error, fallback serverPenalty) serverPenalty {
	var ruleErr *RuleError
	switch {
	case n.ctx != nil && n.ctx.Err() != nil:
		return penaltyNone
	case errors.As(err, &ruleErr):
		return penaltyQuarantine
	case errors.Is(err, errInsufficientWork):
		return penaltyFailure
	case errors.Is(err, errTimeTooNew), errors.Is(err, errForkNotFound):
		return penaltyNone
	default:
		return fallback
	}
}

// penalize applies the penalty err earns the server at addr (see
// headerErrorPenalty) and returns it.
func (n *Network) penalize(addr string, err error, fallback serverPenalty) serverPenalty {
	p := n.headerErrorPenalty(err, fallback)
	switch p {
	case penaltyQuarantine:
		n.pool.quarantine(addr, err.Error())
	case penaltyFailure:
		n.scoreFailure(addr)
	}
	return p
}

// scoreFailure records a failure against the server at addr unless the
// network is shutting down: a dial or request cut short by Stop says nothing
// about the server.
func (n *Network) scoreFailure(addr string) {
	if n.ctx != nil && n.ctx.Err() != nil {
		return
	}
	n.pool.recordFailure(addr)
}

// walletServerFault handles an error from the live wallet server's header
// processing: a tip that failed to connect or a catch-up that failed. A
// RuleError quarantines the server and rotates away from it.
// errInsufficientWork records a failure and rotates with a hint for the
// store's tip, so the election prefers servers on the chain the store holds.
// Everything else (errTimeTooNew, errForkNotFound from a server that reorged
// back to the store's tip, and on this path fetch and persistence errors) is
// logged with no penalty; a server that stays useless falls to the lag
// rotation, and a dead connection to watchNode.
//
// The rotation runs outside node's loop group, which it joins, as an
// n.wg-tracked goroutine; a duplicate for the same node finds the slot
// already handed over and does nothing.
func (n *Network) walletServerFault(node *Node, err error, what string) {
	if node == nil || n.ctx.Err() != nil {
		return
	}
	addr := node.Addr().Addr
	p := n.penalize(addr, err, penaltyNone)
	var hint *electionHint
	var reason string
	switch p {
	case penaltyNone:
		log.Warnf("electrum: %s from wallet server %s: %v", what, addr, err)
		return
	case penaltyQuarantine:
		reason = "header rule violation"
		log.Warnf("electrum: %s: wallet server %s broke a header rule; "+
			"quarantined, rotating: %v", what, addr, err)
	case penaltyFailure:
		reason = "branch with less work than the stored chain"
		tip, tipHash := n.headers.tipSnapshot()
		hint = &electionHint{wantHeight: tip, wantHash: tipHash}
		log.Warnf("electrum: %s: wallet server %s serves a branch with less work "+
			"than the stored chain; demoted, rotating: %v", what, addr, err)
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.rotate(node, hint, reason)
	}()
}

// discardOnCheckpointMismatch handles an error from syncing: when err is a
// checkpoint mismatch, the server's chain contradicts a checkpoint above the
// store's tip, and every header the store took above the highest checkpoint
// it holds may have come from that server. None of those headers was hashed
// for PoW (they lie below the newest checkpoint), and a span deeper than
// maxReorgDepth could never be reorged out, so the store discards back to
// that checkpoint and publishes the disconnects as one TipChange. It returns
// err, with any discard failure joined. The caller penalizes the server.
func (n *Network) discardOnCheckpointMismatch(err error) error {
	if !errors.Is(err, errCheckpointMismatch) {
		return err
	}
	disc, derr := n.headers.discardToHeldCheckpoint()
	if lowest, ok := lowestDisconnected(disc); ok {
		log.Warnf("electrum: server chain contradicts a checkpoint; discarded %d "+
			"headers back to height %d", len(disc), lowest-1)
		if !n.deliverTipChange(&TipChange{Disconnected: disc}) {
			return errors.Join(err, n.ctx.Err())
		}
	}
	if derr != nil {
		return errors.Join(err, fmt.Errorf("electrum: discard to held checkpoint: %w", derr))
	}
	return err
}
