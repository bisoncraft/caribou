// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperevm

import (
	"fmt"
	"math/big"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/eth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

// mainnetCompatibilityData is verified live against
// https://rpc.hyperliquid.xyz/evm (2026-08-22): Addr's balance, TxHash's
// receipt (block 43883683 / 0x29d9ca3), and BlockHash all resolve, and
// eth_getBlockByNumber confirms EIP-1559 support (baseFeePerGas present).
var mainnetCompatibilityData = eth.CompatibilityData{
	Addr:      common.HexToAddress("0x7e9401990267577a30ea452e2c2d4d35bcaa62b9"),
	TokenAddr: common.Address{},
	TxHash:    common.HexToHash("0x210b74bf3ab0bd57b187889a64818a6f0c5a9cc66b89175b7be3c7c4488cf463"),
	BlockHash: common.HexToHash("0x9da92844e5f407106223a5370c901ceabe8e65cb434e23d97c2652e46db3e12d"),
}

// NetworkCompatibilityData returns the CompatibilityData for the specified
// network. Only Mainnet is currently supported — see ChainIDs.
func NetworkCompatibilityData(net util.Network) (c eth.CompatibilityData, err error) {
	switch net {
	case util.Mainnet:
		return mainnetCompatibilityData, nil
	default:
		return c, fmt.Errorf("no compatibility data for network # %d", net)
	}
}

// ChainConfig returns the core configuration for the blockchain. Only
// Mainnet is currently supported: HyperEVM testnet's RPC endpoint
// (rpc.hyperliquid-testnet.xyz) could not be reached to verify its chain ID,
// and there is no local HyperEVM node for simnet testing.
func ChainConfig(net util.Network) (c *params.ChainConfig, err error) {
	c = new(params.ChainConfig)
	switch net {
	case util.Mainnet:
		c.ChainID = big.NewInt(ChainIDs[net])
		// HyperEVM supports EIP-1559 typed transactions from genesis.
		c.LondonBlock = big.NewInt(0)
		return c, nil
	default:
		return c, fmt.Errorf("unknown network %d", net)
	}
}
