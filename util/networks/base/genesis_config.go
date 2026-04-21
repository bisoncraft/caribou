package base

import (
	"github.com/bisoncraft/meshwallet/util"
)

// These are the chain IDs of the various base networks.
const (
	MainnetChainID = 8453
	TestnetChainID = 84532
	SimnetChainID  = 1337
)

var (
	// ChainIDs is a map of the network name to it's chain ID.
	ChainIDs = map[util.Network]int64{
		util.Simnet:  SimnetChainID,
		util.Mainnet: MainnetChainID,
		util.Testnet: TestnetChainID,
	}
)
