// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
)

// operatorKey is the operator key of the regtest shared registration Dash Core
// accepted (protx testdata regshared), so it is a valid basic-scheme key.
const operatorKey = "96b50d6f7e0ac45e448b235e55f2341f042cee47fb726fa51f78c062b40e31b0b61a18f802e561bf917892d9f17a61bd"

func p2pkh(b byte) []byte {
	s := []byte{0x76, 0xa9, 0x14}
	for range 20 {
		s = append(s, b)
	}
	return append(s, 0x88, 0xac)
}

func p2sh(b byte) []byte {
	s := []byte{0xa9, 0x14}
	for range 20 {
		s = append(s, b)
	}
	return append(s, 0x87)
}

func keyID(b byte) (id [20]byte) {
	for i := range id {
		id[i] = b
	}
	return
}

// validTerms is a 2-share 600/400 registration with a regtest address.
func validTerms(t *testing.T) *Terms {
	t.Helper()
	op, err := hex.DecodeString(operatorKey)
	if err != nil {
		t.Fatal(err)
	}
	ni, err := CoreP2PNetInfo(netip.MustParseAddrPort("127.0.0.1:19996"))
	if err != nil {
		t.Fatal(err)
	}
	tm := &Terms{
		Shares: []protx.Share{
			{Amount: 600e8, RefundScript: p2pkh(1), RewardScript: p2pkh(2), KeyIDOwner: keyID(0x11)},
			{Amount: 400e8, RefundScript: p2pkh(3), KeyIDOwner: keyID(0x22)},
		},
		NetInfo:           ni,
		KeyIDVoting:       keyID(0x33),
		OperatorReward:    100,
		EarlyPeriodBlocks: DefaultEarlyPeriodBlocks,
		EarlyPenalty:      DefaultEarlyPenalty([]int64{600e8, 400e8}),
	}
	copy(tm.PubKeyOperator[:], op)
	return tm
}

// second adds an IPv6 entry for addr to the Core P2P addresses of tm.
func second(tm *Terms, addr string) {
	e := tm.NetInfo.Purposes[0].Entries[0]
	e.Network, e.Addr = protx.NetworkIPv6, netip.MustParseAddr(addr).AsSlice()
	tm.NetInfo.Purposes[0].Entries = append(tm.NetInfo.Purposes[0].Entries, e)
}

