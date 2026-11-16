// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

// Package protx encodes and decodes the DIP-3 provider transaction payloads
// (the extra payload of Dash special transactions of types 1 to 4), the shared
// masternode payloads of Dash Core v24 (types 10 to 12), and computes the
// hashes the older ones commit to. It has no wallet dependency.
//
// Payload version 2 (basic BLS scheme, one CService address) is supported by
// all four DIP-3 payloads, for regular masternodes only. A ProRegTx also
// decodes at version 3 (extended addresses, payout lists and shares, regular
// and EvoNode); the other DIP-3 payloads at version 3 are not supported yet.
// Released chains accept version 3 only once DEPLOYMENT_V24 activates. The
// layouts were pinned against transactions built by Dash Core v23.1.4 (version
// 2) and v24.0.0 (version 3, types 10 to 12), see testdata.
//
// Decoding a version 3 ProRegTx and encoding it again gives the same bytes,
// but a hash over a payload this wallet did not build must still use the bytes
// as received (bisonwire.Tx.ExtraPayload), never a re-encoding. Version 2 does
// not have that property, see Service. PayloadHash and CollateralMessage return
// ErrUnsupportedVersion for version 3 until the digests are pinned.
//
// Serialize applies the length limits Deserialize does (10000 bytes for a
// script, 96 for a signature), so a payload that is written can be read back.
// Dash only accepts a 65 byte compact signature or none.
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
	// Version is the payload version of the basic BLS scheme with a single
	// CService address, supported by every DIP-3 payload.
	Version = 2
	// VersionExtAddr is the payload version with extended addresses
	// (NetInfo). Only a ProRegTx supports it.
	VersionExtAddr = 3
	// SharedVersion is the payload version of the shared masternode types 10
	// to 12.
	SharedVersion = 1

	// TypeRegular is the masternode type of a regular masternode.
	TypeRegular = 0
	// TypeEvo is the masternode type of an EvoNode. It is only supported at
	// VersionExtAddr.
	TypeEvo = 1

	// KeyIDSize is the size of a key id (hash160 of a public key).
	KeyIDSize = 20
	// OperatorPubKeySize is the size of a compressed BLS12-381 G1 public key.
	OperatorPubKeySize = 48
	// BLSSigSize is the size of a compressed BLS12-381 G2 signature.
	BLSSigSize = 96
	// CompactSigSize is the size of a compact ECDSA signature.
	CompactSigSize = 65

	// maxScriptSize bounds a payout script read from a payload so a
	// malformed length cannot force a large allocation.
	maxScriptSize = 10_000
	// maxSigSize bounds a payloadSig. A compact ECDSA signature is 65 bytes
	// and a BLS signature is 96.
	maxSigSize = BLSSigSize
	// maxCount bounds a count of payouts, shares, signatures or netInfo
	// items read from a payload, as an allocation guard. The limits Dash
	// enforces are much lower and are validation, not codec, concerns.
	maxCount = 255
)

var (
	// ErrUnsupportedVersion is returned for a payload version the payload
	// type does not support.
	ErrUnsupportedVersion = errors.New("unsupported provider tx version")
	// ErrUnsupportedType is returned for a masternode type the payload
	// version does not support.
	ErrUnsupportedType = errors.New("unsupported masternode type")
	// ErrUnsupportedNetInfo is returned for a netInfo this package cannot
	// carry: another layout version, an entry kind other than a service
	// address, or an IPv4 or IPv6 address of the wrong length.
	ErrUnsupportedNetInfo = errors.New("unsupported netInfo")
	// ErrFieldTooLong is returned by Serialize for a field longer than
	// Deserialize accepts.
	ErrFieldTooLong = errors.New("provider tx field too long")
	// ErrTrailingData is returned when a payload has bytes after the last
	// field.
	ErrTrailingData = errors.New("trailing data after provider tx payload")
	// ErrNonCanonicalAddress is returned by Serialize and PayloadHash for a
	// service address Dash would store differently from how it was written,
	// see Service.
	ErrNonCanonicalAddress = errors.New("non canonical service address")
)

var (
	ipv4MappedPrefix = [12]byte{10: 0xff, 11: 0xff}
	torV2Prefix      = [6]byte{0xfd, 0x87, 0xd8, 0x7e, 0xeb, 0x43}
	internalPrefix   = [6]byte{0xfd, 0x6b, 0x88, 0xc0, 0x87, 0x24}
)

// Service is a masternode's core P2P address in Dash's CService encoding: a
// 16 byte IPv6 (IPv4 addresses are v4-mapped) and a big endian port.
//
// Dash does not hash the address it received but a copy it parsed and wrote
// back, and it stores an address that is not valid as the empty Service. So
// decoding does the same: an invalid address becomes Service{}, and the bytes
// read are not kept. Serializing refuses such an address, so only valid ones
// and the empty Service are ever written.
type Service struct {
	IP   [16]byte
	Port uint16
}

