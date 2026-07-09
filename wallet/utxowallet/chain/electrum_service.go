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
	StartHeight() int32
	Synced() bool
	// TargetTip returns the server's reported best height (from the initial
	// SubscribeHeaders reply and updated on each tip-change notification). It
	// is always >= Tip() and lets callers show meaningful sync progress while
	// the local store is still catching up. Returns 0 if not yet known.
	TargetTip() int32
	HeaderByHeight(height int32) *wire.BlockHeader
	HeaderByHash(hash *chainhash.Hash) (int32, *wire.BlockHeader, bool)
	TipHash() chainhash.Hash

	// Notification channels. Closed when the network shuts down.
	TipChangeChan() <-chan *electrum.TipChange
	ScripthashChan() <-chan *electrum.ScripthashStatus

	// Scripthash subscriptions.
	SubscribeScripthash(ctx context.Context, scripthash string) (*electrum.ScripthashStatus, error)
	UnsubscribeScripthash(ctx context.Context, scripthash string)

	// Per-call API.
	GetHistory(ctx context.Context, scripthash string) ([]electrum.HistoryEntry, error)
	ListUnspent(ctx context.Context, scripthash string) ([]electrum.UnspentEntry, error)
	GetRawTransaction(ctx context.Context, txid string) (string, error)
	GetMerkle(ctx context.Context, txid string, height int32) (*electrum.MerkleProof, error)
	GetTransactionVerbose(ctx context.Context, txid string) (*electrum.VerboseTx, error)
	Broadcast(ctx context.Context, rawTxHex string) (string, error)
	EstimateFee(ctx context.Context, confTarget int32) (int64, error)
}

// Compile-time check: *electrum.Network satisfies ElectrumNetwork.
var _ ElectrumNetwork = (*electrum.Network)(nil)
