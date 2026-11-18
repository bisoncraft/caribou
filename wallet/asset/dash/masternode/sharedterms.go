// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"errors"
	"fmt"
	"net/netip"

	bls "github.com/consensys/gnark-crypto/ecc/bls12-381"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
)

// Limits of a shared masternode registration, from Dash Core v24.0.0.
const (
	// CollateralAmount is the total collateral of a masternode in duffs.
	CollateralAmount = 1000 * 1e8
	// MinShareAmount is the smallest contribution of one share in duffs.
	MinShareAmount = 100 * 1e8
	// MinShares and MaxShares bound the number of shares.
	MinShares = 2
	MaxShares = 8
	// MaxEarlyPeriodBlocks is the longest early dissolution period.
	MaxEarlyPeriodBlocks = 420480
	// DefaultEarlyPeriodBlocks is about 30 days of blocks.
	DefaultEarlyPeriodBlocks = 17280
	// MaxOperatorReward is 100% in basis points.
	MaxOperatorReward = 10000

	maxEntriesPerPurpose = 4
	mainnetCorePort      = 9999
)

// ErrInvalidTerms is wrapped by every error Terms.Validate returns. The text
// after it starts with the reason Dash Core would reject the registration with.
var ErrInvalidTerms = errors.New("invalid shared masternode terms")

// NetRules are the network rules for the address a masternode announces. Dash
// Core applies them to a regular masternode's Core P2P addresses.
type NetRules struct {
	// Mainnet pins the first Core P2P port to 9999.
	Mainnet bool
	// RequireRoutable rejects private, loopback and other unroutable
	// addresses. Dash Core waives it on regtest only.
	RequireRoutable bool
}

// Rules for the Dash networks.
var (
	MainnetRules = NetRules{Mainnet: true, RequireRoutable: true}
	TestnetRules = NetRules{RequireRoutable: true}
	RegtestRules = NetRules{}
)

// Terms are the registration terms the share owners agree on: everything of the
// registration except who funds it and the join signatures.
type Terms struct {
	// Shares is the share table. Each share's JoinSig is ignored.
	Shares []protx.Share
	// NetInfo is the masternode's addresses. It may be empty (the operator
	// announces the address later).
	NetInfo        protx.NetInfo
	PubKeyOperator [protx.OperatorPubKeySize]byte
	KeyIDVoting    [protx.KeyIDSize]byte
	// OperatorReward is the operator's share of the reward in basis points.
	OperatorReward    uint16
	EarlyPeriodBlocks uint32
	// EarlyPenalty is what a share owner who dissolves during the early
	// period forfeits, in duffs. It is 0 when EarlyPeriodBlocks is 0.
	EarlyPenalty int64
}

// DefaultEarlyPenalty is 5% of the smallest amount, which is below it as Dash
// Core requires.
func DefaultEarlyPenalty(amounts []int64) int64 {
	if len(amounts) == 0 {
		return 0
	}
	smallest := amounts[0]
	for _, a := range amounts[1:] {
		smallest = min(smallest, a)
	}
	return smallest / 20
}

// CoreP2PNetInfo returns the netInfo of a regular masternode announcing addrs
// as its Core P2P addresses. With none it is the empty netInfo.
func CoreP2PNetInfo(addrs ...netip.AddrPort) (protx.NetInfo, error) {
	ni := protx.NetInfo{Version: 1}
	if len(addrs) == 0 {
		return ni, nil
	}
	entries := make([]protx.NetEntry, 0, len(addrs))
	for _, ap := range addrs {
		a := ap.Addr().Unmap()
		e := protx.NetEntry{Kind: protx.EntryService, Port: ap.Port()}
		switch {
		case a.Is4():
			b := a.As4()
			e.Network, e.Addr = protx.NetworkIPv4, b[:]
		case a.Is6():
			b := a.As16()
			e.Network, e.Addr = protx.NetworkIPv6, b[:]
		default:
			return protx.NetInfo{}, fmt.Errorf("invalid address %v", ap)
		}
		entries = append(entries, e)
	}
	ni.Purposes = []protx.NetPurpose{{ID: protx.PurposeCoreP2P, Entries: entries}}
	return ni, nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTerms, fmt.Sprintf(format, args...))
}

