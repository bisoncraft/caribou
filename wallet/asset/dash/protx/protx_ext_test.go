// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package protx

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

var purposeNames = map[uint8]string{
	PurposeCoreP2P:       "core_p2p",
	PurposePlatformP2P:   "platform_p2p",
	PurposePlatformHTTPS: "platform_https",
}

// netInfoAddresses formats a NetInfo the way dashd prints it, so a decode is
// compared to what Dash Core itself read from the same bytes.
func netInfoAddresses(t *testing.T, n *NetInfo) map[string][]string {
	t.Helper()
	got := map[string][]string{}
	for _, pu := range n.Purposes {
		name, ok := purposeNames[pu.ID]
		if !ok {
			t.Fatalf("no name for purpose %d", pu.ID)
		}
		for _, e := range pu.Entries {
			got[name] = append(got[name], net.JoinHostPort(net.IP(e.Addr).String(), strconv.Itoa(int(e.Port))))
		}
	}
	return got
}

// reversedHex is how dashd prints a 20 or 32 byte value it stores in wire
// order.
func reversedHex(b []byte) string {
	r := slices.Clone(b)
	slices.Reverse(r)
	return hex.EncodeToString(r)
}

// TestProRegTxExt decodes the version 3 registrations dashd built and checks
// every field against what dashd decoded from the same bytes, then that
// encoding gives the bytes back.
func TestProRegTxExt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		vector  string
		mnType  uint16
		payouts int
		shares  int
		// netInfoSize is the encoded size of the netInfo, which a changed
		// grammar would move.
		netInfoSize int
	}{
		{"shared, one core address", "regshared", TypeRegular, 0, 2, 13},
		{"shared, no address", "regshared_nonet", TypeRegular, 0, 2, 2},
		{"evonode, core and platform addresses", "regevo", TypeEvo, 1, 0, 35},
		{"evonode, ipv6 and several addresses", "regevo_multi", TypeEvo, 1, 0, 77},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v, tx := loadVector(t, tc.vector)
			d := v.Dashd.ProRegTx
			var p ProRegTx
			if err := Parse(tx.ExtraPayload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Version != VersionExtAddr || p.Version != d.Version {
				t.Fatalf("version %d, dashd %d", p.Version, d.Version)
			}
			if p.MNType != tc.mnType || p.MNType != d.Type {
				t.Fatalf("type %d, want %d, dashd %d", p.MNType, tc.mnType, d.Type)
			}
			if p.Collateral.Hash != mustHash(t, d.CollateralHash) || p.Collateral.Index != d.CollateralIndex {
				t.Fatalf("collateral %s, dashd %s:%d", p.Collateral, d.CollateralHash, d.CollateralIndex)
			}

			var size bytes.Buffer
			if err := p.NetInfo.serialize(&size); err != nil {
				t.Fatal(err)
			}
			if size.Len() != tc.netInfoSize {
				t.Errorf("netInfo is %d bytes, want %d", size.Len(), tc.netInfoSize)
			}
			if got, want := netInfoAddresses(t, &p.NetInfo), d.Addresses; len(got) != 0 || len(want) != 0 {
				if !reflect.DeepEqual(got, want) {
					t.Errorf("addresses %v, dashd %v", got, want)
				}
			}

			// A shared masternode has no owner key: its share owners hold them.
			if d.OwnerAddress == "" {
				if p.KeyIDOwner != [KeyIDSize]byte{} {
					t.Errorf("owner key id %x, dashd has no owner", p.KeyIDOwner)
				}
			} else if got := keyIDAddress(t, p.KeyIDOwner); got != d.OwnerAddress {
				t.Errorf("owner %s, dashd %s", got, d.OwnerAddress)
			}
			if got := keyIDAddress(t, p.KeyIDVoting); got != d.VotingAddress {
				t.Errorf("voting %s, dashd %s", got, d.VotingAddress)
			}
			if got := hex.EncodeToString(p.PubKeyOperator[:]); got != d.PubKeyOperator {
				t.Errorf("operator key %s, dashd %s", got, d.PubKeyOperator)
			}
			if p.OperatorReward != d.OperatorReward {
				t.Errorf("operator reward %d, dashd %d", p.OperatorReward, d.OperatorReward)
			}

			if len(p.Payouts) != tc.payouts || len(p.Payouts) != len(d.Payouts) {
				t.Fatalf("%d payouts, want %d, dashd %d", len(p.Payouts), tc.payouts, len(d.Payouts))
			}
			for i, po := range p.Payouts {
				if got := hex.EncodeToString(po.Script); got != d.Payouts[i].Script {
					t.Errorf("payout %d script %s, dashd %s", i, got, d.Payouts[i].Script)
				}
				if got := scriptAddress(t, po.Script); got != d.Payouts[i].Address {
					t.Errorf("payout %d address %s, dashd %s", i, got, d.Payouts[i].Address)
				}
				if po.Reward != d.Payouts[i].Reward {
					t.Errorf("payout %d reward %d, dashd %d", i, po.Reward, d.Payouts[i].Reward)
				}
			}

			if len(p.Shares) != tc.shares || len(p.Shares) != len(d.Shares) {
				t.Fatalf("%d shares, want %d, dashd %d", len(p.Shares), tc.shares, len(d.Shares))
			}
			for i, s := range p.Shares {
				ds := d.Shares[i]
				if s.Amount != ds.Amount {
					t.Errorf("share %d amount %d, dashd %d", i, s.Amount, ds.Amount)
				}
				if got := hex.EncodeToString(s.RefundScript); got != ds.RefundScript {
					t.Errorf("share %d refund script %s, dashd %s", i, got, ds.RefundScript)
				}
				if got := hex.EncodeToString(s.RewardScript); got != ds.RewardScript {
					t.Errorf("share %d reward script %s, dashd %s", i, got, ds.RewardScript)
				}
				if got := scriptAddress(t, s.RefundScript); got != ds.RefundAddress {
					t.Errorf("share %d refund address %s, dashd %s", i, got, ds.RefundAddress)
				}
				if got := keyIDAddress(t, s.KeyIDOwner); got != ds.OwnerAddress {
					t.Errorf("share %d owner %s, dashd %s", i, got, ds.OwnerAddress)
				}
				if s.JoinSig == [CompactSigSize]byte{} {
					t.Errorf("share %d has no join signature", i)
				}
			}
			if p.EarlyPeriodBlocks != d.EarlyPeriodBlocks || p.EarlyPenalty != d.EarlyPenalty {
				t.Errorf("early terms %d blocks %d duffs, dashd %d and %d",
					p.EarlyPeriodBlocks, p.EarlyPenalty, d.EarlyPeriodBlocks, d.EarlyPenalty)
			}

			if tc.mnType == TypeEvo {
				if got := reversedHex(p.PlatformNodeID[:]); got != d.PlatformNodeID {
					t.Errorf("platform node id %s, dashd %s", got, d.PlatformNodeID)
				}
			} else if p.PlatformNodeID != [KeyIDSize]byte{} {
				t.Errorf("platform node id %x on a regular masternode", p.PlatformNodeID)
			}

			if got := mustHash(t, d.InputsHash); p.InputsHash != got || InputsHash(tx.TxIn) != got {
				t.Errorf("inputs hash %s, dashd %s, from the inputs %s",
					p.InputsHash, got, InputsHash(tx.TxIn))
			}
			roundTrip(t, tx, &p)
		})
	}
}

