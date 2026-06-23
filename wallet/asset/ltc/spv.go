// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package ltc

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/assets"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	_ "github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb/bdb"
	"github.com/btcsuite/btcd/chaincfg"
)

const (
	DefaultM       uint64 = 784931 // From ltcutil. Used for gcs filters.
	neutrinoDBName        = "neutrino.db"
	defaultAcctNum        = 0
	dbTimeout             = 20 * time.Second
)

var (
	waddrmgrNamespace = []byte("waddrmgr")

	// Snapshot of valid peers. 10 May 2024
	testnet4Seeds = []string{
		"13.200.66.216:19335",
		"208.91.111.150:18333",
		"92.244.111.167:19335",
		"164.92.171.95:8333",
		"204.16.244.114:18333",
		"34.227.13.195:19335",
		"18.192.56.149:18333",
	}
)

// ltcNetParams returns the *netparams.ChainParams for a given *chaincfg.Params
// by looking up the LTC network in the assets table.
func ltcNetParams(btcParams *chaincfg.Params) *netparams.ChainParams {
	switch btcParams.Name {
	case MainNetParams.Name:
		return assets.LTCParams["mainnet"]
	case TestNet4Params.Name:
		return assets.LTCParams["testnet"]
	case RegressionNetParams.Name:
		return assets.LTCParams["simnet"]
	default:
		return assets.LTCParams["mainnet"]
	}
}

// createSPVWallet creates a new SPV wallet.
func createSPVWallet(privPass []byte, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32, net *chaincfg.Params) error {
	np := ltcNetParams(net)
	loader := wallet.NewLoader(np, walletDir, true, dbTimeout, 250)

	pubPass := []byte(wallet.InsecurePubPassphrase)

	ltcw, err := loader.CreateNewWallet(pubPass, privPass, seed, bday)
	if err != nil {
		return fmt.Errorf("CreateNewWallet error: %w", err)
	}

	errCloser := util.NewErrorCloser()
	defer errCloser.Done(log)
	errCloser.Add(loader.UnloadWallet)

	if extIdx > 0 || intIdx > 0 {
		err = extendAddresses(extIdx, intIdx, ltcw)
		if err != nil {
			return fmt.Errorf("failed to set starting address indexes: %w", err)
		}
	}

	// The chain service DB
	neutrinoDBPath := filepath.Join(walletDir, neutrinoDBName)
	db, err := walletdb.Create("bdb", neutrinoDBPath, true, dbTimeout)
	if err != nil {
		return fmt.Errorf("unable to create neutrino db at %q: %w", neutrinoDBPath, err)
	}
	if err = db.Close(); err != nil {
		return fmt.Errorf("error closing newly created wallet database: %w", err)
	}

	if err := loader.UnloadWallet(); err != nil {
		return fmt.Errorf("error unloading wallet: %w", err)
	}

	errCloser.Success()
	return nil
}

func extendAddresses(extIdx, intIdx uint32, ltcw *wallet.Wallet) error {
	scopedKeyManager, err := ltcw.Manager.FetchScopedKeyManager(ltcw.ScopeForPurpose(84))
	if err != nil {
		return err
	}

	return walletdb.Update(ltcw.Database(), func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(waddrmgrNamespace)
		if extIdx > 0 {
			if err := scopedKeyManager.ExtendExternalAddresses(ns, defaultAcctNum, extIdx); err != nil {
				return err
			}
		}
		if intIdx > 0 {
			return scopedKeyManager.ExtendInternalAddresses(ns, defaultAcctNum, intIdx)
		}
		return nil
	})
}

// bisonwire type needed for TxRecord construction in tests.
var _ = bisonwire.Tx{}
