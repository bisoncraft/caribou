// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
)

// A shared masternode's state comes from two places that are trusted
// differently. Its registration is a mined transaction, which the caller proves
// with a merkle proof against a checked header: the share table, the early
// terms and the collateral come only from it. What changes afterwards (the
// reward scripts, the voting and operator keys, PoSe state) is only known from
// the server, whose word is checked against the registration wherever the
// registration can contradict it.

var (
	// ErrInvalidRegistration is wrapped by errors for a transaction that is
	// not a shared masternode registration this wallet can track.
	ErrInvalidRegistration = errors.New("not a valid shared masternode registration")

	// ErrInfoMismatch is wrapped by errors for a server reply that
	// contradicts the registration, or that is malformed. The caller should
	// distrust the server. A reply for a registration at another height is
	// also reported this way, which a reorg can cause.
	ErrInfoMismatch = errors.New("server masternode info contradicts the registration")
)

func badReg(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRegistration, fmt.Sprintf(format, args...))
}

func badInfo(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInfoMismatch, fmt.Sprintf(format, args...))
}

// RegistrationMN returns the shared masternode that the mined registration tx
// created, as the registration states it. height is the height of the block that
// holds tx, which the caller has proven. A share with no reward script is
// paid at its refund script, so every share's RewardScript is where its rewards
// go.
//
// It checks what this wallet's own arithmetic depends on, not everything Dash
// Core checked: Core accepted the transaction, and a check stricter than
// Core's would lose a masternode the wallet owns.
func RegistrationMN(tx *bisonwire.Tx, height int32) (*SharedMN, error) {
	if height <= 0 {
		return nil, badReg("height %d", height)
	}
	if tx.Type != txTypeProReg || tx.MsgTx.Version != txVersion {
		return nil, badReg("not a provider registration transaction")
	}
	var pr protx.ProRegTx
	if err := protx.Parse(tx.ExtraPayload, &pr); err != nil {
		return nil, fmt.Errorf("%w: payload: %w", ErrInvalidRegistration, err)
	}
	if pr.Version != protx.VersionExtAddr || pr.MNType != protx.TypeRegular {
		return nil, badReg("not a shared registration of a regular masternode")
	}
	if pr.Collateral.Hash != ([32]byte{}) || pr.KeyIDOwner != ([protx.KeyIDSize]byte{}) ||
		len(pr.Payouts) != 0 || len(pr.PayloadSig) != 0 {
		return nil, badReg("fields a shared registration leaves empty are set")
	}
	ci := pr.Collateral.Index
	if uint64(ci) >= uint64(len(tx.TxOut)) {
		return nil, badReg("collateral index %d of %d outputs", ci, len(tx.TxOut))
	}
	if tx.TxOut[ci].Value != CollateralAmount || !bytes.Equal(tx.TxOut[ci].PkScript, SharedCollateralScript) {
		return nil, badReg("output %d is not the collateral", ci)
	}

	// The dissolution and update arithmetic divides by these totals and
	// indexes the table.
	if n := len(pr.Shares); n < MinShares || n > MaxShares {
		return nil, badReg("%d shares", n)
	}
	var sum int64
	for i := range pr.Shares {
		a := pr.Shares[i].Amount
		// An amount above the total is refused so that the sum cannot wrap.
		if a <= 0 || a > CollateralAmount {
			return nil, badReg("share %d amount %d", i, a)
		}
		sum += a
	}
	if sum != CollateralAmount {
		return nil, badReg("shares total %d, want %d", sum, int64(CollateralAmount))
	}
	if pr.EarlyPenalty < 0 {
		return nil, badReg("early penalty %d", pr.EarlyPenalty)
	}

	shares := make([]protx.Share, len(pr.Shares))
	for i, s := range pr.Shares {
		s.RefundScript = slices.Clone(s.RefundScript)
		s.RewardScript = slices.Clone(s.RewardScript)
		if len(s.RewardScript) == 0 {
			s.RewardScript = slices.Clone(s.RefundScript)
		}
		shares[i] = s
	}
	hash := tx.TxHash()
	m := &SharedMN{
		ProTxHash:         hash,
		Shares:            shares,
		RegisteredHeight:  height,
		EarlyPeriodBlocks: pr.EarlyPeriodBlocks,
		EarlyPenalty:      pr.EarlyPenalty,
		PubKeyOperator:    pr.PubKeyOperator,
		KeyIDVoting:       pr.KeyIDVoting,
	}
	m.Collateral.Hash = hash
	m.Collateral.Index = pr.Collateral.Index
	return m, nil
}

// SpendsCollateral reports whether tx has an input that spends the collateral,
// which dissolves the masternode once tx is mined.
func (m *SharedMN) SpendsCollateral(tx *bisonwire.Tx) bool {
	for _, in := range tx.TxIn {
		if in.PreviousOutPoint == m.Collateral {
			return true
		}
	}
	return false
}

