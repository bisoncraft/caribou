// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package wallet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/utxowallet/chain"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
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
// from an electrum server.
//
// When the wallet's own electrum backend is running, the estimate comes over
// its existing wallet-server connection, which already carries the configured
// proxy and the server pool's certificate and quarantine policy. Otherwise
// (the neutrino backend, which cannot estimate fees itself, an electrum
// backend not yet connected, or one whose server gave no estimate), FeeRate
// opens a short-lived connection to each of the wallet's configured servers
// in turn, through the configured proxy. Those connections enforce shipped
// certificate pins but are outside the server pool, so a self-signed server
// without a shipped pin is not authenticated there.
//
// The estimate is electrum's blockchain.estimatefee (the backing daemon's
// estimatesmartfee), which is of variable quality, so it is intended as a
// fallback after external fee-rate APIs. It returns an error when no server
// can provide an estimate, in which case the caller has no rate and
// fee-dependent operations must error rather than substitute a default.
func (w *Wallet) FeeRate(ctx context.Context) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, feeEstimateTimeout)
	defer cancel()

	// One snapshot of the published chain client; Synchronize and Stop set
	// and clear it under chainClientLock.
	client := w.ChainClient()
	eb, isElectrum := client.(chain.ElectrumBackend)
	cb, canReport := client.(connectionReporter)
	if isElectrum && canReport && cb.Connected() {
		rate, err := feeRateFrom(ctx, eb.Network())
		if err == nil {
			return rate, nil
		}
		log.Debugf("Electrum fee estimate from the wallet server failed, trying "+
			"short-lived connections: %v", err)
	}

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

	var lastErr error
	for _, srv := range servers {
		if ctx.Err() != nil {
			break
		}
		rate, err := electrumFeeRate(ctx, srv, w.torProxy, genesis)
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

// connectionReporter is implemented by chain clients that report whether they
// hold a live server connection, as the electrum wallet backend does.
type connectionReporter interface {
	Connected() bool
}

// feeEstimator is the electrum fee-estimate call shared by a single
// connection and the wallet's electrum network.
type feeEstimator interface {
	EstimateFee(ctx context.Context, confTarget int32) (int64, error)
}

// feeRateFrom requests a fee estimate and returns it in atoms per byte.
func feeRateFrom(ctx context.Context, est feeEstimator) (uint64, error) {
	satsPerKB, err := est.EstimateFee(ctx, feeEstimateConfTarget)
	if err != nil {
		return 0, err
	}
	if satsPerKB <= 0 {
		return 0, errors.New("electrum: non-positive fee estimate")
	}
	return uint64(util.IntDivUp(satsPerKB, 1000)), nil // sats/kB => sats/vByte
}

// electrumFeeRate dials a single electrum server, through torProxy when it is
// set, requests a fee estimate, and returns it in atoms per byte. The
// connection is closed before returning.
func electrumFeeRate(ctx context.Context, srv *electrum.ServerAddr, torProxy, genesis string) (uint64, error) {
	node, err := electrum.NewNode(ctx, srv, torProxy, genesis)
	if err != nil {
		return 0, err
	}
	defer node.Close()
	return feeRateFrom(ctx, node)
}
