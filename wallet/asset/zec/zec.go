// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package zec is the Zcash asset wallet: a light wallet on zeclib, seeded by
// core, that holds shielded funds in the Ironwood pool and transparent funds.
package zec

import (
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
)

func init() {
	asset.Register(BipID, &Driver{})
}

// Driver is the Zcash asset driver.
type Driver struct{}

var (
	_ asset.Driver  = (*Driver)(nil)
	_ asset.Creator = (*Driver)(nil)
)

// Open builds the wallet. It opens nothing: the database is opened by
// OpenWithPW, or by Connect when the wallet is opened without a password.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	w, err := newWallet(cfg, logger, network)
	if err != nil {
		if logger == nil {
			logger = util.Disabled
		}
		return nil, explain(logger, err)
	}
	return w, nil
}

// DecodeCoinID returns the transaction ID of a coin ID in display order.
func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	if len(coinID) != 32 {
		return "", fmt.Errorf("a coin ID is 32 bytes, got %d", len(coinID))
	}
	return displayTxID(coinID), nil
}

// Info returns the wallet information.
func (d *Driver) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

// Exists reports whether a wallet has been created in dataDir.
func (d *Driver) Exists(walletType, dataDir string, _ map[string]string, _ util.Network) (bool, error) {
	if walletType != walletTypeNative {
		return false, fmt.Errorf("unknown wallet type %q", walletType)
	}
	return newSeedStore(dataDir).exists()
}

// displayTxID returns a transaction ID in wire byte order as hexadecimal in
// display order, which is reversed.
func displayTxID(wire []byte) string {
	b := slices.Clone(wire)
	slices.Reverse(b)
	return hex.EncodeToString(b)
}
