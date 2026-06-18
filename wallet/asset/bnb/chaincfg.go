// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package bnb

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset/eth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

var (
	userHomeDir = os.UserHomeDir

	mainnetCompatibilityData = eth.CompatibilityData{
		Addr:      common.HexToAddress("0x50e7cd491fa11d54b73913b58459cad5fd66a40a"),
		TokenAddr: common.Address{},
		TxHash:    common.HexToHash("0x2855d50aa3581144e6e1e34035e6673c970a8cbbc47d497a22ced9e81002048b"),
		BlockHash: common.HexToHash("0xed86583437cc82ccfb3e9f64dc7a68107ae5804cb2fcbf8f3638dd79931fb520"),
	}

	testnetCompatibilityData = eth.CompatibilityData{
		Addr:      common.HexToAddress("0x020ea48f8f18d5c56c41d105921e3aafd98f99b3"),
		TokenAddr: common.Address{},
		TxHash:    common.HexToHash("0xf66881fa292c808bc733d695746a8b02b0881f76f88fc34a47e55a730e005088"),
		BlockHash: common.HexToHash("0x81cf5121bce68058248e17e00ef95166debf8dec57007215c2b438e37f75a251"),
	}
)

// NetworkCompatibilityData returns the CompatibilityData for the specified
// network.
func NetworkCompatibilityData(net util.Network) (c eth.CompatibilityData, err error) {
	switch net {
	case util.Mainnet:
		return mainnetCompatibilityData, nil
	case util.Testnet:
		return testnetCompatibilityData, nil
	case util.Simnet:
		return simnetCompatibilityData()
	default:
		return c, fmt.Errorf("no compatibility data for network # %d", net)
	}
}

func simnetCompatibilityData() (eth.CompatibilityData, error) {
	tDir, err := simnetDataDir()
	if err != nil {
		return eth.CompatibilityData{}, err
	}
	return eth.ReadSimnetCompatibilityData(
		tDir,
		common.HexToAddress("0x18d65fb8d60c1199bb1ad381be47aa692b482605"),
		simnetUSDC.Address,
	)
}

func simnetDataDir() (string, error) {
	homeDir, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("error getting home directory: %w", err)
	}
	return filepath.Join(homeDir, "dextest", "bnb"), nil
}

// ChainConfig returns the core configuration for the blockchain.
func ChainConfig(net util.Network) (c *params.ChainConfig, err error) {
	c = new(params.ChainConfig)
	switch net {
	case util.Mainnet, util.Testnet, util.Simnet:
		c.ChainID = big.NewInt(ChainIDs[net])
		// BSC supports EIP-1559 typed transactions. Mark London active at
		// genesis so go-ethereum uses a London-capable signer instead of
		// falling back to HomesteadSigner for sender recovery.
		c.LondonBlock = big.NewInt(0)
		return c, nil
	default:
		return c, fmt.Errorf("unknown network %d", net)
	}
}
