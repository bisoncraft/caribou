package core

import (
	"context"
	"fmt"

	"github.com/bisoncraft/caribou/wallet/asset"
)

// masternodeOwner returns the wallet's asset.MasternodeOwner interface.
func (c *Core) masternodeOwner(assetID uint32) (asset.MasternodeOwner, error) {
	wallet, exists := c.wallet(assetID)
	if !exists {
		return nil, newError(missingWalletErr, "no configured wallet found for %s", unbip(assetID))
	}
	mo, is := wallet.Wallet.(asset.MasternodeOwner)
	if !is {
		return nil, fmt.Errorf("%s wallet is not a MasternodeOwner", unbip(assetID))
	}
	return mo, nil
}

// Masternodes lists the masternodes the wallet owns shares of.
func (c *Core) Masternodes(ctx context.Context, assetID uint32) ([]*asset.Masternode, error) {
	mo, err := c.masternodeOwner(assetID)
	if err != nil {
		return nil, err
	}
	return mo.Masternodes(ctx)
}

// MasternodeStatus is a masternode with the server's current state and keys.
func (c *Core) MasternodeStatus(ctx context.Context, assetID uint32, proTxHash string) (*asset.MasternodeStatus, error) {
	mo, err := c.masternodeOwner(assetID)
	if err != nil {
		return nil, err
	}
	return mo.MasternodeStatus(ctx, proTxHash)
}