// TestProRegTxDeserializeReusedStruct pins that decoding into a ProRegTx that
// held another payload gives exactly what a fresh one would. The two versions
// have fields the other lacks, so a decoder that only overwrote its own fields
// would keep the old ones, and the result could no longer be serialized (a
// caller scanning transactions into one struct would hit it).
func TestProRegTxDeserializeReusedStruct(t *testing.T) {
	t.Parallel()

	vectors := []string{"regfund", "regext", "regshared", "regshared_nonet", "regevo", "regevo_multi"}
	for _, first := range vectors {
		for _, second := range vectors {
			t.Run(first+" then "+second, func(t *testing.T) {
				t.Parallel()

				_, txFirst := loadVector(t, first)
				_, txSecond := loadVector(t, second)

				var reused, fresh ProRegTx
				if err := Parse(txFirst.ExtraPayload, &reused); err != nil {
					t.Fatal(err)
				}
				if err := Parse(txSecond.ExtraPayload, &reused); err != nil {
					t.Fatal(err)
				}
				if err := Parse(txSecond.ExtraPayload, &fresh); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(&reused, &fresh) {
					t.Fatalf("decoded into a reused struct:\n got %+v\nwant %+v", reused, fresh)
				}
				roundTrip(t, txSecond, &reused)
			})
		}
	}
}

