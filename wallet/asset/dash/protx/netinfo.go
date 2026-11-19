// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package protx

import (
	"fmt"
	"io"

	"github.com/btcsuite/btcd/wire"
)

// Network purposes of a NetInfo, as numbered by Dash Core.
const (
	PurposeCoreP2P       = 0
	PurposePlatformP2P   = 1
	PurposePlatformHTTPS = 2
)

// Address networks of a NetEntry, as numbered by BIP155.
const (
	NetworkIPv4 = 1
	NetworkIPv6 = 2
	NetworkTORv3 = 4
	NetworkI2P   = 5
	NetworkCJDNS = 6
)

// Entry kinds of a NetEntry, as numbered by Dash Core.
const (
	// EntryService is a network address and a port.
	EntryService = 1
	// EntryDomain is a domain name and a port. Dash only allows it for
	// PurposePlatformHTTPS, which only EvoNodes have.
	EntryDomain = 2
)

const (
	// netInfoVersion is the only NetInfo layout version.
	netInfoVersion = 1
	// maxAddrSize is the largest address BIP155 allows.
	maxAddrSize = 512
	// maxDomainSize is the longest domain name Dash Core reads
	// (DOMAIN_MAX_LEN).
	maxDomainSize = 253
)

// NetInfo is the extended network information of a version 3 ProRegTx: a list
// of addresses for each purpose, in registration order.
//
// Decoding keeps what was written, with no normalisation, so decoding and
// encoding again gives the same bytes. Which addresses Dash accepts (an IPv4
// first Core entry, ports, no duplicates) is validation and not checked here.
type NetInfo struct {
	// Version is 1.
	Version  uint8
	Purposes []NetPurpose
}

// NetPurpose is the addresses of one purpose, for example PurposeCoreP2P.
type NetPurpose struct {
	ID      uint8
	Entries []NetEntry
}

// NetEntry is one address. Kind is EntryService or EntryDomain.
//
// A service entry has a Network and an Addr, 4 bytes for NetworkIPv4, 16 for
// NetworkIPv6, 32 for NetworkTORv3, 32 for NetworkI2P and 16 for NetworkCJDNS,
// the sizes Dash Core refuses a payload over. Other networks (TORv2 and ones
// Core does not know) are carried as they are; Core ignores such an entry. A domain entry has a
// Domain instead. The fields of the other kind must be left zero.
type NetEntry struct {
	Kind    uint8
	Network uint8
	Addr    []byte
	Domain  string
	Port    uint16
}

func (n *NetInfo) serialize(w io.Writer) error {
	if n.Version != netInfoVersion {
		return fmt.Errorf("%w: version %d", ErrUnsupportedNetInfo, n.Version)
	}
	if err := writeU8(w, n.Version); err != nil {
		return err
	}
	if err := writeCount(w, "netInfo purposes", len(n.Purposes)); err != nil {
		return err
	}
	for i := range n.Purposes {
		pu := &n.Purposes[i]
		if err := writeU8(w, pu.ID); err != nil {
			return err
		}
		if err := writeCount(w, "netInfo entries", len(pu.Entries)); err != nil {
			return err
		}
		for j := range pu.Entries {
			if err := pu.Entries[j].serialize(w); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *NetEntry) serialize(w io.Writer) error {
	switch e.Kind {
	case EntryService:
		if e.Domain != "" {
			return fmt.Errorf("%w: domain on a service entry", ErrUnsupportedNetInfo)
		}
		if err := e.checkAddr(); err != nil {
			return err
		}
		if err := writeU8(w, e.Kind); err != nil {
			return err
		}
		if err := writeU8(w, e.Network); err != nil {
			return err
		}
		if err := wire.WriteVarInt(w, 0, uint64(len(e.Addr))); err != nil {
			return err
		}
		if _, err := w.Write(e.Addr); err != nil {
			return err
		}
	case EntryDomain:
		if e.Network != 0 || len(e.Addr) != 0 {
			return fmt.Errorf("%w: address on a domain entry", ErrUnsupportedNetInfo)
		}
		if len(e.Domain) > maxDomainSize {
			return fmt.Errorf("%w: %d byte domain", ErrUnsupportedNetInfo, len(e.Domain))
		}
		if err := writeU8(w, e.Kind); err != nil {
			return err
		}
		if err := wire.WriteVarInt(w, 0, uint64(len(e.Domain))); err != nil {
			return err
		}
		if _, err := io.WriteString(w, e.Domain); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: entry kind %d", ErrUnsupportedNetInfo, e.Kind)
	}
	return writeU16BE(w, e.Port)
}

// checkAddr applies the address length Dash Core requires of the networks it
// knows how to size.
func (e *NetEntry) checkAddr() error {
	var want int
	switch e.Network {
	case NetworkIPv4:
		want = 4
	case NetworkIPv6, NetworkCJDNS:
		want = 16
	case NetworkTORv3, NetworkI2P:
		want = 32
	default:
		if len(e.Addr) > maxAddrSize {
			return fmt.Errorf("%w: %d byte address", ErrUnsupportedNetInfo, len(e.Addr))
		}
		return nil
	}
	if len(e.Addr) != want {
		return fmt.Errorf("%w: network %d with a %d byte address",
			ErrUnsupportedNetInfo, e.Network, len(e.Addr))
	}
	return nil
}

func (n *NetInfo) deserialize(r io.Reader) error {
	var err error
	if n.Version, err = readU8(r); err != nil {
		return err
	}
	if n.Version != netInfoVersion {
		return fmt.Errorf("%w: version %d", ErrUnsupportedNetInfo, n.Version)
	}
	np, err := readCount(r, "netInfo purposes")
	if err != nil {
		return err
	}
	if np > 0 {
		n.Purposes = make([]NetPurpose, np)
	}
	for i := range n.Purposes {
		pu := &n.Purposes[i]
		if pu.ID, err = readU8(r); err != nil {
			return err
		}
		ne, err := readCount(r, "netInfo entries")
		if err != nil {
			return err
		}
		if ne > 0 {
			pu.Entries = make([]NetEntry, ne)
		}
		for j := range pu.Entries {
			if err := pu.Entries[j].deserialize(r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *NetEntry) deserialize(r io.Reader) error {
	kind, err := readU8(r)
	if err != nil {
		return err
	}
	*e = NetEntry{Kind: kind}
	switch kind {
	case EntryService:
		if e.Network, err = readU8(r); err != nil {
			return err
		}
		size, err := wire.ReadVarInt(r, 0)
		if err != nil {
			return err
		}
		if size > maxAddrSize {
			return fmt.Errorf("%w: %d byte address", ErrUnsupportedNetInfo, size)
		}
		e.Addr = make([]byte, size)
		if _, err = io.ReadFull(r, e.Addr); err != nil {
			return err
		}
		if err = e.checkAddr(); err != nil {
			return err
		}
	case EntryDomain:
		size, err := wire.ReadVarInt(r, 0)
		if err != nil {
			return err
		}
		if size > maxDomainSize {
			return fmt.Errorf("%w: %d byte domain", ErrUnsupportedNetInfo, size)
		}
		name := make([]byte, size)
		if _, err = io.ReadFull(r, name); err != nil {
			return err
		}
		e.Domain = string(name)
	default:
		// The size of any other kind is unknown, so it cannot be skipped.
		return fmt.Errorf("%w: entry kind %d", ErrUnsupportedNetInfo, kind)
	}
	e.Port, err = readU16BE(r)
	return err
}
