// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/bisoncraft/caribou/util"
	"github.com/ethereum/go-ethereum/common"
	ethcore "github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/params"
)

// CompatibilityData is some addresses and hashes used for validating an RPC
// APIs compatibility with trading.
type CompatibilityData struct {
	Addr      common.Address
	TokenAddr common.Address
	TxHash    common.Hash
	BlockHash common.Hash
}

var (
	mainnetCompatibilityData = CompatibilityData{
		// Vitalik's address from https://twitter.com/VitalikButerin/status/1050126908589887488
		Addr:      common.HexToAddress("0xab5801a7d398351b8be11c439e05c5b3259aec9b"),
		TokenAddr: common.HexToAddress("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"),
		TxHash:    common.HexToHash("0x67b27503aa20dd35bc2ae18fabfec477cf4d089b32173a590dc8d7319cbd0184"),
		BlockHash: common.HexToHash("0x95118897eba948a62df72765e20186081f2554d90a146403ba93da690b322e06"),
	}

	testnetCompatibilityData = CompatibilityData{
		Addr:      common.HexToAddress("0x1268ad189526ac0b386faf06effc46779c340ee6"),
		TokenAddr: common.HexToAddress("0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238"), // USDC
		TxHash:    common.HexToHash("0x854860598c69e9b9e259fe74ca88610752428541e747180e0d40a4409d036f02"),
		BlockHash: common.HexToHash("0x7f3cd2786bc82872ffb55eface91a0afb927a9868adbb16a8d7939fef7385772"),
	}
)

// NetworkCompatibilityData returns the CompatibilityData for the specified
// network. If using simnet, make sure the simnet harness is running.
func NetworkCompatibilityData(net util.Network) (c CompatibilityData, err error) {
	switch net {
	case util.Mainnet:
		return mainnetCompatibilityData, nil
	case util.Testnet:
		return testnetCompatibilityData, nil
	case util.Simnet:
	default:
		return c, fmt.Errorf("no compatibility data for network # %d", net)
	}
	// simnet
	tDir, err := simnetDataDir()
	if err != nil {
		return
	}
	return ReadSimnetCompatibilityData(tDir, common.HexToAddress("18d65fb8d60c1199bb1ad381be47aa692b482605"), common.Address{})
}

// simnetDataDir returns the data directory for Ethereum simnet.
func simnetDataDir() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("error getting current user: %w", err)
	}

	return filepath.Join(u.HomeDir, "dextest", "eth"), nil
}

// ReadSimnetCompatibilityData reads simnet compatibility data from the harness
// info files in tDir. tokenAddr may be the zero address, in which case it is
// read from test_usdc_contract_address.txt.
func ReadSimnetCompatibilityData(tDir string, walletAddr common.Address, tokenAddr common.Address) (CompatibilityData, error) {
	readFile := func(path string) (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}

	txHash, err := readFile(filepath.Join(tDir, "test_tx_hash.txt"))
	if err != nil {
		return CompatibilityData{}, fmt.Errorf("read test tx hash: %w", err)
	}
	blockHash, err := readFile(filepath.Join(tDir, "test_block1_hash.txt"))
	if err != nil {
		return CompatibilityData{}, fmt.Errorf("read test block hash: %w", err)
	}
	if tokenAddr == (common.Address{}) {
		contractAddr, err := readFile(filepath.Join(tDir, "test_usdc_contract_address.txt"))
		if err != nil {
			return CompatibilityData{}, fmt.Errorf("read test token address: %w", err)
		}
		tokenAddr = common.HexToAddress(contractAddr)
	}

	return CompatibilityData{
		Addr:      walletAddr,
		TokenAddr: tokenAddr,
		TxHash:    common.HexToHash(txHash),
		BlockHash: common.HexToHash(blockHash),
	}, nil
}

// ETHConfig returns the ETH protocol configuration for the specified network.
func ETHConfig(net util.Network) (c ethconfig.Config, err error) {
	c = ethconfig.Defaults
	switch net {
	// Ethereum
	case util.Mainnet:
		c.Genesis = ethcore.DefaultGenesisBlock()
	case util.Testnet:
		c.Genesis = ethcore.DefaultSepoliaGenesisBlock()
	case util.Simnet:
		// Args are gasLimit, faucet address.
		c.Genesis = ethcore.DeveloperGenesisBlock(30000000, nil)
	default:
		return c, fmt.Errorf("unknown network %d", net)

	}
	c.NetworkId = c.Genesis.Config.ChainID.Uint64()
	return
}

// ChainConfig returns the core configuration for the blockchain.
func ChainConfig(net util.Network) (c *params.ChainConfig, err error) {
	cfg, err := ETHConfig(net)
	if err != nil {
		return nil, err
	}
	return cfg.Genesis.Config, nil
}
