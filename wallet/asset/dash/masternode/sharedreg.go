// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet/txauthor"
)

// A shared masternode registration is one transaction that every share owner
// funds and consents to. The registrar builds it (Proposal.Build) and sends it
// to the other owners. Each owner reviews it against what it expects of its own
// share and funds (Review), then signs its consent (RegReview.SignConsent) and
// its inputs (RegReview.SignInputs). The signatures are collected into the one
// transaction (RegReview.AddJoinSigs) and, once every owner has consented
// (RegReview.Complete), it is ready to broadcast.
//
// The consent digest does not bind the collateral index, the owner key ids, the
// input values or the other participants' inputs, so Review checks them for
// the reviewing owner: signing without it would only prove that everyone
// signed the same thing, not that it is the thing they wanted.

const (
	// MaxFeePortion is the most one share may pay towards the fee, in duffs.
	MaxFeePortion = 1_000_000
	// dustLimit is the smallest change output worth making, in duffs.
	dustLimit = 546

	finalSequence = 0xffffffff
	txVersion     = bisonwire.DashSpecialTxVersion
	txTypeProReg  = 1
)

// SharedCollateralScript is the script of a shared masternode's collateral
// output: a push of "DSHC", OP_DROP and OP_TRUE. Nobody holds a key for it, so
// it is spent by the protocol (a dissolution), not by a signature.
var SharedCollateralScript = []byte{0x04, 'D', 'S', 'H', 'C', 0x75, 0x51}

var (
	// ErrInvalidProposal is wrapped by errors for a registration that must
	// not be built or signed.
	ErrInvalidProposal = errors.New("invalid shared registration")
	// ErrNotComplete is returned when a join signature is missing.
	ErrNotComplete = errors.New("shared registration is missing join signatures")
)

func bad(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidProposal, fmt.Sprintf(format, args...))
}

// FundingInput is a P2PKH output a share owner spends into the registration.
// Value is the owner's own knowledge of the output, which the transaction does
// not carry.
type FundingInput struct {
	OutPoint wire.OutPoint
	PkScript []byte
	Value    int64
}

// Funding is what one share owner contributes. ChangeScript receives what the
// inputs hold beyond the share and the fee portion.
type Funding struct {
	Inputs       []FundingInput
	ChangeScript []byte
}

// Proposal is a shared registration to build: the terms, and one Funding per
// share in share order. Fee is split equally between the shares, the remainder
// going to the first.
type Proposal struct {
	Terms   Terms
	Funding []Funding
	Fee     int64
}

// FeePortion is what share i of n pays of fee: an equal part, with the
// remainder on the first share.
func FeePortion(fee int64, i, n int) int64 {
	p := fee / int64(n)
	if i == 0 {
		p += fee % int64(n)
	}
	return p
}

