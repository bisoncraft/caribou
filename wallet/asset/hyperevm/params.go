// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperevm

import (
	"github.com/bisoncraft/caribou/util"
	dexeth "github.com/bisoncraft/caribou/wallet/asset/eth"
)

const (
	// MainnetChainID is HyperEVM's chain ID (verified live via eth_chainId
	// against https://rpc.hyperliquid.xyz/evm, 2026-08-22).
	MainnetChainID = 999
)

var (
	// ChainIDs maps only Mainnet: HyperEVM testnet's RPC endpoint could not be
	// reached (TLS handshake failure) to verify a chain ID, and simnet has no
	// local HyperEVM node to test against. See ChainConfig.
	ChainIDs = map[util.Network]int64{
		util.Mainnet: MainnetChainID,
	}

	// UnitInfo describes HYPE, the native gas token on HyperEVM. This is the
	// same token as HyperCore spot HYPE, but a distinct on-chain balance —
	// HyperCore uses ConversionFactor 1e8 (params.go in wallet/asset/
	// hyperliquid), HyperEVM native HYPE uses the standard 18-decimal wei
	// atomized to gwei like any other EVM native asset.
	UnitInfo = util.UnitInfo{
		AtomicUnit: "gwei",
		Conventional: util.Denomination{
			Unit:             "HYPE",
			ConversionFactor: 1e9,
		},
		FeeRateDenom: "gas",
	}

	// VersionedGases has zero gas estimates because no swap contracts are
	// deployed on HyperEVM.
	VersionedGases = map[uint32]*dexeth.Gases{
		1: {},
	}
)
