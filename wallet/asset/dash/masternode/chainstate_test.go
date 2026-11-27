// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
)

// The vectors in testdata are captured unedited from a Dash Core v24 regtest
// node and its ElectrumX: the registration of 8e7019ff (collateral output 2,
// two shares of 500 DASH, early period 10 blocks with a 5 DASH penalty) mined
// at 798, the registration db7fab62 mined at 781 and dissolved by 410de96f, and
// protx.info for 8e7019ff and for a banned masternode.

// regtestNet is the address format of the vectors' network.
var regtestNet = &chaincfg.Params{Name: "dashregtest", PubKeyHashAddrID: 0x8c, ScriptHashAddrID: 0x13}

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func txFromHex(t *testing.T, h string) *bisonwire.Tx {
	t.Helper()
	raw, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	tx := &bisonwire.Tx{Chain: "dash"}
	r := bytes.NewReader(raw)
	if err := tx.Deserialize(r); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 0 {
		t.Fatal("trailing bytes")
	}
	return tx
}

func specialTx(t *testing.T, name string) *bisonwire.Tx {
	t.Helper()
	var all []struct{ Name, Hex string }
	readJSON(t, "dash_special_txs.json", &all)
	for _, v := range all {
		if v.Name == name {
			return txFromHex(t, v.Hex)
		}
	}
	t.Fatalf("no vector %s", name)
	return nil
}

func dissolvedRegTx(t *testing.T) *bisonwire.Tx {
	t.Helper()
	var v struct{ Hex string }
	readJSON(t, "proregtx_dissolved.json", &v)
	return txFromHex(t, v.Hex)
}

func loadInfo(t *testing.T, name string) *electrum.ProTxInfo {
	t.Helper()
	var info electrum.ProTxInfo
	readJSON(t, name, &info)
	return &info
}

