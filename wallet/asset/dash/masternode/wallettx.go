// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import "encoding/binary"

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
