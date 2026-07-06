// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package wallet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/electrum"
)

const (
	// feeEstimateConfTarget is the confirmation target for the electrum fee
	// estimate. A target of 1 tends to produce volatile estimatesmartfee values.
	feeEstimateConfTarget int32 = 2
	// feeEstimateTimeout bounds the whole fee-estimate attempt, shared across
	// server tries, so a slow or unreachable server can't stall the caller.
	feeEstimateTimeout = 8 * time.Second
)

// FeeRate returns a fee-rate estimate in atoms per byte (e.g. sats/vByte)
// obtained from one of the wallet's configured electrum servers. It works
// regardless of the chain backend in use: it opens a short-lived connection for
// the query, so it also serves wallets that sync via the neutrino backend,
// which cannot estimate fees itself.
//
// The estimate is electrum's blockchain.estimatefee (the backing daemon's
// estimatesmartfee), which is of variable quality, so it is intended as a
// fallback. It returns an error when no server can provide an estimate; callers
// should have a default rate.
func (w *Wallet) FeeRate(ctx context.Context) (uint64, error) {
	servers := w.chainParams.ElectrumServers
	if len(servers) == 0 {
		return 0, errors.New("wallet: no electrum servers configured for fee estimation")
	}
	// The genesis hash, when known, lets NewNode reject a server on the wrong
	// chain; an empty string skips that check.
	var genesis string
	if w.chainParams.GenesisHash != nil {
		genesis = w.chainParams.GenesisHash.String()
	}

	ctx, cancel := context.WithTimeout(ctx, feeEstimateTimeout)
	defer cancel()

	var lastErr error
	for _, srv := range servers {
		if ctx.Err() != nil {
			break
		}
		rate, err := electrumFeeRate(ctx, srv, genesis)
		if err != nil {
			lastErr = err
			continue
		}
		return rate, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no estimate returned")
	}
	return 0, fmt.Errorf("wallet: electrum fee estimate failed: %w", lastErr)
}

// electrumFeeRate dials a single electrum server, requests a fee estimate, and
// returns it in atoms per byte. The connection is closed before returning.
func electrumFeeRate(ctx context.Context, srv *electrum.ServerAddr, genesis string) (uint64, error) {
	node, err := electrum.NewNode(ctx, srv, "", genesis)
	if err != nil {
		return 0, err
	}
	defer node.Close()

	satsPerKB, err := node.EstimateFee(ctx, feeEstimateConfTarget)
	if err != nil {
		return 0, err
	}
	if satsPerKB <= 0 {
		return 0, errors.New("electrum: non-positive fee estimate")
	}
	return uint64(util.IntDivUp(satsPerKB, 1000)), nil // sats/kB => sats/vByte
}
