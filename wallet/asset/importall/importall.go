package importall

import (
	// _ "github.com/bisoncraft/caribou/wallet/asset/bch"          // register bch asset
	_ "github.com/bisoncraft/caribou/wallet/asset/btc"         // register btc asset
	_ "github.com/bisoncraft/caribou/wallet/asset/hyperevm"    // register hyperevm asset
	_ "github.com/bisoncraft/caribou/wallet/asset/hyperliquid" // register hyperliquid asset
	_ "github.com/bisoncraft/caribou/wallet/asset/sol"         // register sol asset

	_ "github.com/bisoncraft/caribou/wallet/asset/dash" // register dash asset
	// dcr is registered in importall_dcr.go under the "dcr" build tag.
	// _ "github.com/bisoncraft/caribou/wallet/asset/dgb"  // register dgb asset
	// _ "github.com/bisoncraft/caribou/wallet/asset/doge" // register doge asset
	// _ "github.com/bisoncraft/caribou/wallet/asset/firo" // register firo asset
	_ "github.com/bisoncraft/caribou/wallet/asset/ltc" // register ltc asset
	// _ "github.com/bisoncraft/caribou/wallet/asset/near" // register near asset
	// _ "github.com/bisoncraft/caribou/wallet/asset/zec"  // register zec asset
	_ "github.com/bisoncraft/caribou/wallet/asset/xrp" // register xrp asset
)