// Build returns the unsigned registration transaction: every share's inputs in
// share order, then each share's change, then the collateral output. The join
// signatures and input scripts are empty.
func (p *Proposal) Build(rules NetRules) (*bisonwire.Tx, error) {
	if err := p.Terms.Validate(rules); err != nil {
		return nil, err
	}
	n := len(p.Terms.Shares)
	if len(p.Funding) != n {
		return nil, bad("%d fundings for %d shares", len(p.Funding), n)
	}
	if p.Fee < 0 || FeePortion(p.Fee, 0, n) > MaxFeePortion {
		return nil, bad("fee %d", p.Fee)
	}

	msg := wire.NewMsgTx(txVersion)
	seen := make(map[wire.OutPoint]struct{})
	var changes []*wire.TxOut
	for i, f := range p.Funding {
		if len(f.Inputs) == 0 {
			return nil, bad("share %d funds no inputs", i)
		}
		var in int64
		for _, fi := range f.Inputs {
			if !txscript.IsPayToPubKeyHash(fi.PkScript) {
				return nil, bad("share %d: input %v is not P2PKH", i, fi.OutPoint)
			}
			if _, dup := seen[fi.OutPoint]; dup {
				return nil, bad("input %v is spent twice", fi.OutPoint)
			}
			seen[fi.OutPoint] = struct{}{}
			if fi.Value <= 0 {
				return nil, bad("share %d: input %v has value %d", i, fi.OutPoint, fi.Value)
			}
			in += fi.Value
			txin := wire.NewTxIn(&fi.OutPoint, nil, nil)
			txin.Sequence = finalSequence
			msg.AddTxIn(txin)
		}
		change := in - p.Terms.Shares[i].Amount - FeePortion(p.Fee, i, n)
		switch {
		case change < 0:
			return nil, bad("share %d: inputs hold %d, need %d", i, in, p.Terms.Shares[i].Amount+FeePortion(p.Fee, i, n))
		case change == 0:
		case change < dustLimit:
			return nil, bad("share %d: change %d is dust", i, change)
		case !txscript.IsPayToPubKeyHash(f.ChangeScript):
			return nil, bad("share %d: change needs a P2PKH script", i)
		default:
			changes = append(changes, wire.NewTxOut(change, f.ChangeScript))
		}
	}
	for _, o := range changes {
		msg.AddTxOut(o)
	}
	msg.AddTxOut(wire.NewTxOut(CollateralAmount, SharedCollateralScript))

	pr := &protx.ProRegTx{
		Version:           protx.VersionExtAddr,
		MNType:            protx.TypeRegular,
		Collateral:        wire.OutPoint{Index: uint32(len(msg.TxOut) - 1)},
		NetInfo:           p.Terms.NetInfo,
		PubKeyOperator:    p.Terms.PubKeyOperator,
		KeyIDVoting:       p.Terms.KeyIDVoting,
		OperatorReward:    p.Terms.OperatorReward,
		Shares:            make([]protx.Share, n),
		EarlyPeriodBlocks: p.Terms.EarlyPeriodBlocks,
		EarlyPenalty:      p.Terms.EarlyPenalty,
		InputsHash:        protx.InputsHash(msg.TxIn),
	}
	for i, s := range p.Terms.Shares {
		s.JoinSig = [protx.CompactSigSize]byte{}
		pr.Shares[i] = s
	}
	tx := &bisonwire.Tx{Chain: "dash", MsgTx: *msg, Type: txTypeProReg}
	if err := setPayload(tx, pr); err != nil {
		return nil, err
	}
	return tx, nil
}

func setPayload(tx *bisonwire.Tx, pr *protx.ProRegTx) error {
	var b bytes.Buffer
	if err := pr.Serialize(&b); err != nil {
		return err
	}
	tx.ExtraPayload = b.Bytes()
	return nil
}

// Own is what a share owner expects of the registration it is asked to sign.
// Everything the consent digest binds but does not check is here, so Review
// can require the registration to be exactly what the owner agreed to.
type Own struct {
	// ShareIndex is the owner's place in the share table.
	ShareIndex int
	// Terms are the registration terms the owner agreed to: the whole share
	// table, the operator and voting keys, the early terms and the netInfo.
	// The owner's own share, which it must check most closely, is
	// Terms.Shares[ShareIndex]; its join signature is ignored.
	Terms Terms
	// Fee is the total fee the owners agreed to. The owner pays
	// FeePortion(Fee, ShareIndex, shares) of it, and Review requires exactly
	// that, since it cannot see the other inputs to work out the real fee.
	Fee int64
	// Inputs are the owner's funding inputs, and ChangeScript where its
	// change goes (empty if none is expected).
	Inputs       []FundingInput
	ChangeScript []byte
}

// maxMoney is the most any output or input may hold, in duffs.
const maxMoney = 21_000_000 * 1e8

func termsEqual(a, b *Terms) bool {
	if len(a.Shares) != len(b.Shares) || a.PubKeyOperator != b.PubKeyOperator ||
		a.KeyIDVoting != b.KeyIDVoting || a.OperatorReward != b.OperatorReward ||
		a.EarlyPeriodBlocks != b.EarlyPeriodBlocks || a.EarlyPenalty != b.EarlyPenalty ||
		!netInfoEqual(&a.NetInfo, &b.NetInfo) {
		return false
	}
	for i := range a.Shares {
		x, y := &a.Shares[i], &b.Shares[i]
		if x.Amount != y.Amount || x.KeyIDOwner != y.KeyIDOwner ||
			!bytes.Equal(x.RefundScript, y.RefundScript) || !bytes.Equal(x.RewardScript, y.RewardScript) {
			return false
		}
	}
	return true
}

