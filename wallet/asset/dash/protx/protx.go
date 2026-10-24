// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

// Package protx encodes and decodes the DIP-3 provider transaction payloads
// (the extra payload of Dash special transactions of types 1 to 4) and computes
// the hashes they commit to. It has no wallet dependency.
//
// Only payload version 2 (basic BLS scheme, one CService address) is
// supported. Version 3 (extended addresses) is not accepted by released chains
// until DEPLOYMENT_V24 activates, and evonode registrations are not supported.
// The layouts were pinned against transactions built by Dash Core v23.1.4,
// see testdata.
//
// Serialize does not enforce the length limits Deserialize applies to the
// payout script (10000 bytes) and the payloadSig (96 bytes), so a payload built
// with a longer one cannot be read back. Dash only accepts a 65 byte compact
// signature or none.
package protx

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const (
	// Version is the only supported payload version: the basic BLS scheme
	// with a single CService address.
	Version = 2

	// TypeRegular is the masternode type of a regular masternode.
	TypeRegular = 0

	// KeyIDSize is the size of a key id (hash160 of a public key).
	KeyIDSize = 20
	// OperatorPubKeySize is the size of a compressed BLS12-381 G1 public key.
	OperatorPubKeySize = 48
	// BLSSigSize is the size of a compressed BLS12-381 G2 signature.
	BLSSigSize = 96

	// maxScriptSize bounds a payout script read from a payload so a
	// malformed length cannot force a large allocation.
	maxScriptSize = 10_000
	// maxSigSize bounds a payloadSig. A compact ECDSA signature is 65 bytes
	// and a BLS signature is 96.
	maxSigSize = BLSSigSize
)

var (
	// ErrUnsupportedVersion is returned for a payload version other than
	// Version.
	ErrUnsupportedVersion = errors.New("unsupported provider tx version")
	// ErrUnsupportedType is returned for a masternode type other than
	// TypeRegular.
	ErrUnsupportedType = errors.New("unsupported masternode type")
	// ErrTrailingData is returned when a payload has bytes after the last
	// field.
	ErrTrailingData = errors.New("trailing data after provider tx payload")
)

// Service is a masternode's core P2P address in Dash's CService encoding: a
// 16 byte IPv6 (IPv4 addresses are v4-mapped) and a big endian port.
type Service struct {
	IP   [16]byte
	Port uint16
}

func (s *Service) serialize(w io.Writer) error {
	if _, err := w.Write(s.IP[:]); err != nil {
		return err
	}
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], s.Port)
	_, err := w.Write(port[:])
	return err
}

func (s *Service) deserialize(r io.Reader) error {
	if _, err := io.ReadFull(r, s.IP[:]); err != nil {
		return err
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return err
	}
	s.Port = binary.BigEndian.Uint16(port[:])
	return nil
}

// ProRegTx is a provider registration payload (special transaction type 1).
type ProRegTx struct {
	Version uint16
	// MNType is TypeRegular.
	MNType uint16
	Mode   uint16
	// Collateral is the collateral outpoint. A zero hash means the collateral
	// is output Index of the registration transaction itself.
	Collateral     wire.OutPoint
	Service        Service
	KeyIDOwner     [KeyIDSize]byte
	PubKeyOperator [OperatorPubKeySize]byte
	KeyIDVoting    [KeyIDSize]byte
	// OperatorReward is the operator's share of the reward in basis points.
	OperatorReward uint16
	ScriptPayout   []byte
	// InputsHash commits to the transaction's input outpoints, see
	// InputsHash.
	InputsHash chainhash.Hash
	// PayloadSig is empty for internal collateral and a 65 byte compact
	// signature by the collateral key for external collateral.
	PayloadSig []byte
}

// Serialize writes the payload.
func (p *ProRegTx) Serialize(w io.Writer) error {
	return p.serialize(w, true)
}

