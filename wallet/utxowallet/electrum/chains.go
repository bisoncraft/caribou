// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"fmt"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
)

// ChainConfig captures the per-chain knobs needed to drive an electrumx
// connection. It is derived from a *netparams.ChainParams plus a small amount
// of electrumx-specific metadata (header size, sync start height).
type ChainConfig struct {
	// Chain identifies the chain. It controls block/tx decoding via the
	// bisonwire package.
	Chain bisonwire.Chain

	// Params is the canonical chain parameter set. Used for genesis hash
	// verification against the server's reported genesis_hash.
	Params *netparams.ChainParams

	// HeaderSize is the serialized block-header size in bytes. 80 for both
	// BTC and LTC.
	HeaderSize int

	// StartHeight is the lowest height we will sync headers from. Below
	// this we trust the chain implicitly. For regtest this is 0; for
	// mainnet/testnet we may want a more recent checkpoint to bound the
	// initial sync.
	StartHeight int32
}

// NewChainConfig returns a ChainConfig for the given chain and params, using
// the standard defaults (80-byte header, sync-from-genesis on regtest).
//
// Dash uses the same 80-byte header layout as BTC/LTC; only its header hash
// differs (X11), which is handled via the netparams HeaderHash hook, not here.
func NewChainConfig(chain bisonwire.Chain, params *netparams.ChainParams) (*ChainConfig, error) {
	switch chain {
	case bisonwire.ChainBTC, bisonwire.ChainLTC, bisonwire.ChainDASH:
	default:
		return nil, fmt.Errorf("electrum: unsupported chain %q", chain)
	}
	if params == nil {
		return nil, fmt.Errorf("electrum: nil chain params")
	}
	if params.GenesisHash == nil {
		return nil, fmt.Errorf("electrum: chain params missing genesis hash")
	}
	return &ChainConfig{
		Chain:       chain,
		Params:      params,
		HeaderSize:  80,
		StartHeight: 0,
	}, nil
}
