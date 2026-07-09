// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"github.com/bisoncraft/caribou/util"
	"github.com/ethereum/go-ethereum/common"
)

// AcrossAssetMapping is a test-only view of acrossAsset that exposes the
// internal fields for external test packages.
type AcrossAssetMapping struct {
	ChainID uint64
	Symbol  string
	Address common.Address
}

// AssetIDToAcrossAsset is a test export of assetIDToAcrossAsset.
func AssetIDToAcrossAsset(net util.Network, assetID uint32) *AcrossAssetMapping {
	a := assetIDToAcrossAsset(net, assetID)
	if a == nil {
		return nil
	}
	return &AcrossAssetMapping{ChainID: a.chainID, Symbol: a.symbol, Address: a.address}
}

// AcrossAssetToAssetID is a test export of acrossAssetToAssetID.
func AcrossAssetToAssetID(net util.Network, symbol string, chainID uint64) (uint32, bool) {
	return acrossAssetToAssetID(net, symbol, chainID)
}

// WalletTypeToken is a test export of walletTypeToken.
const WalletTypeToken = walletTypeToken