func vectorMN(t *testing.T) *SharedMN {
	t.Helper()
	m, err := RegistrationMN(specialTx(t, "proregtx"), 798)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRegistrationMN(t *testing.T) {
	tx := specialTx(t, "proregtx")
	m := vectorMN(t)
	if m.ProTxHash.String() != "8e7019ff0db6eb8e4e1333834f95f3b155d232d6234c95bb047a545cb38831d6" {
		t.Fatalf("hash %s", m.ProTxHash)
	}
	if m.Collateral.Hash != m.ProTxHash || m.Collateral.Index != 2 {
		t.Fatalf("collateral %v", m.Collateral)
	}
	if m.RegisteredHeight != 798 || m.EarlyPeriodBlocks != 10 || m.EarlyPenalty != 5e8 {
		t.Fatalf("terms %d %d %d", m.RegisteredHeight, m.EarlyPeriodBlocks, m.EarlyPenalty)
	}
	if m.StandbyValidFrom() != 808 {
		t.Fatalf("standby from %d", m.StandbyValidFrom())
	}
	if len(m.Shares) != 2 || m.Shares[0].Amount != 500e8 || m.Shares[1].Amount != 500e8 {
		t.Fatalf("shares %+v", m.Shares)
	}
	// The registration leaves the reward scripts empty, which Core pays at the
	// refund script, and protx.info reports as such.
	var pr protx.ProRegTx
	if err := protx.Parse(tx.ExtraPayload, &pr); err != nil {
		t.Fatal(err)
	}
	for i, s := range m.Shares {
		if len(pr.Shares[i].RewardScript) != 0 {
			t.Fatalf("vector share %d has a reward script", i)
		}
		if len(s.RewardScript) == 0 || !bytes.Equal(s.RewardScript, s.RefundScript) {
			t.Fatalf("share %d reward script %x, refund %x", i, s.RewardScript, s.RefundScript)
		}
	}
	if hex.EncodeToString(m.PubKeyOperator[:]) != loadInfo(t, "protx_info_shared.json").State.PubKeyOperator {
		t.Fatal("operator key")
	}
	if m.KeyIDVoting != pr.KeyIDVoting {
		t.Fatal("voting key")
	}

	// The reward script is a copy: changing it must not change the refund.
	m.Shares[0].RewardScript[0] ^= 0xff
	if m.Shares[0].RefundScript[0] == m.Shares[0].RewardScript[0] {
		t.Fatal("reward and refund scripts share memory")
	}

	d, err := RegistrationMN(dissolvedRegTx(t), 781)
	if err != nil {
		t.Fatal(err)
	}
	if d.ProTxHash.String()[:8] != "db7fab62" || d.RegisteredHeight != 781 {
		t.Fatalf("dissolved registration %s at %d", d.ProTxHash, d.RegisteredHeight)
	}
}

// mutated returns the vector registration after f changed its decoded payload
// and transaction, with the payload serialized again.
func mutated(t *testing.T, f func(pr *protx.ProRegTx, tx *bisonwire.Tx)) *bisonwire.Tx {
	t.Helper()
	tx := specialTx(t, "proregtx")
	var pr protx.ProRegTx
	if err := protx.Parse(tx.ExtraPayload, &pr); err != nil {
		t.Fatal(err)
	}
	f(&pr, tx)
	var b bytes.Buffer
	if err := pr.Serialize(&b); err != nil {
		t.Fatal(err)
	}
	tx.ExtraPayload = b.Bytes()
	return tx
}

// The fields a shared registration leaves empty cannot be set: the codec
// refuses to write them, and so to read them, before RegistrationMN sees one.
func TestSharedRegistrationLeavesFieldsEmpty(t *testing.T) {
	tests := map[string]func(pr *protx.ProRegTx){
		"owner key":           func(pr *protx.ProRegTx) { pr.KeyIDOwner[0] = 1 },
		"external collateral": func(pr *protx.ProRegTx) { pr.Collateral.Hash[0] = 1 },
		"owner payouts": func(pr *protx.ProRegTx) {
			pr.Payouts = []protx.Payout{{Script: pr.Shares[0].RefundScript, Reward: 10000}}
		},
	}
	for name, f := range tests {
		var pr protx.ProRegTx
		if err := protx.Parse(specialTx(t, "proregtx").ExtraPayload, &pr); err != nil {
			t.Fatal(err)
		}
		f(&pr)
		if err := pr.Serialize(&bytes.Buffer{}); err == nil {
			t.Errorf("%s: written", name)
		}
	}
}

func TestRegistrationMNRejects(t *testing.T) {
	tests := map[string]struct {
		tx     func() *bisonwire.Tx
		height int32
	}{
		"height zero":     {func() *bisonwire.Tx { return specialTx(t, "proregtx") }, 0},
		"height negative": {func() *bisonwire.Tx { return specialTx(t, "proregtx") }, -1},
		"coinbase":        {func() *bisonwire.Tx { return specialTx(t, "coinbase") }, 798},
		"dissolution":     {func() *bisonwire.Tx { return specialTx(t, "prodistx") }, 798},
		"normal type": {func() *bisonwire.Tx {
			tx := specialTx(t, "proregtx")
			tx.Type = 0
			return tx
		}, 798},
		"version 2": {func() *bisonwire.Tx {
			tx := specialTx(t, "proregtx")
			tx.MsgTx.Version = 2
			return tx
		}, 798},
		"trailing payload byte": {func() *bisonwire.Tx {
			tx := specialTx(t, "proregtx")
			tx.ExtraPayload = append(tx.ExtraPayload, 0)
			return tx
		}, 798},
		"truncated payload": {func() *bisonwire.Tx {
			tx := specialTx(t, "proregtx")
			tx.ExtraPayload = tx.ExtraPayload[:len(tx.ExtraPayload)-1]
			return tx
		}, 798},
		"payload signature": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) { pr.PayloadSig = make([]byte, 65) })
		}, 798},
		"collateral index past the outputs": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, tx *bisonwire.Tx) { pr.Collateral.Index = uint32(len(tx.TxOut)) })
		}, 798},
		"collateral index at another output": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) { pr.Collateral.Index = 0 })
		}, 798},
		"collateral one duff short": {func() *bisonwire.Tx {
			return mutated(t, func(_ *protx.ProRegTx, tx *bisonwire.Tx) { tx.TxOut[2].Value-- })
		}, 798},
		"collateral script differs": {func() *bisonwire.Tx {
			return mutated(t, func(_ *protx.ProRegTx, tx *bisonwire.Tx) { tx.TxOut[2].PkScript[6] = 0x52 })
		}, 798},
		"single share": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) {
				pr.Shares = pr.Shares[:1]
				pr.Shares[0].Amount = CollateralAmount
			})
		}, 798},
		"nine shares": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) {
				pr.Shares = nil
				for i := range 9 {
					s := vectorShare(t)
					s.Amount = int64(CollateralAmount) / 9
					s.KeyIDOwner[0] = byte(i + 1)
					pr.Shares = append(pr.Shares, s)
				}
				pr.Shares[0].Amount += int64(CollateralAmount) - 9*(int64(CollateralAmount)/9)
			})
		}, 798},
		"zero amount": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) {
				pr.Shares[0].Amount = 0
				pr.Shares[1].Amount = CollateralAmount
			})
		}, 798},
		"negative amount": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) {
				pr.Shares[0].Amount = -1
				pr.Shares[1].Amount = CollateralAmount + 1
			})
		}, 798},
		"amounts do not sum to the collateral": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) { pr.Shares[0].Amount-- })
		}, 798},
		"evo masternode": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) { pr.MNType = protx.TypeEvo })
		}, 798},
		// 2^62 four times wraps an int64 sum to zero; three of them and one
		// that makes the total exactly the collateral wrap to 1000 DASH.
		"amounts that wrap the sum": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) {
				pr.Shares = nil
				for i := range 4 {
					s := vectorShare(t)
					s.Amount = 1 << 62
					s.KeyIDOwner[0] = byte(i + 1)
					pr.Shares = append(pr.Shares, s)
				}
				pr.Shares[3].Amount += int64(CollateralAmount)
			})
		}, 798},
		"collateral index of 2^31": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) { pr.Collateral.Index = 1 << 31 })
		}, 798},
		"negative penalty": {func() *bisonwire.Tx {
			return mutated(t, func(pr *protx.ProRegTx, _ *bisonwire.Tx) { pr.EarlyPenalty = -1 })
		}, 798},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := RegistrationMN(tc.tx(), tc.height)
			if !errors.Is(err, ErrInvalidRegistration) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func vectorShare(t *testing.T) protx.Share {
	t.Helper()
	var pr protx.ProRegTx
	if err := protx.Parse(specialTx(t, "proregtx").ExtraPayload, &pr); err != nil {
		t.Fatal(err)
	}
	return pr.Shares[0]
}

