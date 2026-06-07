// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import "github.com/bisoncraft/bosswallet/util"

const (
	BipID = 144

	// BaseReserveDrops is the minimum XRP balance required to activate an
	// account. Reduced on 2024-12-02 from 10 XRP to 1 XRP; query
	// server_state for the authoritative network value.
	BaseReserveDrops = 1_000_000

	// OwnerReserveDrops is the per-owned-object reserve. Reduced on
	// 2024-12-02 from 2 XRP to 0.2 XRP per object.
	OwnerReserveDrops = 200_000

	// DefaultFeeDrops is used when the network fee cannot be queried.
	DefaultFeeDrops = 12

	// MaxBlockInterval is the maximum seconds since the last validated ledger
	// before the wallet is considered out of sync.
	MaxBlockInterval = 30

	keyFileName   = "xrp-keyfile.json"
	walletTypeRPC = "rpc"
)

var (
	UnitInfo = util.UnitInfo{
		AtomicUnit: "drops",
		Conventional: util.Denomination{
			Unit:             "XRP",
			ConversionFactor: 1e6,
		},
	}

	DefaultRPCEndpoints = map[util.Network]string{
		util.Mainnet: "https://s1.ripple.com:51234",
		util.Testnet: "https://s.altnet.rippletest.net:51234",
		util.Simnet:  "http://localhost:15005",
	}

	DefaultWSEndpoints = map[util.Network]string{
		util.Mainnet: "wss://s1.ripple.com/",
		util.Testnet: "wss://s.altnet.rippletest.net:51233/",
		util.Simnet:  "ws://localhost:16006",
	}
)
