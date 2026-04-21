// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package msgjson

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/bisoncraft/meshwallet/util"
)

// Error codes. Values are fixed by the wire protocol; do not renumber.
const (
	RPCErrorUnspecified   = 0
	RPCParseError         = 1
	RPCUnknownRoute       = 2
	RPCInternal           = 3
	RPCQuarantineClient   = 4
	RPCVersionUnsupported = 5
	RPCUnknownMatch       = 6
	RPCInternalError      = 7
	// 8-45 intentionally skipped — values reserved by wire protocol.
	UnknownMessageType = 46
)

// Route constants used by surviving wallet packages.
const (
	// MatchRoute is the route of a DEX-originating request-type message notifying
	// the client of a match and initiating swap negotiation.
	MatchRoute = "match"
	// InitRoute is the route of a client-originating request-type message
	// notifying the DEX, and subsequently the match counter-party, of the details
	// of a swap contract.
	InitRoute = "init"
	// BookOrderRoute is the DEX-originating notification-type message informing
	// the client to add the order to the order book.
	BookOrderRoute = "book_order"
	// UnbookOrderRoute is the DEX-originating notification-type message informing
	// the client to remove an order from the order book.
	UnbookOrderRoute = "unbook_order"
	// UpdateRemainingRoute is the DEX-originating notification-type message that
	// updates the remaining amount of unfilled quantity on a standing limit order.
	UpdateRemainingRoute = "update_remaining"
)

const errNullRespPayload = util.ErrorKind("null response payload")

type Bytes = util.Bytes

// Signable allows for serialization and signing.
type Signable interface {
	Serialize() []byte
	SetSig([]byte)
	SigBytes() []byte
}

// Signature partially implements Signable, and can be embedded by types intended
// to satisfy Signable, which must themselves implement the Serialize method.
type Signature struct {
	Sig Bytes `json:"sig"`
}

// SetSig sets the Sig field.
func (s *Signature) SetSig(b []byte) {
	s.Sig = b
}

// SigBytes returns the signature as a []byte.
func (s *Signature) SigBytes() []byte {
	return s.Sig
}

// Error is returned as part of the Response to indicate that an error
// occurred during method execution.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error returns the error message. Satisfies the error interface.
func (e *Error) Error() string {
	return e.String()
}

// String satisfies the Stringer interface for pretty printing.
func (e Error) String() string {
	return fmt.Sprintf("error code %d: %s", e.Code, e.Message)
}

// NewError is a constructor for an Error.
func NewError(code int, format string, a ...any) *Error {
	return &Error{
		Code:    code,
		Message: fmt.Sprintf(format, a...),
	}
}

// ResponsePayload is the payload for a Response-type Message.
type ResponsePayload struct {
	// Result is the payload, if successful, else nil.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is the error, or nil if none was encountered.
	Error *Error `json:"error,omitempty"`
}

// MessageType indicates the type of message. MessageType is typically the first
// switch checked when examining a message, and how the rest of the message is
// decoded depends on its MessageType.
type MessageType uint8

// There are presently three recognized message types: request, response, and
// notification.
const (
	InvalidMessageType MessageType = iota // 0
	Request                               // 1
	Response                              // 2
	Notification                          // 3
)

// String satisfies the Stringer interface for translating the MessageType code
// into a description, primarily for logging.
func (mt MessageType) String() string {
	switch mt {
	case Request:
		return "request"
	case Response:
		return "response"
	case Notification:
		return "notification"
	default:
		return "unknown MessageType"
	}
}

// Message is the primary messaging type for websocket communications.
type Message struct {
	// Type is the message type.
	Type MessageType `json:"type"`
	// Route is used for requests and notifications, and specifies a handler for
	// the message.
	Route string `json:"route,omitempty"`
	// ID is a unique number that is used to link a response to a request.
	ID uint64 `json:"id,omitempty"`
	// Payload is any data attached to the message. How Payload is decoded
	// depends on the Route.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Sig is a signature of the message. This is the new-style signature
	// scheme. The old way was to sign individual payloads. Which is used
	// depends on the route.
	Sig util.Bytes `json:"sig"`
}