// Each rejection above must be for its own reason: an otherwise valid
// registration is accepted with the same mutation helper, so a case cannot pass
// because the helper broke the payload.
func TestMutationHelperKeepsRegistrationValid(t *testing.T) {
	tx := mutated(t, func(*protx.ProRegTx, *bisonwire.Tx) {})
	if _, err := RegistrationMN(tx, 798); err != nil {
		t.Fatal(err)
	}
}

func TestApplyInfo(t *testing.T) {
	m := vectorMN(t)
	info := loadInfo(t, "protx_info_shared.json")
	if err := m.ApplyInfo(info, regtestNet); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(m.PubKeyOperator[:]) != info.State.PubKeyOperator {
		t.Fatal("operator key")
	}
	if m.KeyIDVoting == ([protx.KeyIDSize]byte{}) {
		t.Fatal("voting key")
	}
	// The owner address of the vector decodes to the key id in the payload,
	// which is what ties a server share to the registration's.
	id, err := keyIDOf("yiBH7BTY72jHCUYTupKGxymbnXCvZeYdbY", regtestNet)
	if err != nil || id != m.Shares[0].KeyIDOwner {
		t.Fatalf("owner %x %v", id, err)
	}

	// What changed on the server is taken in full: new reward script, new
	// voting key and an all zero operator key, as a revocation leaves.
	other := loadInfo(t, "protx_info_shared.json")
	// Core can move the state version after an update; the registration is
	// pinned by its type and share table, not by it.
	other.State.Version = 4
	other.State.Shares[1].RewardScript = "a914" + "11223344556677889900112233445566778899aa" + "87"
	other.State.VotingAddress = "yTimJeENUr1XFg2EtPpyTV5vgyuvg5oFs7"
	other.State.PubKeyOperator = hex.EncodeToString(make([]byte, 48))
	if err := m.ApplyInfo(other, regtestNet); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(m.Shares[1].RewardScript) != other.State.Shares[1].RewardScript {
		t.Fatalf("reward %x", m.Shares[1].RewardScript)
	}
	if bytes.Equal(m.Shares[1].RewardScript, m.Shares[1].RefundScript) {
		t.Fatal("reward not applied")
	}
	if m.PubKeyOperator != ([protx.OperatorPubKeySize]byte{}) {
		t.Fatal("operator key not applied")
	}
	want, _ := keyIDOf(other.State.VotingAddress, regtestNet)
	if m.KeyIDVoting != want {
		t.Fatal("voting key not applied")
	}
}

