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

// unlockedMasternodeOwner is masternodeOwner with the wallet connected and
// unlocked with pw.
func (c *Core) unlockedMasternodeOwner(assetID uint32, pw []byte) (asset.MasternodeOwner, error) {
	mo, err := c.masternodeOwner(assetID)
	if err != nil {
		return nil, err
	}
	wallet, _ := c.wallet(assetID)
	crypter, err := c.encryptionKey(pw)
	if err != nil {
		return nil, fmt.Errorf("password error: %w", err)
	}
	defer crypter.Close()
	if err := c.connectAndUnlock(crypter, wallet); err != nil {
		return nil, err
	}
	return mo, nil
}

// CreateMasternodeOffer commits an amount of the wallet to a shared
// registration that others build.
func (c *Core) CreateMasternodeOffer(ctx context.Context, assetID uint32, pw []byte, amount uint64) (*asset.MasternodeOffer, error) {
	mo, err := c.unlockedMasternodeOwner(assetID, pw)
	if err != nil {
		return nil, err
	}
	offer, err := mo.CreateMasternodeOffer(ctx, amount)
	if err != nil {
		return nil, err
	}
	c.updateAssetBalance(assetID)
	return offer, nil
}

// MasternodeOffers lists the wallet's offers.
func (c *Core) MasternodeOffers(ctx context.Context, assetID uint32) ([]*asset.MasternodeOffer, error) {
	mo, err := c.masternodeOwner(assetID)
	if err != nil {
		return nil, err
	}
	offers, err := mo.MasternodeOffers(ctx)
	if err != nil {
		return nil, err
	}
	return offers, nil
}

// CancelMasternodeOffer releases an offer's inputs and deletes it.
func (c *Core) CancelMasternodeOffer(ctx context.Context, assetID uint32, offerID string) error {
	mo, err := c.masternodeOwner(assetID)
	if err != nil {
		return err
	}
	if err := mo.CancelMasternodeOffer(ctx, offerID); err != nil {
		return err
	}
	c.updateAssetBalance(assetID)
	return nil
}

// ReviewMasternodeRegistration checks a registration against an offer.
func (c *Core) ReviewMasternodeRegistration(ctx context.Context, assetID uint32, offerID, blob string) (*asset.MasternodeRegReview, error) {
	mo, err := c.masternodeOwner(assetID)
	if err != nil {
		return nil, err
	}
	return mo.ReviewMasternodeRegistration(ctx, offerID, blob)
}

// SignMasternodeRegistration signs the registration that reviewID was returned
// for.
func (c *Core) SignMasternodeRegistration(ctx context.Context, assetID uint32, pw []byte, offerID, blob, reviewID string) (*asset.MasternodeRegSigned, error) {
	mo, err := c.unlockedMasternodeOwner(assetID, pw)
	if err != nil {
		return nil, err
	}
	return mo.SignMasternodeRegistration(ctx, offerID, blob, reviewID)
}

// BroadcastMasternodeRegistration broadcasts a fully signed registration.
func (c *Core) BroadcastMasternodeRegistration(ctx context.Context, assetID uint32, offerID, blob string) (string, error) {
	mo, err := c.masternodeOwner(assetID)
	if err != nil {
		return "", err
	}
	txid, err := mo.BroadcastMasternodeRegistration(ctx, offerID, blob)
	if err != nil {
		return txid, err
	}
	c.updateAssetBalance(assetID)
	return txid, nil
}
