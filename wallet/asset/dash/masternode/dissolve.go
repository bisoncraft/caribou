// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
)

// A dissolution (ProDisTx, type 10) spends a shared masternode's collateral and
// returns each share's principal to its refund script. It has one input, the
// collateral, spent with an empty signature script, and the signatures are in
// the payload.
//
// A dissolution is either unilateral, signed by one share owner (the actor),
// or unanimous, signed by every share owner. During the early period a
// unilateral one must pay the early penalty to the other shares. Dash Core
// accepts it from any height after the early period, so one signed in advance
// (a standby) becomes valid on its own.

const (
	// MaxDissolutionFee is the most a dissolution may leave as fee, in duffs,
	// taken from the actor's share.
	MaxDissolutionFee = 1_000_000
	txTypeProDis      = 10
)

// ErrInvalidDissolution is wrapped by errors for a dissolution that Dash Core
// would reject or that this wallet must not sign.
var ErrInvalidDissolution = fmt.Errorf("invalid shared masternode dissolution")

func badDis(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidDissolution, fmt.Sprintf(format, args...))
}

// SharedMN is the on-chain state of a shared masternode that a dissolution is
// built from. Only the amount, refund script and owner key id of each share are
// used.
type SharedMN struct {
	ProTxHash         chainhash.Hash
	Collateral        wire.OutPoint
	Shares            []protx.Share
	RegisteredHeight  int32
	EarlyPeriodBlocks uint32
	EarlyPenalty      int64
}

// early reports whether a transaction mined at spendHeight is in the early
// period.
func (m *SharedMN) early(spendHeight int32) bool {
	return int64(spendHeight)-int64(m.RegisteredHeight) < int64(m.EarlyPeriodBlocks)
}

// RequiredPenalty is the penalty a dissolution mined at spendHeight must pay to
// the other shares: the early penalty for a unilateral dissolution during the
// early period, and none otherwise. It never increases with the height.
func (m *SharedMN) RequiredPenalty(spendHeight int32, unanimous bool) int64 {
	if unanimous || !m.early(spendHeight) {
		return 0
	}
	return m.EarlyPenalty
}

// StandbyValidFrom is the first height at which a unilateral dissolution that
// pays no penalty is valid.
func (m *SharedMN) StandbyValidFrom() int32 {
	return int32(min(int64(m.RegisteredHeight)+int64(m.EarlyPeriodBlocks), math.MaxInt32))
}

func (m *SharedMN) total() int64 {
	var t int64
	for i := range m.Shares {
		t += m.Shares[i].Amount
	}
	return t
}

// nonActorTotal is the collateral of every share but actor.
func (m *SharedMN) nonActorTotal(actor int) int64 {
	return m.total() - m.Shares[actor].Amount
}

// proRataFloor is floor(total*weight/weightTotal), as Dash Core computes it.
func proRataFloor(total, weight, weightTotal int64) int64 {
	v := new(big.Int).Mul(big.NewInt(total), big.NewInt(weight))
	return v.Div(v, big.NewInt(weightTotal)).Int64()
}

// Preview is what a dissolution pays.
type Preview struct {
	// Outputs holds, for each share, what it receives. The actor's is its
	// principal less the penalty and the fee.
	Outputs []int64
	// Penalty is what the actor pays to the other shares.
	Penalty int64
}

// DissolveParams chooses a dissolution.
type DissolveParams struct {
	// Actor is the share that dissolves and pays the fee.
	Actor int
	// Unanimous makes every share owner sign, with no penalty. Otherwise
	// only the actor signs.
	Unanimous bool
	// Penalty is what the actor pays the other shares. It is at most the
	// early penalty, and 0 when Unanimous. For a dissolution to be mined
	// now it must be at least RequiredPenalty at the height it is mined; a
	// standby uses 0 and is valid from StandbyValidFrom.
	Penalty int64
	// Fee is taken from the actor's share, at most MaxDissolutionFee.
	Fee int64
}

// Preview returns what the dissolution pays each share, so a user can see the
// penalty and fee before signing.
func (m *SharedMN) Preview(p DissolveParams) (Preview, error) {
	outs, err := m.outputs(p)
	if err != nil {
		return Preview{}, err
	}
	pv := Preview{Outputs: make([]int64, len(m.Shares)), Penalty: p.Penalty}
	// The outputs are the other shares' in share order, then the actor's if
	// it has anything left.
	k := 0
	for i := range m.Shares {
		if i != p.Actor {
			pv.Outputs[i] = outs[k].Value
			k++
		}
	}
	if k < len(outs) {
		pv.Outputs[p.Actor] = outs[k].Value
	}
	return pv, nil
}