func netInfoEqual(a, b *protx.NetInfo) bool {
	if a.Version != b.Version || len(a.Purposes) != len(b.Purposes) {
		return false
	}
	for i := range a.Purposes {
		x, y := &a.Purposes[i], &b.Purposes[i]
		if x.ID != y.ID || len(x.Entries) != len(y.Entries) {
			return false
		}
		for k := range x.Entries {
			e, f := &x.Entries[k], &y.Entries[k]
			if e.Kind != f.Kind || e.Network != f.Network || e.Port != f.Port ||
				e.Domain != f.Domain || !bytes.Equal(e.Addr, f.Addr) {
				return false
			}
		}
	}
	return true
}

// RegReview is a registration an owner has checked. Only a review can sign.
type RegReview struct {
	// Terms are the registration's terms.
	Terms Terms
	// CollateralIndex is the collateral output's index.
	CollateralIndex int
	// ConsentHash is what every owner signs.
	ConsentHash chainhash.Hash
	// FeePortion is what the reviewing owner pays towards the fee.
	FeePortion int64
	// OtherOutputs are outputs that are neither the collateral nor the
	// reviewing owner's change, which other owners fund. A user interface
	// should show them.
	OtherOutputs []*wire.TxOut

	own        Own
	ownerKeyID [protx.KeyIDSize]byte
	tx         *bisonwire.Tx
	pr         protx.ProRegTx
	prev       map[int][]byte
}

