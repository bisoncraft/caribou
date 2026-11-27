// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"context"
	"time"
)

// MinBirthdayMargin is the smallest margin HeightForTime accepts.
const MinBirthdayMargin = 2 * time.Hour

// HeightForTime returns a birthday height for a seed made at t: an
// approximation of the greatest height whose block time is at most t less
// margin, never below the activation of NU6.3, which is where every account
// begins. A t at or after the time of the server's chain tip, less margin,
// gives the tip. A margin below MinBirthdayMargin fails with
// ErrInvalidArgument.
//
// Block times are not in height order, so no search of a few blocks can
// promise the greatest such height. What the result does promise is that no
// block below it has a time at or after t. The search returns a height whose
// own time is at most t less margin, or the activation. Consensus requires a
// block's time to be above the median of the times of the eleven blocks
// before it, and no more than 90 minutes above that median, and the median
// never decreases with height. A block can therefore be later than a block
// above it by less than 90 minutes, and with a margin above that, every
// block below the result is earlier than t.
//
// The margin also covers a device clock that was wrong when the seed was
// made. A birthday too late misses the funds before it, and the wallet has no
// rescan to recover them; a birthday too early costs only a longer scan.
// Whether a seed is new or restored is the caller's decision, not this
// function's.
//
// The search asks a server for block times: an interpolation from the block
// times it has seen, with a bisection when that makes no progress. Typically
// about half a dozen blocks are fetched, and at most a few dozen. The servers
// are tried in turn as by CreateAccount, and each is first checked to follow
// the wallet's chain and consensus rules. On a regtest chain, whose block
// times start at its genesis time, a recent t gives the tip. The server
// learns roughly when the seed was made, which the tree state requested for
// the birthday tells it anyway.
func (w *Wallet) HeightForTime(ctx context.Context, t time.Time, margin time.Duration) (uint32, error) {
	if margin < MinBirthdayMargin {
		return 0, wrapf(ErrInvalidArgument, "the margin %s is below the minimum of %s", margin, MinBirthdayMargin)
	}
	if w.serversErr != nil {
		return 0, w.serversErr
	}
	target := t.Add(-margin).Unix()
	var err error
	for range w.servers.len() {
		server := w.servers.get()
		var height uint32
		height, err = w.heightForTimeAt(ctx, server, target)
		if err == nil || ctx.Err() != nil || !countsAgainstServer(err) {
			return height, err
		}
		if next := w.servers.rotate(server); next != server {
			w.log.Warnf("lightwalletd server %s failed, moving on to %s: %v", server.address, next.address, err)
		}
	}
	return 0, err
}

// heightForTimeAt is HeightForTime with the chain of one server.
func (w *Wallet) heightForTimeAt(ctx context.Context, server endpoint, target int64) (uint32, error) {
	src, err := w.dial(server)
	if err != nil {
		return 0, err
	}
	defer src.close()
	info, err := checkServerChain(ctx, src, w.cfg.Network)
	if err != nil {
		return 0, err
	}
	if err := checkConsensusBranch(info, w.be); err != nil {
		return 0, err
	}
	return heightForTime(ctx, src, w.cfg.Network.ironwoodActivation(), target)
}

// blockTime returns the time of the block at height, in Unix seconds.
func blockTime(ctx context.Context, src chainSource, height uint32) (int64, error) {
	b, err := src.block(ctx, height)
	if err != nil {
		return 0, err
	}
	return int64(b.Time), nil
}

// heightForTime returns a height in [floor, tip] whose block time is at most
// target, near the greatest such height, or floor if the time of floor is
// above target or the tip is not above floor.
func heightForTime(ctx context.Context, src chainSource, floor uint32, target int64) (uint32, error) {
	tip, err := latestHeight(ctx, src)
	if err != nil {
		return 0, err
	}
	if tip <= floor {
		return floor, nil
	}
	tipTime, err := blockTime(ctx, src, tip)
	if err != nil {
		return 0, err
	}
	if tipTime <= target {
		return tip, nil
	}
	floorTime, err := blockTime(ctx, src, floor)
	if err != nil {
		return 0, err
	}
	if floorTime > target {
		return floor, nil
	}
	// lo's time is at most target and hi's is above it, which every step
	// keeps. Block times are not in height order, so the result is a
	// height near where the times cross target, not always the greatest
	// one at or below it.
	lo, loTime, hi, hiTime := floor, floorTime, tip, tipTime
	interpolate := true
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		if interpolate && hiTime > loTime {
			guess := int64(lo) + (target-loTime)*int64(hi-lo)/(hiTime-loTime)
			mid = uint32(min(max(guess, int64(lo)+1), int64(hi)-1))
		}
		midTime, err := blockTime(ctx, src, mid)
		if err != nil {
			return 0, err
		}
		width := hi - lo
		if midTime <= target {
			lo, loTime = mid, midTime
		} else {
			hi, hiTime = mid, midTime
		}
		// An interpolation that took off less than a quarter of the range
		// gives way to bisection for the next step.
		interpolate = !interpolate || hi-lo < width-width/4
	}
	return lo, nil
}
