// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet/txauthor"
)

// A shared masternode can be changed in two ways after it is registered.
//
// A share update (ProUpShareTx, type 11) changes where one share's rewards are
// paid. Only that share's owner signs it, and it also funds it, so one party
// builds and signs the whole transaction (ShareUpdate).
//
// A registrar update (ProUpSharedRegTx, type 12) changes the operator key and
// the voting key. Every share owner signs it and one of them, the funder, pays
// the fee (RegistrarUpdate). The signatures bind the keys and the funder's
// input outpoints, not the outputs, so the funder checks the outputs itself
// before it signs its inputs (SignInputs). Changing the operator key stops the
// masternode from being paid until the new operator announces its service.
//
// Both carry funding inputs, and an input signature covers the payload, so the
// payload signatures are in before any input is signed.
//
// A registrar update's signatures bind the masternode, the two keys and the
// funder's input outpoints, and nothing else: no height, no expiry, and no link
// to the masternode's current keys. Once every owner has signed, the update can
// be broadcast at any time until the funder's inputs are spent, even after a
// later update has changed the keys, and then it sets the old ones again. This
// is Dash Core's payload format. Owners should treat a signed round as live
// until its funding outpoints are spent, and when one is abandoned the funder
// should spend them.

const (
	// MaxUpdateFee is the most an update may leave as fee, in duffs. It is a
	// limit of this wallet, not of Dash Core.
	MaxUpdateFee = 1_000_000

	txTypeProUpShare     = 11
	txTypeProUpSharedReg = 12
)

// ErrInvalidUpdate is wrapped by errors for an update that Dash Core would
// reject or that this wallet must not sign.
var ErrInvalidUpdate = errors.New("invalid shared masternode update")

func badUp(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidUpdate, fmt.Sprintf(format, args...))
}

// keyHash returns the key hash a P2PKH script pays to.
func keyHash(script []byte) (h [protx.KeyIDSize]byte, ok bool) {
	if len(script) != 25 || !payableScript(script) {
		return h, false
	}
	copy(h[:], script[3:23])
	return h, true
}

// fundedTx builds the transaction of an update: the funder's inputs, one change
// output and the fee. It returns the previous scripts by input index, for
// signing.
func fundedTx(f Funding, fee int64, typ uint16) (*bisonwire.Tx, map[int][]byte, error) {
	if len(f.Inputs) == 0 {
		return nil, nil, badUp("no inputs")
	}
	if fee <= 0 || fee > MaxUpdateFee {
		return nil, nil, badUp("fee %d", fee)
	}
	msg := wire.NewMsgTx(txVersion)
	prev := make(map[int][]byte, len(f.Inputs))
	seen := make(map[wire.OutPoint]struct{}, len(f.Inputs))
	var in int64
	for i := range f.Inputs {
		fi := f.Inputs[i]
		if !txscript.IsPayToPubKeyHash(fi.PkScript) {
			return nil, nil, badUp("input %v is not P2PKH", fi.OutPoint)
		}
		if _, dup := seen[fi.OutPoint]; dup {
			return nil, nil, badUp("input %v is spent twice", fi.OutPoint)
		}
		seen[fi.OutPoint] = struct{}{}
		if fi.Value <= 0 || fi.Value > maxMoney {
			return nil, nil, badUp("input %v has value %d", fi.OutPoint, fi.Value)
		}
		in += fi.Value
		if in > maxMoney {
			return nil, nil, badUp("inputs hold too much")
		}
		txin := wire.NewTxIn(&fi.OutPoint, nil, nil)
		txin.Sequence = finalSequence
		msg.AddTxIn(txin)
		prev[i] = fi.PkScript
	}
	if change := in - fee; change < dustLimit {
		return nil, nil, badUp("inputs hold %d, which leaves %d after the fee", in, change)
	}
	if !txscript.IsPayToPubKeyHash(f.ChangeScript) {
		return nil, nil, badUp("change needs a P2PKH script")
	}
	msg.AddTxOut(wire.NewTxOut(in-fee, f.ChangeScript))
	return &bisonwire.Tx{Chain: "dash", MsgTx: *msg, Type: typ}, prev, nil
}

