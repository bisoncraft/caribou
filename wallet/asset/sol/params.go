// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import "github.com/bisoncraft/bosswallet/util"

const (
	BipID = 501

	DefaultFee = 5_000

	MaxBlockInterval = 30

	keyFileName   = "sol-keyfile.json"
	walletTypeRPC = "rpc"
)

var (
	UnitInfo = util.UnitInfo{
		AtomicUnit: "lamports",
		Conventional: util.Denomination{
			Unit:             "SOL",
			ConversionFactor: 1e9,
		},
	}

	DefaultRPCEndpoints = map[util.Network]string{
		util.Mainnet: "https://api.mainnet-beta.solana.com",
		util.Testnet: "https://api.testnet.solana.com",
		util.Simnet:  "http://127.0.0.1:18999",
	}

	DefaultWSEndpoints = map[util.Network]string{
		util.Mainnet: "wss://api.mainnet-beta.solana.com",
		util.Testnet: "wss://api.testnet.solana.com",
		util.Simnet:  "ws://127.0.0.1:19000",
	}
)