// usable mirrors CNetAddr::IsValid in Dash Core for the 16 byte encoding.
func (s *Service) usable() bool {
	ip := s.IP[:]
	switch {
	case bytes.Equal(ip, make([]byte, len(ip))):
		return false
	case bytes.HasPrefix(ip, ipv4MappedPrefix[:]):
		v4 := ip[len(ipv4MappedPrefix):]
		return !bytes.Equal(v4, []byte{0, 0, 0, 0}) &&
			!bytes.Equal(v4, []byte{0xff, 0xff, 0xff, 0xff})
	case bytes.HasPrefix(ip, torV2Prefix[:]), bytes.HasPrefix(ip, internalPrefix[:]):
		return false
	case bytes.HasPrefix(ip, []byte{0x20, 0x01, 0x0d, 0xb8}):
		// Documentation prefix, 2001:db8::/32.
		return false
	}
	return true
}

func (s *Service) canonical() bool {
	return s.usable() || *s == Service{}
}

func (s *Service) serialize(w io.Writer) error {
	if !s.canonical() {
		return ErrNonCanonicalAddress
	}
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
	if !s.canonical() {
		*s = Service{}
	}
	return nil
}

// Payout is one payee of a masternode's owner reward.
type Payout struct {
	Script []byte
	// Reward is the payee's share in basis points.
	Reward uint16
}

// Share is one participant of a shared masternode: its part of the collateral
// and what it signed to join.
type Share struct {
	// Amount is the collateral contribution in duffs.
	Amount int64
	// RefundScript receives the principal at dissolution.
	RefundScript []byte
	// RewardScript receives this share's rewards.
	RewardScript []byte
	KeyIDOwner   [KeyIDSize]byte
	// JoinSig is the owner's compact signature consenting to the
	// registration. On the wire all shares are written first, then all join
	// signatures.
	JoinSig [CompactSigSize]byte
}

// ProRegTx is a provider registration payload (special transaction type 1).
//
// Version 2 uses Service and ScriptPayout. Version 3 replaces them with
// NetInfo and Payouts and adds Shares, EarlyPeriodBlocks, EarlyPenalty and, for
// an EvoNode, PlatformNodeID. A field the version does not have must be left
// zero, or Serialize fails.
type ProRegTx struct {
	// Version is Version or VersionExtAddr.
	Version uint16
	// MNType is TypeRegular, or TypeEvo at VersionExtAddr.
	MNType uint16
	Mode   uint16
	// Collateral is the collateral outpoint. A zero hash means the collateral
	// is output Index of the registration transaction itself. A shared
	// masternode's collateral is always that.
	Collateral wire.OutPoint
	// Service is the core P2P address (version 2).
	Service Service
	// NetInfo holds the addresses (version 3).
	NetInfo NetInfo
	// KeyIDOwner is zero for a shared masternode: the share owners hold the
	// keys.
	KeyIDOwner     [KeyIDSize]byte
	PubKeyOperator [OperatorPubKeySize]byte
	KeyIDVoting    [KeyIDSize]byte
	// OperatorReward is the operator's share of the reward in basis points.
	OperatorReward uint16
	// ScriptPayout is the owner's payout script (version 2).
	ScriptPayout []byte
	// Payouts splits the owner's reward (version 3).
	Payouts []Payout
	// Shares is the collateral share table of a shared masternode (version
	// 3). It is empty for a masternode with one owner.
	Shares []Share
	// EarlyPeriodBlocks and EarlyPenalty are the early dissolution terms of a
	// shared masternode (version 3), zero otherwise.
	EarlyPeriodBlocks uint32
	EarlyPenalty      int64
	// InputsHash commits to the transaction's input outpoints, see
	// InputsHash.
	InputsHash chainhash.Hash
	// PlatformNodeID is the Platform node id of an EvoNode (version 3).
	PlatformNodeID [KeyIDSize]byte
	// PayloadSig is empty for internal collateral and a 65 byte compact
	// signature by the collateral key for external collateral.
	PayloadSig []byte
}

// Serialize writes the payload.
func (p *ProRegTx) Serialize(w io.Writer) error {
	return p.serialize(w, true)
}