// Review checks that tx is the registration its owner expects and returns it
// for signing. It does not modify tx. It fails if anything the consent digest
// leaves unbound is not as the owner expects: the collateral index and output,
// the owner's own share entry and the money it pays, and the inputs.
func Review(tx *bisonwire.Tx, rules NetRules, own Own) (*RegReview, error) {
	if tx.Type != txTypeProReg || tx.MsgTx.Version != txVersion {
		return nil, bad("not a provider registration transaction")
	}
	if tx.LockTime != 0 {
		return nil, bad("lock time %d", tx.LockTime)
	}
	var pr protx.ProRegTx
	if err := protx.Parse(tx.ExtraPayload, &pr); err != nil {
		return nil, fmt.Errorf("%w: payload: %w", ErrInvalidProposal, err)
	}
	if pr.Version != protx.VersionExtAddr || pr.MNType != protx.TypeRegular || pr.Mode != 0 {
		return nil, bad("not a shared registration of a regular masternode")
	}
	if pr.Collateral.Hash != (chainhash.Hash{}) || pr.KeyIDOwner != ([protx.KeyIDSize]byte{}) ||
		len(pr.Payouts) != 0 || len(pr.PayloadSig) != 0 || pr.PlatformNodeID != ([protx.KeyIDSize]byte{}) {
		return nil, bad("fields a shared registration must leave empty are set")
	}
	if pr.InputsHash != protx.InputsHash(tx.TxIn) {
		return nil, bad("inputs hash does not match the inputs")
	}
	terms := Terms{
		Shares:            slices.Clone(pr.Shares),
		NetInfo:           pr.NetInfo,
		PubKeyOperator:    pr.PubKeyOperator,
		KeyIDVoting:       pr.KeyIDVoting,
		OperatorReward:    pr.OperatorReward,
		EarlyPeriodBlocks: pr.EarlyPeriodBlocks,
		EarlyPenalty:      pr.EarlyPenalty,
	}
	if err := terms.Validate(rules); err != nil {
		return nil, err
	}

	// The collateral: one output, of the full amount, to the shared
	// collateral script, at the index the payload names.
	ci := int(pr.Collateral.Index)
	if ci >= len(tx.TxOut) {
		return nil, bad("collateral index %d of %d outputs", ci, len(tx.TxOut))
	}
	if tx.TxOut[ci].Value != CollateralAmount || !bytes.Equal(tx.TxOut[ci].PkScript, SharedCollateralScript) {
		return nil, bad("output %d is not the collateral", ci)
	}
	for i, o := range tx.TxOut {
		if i != ci && bytes.Equal(o.PkScript, SharedCollateralScript) {
			return nil, bad("output %d is a second collateral", i)
		}
	}

	// The terms are what the owner agreed to. The consent digest binds
	// them, which only shows that everyone signed the same terms.
	if own.ShareIndex < 0 || own.ShareIndex >= len(pr.Shares) {
		return nil, bad("share index %d of %d", own.ShareIndex, len(pr.Shares))
	}
	if !termsEqual(&terms, &own.Terms) {
		return nil, bad("the terms are not the ones the owner agreed to")
	}
	share := &pr.Shares[own.ShareIndex]
	n := len(pr.Shares)
	if own.Fee < 0 || own.Fee > int64(n)*MaxFeePortion {
		return nil, bad("fee %d", own.Fee)
	}

	// The inputs: final sequences (the digest binds them, and a BIP68 value
	// would lock the registration), no repeats, and the owner's own present.
	seen := make(map[wire.OutPoint]int, len(tx.TxIn))
	for i, in := range tx.TxIn {
		if in.Sequence != finalSequence {
			return nil, bad("input %d sequence %#x", i, in.Sequence)
		}
		if _, dup := seen[in.PreviousOutPoint]; dup {
			return nil, bad("input %d repeats an outpoint", i)
		}
		seen[in.PreviousOutPoint] = i
	}
	prev := make(map[int][]byte, len(own.Inputs))
	var ownIn int64
	for _, fi := range own.Inputs {
		i, ok := seen[fi.OutPoint]
		if !ok {
			return nil, bad("own input %v is missing", fi.OutPoint)
		}
		if len(tx.TxIn[i].SignatureScript) != 0 {
			return nil, bad("own input %v already has a signature script", fi.OutPoint)
		}
		if fi.Value <= 0 || fi.Value > maxMoney || !txscript.IsPayToPubKeyHash(fi.PkScript) {
			return nil, bad("own input %v: value %d or script not usable", fi.OutPoint, fi.Value)
		}
		prev[i] = fi.PkScript
		ownIn += fi.Value
	}
	if len(prev) == 0 {
		return nil, bad("the owner funds no inputs")
	}

	// The money: what the owner puts in less its change is its share plus a
	// bounded part of the fee. Nothing in the digest ties the outputs to an
	// owner, so a builder that skimmed an owner's inputs shows up here.
	var change int64
	var others []*wire.TxOut
	for i, o := range tx.TxOut {
		if o.Value < 0 || o.Value > maxMoney {
			return nil, bad("output %d value %d", i, o.Value)
		}
		switch {
		case i == ci:
		case len(own.ChangeScript) != 0 && bytes.Equal(o.PkScript, own.ChangeScript):
			change += o.Value
		default:
			others = append(others, o)
		}
	}
	portion := ownIn - change - share.Amount
	if want := FeePortion(own.Fee, own.ShareIndex, n); portion != want {
		return nil, bad("the owner pays %d beyond its share, want exactly %d", portion, want)
	}

	cp, err := cloneTx(tx)
	if err != nil {
		return nil, err
	}
	hash, err := pr.SharedRegConsentHash(&cp.MsgTx)
	if err != nil {
		return nil, err
	}
	own.Inputs = slices.Clone(own.Inputs)
	rv := &RegReview{
		Terms: terms, CollateralIndex: ci, ConsentHash: hash,
		FeePortion: portion, OtherOutputs: others,
		own: own, tx: cp, pr: pr, prev: prev,
		ownerKeyID: share.KeyIDOwner,
	}
	// Join signatures already in the transaction must be valid.
	for i := range pr.Shares {
		if pr.Shares[i].JoinSig == ([protx.CompactSigSize]byte{}) {
			continue
		}
		if err := protx.VerifyCompact(hash, pr.Shares[i].KeyIDOwner, pr.Shares[i].JoinSig[:]); err != nil {
			return nil, fmt.Errorf("%w: join signature %d: %w", ErrInvalidProposal, i, err)
		}
	}
	return rv, nil
}

// SignedShares are the indexes of the shares whose join signature is in.
func (rv *RegReview) SignedShares() []int {
	var out []int
	for i := range rv.pr.Shares {
		if rv.pr.Shares[i].JoinSig != ([protx.CompactSigSize]byte{}) {
			out = append(out, i)
		}
	}
	return out
}

