//go:build xmr

package importall

import (
	_ "github.com/bisoncraft/meshwallet/wallet/asset/bch"  // register bch asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/btc"  // register btc asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/dash" // register dash asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/dcr"  // register dcr asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/dgb"  // register dgb asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/doge" // register doge asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/firo" // register firo asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/ltc"  // register ltc asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/near" // register near asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/xmr"  // register xmr asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/zec"  // register zec asset
)