func (p *ProRegTx) serialize(w io.Writer, withSig bool) error {
	if p.Version != Version {
		return ErrUnsupportedVersion
	}
	if p.MNType != TypeRegular {
		return ErrUnsupportedType
	}
	if err := writeU16s(w, p.Version, p.MNType, p.Mode); err != nil {
		return err
	}
	if _, err := w.Write(p.Collateral.Hash[:]); err != nil {
		return err
	}
	if err := writeU32(w, p.Collateral.Index); err != nil {
		return err
	}
	if err := p.Service.serialize(w); err != nil {
		return err
	}
	for _, b := range [][]byte{p.KeyIDOwner[:], p.PubKeyOperator[:], p.KeyIDVoting[:]} {
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	if err := writeU16s(w, p.OperatorReward); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(w, 0, p.ScriptPayout); err != nil {
		return err
	}
	if _, err := w.Write(p.InputsHash[:]); err != nil {
		return err
	}
	if !withSig {
		return nil
	}
	return wire.WriteVarBytes(w, 0, p.PayloadSig)
}

// Deserialize reads the payload.
func (p *ProRegTx) Deserialize(r io.Reader) error {
	var err error
	if p.Version, err = readU16(r); err != nil {
		return err
	}
	if p.Version != Version {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, p.Version)
	}
	if p.MNType, err = readU16(r); err != nil {
		return err
	}
	if p.MNType != TypeRegular {
		return fmt.Errorf("%w: %d", ErrUnsupportedType, p.MNType)
	}
	if p.Mode, err = readU16(r); err != nil {
		return err
	}
	if _, err = io.ReadFull(r, p.Collateral.Hash[:]); err != nil {
		return err
	}
	if p.Collateral.Index, err = readU32(r); err != nil {
		return err
	}
	if err = p.Service.deserialize(r); err != nil {
		return err
	}
	for _, b := range [][]byte{p.KeyIDOwner[:], p.PubKeyOperator[:], p.KeyIDVoting[:]} {
		if _, err = io.ReadFull(r, b); err != nil {
			return err
		}
	}
	if p.OperatorReward, err = readU16(r); err != nil {
		return err
	}
	if p.ScriptPayout, err = wire.ReadVarBytes(r, 0, maxScriptSize, "scriptPayout"); err != nil {
		return err
	}
	if _, err = io.ReadFull(r, p.InputsHash[:]); err != nil {
		return err
	}
	p.PayloadSig, err = wire.ReadVarBytes(r, 0, maxSigSize, "payloadSig")
	return err
}

// PayloadHash returns the hash the payload signature commits to: the double
// SHA-256 of the payload without its signature.
func (p *ProRegTx) PayloadHash() (chainhash.Hash, error) {
	return payloadHash(func(w io.Writer) error { return p.serialize(w, false) })
}

// CollateralMessage returns the message an external collateral key signs to
// authorize the registration:
//
//	payoutAddress|operatorReward|ownerAddress|votingAddress|payloadHash
//
// The payout is written as an address, or as hex if the script has none. The
// hash is the payload hash in its usual (byte reversed) hex form. net supplies
// the address prefixes.
func (p *ProRegTx) CollateralMessage(net *chaincfg.Params) (string, error) {
	hash, err := p.PayloadHash()
	if err != nil {
		return "", err
	}

	payout := hex.EncodeToString(p.ScriptPayout)
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(p.ScriptPayout, net)
	if err == nil && len(addrs) == 1 {
		payout = addrs[0].EncodeAddress()
	}

	owner, err := btcutil.NewAddressPubKeyHash(p.KeyIDOwner[:], net)
	if err != nil {
		return "", err
	}
	voting, err := btcutil.NewAddressPubKeyHash(p.KeyIDVoting[:], net)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		payout,
		strconv.FormatUint(uint64(p.OperatorReward), 10),
		owner.EncodeAddress(),
		voting.EncodeAddress(),
		hash.String(),
	}, "|"), nil
}

// ProUpServTx is a provider service update payload (special transaction type
// 2), signed by the operator.
type ProUpServTx struct {
	Version uint16
	// MNType is TypeRegular.
	MNType            uint16
	ProTxHash         chainhash.Hash
	Service           Service
	ScriptOperatorPay []byte
	InputsHash        chainhash.Hash
	// Sig is the operator's BLS signature.
	Sig [BLSSigSize]byte
}

// Serialize writes the payload.
func (p *ProUpServTx) Serialize(w io.Writer) error {
	if err := p.serialize(w); err != nil {
		return err
	}
	_, err := w.Write(p.Sig[:])
	return err
}