// DecodeMessage decodes a *Message from JSON-formatted bytes. Note that
// *Message may be nil even if error is nil, when the message is JSON null,
// []byte("null").
func DecodeMessage(b []byte) (msg *Message, _ error) {
	err := json.Unmarshal(b, &msg)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// NewRequest is the constructor for a Request-type *Message.
func NewRequest(id uint64, route string, payload any) (*Message, error) {
	if id == 0 {
		return nil, fmt.Errorf("id = 0 not allowed for a request-type message")
	}
	if route == "" {
		return nil, fmt.Errorf("empty string not allowed for route of request-type message")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &Message{
		Type:    Request,
		Payload: json.RawMessage(encoded),
		Route:   route,
		ID:      id,
	}, nil
}

// NewResponse encodes the result and creates a Response-type *Message.
func NewResponse(id uint64, result any, rpcErr *Error) (*Message, error) {
	if id == 0 {
		return nil, fmt.Errorf("id = 0 not allowed for response-type message")
	}
	encResult, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	resp := &ResponsePayload{
		Result: encResult,
		Error:  rpcErr,
	}
	encResp, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &Message{
		Type:    Response,
		Payload: json.RawMessage(encResp),
		ID:      id,
	}, nil
}

// Response attempts to decode the payload to a *ResponsePayload. Response will
// return an error if the Type is not Response. It is an error if the Message's
// Payload is []byte("null").
func (msg *Message) Response() (*ResponsePayload, error) {
	if msg.Type != Response {
		return nil, fmt.Errorf("invalid type %d for ResponsePayload", msg.Type)
	}
	resp := new(ResponsePayload)
	err := json.Unmarshal(msg.Payload, &resp)
	if err != nil {
		return nil, err
	}
	if resp == nil /* null JSON */ {
		return nil, errNullRespPayload
	}
	return resp, nil
}

// NewNotification encodes the payload and creates a Notification-type *Message.
func NewNotification(route string, payload any) (*Message, error) {
	if route == "" {
		return nil, fmt.Errorf("empty string not allowed for route of notification-type message")
	}
	encPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &Message{
		Type:    Notification,
		Route:   route,
		Payload: json.RawMessage(encPayload),
	}, nil
}

// Unmarshal unmarshals the Payload field into the provided interface. Note that
// the payload interface must contain a pointer. If it is a pointer to a
// pointer, it may become nil for a Message.Payload of []byte("null").
func (msg *Message) Unmarshal(payload any) error {
	return json.Unmarshal(msg.Payload, payload)
}

// UnmarshalResult is a convenience method for decoding the Result field of a
// ResponsePayload.
func (msg *Message) UnmarshalResult(result any) error {
	resp, err := msg.Response()
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("rpc error: %w", resp.Error)
	}
	return json.Unmarshal(resp.Result, result)
}

// String prints the message as a JSON-encoded string.
func (msg *Message) String() string {
	b, err := json.Marshal(msg)
	if err != nil {
		return "[Message decode error]"
	}
	return string(b)
}

// Match is the params for a DEX-originating MatchRoute request.
type Match struct {
	Signature
	OrderID    Bytes  `json:"orderid"`
	MatchID    Bytes  `json:"matchid"`
	Quantity   uint64 `json:"qty"`
	Rate       uint64 `json:"rate"`
	ServerTime uint64 `json:"tserver"`
	// Address carries the counterparty's per-match swap address in the
	// connect response. During initial match notification it carries the
	// order-level address for backward compatibility, but that value is not
	// used for contracts. The actual per-match address is delivered via the
	// counterparty_address notification.
	Address      string `json:"address"`
	FeeRateBase  uint64 `json:"feeratebase"`
	FeeRateQuote uint64 `json:"feeratequote"`
	// Status and Side are provided for convenience and are not part of the
	// match serialization.
	Status uint8 `json:"status"`
	Side   uint8 `json:"side"`
}

var _ Signable = (*Match)(nil)

// Serialize serializes the Match data.
func (m *Match) Serialize() []byte {
	// Match serialization is orderid (32) + matchid (32) + quantity (8) + rate (8)
	// + server time (8) + address (variable, guess 35) + base fee rate (8) +
	// quote fee rate (8) = 139
	s := make([]byte, 0, 139)
	s = append(s, m.OrderID...)
	s = append(s, m.MatchID...)
	s = append(s, uint64Bytes(m.Quantity)...)
	s = append(s, uint64Bytes(m.Rate)...)
	s = append(s, uint64Bytes(m.ServerTime)...)
	s = append(s, []byte(m.Address)...)
	s = append(s, uint64Bytes(m.FeeRateBase)...)
	return append(s, uint64Bytes(m.FeeRateQuote)...)
}

// Init is the payload for a client-originating InitRoute request.
type Init struct {
	Signature
	OrderID  Bytes `json:"orderid"`
	MatchID  Bytes `json:"matchid"`
	CoinID   Bytes `json:"coinid"`
	Contract Bytes `json:"contract"`
}

var _ Signable = (*Init)(nil)

// Serialize serializes the Init data.
func (init *Init) Serialize() []byte {
	// Init serialization is orderid (32) + matchid (32) + coinid (probably 36)
	// + contract (97 ish). Sum = 197
	s := make([]byte, 0, 197)
	s = append(s, init.OrderID...)
	s = append(s, init.MatchID...)
	s = append(s, init.CoinID...)
	return append(s, init.Contract...)
}

// Certain order properties are specified with the following constants.
const (
	BuyOrderNum  = 1
	SellOrderNum = 2
)

// OrderNote is part of a notification about any type of order.
type OrderNote struct {
	Seq      uint64 `json:"seq,omitempty"`      // May be empty when part of an OrderBook.
	MarketID string `json:"marketid,omitempty"` // May be empty when part of an OrderBook.
	OrderID  Bytes  `json:"oid"`
}

// TradeNote is part of a notification that includes information about a
// limit or market order.
type TradeNote struct {
	Side     uint8  `json:"side,omitempty"`
	Quantity uint64 `json:"qty,omitempty"`
	Rate     uint64 `json:"rate,omitempty"`
	TiF      uint8  `json:"tif,omitempty"`
	Time     uint64 `json:"time,omitempty"`
}

// BookOrderNote is the payload for a DEX-originating notification-type message
// informing the client to add the order to the order book.
type BookOrderNote struct {
	OrderNote
	TradeNote
}

// UnbookOrderNote is the DEX-originating notification-type message informing
// the client to remove an order from the order book.
type UnbookOrderNote OrderNote

// EpochOrderNote is the DEX-originating notification-type message informing the
// client about an order added to the epoch queue.
type EpochOrderNote struct {
	BookOrderNote
	Commit    Bytes  `json:"com"`
	OrderType uint8  `json:"otype"`
	Epoch     uint64 `json:"epoch"`
	TargetID  Bytes  `json:"target,omitempty"` // omit for cancel orders
}

// UpdateRemainingNote is the DEX-originating notification-type message
// informing the client about an update to a booked order's remaining quantity.
type UpdateRemainingNote struct {
	OrderNote
	Remaining uint64 `json:"remaining"`
}

// OrderBook is the response to a successful OrderBookSubscription.
type OrderBook struct {
	MarketID string `json:"marketid"`
	Seq      uint64 `json:"seq"`
	Epoch    uint64 `json:"epoch"`
	// DRAFT NOTE: We might want to use a different structure for bulk updates.
	// Sending a struct of arrays rather than an array of structs could
	// potentially cut the encoding effort and encoded size substantially.
	Orders       []*BookOrderNote `json:"orders"`
	BaseFeeRate  uint64           `json:"baseFeeRate"`
	QuoteFeeRate uint64           `json:"quoteFeeRate"`
	// RecentMatches is [rate, qty, timestamp]. Quantity is signed.
	// Negative means that the maker was a sell order.
	RecentMatches [][3]int64 `json:"recentMatches"`
}

// MatchProofNote is the match_proof notification payload.
type MatchProofNote struct {
	MarketID  string  `json:"marketid"`
	Epoch     uint64  `json:"epoch"`
	Preimages []Bytes `json:"preimages"`
	Misses    []Bytes `json:"misses"`
	CSum      Bytes   `json:"csum"`
	Seed      Bytes   `json:"seed"`
}

// Candle is a statistical history of a specified period of market activity.
type Candle struct {
	StartStamp  uint64 `json:"startStamp"`
	EndStamp    uint64 `json:"endStamp"`
	MatchVolume uint64 `json:"matchVolume"`
	QuoteVolume uint64 `json:"quoteVolume"`
	HighRate    uint64 `json:"highRate"`
	LowRate     uint64 `json:"lowRate"`
	StartRate   uint64 `json:"startRate"`
	EndRate     uint64 `json:"endRate"`
}

// EpochReportNote is a report about an epoch sent after all of the epoch's book
// updates. Like TradeResumption, and TradeSuspension when Persist is true, Seq
// is omitted since it doesn't modify the book.
type EpochReportNote struct {
	MarketID     string `json:"marketid"`
	Epoch        uint64 `json:"epoch"`
	BaseFeeRate  uint64 `json:"baseFeeRate"`
	QuoteFeeRate uint64 `json:"quoteFeeRate"`
	// MatchSummary: [rate, quantity]. Quantity is signed. Negative means that
	// the maker was a sell order.
	MatchSummary [][2]int64 `json:"matchSummary"`
	Candle
}

// SnapOrder represents a single standing order in an epoch snapshot.
type SnapOrder struct {
	Rate uint64 `json:"rate"`
	Qty  uint64 `json:"qty"`
}

// MMEpochSnapshot is a server-signed, per-account snapshot of an account's
// standing orders on the book after a match cycle, along with market-wide best
// bid/ask for context.
type MMEpochSnapshot struct {
	Signature
	MarketID   string      `json:"marketid"`
	Base       uint32      `json:"base"`
	Quote      uint32      `json:"quote"`
	EpochIdx   uint64      `json:"epochIdx"`
	EpochDur   uint64      `json:"epochDur"`
	AccountID  Bytes       `json:"accountID"`
	BuyOrders  []SnapOrder `json:"buyOrders"`
	SellOrders []SnapOrder `json:"sellOrders"`
	BestBuy    uint64      `json:"bestBuy"`
	BestSell   uint64      `json:"bestSell"`
}

var _ Signable = (*MMEpochSnapshot)(nil)

// Serialize serializes the MMEpochSnapshot for signing. Format:
// base(4) + quote(4) + epochIdx(8) + epochDur(8) + accountID(32) +
// bestBuy(8) + bestSell(8) + numBuys(2) + [rate(8)+qty(8)]... +
// numSells(2) + [rate(8)+qty(8)]...
// Orders are sorted by rate ascending within each side for deterministic
// output.
func (s *MMEpochSnapshot) Serialize() []byte {
	cmpRate := func(a, b SnapOrder) int {
		if a.Rate < b.Rate {
			return -1
		}
		if a.Rate > b.Rate {
			return 1
		}
		return 0
	}
	buys := slices.SortedFunc(slices.Values(s.BuyOrders), cmpRate)
	sells := slices.SortedFunc(slices.Values(s.SellOrders), cmpRate)
	nBuys := len(buys)
	nSells := len(sells)
	if nBuys > math.MaxUint16 {
		nBuys = math.MaxUint16
		buys = buys[:nBuys]
	}
	if nSells > math.MaxUint16 {
		nSells = math.MaxUint16
		sells = sells[:nSells]
	}
	// 4+4+8+8+32+8+8 + 2+nBuys*16 + 2+nSells*16 = 76 + nBuys*16 + nSells*16
	b := make([]byte, 0, 76+nBuys*16+nSells*16)
	b = append(b, uint32Bytes(s.Base)...)
	b = append(b, uint32Bytes(s.Quote)...)
	b = append(b, uint64Bytes(s.EpochIdx)...)
	b = append(b, uint64Bytes(s.EpochDur)...)
	b = append(b, s.AccountID...)
	b = append(b, uint64Bytes(s.BestBuy)...)
	b = append(b, uint64Bytes(s.BestSell)...)
	b = append(b, uint16Bytes(uint16(nBuys))...)
	for _, o := range buys {
		b = append(b, uint64Bytes(o.Rate)...)
		b = append(b, uint64Bytes(o.Qty)...)
	}
	b = append(b, uint16Bytes(uint16(nSells))...)
	for _, o := range sells {
		b = append(b, uint64Bytes(o.Rate)...)
		b = append(b, uint64Bytes(o.Qty)...)
	}
	return b
}

// Market describes a market and its variables, as used by wallet/orderbook for
// time-based running checks.
type MarketStatus struct {
	StartEpoch uint64 `json:"startepoch"`
	FinalEpoch uint64 `json:"finalepoch,omitempty"`
	Persist    *bool  `json:"persistbook,omitempty"` // nil and omitted when finalepoch is omitted
}

// Market describes a market, returned as part of a ConfigResult.
type Market struct {
	Name            string  `json:"name"`
	Base            uint32  `json:"base"`
	Quote           uint32  `json:"quote"`
	EpochLen        uint64  `json:"epochlen"`
	LotSize         uint64  `json:"lotsize"`
	RateStep        uint64  `json:"ratestep"`
	MarketBuyBuffer float64 `json:"buybuffer"`
	ParcelSize      uint32  `json:"parcelSize"`
	MarketStatus    `json:"status"`
}

// Running indicates if the market should be running given the known StartEpoch,
// EpochLen, and FinalEpoch (if set).
func (m *Market) Running() bool {
	dur := m.EpochLen
	now := uint64(time.Now().UnixMilli())
	start := m.StartEpoch * dur
	end := m.FinalEpoch * dur
	return now >= start && (now < end || end < start) // end < start detects obsolete end
}

// Convert uint64 to 8 bytes.
func uint64Bytes(i uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, i)
	return b
}

// Convert uint32 to 4 bytes.
func uint32Bytes(i uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, i)
	return b
}

// Convert uint16 to 2 bytes.
func uint16Bytes(i uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, i)
	return b
}
