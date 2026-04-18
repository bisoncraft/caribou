package importall

import (
	_ "github.com/bisoncraft/meshwallet/server/asset/bch"  // register bch asset
	_ "github.com/bisoncraft/meshwallet/server/asset/btc"  // register btc asset
	_ "github.com/bisoncraft/meshwallet/server/asset/dash" // register dash asset
	_ "github.com/bisoncraft/meshwallet/server/asset/dcr"  // register dcr asset
	_ "github.com/bisoncraft/meshwallet/server/asset/dgb"  // register dgb asset
	_ "github.com/bisoncraft/meshwallet/server/asset/doge" // register doge asset
	_ "github.com/bisoncraft/meshwallet/server/asset/firo" // register firo asset
	_ "github.com/bisoncraft/meshwallet/server/asset/ltc"  // register ltc asset
	_ "github.com/bisoncraft/meshwallet/server/asset/xmr"  // register xmr asset
	_ "github.com/bisoncraft/meshwallet/server/asset/zec"  // register zec asset
	// nixed
	// _ "github.com/bisoncraft/meshwallet/server/asset/zcl"  // register zcl asset
)