func (p *ProUpServTx) serialize(w io.Writer) error {
	if p.Version != Version {
		return ErrUnsupportedVersion
	}
	if p.MNType != TypeRegular {
		return ErrUnsupportedType
	}
	if err := writeU16s(w, p.Version, p.MNType); err != nil {
		return err
	}
	if _, err := w.Write(p.ProTxHash[:]); err != nil {
		return err
	}
	if err := p.Service.serialize(w); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(w, 0, p.ScriptOperatorPay); err != nil {
		return err
	}
	_, err := w.Write(p.InputsHash[:])
	return err
}

// Deserialize reads the payload.
func (p *ProUpServTx) Deserialize(r io.Reader) error {
	var err error
	if p.Version, err = readU16(r); err != nil {
		return err
	}
	if p.Version != Version {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, p.Version)
	}
	if p.MNType, err = readU16(r); err != nil {
		return err
	}
	if p.MNType != TypeRegular {
		return fmt.Errorf("%w: %d", ErrUnsupportedType, p.MNType)
	}
	if _, err = io.ReadFull(r, p.ProTxHash[:]); err != nil {
		return err
	}
	if err = p.Service.deserialize(r); err != nil {
		return err
	}
	if p.ScriptOperatorPay, err = wire.ReadVarBytes(r, 0, maxScriptSize, "scriptOperatorPayout"); err != nil {
		return err
	}
	if _, err = io.ReadFull(r, p.InputsHash[:]); err != nil {
		return err
	}
	_, err = io.ReadFull(r, p.Sig[:])
	return err
}

// PayloadHash returns the hash the operator signs: the double SHA-256 of the
// payload without its signature.
func (p *ProUpServTx) PayloadHash() (chainhash.Hash, error) {
	return payloadHash(p.serialize)
}

// ProUpRegTx is a provider registrar update payload (special transaction type
// 3), signed by the owner key.
type ProUpRegTx struct {
	Version        uint16
	ProTxHash      chainhash.Hash
	Mode           uint16
	PubKeyOperator [OperatorPubKeySize]byte
	KeyIDVoting    [KeyIDSize]byte
	ScriptPayout   []byte
	InputsHash     chainhash.Hash
	// PayloadSig is a 65 byte compact signature by the owner key over
	// PayloadHash.
	PayloadSig []byte
}

// Serialize writes the payload.
func (p *ProUpRegTx) Serialize(w io.Writer) error {
	return p.serialize(w, true)
}

func (p *ProUpRegTx) serialize(w io.Writer, withSig bool) error {
	if p.Version != Version {
		return ErrUnsupportedVersion
	}
	if err := writeU16s(w, p.Version); err != nil {
		return err
	}
	if _, err := w.Write(p.ProTxHash[:]); err != nil {
		return err
	}
	if err := writeU16s(w, p.Mode); err != nil {
		return err
	}
	if _, err := w.Write(p.PubKeyOperator[:]); err != nil {
		return err
	}
	if _, err := w.Write(p.KeyIDVoting[:]); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(w, 0, p.ScriptPayout); err != nil {
		return err
	}
	if _, err := w.Write(p.InputsHash[:]); err != nil {
		return err
	}
	if !withSig {
		return nil
	}
	return wire.WriteVarBytes(w, 0, p.PayloadSig)
}

// Deserialize reads the payload.
func (p *ProUpRegTx) Deserialize(r io.Reader) error {
	var err error
	if p.Version, err = readU16(r); err != nil {
		return err
	}
	if p.Version != Version {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, p.Version)
	}
	if _, err = io.ReadFull(r, p.ProTxHash[:]); err != nil {
		return err
	}
	if p.Mode, err = readU16(r); err != nil {
		return err
	}
	if _, err = io.ReadFull(r, p.PubKeyOperator[:]); err != nil {
		return err
	}
	if _, err = io.ReadFull(r, p.KeyIDVoting[:]); err != nil {
		return err
	}
	if p.ScriptPayout, err = wire.ReadVarBytes(r, 0, maxScriptSize, "scriptPayout"); err != nil {
		return err
	}
	if _, err = io.ReadFull(r, p.InputsHash[:]); err != nil {
		return err
	}
	p.PayloadSig, err = wire.ReadVarBytes(r, 0, maxSigSize, "payloadSig")
	return err
}

