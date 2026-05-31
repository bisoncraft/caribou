// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package txdb

import (
	"github.com/bisoncraft/bosswallet/util"
)

// TransactionType is the type of transaction made by a wallet.
type TransactionType uint16

const (
	Unknown TransactionType = iota
	Send
	Receive
	Swap
	Redeem
	Refund
	Split
	CreateBond
	RedeemBond
	ApproveToken
	Acceleration
	SelfSend
	RevokeTokenApproval
	TicketPurchase
	TicketVote
	TicketRevocation
	// SwapOrSend is used when a wallet scanned its historical transactions
	// and was unable to determine if the transaction was a swap or a send.
	SwapOrSend
	Mix
	InitiateBridge
	CompleteBridge
	DeployContract
)

// IncomingTxType returns true if the wallet's balance increases due to a
// transaction.
func IncomingTxType(txType TransactionType) bool {
	return txType == Receive || txType == Redeem || txType == Refund || txType == RedeemBond
}

// BondTxInfo contains information about a CreateBond or RedeemBond
// transaction.
type BondTxInfo struct {
	// AccountID is the account is the account ID that the bond is applied to.
	AccountID util.Bytes `json:"accountID"`
	// LockTime is the time until which the bond is locked.
	LockTime uint64 `json:"lockTime"`
	// BondID is the ID of the bond.
	BondID util.Bytes `json:"bondID"`
}

// BridgeCounterpartTx identifies an initiation transaction if it is part of a
// completion transaction, or a completion transaction if it is part of an
// initiation transaction.
type BridgeCounterpartTx struct {
	AssetID  uint32   `json:"assetID"`
	IDs      []string `json:"ids"`
	Complete bool     `json:"complete"`
	// AmountReceived is the amount of the asset that was minted
	// on the destination chain.
	AmountReceived uint64 `json:"amountReceived"`
	// Fees are the network fees paid on the destination chain.
	Fees uint64 `json:"fees"`
}

type Confirms struct {
	Current uint32 `json:"current"`
	Target  uint32 `json:"target"`
}

// WalletTransaction represents a transaction that was made by a wallet.
type WalletTransaction struct {
	Type   TransactionType `json:"type"`
	ID     string          `json:"id"`
	Amount uint64          `json:"amount"`
	Fees   uint64          `json:"fees"`
	// BlockNumber is 0 for txs in the mempool.
	BlockNumber uint64 `json:"blockNumber"`
	// Timestamp is the time the transaction was mined.
	Timestamp uint64 `json:"timestamp"`
	// TokenID will be non-nil if the BalanceDelta applies to the balance
	// of a token.
	TokenID *uint32 `json:"tokenID,omitempty"`
	// Recipient will be non-nil for Send/Receive transactions, and specifies the
	// recipient address of the transaction.
	Recipient *string `json:"recipient,omitempty"`
	// BondInfo will be non-nil for CreateBond and RedeemBond transactions.
	BondInfo *BondTxInfo `json:"bondInfo,omitempty"`
	// AdditionalData contains asset specific information, i.e. nonce
	// for ETH.
	AdditionalData map[string]string `json:"additionalData"`
	// Confirmed is true when the transaction is considered finalized.
	// Confirmed transactions are no longer updated and will be considered
	// finalized forever.
	Confirmed bool `json:"confirmed"`
	// Confirms is the number of current confirmations and the number of
	// confirmations required for finalization.
	Confirms *Confirms `json:"confirms"`
	// Rejected will be true the transaction was rejected and did not have any
	// effect, though fees were incurred.
	Rejected bool `json:"rejected,omitempty"`
	// BridgeCounterpartTx is only populated for Bridge related transactions.
	// It contains the ID and asset ID of the transaction that either initiated
	// the bridge or completed it.
	BridgeCounterpartTx *BridgeCounterpartTx `json:"bridgeCounterpartTx,omitempty"`
	// BridgeName is the name of the bridge. It is only populated for bridge
	// related transactions.
	BridgeName string `json:"bridgeName,omitempty"`
	// RelayTxID is the id of the on-chain transaction submitted by the
	// relay for a gasless redemption.
	RelayTxID string `json:"relayTxID,omitempty"`
	// IsRelay will be true if the transaction is a relay redemption.
	IsRelay bool `json:"isRelay"`
	// RawTx is an opaque, network-specific encoding of the transaction managed
	// by the wallet that stored it. The asset-agnostic consumer (Core/UI) never
	// reads it, so it is excluded from JSON; it is persisted separately by the
	// transaction database. A wallet typically encodes its extended transaction
	// representation (nonce, receipt, raw bytes, bridge state, etc.) here.
	RawTx []byte `json:"-"`
}

// TxHistoryRequest is a request for transaction history.
type TxHistoryRequest struct {
	N           int               `json:"n"`
	RefID       *string           `json:"refID"`
	Past        bool              `json:"past"`
	IgnoreTypes []TransactionType `json:"ignoreTypes"`
}

func (r *TxHistoryRequest) IngoreTypesLookup() map[TransactionType]bool {
	m := make(map[TransactionType]bool)
	for _, txType := range r.IgnoreTypes {
		m[txType] = true
	}
	return m
}

// TxHistoryResponse is a paginated response of transaction history.
type TxHistoryResponse struct {
	Txs           []*WalletTransaction `json:"txs"`
	MoreAvailable bool                 `json:"moreAvailable"`
}