// TestNetInfoDomainEntry pins the domain entry Dash Core v24 allows for Platform
// HTTPS: kind 2, a CompactSize length, the name and a big endian port. A
// decoder that only knew service entries would reject a valid EvoNode.
func TestNetInfoDomainEntry(t *testing.T) {
	t.Parallel()

	p := ProRegTx{
		Version: VersionExtAddr,
		MNType:  TypeEvo,
		NetInfo: NetInfo{Version: 1, Purposes: []NetPurpose{
			{ID: PurposeCoreP2P, Entries: []NetEntry{{Kind: EntryService, Network: NetworkIPv4, Addr: []byte{8, 8, 4, 4}, Port: 19000}}},
			{ID: PurposePlatformHTTPS, Entries: []NetEntry{{Kind: EntryDomain, Domain: "example.com", Port: 443}}},
		}},
	}
	var buf bytes.Buffer
	if err := p.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	wantEntry := append([]byte{EntryDomain, 11}, "example.com"...)
	wantEntry = append(wantEntry, 0x01, 0xbb)
	if !bytes.Contains(buf.Bytes(), wantEntry) {
		t.Fatalf("no domain entry %x in %x", wantEntry, buf.Bytes())
	}

	var got ProRegTx
	if err := Parse(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.NetInfo, p.NetInfo) {
		t.Fatalf("netInfo %+v, want %+v", got.NetInfo, p.NetInfo)
	}
	var again bytes.Buffer
	if err := got.Serialize(&again); err != nil || !bytes.Equal(again.Bytes(), buf.Bytes()) {
		t.Fatalf("round trip differs: %v\n got %x\nwant %x", err, again.Bytes(), buf.Bytes())
	}
}

// TestNetInfoDomainEntryRejects pins the limits on a domain entry and that an
// entry cannot carry the fields of the other kind, which would not survive
// encoding.
func TestNetInfoDomainEntryRejects(t *testing.T) {
	t.Parallel()

	withEntry := func(e NetEntry) *ProRegTx {
		return &ProRegTx{
			Version: VersionExtAddr,
			NetInfo: NetInfo{Version: 1, Purposes: []NetPurpose{{ID: PurposePlatformHTTPS, Entries: []NetEntry{e}}}},
		}
	}
	long := strings.Repeat("a", maxDomainSize+1)
	for name, p := range map[string]*ProRegTx{
		"domain over the limit":     withEntry(NetEntry{Kind: EntryDomain, Domain: long, Port: 443}),
		"address on a domain entry": withEntry(NetEntry{Kind: EntryDomain, Domain: "example.com", Network: NetworkIPv4, Addr: []byte{1, 2, 3, 4}}),
		"domain on a service entry": withEntry(NetEntry{Kind: EntryService, Network: NetworkIPv4, Addr: []byte{1, 2, 3, 4}, Domain: "example.com"}),
		"no kind":                   withEntry(NetEntry{Network: NetworkIPv4, Addr: []byte{1, 2, 3, 4}}),
		"unknown kind":              withEntry(NetEntry{Kind: 3, Domain: "example.com"}),
	} {
		if err := p.Serialize(io.Discard); !errors.Is(err, ErrUnsupportedNetInfo) {
			t.Errorf("%s: serialize %v, want ErrUnsupportedNetInfo", name, err)
		}
	}

	// A decoder refuses a domain over the limit before reading it.
	ok := withEntry(NetEntry{Kind: EntryDomain, Domain: "example.com", Port: 443})
	var buf bytes.Buffer
	if err := ok.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()
	at := bytes.Index(good, append([]byte{EntryDomain, 11}, "example.com"...))
	if at < 0 {
		t.Fatalf("no domain entry in %x", good)
	}
	over := append(bytes.Clone(good[:at+1]), 0xfd, byte(len(long)), 0x00) // CompactSize 254
	over = append(over, good[at+2:]...)
	if err := Parse(over, new(ProRegTx)); !errors.Is(err, ErrUnsupportedNetInfo) {
		t.Errorf("domain of %d bytes: %v, want ErrUnsupportedNetInfo", len(long), err)
	}
}

