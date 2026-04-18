// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.
//
// By default, this app also imports go-ethereum code and so carries the burden
// of go-ethereum's GNU Lesser General Public License. If that is unacceptable,
// build with the nolgpl tag.

//go:build !nolgpl

package main

import (
	dexbase "github.com/bisoncraft/meshwallet/dex/networks/base"
	dexeth "github.com/bisoncraft/meshwallet/dex/networks/eth"
	dexpolygon "github.com/bisoncraft/meshwallet/dex/networks/polygon"
	_ "github.com/bisoncraft/meshwallet/server/asset/base"    // register base asset
	_ "github.com/bisoncraft/meshwallet/server/asset/eth"     // register eth asset
	_ "github.com/bisoncraft/meshwallet/server/asset/polygon" // register polygon asset
)

func init() {
	dexeth.MaybeReadSimnetAddrs()
	dexpolygon.MaybeReadSimnetAddrs()
	dexbase.MaybeReadSimnetAddrs()
}
