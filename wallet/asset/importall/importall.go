//go:build !xmr

package importall

import (
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/bch"          // register bch asset
	_ "github.com/bisoncraft/bosswallet/wallet/asset/btc"         // register btc asset
	_ "github.com/bisoncraft/bosswallet/wallet/asset/hyperliquid" // register hyperliquid asset

	// _ "github.com/bisoncraft/bosswallet/wallet/asset/dash" // register dash asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/dcr"  // register dcr asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/dgb"  // register dgb asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/doge" // register doge asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/firo" // register firo asset
	_ "github.com/bisoncraft/bosswallet/wallet/asset/ltc" // register ltc asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/near" // register near asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/zec"  // register zec asset
	_ "github.com/bisoncraft/bosswallet/wallet/asset/xrp" // register xrp asset
)
