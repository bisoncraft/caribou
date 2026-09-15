// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package utxowallet provides cross-cutting setup for the shared utxowallet
// subsystems. It exists only to wire their logging in one place.
package utxowallet

import (
	"github.com/btcsuite/btclog"

	"github.com/bisoncraft/caribou/wallet/utxowallet/chain"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
	"github.com/bisoncraft/caribou/wallet/utxowallet/peer"
	"github.com/bisoncraft/caribou/wallet/utxowallet/spv"
	"github.com/bisoncraft/caribou/wallet/utxowallet/spv/blockntfns"
	"github.com/bisoncraft/caribou/wallet/utxowallet/spv/chanutils"
	"github.com/bisoncraft/caribou/wallet/utxowallet/spv/filterdb"
	"github.com/bisoncraft/caribou/wallet/utxowallet/spv/pushtx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/spv/query"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wtxmgr"
)

// UseLogger points every utxowallet subsystem at a logger from newLogger, which
// receives the subsystem name and the level this package defaults it to, and
// may substitute its own. Each subpackage holds a single package-global logger,
// so this configures logging process-wide and is meant to be called once at
// startup. It replaces the per-asset, per-wallet-dir setup that btc and ltc
// each carried (the old "logNeutrino"), which collided on the shared globals in
// a multi-asset process and missed several subsystems.
func UseLogger(newLogger func(name string, defaultLevel btclog.Level) btclog.Logger) {
	spv.UseLogger(newLogger("NTRNO", btclog.LevelDebug))
	chain.UseLogger(newLogger("CHAIN", btclog.LevelDebug))
	electrum.UseLogger(newLogger("ELEC", btclog.LevelDebug))
	peer.UseLogger(newLogger("PEER", btclog.LevelInfo))
	wallet.UseLogger(newLogger("WLLT", btclog.LevelDebug))
	wtxmgr.UseLogger(newLogger("TXMGR", btclog.LevelInfo))
	waddrmgr.UseLogger(newLogger("ADDR", btclog.LevelInfo))
	blockntfns.UseLogger(newLogger("BLKN", btclog.LevelInfo))
	chanutils.UseLogger(newLogger("CHUT", btclog.LevelInfo))
	filterdb.UseLogger(newLogger("FLTR", btclog.LevelInfo))
	pushtx.UseLogger(newLogger("PSTX", btclog.LevelInfo))
	query.UseLogger(newLogger("QURY", btclog.LevelInfo))
}
