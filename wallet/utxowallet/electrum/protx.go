// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// A Dash ElectrumX serves `protx.info`, which proxies dashd's `protx info`: the
// state of one masternode registration. Whatever it returns is the server's
// word, and the caller must not rely on it for anything the chain can prove.

var (
	// ErrProTxUnsupported is returned when the server does not know the protx
	// methods, which the stock ElectrumX does not for any coin but Dash. It is
	// remembered for the life of the Node.
	ErrProTxUnsupported = errors.New("electrum: server does not support protx")

	// ErrProTxNotFound is returned when the server has no such registration. It
	// is ambiguous: a masternode whose collateral was spent, such as a
	// dissolved shared one, is reported the same way as one that never
	// existed, so it must never be read as a dissolution.
	ErrProTxNotFound = errors.New("electrum: protx not found")
)

// errCodeMethodNotFound is the JSON-RPC code of an unknown method.
const errCodeMethodNotFound = -32601

// ProTxShare is a share of a shared masternode as the server reports it.
type ProTxShare struct {
	// Amount is the share's collateral in duffs.
	Amount       int64  `json:"amount"`
	RefundScript string `json:"refundScript"`
	// RewardScript is where the share's rewards are paid now.
	RewardScript string `json:"rewardScript"`
	OwnerAddress string `json:"ownerAddress"`
}

// ProTxState is the state of a registration as the server reports it. The
// height fields are -1 when they do not apply.
type ProTxState struct {
	Version           uint16 `json:"version"`
	RegisteredHeight  int32  `json:"registeredHeight"`
	LastPaidHeight    int32  `json:"lastPaidHeight"`
	PoSePenalty       int32  `json:"PoSePenalty"`
	PoSeRevivedHeight int32  `json:"PoSeRevivedHeight"`
	PoSeBanHeight     int32  `json:"PoSeBanHeight"`
	RevocationReason  int32  `json:"revocationReason"`
	VotingAddress     string `json:"votingAddress"`
	// PubKeyOperator is the operator's BLS key in hex.
	PubKeyOperator string `json:"pubKeyOperator"`
	// Shares, EarlyPeriodBlocks and EarlyPenalty are set for a shared
	// masternode only.
	Shares            []ProTxShare `json:"shares"`
	EarlyPeriodBlocks uint32       `json:"earlyPeriodBlocks"`
	EarlyPenalty      int64        `json:"earlyPenalty"`
}

// ProTxInfo is the result of `protx.info`.
type ProTxInfo struct {
	Type            string     `json:"type"`
	ProTxHash       string     `json:"proTxHash"`
	CollateralHash  string     `json:"collateralHash"`
	CollateralIndex uint32     `json:"collateralIndex"`
	State           ProTxState `json:"state"`
	Confirmations   int32      `json:"confirmations"`
}

// ProTxInfo issues `protx.info` for the registration with the given
// ProRegTx hash. It returns ErrProTxUnsupported if the server does not have the
// method and ErrProTxNotFound if it has no such registration.
func (n *Node) ProTxInfo(ctx context.Context, proTxHash chainhash.Hash) (*ProTxInfo, error) {
	if n.protxUnsupported.Load() {
		return nil, ErrProTxUnsupported
	}
	var out ProTxInfo
	err := n.server.request(ctx, "protx.info", positional{proTxHash.String()}, &out)
	var rpcErr *RPCError
	switch {
	case err == nil:
		// Only this layer knows which registration was asked for. A null
		// result decodes to an empty one and a reply about another registration
		// is well formed, and neither may reach a caller as this one's state.
		if out.ProTxHash != proTxHash.String() {
			return nil, fmt.Errorf("electrum: protx.info for %s returned %q", proTxHash, out.ProTxHash)
		}
		return &out, nil
	case errors.As(err, &rpcErr) && rpcErr.Code == errCodeMethodNotFound:
		n.protxUnsupported.Store(true)
		return nil, ErrProTxUnsupported
	case errors.As(err, &rpcErr) && rpcErr.Code == 2 && strings.Contains(rpcErr.Message, "not found"):
		return nil, ErrProTxNotFound
	}
	return nil, err
}

// ProTxInfo issues `protx.info` on the current server. See Node.ProTxInfo.
func (n *Network) ProTxInfo(ctx context.Context, proTxHash chainhash.Hash) (*ProTxInfo, error) {
	node, err := n.getNode()
	if err != nil {
		return nil, err
	}
	return node.ProTxInfo(ctx, proTxHash)
}

// MerkleRoot folds the merkle branch of a proof, with the transaction at the
// position the proof gives, to the root it commits to. The caller compares the
// root with the header of the block the proof claims, which is what makes the
// proof an inclusion proof.
//
// The caller must also hold the transaction's raw bytes and refuse one of 64
// bytes, which can be an interior node of the tree rather than a transaction.
func MerkleRoot(txid chainhash.Hash, p *MerkleProof) (chainhash.Hash, error) {
	// A position with bits beyond the branch is not a position in a tree of
	// that depth, and folding would silently ignore them.
	if p.Pos>>len(p.Merkle) != 0 {
		return chainhash.Hash{}, fmt.Errorf("electrum: merkle position %d is beyond a branch of %d", p.Pos, len(p.Merkle))
	}
	cur := txid
	pos := p.Pos
	for _, sibHex := range p.Merkle {
		sib, err := chainhash.NewHashFromStr(sibHex)
		if err != nil {
			return chainhash.Hash{}, fmt.Errorf("electrum: bad merkle sibling: %w", err)
		}
		var cat [64]byte
		if pos&1 == 1 {
			copy(cat[:32], sib[:])
			copy(cat[32:], cur[:])
		} else {
			copy(cat[:32], cur[:])
			copy(cat[32:], sib[:])
		}
		cur = chainhash.DoubleHashH(cat[:])
		pos >>= 1
	}
	return cur, nil
}