func (p *ProRegTx) serialize(w io.Writer, withSig bool) error {
	if err := p.checkSerializable(); err != nil {
		return err
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
	if p.Version == VersionExtAddr {
		return p.serializeExt(w, withSig)
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
	if err := writeBytes(w, "scriptPayout", p.ScriptPayout, maxScriptSize); err != nil {
		return err
	}
	if _, err := w.Write(p.InputsHash[:]); err != nil {
		return err
	}
	if !withSig {
		return nil
	}
	return writeBytes(w, "payloadSig", p.PayloadSig, maxSigSize)
}

// checkSerializable rejects a version and type this package does not support
// and a field the version does not have.
func (p *ProRegTx) checkSerializable() error {
	switch p.Version {
	case Version:
		if p.MNType != TypeRegular {
			return ErrUnsupportedType
		}
		if p.NetInfo.Version != 0 || len(p.NetInfo.Purposes) != 0 ||
			len(p.Payouts) != 0 || len(p.Shares) != 0 ||
			p.EarlyPeriodBlocks != 0 || p.EarlyPenalty != 0 ||
			p.PlatformNodeID != [KeyIDSize]byte{} {
			return fmt.Errorf("%w: version 3 field set on a version %d payload",
				ErrUnsupportedVersion, p.Version)
		}
	case VersionExtAddr:
		if p.MNType != TypeRegular && p.MNType != TypeEvo {
			return ErrUnsupportedType
		}
		if p.Service != (Service{}) || len(p.ScriptPayout) != 0 {
			return fmt.Errorf("%w: version 2 field set on a version %d payload",
				ErrUnsupportedVersion, p.Version)
		}
		if p.MNType != TypeEvo && p.PlatformNodeID != [KeyIDSize]byte{} {
			return fmt.Errorf("%w: platformNodeID on a regular masternode",
				ErrUnsupportedType)
		}
	default:
		return ErrUnsupportedVersion
	}
	return nil
}

// serializeExt writes a version 3 payload after the collateral.
func (p *ProRegTx) serializeExt(w io.Writer, withSig bool) error {
	if err := p.NetInfo.serialize(w); err != nil {
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
	if err := writeCount(w, "payouts", len(p.Payouts)); err != nil {
		return err
	}
	for i := range p.Payouts {
		if err := writeBytes(w, "payout script", p.Payouts[i].Script, maxScriptSize); err != nil {
			return err
		}
		if err := writeU16s(w, p.Payouts[i].Reward); err != nil {
			return err
		}
	}
	if err := writeCount(w, "shares", len(p.Shares)); err != nil {
		return err
	}
	for i := range p.Shares {
		s := &p.Shares[i]
		if err := writeU64(w, uint64(s.Amount)); err != nil {
			return err
		}
		if err := writeBytes(w, "refund script", s.RefundScript, maxScriptSize); err != nil {
			return err
		}
		if err := writeBytes(w, "reward script", s.RewardScript, maxScriptSize); err != nil {
			return err
		}
		if _, err := w.Write(s.KeyIDOwner[:]); err != nil {
			return err
		}
	}
	for i := range p.Shares {
		if _, err := w.Write(p.Shares[i].JoinSig[:]); err != nil {
			return err
		}
	}
	if err := writeU32(w, p.EarlyPeriodBlocks); err != nil {
		return err
	}
	if err := writeU64(w, uint64(p.EarlyPenalty)); err != nil {
		return err
	}
	if _, err := w.Write(p.InputsHash[:]); err != nil {
		return err
	}
	if p.MNType == TypeEvo {
		if _, err := w.Write(p.PlatformNodeID[:]); err != nil {
			return err
		}
	}
	if !withSig {
		return nil
	}
	return writeBytes(w, "payloadSig", p.PayloadSig, maxSigSize)
}

// Deserialize reads the payload.
func (p *ProRegTx) Deserialize(r io.Reader) error {
	var err error
	if p.Version, err = readU16(r); err != nil {
		return err
	}
	if p.Version != Version && p.Version != VersionExtAddr {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, p.Version)
	}
	if p.MNType, err = readU16(r); err != nil {
		return err
	}
	if p.MNType != TypeRegular && (p.MNType != TypeEvo || p.Version != VersionExtAddr) {
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
	if p.Version == VersionExtAddr {
		return p.deserializeExt(r)
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
	if p.ScriptPayout, err = readBytes(r, "scriptPayout", maxScriptSize); err != nil {
		return err
	}
	if _, err = io.ReadFull(r, p.InputsHash[:]); err != nil {
		return err
	}
	p.PayloadSig, err = readBytes(r, "payloadSig", maxSigSize)
	return err
}

// deserializeExt reads a version 3 payload after the collateral.
func (p *ProRegTx) deserializeExt(r io.Reader) error {
	if err := p.NetInfo.deserialize(r); err != nil {
		return err
	}
	for _, b := range [][]byte{p.KeyIDOwner[:], p.PubKeyOperator[:], p.KeyIDVoting[:]} {
		if _, err := io.ReadFull(r, b); err != nil {
			return err
		}
	}
	var err error
	if p.OperatorReward, err = readU16(r); err != nil {
		return err
	}
	np, err := readCount(r, "payouts")
	if err != nil {
		return err
	}
	p.Payouts = nil
	if np > 0 {
		p.Payouts = make([]Payout, np)
	}
	for i := range p.Payouts {
		if p.Payouts[i].Script, err = readBytes(r, "payout script", maxScriptSize); err != nil {
			return err
		}
		if p.Payouts[i].Reward, err = readU16(r); err != nil {
			return err
		}
	}
	ns, err := readCount(r, "shares")
	if err != nil {
		return err
	}
	p.Shares = nil
	if ns > 0 {
		p.Shares = make([]Share, ns)
	}
	for i := range p.Shares {
		s := &p.Shares[i]
		amount, err := readU64(r)
		if err != nil {
			return err
		}
		s.Amount = int64(amount)
		if s.RefundScript, err = readBytes(r, "refund script", maxScriptSize); err != nil {
			return err
		}
		if s.RewardScript, err = readBytes(r, "reward script", maxScriptSize); err != nil {
			return err
		}
		if _, err = io.ReadFull(r, s.KeyIDOwner[:]); err != nil {
			return err
		}
	}
	for i := range p.Shares {
		if _, err = io.ReadFull(r, p.Shares[i].JoinSig[:]); err != nil {
			return err
		}
	}
	if p.EarlyPeriodBlocks, err = readU32(r); err != nil {
		return err
	}
	penalty, err := readU64(r)
	if err != nil {
		return err
	}
	p.EarlyPenalty = int64(penalty)
	if _, err = io.ReadFull(r, p.InputsHash[:]); err != nil {
		return err
	}
	if p.MNType == TypeEvo {
		if _, err = io.ReadFull(r, p.PlatformNodeID[:]); err != nil {
			return err
		}
	}
	p.PayloadSig, err = readBytes(r, "payloadSig", maxSigSize)
	return err
}

// PayloadHash returns the hash the payload signature commits to: the double
// SHA-256 of the payload without its signature. It supports version 2 only.
func (p *ProRegTx) PayloadHash() (chainhash.Hash, error) {
	if p.Version != Version {
		return chainhash.Hash{}, ErrUnsupportedVersion
	}
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

	// Dash's ExtractDestination only finds an address for these classes, where
	// ExtractPkScriptAddrs also returns one for 1-of-1 multisig and witness
	// programs.
	payout := hex.EncodeToString(p.ScriptPayout)
	switch txscript.GetScriptClass(p.ScriptPayout) {
	case txscript.PubKeyHashTy, txscript.ScriptHashTy, txscript.PubKeyTy:
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(p.ScriptPayout, net)
		if err == nil && len(addrs) == 1 {
			payout = addrs[0].EncodeAddress()
		}
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
	if err := writeBytes(w, "scriptOperatorPayout", p.ScriptOperatorPay, maxScriptSize); err != nil {
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
	if err := writeBytes(w, "scriptPayout", p.ScriptPayout, maxScriptSize); err != nil {
		return err
	}
	if _, err := w.Write(p.InputsHash[:]); err != nil {
		return err
	}
	if !withSig {
		return nil
	}
	return writeBytes(w, "payloadSig", p.PayloadSig, maxSigSize)
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
		// A payload that ends early is malformed, not finished.
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
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

func writeU64(w io.Writer, v uint64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func readU64(r io.Reader) (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

func writeU8(w io.Writer, v uint8) error {
	_, err := w.Write([]byte{v})
	return err
}

func readU8(r io.Reader) (uint8, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func writeU16BE(w io.Writer, v uint16) error {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func readU16BE(r io.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b[:]), nil
}

// writeBytes writes b as a length prefixed field, refusing one longer than
// Deserialize would read.
func writeBytes(w io.Writer, field string, b []byte, limit int) error {
	if len(b) > limit {
		return fmt.Errorf("%w: %s is %d bytes, at most %d", ErrFieldTooLong, field, len(b), limit)
	}
	return wire.WriteVarBytes(w, 0, b)
}

func readBytes(r io.Reader, field string, limit int) ([]byte, error) {
	return wire.ReadVarBytes(r, 0, uint32(limit), field)
}

// writeCount writes the count of a list as a CompactSize, refusing more items
// than Deserialize would read.
func writeCount(w io.Writer, field string, n int) error {
	if n > maxCount {
		return fmt.Errorf("%w: %d %s, at most %d", ErrFieldTooLong, n, field, maxCount)
	}
	return wire.WriteVarInt(w, 0, uint64(n))
}

// readCount reads the count of a list. A CompactSize that is not the shortest
// encoding is an error, so decoding and encoding again gives the same bytes.
func readCount(r io.Reader, field string) (int, error) {
	n, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return 0, err
	}
	if n > maxCount {
		return 0, fmt.Errorf("%w: %d %s, at most %d", ErrFieldTooLong, n, field, maxCount)
	}
	return int(n), nil
}
