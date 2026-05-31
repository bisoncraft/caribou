// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"context"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/electrum"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// ElectrumNetwork is the subset of *electrum.Network's surface that
// ElectrumClient depends on. Defining it as an interface lets us substitute
// a stub in unit tests; the real *electrum.Network satisfies it.
type ElectrumNetwork interface {
	// Lifecycle.
	Start(ctx context.Context) error
	Stop()
	Done() <-chan struct{}
	WaitForShutdown()

	// Header state served from the local store.
	Tip() int32
	Synced() bool
	HeaderByHeight(height int32) *wire.BlockHeader
	HeaderByHash(hash *chainhash.Hash) (int32, *wire.BlockHeader, bool)
	TipHash() chainhash.Hash

	// Notification channels. Closed when the network shuts down.
	TipChangeChan() <-chan int32
	ScripthashChan() <-chan *electrum.ScripthashStatus

	// Scripthash subscriptions.
	SubscribeScripthash(ctx context.Context, scripthash string) (*electrum.ScripthashStatus, error)
	UnsubscribeScripthash(ctx context.Context, scripthash string)

	// Per-call API.
	GetHistory(ctx context.Context, scripthash string) ([]electrum.HistoryEntry, error)
	ListUnspent(ctx context.Context, scripthash string) ([]electrum.UnspentEntry, error)
	GetRawTransaction(ctx context.Context, txid string) (string, error)
	GetTransactionVerbose(ctx context.Context, txid string) (*electrum.VerboseTx, error)
	Broadcast(ctx context.Context, rawTxHex string) (string, error)
	EstimateFee(ctx context.Context, confTarget int32) (int64, error)
}

// electrumNetworkAdapter wraps a concrete *electrum.Network with the small
// adapter methods needed to satisfy ElectrumNetwork. It exists because the
// concrete type exposes HeaderByHash via Headers().ByHash, and we want a flat
// interface for testability.
type electrumNetworkAdapter struct {
	*electrum.Network
}

// HeaderByHash forwards to the underlying headers store.
func (a *electrumNetworkAdapter) HeaderByHash(hash *chainhash.Hash) (int32, *wire.BlockHeader, bool) {
	return a.Network.Headers().ByHash(hash)
}

// TipHash forwards to the underlying headers store.
func (a *electrumNetworkAdapter) TipHash() chainhash.Hash {
	return a.Network.Headers().TipHash()
}

// AdaptNetwork wraps a concrete *electrum.Network so it satisfies the
// ElectrumNetwork interface used by ElectrumClient.
func AdaptNetwork(n *electrum.Network) ElectrumNetwork {
	return &electrumNetworkAdapter{Network: n}
}