// outputs returns the dissolution's outputs: one per share but the actor in
// share order, then the actor's if it is not empty.
func (m *SharedMN) outputs(p DissolveParams) ([]*wire.TxOut, error) {
	n := len(m.Shares)
	if n < MinShares || n > MaxShares {
		return nil, badDis("%d shares", n)
	}
	if p.Actor < 0 || p.Actor >= n {
		return nil, badDis("actor %d of %d shares", p.Actor, n)
	}
	if p.Fee < 0 || p.Fee > MaxDissolutionFee {
		return nil, badDis("fee %d", p.Fee)
	}
	if p.Penalty < 0 || p.Penalty > m.EarlyPenalty || (p.Unanimous && p.Penalty != 0) {
		return nil, badDis("penalty %d", p.Penalty)
	}

	w := m.nonActorTotal(p.Actor)
	var outs []*wire.TxOut
	var paid, bonused int64
	last := -1
	for i := range m.Shares {
		if i == p.Actor {
			continue
		}
		bonus := proRataFloor(p.Penalty, m.Shares[i].Amount, w)
		bonused += bonus
		paid += m.Shares[i].Amount + bonus
		outs = append(outs, wire.NewTxOut(m.Shares[i].Amount+bonus, m.Shares[i].RefundScript))
		last = len(outs) - 1
	}
	// The floors drop remainders under a duff; Dash Core requires the bonuses
	// to add up to the penalty, so the last share takes the rest.
	rest := p.Penalty - bonused
	outs[last].Value += rest
	paid += rest

	actorOut := m.total() - paid - p.Fee
	switch {
	case actorOut < 0:
		return nil, badDis("the actor's share cannot cover the penalty and fee")
	case actorOut == 0:
	case actorOut < dustLimit:
		return nil, badDis("the actor's output %d is dust", actorOut)
	default:
		outs = append(outs, wire.NewTxOut(actorOut, m.Shares[p.Actor].RefundScript))
	}
	return outs, nil
}

// Dissolution is a dissolution being signed.
type Dissolution struct {
	mn *SharedMN
	tx *bisonwire.Tx
	pd protx.ProDisTx
}

// NewDissolution builds the unsigned dissolution: a transaction with the
// collateral as its one input and the payout outputs, and a payload with empty
// signature slots.
func NewDissolution(m *SharedMN, p DissolveParams) (*Dissolution, error) {
	outs, err := m.outputs(p)
	if err != nil {
		return nil, err
	}
	msg := wire.NewMsgTx(txVersion)
	in := wire.NewTxIn(&m.Collateral, nil, nil)
	in.Sequence = finalSequence
	msg.AddTxIn(in)
	for _, o := range outs {
		msg.AddTxOut(o)
	}
	slots := 1
	if p.Unanimous {
		slots = len(m.Shares)
	}
	d := &Dissolution{
		mn: m,
		tx: &bisonwire.Tx{Chain: "dash", MsgTx: *msg, Type: txTypeProDis},
		pd: protx.ProDisTx{
			Version:    protx.SharedVersion,
			ProTxHash:  m.ProTxHash,
			ActorIndex: uint16(p.Actor),
			Sigs:       make([][protx.CompactSigSize]byte, slots),
		},
	}
	if err := d.sync(); err != nil {
		return nil, err
	}
	// Whatever the penalty, the dissolution must be valid from some height on.
	if err := d.CheckAt(math.MaxInt32); err != nil {
		return nil, err
	}
	return d, nil
}

// sync writes the payload into the transaction.
func (d *Dissolution) sync() error {
	var b bytes.Buffer
	if err := d.pd.Serialize(&b); err != nil {
		return err
	}
	d.tx.ExtraPayload = b.Bytes()
	return nil
}

// FeeFor returns the fee for a dissolution of m at feeRate duffs per kilobyte,
// rounded up and capped at MaxDissolutionFee. The size is exact: the input has
// no signature script and the signatures have a fixed size.
func FeeFor(m *SharedMN, unanimous bool, feeRate int64) (int64, error) {
	d, err := NewDissolution(m, DissolveParams{Actor: 0, Unanimous: unanimous})
	if err != nil {
		return 0, err
	}
	raw, err := d.Raw()
	if err != nil {
		return 0, err
	}
	if feeRate < 0 {
		return 0, badDis("fee rate %d", feeRate)
	}
	fee := (int64(len(raw))*feeRate + 999) / 1000
	return min(fee, MaxDissolutionFee), nil
}