// Raw returns the serialized registration with the signatures added so far.
func (rv *RegReview) Raw() ([]byte, error) {
	var b bytes.Buffer
	if err := rv.tx.Serialize(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ShareSig is a join signature in the form Dash Core's shared_sign and
// shared_combine exchange: the share index and the base64 compact signature.
type ShareSig struct {
	ShareIndex int    `json:"shareIndex"`
	Signature  string `json:"signature"`
}

// SignConsent signs the owner's consent with its owner key, which must be the
// key of the reviewed share.
func (rv *RegReview) SignConsent(key *btcec.PrivateKey) (ShareSig, error) {
	if !bytes.Equal(btcutil.Hash160(key.PubKey().SerializeCompressed()), rv.ownerKeyID[:]) {
		return ShareSig{}, bad("the key is not share %d's owner key", rv.own.ShareIndex)
	}
	sig := protx.SignCompact(key, rv.ConsentHash)
	return ShareSig{rv.own.ShareIndex, base64.StdEncoding.EncodeToString(sig[:])}, nil
}

// AddJoinSigs adds other owners' join signatures. Each is verified against its
// share's owner key and the consent digest, and none is added if any is bad.
func (rv *RegReview) AddJoinSigs(sigs []ShareSig) error {
	parsed := make(map[int][protx.CompactSigSize]byte, len(sigs))
	for _, s := range sigs {
		if s.ShareIndex < 0 || s.ShareIndex >= len(rv.pr.Shares) {
			return bad("signature for share %d of %d", s.ShareIndex, len(rv.pr.Shares))
		}
		raw, err := base64.StdEncoding.DecodeString(s.Signature)
		if err != nil {
			return bad("share %d signature: %v", s.ShareIndex, err)
		}
		if err := protx.VerifyCompact(rv.ConsentHash, rv.pr.Shares[s.ShareIndex].KeyIDOwner, raw); err != nil {
			return fmt.Errorf("%w: share %d: %w", ErrInvalidProposal, s.ShareIndex, err)
		}
		parsed[s.ShareIndex] = [protx.CompactSigSize]byte(raw)
	}
	for i, sig := range parsed {
		rv.pr.Shares[i].JoinSig = sig
	}
	return setPayload(rv.tx, &rv.pr)
}

// Complete returns nil once every owner's join signature is in.
func (rv *RegReview) Complete() error {
	for i := range rv.pr.Shares {
		if rv.pr.Shares[i].JoinSig == ([protx.CompactSigSize]byte{}) {
			return fmt.Errorf("%w: share %d", ErrNotComplete, i)
		}
	}
	return nil
}

// SignInputs signs the owner's funding inputs. The transaction must be final
// first: a signature covers the whole transaction, including the payload, so
// the join signatures are added before any input is signed.
func (rv *RegReview) SignInputs(secrets txauthor.SecretsSource) error {
	if err := rv.Complete(); err != nil {
		return err
	}
	return txauthor.AddDashInputScripts(rv.tx, rv.prev, secrets)
}

// Blob is what the owners exchange: the transaction and the join signatures
// collected so far. The signatures are in Dash Core's shared_combine format.
type Blob struct {
	Tx         string     `json:"tx"`
	Signatures []ShareSig `json:"signatures,omitempty"`
}

// Blob returns the registration as it stands, with the join signatures in it.
func (rv *RegReview) Blob() (Blob, error) {
	raw, err := rv.Raw()
	if err != nil {
		return Blob{}, err
	}
	out := Blob{Tx: hex.EncodeToString(raw)}
	for i := range rv.pr.Shares {
		if sig := rv.pr.Shares[i].JoinSig; sig != ([protx.CompactSigSize]byte{}) {
			out.Signatures = append(out.Signatures, ShareSig{i, base64.StdEncoding.EncodeToString(sig[:])})
		}
	}
	return out, nil
}

// ParseBlob decodes a blob and returns its transaction and signatures.
func ParseBlob(data []byte) (*bisonwire.Tx, []ShareSig, error) {
	var b Blob
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, nil, fmt.Errorf("%w: blob: %w", ErrInvalidProposal, err)
	}
	raw, err := hex.DecodeString(b.Tx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: blob tx: %w", ErrInvalidProposal, err)
	}
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, nil, fmt.Errorf("%w: blob tx: %w", ErrInvalidProposal, err)
	}
	return tx, b.Signatures, nil
}

func cloneTx(tx *bisonwire.Tx) (*bisonwire.Tx, error) {
	var b bytes.Buffer
	if err := tx.Serialize(&b); err != nil {
		return nil, err
	}
	cp := &bisonwire.Tx{Chain: "dash"}
	if err := cp.Deserialize(&b); err != nil {
		return nil, err
	}
	return cp, nil
}
