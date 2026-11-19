//go:build harness

package masternode

// This test checks that share and registrar updates built and signed by this
// package are accepted by Dash Core v24, that Core's verdict agrees with the
// rules this package applies, and that Dash Core and this package interoperate
// on a registrar update either of them coordinates.
//
// The funding inputs are signed by this package, from a key the test holds
// (the coins are sent to it from wallet gamma), which is the first check of
// txauthor.AddDashInputScripts against Dash Core.
//
// It registers shared masternodes on the regtest harness, which are broadcast
// and mined, and mines the share update and a registrar update, which leaves
// the second masternode with another operator key and banned until an operator
// announces its service. The remaining updates are only passed to
// testmempoolaccept.
//
// Prerequisites (run by the user): the Dash regtest harness from
// util/testing/dash with V24 active and wallet gamma funded with at least 1003
// DASH. The node is alpha on port 20777.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet/txauthor"
)

const regtestPubKeyHashVersion = 140

// fundKey sends amount duffs to a new key's address from wallet gamma and
// returns the output as funding for the key.
func fundKey(t *testing.T, key *btcec.PrivateKey, amount int64) Funding {
	t.Helper()
	address := base58.CheckEncode(btcutil.Hash160(key.PubKey().SerializeCompressed()), regtestPubKeyHashVersion)
	sent := rpcStr(t, "sendtoaddress", address, float64(amount)/1e8)
	mine(t, 1)
	info, err := rpc(t, "getrawtransaction", sent, true)
	if err != nil {
		t.Fatal(err)
	}
	var tx struct {
		Vout []struct {
			Value        float64 `json:"value"`
			N            uint32  `json:"n"`
			ScriptPubKey struct {
				Hex string `json:"hex"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	if err := json.Unmarshal(info, &tx); err != nil {
		t.Fatal(err)
	}
	script := scriptFor(key)
	h, err := chainhash.NewHashFromStr(sent)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range tx.Vout {
		if o.ScriptPubKey.Hex == hex.EncodeToString(script) {
			return Funding{
				Inputs: []FundingInput{{
					OutPoint: wire.OutPoint{Hash: *h, Index: o.N},
					PkScript: script,
					Value:    int64(math.Round(o.Value * 1e8)),
				}},
				ChangeScript: script,
			}
		}
	}
	t.Fatalf("%s pays nothing to the key", sent)
	return Funding{}
}

func sendRaw(t *testing.T, raw []byte) {
	t.Helper()
	rpcStr(t, "sendrawtransaction", hex.EncodeToString(raw))
	mine(t, 1)
}

func protxInfo(t *testing.T, m *SharedMN) string {
	t.Helper()
	r, err := rpc(t, "protx", "info", m.ProTxHash.String())
	if err != nil {
		t.Fatal(err)
	}
	return string(r)
}

// scriptAddress is the regtest address of a P2PKH script.
func scriptAddress(script []byte) string {
	return base58.CheckEncode(script[3:23], regtestPubKeyHashVersion)
}

// forceSignShare signs the payload of u with key whatever share it is, then the
// inputs, to build what NewShareUpdate and Sign refuse, so Dash Core's verdict on
// it can be compared with this package's.
func forceSignShare(t *testing.T, u *ShareUpdate, key *btcec.PrivateKey, fund *btcec.PrivateKey) []byte {
	t.Helper()
	if err := u.sync(); err != nil {
		t.Fatal(err)
	}
	h, err := u.pu.PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	sig := protx.SignCompact(key, h)
	u.pu.Sig = sig[:]
	if err := u.sync(); err != nil {
		t.Fatal(err)
	}
	if err := txauthor.AddDashInputScripts(u.tx, u.prev, secrets{fund}); err != nil {
		t.Fatal(err)
	}
	raw, err := u.Raw()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestShareUpdateAcceptedByDashCore(t *testing.T) {
	m, keys := registerHarnessMN(t)
	fund, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	const fee = 20_000

	// fresh builds a valid update of share 0 from new coins.
	_, newScript, _ := addr(t)
	fresh := func() *ShareUpdate {
		t.Helper()
		u, err := NewShareUpdate(m, 0, newScript, fundKey(t, fund, 1e8), fee)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	u := fresh()
	if err := u.Sign(keys[0], secrets{fund}); err != nil {
		t.Fatal(err)
	}
	raw, _ := u.Raw()
	if ok, why := accepted(t, raw); !ok {
		t.Fatalf("Core refused a share update: %s", why)
	}

	// Core's verdict on what NewShareUpdate refuses must be a refusal, with
	// the reason the rule is named for.
	votingScript := p2pkhScript(m.KeyIDVoting)
	for _, tt := range []struct {
		name   string
		mutate func(u *ShareUpdate)
		signer int
		want   string
	}{
		{"reward pays the voting key", func(u *ShareUpdate) { u.pu.ScriptReward = votingScript }, 0, "bad-proupshare-payee-reuse"},
		{"reward pays another owner key", func(u *ShareUpdate) { u.pu.ScriptReward = p2pkhScript(m.Shares[1].KeyIDOwner) }, 0, "bad-proupshare-payee-reuse"},
		{"reward pays its own owner key", func(u *ShareUpdate) { u.pu.ScriptReward = p2pkhScript(m.Shares[0].KeyIDOwner) }, 0, "bad-proupshare-payee-reuse"},
		{"signed by another share", func(u *ShareUpdate) {}, 1, "bad-proupshare-sig"},
		{"share index past the table", func(u *ShareUpdate) { u.pu.ShareIndex = 2 }, 0, "bad-proupshare-index"},
		{"empty reward", func(u *ShareUpdate) { u.pu.ScriptReward = nil }, 0, "bad-proupshare-payee"},
	} {
		v := fresh()
		tt.mutate(v)
		raw := forceSignShare(t, v, keys[tt.signer], fund)
		if ok, why := accepted(t, raw); ok || !strings.Contains(why, tt.want) {
			t.Errorf("%s: Core's verdict %v %q, this package refuses it, want %q", tt.name, ok, why, tt.want)
		}
	}

	// What it accepts is accepted: a script hash reward.
	v := fresh()
	v.pu.ScriptReward = p2sh(5)
	if ok, why := accepted(t, forceSignShare(t, v, keys[0], fund)); !ok {
		t.Errorf("Core refused a P2SH reward: %s", why)
	}

	// Mined, it takes effect.
	sendRaw(t, raw)
	if info := protxInfo(t, m); !strings.Contains(info, scriptAddress(newScript)) {
		t.Fatalf("the new reward address is not in protx info: %s", info)
	}
}

// harnessOperator returns a new BLS operator key as Dash Core generates it.
func harnessOperator(t *testing.T) (pub [protx.OperatorPubKeySize]byte) {
	t.Helper()
	r, err := rpc(t, "bls", "generate")
	if err != nil {
		t.Fatal(err)
	}
	var k struct{ Public string }
	if err := json.Unmarshal(r, &k); err != nil {
		t.Fatal(err)
	}
	copy(pub[:], mustHexT(t, k.Public))
	return
}

func TestRegistrarUpdateAcceptedByDashCore(t *testing.T) {
	m, keys := registerHarnessMN(t)
	fund, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	const fee = 20_000
	_, _, votingID := addr(t)
	change := RegistrarChange{PubKeyOperator: harnessOperator(t), KeyIDVoting: votingID}

	// signed returns an update with the signatures of every share on it.
	signed := func(c RegistrarChange, f Funding) *RegistrarUpdate {
		t.Helper()
		u, err := NewRegistrarUpdate(m, c, f, fee)
		if err != nil {
			t.Fatal(err)
		}
		var sigs []ShareSig
		for i, k := range keys {
			// Each owner reviews what it was sent before it signs.
			blob, _ := u.Blob()
			data, _ := json.Marshal(blob)
			tx, _, err := ParseBlob(data)
			if err != nil {
				t.Fatal(err)
			}
			rv, err := ReviewRegistrarUpdate(tx, m, c)
			if err != nil {
				t.Fatal(err)
			}
			s, err := rv.Sign(k, i)
			if err != nil {
				t.Fatal(err)
			}
			sigs = append(sigs, s)
		}
		if err := u.AddSigs(sigs); err != nil {
			t.Fatal(err)
		}
		return u
	}

	f := fundKey(t, fund, 1e8)
	u := signed(change, f)
	if !u.OperatorChanged {
		t.Fatal("the operator changed")
	}
	if err := u.SignInputs(f, fee, secrets{fund}); err != nil {
		t.Fatal(err)
	}
	raw, _ := u.Raw()
	if ok, why := accepted(t, raw); !ok {
		t.Fatalf("Core refused a registrar update: %s", why)
	}

	// Core's verdict on what this package refuses.
	// forced builds an update with the keys of c, every owner signing it as it
	// is after mut changed the payload, and the inputs signed after that.
	forced := func(c RegistrarChange, mut func(pu *protx.ProUpSharedRegTx)) []byte {
		t.Helper()
		f := fundKey(t, fund, 1e8)
		v, err := NewRegistrarUpdate(m, change, f, fee)
		if err != nil {
			t.Fatal(err)
		}
		v.pu.PubKeyOperator, v.pu.KeyIDVoting = c.PubKeyOperator, c.KeyIDVoting
		if mut != nil {
			mut(&v.pu)
		}
		h, _ := v.pu.PayloadHash()
		for i := range v.pu.Sigs {
			if v.pu.Sigs[i] == ([protx.CompactSigSize]byte{}) && i < len(keys) {
				v.pu.Sigs[i] = protx.SignCompact(keys[i], h)
			}
		}
		if err := v.sync(); err != nil {
			t.Fatal(err)
		}
		if err := txauthor.AddDashInputScripts(v.tx, map[int][]byte{0: f.Inputs[0].PkScript}, secrets{fund}); err != nil {
			t.Fatal(err)
		}
		raw, _ := v.Raw()
		return raw
	}
	refundReuse := change
	refundReuse.KeyIDVoting = [20]byte(m.Shares[0].RefundScript[3:23])
	for _, tt := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"voting key paid by a refund script", forced(refundReuse, nil), "bad-proupsharedreg-payee-reuse"},
		{"too few signatures", forced(change, func(pu *protx.ProUpSharedRegTx) { pu.Sigs = pu.Sigs[:1] }), "bad-proupsharedreg-sig-count"},
		{"a signature that is not the owner's", forced(change, func(pu *protx.ProUpSharedRegTx) {
			h, _ := pu.PayloadHash()
			pu.Sigs[0] = protx.SignCompact(keys[1], h)
			pu.Sigs[1] = protx.SignCompact(keys[1], h)
		}), "bad-proupsharedreg-sig"},
	} {
		if ok, why := accepted(t, tt.raw); ok || !strings.Contains(why, tt.want) {
			t.Errorf("%s: Core's verdict %v %q, want %q", tt.name, ok, why, tt.want)
		}
	}

	// Mined, it takes effect.
	sendRaw(t, raw)
	info := protxInfo(t, m)
	if !strings.Contains(info, hex.EncodeToString(change.PubKeyOperator[:])) {
		t.Fatalf("the new operator key is not in protx info: %s", info)
	}
	if !strings.Contains(info, base58.CheckEncode(change.KeyIDVoting[:], regtestPubKeyHashVersion)) {
		t.Fatalf("the new voting address is not in protx info: %s", info)
	}
}

// TestRegistrarUpdateCoordinatedByDashCore has Dash Core prepare and combine a
// registrar update, which this package reviews and signs as the share owners.
func TestRegistrarUpdateCoordinatedByDashCore(t *testing.T) {
	m, keys := registerHarnessMN(t)
	change := RegistrarChange{PubKeyOperator: harnessOperator(t), KeyIDVoting: m.KeyIDVoting}

	feeAddr := rpcStr(t, "getnewaddress")
	rpcStr(t, "sendtoaddress", feeAddr, 1.0)
	mine(t, 1)

	r, err := rpc(t, "protx", "shared_update_registrar_prepare", m.ProTxHash.String(),
		hex.EncodeToString(change.PubKeyOperator[:]), "", feeAddr)
	if err != nil {
		t.Fatal(err)
	}
	var prep struct{ Tx, SignHash string }
	if err := json.Unmarshal(r, &prep); err != nil {
		t.Fatal(err)
	}
	txRaw := mustHexT(t, prep.Tx)
	data, _ := json.Marshal(Blob{Tx: prep.Tx})
	tx, _, err := ParseBlob(data)
	if err != nil {
		t.Fatal(err)
	}

	var sigs []ShareSig
	for i, k := range keys {
		rv, err := ReviewRegistrarUpdate(tx, m, change)
		if err != nil {
			t.Fatalf("owner %d refused what Core prepared: %v", i, err)
		}
		if h, _ := rv.pu.PayloadHash(); h.String() != prep.SignHash {
			t.Fatalf("payload hash %s, Core's signHash %s", h, prep.SignHash)
		}
		s, err := rv.Sign(k, i)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	sigJSON, _ := json.Marshal(sigs)
	combined := rpcStr(t, "protx", "shared_combine", prep.Tx, json.RawMessage(sigJSON))
	if ok, why := accepted(t, mustHexT(t, combined)); !ok {
		t.Fatalf("Core refused the update it combined from this package's signatures: %s", why)
	}
	if bytes.Equal(txRaw, mustHexT(t, combined)) {
		t.Fatal("combining added nothing")
	}
}