// TestListCountsAreOneByte pins that the payout, share and signature counts are
// one byte, as Dash Core writes them (uint8_t), not a CompactSize. The two are
// the same below 253; at 253 a CompactSize would be three bytes, so a decoder
// that read one would parse a payload Core reads differently.
func TestListCountsAreOneByte(t *testing.T) {
	t.Parallel()

	const n = 253

	reg := ProRegTx{Version: VersionExtAddr, NetInfo: NetInfo{Version: 1}, Shares: make([]Share, n)}
	var buf bytes.Buffer
	if err := reg.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	// 42 byte header, 2 byte empty netInfo, owner, operator, voting keys and
	// the operator reward, then the payout count and the share count.
	const sharesAt = 42 + 2 + KeyIDSize + OperatorPubKeySize + KeyIDSize + 2 + 1
	// Each share is its amount, two empty scripts, the owner key id and, after
	// all of them, its join signature; then the early terms, the inputs hash
	// and an empty signature follow. The byte at sharesAt is 0xfd under either
	// encoding (253, or the CompactSize marker), so the length is what tells a
	// one byte count from a three byte one.
	const wantLen = sharesAt + 1 + n*(8+1+1+KeyIDSize+CompactSigSize) + 4 + 8 + chainhash.HashSize + 1
	if buf.Bytes()[sharesAt] != n || buf.Len() != wantLen {
		t.Errorf("share count byte %#x at %d, %d bytes; want %#x and %d",
			buf.Bytes()[sharesAt], sharesAt, buf.Len(), n, wantLen)
	}
	var gotReg ProRegTx
	if err := Parse(buf.Bytes(), &gotReg); err != nil || len(gotReg.Shares) != n {
		t.Errorf("%d shares read back as %d: %v", n, len(gotReg.Shares), err)
	}

	dis := ProDisTx{Version: SharedVersion, Sigs: make([][CompactSigSize]byte, n)}
	buf.Reset()
	if err := dis.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	const sigsAt = 2 + chainhash.HashSize + 2
	if buf.Bytes()[sigsAt] != n || buf.Len() != sigsAt+1+n*CompactSigSize {
		t.Errorf("sig count byte %#x, %d bytes; want %#x and %d",
			buf.Bytes()[sigsAt], buf.Len(), n, sigsAt+1+n*CompactSigSize)
	}
	var gotDis ProDisTx
	if err := Parse(buf.Bytes(), &gotDis); err != nil || len(gotDis.Sigs) != n {
		t.Errorf("%d signatures read back as %d: %v", n, len(gotDis.Sigs), err)
	}
}

