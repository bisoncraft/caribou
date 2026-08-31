// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// NEAR Intents swap-record storage. Swap records live in their own table of
// the core database, keyed by the swap's single-use deposit address, with
// two secondary indexes derived from the record itself: the creation stamp,
// for newest-first listing, and an active index holding only records whose
// state is not terminal, so the status poller reads just the swaps that
// still need polling. A record re-stored in a terminal state leaves the
// active index (both storage engines purge a datum's old index entries on
// replace).

package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/bisoncraft/caribou/wallet/intents"
)

const (
	intentsSwapsTable = "intentsSwaps"

	// intentsStampIndexName indexes every swap record by its creation
	// stamp, enabling newest-first listing without a scan-and-sort.
	intentsStampIndexName = "intentsStamp"

	// intentsActiveIndexName indexes only records in a non-terminal state,
	// by creation stamp, so the poller's working set is read directly
	// rather than filtered out of the full table.
	intentsActiveIndexName = "intentsActive"
)

// ErrIntentsSwapNotFound is returned by IntentsSwap for an unknown deposit
// address.
var ErrIntentsSwapNotFound = util.ErrorKind("intents swap not found")

// Engine-local swap states. All other states mirror the service statuses in
// the intents package (StatusProcessing, StatusSuccess, ...).
const (
	// IntentsStateQuoted: a committed quote exists but the deposit has not
	// been broadcast.
	IntentsStateQuoted = "QUOTED"
	// IntentsStateDepositSent: the origin-chain deposit is broadcast but
	// the service hasn't yet observed it.
	IntentsStateDepositSent = "DEPOSIT_SENT"
	// IntentsStateExpired: never funded and the deadline (plus grace) has
	// passed. Terminal, local-only.
	IntentsStateExpired = "EXPIRED"
	// IntentsStateDepositFailed: the deposit transaction was mined but
	// rejected on-chain, so the service will never observe a deposit. The
	// deposited funds never left the wallet, though the network fee was
	// spent. Terminal, local-only.
	IntentsStateDepositFailed = "DEPOSIT_FAILED"
)