// keyIDOf decodes a P2PKH address of net and returns its key id.
func keyIDOf(addr string, net *chaincfg.Params) ([protx.KeyIDSize]byte, error) {
	var id [protx.KeyIDSize]byte
	a, err := btcutil.DecodeAddress(addr, net)
	if err != nil {
		return id, err
	}
	p, ok := a.(*btcutil.AddressPubKeyHash)
	// DecodeAddress refuses another network's version byte.
	if !ok {
		return id, errors.New("not a pay to key hash address")
	}
	copy(id[:], p.Hash160()[:])
	return id, nil
}

// ApplyInfo updates m from the server's reply for it: the reward scripts, the
// voting key and the operator key. Everything the registration fixes must match
// it or the error wraps ErrInfoMismatch. net is the network the addresses in
// info are for. It changes nothing on error, and never writes into the Shares
// array it replaces, so a builder holding it is not affected.
//
// The reward scripts, voting key and operator key are the server's word, not the
// chain's, and after ApplyInfo m holds them where RegistrationMN put the
// registered ones. A caller that signs an update must not take them as
// verified: a key to keep must be one the owner confirmed, not one read from m.
//
// The operator key is taken as 48 bytes without checking that it is a point,
// because a revoked masternode's key can be all zeros. The reward scripts and
// voting key are checked as Core checks them where the check is local, but not
// against each other: that data is the server's, and refusing it would only
// hide the masternode.
func (m *SharedMN) ApplyInfo(info *electrum.ProTxInfo, net *chaincfg.Params) error {
	if info == nil {
		return badInfo("no info")
	}
	st := &info.State
	if info.ProTxHash != m.ProTxHash.String() {
		return badInfo("info is for %q", info.ProTxHash)
	}
	if info.CollateralHash != m.Collateral.Hash.String() || info.CollateralIndex != m.Collateral.Index {
		return badInfo("collateral %s:%d", info.CollateralHash, info.CollateralIndex)
	}
	// The state version is not checked: Core can move it after an update, and
	// the type and the share table already pin the registration.
	if info.Type != "Regular" {
		return badInfo("type %q", info.Type)
	}
	if st.RegisteredHeight != m.RegisteredHeight {
		return badInfo("registered at %d, not %d", st.RegisteredHeight, m.RegisteredHeight)
	}
	if st.EarlyPeriodBlocks != m.EarlyPeriodBlocks || st.EarlyPenalty != m.EarlyPenalty {
		return badInfo("early terms %d, %d", st.EarlyPeriodBlocks, st.EarlyPenalty)
	}
	if len(st.Shares) != len(m.Shares) {
		return badInfo("%d shares, not %d", len(st.Shares), len(m.Shares))
	}
	shares := make([]protx.Share, len(m.Shares))
	for i, want := range m.Shares {
		got := st.Shares[i]
		if got.Amount != want.Amount {
			return badInfo("share %d amount %d", i, got.Amount)
		}
		refund, err := hex.DecodeString(got.RefundScript)
		if err != nil || !bytes.Equal(refund, want.RefundScript) {
			return badInfo("share %d refund script", i)
		}
		owner, err := keyIDOf(got.OwnerAddress, net)
		if err != nil || owner != want.KeyIDOwner {
			return badInfo("share %d owner address", i)
		}
		reward, err := hex.DecodeString(got.RewardScript)
		if err != nil || !payableScript(reward) {
			return badInfo("share %d reward script", i)
		}
		s := want
		s.RefundScript = slices.Clone(want.RefundScript)
		s.RewardScript = reward
		shares[i] = s
	}
	voting, err := keyIDOf(st.VotingAddress, net)
	if err != nil || voting == ([protx.KeyIDSize]byte{}) {
		return badInfo("voting address %q", st.VotingAddress)
	}
	var op [protx.OperatorPubKeySize]byte
	b, err := hex.DecodeString(st.PubKeyOperator)
	if err != nil || len(b) != len(op) {
		return badInfo("operator key")
	}
	copy(op[:], b)

	m.Shares = shares
	m.KeyIDVoting = voting
	m.PubKeyOperator = op
	return nil
}

// State is what is known of a masternode's standing.
type State uint8

const (
	// StateUnknown is for a masternode whose server has no info. That may
	// be a lagging server or a hidden spend, and is never read as
	// dissolved.
	StateUnknown State = iota
	// StateActive is registered and not PoSe banned.
	StateActive
	// StateBanned is PoSe banned or revoked, as the server reports.
	StateBanned
	// StateDissolved is a masternode whose collateral has been spent.
	StateDissolved
)

func (s State) String() string {
	switch s {
	case StateActive:
		return "active"
	case StateBanned:
		return "banned"
	case StateDissolved:
		return "dissolved"
	}
	return "unknown"
}

// DeriveState is the state of a masternode from the server's info, which is nil
// if it has none, and whether the collateral has been spent. dissolved must
// mean that a mined transaction, proven by a merkle proof, spends it (see
// SpendsCollateral): a spend in the mempool does not count. A dissolution
// overrides the info, because a server may still report a dissolved
// masternode. info must be a reply that ApplyInfo accepted.
func DeriveState(info *electrum.ProTxInfo, dissolved bool) State {
	switch {
	case dissolved:
		return StateDissolved
	case info == nil:
		return StateUnknown
	case info.State.PoSeBanHeight != -1:
		return StateBanned
	}
	return StateActive
}
