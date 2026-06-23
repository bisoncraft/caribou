package btc

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
)

const (
	dbTimeout = 20 * time.Second
)

// btcNetParams wraps a *chaincfg.Params in a *netparams.ChainParams suitable
// for the in-tree utxowallet library. The btcdParams field is set so that
// netparams.ChainParams.BTCDParams() returns the original *chaincfg.Params.
func btcNetParams(p *chaincfg.Params) *netparams.ChainParams {
	np := &netparams.ChainParams{
		Chain:                    "btc",
		Name:                     p.Name,
		Net:                      p.Net,
		GenesisBlock:             p.GenesisBlock,
		GenesisHash:              p.GenesisHash,
		TargetTimespan:           p.TargetTimespan,
		TargetTimePerBlock:       p.TargetTimePerBlock,
		RetargetAdjustmentFactor: p.RetargetAdjustmentFactor,
		ReduceMinDifficulty:      p.ReduceMinDifficulty,
		MinDiffReductionTime:     p.MinDiffReductionTime,
		Checkpoints:              p.Checkpoints,
		PowLimit:                 p.PowLimit,
		PowLimitBits:             p.PowLimitBits,
		PoWNoRetargeting:         p.PoWNoRetargeting,
		DNSSeeds:                 p.DNSSeeds,
		DefaultPort:              p.DefaultPort,
		Bech32HRPSegwit:          p.Bech32HRPSegwit,
		PubKeyHashAddrID:         p.PubKeyHashAddrID,
		ScriptHashAddrID:         p.ScriptHashAddrID,
		PrivateKeyID:             p.PrivateKeyID,
		WitnessPubKeyHashAddrID:  p.WitnessPubKeyHashAddrID,
		WitnessScriptHashAddrID:  p.WitnessScriptHashAddrID,
		HDPrivateKeyID:           p.HDPrivateKeyID,
		HDPublicKeyID:            p.HDPublicKeyID,
		HDCoinType:               p.HDCoinType,
		BIP0034Height:            p.BIP0034Height,
		BIP0065Height:            p.BIP0065Height,
		BIP0066Height:            p.BIP0066Height,
		CoinbaseMaturity:         p.CoinbaseMaturity,
		MaxSatoshi:               btcutil.MaxSatoshi,
	}
	return np
}

// createSPVWallet creates a new SPV wallet.
func createSPVWallet(privPass []byte, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32, net *chaincfg.Params) error {
	loader := wallet.NewLoader(btcNetParams(net), walletDir, true, dbTimeout, 250)

	pubPass := []byte(wallet.InsecurePubPassphrase)

	// CreateWallet adds a -48 hrs buffer on the bday during creation.
	btcw, err := loader.CreateNewWallet(pubPass, privPass, seed, bday)
	if err != nil {
		return fmt.Errorf("CreateNewWallet error: %w", err)
	}

	bailOnWallet := func() {
		if err := loader.UnloadWallet(); err != nil {
			log.Errorf("Error unloading wallet after createSPVWallet error: %v", err)
		}
	}

	if extIdx > 0 || intIdx > 0 {
		err = extendAddresses(extIdx, intIdx, btcw)
		if err != nil {
			bailOnWallet()
			return fmt.Errorf("failed to set starting address indexes: %w", err)
		}
	}

	// The chain service DB
	neutrinoDBPath := filepath.Join(walletDir, neutrinoDBName)
	db, err := walletdb.Create("bdb", neutrinoDBPath, true, dbTimeout)
	if err != nil {
		bailOnWallet()
		return fmt.Errorf("unable to create neutrino db at %q: %w", neutrinoDBPath, err)
	}
	if err = db.Close(); err != nil {
		bailOnWallet()
		return fmt.Errorf("error closing newly created wallet database: %w", err)
	}

	if err := loader.UnloadWallet(); err != nil {
		return fmt.Errorf("error unloading wallet: %w", err)
	}

	return nil
}
