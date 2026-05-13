// Copyright (c) 2013-2015 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package netparams

import (
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
)

// NMainnetParams are chain parameters sufficient for packages that only need
// the network identifier and genesis block data for Bitcoin mainnet.
var NMainnetParams = &ChainParams{
	Net:          wire.MainNet,
	GenesisBlock: chaincfg.MainNetParams.GenesisBlock,
	GenesisHash:  chaincfg.MainNetParams.GenesisHash,
}

// NSimnetParams are chain parameters sufficient for packages that only need
// the network identifier and genesis block data for Bitcoin simnet.
var NSimnetParams = &ChainParams{
	Net:          chaincfg.SimNetParams.Net,
	GenesisBlock: chaincfg.SimNetParams.GenesisBlock,
	GenesisHash:  chaincfg.SimNetParams.GenesisHash,
}