// CheckAt applies the rules Dash Core checks a dissolution against when it is
// mined at spendHeight, except for the signatures. A dissolution that passes at
// a height passes at every later one.
func (d *Dissolution) CheckAt(spendHeight int32) error {
	return d.mn.check(d.tx, &d.pd, spendHeight)
}

func (m *SharedMN) check(tx *bisonwire.Tx, pd *protx.ProDisTx, spendHeight int32) error {
	n := len(m.Shares)
	if pd.ProTxHash != m.ProTxHash {
		return badDis("bad-prodis-hash")
	}
	if len(tx.TxIn) != 1 || tx.TxIn[0].PreviousOutPoint != m.Collateral || len(tx.TxIn[0].SignatureScript) != 0 {
		return badDis("bad-prodis-input")
	}
	actor := int(pd.ActorIndex)
	if actor >= n {
		return badDis("bad-prodis-actor")
	}
	if len(pd.Sigs) != 1 && len(pd.Sigs) != n {
		return badDis("bad-prodis-sig-count")
	}
	unanimous := len(pd.Sigs) == n
	required := m.RequiredPenalty(spendHeight, unanimous)

	if len(tx.TxOut) != n-1 && len(tx.TxOut) != n {
		return badDis("bad-prodis-payee-count")
	}
	w := m.nonActorTotal(actor)
	var bonuses, k int64
	for i := range m.Shares {
		if i == actor {
			continue
		}
		out := tx.TxOut[k]
		k++
		if !bytes.Equal(out.PkScript, m.Shares[i].RefundScript) {
			return badDis("bad-prodis-payee")
		}
		bonus := out.Value - m.Shares[i].Amount
		if bonus < proRataFloor(required, m.Shares[i].Amount, w) {
			return badDis("bad-prodis-penalty-floor")
		}
		bonuses += bonus
	}
	if len(tx.TxOut) == n {
		out := tx.TxOut[n-1]
		if !bytes.Equal(out.PkScript, m.Shares[actor].RefundScript) {
			return badDis("bad-prodis-payee")
		}
		if out.Value == 0 {
			return badDis("bad-prodis-actor-output-zero")
		}
	}
	if bonuses < required {
		return badDis("bad-prodis-penalty-sum")
	}
	var outTotal int64
	for _, o := range tx.TxOut {
		outTotal += o.Value
	}
	if m.total()-outTotal > MaxDissolutionFee || m.total()-outTotal < 0 {
		return badDis("bad-prodis-fee")
	}
	if !unanimous && bonuses > m.EarlyPenalty {
		return badDis("bad-prodis-bonus")
	}
	return nil
}

// slot returns the index of the signature slot of share i, or an error if the
// share does not sign this dissolution.
func (d *Dissolution) slot(i int) (int, error) {
	if i < 0 || i >= len(d.mn.Shares) {
		return 0, badDis("share %d of %d", i, len(d.mn.Shares))
	}
	if len(d.pd.Sigs) == 1 {
		if i != int(d.pd.ActorIndex) {
			return 0, badDis("only the actor, share %d, signs a unilateral dissolution", d.pd.ActorIndex)
		}
		return 0, nil
	}
	return i, nil
}

func (d *Dissolution) hash() (chainhash.Hash, error) {
	return d.pd.SignHash(&d.tx.MsgTx, uint8(len(d.pd.Sigs)))
}

// Sign signs the dissolution as share i with the share's owner key.
func (d *Dissolution) Sign(key *btcec.PrivateKey, i int) (ShareSig, error) {
	if _, err := d.slot(i); err != nil {
		return ShareSig{}, err
	}
	if !bytes.Equal(btcutil.Hash160(key.PubKey().SerializeCompressed()), d.mn.Shares[i].KeyIDOwner[:]) {
		return ShareSig{}, badDis("the key is not share %d's owner key", i)
	}
	h, err := d.hash()
	if err != nil {
		return ShareSig{}, err
	}
	sig := protx.SignCompact(key, h)
	return ShareSig{i, base64.StdEncoding.EncodeToString(sig[:])}, nil
}

