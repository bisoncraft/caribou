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
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
)

// loadVectorTx reads a transaction Dash Core made, from the protx test data.
func loadVectorTx(t *testing.T, name string) (*bisonwire.Tx, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "protx", "testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Hex string }
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(v.Hex)
	if err != nil {
		t.Fatal(err)
	}
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	return tx, raw
}

// coreMN is the masternode Dash Core registered in the regshared vector, which
// the dis and dis_standby vectors dissolve. It is registered at height 1000
// with the vector's early period of 10 blocks.
func coreMN(t *testing.T) *SharedMN {
	t.Helper()
	tx, _ := loadVectorTx(t, "regshared")
	var pr protx.ProRegTx
	if err := protx.Parse(tx.ExtraPayload, &pr); err != nil {
		t.Fatal(err)
	}
	return &SharedMN{
		ProTxHash:         tx.TxHash(),
		Collateral:        wire.OutPoint{Hash: tx.TxHash(), Index: pr.Collateral.Index},
		Shares:            pr.Shares,
		RegisteredHeight:  1000,
		EarlyPeriodBlocks: pr.EarlyPeriodBlocks,
		EarlyPenalty:      pr.EarlyPenalty,
	}
}

func vectorSigs(t *testing.T, tx *bisonwire.Tx) (protx.ProDisTx, []ShareSig) {
	t.Helper()
	var pd protx.ProDisTx
	if err := protx.Parse(tx.ExtraPayload, &pd); err != nil {
		t.Fatal(err)
	}
	var sigs []ShareSig
	for i, s := range pd.Sigs {
		idx := i
		if len(pd.Sigs) == 1 {
			idx = int(pd.ActorIndex)
		}
		sigs = append(sigs, ShareSig{idx, base64.StdEncoding.EncodeToString(s[:])})
	}
	return pd, sigs
}

func feeOf(m *SharedMN, tx *bisonwire.Tx) int64 {
	out := int64(0)
	for _, o := range tx.TxOut {
		out += o.Value
	}
	return m.total() - out
}

