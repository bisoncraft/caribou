//go:build xmr

package importall

import (
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/bch"  // register bch asset
	_ "github.com/bisoncraft/bosswallet/wallet/asset/btc" // register btc asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/dash" // register dash asset
	// dcr is registered in importall_dcr.go under the "dcr" build tag.
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/dgb"  // register dgb asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/doge" // register doge asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/firo" // register firo asset
	_ "github.com/bisoncraft/bosswallet/wallet/asset/ltc" // register ltc asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/near" // register near asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/xmr"  // register xmr asset
	// _ "github.com/bisoncraft/bosswallet/wallet/asset/zec"  // register zec asset
)
