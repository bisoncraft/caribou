// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package base

import (
	"fmt"

	"github.com/bisoncraft/meshwallet/dex"
	dexbase "github.com/bisoncraft/meshwallet/dex/networks/base"
	dexeth "github.com/bisoncraft/meshwallet/dex/networks/eth"
	"github.com/bisoncraft/meshwallet/server/asset"
	"github.com/bisoncraft/meshwallet/server/asset/eth"
)

const BipID = 8453

var registeredTokens = make(map[uint32]*eth.VersionedToken)

func registerToken(assetID uint32, protocolVersion dexeth.ProtocolVersion) {
	token, exists := dexbase.Tokens[assetID]
	if !exists {
		panic(fmt.Sprintf("no token constructor for asset ID %d", assetID))
	}
	asset.RegisterToken(assetID, &eth.TokenDriver{
		DriverBase: eth.DriverBase{
			ProtocolVersion: protocolVersion,
			UI:              token.UnitInfo,
			Nam:             token.Name,
		},
		Token: token.Token,
	})
	registeredTokens[assetID] = &eth.VersionedToken{
		Token:           token,
		ContractVersion: protocolVersion.ContractVersion(),
	}
}

func init() {
	asset.Register(BipID, &Driver{eth.Driver{
		DriverBase: eth.DriverBase{
			ProtocolVersion: eth.ProtocolVersion(BipID),
			UI:              dexbase.UnitInfo,
			Nam:             "Base",
		},
	}})

	registerToken(usdcID, eth.ProtocolVersion(usdcID))
	registerToken(usdtID, eth.ProtocolVersion(usdtID))
	registerToken(wbtcTokenID, eth.ProtocolVersion(wbtcTokenID))
}

var (
	usdcID, _      = dex.BipSymbolID("usdc.base")
	usdtID, _      = dex.BipSymbolID("usdt.base")
	wbtcTokenID, _ = dex.BipSymbolID("wbtc.base")
)

type Driver struct {
	eth.Driver
}

// Setup creates the ETH backend. Start the backend with its Run method.
func (d *Driver) Setup(cfg *asset.BackendConfig) (asset.Backend, error) {
	return eth.NewEVMBackend(cfg, uint64(dexbase.ChainIDs[cfg.Net]), dexbase.ContractAddresses, registeredTokens)
}