// TestDissolutionMatchesCore rebuilds the two dissolutions dashd made of the
// regshared masternode, a unanimous one and a unilateral one that paid the
// early penalty, and requires the same bytes. Adding the signatures Core's wallet made must then verify and
// give back Core's transaction exactly.
func TestDissolutionMatchesCore(t *testing.T) {
	m := coreMN(t)
	for _, tt := range []struct {
		name      string
		unanimous bool
		penalty   int64
	}{{"dis", true, 0}, {"dis_standby", false, 5e8}} {
		t.Run(tt.name, func(t *testing.T) {
			core, coreRaw := loadVectorTx(t, tt.name)
			pd, sigs := vectorSigs(t, core)
			d, err := NewDissolution(m, DissolveParams{
				Actor: int(pd.ActorIndex), Unanimous: tt.unanimous, Penalty: tt.penalty, Fee: feeOf(m, core),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := d.AddSigs(sigs); err != nil {
				t.Fatalf("Core's signatures do not verify against our digest: %v", err)
			}
			if err := d.Complete(); err != nil {
				t.Fatal(err)
			}
			got, err := d.Raw()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, coreRaw) {
				t.Fatalf("built\n%x\nCore\n%x", got, coreRaw)
			}
		})
	}
}

// The size is exact, so the fee for a rate is exact: the standby and the
// unanimous dissolution Core made are 102 and 167 bytes of payload.
func TestFeeFor(t *testing.T) {
	m := coreMN(t)
	for _, tt := range []struct {
		name      string
		unanimous bool
	}{{"dis", true}, {"dis_standby", false}} {
		_, coreRaw := loadVectorTx(t, tt.name)
		fee, err := FeeFor(m, tt.unanimous, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if fee != int64(len(coreRaw)) {
			t.Fatalf("%s: fee %d at 1 duff a byte, Core's transaction is %d bytes", tt.name, fee, len(coreRaw))
		}
	}
	if _, err := FeeFor(m, true, -1); err == nil {
		t.Fatal("a negative fee rate was accepted")
	}
	if fee, _ := FeeFor(m, true, 1<<40); fee != MaxDissolutionFee {
		t.Fatalf("fee %d is not capped", fee)
	}
}

func TestRequiredPenalty(t *testing.T) {
	m := &SharedMN{RegisteredHeight: 100, EarlyPeriodBlocks: 10, EarlyPenalty: 7}
	for _, tt := range []struct {
		height    int32
		unanimous bool
		want      int64
	}{
		{100, false, 7}, {109, false, 7}, // inside the early period
		{110, false, 0}, {5000, false, 0}, // from registration plus the period
		{105, true, 0}, // a unanimous one never pays
	} {
		if got := m.RequiredPenalty(tt.height, tt.unanimous); got != tt.want {
			t.Fatalf("height %d unanimous %v: %d, want %d", tt.height, tt.unanimous, got, tt.want)
		}
	}
	if m.StandbyValidFrom() != 110 {
		t.Fatalf("valid from %d", m.StandbyValidFrom())
	}
	// Validity is monotone: a penalty that is not required at a height is
	// not required later.
	prev := m.RequiredPenalty(0, false)
	for h := int32(0); h < 200; h++ {
		p := m.RequiredPenalty(h, false)
		if p > prev {
			t.Fatalf("penalty rose at height %d", h)
		}
		prev = p
	}
}

// The standby of the Core vector is refused inside the early period and
// accepted after it, as dashd was seen to do (bad-prodis-penalty-floor).
func TestStandbyEarlyPeriod(t *testing.T) {
	m := coreMN(t)
	d, err := NewDissolution(m, DissolveParams{Actor: 0, Fee: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckAt(m.RegisteredHeight + int32(m.EarlyPeriodBlocks) - 1); err == nil || !strings.Contains(err.Error(), "penalty-floor") {
		t.Fatalf("standby valid inside the early period: %v", err)
	}
	if err := d.CheckAt(m.StandbyValidFrom()); err != nil {
		t.Fatal(err)
	}
	// Paying the penalty makes it valid inside the period too.
	d2, err := NewDissolution(m, DissolveParams{Actor: 0, Penalty: m.EarlyPenalty, Fee: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.CheckAt(m.RegisteredHeight); err != nil {
		t.Fatal(err)
	}
}

// Dash Core's check, case by case: each mutation breaks one rule.
func TestCheckRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(d *Dissolution)
		height int32
		reason string
	}{
		{"another masternode", func(d *Dissolution) { d.pd.ProTxHash[0] ^= 1 }, math.MaxInt32, "prodis-hash"},
		{"two inputs", func(d *Dissolution) { d.tx.TxIn = append(d.tx.TxIn, d.tx.TxIn[0]) }, math.MaxInt32, "prodis-input"},
		{"another collateral", func(d *Dissolution) { d.tx.TxIn[0].PreviousOutPoint.Index++ }, math.MaxInt32, "prodis-input"},
		{"signature script", func(d *Dissolution) { d.tx.TxIn[0].SignatureScript = []byte{1} }, math.MaxInt32, "prodis-input"},
		{"actor out of range", func(d *Dissolution) { d.pd.ActorIndex = 5 }, math.MaxInt32, "prodis-actor"},
		{"three signatures", func(d *Dissolution) { d.pd.Sigs = make([][65]byte, 3) }, math.MaxInt32, "sig-count"},
		{"too many outputs", func(d *Dissolution) { d.tx.TxOut = append(d.tx.TxOut, d.tx.TxOut[0], d.tx.TxOut[0]) }, math.MaxInt32, "payee-count"},
		{"no outputs", func(d *Dissolution) { d.tx.TxOut = nil }, math.MaxInt32, "payee-count"},
		{"payee is not the refund script", func(d *Dissolution) { d.tx.TxOut[0].PkScript = p2pkh(9) }, math.MaxInt32, "prodis-payee"},
		{"actor output to another script", func(d *Dissolution) { d.tx.TxOut[1].PkScript = p2pkh(9) }, math.MaxInt32, "prodis-payee"},
		{"zero actor output", func(d *Dissolution) { d.tx.TxOut[1].Value = 0 }, math.MaxInt32, "actor-output-zero"},
		{"fee over the cap", func(d *Dissolution) { d.tx.TxOut[1].Value -= MaxDissolutionFee }, math.MaxInt32, "prodis-fee"},
		{"outputs exceed the collateral", func(d *Dissolution) { d.tx.TxOut[1].Value += 1001 }, math.MaxInt32, "prodis-fee"},
		{"unilateral bonus over the penalty", func(d *Dissolution) {
			d.tx.TxOut[0].Value += d.mn.EarlyPenalty + 1
			d.tx.TxOut[1].Value -= d.mn.EarlyPenalty + 1
		}, math.MaxInt32, "prodis-bonus"},
		{"early without the penalty", func(d *Dissolution) {}, 1000, "penalty-floor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := coreMN(t)
			d, err := NewDissolution(m, DissolveParams{Actor: 0, Fee: 1000})
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(d)
			err = d.CheckAt(tt.height)
			if !errors.Is(err, ErrInvalidDissolution) || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("want %q, got %v", tt.reason, err)
			}
		})
	}
}

func TestNewDissolutionRejects(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    DissolveParams
	}{
		{"actor out of range", DissolveParams{Actor: 2}},
		{"negative actor", DissolveParams{Actor: -1}},
		{"fee over the cap", DissolveParams{Fee: MaxDissolutionFee + 1}},
		{"negative fee", DissolveParams{Fee: -1}},
		{"penalty over the early penalty", DissolveParams{Penalty: 5e8 + 1}},
		{"penalty on a unanimous one", DissolveParams{Unanimous: true, Penalty: 1}},
		{"negative penalty", DissolveParams{Penalty: -1}},
		{"nothing left for the actor", DissolveParams{Penalty: 5e8, Fee: 0}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := coreMN(t)
			if tt.name == "nothing left for the actor" {
				m.Shares[0].Amount, m.Shares[1].Amount = 4e8, 996e8
				m.EarlyPenalty = 5e8
			}
			if _, err := NewDissolution(m, tt.p); !errors.Is(err, ErrInvalidDissolution) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// The preview is what the owner is shown before it signs a unilateral
// dissolution: the other shares get the penalty, the actor pays it and the fee.
func TestPreview(t *testing.T) {
	m := coreMN(t)
	pv, err := m.Preview(DissolveParams{Actor: 0, Penalty: 5e8, Fee: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Outputs[1] != 500e8+5e8 || pv.Outputs[0] != 500e8-5e8-1000 || pv.Penalty != 5e8 {
		t.Fatalf("%+v", pv)
	}
	// With three shares the penalty is split by principal with no duff lost.
	m3 := &SharedMN{
		Shares: []protx.Share{
			{Amount: 500e8, RefundScript: p2pkh(1)}, {Amount: 300e8, RefundScript: p2pkh(2)}, {Amount: 200e8, RefundScript: p2pkh(3)},
		},
		EarlyPeriodBlocks: 10, EarlyPenalty: 1_000_000_007,
	}
	pv, err = m3.Preview(DissolveParams{Actor: 0, Penalty: 1_000_000_007, Fee: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if got := pv.Outputs[1] + pv.Outputs[2] - 500e8; got != 1_000_000_007 {
		t.Fatalf("the others got %d of the penalty", got)
	}
	var sum int64
	for _, o := range pv.Outputs {
		sum += o
	}
	if sum != 1000e8-1000 {
		t.Fatalf("outputs sum to %d", sum)
	}
}

// threeOwners is a registered 500/300/200 masternode whose owner keys we hold.
func threeOwners(t *testing.T) (*SharedMN, []*btcec.PrivateKey) {
	t.Helper()
	var keys []*btcec.PrivateKey
	m := &SharedMN{
		ProTxHash:         chainhash.Hash{7},
		Collateral:        wire.OutPoint{Hash: chainhash.Hash{7}, Index: 1},
		RegisteredHeight:  100,
		EarlyPeriodBlocks: 10,
		EarlyPenalty:      10e8,
	}
	for i, amt := range []int64{500e8, 300e8, 200e8} {
		k := testKey(byte(0x50 + i))
		keys = append(keys, k)
		m.Shares = append(m.Shares, protx.Share{Amount: amt, RefundScript: p2pkh(byte(i + 1)), KeyIDOwner: idFor(k)})
	}
	return m, keys
}

// TestUnanimousFlow runs the exchange of a unanimous dissolution among three
// owners: the actor builds it, each of the others reviews and signs, and the
// signatures are added to the one transaction.
func TestUnanimousFlow(t *testing.T) {
	m, keys := threeOwners(t)
	d, err := NewDissolution(m, DissolveParams{Actor: 1, Unanimous: true, Fee: 5000})
	if err != nil {
		t.Fatal(err)
	}
	cur := d.tx
	var sigs []ShareSig
	for i, k := range keys {
		rv, err := ReviewDissolution(cur, m, i, 5000)
		if err != nil {
			t.Fatalf("share %d: %v", i, err)
		}
		s, err := rv.Sign(k, i)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	if err := d.Complete(); err == nil {
		t.Fatal("complete with no signatures")
	}
	if err := d.AddSigs(sigs); err != nil {
		t.Fatal(err)
	}
	if err := d.Complete(); err != nil {
		t.Fatal(err)
	}
	// Anyone can re-review the finished transaction.
	blob, _ := d.Blob()
	raw, _ := json.Marshal(blob)
	tx, got, err := ParseBlob(raw)
	if err != nil || len(got) != 3 {
		t.Fatalf("blob: %d sigs, %v", len(got), err)
	}
	if _, err := ReviewDissolution(tx, m, 0, 5000); err != nil {
		t.Fatal(err)
	}
}

// ReviewDissolution is the guard on a unanimous dissolution, where the other
// owners' signatures cover the outputs: it must refuse anything that gives one
// of them less than the principal, or hands out more than agreed.
func TestReviewDissolutionRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(d *Dissolution)
		maxFee int64
		reason string
	}{
		{"a share is short", func(d *Dissolution) { d.tx.TxOut[0].Value -= 1000 }, 5000, "penalty-floor"},
		{"a share gets a bonus", func(d *Dissolution) { d.tx.TxOut[0].Value += 1000; d.tx.TxOut[2].Value -= 1000 }, 5000, "exactly its principal"},
		{"the fee is over what was agreed", func(d *Dissolution) { d.tx.TxOut[2].Value -= 1 }, 5000, "over the"},
		{"unilateral", func(d *Dissolution) { d.pd.Sigs = d.pd.Sigs[:1]; _ = d.sync() }, 5000, "not a unanimous"},
		{"payee is not a refund script", func(d *Dissolution) { d.tx.TxOut[0].PkScript = p2pkh(9) }, 5000, "prodis-payee"},
		{"another masternode", func(d *Dissolution) { d.pd.ProTxHash[0] ^= 1; _ = d.sync() }, 5000, "prodis-hash"},
		{"relative lock", func(d *Dissolution) { d.tx.TxIn[0].Sequence = 1 }, 5000, "final sequence"},
		{"lock time", func(d *Dissolution) { d.tx.LockTime = 5 }, 5000, "not a dissolution"},
		{"a forged signature", func(d *Dissolution) { d.pd.Sigs[0][0], d.pd.Sigs[0][1] = 31, 1; _ = d.sync() }, 5000, "signature 0"},
		{"agreed fee over the cap", func(d *Dissolution) {}, MaxDissolutionFee + 1, "maximum fee"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := threeOwners(t)
			d, err := NewDissolution(m, DissolveParams{Actor: 1, Unanimous: true, Fee: 5000})
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(d)
			if err := d.sync(); err != nil && tt.name != "unilateral" {
				t.Fatal(err)
			}
			_, err = ReviewDissolution(d.tx, m, 0, tt.maxFee)
			if !errors.Is(err, ErrInvalidDissolution) || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("want %q, got %v", tt.reason, err)
			}
		})
	}
}

func TestReviewDissolutionDoesNotModify(t *testing.T) {
	m, keys := threeOwners(t)
	d, _ := NewDissolution(m, DissolveParams{Actor: 1, Unanimous: true, Fee: 5000})
	before, _ := d.Raw()
	rv, err := ReviewDissolution(d.tx, m, 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := rv.Sign(keys[0], 0)
	if err := rv.AddSigs([]ShareSig{s}); err != nil {
		t.Fatal(err)
	}
	if after, _ := d.Raw(); !bytes.Equal(before, after) {
		t.Fatal("the reviewed transaction was modified")
	}
}

func TestDissolutionSigning(t *testing.T) {
	m, keys := threeOwners(t)

	t.Run("only the actor signs a unilateral one", func(t *testing.T) {
		d, _ := NewDissolution(m, DissolveParams{Actor: 1, Fee: 1000})
		if _, err := d.Sign(keys[0], 0); err == nil {
			t.Fatal("a non-actor signed a unilateral dissolution")
		}
		if _, err := d.Sign(keys[1], 1); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the key must be the share's", func(t *testing.T) {
		d, _ := NewDissolution(m, DissolveParams{Actor: 1, Unanimous: true, Fee: 1000})
		if _, err := d.Sign(keys[0], 1); err == nil {
			t.Fatal("signed as another share")
		}
		if _, err := d.Sign(keys[0], 9); err == nil {
			t.Fatal("signed as a share that does not exist")
		}
	})
	t.Run("a bad signature adds nothing", func(t *testing.T) {
		d, _ := NewDissolution(m, DissolveParams{Actor: 1, Unanimous: true, Fee: 1000})
		good, _ := d.Sign(keys[0], 0)
		wrong := good
		wrong.ShareIndex = 2
		before, _ := d.Raw()
		for _, bad := range []ShareSig{wrong, {ShareIndex: 7, Signature: good.Signature}, {ShareIndex: 1, Signature: "!!"}} {
			if err := d.AddSigs([]ShareSig{good, bad}); err == nil {
				t.Fatalf("accepted %+v", bad)
			}
			if after, _ := d.Raw(); !bytes.Equal(before, after) {
				t.Fatal("a rejected call still changed the transaction")
			}
		}
	})
	t.Run("a signature does not carry over to the other mode", func(t *testing.T) {
		// The digest includes the signature count, so a unilateral signature
		// must not verify on the unanimous version of the same outputs.
		uni, _ := NewDissolution(m, DissolveParams{Actor: 1, Fee: 1000})
		s, _ := uni.Sign(keys[1], 1)
		una, _ := NewDissolution(m, DissolveParams{Actor: 1, Unanimous: true, Fee: 1000})
		if err := una.AddSigs([]ShareSig{s}); err == nil {
			t.Fatal("a unilateral signature verified on a unanimous dissolution")
		}
	})
}

// Dash Core's check drops sub-duff remainders in each floor and requires the
// bonuses to add up to the penalty, so a dissolution that stops at the floors
// is refused while the early penalty applies.
func TestPenaltySumNeedsTheRemainder(t *testing.T) {
	m := &SharedMN{
		Shares: []protx.Share{
			{Amount: 200e8, RefundScript: p2pkh(1)}, {Amount: 300e8, RefundScript: p2pkh(2)}, {Amount: 500e8, RefundScript: p2pkh(3)},
		},
		EarlyPeriodBlocks: 10, EarlyPenalty: 1_000_000_007,
	}
	d, err := NewDissolution(m, DissolveParams{Actor: 0, Penalty: 1_000_000_007, Fee: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckAt(0); err != nil {
		t.Fatal(err)
	}
	// Take the remainder back from the last share and give it to the actor.
	d.tx.TxOut[1].Value--
	d.tx.TxOut[2].Value++
	if err := d.CheckAt(0); err == nil || !strings.Contains(err.Error(), "penalty-sum") {
		t.Fatalf("a dissolution short of the penalty by a duff was accepted: %v", err)
	}
}

// A stored standby is one signature by the actor. A complete unanimous
// dissolution is not one, whatever its record says.
func TestParseStandbyRefusesUnanimous(t *testing.T) {
	m, keys := threeOwners(t)
	d, err := NewDissolution(m, DissolveParams{Actor: 0, Unanimous: true, Fee: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var sigs []ShareSig
	for i, k := range keys {
		s, err := d.Sign(k, i)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	if err := d.AddSigs(sigs); err != nil {
		t.Fatal(err)
	}
	raw, _ := d.Raw()
	rec, _ := json.Marshal(Standby{ProTxHash: m.ProTxHash, Actor: 0, ValidFromHeight: m.StandbyValidFrom(), Tx: hex.EncodeToString(raw)})
	if _, _, err := ParseStandby(rec, m); err == nil {
		t.Fatal("a unanimous dissolution was accepted as a standby")
	}
}

func mustHexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStandby(t *testing.T) {
	m, keys := threeOwners(t)
	s, err := NewStandby(m, 2, keys[2], 1000)
	if err != nil {
		t.Fatal(err)
	}
	if s.ValidFromHeight != 110 || s.Ready(109) || !s.Ready(110) {
		t.Fatalf("valid from %d", s.ValidFromHeight)
	}
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, tx, err := ParseStandby(data, m); err != nil || tx.Type != 10 {
		t.Fatalf("round trip: %v", err)
	}

	// Another key cannot make a standby for the share.
	if _, err := NewStandby(m, 2, keys[0], 1000); err == nil {
		t.Fatal("standby signed with another share's key")
	}

	// A stored standby is checked against the masternode it is loaded for.
	other := *m
	other.ProTxHash = chainhash.Hash{9}
	if _, _, err := ParseStandby(data, &other); err == nil {
		t.Fatal("standby accepted for another masternode")
	}
	var rec Standby
	_ = json.Unmarshal(data, &rec)
	raw, _ := hex.DecodeString(rec.Tx)
	raw[len(raw)-1] ^= 1 // a signature byte
	rec.Tx = hex.EncodeToString(raw)
	bad, _ := json.Marshal(rec)
	if _, _, err := ParseStandby(bad, m); err == nil {
		t.Fatal("a standby with a bad signature was accepted")
	}
	// A changed share table (the refund script moved) invalidates it.
	moved := *m
	moved.Shares = append([]protx.Share(nil), m.Shares...)
	moved.Shares[0].RefundScript = p2pkh(0x77)
	if _, _, err := ParseStandby(data, &moved); err == nil {
		t.Fatal("standby accepted after the share table changed")
	}
	// The signature does not cover the type, so a record whose transaction was
	// retyped, or whose valid-from height was edited, must be refused.
	retyped := &bisonwire.Tx{Chain: "dash"}
	if err := retyped.Deserialize(bytes.NewReader(mustHexBytes(t, s.Tx))); err != nil {
		t.Fatal(err)
	}
	retyped.Type = 11
	var rb bytes.Buffer
	if err := retyped.Serialize(&rb); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(r *Standby){
		"retyped transaction": func(r *Standby) { r.Tx = hex.EncodeToString(rb.Bytes()) },
		"valid-from lowered":  func(r *Standby) { r.ValidFromHeight = 0 },
		"valid-from raised":   func(r *Standby) { r.ValidFromHeight += 50 },
	} {
		r := rec
		r.Tx = s.Tx
		edit(&r)
		b, _ := json.Marshal(r)
		if _, _, err := ParseStandby(b, m); !errors.Is(err, ErrInvalidDissolution) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	for _, in := range []string{``, `{`, `{"tx":"zz"}`, `{"tx":"00"}`} {
		if _, _, err := ParseStandby([]byte(in), m); !errors.Is(err, ErrInvalidDissolution) {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

// The payout arithmetic divides by sums of share amounts, so a table that is
// not a real registration must be refused by every entry point rather than
// panic or pay out of proportion.
func TestShareTableValidated(t *testing.T) {
	m, keys := threeOwners(t)
	s, err := NewStandby(m, 0, keys[0], 1000)
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(sh []protx.Share){
		"others hold nothing": func(sh []protx.Share) { sh[1].Amount, sh[2].Amount = 0, 0 },
		"negative amount":     func(sh []protx.Share) { sh[1].Amount, sh[2].Amount = -1, 501e8 },
		"short of collateral": func(sh []protx.Share) { sh[2].Amount-- },
		"over the collateral": func(sh []protx.Share) { sh[2].Amount++ },
	} {
		t.Run(name, func(t *testing.T) {
			bad := *m
			bad.Shares = append([]protx.Share(nil), m.Shares...)
			edit(bad.Shares)
			if _, err := NewDissolution(&bad, DissolveParams{Actor: 0}); !errors.Is(err, ErrInvalidDissolution) {
				t.Fatalf("NewDissolution: %v", err)
			}
			if _, err := bad.Preview(DissolveParams{Actor: 0}); !errors.Is(err, ErrInvalidDissolution) {
				t.Fatalf("Preview: %v", err)
			}
			if _, _, err := ParseStandby(data, &bad); !errors.Is(err, ErrInvalidDissolution) {
				t.Fatalf("ParseStandby: %v", err)
			}
		})
	}
}

// A huge fee rate must cap the fee, not wrap into a negative one.
func TestFeeForHugeRate(t *testing.T) {
	m := coreMN(t)
	for _, rate := range []int64{math.MaxInt64 / 100, math.MaxInt64} {
		if fee, err := FeeFor(m, false, rate); err != nil || fee != MaxDissolutionFee {
			t.Fatalf("rate %d: fee %d, %v", rate, fee, err)
		}
	}
}

// The fee is capped exactly where the rate makes it pass MaxDissolutionFee:
// at the largest rate whose product is safe it is still computed, and one
// more caps it.
func TestFeeForCapBoundary(t *testing.T) {
	m := coreMN(t)
	d, err := NewDissolution(m, DissolveParams{Actor: 0})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := d.Raw()
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(raw))
	edge := MaxDissolutionFee * 1000 / size
	want := (size*edge + 999) / 1000
	if fee, err := FeeFor(m, false, edge); err != nil || fee != want || fee > MaxDissolutionFee {
		t.Fatalf("rate %d: fee %d, want %d (%v)", edge, fee, want, err)
	}
	if fee, err := FeeFor(m, false, edge+1); err != nil || fee != MaxDissolutionFee {
		t.Fatalf("rate %d: fee %d, want the cap (%v)", edge+1, fee, err)
	}
}

// Bytes after the transaction would leave the stored hex different from the
// transaction returned, so the record must be refused.
func TestParseStandbyTrailingBytes(t *testing.T) {
	m, keys := threeOwners(t)
	s, err := NewStandby(m, 2, keys[2], 1000)
	if err != nil {
		t.Fatal(err)
	}
	s.Tx += "00"
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseStandby(data, m); !errors.Is(err, ErrInvalidDissolution) || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("want trailing bytes refused, got %v", err)
	}
}