// TestValidateAcceptsMainnetPortOffMainnet pins that the mainnet port is only
// pinned on mainnet: Dash Core's v3 netInfo does not refuse it elsewhere.
func TestValidateAcceptsMainnetPortOffMainnet(t *testing.T) {
	tm := validTerms(t)
	tm.NetInfo, _ = CoreP2PNetInfo(netip.MustParseAddrPort("8.8.8.8:9999"))
	if err := tm.Validate(TestnetRules); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAccepts(t *testing.T) {
	if err := validTerms(t).Validate(RegtestRules); err != nil {
		t.Fatal(err)
	}
	// No early period and no penalty, and an empty netInfo, are valid.
	tm := validTerms(t)
	tm.EarlyPeriodBlocks, tm.EarlyPenalty = 0, 0
	tm.NetInfo = protx.NetInfo{Version: 1}
	if err := tm.Validate(MainnetRules); err != nil {
		t.Fatal(err)
	}
}

// Each case breaks one Core rule, and the reason must appear in the error, so a
// regression in any check is caught by the case that names it.
func TestValidateRejects(t *testing.T) {
	public := func(tm *Terms, addr string) {
		tm.NetInfo, _ = CoreP2PNetInfo(netip.MustParseAddrPort(addr))
	}
	tests := []struct {
		name   string
		rules  NetRules
		mutate func(tm *Terms)
		reason string
	}{
		{"one share", RegtestRules, func(tm *Terms) { tm.Shares = tm.Shares[:1] }, "shares-count"},
		{"nine shares", RegtestRules, func(tm *Terms) {
			tm.Shares = nil
			for i := range 9 {
				tm.Shares = append(tm.Shares, protx.Share{Amount: 100e8, RefundScript: p2pkh(byte(i)), KeyIDOwner: keyID(byte(i + 1))})
			}
		}, "shares-count"},
		{"share below minimum", RegtestRules, func(tm *Terms) { tm.Shares[0].Amount, tm.Shares[1].Amount = 950e8, 50e8 }, "shares-amount:"},
		{"sum short", RegtestRules, func(tm *Terms) { tm.Shares[1].Amount = 300e8 }, "shares-amount-sum"},
		{"sum long", RegtestRules, func(tm *Terms) { tm.Shares[1].Amount = 500e8 }, "shares-amount-sum"},
		{"null owner", RegtestRules, func(tm *Terms) { tm.Shares[0].KeyIDOwner = [20]byte{} }, "shares-key-null"},
		{"duplicate owner", RegtestRules, func(tm *Terms) { tm.Shares[1].KeyIDOwner = tm.Shares[0].KeyIDOwner }, "shares-dup-key"},
		{"refund not payable", RegtestRules, func(tm *Terms) { tm.Shares[0].RefundScript = []byte{0x6a} }, "shares-payee:"},
		{"empty refund", RegtestRules, func(tm *Terms) { tm.Shares[0].RefundScript = nil }, "shares-payee:"},
		{"duplicate refund", RegtestRules, func(tm *Terms) { tm.Shares[1].RefundScript = tm.Shares[0].RefundScript }, "shares-dup-refund"},
		{"reward not payable", RegtestRules, func(tm *Terms) { tm.Shares[0].RewardScript = []byte{0x6a} }, "shares-payee:"},
		{"null voting key", RegtestRules, func(tm *Terms) { tm.KeyIDVoting = [20]byte{} }, "protx-key-null"},
		{"refund pays the voting key", RegtestRules, func(tm *Terms) { tm.Shares[0].RefundScript = p2pkh(0x33) }, "payee-reuse"},
		{"refund pays another owner", RegtestRules, func(tm *Terms) { tm.Shares[0].RefundScript = p2pkh(0x22) }, "payee-reuse"},
		{"refund pays its own owner", RegtestRules, func(tm *Terms) { tm.Shares[0].RefundScript = p2pkh(0x11) }, "payee-reuse"},
		{"reward pays the voting key", RegtestRules, func(tm *Terms) { tm.Shares[0].RewardScript = p2pkh(0x33) }, "payee-reuse"},
		{"unspecified address", RegtestRules, func(tm *Terms) { public(tm, "0.0.0.0:19996") }, "netinfo-addr:"},
		{"documentation ipv6 second entry", RegtestRules, func(tm *Terms) {
			e := tm.NetInfo.Purposes[0].Entries[0]
			e.Network, e.Addr = protx.NetworkIPv6, netip.MustParseAddr("2001:db8::1").AsSlice()
			tm.NetInfo.Purposes[0].Entries = append(tm.NetInfo.Purposes[0].Entries, e)
		}, "netinfo-addr:"},
		{"ipv4 mapped ipv6 second entry", MainnetRules, func(tm *Terms) { public(tm, "8.8.8.8:9999"); second(tm, "::ffff:8.8.4.4") }, "netinfo-addr:"},
		{"ipv4 mapped private ipv6 second entry", MainnetRules, func(tm *Terms) { public(tm, "8.8.8.8:9999"); second(tm, "::ffff:10.0.0.1") }, "netinfo-addr:"},
		{"internal ipv6 range on regtest", RegtestRules, func(tm *Terms) { second(tm, "fd6b:88c0:8724::1") }, "netinfo-addr:"},
		{"tor v2 ipv6 range on regtest", RegtestRules, func(tm *Terms) { second(tm, "fd87:d87e:eb43::1") }, "netinfo-addr:"},
		{"cgnat on mainnet", MainnetRules, func(tm *Terms) { public(tm, "100.64.1.1:9999") }, "unroutable"},
		{"benchmarking range on testnet", TestnetRules, func(tm *Terms) { public(tm, "198.18.0.1:19999") }, "unroutable"},
		{"operator reward over 100%", RegtestRules, func(tm *Terms) { tm.OperatorReward = 10001 }, "operator-reward"},
		{"operator key zero", RegtestRules, func(tm *Terms) { tm.PubKeyOperator = [48]byte{} }, "protx-key-null"},
		{"operator key identity", RegtestRules, func(tm *Terms) { tm.PubKeyOperator = [48]byte{0xc0} }, "protx-key-null"},
		{"operator key off curve", RegtestRules, func(tm *Terms) { tm.PubKeyOperator[47] ^= 1 }, "protx-key-null"},
		{"early period too long", RegtestRules, func(tm *Terms) { tm.EarlyPeriodBlocks = MaxEarlyPeriodBlocks + 1 }, "shares-early-period"},
		{"negative penalty", RegtestRules, func(tm *Terms) { tm.EarlyPenalty = -1 }, "shares-penalty"},
		{"penalty at smallest share", RegtestRules, func(tm *Terms) { tm.EarlyPenalty = 400e8 }, "shares-penalty"},
		{"penalty without period", RegtestRules, func(tm *Terms) { tm.EarlyPeriodBlocks = 0 }, "shares-penalty"},
		{"netinfo version", RegtestRules, func(tm *Terms) { tm.NetInfo.Version = 2 }, "netinfo-version"},
		{"platform purpose", RegtestRules, func(tm *Terms) {
			tm.NetInfo.Purposes = append(tm.NetInfo.Purposes, protx.NetPurpose{ID: protx.PurposePlatformP2P, Entries: tm.NetInfo.Purposes[0].Entries})
		}, "purpose 1"},
		{"purposes out of order", RegtestRules, func(tm *Terms) {
			p := tm.NetInfo.Purposes[0]
			tm.NetInfo.Purposes = []protx.NetPurpose{p, p}
		}, "ascending"},
		{"empty core purpose", RegtestRules, func(tm *Terms) { tm.NetInfo.Purposes[0].Entries = nil }, "netinfo-empty"},
		{"five addresses", RegtestRules, func(tm *Terms) {
			e := tm.NetInfo.Purposes[0].Entries[0]
			tm.NetInfo.Purposes[0].Entries = nil
			for i := range 5 {
				e.Addr = []byte{10, 0, 0, byte(i)}
				tm.NetInfo.Purposes[0].Entries = append(tm.NetInfo.Purposes[0].Entries, e)
			}
		}, "netinfo-maxlimit"},
		{"domain entry", RegtestRules, func(tm *Terms) {
			tm.NetInfo.Purposes[0].Entries[0] = protx.NetEntry{Kind: protx.EntryDomain, Domain: "a.b", Port: 19996}
		}, "netinfo-entry"},
		{"onion entry", RegtestRules, func(tm *Terms) { tm.NetInfo.Purposes[0].Entries[0].Network = 4 }, "netinfo-addr-type"},
		{"short ipv4", RegtestRules, func(tm *Terms) { tm.NetInfo.Purposes[0].Entries[0].Addr = []byte{1, 2, 3} }, "netinfo-addr-type"},
		{"port zero", RegtestRules, func(tm *Terms) { tm.NetInfo.Purposes[0].Entries[0].Port = 0 }, "netinfo-port"},
		{"privileged port", RegtestRules, func(tm *Terms) { tm.NetInfo.Purposes[0].Entries[0].Port = 1023 }, "netinfo-port"},
		{"bad port 8333", RegtestRules, func(tm *Terms) { tm.NetInfo.Purposes[0].Entries[0].Port = 8333 }, "netinfo-port"},
		{"duplicate address", RegtestRules, func(tm *Terms) {
			e := tm.NetInfo.Purposes[0].Entries[0]
			tm.NetInfo.Purposes[0].Entries = append(tm.NetInfo.Purposes[0].Entries, e)
		}, "dup-netinfo-entry"},
		{"first address ipv6", RegtestRules, func(tm *Terms) { public(tm, "[2606:4700::1]:19996") }, "netinfo-addr-type"},
		{"private address on testnet", TestnetRules, func(tm *Terms) {}, "unroutable"},
		{"rfc1918 on mainnet", MainnetRules, func(tm *Terms) { public(tm, "192.168.1.2:9999") }, "unroutable"},
		{"mainnet wrong port", MainnetRules, func(tm *Terms) { public(tm, "8.8.8.8:19996") }, "netinfo-port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := validTerms(t)
			tt.mutate(tm)
			err := tm.Validate(tt.rules)
			if !errors.Is(err, ErrInvalidTerms) {
				t.Fatalf("want ErrInvalidTerms, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("want reason %q, got %v", tt.reason, err)
			}
		})
	}
}

// Mainnet's one valid shape: routable, port 9999.
func TestValidateMainnetAddress(t *testing.T) {
	tm := validTerms(t)
	tm.NetInfo, _ = CoreP2PNetInfo(netip.MustParseAddrPort("8.8.8.8:9999"))
	if err := tm.Validate(MainnetRules); err != nil {
		t.Fatal(err)
	}
}

// P2SH refund and reward scripts are accepted as Core does.
func TestValidateP2SH(t *testing.T) {
	tm := validTerms(t)
	tm.Shares[0].RefundScript, tm.Shares[0].RewardScript = p2sh(7), p2sh(8)
	if err := tm.Validate(RegtestRules); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultEarlyPenalty(t *testing.T) {
	if got := DefaultEarlyPenalty([]int64{600e8, 400e8, 500e8}); got != 20e8 {
		t.Fatalf("got %d", got)
	}
	if DefaultEarlyPenalty(nil) != 0 {
		t.Fatal("empty must be 0")
	}
	// Must stay below the smallest share for any valid share.
	if p := DefaultEarlyPenalty([]int64{MinShareAmount}); p >= MinShareAmount {
		t.Fatalf("penalty %d not below smallest", p)
	}
}

func TestCoreP2PNetInfo(t *testing.T) {
	ni, err := CoreP2PNetInfo()
	if err != nil || len(ni.Purposes) != 0 || ni.Version != 1 {
		t.Fatalf("empty: %+v %v", ni, err)
	}
	ni, err = CoreP2PNetInfo(netip.MustParseAddrPort("1.2.3.4:9999"), netip.MustParseAddrPort("[::ffff:5.6.7.8]:9999"))
	if err != nil {
		t.Fatal(err)
	}
	es := ni.Purposes[0].Entries
	// An IPv4-mapped address is announced as IPv4, as Core would read it.
	if len(es) != 2 || es[1].Network != protx.NetworkIPv4 || len(es[1].Addr) != 4 {
		t.Fatalf("entries %+v", es)
	}
	if _, err := CoreP2PNetInfo(netip.AddrPort{}); err == nil {
		t.Fatal("invalid address accepted")
	}
}
