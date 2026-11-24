// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
)

const (
	// BipID is the SLIP-0044 coin type of Zcash.
	BipID = 133

	// walletTypeNative is the one wallet type: a light wallet on zeclib,
	// seeded by core.
	walletTypeNative = "native"

	// feeRate is the ZIP 317 marginal fee, in zatoshis per logical action.
	// The fee of a Zcash transaction is fixed by its shape, not chosen, so
	// it is the only fee rate the wallet takes.
	feeRate = 5_000

	// finalConfirmations is the number of confirmations at which a
	// transaction record is final: it is stored as Confirmed, and it stops
	// being read as an open row. It matches the ZIP 315 depth at which a
	// received output becomes spendable.
	finalConfirmations = 10

	// settingServer is the key of the server setting.
	settingServer = "lightwalletdserver"

	// simnetServer is the lightwalletd endpoint of the simnet harness.
	simnetServer = "127.0.0.1:22770"
	// simnetBirthday is the birthday of a simnet account: the harness
	// activates every upgrade at height 1, and its first funds are mined
	// after that.
	simnetBirthday = 2
)

var (
	// UnitInfo is the Zcash unit information. A fee rate is in zatoshis per
	// logical action (ZIP 317).
	UnitInfo = util.UnitInfo{
		AtomicUnit: "zats",
		Conventional: util.Denomination{
			Unit:             "ZEC",
			ConversionFactor: 1e8,
		},
		FeeRateDenom: "action",
	}

	walletDefinition = &asset.WalletDefinition{
		Type:        walletTypeNative,
		Tab:         "Native",
		Description: "A built-in light wallet that holds shielded funds in the Ironwood pool and transparent funds.",
		Seeded:      true,
		ConfigOpts: []*asset.ConfigOption{{
			Key:         settingServer,
			DisplayName: "lightwalletd server",
			Description: "A lightwalletd server (host:port) to use in place of the built-in public servers. " +
				"Only a server on this machine (localhost or a 127.x or ::1 address) is reached without TLS; " +
				"any other server, one elsewhere on the local network included, needs TLS. " +
				"A server named by localhost or by a private IP address is not reached through the Tor proxy; " +
				"a host name is, even on the local network.",
		}},
	}

	// WalletInfo describes the Zcash wallet.
	WalletInfo = asset.WalletInfo{
		Name:             "Zcash",
		AvailableWallets: []*asset.WalletDefinition{walletDefinition},
		UnitInfo:         UnitInfo,
		BlockchainClass:  asset.BlockchainClassUTXO,
		// The ceiling is the one rate the wallet takes.
		MaxFeeRate: feeRate,
		Network:    &asset.Network{ID: BipID, NativeAssetID: BipID, Priority: true},
	}
)
