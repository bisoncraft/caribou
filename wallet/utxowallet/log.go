// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package utxowallet provides cross-cutting setup for the shared utxowallet
// subsystems. It exists only to wire their logging in one place.
package utxowallet

import (
	"github.com/btcsuite/btclog"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/chain"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/electrum"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/peer"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv/blockntfns"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv/chanutils"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv/filterdb"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv/pushtx"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv/query"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wtxmgr"
)

// UseLogger points every utxowallet subsystem at backend. Each subpackage holds
// a single package-global logger, so this configures logging process-wide and
// is meant to be called once at startup. It replaces the per-asset,
// per-wallet-dir setup that btc and ltc each carried (the old "logNeutrino"),
// which collided on the shared globals in a multi-asset process and missed
// several subsystems.
func UseLogger(backend *btclog.Backend) {
	sub := func(name string, lvl btclog.Level) btclog.Logger {
		l := backend.Logger(name)
		l.SetLevel(lvl)
		return l
	}

	spv.UseLogger(sub("NTRNO", btclog.LevelDebug))
	chain.UseLogger(sub("CHAIN", btclog.LevelDebug))
	electrum.UseLogger(sub("ELEC", btclog.LevelDebug))
	peer.UseLogger(sub("PEER", btclog.LevelInfo))
	wallet.UseLogger(sub("WLLT", btclog.LevelDebug))
	wtxmgr.UseLogger(sub("TXMGR", btclog.LevelInfo))
	waddrmgr.UseLogger(sub("ADDR", btclog.LevelInfo))
	blockntfns.UseLogger(sub("BLKN", btclog.LevelInfo))
	chanutils.UseLogger(sub("CHUT", btclog.LevelInfo))
	filterdb.UseLogger(sub("FLTR", btclog.LevelInfo))
	pushtx.UseLogger(sub("PSTX", btclog.LevelInfo))
	query.UseLogger(sub("QURY", btclog.LevelInfo))
}