// PayloadHash returns the hash the owner signs: the double SHA-256 of the
// payload without its signature.
func (p *ProUpRegTx) PayloadHash() (chainhash.Hash, error) {
	return payloadHash(func(w io.Writer) error { return p.serialize(w, false) })
}

// ProUpRevTx is a provider revocation payload (special transaction type 4),
// signed by the operator.
type ProUpRevTx struct {
	Version   uint16
	ProTxHash chainhash.Hash
	// Reason is the revocation reason, 0 to 3.
	Reason     uint16
	InputsHash chainhash.Hash
	// Sig is the operator's BLS signature.
	Sig [BLSSigSize]byte
}

// Serialize writes the payload.
func (p *ProUpRevTx) Serialize(w io.Writer) error {
	if err := p.serialize(w); err != nil {
		return err
	}
	_, err := w.Write(p.Sig[:])
	return err
}

func (p *ProUpRevTx) serialize(w io.Writer) error {
	if p.Version != Version {
		return ErrUnsupportedVersion
	}
	if err := writeU16s(w, p.Version); err != nil {
		return err
	}
	if _, err := w.Write(p.ProTxHash[:]); err != nil {
		return err
	}
	if err := writeU16s(w, p.Reason); err != nil {
		return err
	}
	_, err := w.Write(p.InputsHash[:])
	return err
}

// Deserialize reads the payload.
func (p *ProUpRevTx) Deserialize(r io.Reader) error {
	var err error
	if p.Version, err = readU16(r); err != nil {
		return err
	}
	if p.Version != Version {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, p.Version)
	}
	if _, err = io.ReadFull(r, p.ProTxHash[:]); err != nil {
		return err
	}
	if p.Reason, err = readU16(r); err != nil {
		return err
	}
	if _, err = io.ReadFull(r, p.InputsHash[:]); err != nil {
		return err
	}
	_, err = io.ReadFull(r, p.Sig[:])
	return err
}

// PayloadHash returns the hash the operator signs: the double SHA-256 of the
// payload without its signature.
func (p *ProUpRevTx) PayloadHash() (chainhash.Hash, error) {
	return payloadHash(p.serialize)
}

// payloadDecoder is implemented by every payload type.
type payloadDecoder interface {
	Deserialize(io.Reader) error
}

// Parse decodes a whole payload into p and fails if bytes follow the last
// field. A payload that Deserialize accepts with bytes left over would not be
// covered by the hash recomputed from the decoded fields.
func Parse(payload []byte, p payloadDecoder) error {
	r := bytes.NewReader(payload)
	if err := p.Deserialize(r); err != nil {
		return err
	}
	if r.Len() != 0 {
		return ErrTrailingData
	}
	return nil
}

// InputsHash returns the hash a provider transaction commits to for its
// inputs: the double SHA-256 of the input outpoints in order, each as the
// 32 byte transaction hash followed by the little endian output index.
func InputsHash(txIns []*wire.TxIn) chainhash.Hash {
	buf := make([]byte, 0, len(txIns)*(chainhash.HashSize+4))
	for _, in := range txIns {
		buf = append(buf, in.PreviousOutPoint.Hash[:]...)
		buf = binary.LittleEndian.AppendUint32(buf, in.PreviousOutPoint.Index)
	}
	return chainhash.DoubleHashH(buf)
}

func payloadHash(serialize func(io.Writer) error) (chainhash.Hash, error) {
	var buf bytes.Buffer
	if err := serialize(&buf); err != nil {
		return chainhash.Hash{}, err
	}
	return chainhash.DoubleHashH(buf.Bytes()), nil
}

func writeU16s(w io.Writer, vs ...uint16) error {
	var b [2]byte
	for _, v := range vs {
		binary.LittleEndian.PutUint16(b[:], v)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}
	}
	return nil
}

func writeU32(w io.Writer, v uint32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func readU16(r io.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b[:]), nil
}

func readU32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}