func TestApplyInfoRejects(t *testing.T) {
	tests := map[string]func(i *electrum.ProTxInfo){
		"another proTxHash": func(i *electrum.ProTxInfo) { i.ProTxHash = i.CollateralHash[:63] + "0" },
		"nil info":          nil,
		"collateral hash":   func(i *electrum.ProTxInfo) { i.CollateralHash = i.CollateralHash[:63] + "0" },
		"collateral index":  func(i *electrum.ProTxInfo) { i.CollateralIndex = 1 },
		"type":              func(i *electrum.ProTxInfo) { i.Type = "Evo" },
		"height up":         func(i *electrum.ProTxInfo) { i.State.RegisteredHeight++ },
		"height down":       func(i *electrum.ProTxInfo) { i.State.RegisteredHeight-- },
		"share count":       func(i *electrum.ProTxInfo) { i.State.Shares = i.State.Shares[:1] },
		"extra share": func(i *electrum.ProTxInfo) {
			i.State.Shares = append(i.State.Shares, i.State.Shares[1])
		},
		"voting key is zero": func(i *electrum.ProTxInfo) {
			a, _ := btcutil.NewAddressPubKeyHash(make([]byte, 20), regtestNet)
			i.State.VotingAddress = a.EncodeAddress()
		},
		"share amount":          func(i *electrum.ProTxInfo) { i.State.Shares[0].Amount-- },
		"refund script":         func(i *electrum.ProTxInfo) { i.State.Shares[0].RefundScript = i.State.Shares[1].RefundScript },
		"refund script not hex": func(i *electrum.ProTxInfo) { i.State.Shares[0].RefundScript = "zz" },
		"owner of another key": func(i *electrum.ProTxInfo) {
			i.State.Shares[0].OwnerAddress = i.State.Shares[1].OwnerAddress
		},
		"owner on another network": func(i *electrum.ProTxInfo) {
			i.State.Shares[0].OwnerAddress = "XdAUmwtig27HBG6WfYyHAzP8n6XC9jESEw"
		},
		"owner not an address": func(i *electrum.ProTxInfo) { i.State.Shares[0].OwnerAddress = "x" },
		"owner is a script address": func(i *electrum.ProTxInfo) {
			i.State.Shares[0].OwnerAddress = p2shAddr(t)
		},
		"early period":       func(i *electrum.ProTxInfo) { i.State.EarlyPeriodBlocks++ },
		"early penalty":      func(i *electrum.ProTxInfo) { i.State.EarlyPenalty++ },
		"reward not payable": func(i *electrum.ProTxInfo) { i.State.Shares[0].RewardScript = "6a" },
		"reward not hex":     func(i *electrum.ProTxInfo) { i.State.Shares[0].RewardScript = "zz" },
		"voting garbage":     func(i *electrum.ProTxInfo) { i.State.VotingAddress = "nope" },
		"voting script address": func(i *electrum.ProTxInfo) {
			i.State.VotingAddress = p2shAddr(t)
		},
		"operator 47 bytes": func(i *electrum.ProTxInfo) { i.State.PubKeyOperator = i.State.PubKeyOperator[:94] },
		"operator not hex":  func(i *electrum.ProTxInfo) { i.State.PubKeyOperator = "zz" },
	}
	for name, mut := range tests {
		t.Run(name, func(t *testing.T) {
			m := vectorMN(t)
			before := cloneMN(m)
			var info *electrum.ProTxInfo
			if mut != nil {
				info = loadInfo(t, "protx_info_shared.json")
				mut(info)
			}
			err := m.ApplyInfo(info, regtestNet)
			if !errors.Is(err, ErrInfoMismatch) {
				t.Fatalf("got %v", err)
			}
			if !reflect.DeepEqual(m, before) {
				t.Fatal("a failed ApplyInfo changed the masternode")
			}
		})
	}
}