func serializeTx(tx *bisonwire.Tx) ([]byte, error) {
	var b bytes.Buffer
	if err := tx.Serialize(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ShareUpdate is a share update being built and signed by the share's owner.
type ShareUpdate struct {
	mn   *SharedMN
	tx   *bisonwire.Tx
	pu   protx.ProUpShareTx
	prev map[int][]byte
}

// NewShareUpdate builds the unsigned update that makes reward the reward script
// of share. Dash Core accepts only a P2PKH or P2SH script that is not the
// voting key or any owner key, so pay it to an address of its own: an empty
// script is not a reset to the refund script, it is invalid.
func NewShareUpdate(m *SharedMN, share int, reward []byte, f Funding, fee int64) (*ShareUpdate, error) {
	if share < 0 || share >= len(m.Shares) {
		return nil, badUp("bad-proupshare-index: share %d of %d", share, len(m.Shares))
	}
	if m.KeyIDVoting == ([protx.KeyIDSize]byte{}) {
		return nil, badUp("the masternode's voting key is not set")
	}
	if !payableScript(reward) {
		return nil, badUp("bad-proupshare-payee: the reward script is not P2PKH or P2SH")
	}
	if h, ok := keyHash(reward); ok {
		if h == m.KeyIDVoting {
			return nil, badUp("bad-proupshare-payee-reuse: the reward pays the voting key")
		}
		for i := range m.Shares {
			if h == m.Shares[i].KeyIDOwner {
				return nil, badUp("bad-proupshare-payee-reuse: the reward pays share %d's owner key", i)
			}
		}
	}
	tx, prev, err := fundedTx(f, fee, txTypeProUpShare)
	if err != nil {
		return nil, err
	}
	u := &ShareUpdate{
		mn: m, tx: tx, prev: prev,
		pu: protx.ProUpShareTx{
			Version:      protx.SharedVersion,
			ProTxHash:    m.ProTxHash,
			ShareIndex:   uint16(share),
			ScriptReward: bytes.Clone(reward),
			InputsHash:   protx.InputsHash(tx.TxIn),
			Sig:          make([]byte, protx.CompactSigSize),
		},
	}
	if err := u.sync(); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *ShareUpdate) sync() error {
	var b bytes.Buffer
	if err := u.pu.Serialize(&b); err != nil {
		return err
	}
	u.tx.ExtraPayload = b.Bytes()
	return nil
}

// Sign signs the update with the share's owner key, then the funding inputs
// with secrets. It changes nothing if it fails.
func (u *ShareUpdate) Sign(owner *btcec.PrivateKey, secrets txauthor.SecretsSource) error {
	if !bytes.Equal(btcutil.Hash160(owner.PubKey().SerializeCompressed()), u.mn.Shares[u.pu.ShareIndex].KeyIDOwner[:]) {
		return badUp("the key is not share %d's owner key", u.pu.ShareIndex)
	}
	h, err := u.pu.PayloadHash()
	if err != nil {
		return err
	}
	old := u.pu.Sig
	sig := protx.SignCompact(owner, h)
	u.pu.Sig = sig[:]
	if err := u.sync(); err != nil {
		u.pu.Sig = old
		return err
	}
	if err := txauthor.AddDashInputScripts(u.tx, u.prev, secrets); err != nil {
		u.pu.Sig = old
		if serr := u.sync(); serr != nil {
			return errors.Join(err, serr)
		}
		return err
	}
	return nil
}

// Raw returns the serialized update. It is ready to broadcast once Sign
// succeeded.
func (u *ShareUpdate) Raw() ([]byte, error) { return serializeTx(u.tx) }

// RegistrarChange is the operator key and voting key a registrar update sets.
// Both are always set: to keep one, give its current value.
type RegistrarChange struct {
	PubKeyOperator [protx.OperatorPubKeySize]byte
	KeyIDVoting    [protx.KeyIDSize]byte
}

// checkRegistrar applies the rules of Dash Core's CheckProUpSharedRegTx that
// can be checked without the chain. Whether the operator key is used by another
// masternode cannot, and Dash Core refuses it.
func (m *SharedMN) checkRegistrar(c RegistrarChange) error {
	if n := len(m.Shares); n < MinShares || n > MaxShares {
		return badUp("%d shares, not %d to %d", n, MinShares, MaxShares)
	}
	if err := validBLSKey(c.PubKeyOperator[:]); err != nil {
		return badUp("bad-protx-operator-pubkey: %v", err)
	}
	if c.KeyIDVoting == ([protx.KeyIDSize]byte{}) {
		return badUp("bad-protx-key-null: the voting key is empty")
	}
	for i := range m.Shares {
		for _, sc := range [][]byte{m.Shares[i].RefundScript, m.Shares[i].RewardScript} {
			if h, ok := keyHash(sc); ok && h == c.KeyIDVoting {
				return badUp("bad-proupsharedreg-payee-reuse: the voting key is paid by share %d", i)
			}
		}
	}
	return nil
}

// RegistrarUpdate is a registrar update being signed by every share owner.
type RegistrarUpdate struct {
	// OperatorChanged is whether the update sets a different operator key, which
	// bans the masternode until the new operator announces its service. A user
	// must be told before it signs. It compares with the masternode's current
	// operator key, which is zero once the operator was revoked, so then any
	// key is a change. The masternode must be filled in from chain state.
	OperatorChanged bool

	mn *SharedMN
	tx *bisonwire.Tx
	pu protx.ProUpSharedRegTx
}

// NewRegistrarUpdate builds the unsigned update that sets c, funded by f. The
// funder is the one who builds it.
func NewRegistrarUpdate(m *SharedMN, c RegistrarChange, f Funding, fee int64) (*RegistrarUpdate, error) {
	if err := m.checkRegistrar(c); err != nil {
		return nil, err
	}
	// A zero key means the masternode's state was not filled in, or its operator
	// was revoked, which this API does not recover from: with it every update
	// would report an operator change.
	tx, _, err := fundedTx(f, fee, txTypeProUpSharedReg)
	if err != nil {
		return nil, err
	}
	u := &RegistrarUpdate{
		OperatorChanged: c.PubKeyOperator != m.PubKeyOperator,
		mn:              m, tx: tx,
		pu: protx.ProUpSharedRegTx{
			Version:        protx.SharedVersion,
			ProTxHash:      m.ProTxHash,
			PubKeyOperator: c.PubKeyOperator,
			KeyIDVoting:    c.KeyIDVoting,
			InputsHash:     protx.InputsHash(tx.TxIn),
			Sigs:           make([][protx.CompactSigSize]byte, len(m.Shares)),
		},
	}
	if err := u.sync(); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *RegistrarUpdate) sync() error {
	var b bytes.Buffer
	if err := u.pu.Serialize(&b); err != nil {
		return err
	}
	u.tx.ExtraPayload = b.Bytes()
	return nil
}

// ReviewRegistrarUpdate is what a share owner runs before it signs a registrar
// update someone else made. A signature binds the keys and the input outpoints
// and nothing else, so it checks that the keys are exactly want, the ones the
// owner agreed to, and that they are valid. It does not look at the outputs,
// which only the funder's signature depends on. It does not modify tx.
//
// A signature made after this review stays valid until the funder's inputs are
// spent, whatever the masternode's keys become in the meantime (see the package
// comment).
func ReviewRegistrarUpdate(tx *bisonwire.Tx, m *SharedMN, want RegistrarChange) (*RegistrarUpdate, error) {
	if tx.Type != txTypeProUpSharedReg || tx.MsgTx.Version != txVersion {
		return nil, badUp("not a registrar update transaction")
	}
	var pu protx.ProUpSharedRegTx
	if err := protx.Parse(tx.ExtraPayload, &pu); err != nil {
		return nil, fmt.Errorf("%w: payload: %w", ErrInvalidUpdate, err)
	}
	if pu.ProTxHash != m.ProTxHash {
		return nil, badUp("the update is for another masternode")
	}
	if pu.PubKeyOperator != want.PubKeyOperator {
		return nil, badUp("the operator key is not the one agreed")
	}
	if pu.KeyIDVoting != want.KeyIDVoting {
		return nil, badUp("the voting key is not the one agreed")
	}
	if err := m.checkRegistrar(want); err != nil {
		return nil, err
	}
	// A zero key means the masternode's state was not filled in, or its operator
	// was revoked, which this API does not recover from: with it every update
	// would report an operator change.
	if len(pu.Sigs) != len(m.Shares) {
		return nil, badUp("bad-proupsharedreg-sig-count: %d signatures for %d shares", len(pu.Sigs), len(m.Shares))
	}
	if len(tx.TxIn) == 0 || pu.InputsHash != protx.InputsHash(tx.TxIn) {
		return nil, badUp("bad-protx-inputs-hash")
	}
	h, err := pu.PayloadHash()
	if err != nil {
		return nil, err
	}
	for i, s := range pu.Sigs {
		if s == ([protx.CompactSigSize]byte{}) {
			continue
		}
		if err := protx.VerifyCompact(h, m.Shares[i].KeyIDOwner, s[:]); err != nil {
			return nil, fmt.Errorf("%w: signature %d: %w", ErrInvalidUpdate, i, err)
		}
	}
	cp, err := cloneTx(tx)
	if err != nil {
		return nil, err
	}
	return &RegistrarUpdate{
		OperatorChanged: pu.PubKeyOperator != m.PubKeyOperator,
		mn:              m, tx: cp, pu: pu,
	}, nil
}

// Sign signs the update as share i with the share's owner key.
func (u *RegistrarUpdate) Sign(key *btcec.PrivateKey, i int) (ShareSig, error) {
	if i < 0 || i >= len(u.mn.Shares) {
		return ShareSig{}, badUp("share %d of %d", i, len(u.mn.Shares))
	}
	if !bytes.Equal(btcutil.Hash160(key.PubKey().SerializeCompressed()), u.mn.Shares[i].KeyIDOwner[:]) {
		return ShareSig{}, badUp("the key is not share %d's owner key", i)
	}
	h, err := u.pu.PayloadHash()
	if err != nil {
		return ShareSig{}, err
	}
	sig := protx.SignCompact(key, h)
	return ShareSig{i, base64.StdEncoding.EncodeToString(sig[:])}, nil
}

// AddSigs adds signatures made by share owners. Each is verified, and none is
// added if any is bad.
func (u *RegistrarUpdate) AddSigs(sigs []ShareSig) error {
	h, err := u.pu.PayloadHash()
	if err != nil {
		return err
	}
	parsed := make(map[int][protx.CompactSigSize]byte, len(sigs))
	for _, s := range sigs {
		if s.ShareIndex < 0 || s.ShareIndex >= len(u.mn.Shares) {
			return badUp("signature for share %d of %d", s.ShareIndex, len(u.mn.Shares))
		}
		raw, err := base64.StdEncoding.DecodeString(s.Signature)
		if err != nil {
			return badUp("share %d signature: %v", s.ShareIndex, err)
		}
		if err := protx.VerifyCompact(h, u.mn.Shares[s.ShareIndex].KeyIDOwner, raw); err != nil {
			return fmt.Errorf("%w: share %d: %w", ErrInvalidUpdate, s.ShareIndex, err)
		}
		parsed[s.ShareIndex] = [protx.CompactSigSize]byte(raw)
	}
	// A signature that replaced another would change the payload the funder's
	// input signatures cover, so only a repeat of what is there is accepted.
	for i, sig := range parsed {
		if cur := u.pu.Sigs[i]; cur != ([protx.CompactSigSize]byte{}) && cur != sig {
			return badUp("share %d has already signed", i)
		}
	}
	for i, sig := range parsed {
		u.pu.Sigs[i] = sig
	}
	return u.sync()
}

// Complete returns nil once every share owner's signature is in.
func (u *RegistrarUpdate) Complete() error {
	for i, s := range u.pu.Sigs {
		if s == ([protx.CompactSigSize]byte{}) {
			return fmt.Errorf("%w: signature %d is missing", ErrInvalidUpdate, i)
		}
	}
	return nil
}

// SignInputs is run by the funder once every signature is in. The share
// signatures do not cover the outputs, so before it signs it requires the
// transaction to spend exactly f's inputs and pay only to f's change script,
// leaving a fee of at most maxFee, with no lock time and final sequences, as it
// was built. Anyone who passed the transaction round could otherwise have
// redirected the change or time-locked it, and the share signatures cover
// neither.
func (u *RegistrarUpdate) SignInputs(f Funding, maxFee int64, secrets txauthor.SecretsSource) error {
	if err := u.Complete(); err != nil {
		return err
	}
	if maxFee <= 0 || maxFee > MaxUpdateFee {
		return badUp("maximum fee %d", maxFee)
	}
	if u.tx.LockTime != 0 {
		return badUp("lock time %d", u.tx.LockTime)
	}
	for i, txin := range u.tx.TxIn {
		if txin.Sequence != finalSequence {
			return badUp("input %d sequence %#x", i, txin.Sequence)
		}
	}
	if len(u.tx.TxIn) != len(f.Inputs) {
		return badUp("the transaction spends %d inputs, not the funder's %d", len(u.tx.TxIn), len(f.Inputs))
	}
	prev := make(map[int][]byte, len(f.Inputs))
	var in, out int64
	for _, fi := range f.Inputs {
		idx := -1
		for i, txin := range u.tx.TxIn {
			if txin.PreviousOutPoint == fi.OutPoint {
				idx = i
				break
			}
		}
		if idx < 0 {
			return badUp("the transaction does not spend %v", fi.OutPoint)
		}
		if _, dup := prev[idx]; dup {
			return badUp("input %v is listed twice", fi.OutPoint)
		}
		if fi.Value <= 0 || fi.Value > maxMoney {
			return badUp("input %v has value %d", fi.OutPoint, fi.Value)
		}
		prev[idx] = fi.PkScript
		in += fi.Value
		if in > maxMoney {
			return badUp("inputs hold too much")
		}
	}
	if len(u.tx.TxOut) == 0 {
		return badUp("no outputs")
	}
	for _, o := range u.tx.TxOut {
		if !bytes.Equal(o.PkScript, f.ChangeScript) || o.Value < dustLimit || o.Value > maxMoney {
			return badUp("an output does not pay the funder's change script or is dust")
		}
		out += o.Value
		if out > maxMoney {
			return badUp("outputs pay too much")
		}
	}
	if fee := in - out; fee <= 0 || fee > maxFee {
		return badUp("fee %d is not within (0, %d]", fee, maxFee)
	}
	return txauthor.AddDashInputScripts(u.tx, prev, secrets)
}

// Raw returns the serialized update with the signatures added so far.
func (u *RegistrarUpdate) Raw() ([]byte, error) { return serializeTx(u.tx) }

// Blob returns the update with the signatures so far, in the form owners
// exchange.
func (u *RegistrarUpdate) Blob() (Blob, error) {
	raw, err := u.Raw()
	if err != nil {
		return Blob{}, err
	}
	out := Blob{Tx: hex.EncodeToString(raw)}
	for i, s := range u.pu.Sigs {
		if s != ([protx.CompactSigSize]byte{}) {
			out.Signatures = append(out.Signatures, ShareSig{i, base64.StdEncoding.EncodeToString(s[:])})
		}
	}
	return out, nil
}