// TestProRegTxExtHashesRefuseVersion3 pins that the v2 hashes are not applied
// to a version 3 payload, whose digests are not pinned yet. A caller that
// needed one would otherwise sign over the wrong bytes.
func TestProRegTxExtHashesRefuseVersion3(t *testing.T) {
	t.Parallel()

	_, tx := loadVector(t, "regshared")
	var p ProRegTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PayloadHash(); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("PayloadHash: %v, want ErrUnsupportedVersion", err)
	}
	if _, err := p.CollateralMessage(regtestParams()); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("CollateralMessage: %v, want ErrUnsupportedVersion", err)
	}
}

// TestProRegTxExtShareCountIsNotCodecBusiness pins that the number of shares is
// validation, not decoding: Dash allows 2 to 8, and this package must still
// read and rewrite what it is given so a caller can report the real problem.
func TestProRegTxExtShareCountIsNotCodecBusiness(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 9, maxCount} {
		p := ProRegTx{Version: VersionExtAddr, NetInfo: NetInfo{Version: 1}, Shares: make([]Share, n)}
		var buf bytes.Buffer
		if err := p.Serialize(&buf); err != nil {
			t.Fatalf("%d shares: %v", n, err)
		}
		var got ProRegTx
		if err := Parse(buf.Bytes(), &got); err != nil {
			t.Fatalf("%d shares: %v", n, err)
		}
		if len(got.Shares) != n {
			t.Fatalf("%d shares read back as %d", n, len(got.Shares))
		}
	}
}

// TestProDisTx covers type 10: the unanimous dissolution has one signature per
// share and a unilateral standby one.
func TestProDisTx(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		vector string
		sigs   int
	}{{"dis", 2}, {"dis_standby", 1}} {
		t.Run(tc.vector, func(t *testing.T) {
			t.Parallel()

			v, tx := loadVector(t, tc.vector)
			d := v.Dashd.ProDis
			var p ProDisTx
			if err := Parse(tx.ExtraPayload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Version != SharedVersion || p.Version != d.Version {
				t.Errorf("version %d, dashd %d", p.Version, d.Version)
			}
			if p.ProTxHash != mustHash(t, d.ProTxHash) {
				t.Errorf("proTxHash %s, dashd %s", p.ProTxHash, d.ProTxHash)
			}
			if p.ActorIndex != d.ActorIndex {
				t.Errorf("actor %d, dashd %d", p.ActorIndex, d.ActorIndex)
			}
			if len(p.Sigs) != tc.sigs || len(p.Sigs) != d.SigCount {
				t.Errorf("%d signatures, want %d, dashd %d", len(p.Sigs), tc.sigs, d.SigCount)
			}
			roundTrip(t, tx, &p)
		})
	}
}

// TestProUpShareTxDecodeSigLength checks that a share update whose signature
// is not a 65 byte compact signature does not decode.
func TestProUpShareTxDecodeSigLength(t *testing.T) {
	t.Parallel()

	p := ProUpShareTx{Version: SharedVersion, Sig: make([]byte, CompactSigSize)}
	var buf bytes.Buffer
	if err := p.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()
	// The signature is last, behind a one byte length.
	at := len(good) - CompactSigSize - 1
	for _, n := range []int{0, CompactSigSize - 1, CompactSigSize + 1} {
		raw := append(append([]byte{}, good[:at]...), byte(n))
		raw = append(raw, make([]byte, n)...)
		var got ProUpShareTx
		if err := got.Deserialize(bytes.NewReader(raw)); !errors.Is(err, ErrFieldTooLong) {
			t.Errorf("%d byte signature: got %v, want ErrFieldTooLong", n, err)
		}
	}
}