// Validate checks the terms against the rules Dash Core v24.0.0 applies to a
// shared masternode registration, so a registration Core would reject is refused
// before anyone signs it. It does not check anything that needs the chain, such
// as whether a key or address is already registered.
func (t *Terms) Validate(rules NetRules) error {
	if n := len(t.Shares); n < MinShares || n > MaxShares {
		return invalid("bad-protx-shares-count: %d shares, want %d to %d", n, MinShares, MaxShares)
	}

	owners := make(map[[protx.KeyIDSize]byte]struct{}, len(t.Shares))
	refunds := make(map[string]struct{}, len(t.Shares))
	var sum int64
	smallest := t.Shares[0].Amount
	for i := range t.Shares {
		s := &t.Shares[i]
		if s.Amount < MinShareAmount || s.Amount > CollateralAmount {
			return invalid("bad-protx-shares-amount: share %d is %d duffs", i, s.Amount)
		}
		sum += s.Amount
		smallest = min(smallest, s.Amount)

		if s.KeyIDOwner == ([protx.KeyIDSize]byte{}) {
			return invalid("bad-protx-shares-key-null: share %d", i)
		}
		if _, dup := owners[s.KeyIDOwner]; dup {
			return invalid("bad-protx-shares-dup-key: share %d", i)
		}
		owners[s.KeyIDOwner] = struct{}{}

		if !payableScript(s.RefundScript) {
			return invalid("bad-protx-shares-payee: share %d", i)
		}
		if _, dup := refunds[string(s.RefundScript)]; dup {
			return invalid("bad-protx-shares-dup-refund: share %d", i)
		}
		refunds[string(s.RefundScript)] = struct{}{}

		// An empty reward script means the refund script is used.
		if len(s.RewardScript) != 0 && !payableScript(s.RewardScript) {
			return invalid("bad-protx-shares-payee: share %d", i)
		}
	}
	// Core rejects a payee that is the voting key or any share owner's key.
	for i := range t.Shares {
		for _, sc := range [][]byte{t.Shares[i].RefundScript, t.Shares[i].RewardScript} {
			if len(sc) != 25 {
				continue // empty, or P2SH which never matches a key hash
			}
			var h [protx.KeyIDSize]byte
			copy(h[:], sc[3:23])
			if _, reuse := owners[h]; reuse || h == t.KeyIDVoting {
				return invalid("bad-protx-shares-payee-reuse: share %d pays to a registration key", i)
			}
		}
	}
	if sum != CollateralAmount {
		return invalid("bad-protx-shares-amount-sum: shares total %d duffs, want %d", sum, int64(CollateralAmount))
	}

	if t.KeyIDVoting == ([protx.KeyIDSize]byte{}) {
		return invalid("bad-protx-key-null")
	}
	if t.OperatorReward > MaxOperatorReward {
		return invalid("bad-protx-operator-reward: %d", t.OperatorReward)
	}
	if err := validBLSKey(t.PubKeyOperator[:]); err != nil {
		return invalid("bad-protx-key-null: %v", err)
	}

	if t.EarlyPeriodBlocks > MaxEarlyPeriodBlocks {
		return invalid("bad-protx-shares-early-period: %d blocks, at most %d", t.EarlyPeriodBlocks, MaxEarlyPeriodBlocks)
	}
	if t.EarlyPenalty < 0 || t.EarlyPenalty >= smallest {
		return invalid("bad-protx-shares-penalty: %d, want 0 up to below %d", t.EarlyPenalty, smallest)
	}
	if t.EarlyPeriodBlocks == 0 && t.EarlyPenalty != 0 {
		return invalid("bad-protx-shares-penalty: a penalty needs an early period")
	}

	return t.validateNetInfo(rules)
}

// validBLSKey checks a basic-scheme operator key: a compressed G1 point that is
// on the curve, in the prime-order subgroup and not the identity.
func validBLSKey(b []byte) error {
	var p bls.G1Affine
	if _, err := p.SetBytes(b); err != nil {
		return err
	}
	if p.IsInfinity() {
		return errors.New("the identity")
	}
	return nil
}

// payableScript reports whether s is P2PKH or P2SH, the only scripts Dash Core
// lets a share pay to.
func payableScript(s []byte) bool {
	switch len(s) {
	case 25:
		return s[0] == 0x76 && s[1] == 0xa9 && s[2] == 0x14 && s[23] == 0x88 && s[24] == 0xac
	case 23:
		return s[0] == 0xa9 && s[1] == 0x14 && s[22] == 0x87
	}
	return false
}

// badPorts are the ports Dash Core refuses to announce (IsBadPort) besides
// 1 to 1023.
var badPorts = map[uint16]struct{}{
	1719: {}, 1720: {}, 1723: {}, 2049: {}, 3659: {}, 4045: {}, 5060: {}, 5061: {},
	6000: {}, 6566: {}, 6665: {}, 6666: {}, 6667: {}, 6668: {}, 6669: {}, 6697: {},
	8332: {}, 8333: {}, 10080: {}, 18332: {}, 18333: {},
}

