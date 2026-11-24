// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"encoding/binary"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// IsProviderTx reports whether raw is a serialized shared masternode
// registration or dissolution, by the type in its version word, which is all
// that is needed to pick them out of a wallet's transactions. It does not decode
// the transaction: Track does that for what it picks.
func IsProviderTx(raw []byte) bool {
	if len(raw) < 4 {
		return false
	}
	v := binary.LittleEndian.Uint32(raw)
	version, typ := v&0xffff, v>>16
	return version == txVersion && (typ == txTypeProReg || typ == txTypeProDis)
}

// Discovered reports whether a Discover has read the history of the shared
// collateral script to the end. One that the server refused has not.
func (m *Manager) Discovered() (bool, error) {
	snap, err := m.store.Load()
	if err != nil {
		return false, err
	}
	return snap.Cursor.Height > 0, nil
}

// Tip is the height of the chain's tip.
func (m *Manager) Tip() int32 { return m.chain.Tip() }

// TxRead is the height the wallet's transactions have been given to Track
// through, or 0 if none have.
func (m *Manager) TxRead() (int32, error) {
	snap, err := m.store.Load()
	if err != nil {
		return 0, err
	}
	return snap.TxRead, nil
}

// SetTxRead records that the wallet's transactions have been given to Track
// through height.
func (m *Manager) SetTxRead(height int32) error {
	return m.store.Save(Update{TxRead: &height})
}

// Tracked returns the transactions that are waiting to be proven and that came
// from the wallet's own transactions: not those put back after a reorg, nor
// those added by id.
func (m *Manager) Tracked() ([]chainhash.Hash, error) {
	snap, err := m.store.Load()
	if err != nil {
		return nil, err
	}
	ids := make([]chainhash.Hash, 0, len(snap.Tracked))
	for id := range snap.Tracked {
		if _, ok := snap.FromWallet[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