func TestProUpShareTx(t *testing.T) {
	t.Parallel()

	v, tx := loadVector(t, "upshare")
	d := v.Dashd.ProUpShare
	var p ProUpShareTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Version != SharedVersion || p.Version != d.Version {
		t.Errorf("version %d, dashd %d", p.Version, d.Version)
	}
	if p.ProTxHash != mustHash(t, d.ProTxHash) {
		t.Errorf("proTxHash %s, dashd %s", p.ProTxHash, d.ProTxHash)
	}
	if p.ShareIndex != d.ShareIndex {
		t.Errorf("share %d, dashd %d", p.ShareIndex, d.ShareIndex)
	}
	if got := scriptAddress(t, p.ScriptReward); got != d.RewardAddress {
		t.Errorf("reward address %s, dashd %s", got, d.RewardAddress)
	}
	if got := mustHash(t, d.InputsHash); p.InputsHash != got || InputsHash(tx.TxIn) != got {
		t.Errorf("inputs hash %s, dashd %s, from the inputs %s", p.InputsHash, got, InputsHash(tx.TxIn))
	}
	if len(p.Sig) != CompactSigSize {
		t.Errorf("signature is %d bytes, want %d", len(p.Sig), CompactSigSize)
	}
	roundTrip(t, tx, &p)
}

func TestProUpSharedRegTx(t *testing.T) {
	t.Parallel()

	v, tx := loadVector(t, "upsharedreg")
	d := v.Dashd.ProUpSharedReg
	var p ProUpSharedRegTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Version != SharedVersion || p.Version != d.Version {
		t.Errorf("version %d, dashd %d", p.Version, d.Version)
	}
	if p.ProTxHash != mustHash(t, d.ProTxHash) {
		t.Errorf("proTxHash %s, dashd %s", p.ProTxHash, d.ProTxHash)
	}
	if got := keyIDAddress(t, p.KeyIDVoting); got != d.VotingAddress {
		t.Errorf("voting %s, dashd %s", got, d.VotingAddress)
	}
	if got := hex.EncodeToString(p.PubKeyOperator[:]); got != d.PubKeyOperator {
		t.Errorf("operator key %s, dashd %s", got, d.PubKeyOperator)
	}
	if got := mustHash(t, d.InputsHash); p.InputsHash != got || InputsHash(tx.TxIn) != got {
		t.Errorf("inputs hash %s, dashd %s, from the inputs %s", p.InputsHash, got, InputsHash(tx.TxIn))
	}
	if len(p.Sigs) != d.SigCount {
		t.Errorf("%d signatures, dashd %d", len(p.Sigs), d.SigCount)
	}
	roundTrip(t, tx, &p)
}