// p2shAddr is a valid pay to script hash address of the vectors' network.
func p2shAddr(t *testing.T) string {
	t.Helper()
	a, err := btcutil.NewAddressScriptHashFromHash(bytes.Repeat([]byte{7}, 20), regtestNet)
	if err != nil {
		t.Fatal(err)
	}
	return a.EncodeAddress()
}

func cloneMN(m *SharedMN) *SharedMN {
	c := *m
	c.Shares = make([]protx.Share, len(m.Shares))
	for i, s := range m.Shares {
		s.RefundScript = slices.Clone(s.RefundScript)
		s.RewardScript = slices.Clone(s.RewardScript)
		c.Shares[i] = s
	}
	return &c
}

// A builder that took the shares before an update must not see them change.
func TestApplyInfoDoesNotWriteIntoOldShares(t *testing.T) {
	m := vectorMN(t)
	old := m.Shares
	oldReward := slices.Clone(old[1].RewardScript)
	info := loadInfo(t, "protx_info_shared.json")
	info.State.Shares[1].RewardScript = "a914" + "11223344556677889900112233445566778899aa" + "87"
	if err := m.ApplyInfo(info, regtestNet); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old[1].RewardScript, oldReward) {
		t.Fatal("ApplyInfo wrote into the previous shares")
	}
}

func TestDeriveState(t *testing.T) {
	active := loadInfo(t, "protx_info_shared.json")
	banned := loadInfo(t, "protx_info_shared_banned.json")
	if banned.State.PoSeBanHeight <= 0 {
		t.Fatal("vector is not banned")
	}
	// Core treats any ban height but -1 as banned.
	banned0 := loadInfo(t, "protx_info_shared.json")
	banned0.State.PoSeBanHeight = 0
	tests := []struct {
		name      string
		info      *electrum.ProTxInfo
		dissolved bool
		want      State
	}{
		{"no info", nil, false, StateUnknown},
		{"active", active, false, StateActive},
		{"banned", banned, false, StateBanned},
		{"banned at height 0", banned0, false, StateBanned},
		{"dissolved over active info", active, true, StateDissolved},
		{"dissolved without info", nil, true, StateDissolved},
	}
	for _, tc := range tests {
		if got := DeriveState(tc.info, tc.dissolved); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if StateDissolved.String() != "dissolved" || StateUnknown.String() != "unknown" ||
		StateActive.String() != "active" || StateBanned.String() != "banned" {
		t.Fatal("state names")
	}
}

func TestSpendsCollateral(t *testing.T) {
	dis := specialTx(t, "prodistx")
	d, err := RegistrationMN(dissolvedRegTx(t), 781)
	if err != nil {
		t.Fatal(err)
	}
	if !d.SpendsCollateral(dis) {
		t.Fatal("the dissolution of db7fab62 does not spend its collateral")
	}
	if vectorMN(t).SpendsCollateral(dis) {
		t.Fatal("the dissolution of db7fab62 spends 8e7019ff's collateral")
	}
	// Spending another output of the registration is not a dissolution.
	other := &bisonwire.Tx{Chain: "dash"}
	other.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: d.ProTxHash, Index: d.Collateral.Index + 1}, nil, nil))
	if d.SpendsCollateral(other) {
		t.Fatal("another output counted as the collateral")
	}
}
