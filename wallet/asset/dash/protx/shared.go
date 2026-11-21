// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package protx

import (
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// The payloads below belong to the shared masternodes of Dash Core v24 and
// are special transaction types 10 to 12. They have no PayloadHash yet: the
// digests their signatures cover are not the double SHA-256 of the payload,
// and are left to the code that signs them.

// ProDisTx is a shared masternode dissolution payload (special transaction type
// 10). It has no inputsHash: the signatures cover the transaction's inputs and
// outputs. One signature is a unilateral dissolution by the actor share, one
// per share is a unanimous one.
type ProDisTx struct {
	Version   uint16
	ProTxHash chainhash.Hash
	// ActorIndex is the share that dissolves, and pays the fee.
	ActorIndex uint16
	// Sigs are 65 byte compact signatures by share owner keys.
	Sigs [][CompactSigSize]byte
}

// Serialize writes the payload.
func (p *ProDisTx) Serialize(w io.Writer) error {
	if p.Version != SharedVersion {
		return ErrUnsupportedVersion
	}
	if err := writeU16s(w, p.Version); err != nil {
		return err
	}
	if _, err := w.Write(p.ProTxHash[:]); err != nil {
		return err
	}
	if err := writeU16s(w, p.ActorIndex); err != nil {
		return err
	}
	return writeSigs(w, p.Sigs)
}

// Deserialize reads the payload.
func (p *ProDisTx) Deserialize(r io.Reader) error {
	var err error
	if p.Version, err = readU16(r); err != nil {
		return err
	}
	if p.Version != SharedVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, p.Version)
	}
	if _, err = io.ReadFull(r, p.ProTxHash[:]); err != nil {
		return err
	}
	if p.ActorIndex, err = readU16(r); err != nil {
		return err
	}
	p.Sigs, err = readSigs(r)
	return err
}

// ProUpShareTx is a share update payload (special transaction type 11): the
// owner of one share changes where that share's rewards are paid.
type ProUpShareTx struct {
	Version    uint16
	ProTxHash  chainhash.Hash
	ShareIndex uint16
	// ScriptReward is the new reward script.
	ScriptReward []byte
	InputsHash   chainhash.Hash
	// Sig is a 65 byte compact signature by the share's owner key.
	Sig []byte
}

// Serialize writes the payload.
func (p *ProUpShareTx) Serialize(w io.Writer) error {
	if p.Version != SharedVersion {
		return ErrUnsupportedVersion
	}
	if err := writeU16s(w, p.Version); err != nil {
		return err
	}
	if _, err := w.Write(p.ProTxHash[:]); err != nil {
		return err
	}
	if err := writeU16s(w, p.ShareIndex); err != nil {
		return err
	}
	if err := writeBytes(w, "scriptReward", p.ScriptReward, maxScriptSize); err != nil {
		return err
	}
	if _, err := w.Write(p.InputsHash[:]); err != nil {
		return err
	}
	if len(p.Sig) < CompactSigSize {
		return fmt.Errorf("%w: %d bytes, want %d", ErrBadSigSize, len(p.Sig), CompactSigSize)
	}
	return writeBytes(w, "sig", p.Sig, CompactSigSize)
}

// Deserialize reads the payload.
func (p *ProUpShareTx) Deserialize(r io.Reader) error {
	var err error
	if p.Version, err = readU16(r); err != nil {
		return err
	}
	if p.Version != SharedVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, p.Version)
	}
	if _, err = io.ReadFull(r, p.ProTxHash[:]); err != nil {
		return err
	}
	if p.ShareIndex, err = readU16(r); err != nil {
		return err
	}
	if p.ScriptReward, err = readBytes(r, "scriptReward", maxScriptSize); err != nil {
		return err
	}
	if _, err = io.ReadFull(r, p.InputsHash[:]); err != nil {
		return err
	}
	if p.Sig, err = readBytes(r, "sig", CompactSigSize); err != nil {
		return err
	}
	if len(p.Sig) < CompactSigSize {
		return fmt.Errorf("%w: %d bytes, want %d", ErrBadSigSize, len(p.Sig), CompactSigSize)
	}
	return nil
}

// ProUpSharedRegTx is a shared masternode registrar update payload (special
// transaction type 12): a new operator key and voting key, signed by every
// share owner.
type ProUpSharedRegTx struct {
	Version        uint16
	ProTxHash      chainhash.Hash
	PubKeyOperator [OperatorPubKeySize]byte
	KeyIDVoting    [KeyIDSize]byte
	InputsHash     chainhash.Hash
	// Sigs are 65 byte compact signatures by share owner keys.
	Sigs [][CompactSigSize]byte
}

// Serialize writes the payload.
func (p *ProUpSharedRegTx) Serialize(w io.Writer) error {
	if p.Version != SharedVersion {
		return ErrUnsupportedVersion
	}
	if err := writeU16s(w, p.Version); err != nil {
		return err
	}
	for _, b := range [][]byte{p.ProTxHash[:], p.PubKeyOperator[:], p.KeyIDVoting[:], p.InputsHash[:]} {
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return writeSigs(w, p.Sigs)
}

// Deserialize reads the payload.
func (p *ProUpSharedRegTx) Deserialize(r io.Reader) error {
	var err error
	if p.Version, err = readU16(r); err != nil {
		return err
	}
	if p.Version != SharedVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, p.Version)
	}
	for _, b := range [][]byte{p.ProTxHash[:], p.PubKeyOperator[:], p.KeyIDVoting[:], p.InputsHash[:]} {
		if _, err = io.ReadFull(r, b); err != nil {
			return err
		}
	}
	p.Sigs, err = readSigs(r)
	return err
}

// writeSigs writes a count and that many 65 byte signatures.
func writeSigs(w io.Writer, sigs [][CompactSigSize]byte) error {
	if err := writeCount8(w, "signatures", len(sigs)); err != nil {
		return err
	}
	for i := range sigs {
		if _, err := w.Write(sigs[i][:]); err != nil {
			return err
		}
	}
	return nil
}

func readSigs(r io.Reader) ([][CompactSigSize]byte, error) {
	n, err := readCount8(r)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	sigs := make([][CompactSigSize]byte, n)
	for i := range sigs {
		if _, err := io.ReadFull(r, sigs[i][:]); err != nil {
			return nil, err
		}
	}
	return sigs, nil
}