// TestParseNetInfo patches the netInfo of a vector to check what the decoder
// refuses, and what it must carry through unchanged. The netInfo of regshared
// starts at byte 42: version, purpose count, then purpose 0 (core P2P) with one
// entry of kind 1, network 1, a 4 byte address and a big endian port.
func TestParseNetInfo(t *testing.T) {
	t.Parallel()

	_, tx := loadVector(t, "regshared")
	good := tx.ExtraPayload
	const (
		at       = 42
		verAt    = at
		countAt  = at + 1
		kindAt   = at + 4
		netAt    = at + 5
		addrLen  = at + 6
		entryEnd = at + 13 // first byte after the single entry
	)
	if good[verAt] != 1 || good[countAt] != 1 || good[kindAt] != 1 || good[netAt] != NetworkIPv4 || good[addrLen] != 4 {
		t.Fatalf("vector layout moved: % x", good[at:entryEnd])
	}

	// patch returns good with the bytes [from, to) replaced.
	patch := func(from, to int, with ...byte) []byte {
		out := bytes.Clone(good[:from])
		out = append(out, with...)
		return append(out, good[to:]...)
	}
	// entry returns good with the single entry replaced.
	entry := func(with ...byte) []byte { return patch(kindAt, entryEnd, with...) }

	rejects := []struct {
		name    string
		payload []byte
		wantErr error
		// anyMsg accepts any wire.MessageError, for a CompactSize btcd
		// refuses.
		anyMsg bool
	}{
		{"netInfo version 2", patch(verAt, verAt+1, 2), ErrUnsupportedNetInfo, false},
		{"unknown entry kind", patch(kindAt, kindAt+1, 3), ErrUnsupportedNetInfo, false},
		{"ipv4 with 16 bytes", patch(addrLen, addrLen+1, 16), ErrUnsupportedNetInfo, false},
		{"ipv6 with 4 bytes", patch(netAt, netAt+1, NetworkIPv6), ErrUnsupportedNetInfo, false},
		{"torv3 with 4 bytes", patch(netAt, netAt+1, NetworkTORv3), ErrUnsupportedNetInfo, false},
		{"i2p with 4 bytes", patch(netAt, netAt+1, NetworkI2P), ErrUnsupportedNetInfo, false},
		{"cjdns with 4 bytes", patch(netAt, netAt+1, NetworkCJDNS), ErrUnsupportedNetInfo, false},
		{"address of 516 bytes", patch(addrLen, addrLen+1, 0xfd, 0x04, 0x02), ErrUnsupportedNetInfo, false},
		{"more purposes than the limit", patch(countAt, countAt+1, 0xfd, 0x00, 0x01), ErrFieldTooLong, false},
		{"non canonical purpose count", patch(countAt, countAt+1, 0xfd, 0x01, 0x00), nil, true},
		{"non canonical address length", patch(addrLen, addrLen+1, 0xfd, 0x04, 0x00), nil, true},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := Parse(tc.payload, new(ProRegTx))
			var msgErr *wire.MessageError
			switch {
			case err == nil:
				t.Fatal("accepted")
			case tc.anyMsg && !errors.As(err, &msgErr):
				t.Fatalf("error %v, want a wire message error", err)
			case !tc.anyMsg && !errors.Is(err, tc.wantErr):
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}
		})
	}

	// Anything else is carried as written: an address of a network this
	// package cannot size, an unusual purpose, port 0, a repeated purpose.
	carries := []struct {
		name    string
		payload []byte
	}{
		{"unknown network", entry(1, 3, 10, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 0x12, 0x34)},
		{"port zero", entry(1, 1, 4, 1, 2, 3, 4, 0, 0)},
		{"unknown purpose", patch(at+2, at+3, 9)},
		{"repeated purpose", patch(countAt, entryEnd, 2, 0, 1, 1, 1, 4, 1, 2, 3, 4, 0, 1, 0, 1, 1, 1, 4, 5, 6, 7, 8, 0, 2)},
	}
	for _, tc := range carries {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var p ProRegTx
			if err := Parse(tc.payload, &p); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err := p.Serialize(&buf); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), tc.payload) {
				t.Fatalf("round trip differs:\n got %x\nwant %x", buf.Bytes(), tc.payload)
			}
		})
	}
}

// TestNetEntryAddrSizes checks the address size of each network Dash Core sizes
// and that a network it does not know is carried as it is.
func TestNetEntryAddrSizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		network uint8
		size    int
		ok      bool
	}{
		{NetworkIPv4, 4, true}, {NetworkIPv4, 16, false},
		{NetworkIPv6, 16, true}, {NetworkIPv6, 4, false},
		{NetworkTORv3, 32, true}, {NetworkTORv3, 10, false}, {NetworkTORv3, 33, false},
		{NetworkI2P, 32, true}, {NetworkI2P, 16, false},
		{NetworkCJDNS, 16, true}, {NetworkCJDNS, 32, false},
		{3, 10, true}, {99, 7, true},
	}
	for _, tc := range tests {
		e := NetEntry{Kind: EntryService, Network: tc.network, Addr: make([]byte, tc.size), Port: 9999}
		err := e.serialize(io.Discard)
		if tc.ok && err != nil {
			t.Errorf("network %d with %d bytes: %v", tc.network, tc.size, err)
		}
		if !tc.ok && !errors.Is(err, ErrUnsupportedNetInfo) {
			t.Errorf("network %d with %d bytes: got %v, want ErrUnsupportedNetInfo", tc.network, tc.size, err)
		}
	}
}
