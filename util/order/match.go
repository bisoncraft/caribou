// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package order

import (
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/decred/dcrd/crypto/blake256"
)

// MatchIDSize defines the length in bytes of an MatchID.
const MatchIDSize = blake256.Size

// MatchID is the unique identifier for each match.
type MatchID [MatchIDSize]byte

// MatchID implements fmt.Stringer.
func (id MatchID) String() string {
	return hex.EncodeToString(id[:])
}

// MarshalJSON satisfies the json.Marshaller interface, and will marshal the
// id to a hex string.
func (id MatchID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.String())
}

// Bytes returns the match ID as a []byte.
func (id MatchID) Bytes() []byte {
	return id[:]
}

// Value implements the sql/driver.Valuer interface.
func (id MatchID) Value() (driver.Value, error) {
	return id[:], nil // []byte
}

// Scan implements the sql.Scanner interface.
func (id *MatchID) Scan(src any) error {
	idB, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("cannot convert %T to OrderID", src)
	}
	copy(id[:], idB)
	return nil
}

var zeroMatchID MatchID

// DecodeMatchID checks a string as being both hex and the right length and
// returns its bytes encoded as an order.MatchID.
func DecodeMatchID(matchIDStr string) (MatchID, error) {
	var matchID MatchID
	if len(matchIDStr) != MatchIDSize*2 {
		return matchID, errors.New("match id has incorrect length")
	}
	if _, err := hex.Decode(matchID[:], []byte(matchIDStr)); err != nil {
		return matchID, fmt.Errorf("could not decode match id: %w", err)
	}
	return matchID, nil
}

// MatchStatus represents the current negotiation step for a match.
type MatchStatus uint8

// The different states of order execution.
const (
	// NewlyMatched: DEX has sent match notifications, but the maker has not yet
	// acted.
	NewlyMatched MatchStatus = iota // 0
	// MakerSwapCast: Maker has acknowledged their match notification and
	// broadcast their swap notification. The DEX has validated the swap
	// notification and sent the details to the taker.
	MakerSwapCast // 1
	// TakerSwapCast: Taker has acknowledged their match notification and
	// broadcast their swap notification. The DEX has validated the swap
	// notification and sent the details to the maker.
	TakerSwapCast // 2
	// MakerRedeemed: Maker has acknowledged their audit request and broadcast
	// their redemption transaction. The DEX has validated the redemption and
	// sent the details to the taker.
	MakerRedeemed // 3
	// MatchComplete: Taker has acknowledged their audit request and broadcast
	// their redemption transaction. The DEX has validated the redemption and
	// sent the details to the maker.
	MatchComplete // 4
	// MatchConfirmed is a status used only by the client that represents
	// that the user's redemption or refund transaction has been confirmed.
	MatchConfirmed // 5
)

// String satisfies fmt.Stringer.
func (status MatchStatus) String() string {
	switch status {
	case NewlyMatched:
		return "NewlyMatched"
	case MakerSwapCast:
		return "MakerSwapCast"
	case TakerSwapCast:
		return "TakerSwapCast"
	case MakerRedeemed:
		return "MakerRedeemed"
	case MatchComplete:
		return "MatchComplete"
	case MatchConfirmed:
		return "MatchConfirmed"
	}
	return "MatchStatusUnknown"
}

// MatchSide is the client's side in a match. It will be one of Maker or Taker.
type MatchSide uint8

const (
	// Maker is the order that matches out of the epoch queue.
	Maker MatchSide = iota
	// Taker is the order from the order book.
	Taker
)

func (side MatchSide) String() string {
	switch side {
	case Maker:
		return "Maker"
	case Taker:
		return "Taker"
	}
	return "UnknownMatchSide"
}

// A UserMatch is similar to a Match, but contains less information about the
// counter-party, and it clarifies which side the user is on. This is the
// information that might be provided to the client when they are resyncing
// their matches after a reconnect.
type UserMatch struct {
	OrderID  OrderID
	MatchID  MatchID
	Quantity uint64
	Rate     uint64
	// Deprecated: Address is the order-level counterparty address. It is no
	// longer used as a contract recipient. Per-match swap addresses
	// (MetaData.CounterPartyAddr on the client, SwapData.MakerSwapAddr/
	// TakerSwapAddr on the server) are used instead. This field is retained
	// for cancel match detection (empty Address == cancel) and data export.
	Address     string
	Status      MatchStatus
	Side        MatchSide
	FeeRateSwap uint64
	// TODO: include Sell bool?
}

// String is the match ID string, implements fmt.Stringer.
func (m *UserMatch) String() string {
	return m.MatchID.String()
}
