// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"fmt"
)

// OwnerIndexStore hands out owner key indexes. An index must never be handed
// out twice, even across restarts: a share's owner key is revealed to the other
// participants and signs for the share, so reusing it for another registration
// would link the two.
type OwnerIndexStore interface {
	// ReserveOwnerIndex returns the next unused index and durably records
	// it as used before returning. It does not matter if the registration
	// is later abandoned: the index stays used.
	ReserveOwnerIndex() (uint32, error)
}

// NewShareOwner reserves a fresh owner index and returns it with the key id
// (hash160 of the compressed public key) of the owner key at that index, which
// goes in the share table.
//
// The index is reserved and recorded before the key is derived, so a failure
// after the reservation wastes an index and never reuses one. The private key
// is not kept: the owner re-derives it with OwnerKey(d, coinType, index) to
// sign.
func NewShareOwner(s OwnerIndexStore, d KeyDeriver, coinType uint32) (uint32, [20]byte, error) {
	var id [20]byte
	idx, err := s.ReserveOwnerIndex()
	if err != nil {
		return 0, id, fmt.Errorf("reserve owner index: %w", err)
	}
	id, err = ownerKeyID(d, coinType, idx)
	if err != nil {
		return 0, id, err
	}
	return idx, id, nil
}