func badPort(p uint16) bool {
	if p < 1024 {
		return true
	}
	_, bad := badPorts[p]
	return bad
}

// validateNetInfo applies Dash Core's rules for a regular masternode's netInfo.
// An empty one is valid. This wallet only builds Core P2P addresses, so a domain
// or another network is refused, and Core refuses Platform purposes on a regular
// masternode.
func (t *Terms) validateNetInfo(rules NetRules) error {
	ni := &t.NetInfo
	if ni.Version != 1 {
		return invalid("bad-protx-netinfo-version: %d", ni.Version)
	}
	if len(ni.Purposes) == 0 {
		return nil
	}

	prev := -1
	for i := range ni.Purposes {
		id := int(ni.Purposes[i].ID)
		if id <= prev {
			return invalid("bad-protx-netinfo-bad: purposes not in ascending order")
		}
		prev = id
		if id != protx.PurposeCoreP2P {
			return invalid("bad-protx-netinfo-bad: purpose %d on a regular masternode", id)
		}
	}
	core := ni.Purposes[0].Entries
	if len(core) == 0 {
		return invalid("bad-protx-netinfo-empty: no Core P2P address")
	}
	if len(core) > maxEntriesPerPurpose {
		return invalid("bad-protx-netinfo-maxlimit: %d addresses, at most %d", len(core), maxEntriesPerPurpose)
	}

	seen := make(map[string]struct{}, len(core))
	for i := range core {
		e := &core[i]
		if e.Kind != protx.EntryService {
			return invalid("bad-protx-netinfo-entry: entry %d is not a service address", i)
		}
		var ip netip.Addr
		switch {
		case e.Network == protx.NetworkIPv4 && len(e.Addr) == 4:
			ip = netip.AddrFrom4([4]byte(e.Addr))
		case e.Network == protx.NetworkIPv6 && len(e.Addr) == 16:
			ip = netip.AddrFrom16([16]byte(e.Addr))
		default:
			return invalid("bad-protx-netinfo-addr-type: entry %d is not IPv4 or IPv6", i)
		}
		if badPort(e.Port) {
			return invalid("bad-protx-netinfo-port: entry %d port %d", i, e.Port)
		}
		if !validAddr(ip) {
			return invalid("bad-protx-netinfo-addr: entry %d address %v", i, ip)
		}
		if rules.RequireRoutable && !routable(ip) {
			return invalid("bad-protx-netinfo-addr-unroutable: entry %d address %v is not routable", i, ip)
		}
		key := string([]byte{e.Network}) + string(e.Addr)
		if _, dup := seen[key]; dup {
			return invalid("bad-protx-dup-netinfo-entry: entry %d repeats an address", i)
		}
		seen[key] = struct{}{}
		if i == 0 {
			// Dash Core v24 needs the first Core address to be IPv4.
			if e.Network != protx.NetworkIPv4 {
				return invalid("bad-protx-netinfo-addr-type: the first address must be IPv4")
			}
			if rules.Mainnet && e.Port != mainnetCorePort {
				return invalid("bad-protx-netinfo-port: mainnet port must be %d", mainnetCorePort)
			}
		}
	}
	return nil
}

// validAddr follows CNetAddr::IsValid: it is false for the unspecified address,
// the IPv4 broadcast address and the documentation range 2001:db8::/32.
func validAddr(ip netip.Addr) bool {
	if ip.IsUnspecified() {
		return false
	}
	if ip.Is4() && ip.As4() == [4]byte{255, 255, 255, 255} {
		return false
	}
	return !prefix("2001:db8::/32").Contains(ip)
}

// nonRoutable are the ranges CNetAddr::IsRoutable excludes: for IPv4 RFC1918,
// RFC2544, RFC3927, RFC6598, RFC5737, 0.0.0.0/8 and loopback; for IPv6
// loopback, RFC4193, RFC4862, RFC4843 and RFC7343.
var nonRoutable = []netip.Prefix{
	prefix("10.0.0.0/8"), prefix("172.16.0.0/12"), prefix("192.168.0.0/16"),
	prefix("198.18.0.0/15"), prefix("169.254.0.0/16"), prefix("100.64.0.0/10"),
	prefix("192.0.2.0/24"), prefix("198.51.100.0/24"), prefix("203.0.113.0/24"),
	prefix("0.0.0.0/8"), prefix("127.0.0.0/8"),
	prefix("::1/128"), prefix("fc00::/7"), prefix("fe80::/64"),
	prefix("2001:10::/28"), prefix("2001:20::/28"),
}

func prefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

// routable follows CNetAddr::IsRoutable.
func routable(ip netip.Addr) bool {
	if !validAddr(ip) {
		return false
	}
	for _, p := range nonRoutable {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