// IntentsSwap is a persisted cross-chain swap record, keyed by its
// single-use deposit address. Amount strings are in the Intents-side base
// units; Atoms fields are wallet atomic units.
type IntentsSwap struct {
	DepositAddress string `json:"depositAddress"`
	DepositMemo    string `json:"depositMemo,omitempty"`
	State          string `json:"state"`
	OriginAssetID  uint32 `json:"originAssetID"`
	DestAssetID    uint32 `json:"destAssetID"`
	OriginAsset    string `json:"originAsset"` // Intents asset ID
	DestAsset      string `json:"destAsset"`   // Intents asset ID
	AtomsIn        uint64 `json:"atomsIn"`
	AmountIn       string `json:"amountIn"`
	// AmountOut/AtomsOut hold the quoted output until the service reports
	// the realized fill in its swap details, which then replaces them; a
	// completed swap's record shows what was actually delivered. The
	// quoted terms remain auditable in QuoteEnvelope.
	AmountOut    string    `json:"amountOut"` // destination units
	AtomsOut     uint64    `json:"atomsOut"`  // destination wallet atoms
	Recipient    string    `json:"recipient"`
	RefundTo     string    `json:"refundTo"`
	Deadline     time.Time `json:"deadline"`
	TimeEstimate int64     `json:"timeEstimate"` // seconds, from the quote

	DepositTxID string `json:"depositTxID,omitempty"`
	// DepositTxFee is the network fee the origin wallet paid for the
	// deposit transaction, denominated in the origin network's parent-asset
	// atoms (gas for a token deposit is paid in the chain native, not the
	// token). Wallets that estimate fees at send time (EVM) refine the
	// recorded fee once the transaction settles, so the value is
	// re-captured on every status poll while the swap is active. Zero means
	// not yet known.
	DepositTxFee   uint64   `json:"depositTxFee,omitempty"`
	NearTxHashes   []string `json:"nearTxHashes,omitempty"`
	DestinationTxs []string `json:"destinationTxs,omitempty"`
	RefundTxs      []string `json:"refundTxs,omitempty"`
	RefundedAmount string   `json:"refundedAmount,omitempty"`
	RefundReason   string   `json:"refundReason,omitempty"`

	// USD values are captured when the corresponding amounts are recorded,
	// since exchange rates drift and a later conversion would misprice the
	// history. USDIn and USDOut carry the service's own pricing from the
	// committed quote, replaced by the status endpoint's swap-details
	// pricing when reported (the realized fill). USDDepositFee and
	// USDRefunded are derived from the catalog spot price at capture time.
	// Zero means not captured.
	USDIn         float64 `json:"usdIn,omitempty"`
	USDOut        float64 `json:"usdOut,omitempty"`
	USDDepositFee float64 `json:"usdDepositFee,omitempty"`
	USDRefunded   float64 `json:"usdRefunded,omitempty"`

	// AppFees is the service-rewritten fee breakdown echoed with the
	// quote: the application fee split plus the keyless surcharge entry.
	AppFees []intents.AppFee `json:"appFees,omitempty"`

	// QuoteEnvelope is the full signed /quote response, verified at quote
	// time and retained for audit. Reconstructed records (post-restore)
	// carry the status endpoint's re-serialized, non-verifiable copy and
	// set Reconstructed.
	QuoteEnvelope json.RawMessage `json:"quoteEnvelope,omitempty"`
	Reconstructed bool            `json:"reconstructed,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Terminal reports whether the swap needs no further status polling.
func (s *IntentsSwap) Terminal() bool {
	return s.State == IntentsStateExpired || s.State == IntentsStateDepositFailed ||
		intents.TerminalStatus(s.State)
}

// intentsSwapRecordVersion is the stored-record encoding version.
const intentsSwapRecordVersion = 0

// MarshalBinary satisfies encoding.BinaryMarshaler, encoding the stored swap
// record: version(1) || JSON. The typed swap is what gets passed to Set, so
// the index functions recover it with a type assertion instead of re-decoding
// the stored bytes.
func (s *IntentsSwap) MarshalBinary() ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("error encoding swap record: %w", err)
	}
	return append([]byte{intentsSwapRecordVersion}, b...), nil
}

// decodeIntentsSwap decodes a stored swap record.
func decodeIntentsSwap(b []byte) (*IntentsSwap, error) {
	if len(b) < 1 {
		return nil, errors.New("empty swap record")
	}
	if ver := b[0]; ver != intentsSwapRecordVersion {
		return nil, fmt.Errorf("unknown swap record version %d", ver)
	}
	swap := new(IntentsSwap)
	if err := json.Unmarshal(b[1:], swap); err != nil {
		return nil, fmt.Errorf("error decoding swap record: %w", err)
	}
	return swap, nil
}

// intentsSwapStamp is the 8-byte big-endian index stamp for a swap.
func intentsSwapStamp(swap *IntentsSwap) []byte {
	return uint64Bytes(uint64(swap.CreatedAt.UnixMilli()))
}

// intentsSwapV asserts an intentsSwaps-table value to the *IntentsSwap under
// insertion.
func intentsSwapV(vI kvstore.KV) (*IntentsSwap, error) {
	swap, ok := vI.(*IntentsSwap)
	if !ok {
		return nil, fmt.Errorf("expected *IntentsSwap value, got %T", vI)
	}
	return swap, nil
}

// intentsStampIndexFunc derives the stamp index entry — the swap's creation
// time — from the swap under insertion.
func intentsStampIndexFunc(_, vI kvstore.KV) ([]byte, error) {
	swap, err := intentsSwapV(vI)
	if err != nil {
		return nil, err
	}
	return intentsSwapStamp(swap), nil
}

// intentsActiveIndexFunc derives the active index entry, declining to index
// records in a terminal state.
func intentsActiveIndexFunc(_, vI kvstore.KV) ([]byte, error) {
	swap, err := intentsSwapV(vI)
	if err != nil {
		return nil, err
	}
	if swap.Terminal() {
		return nil, kvstore.ErrNotIndexed
	}
	return intentsSwapStamp(swap), nil
}

// StoreIntentsSwap stores the swap record under its deposit address,
// stamping UpdatedAt and replacing any existing record. CreatedAt orders
// the listing and must not change across re-stores of the same swap. A
// record stored in a terminal state leaves the active set.
func (d *kvDB) StoreIntentsSwap(swap *IntentsSwap) error {
	if swap.DepositAddress == "" {
		return errors.New("no deposit address")
	}
	swap.UpdatedAt = time.Now()
	return d.intentsSwaps.Set([]byte(swap.DepositAddress), swap, kvstore.WithReplace())
}

// IntentsSwap fetches the swap record stored under the deposit address. It
// is ErrIntentsSwapNotFound if no record exists.
func (d *kvDB) IntentsSwap(depositAddress string) (*IntentsSwap, error) {
	b, err := d.intentsSwaps.Get([]byte(depositAddress))
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return nil, ErrIntentsSwapNotFound
		}
		return nil, err
	}
	return decodeIntentsSwap(b)
}

// readIntentsIndex collects the records yielded by an index iteration.
func readIntentsIndex(idx kvstore.StoreIndex, reverse bool) ([]*IntentsSwap, error) {
	var opts []kvstore.IterationOption
	if reverse {
		opts = append(opts, kvstore.WithReverse())
	}
	var swaps []*IntentsSwap
	err := idx.Iterate(nil, func(it kvstore.StoreIter) error {
		return it.V(func(v []byte) error {
			swap, err := decodeIntentsSwap(v)
			if err != nil {
				return err
			}
			swaps = append(swaps, swap)
			return nil
		})
	}, opts...)
	if err != nil {
		return nil, err
	}
	return swaps, nil
}

// IntentsSwaps returns every stored swap record, newest first.
func (d *kvDB) IntentsSwaps() ([]*IntentsSwap, error) {
	return readIntentsIndex(d.intentsStampIdx, true)
}

// ActiveIntentsSwaps returns the records in a non-terminal state, oldest
// first.
func (d *kvDB) ActiveIntentsSwaps() ([]*IntentsSwap, error) {
	return readIntentsIndex(d.intentsActiveIdx, false)
}