// AddSigs adds signatures made by share owners. Each is verified, and none is
// added if any is bad.
func (d *Dissolution) AddSigs(sigs []ShareSig) error {
	h, err := d.hash()
	if err != nil {
		return err
	}
	parsed := make(map[int][protx.CompactSigSize]byte, len(sigs))
	for _, s := range sigs {
		slot, err := d.slot(s.ShareIndex)
		if err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(s.Signature)
		if err != nil {
			return badDis("share %d signature: %v", s.ShareIndex, err)
		}
		if err := protx.VerifyCompact(h, d.mn.Shares[s.ShareIndex].KeyIDOwner, raw); err != nil {
			return fmt.Errorf("%w: share %d: %w", ErrInvalidDissolution, s.ShareIndex, err)
		}
		parsed[slot] = [protx.CompactSigSize]byte(raw)
	}
	for slot, sig := range parsed {
		d.pd.Sigs[slot] = sig
	}
	return d.sync()
}

// Complete returns nil once every required signature is in.
func (d *Dissolution) Complete() error {
	for i, s := range d.pd.Sigs {
		if s == ([protx.CompactSigSize]byte{}) {
			return fmt.Errorf("%w: signature %d is missing", ErrInvalidDissolution, i)
		}
	}
	return nil
}

// Raw returns the serialized dissolution with the signatures added so far.
func (d *Dissolution) Raw() ([]byte, error) {
	var b bytes.Buffer
	if err := d.tx.Serialize(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Blob returns the dissolution with the signatures so far, in the form owners
// exchange.
func (d *Dissolution) Blob() (Blob, error) {
	raw, err := d.Raw()
	if err != nil {
		return Blob{}, err
	}
	out := Blob{Tx: hex.EncodeToString(raw)}
	for i, s := range d.pd.Sigs {
		if s == ([protx.CompactSigSize]byte{}) {
			continue
		}
		idx := i
		if len(d.pd.Sigs) == 1 {
			idx = int(d.pd.ActorIndex)
		}
		out.Signatures = append(out.Signatures, ShareSig{idx, base64.StdEncoding.EncodeToString(s[:])})
	}
	return out, nil
}

// ReviewDissolution is what a share owner runs before it signs a unanimous
// dissolution someone else made. The signature covers the outputs, so it must
// check them: every other share must get exactly its principal back at its
// refund script and the fee, which the actor pays, must not exceed maxFee.
// Dash Core also accepts a dissolution that pays more to other shares when all
// owners sign it, which this refuses.
//
// It checks the rules Dash Core applies and those signatures already in the
// transaction. It does not modify tx.
func ReviewDissolution(tx *bisonwire.Tx, m *SharedMN, own int, maxFee int64) (*Dissolution, error) {
	if tx.Type != txTypeProDis || tx.MsgTx.Version != txVersion || tx.LockTime != 0 {
		return nil, badDis("not a dissolution transaction")
	}
	if own < 0 || own >= len(m.Shares) {
		return nil, badDis("share %d of %d", own, len(m.Shares))
	}
	if maxFee < 0 || maxFee > MaxDissolutionFee {
		return nil, badDis("maximum fee %d", maxFee)
	}
	var pd protx.ProDisTx
	if err := protx.Parse(tx.ExtraPayload, &pd); err != nil {
		return nil, fmt.Errorf("%w: payload: %w", ErrInvalidDissolution, err)
	}
	if len(pd.Sigs) != len(m.Shares) {
		return nil, badDis("not a unanimous dissolution")
	}
	if len(tx.TxIn) != 1 || tx.TxIn[0].Sequence != finalSequence {
		return nil, badDis("the input must have a final sequence")
	}
	if err := m.check(tx, &pd, math.MaxInt32); err != nil {
		return nil, err
	}

	// No bonus for anyone: every payee gets its principal, and what is left
	// after the fee is the actor's.
	actor := int(pd.ActorIndex)
	k := 0
	var out int64
	for i := range m.Shares {
		if i == actor {
			continue
		}
		if tx.TxOut[k].Value != m.Shares[i].Amount {
			return nil, badDis("share %d would not get exactly its principal back", i)
		}
		k++
	}
	for _, o := range tx.TxOut {
		out += o.Value
	}
	if fee := m.total() - out; fee > maxFee {
		return nil, badDis("fee %d is over the %d agreed", fee, maxFee)
	}

	cp, err := cloneTx(tx)
	if err != nil {
		return nil, err
	}
	d := &Dissolution{mn: m, tx: cp, pd: pd}
	h, err := d.hash()
	if err != nil {
		return nil, err
	}
	for i, s := range pd.Sigs {
		if s == ([protx.CompactSigSize]byte{}) {
			continue
		}
		if err := protx.VerifyCompact(h, m.Shares[i].KeyIDOwner, s[:]); err != nil {
			return nil, fmt.Errorf("%w: signature %d: %w", ErrInvalidDissolution, i, err)
		}
	}
	return d, nil
}

// Standby is a signed unilateral dissolution that pays no penalty, kept to be
// broadcast later. It is valid from ValidFromHeight, after the early period, so
// it is a way out that does not need the other share owners, even if this
// wallet's keys are lost. It is data: where it is stored is up to the caller.
type Standby struct {
	ProTxHash       chainhash.Hash `json:"proTxHash"`
	Actor           int            `json:"actor"`
	ValidFromHeight int32          `json:"validFromHeight"`
	// Tx is the serialized transaction in hex.
	Tx string `json:"tx"`
}

// NewStandby builds and signs a standby dissolution by the actor share, with
// the actor's owner key.
func NewStandby(m *SharedMN, actor int, key *btcec.PrivateKey, fee int64) (*Standby, error) {
	d, err := NewDissolution(m, DissolveParams{Actor: actor, Fee: fee})
	if err != nil {
		return nil, err
	}
	s, err := d.Sign(key, actor)
	if err != nil {
		return nil, err
	}
	if err := d.AddSigs([]ShareSig{s}); err != nil {
		return nil, err
	}
	if err := d.Complete(); err != nil {
		return nil, err
	}
	raw, err := d.Raw()
	if err != nil {
		return nil, err
	}
	return &Standby{ProTxHash: m.ProTxHash, Actor: actor, ValidFromHeight: m.StandbyValidFrom(), Tx: hex.EncodeToString(raw)}, nil
}

// Ready reports whether the standby can be mined in a block at height.
func (s *Standby) Ready(height int32) bool { return height >= s.ValidFromHeight }

// Marshal encodes the standby for storage.
func (s *Standby) Marshal() ([]byte, error) { return json.Marshal(s) }

// ParseStandby decodes and checks a stored standby against the masternode's
// state: it must be a complete unilateral dissolution that is valid from its
// recorded height, signed by the actor.
func ParseStandby(data []byte, m *SharedMN) (*Standby, *bisonwire.Tx, error) {
	var s Standby
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, nil, badDis("standby: %v", err)
	}
	raw, err := hex.DecodeString(s.Tx)
	if err != nil {
		return nil, nil, badDis("standby tx: %v", err)
	}
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, nil, badDis("standby tx: %v", err)
	}
	var pd protx.ProDisTx
	if err := protx.Parse(tx.ExtraPayload, &pd); err != nil {
		return nil, nil, badDis("standby payload: %v", err)
	}
	// The signature does not cover the transaction type, which Dash Core
	// hashes, so a tampered type has to be refused here, as do a stored height
	// that is not the one the standby is valid from.
	if tx.Type != txTypeProDis || tx.MsgTx.Version != txVersion || tx.LockTime != 0 ||
		len(tx.TxIn) != 1 || tx.TxIn[0].Sequence != finalSequence {
		return nil, nil, badDis("not a dissolution transaction")
	}
	if s.ValidFromHeight != m.StandbyValidFrom() {
		return nil, nil, badDis("standby valid from %d, not %d", s.ValidFromHeight, m.StandbyValidFrom())
	}
	if s.ProTxHash != m.ProTxHash || int(pd.ActorIndex) != s.Actor || len(pd.Sigs) != 1 {
		return nil, nil, badDis("not a unilateral dissolution of this masternode by share %d", s.Actor)
	}
	if err := m.check(tx, &pd, m.StandbyValidFrom()); err != nil {
		return nil, nil, err
	}
	d := &Dissolution{mn: m, tx: tx, pd: pd}
	h, err := d.hash()
	if err != nil {
		return nil, nil, err
	}
	if err := protx.VerifyCompact(h, m.Shares[s.Actor].KeyIDOwner, pd.Sigs[0][:]); err != nil {
		return nil, nil, fmt.Errorf("%w: standby signature: %w", ErrInvalidDissolution, err)
	}
	return &s, tx, nil
}
