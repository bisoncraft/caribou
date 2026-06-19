// Copyright (c) 2024 The Bison Wallet developers
// Copyright (c) The go-electrum-client authors
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"fmt"
	"net"
)

// ServerAddr identifies an electrumx server endpoint.
type ServerAddr struct {
	// Proto is "ssl" or "tcp".
	Proto string
	// Addr is the host:port endpoint.
	Addr string
	// Onion is true if Addr is a Tor hidden service.
	Onion bool
}

// Network satisfies net.Addr.
func (a *ServerAddr) Network() string { return a.Proto }

// String satisfies net.Addr.
func (a *ServerAddr) String() string { return a.Addr }

// Equal reports whether two server addresses are the same.
func (a *ServerAddr) Equal(b *ServerAddr) bool {
	return a.Proto == b.Proto && a.Addr == b.Addr
}

// Validate sanity-checks a ServerAddr.
func (a *ServerAddr) Validate() error {
	switch a.Proto {
	case "ssl", "tcp":
	default:
		return fmt.Errorf("electrum: unknown proto %q (want ssl or tcp)", a.Proto)
	}
	if _, _, err := net.SplitHostPort(a.Addr); err != nil {
		return fmt.Errorf("electrum: invalid addr %q: %w", a.Addr, err)
	}
	return nil
}

// ScripthashStatus is a notification or response for a scripthash subscription.
// Status is a 32-byte SHA-256 hex digest of the address history; empty when
// the address has no history.
type ScripthashStatus struct {
	Scripthash string
	Status     string
}

// HeadersTip is the payload of a `blockchain.headers.subscribe` response or
// notification.
type HeadersTip struct {
	Height int32  `json:"height"`
	Hex    string `json:"hex"`
}

// DisconnectedBlock captures a header that has been rolled back from the
// chain due to a reorg rewind. The header itself is no longer in the headers
// store by the time consumers see it, so the metadata is included inline.
type DisconnectedBlock struct {
	Hash   [32]byte
	Height int32
	Time   int64 // unix seconds
}

// TipChange is the structured event Network publishes when its header chain
// advances or rewinds. Consumers translate it into chain.Backend
// notifications: BlockDisconnected for each Disconnected entry (in order,
// which is newest-first / disconnect order) and BlockConnected for each
// Connected height (oldest-first / connect order).
//
// Either or both slices may be populated in a single event:
//   - Pure advance: Connected populated, Disconnected empty.
//   - Pure rewind: Disconnected populated, Connected empty.
//   - Reorg (rewind + replacement): both populated. The wallet processes
//     Disconnected first so its syncedTo is on the common ancestor before
//     the new connects are recorded.
type TipChange struct {
	Disconnected []DisconnectedBlock
	Connected    []int32 // heights newly appended; queryable via HeaderByHeight
}

// HistoryEntry is one element of a `blockchain.scripthash.get_history` reply.
type HistoryEntry struct {
	Height int32  `json:"height"`
	TxHash string `json:"tx_hash"`
	Fee    int64  `json:"fee,omitempty"` // satoshis; mempool-only
}

// UnspentEntry is one element of a `blockchain.scripthash.listunspent` reply.
type UnspentEntry struct {
	Height int32  `json:"height"`
	TxPos  int32  `json:"tx_pos"`
	TxHash string `json:"tx_hash"`
	Value  int64  `json:"value"` // satoshis
}

// TxOutScript represents a transaction output's scriptPubKey as returned by
// `blockchain.transaction.get` in verbose mode.
type TxOutScript struct {
	Asm       string   `json:"asm"`
	Hex       string   `json:"hex"`
	ReqSigs   uint32   `json:"reqsigs"`
	Type      string   `json:"type"`
	Addresses []string `json:"addresses,omitempty"`
}

// TxOut represents a transaction output as returned in verbose mode.
type TxOut struct {
	Value    float64     `json:"value"`
	N        uint32      `json:"n"`
	PkScript TxOutScript `json:"scriptpubkey"`
}

// TxInScript represents a transaction input's scriptSig as returned in verbose
// mode.
type TxInScript struct {
	Asm string `json:"asm"`
	Hex string `json:"hex"`
}

// TxIn represents a transaction input as returned in verbose mode.
type TxIn struct {
	TxID      string      `json:"txid"`
	Vout      uint32      `json:"vout"`
	SigScript *TxInScript `json:"scriptsig"`
	Witness   []string    `json:"txinwitness,omitempty"`
	Sequence  uint32      `json:"sequence"`
	Coinbase  string      `json:"coinbase,omitempty"`
}

// VerboseTx is the verbose `blockchain.transaction.get` result. Some servers
// (notably Blockstream's) do not support verbose mode; callers should fall
// back to the raw hex form returned by GetRawTransaction.
type VerboseTx struct {
	TxID          string  `json:"txid"`
	Version       uint32  `json:"version"`
	Size          uint32  `json:"size"`
	VSize         uint32  `json:"vsize"`
	Weight        uint32  `json:"weight"`
	LockTime      uint32  `json:"locktime"`
	Hex           string  `json:"hex"`
	Vin           []TxIn  `json:"vin"`
	Vout          []TxOut `json:"vout"`
	BlockHash     string  `json:"blockhash,omitempty"`
	Confirmations int32   `json:"confirmations,omitempty"`
	Time          int64   `json:"time,omitempty"`
	BlockTime     int64   `json:"blocktime,omitempty"`
}

// ServerFeatures is a trimmed view of the `server.features` result. Only the
// fields we actually use during connection setup are decoded.
type ServerFeatures struct {
	GenesisHash  string `json:"genesis_hash"`
	ProtocolMin  string `json:"protocol_min"`
	ProtocolMax  string `json:"protocol_max"`
	Version      string `json:"server_version"`
	HashFunction string `json:"hash_function"`
}
