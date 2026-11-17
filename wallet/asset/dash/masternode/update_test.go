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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet/txauthor"
)

// operator2 is the operator key of Dash Core's registrar update vector, a valid
// key other than operatorKey.
const operator2 = "aeb37d4d8ec432019f9bdaded9678b68b85abed73b6c890966e063854f628285e5bb7435c5d0cf0a35f287b29a7aec22"

func mustKey(t *testing.T, s string) (k [protx.OperatorPubKeySize]byte) {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(k) {
		t.Fatalf("bad key %q", s)
	}
	copy(k[:], b)
	return
}

// upFunding is n inputs of the key worth value in all, and the change to it.
func upFunding(key *btcec.PrivateKey, n int, value int64) Funding {
	f := Funding{ChangeScript: scriptFor(key)}
	for i := range n {
		f.Inputs = append(f.Inputs, FundingInput{
			OutPoint: wire.OutPoint{Hash: chainhash.Hash{0xaa, byte(i)}, Index: uint32(i)},
			PkScript: scriptFor(key),
			Value:    value / int64(n),
		})
	}
	return f
}

// updateMN is three owners of a masternode with reward scripts, a voting key and
// an operator key, so every update rule has something to check.
func updateMN(t *testing.T) (*SharedMN, []*btcec.PrivateKey) {
	t.Helper()
	m, keys := threeOwners(t)
	for i := range m.Shares {
		m.Shares[i].RewardScript = p2pkh(byte(0x70 + i))
	}
	m.KeyIDVoting = keyID(0x66)
	m.PubKeyOperator = mustKey(t, operatorKey)
	return m, keys
}

// TestShareUpdateMatchesCore builds the update dashd made of the regshared
// masternode and requires its payload bytes, payload hash and inputs hash, and
// that dashd's signature verifies against our digest.
func TestShareUpdateMatchesCore(t *testing.T) {
	m := coreMN(t)
	core, _ := loadVectorTx(t, "upshare")
	var want protx.ProUpShareTx
	if err := protx.Parse(core.ExtraPayload, &want); err != nil {
		t.Fatal(err)
	}
	const fee = 1000
	f := Funding{
		Inputs: []FundingInput{{
			OutPoint: core.TxIn[0].PreviousOutPoint,
			PkScript: p2pkh(9),
			Value:    core.TxOut[0].Value + fee,
		}},
		ChangeScript: core.TxOut[0].PkScript,
	}
	u, err := NewShareUpdate(m, int(want.ShareIndex), want.ScriptReward, f, fee)
	if err != nil {
		t.Fatal(err)
	}
	if u.pu.InputsHash != want.InputsHash {
		t.Fatalf("inputs hash %s, dashd %s", u.pu.InputsHash, want.InputsHash)
	}
	h, err := u.pu.PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	if got := "fae2e65e21384e65b17f62629a0283f7fcba176bcf42841e8e6e58ef07a15664"; h.String() != got {
		t.Fatalf("payload hash %s, dashd %s", h, got)
	}
	if err := protx.VerifyCompact(h, m.Shares[want.ShareIndex].KeyIDOwner, want.Sig); err != nil {
		t.Fatalf("dashd's signature does not verify against our digest: %v", err)
	}
	u.pu.Sig = want.Sig
	if err := u.sync(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(u.tx.ExtraPayload, core.ExtraPayload) {
		t.Fatalf("payload\n got %x\nwant %x", u.tx.ExtraPayload, core.ExtraPayload)
	}
	if u.tx.Type != 11 || u.tx.MsgTx.Version != core.MsgTx.Version {
		t.Fatalf("type %d version %d", u.tx.Type, u.tx.MsgTx.Version)
	}
}

func TestNewShareUpdateAccepts(t *testing.T) {
	m, keys := updateMN(t)
	f := upFunding(keys[0], 1, 1e8)
	other := scriptFor(testKey(0x99))
	for name, reward := range map[string][]byte{
		"a fresh P2PKH":           other,
		"a P2SH":                  p2sh(5),
		"its own refund script":   m.Shares[0].RefundScript,
		"another share's refund":  m.Shares[1].RefundScript,
		"another share's reward":  m.Shares[1].RewardScript,
		"its own current reward":  m.Shares[0].RewardScript,
		"a P2SH of the voting id": p2sh(0x66), // a script hash is never a key hash
	} {
		if _, err := NewShareUpdate(m, 0, reward, f, 5000); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNewShareUpdateRejects(t *testing.T) {
	m, keys := updateMN(t)
	good := upFunding(keys[0], 1, 1e8)
	fresh := scriptFor(testKey(0x99))

	// Funding that is wrong in one way.
	with := func(mut func(f *Funding)) Funding {
		f := upFunding(keys[0], 2, 1e8)
		mut(&f)
		return f
	}
	dup := with(func(f *Funding) { f.Inputs[1].OutPoint = f.Inputs[0].OutPoint })
	voting := append(append([]byte{0x76, 0xa9, 0x14}, m.KeyIDVoting[:]...), 0x88, 0xac)
	owner1 := scriptFor(keys[1])

	for _, tt := range []struct {
		name   string
		share  int
		reward []byte
		f      Funding
		fee    int64
		want   string
	}{
		{"negative index", -1, fresh, good, 5000, "bad-proupshare-index"},
		{"index past the shares", 3, fresh, good, 5000, "bad-proupshare-index"},
		{"empty script", 0, nil, good, 5000, "bad-proupshare-payee"},
		{"OP_RETURN", 0, []byte{0x6a}, good, 5000, "bad-proupshare-payee"},
		{"short P2PKH", 0, fresh[:24], good, 5000, "bad-proupshare-payee"},
		{"voting key", 0, voting, good, 5000, "bad-proupshare-payee-reuse"},
		{"its own owner key", 0, scriptFor(keys[0]), good, 5000, "bad-proupshare-payee-reuse"},
		{"another owner key", 0, owner1, good, 5000, "bad-proupshare-payee-reuse"},
		{"no inputs", 0, fresh, Funding{ChangeScript: good.ChangeScript}, 5000, "no inputs"},
		{"input that is not P2PKH", 0, fresh, with(func(f *Funding) { f.Inputs[0].PkScript = p2sh(1) }), 5000, "not P2PKH"},
		{"repeated input", 0, fresh, dup, 5000, "spent twice"},
		{"zero value input", 0, fresh, with(func(f *Funding) { f.Inputs[0].Value = 0 }), 5000, "has value"},
		{"input over the supply", 0, fresh, with(func(f *Funding) { f.Inputs[0].Value = maxMoney + 1 }), 5000, "has value"},
		{"inputs over the supply", 0, fresh, with(func(f *Funding) {
			f.Inputs[0].Value, f.Inputs[1].Value = maxMoney, maxMoney
		}), 5000, "too much"},
		{"zero fee", 0, fresh, good, 0, "fee"},
		{"fee over the cap", 0, fresh, good, MaxUpdateFee + 1, "fee"},
		{"no change", 0, fresh, upFunding(keys[0], 1, 5000), 5000, "after the fee"},
		{"dust change", 0, fresh, upFunding(keys[0], 1, 5000+dustLimit-1), 5000, "after the fee"},
		{"change that is not P2PKH", 0, fresh, with(func(f *Funding) { f.ChangeScript = p2sh(1) }), 5000, "change needs"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewShareUpdate(m, tt.share, tt.reward, tt.f, tt.fee)
			if !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}

	// The smallest change that is not dust is accepted.
	if _, err := NewShareUpdate(m, 0, fresh, upFunding(keys[0], 1, 5000+dustLimit), 5000); err != nil {
		t.Fatalf("change of exactly the dust limit: %v", err)
	}
	// So are the largest fee and the largest input.
	if _, err := NewShareUpdate(m, 0, fresh, upFunding(keys[0], 1, maxMoney), MaxUpdateFee); err != nil {
		t.Fatalf("the largest fee: %v", err)
	}
}

// signedInputs returns the input scripts of tx as they are.
func signedInputs(tx *bisonwire.Tx) [][]byte {
	var out [][]byte
	for _, in := range tx.TxIn {
		out = append(out, in.SignatureScript)
	}
	return out
}

// reSigned signs the inputs of a copy of tx, with their scripts cleared, as it
// stands, so it is what the input signatures must equal if they cover the
// payload as it stands (signing is deterministic).
func reSigned(t *testing.T, tx *bisonwire.Tx, prev map[int][]byte, key *btcec.PrivateKey) [][]byte {
	t.Helper()
	cp, err := cloneTx(tx)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range cp.TxIn {
		in.SignatureScript = nil
	}
	if err := txauthor.AddDashInputScripts(cp, prev, secrets{key}); err != nil {
		t.Fatal(err)
	}
	return signedInputs(cp)
}

func TestShareUpdateSign(t *testing.T) {
	m, keys := updateMN(t)
	fund := testKey(0x88)
	f := upFunding(fund, 2, 1e8)
	reward := scriptFor(testKey(0x99))

	u, err := NewShareUpdate(m, 1, reward, f, 5000)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := u.Raw()

	// The wrong owner key changes nothing.
	for _, k := range []*btcec.PrivateKey{keys[0], keys[2], fund} {
		if err := u.Sign(k, secrets{fund}); !errors.Is(err, ErrInvalidUpdate) {
			t.Fatalf("signed with another key: %v", err)
		}
	}
	// Neither does a failure to sign the inputs, after the payload is signed.
	if err := u.Sign(keys[1], failingSecrets{}); err == nil {
		t.Fatal("signed without the input keys")
	}
	if after, _ := u.Raw(); !bytes.Equal(before, after) {
		t.Fatal("a failed signing changed the update")
	}

	if err := u.Sign(keys[1], secrets{fund}); err != nil {
		t.Fatal(err)
	}
	h, _ := u.pu.PayloadHash()
	if err := protx.VerifyCompact(h, m.Shares[1].KeyIDOwner, u.pu.Sig); err != nil {
		t.Fatalf("payload signature: %v", err)
	}
	var back protx.ProUpShareTx
	if err := protx.Parse(u.tx.ExtraPayload, &back); err != nil || !bytes.Equal(back.Sig, u.pu.Sig) {
		t.Fatalf("the signature is not in the transaction: %v", err)
	}
	// The inputs are signed over the signed payload, not an unsigned one.
	if got, want := signedInputs(u.tx), reSigned(t, u.tx, u.prev, fund); !equalScripts(got, want) {
		t.Fatal("the inputs were not signed over the final payload")
	}
	for i, in := range u.tx.TxIn {
		if len(in.SignatureScript) == 0 {
			t.Fatalf("input %d is not signed", i)
		}
	}

	// A second signing does not touch the signed update.
	signed, _ := u.Raw()
	if err := u.Sign(keys[1], secrets{fund}); err == nil {
		t.Fatal("signed twice")
	}
	if again, _ := u.Raw(); !bytes.Equal(signed, again) {
		t.Fatal("a refused second signing changed the update")
	}
}

type failingSecrets struct{ secrets }

func (failingSecrets) GetKey(btcutil.Address) (*btcec.PrivateKey, bool, error) {
	return nil, false, errors.New("no key")
}

func equalScripts(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// coreRegistrar loads the registrar update dashd made of the regshared
// masternode and the keys it sets.
func coreRegistrar(t *testing.T) (*bisonwire.Tx, protx.ProUpSharedRegTx, RegistrarChange) {
	t.Helper()
	core, _ := loadVectorTx(t, "upsharedreg")
	var pu protx.ProUpSharedRegTx
	if err := protx.Parse(core.ExtraPayload, &pu); err != nil {
		t.Fatal(err)
	}
	return core, pu, RegistrarChange{PubKeyOperator: pu.PubKeyOperator, KeyIDVoting: pu.KeyIDVoting}
}

func TestRegistrarUpdateMatchesCore(t *testing.T) {
	m := coreMN(t)
	core, want, change := coreRegistrar(t)
	const fee = 469
	f := Funding{
		Inputs: []FundingInput{{
			OutPoint: core.TxIn[0].PreviousOutPoint,
			PkScript: p2pkh(9),
			Value:    core.TxOut[0].Value + fee,
		}},
		ChangeScript: core.TxOut[0].PkScript,
	}
	u, err := NewRegistrarUpdate(m, change, f, fee)
	if err != nil {
		t.Fatal(err)
	}
	if !u.OperatorChanged {
		t.Error("dashd's update sets another operator key")
	}
	if u.pu.InputsHash != want.InputsHash {
		t.Fatalf("inputs hash %s, dashd %s", u.pu.InputsHash, want.InputsHash)
	}
	h, _ := u.pu.PayloadHash()
	if got := "926d29ddf2b5c96edbbfdce85beace49935b5180cd9ebf8659172928732ead04"; h.String() != got {
		t.Fatalf("payload hash %s, dashd %s", h, got)
	}
	var sigs []ShareSig
	for i, s := range want.Sigs {
		sigs = append(sigs, ShareSig{i, base64.StdEncoding.EncodeToString(s[:])})
	}
	if err := u.AddSigs(sigs); err != nil {
		t.Fatalf("dashd's signatures do not verify against our digest: %v", err)
	}
	if err := u.Complete(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(u.tx.ExtraPayload, core.ExtraPayload) {
		t.Fatalf("payload\n got %x\nwant %x", u.tx.ExtraPayload, core.ExtraPayload)
	}
}

// TestReviewCorePrepared reviews the transaction Dash Core's
// shared_update_registrar_prepare made: its sequence is not final and its fee
// input is already signed, neither of which a signer of the keys cares about.
func TestReviewCorePrepared(t *testing.T) {
	m := coreMN(t)
	b, err := os.ReadFile(filepath.Join("testdata", "upreg_prepare.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Tx, SignHash string }
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(v.Tx)
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	_, _, change := coreRegistrar(t)

	u, err := ReviewRegistrarUpdate(tx, m, change)
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := u.pu.PayloadHash(); h.String() != v.SignHash {
		t.Fatalf("payload hash %s, dashd %s", h, v.SignHash)
	}
	if err := u.Complete(); err == nil {
		t.Fatal("Core's prepared transaction has no signatures yet")
	}
}

// updateFlow is a registrar update by the three owners, funded by owner 0.
func updateFlow(t *testing.T) (*SharedMN, []*btcec.PrivateKey, *RegistrarUpdate, RegistrarChange, Funding, *btcec.PrivateKey) {
	t.Helper()
	m, keys := updateMN(t)
	fund := testKey(0x88)
	f := upFunding(fund, 2, 1e8)
	c := RegistrarChange{PubKeyOperator: mustKey(t, operator2), KeyIDVoting: keyID(0x67)}
	u, err := NewRegistrarUpdate(m, c, f, 5000)
	if err != nil {
		t.Fatal(err)
	}
	return m, keys, u, c, f, fund
}

func TestRegistrarUpdateEndToEnd(t *testing.T) {
	m, keys, u, c, f, fund := updateFlow(t)
	if !u.OperatorChanged {
		t.Fatal("the operator key changed")
	}
	// Each owner reviews what it was sent and signs; the funder collects them.
	blob, err := u.Blob()
	if err != nil {
		t.Fatal(err)
	}
	if len(blob.Signatures) != 0 {
		t.Fatalf("a new update has %d signatures", len(blob.Signatures))
	}
	data, _ := json.Marshal(blob)
	var sigs []ShareSig
	for i, k := range keys {
		tx, _, err := ParseBlob(data)
		if err != nil {
			t.Fatal(err)
		}
		rv, err := ReviewRegistrarUpdate(tx, m, c)
		if err != nil {
			t.Fatalf("owner %d: %v", i, err)
		}
		s, err := rv.Sign(k, i)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	if err := u.SignInputs(f, 10_000, secrets{fund}); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("signed the inputs before the signatures were in: %v", err)
	}
	if err := u.AddSigs(sigs); err != nil {
		t.Fatal(err)
	}

	// The funder resumes from the blob, as it would after the round.
	blob, err = u.Blob()
	if err != nil {
		t.Fatal(err)
	}
	if len(blob.Signatures) != 3 {
		t.Fatalf("%d signatures in the blob", len(blob.Signatures))
	}
	data, _ = json.Marshal(blob)
	tx, _, err := ParseBlob(data)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := ReviewRegistrarUpdate(tx, m, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := rv.Complete(); err != nil {
		t.Fatal(err)
	}
	if err := rv.SignInputs(f, 10_000, secrets{fund}); err != nil {
		t.Fatal(err)
	}
	if got, want := signedInputs(rv.tx), reSigned(t, rv.tx, map[int][]byte{0: f.Inputs[0].PkScript, 1: f.Inputs[1].PkScript}, fund); !equalScripts(got, want) {
		t.Fatal("the inputs were not signed over the final payload")
	}
	raw, _ := rv.Raw()
	final := &bisonwire.Tx{Chain: "dash"}
	if err := final.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	var pu protx.ProUpSharedRegTx
	if err := protx.Parse(final.ExtraPayload, &pu); err != nil {
		t.Fatal(err)
	}
	h, _ := pu.PayloadHash()
	for i, s := range pu.Sigs {
		if err := protx.VerifyCompact(h, m.Shares[i].KeyIDOwner, s[:]); err != nil {
			t.Fatalf("signature %d: %v", i, err)
		}
	}
}

func TestReviewRegistrarUpdateRejects(t *testing.T) {
	m, keys, u, c, _, _ := updateFlow(t)
	mod := func(f func(tx *bisonwire.Tx, pu *protx.ProUpSharedRegTx)) *bisonwire.Tx {
		tx, err := cloneTx(u.tx)
		if err != nil {
			t.Fatal(err)
		}
		pu := u.pu
		pu.Sigs = append([][protx.CompactSigSize]byte(nil), u.pu.Sigs...)
		f(tx, &pu)
		var b bytes.Buffer
		if err := pu.Serialize(&b); err != nil {
			t.Fatal(err)
		}
		tx.ExtraPayload = b.Bytes()
		return tx
	}
	truncated, err := cloneTx(u.tx)
	if err != nil {
		t.Fatal(err)
	}
	truncated.ExtraPayload = truncated.ExtraPayload[:10]

	// A signature over the payload with another operator key in it.
	slot := func(i int, key *btcec.PrivateKey, mut func(pu *protx.ProUpSharedRegTx)) func(*bisonwire.Tx, *protx.ProUpSharedRegTx) {
		return func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) {
			if mut != nil {
				mut(pu)
			}
			h, _ := pu.PayloadHash()
			pu.Sigs[i] = protx.SignCompact(key, h)
		}
	}
	badKeyChange := c
	badKeyChange.KeyIDVoting = [20]byte{}
	otherOp := c
	otherOp.PubKeyOperator = mustKey(t, operatorKey)
	otherVote := c
	otherVote.KeyIDVoting = keyID(0x68)
	reuse := c
	reuse.KeyIDVoting = keyID(0x70) // share 0's reward script
	refundReuse := c
	refundReuse.KeyIDVoting = keyID(2) // share 1's refund script
	var zeroOp RegistrarChange
	zeroOp.KeyIDVoting = c.KeyIDVoting

	for _, tt := range []struct {
		name string
		tx   *bisonwire.Tx
		want RegistrarChange
		mn   *SharedMN
		err  string
	}{
		{"wrong type", mod(func(tx *bisonwire.Tx, _ *protx.ProUpSharedRegTx) { tx.Type = 11 }), c, m, "not a registrar"},
		{"wrong version", mod(func(tx *bisonwire.Tx, _ *protx.ProUpSharedRegTx) { tx.MsgTx.Version = 2 }), c, m, "not a registrar"},
		{"bad payload", truncated, c, m, "payload"},
		{"other masternode", mod(func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) { pu.ProTxHash[0] ^= 1 }), c, m, "another masternode"},
		{"operator key that was not agreed", u.tx, otherOp, m, "operator key is not"},
		{"voting key that was not agreed", u.tx, otherVote, m, "voting key is not"},
		{"invalid operator key", mod(func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) { pu.PubKeyOperator = [48]byte{1} }), RegistrarChange{[48]byte{1}, c.KeyIDVoting}, m, "operator-pubkey"},
		{"infinite operator key", mod(func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) { pu.PubKeyOperator = [48]byte{0xc0} }), RegistrarChange{[48]byte{0xc0}, c.KeyIDVoting}, m, "operator-pubkey"},
		{"empty voting key", mod(func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) { pu.KeyIDVoting = [20]byte{} }), badKeyChange, m, "key-null"},
		{"voting key paid by a reward script", mod(func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) { pu.KeyIDVoting = reuse.KeyIDVoting }), reuse, m, "payee-reuse"},
		{"voting key paid by a refund script", mod(func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) { pu.KeyIDVoting = refundReuse.KeyIDVoting }), refundReuse, m, "payee-reuse"},
		{"one signature slot short", mod(func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) { pu.Sigs = pu.Sigs[:2] }), c, m, "sig-count"},
		{"one signature slot over", mod(func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) { pu.Sigs = append(pu.Sigs, [65]byte{}) }), c, m, "sig-count"},
		{"inputs hash", mod(func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) { pu.InputsHash[0] ^= 1 }), c, m, "inputs-hash"},
		{"no inputs", mod(func(tx *bisonwire.Tx, pu *protx.ProUpSharedRegTx) {
			tx.TxIn = nil
			pu.InputsHash = protx.InputsHash(nil)
		}), c, m, "inputs-hash"},
		{"garbage signature", mod(func(_ *bisonwire.Tx, pu *protx.ProUpSharedRegTx) { pu.Sigs[0] = [65]byte{27, 1} }), c, m, "signature 0"},
		{"signature by another share", mod(slot(0, keys[1], nil)), c, m, "signature 0"},
		{"signature over other keys", mod(func(tx *bisonwire.Tx, pu *protx.ProUpSharedRegTx) {
			h, _ := pu.PayloadHash()
			pu.Sigs[2] = protx.SignCompact(keys[2], h)
			pu.KeyIDVoting = keyID(0x69) // changed after the share signed
		}), RegistrarChange{c.PubKeyOperator, keyID(0x69)}, m, "signature 2"},
		{"too few shares", u.tx, c, &SharedMN{ProTxHash: m.ProTxHash, Shares: m.Shares[:1]}, "1 shares, not"},
		{"too many shares", u.tx, c, &SharedMN{ProTxHash: m.ProTxHash, Shares: append(append([]protx.Share(nil), m.Shares...), make([]protx.Share, 6)...)}, "9 shares, not"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReviewRegistrarUpdate(tt.tx, tt.mn, tt.want)
			if !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("got %v, want %q", err, tt.err)
			}
		})
	}

	// What it must accept: a signature already in its slot.
	ok := mod(slot(1, keys[1], nil))
	rv, err := ReviewRegistrarUpdate(ok, m, c)
	if err != nil {
		t.Fatalf("a valid signature already in the update: %v", err)
	}
	if rv.pu.Sigs[1] == ([65]byte{}) {
		t.Fatal("the signature in the update was dropped")
	}
	// A key the owners agreed to that is its own current operator key is not a
	// change.
	same := RegistrarChange{PubKeyOperator: m.PubKeyOperator, KeyIDVoting: keyID(0x67)}
	noop, err := NewRegistrarUpdate(m, same, upFunding(testKey(0x88), 1, 1e8), 5000)
	if err != nil {
		t.Fatal(err)
	}
	if noop.OperatorChanged {
		t.Error("the operator key did not change")
	}
}

func TestReviewRegistrarUpdateDoesNotModify(t *testing.T) {
	m, keys, u, c, _, _ := updateFlow(t)
	s, _ := u.Sign(keys[0], 0)
	if err := u.AddSigs([]ShareSig{s}); err != nil {
		t.Fatal(err)
	}
	before, _ := u.Raw()
	rv, err := ReviewRegistrarUpdate(u.tx, m, c)
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := rv.Sign(keys[1], 1)
	if err := rv.AddSigs([]ShareSig{s1}); err != nil {
		t.Fatal(err)
	}
	if after, _ := u.Raw(); !bytes.Equal(before, after) {
		t.Fatal("the review changed the transaction it was given")
	}
}

func TestRegistrarUpdateSigning(t *testing.T) {
	_, keys, u, _, _, _ := updateFlow(t)
	if _, err := u.Sign(keys[0], 1); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("signed as another share: %v", err)
	}
	for _, i := range []int{-1, 3} {
		if _, err := u.Sign(keys[0], i); !errors.Is(err, ErrInvalidUpdate) {
			t.Fatalf("signed as share %d: %v", i, err)
		}
	}
	good := make([]ShareSig, 3)
	for i := range keys {
		var err error
		if good[i], err = u.Sign(keys[i], i); err != nil {
			t.Fatal(err)
		}
	}

	// None is added if one is bad.
	for name, bad := range map[string]ShareSig{
		"a signature by another share": {ShareIndex: 2, Signature: good[0].Signature},
		"an index past the shares":     {ShareIndex: 3, Signature: good[0].Signature},
		"a negative index":             {ShareIndex: -1, Signature: good[0].Signature},
		"not base64":                   {ShareIndex: 2, Signature: "***"},
		"a short signature":            {ShareIndex: 2, Signature: base64.StdEncoding.EncodeToString([]byte{1})},
	} {
		before, _ := u.Raw()
		if err := u.AddSigs([]ShareSig{good[1], bad}); !errors.Is(err, ErrInvalidUpdate) {
			t.Fatalf("%s: %v", name, err)
		}
		if after, _ := u.Raw(); !bytes.Equal(before, after) {
			t.Fatalf("%s: a signature was added though another was bad", name)
		}
	}

	if err := u.AddSigs(good[:2]); err != nil {
		t.Fatal(err)
	}
	if err := u.Complete(); !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), "signature 2") {
		t.Fatalf("complete with a signature missing: %v", err)
	}
	b, _ := u.Blob()
	if len(b.Signatures) != 2 || b.Signatures[0].ShareIndex != 0 || b.Signatures[1].ShareIndex != 1 {
		t.Fatalf("blob signatures %+v", b.Signatures)
	}
	if err := u.AddSigs(good[2:]); err != nil {
		t.Fatal(err)
	}
	if err := u.Complete(); err != nil {
		t.Fatal(err)
	}
	// Signing is the same whatever the signatures already in the transaction.
	again, err := u.Sign(keys[2], 2)
	if err != nil || again != good[2] {
		t.Fatalf("a signature depends on the others in the update: %v", err)
	}
}

func TestRegistrarUpdateSignInputs(t *testing.T) {
	m, keys, _, c, f, fund := updateFlow(t)
	sign := func(u *RegistrarUpdate) {
		t.Helper()
		var sigs []ShareSig
		for i, k := range keys {
			s, err := u.Sign(k, i)
			if err != nil {
				t.Fatal(err)
			}
			sigs = append(sigs, s)
		}
		if err := u.AddSigs(sigs); err != nil {
			t.Fatal(err)
		}
	}
	fresh := func() *RegistrarUpdate {
		u, err := NewRegistrarUpdate(m, c, f, 5000)
		if err != nil {
			t.Fatal(err)
		}
		sign(u)
		return u
	}
	foreign := func(f Funding) Funding {
		f.Inputs = append([]FundingInput(nil), f.Inputs...)
		f.Inputs[1].OutPoint.Hash = chainhash.Hash{0xee}
		return f
	}
	dearer := func(f Funding) Funding {
		f.Inputs = append([]FundingInput(nil), f.Inputs...)
		f.Inputs[0].Value += 100_000
		return f
	}
	sameOut := func(f Funding) Funding {
		f.Inputs = append([]FundingInput(nil), f.Inputs...)
		f.Inputs[1].OutPoint = f.Inputs[0].OutPoint
		return f
	}

	for _, tt := range []struct {
		name string
		u    func() *RegistrarUpdate
		f    Funding
		max  int64
		err  string
	}{
		{"a fee over the maximum", fresh, f, 4999, "fee 5000"},
		{"a zero maximum", fresh, f, 0, "maximum fee"},
		{"a maximum over the cap", fresh, f, MaxUpdateFee + 1, "maximum fee"},
		{"an input that is not the transaction's", fresh, foreign(f), 5000, "does not spend"},
		{"the same input twice", fresh, sameOut(f), 5000, "listed twice"},
		{"a funder that holds more than the fee implies", fresh, dearer(f), 5000, "fee"},
		{"fewer inputs than the transaction spends", fresh, Funding{Inputs: f.Inputs[:1], ChangeScript: f.ChangeScript}, 5000, "spends 2 inputs"},
		{"another change script", fresh, Funding{Inputs: f.Inputs, ChangeScript: scriptFor(testKey(0x12))}, 5000, "change script"},
		{"a redirected change", func() *RegistrarUpdate {
			u := fresh()
			// Anyone relaying the transaction moves the change to themselves.
			// The share signatures do not cover outputs, so they still verify.
			u.tx.TxOut[0].PkScript = scriptFor(testKey(0x13))
			if err := u.Complete(); err != nil {
				t.Fatal(err)
			}
			return u
		}, f, 5000, "change script"},
		{"an output added", func() *RegistrarUpdate {
			u := fresh()
			u.tx.AddTxOut(wire.NewTxOut(1000, f.ChangeScript))
			u.tx.TxOut[0].Value -= 1000
			u.tx.AddTxOut(wire.NewTxOut(1, scriptFor(testKey(0x13))))
			return u
		}, f, 5000, "change script"},
		{"no outputs", func() *RegistrarUpdate {
			u := fresh()
			u.tx.TxOut = nil
			return u
		}, f, 5000, "no outputs"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := tt.u()
			before, _ := u.Raw()
			err := u.SignInputs(tt.f, tt.max, secrets{fund})
			if !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("got %v, want %q", err, tt.err)
			}
			if after, _ := u.Raw(); !bytes.Equal(before, after) {
				t.Fatal("a refused SignInputs changed the transaction")
			}
		})
	}

	// The exact fee is allowed, and so is a funder holding a different set of
	// inputs in another order.
	u := fresh()
	rev := f
	rev.Inputs = []FundingInput{f.Inputs[1], f.Inputs[0]}
	if err := u.SignInputs(rev, 5000, secrets{fund}); err != nil {
		t.Fatalf("the exact maximum fee, inputs in another order: %v", err)
	}
	for i, in := range u.tx.TxIn {
		if len(in.SignatureScript) == 0 {
			t.Fatalf("input %d is not signed", i)
		}
	}
}

// A script that is not P2PKH is never a key, whatever its bytes say.
func TestRegistrarVotingKeyIgnoresOtherScripts(t *testing.T) {
	m, _ := updateMN(t)
	c := RegistrarChange{PubKeyOperator: m.PubKeyOperator, KeyIDVoting: keyID(0x67)}
	odd := append([]byte{0x00, 0x00, 0x14}, bytes.Repeat([]byte{0x67}, 20)...)
	odd = append(odd, 0x00, 0x00) // 25 bytes, the voting id where a key hash would be
	m.Shares[0].RewardScript = odd
	if err := m.checkRegistrar(c); err != nil {
		t.Fatalf("a script that pays no key reused the voting key: %v", err)
	}
	m.Shares[0].RewardScript = p2pkh(0x67)
	if err := m.checkRegistrar(c); err == nil {
		t.Fatal("a P2PKH to the voting key was not refused")
	}
}

func TestUpdateShapes(t *testing.T) {
	m, keys := updateMN(t)
	reward := append([]byte(nil), scriptFor(testKey(0x99))...)
	f := upFunding(keys[0], 2, 1e8)
	su, err := NewShareUpdate(m, 0, reward, f, 5000)
	if err != nil {
		t.Fatal(err)
	}
	ru, err := NewRegistrarUpdate(m, RegistrarChange{m.PubKeyOperator, keyID(0x67)}, f, 5000)
	if err != nil {
		t.Fatal(err)
	}
	for name, tx := range map[string]*bisonwire.Tx{"share update": su.tx, "registrar update": ru.tx} {
		if tx.LockTime != 0 || tx.MsgTx.Version != txVersion {
			t.Errorf("%s: version %d lock time %d", name, tx.MsgTx.Version, tx.LockTime)
		}
		if len(tx.TxIn) != 2 || len(tx.TxOut) != 1 {
			t.Fatalf("%s: %d inputs, %d outputs", name, len(tx.TxIn), len(tx.TxOut))
		}
		if want := int64(1e8 - 5000); tx.TxOut[0].Value != want || !bytes.Equal(tx.TxOut[0].PkScript, f.ChangeScript) {
			t.Errorf("%s: change %d", name, tx.TxOut[0].Value)
		}
		for i, in := range tx.TxIn {
			if in.Sequence != finalSequence || in.PreviousOutPoint != f.Inputs[i].OutPoint {
				t.Errorf("%s: input %d", name, i)
			}
		}
	}
	// The update owns its reward script.
	reward[5] ^= 0xff
	if bytes.Equal(su.pu.ScriptReward, reward) {
		t.Error("the update shares memory with the caller's reward script")
	}
	if su.tx.Type != txTypeProUpShare || ru.tx.Type != txTypeProUpSharedReg {
		t.Error("transaction types")
	}
}

// Reviewing an update that changes nothing reports no operator change.
func TestReviewOperatorChanged(t *testing.T) {
	m, _ := updateMN(t)
	same := RegistrarChange{PubKeyOperator: m.PubKeyOperator, KeyIDVoting: keyID(0x67)}
	u, err := NewRegistrarUpdate(m, same, upFunding(testKey(0x88), 1, 1e8), 5000)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := ReviewRegistrarUpdate(u.tx, m, same)
	if err != nil {
		t.Fatal(err)
	}
	if rv.OperatorChanged {
		t.Error("the operator key is the current one")
	}
	other := RegistrarChange{PubKeyOperator: mustKey(t, operator2), KeyIDVoting: keyID(0x67)}
	u2, err := NewRegistrarUpdate(m, other, upFunding(testKey(0x88), 1, 1e8), 5000)
	if err != nil {
		t.Fatal(err)
	}
	if rv, err = ReviewRegistrarUpdate(u2.tx, m, other); err != nil || !rv.OperatorChanged {
		t.Errorf("the operator key changed: %v", err)
	}
}

// The edges of what the funder signs.
func TestSignInputsLimits(t *testing.T) {
	m, keys := updateMN(t)
	fund := testKey(0x88)
	c := RegistrarChange{PubKeyOperator: mustKey(t, operator2), KeyIDVoting: keyID(0x67)}
	build := func(f Funding) *RegistrarUpdate {
		u, err := NewRegistrarUpdate(m, c, f, 5000)
		if err != nil {
			t.Fatal(err)
		}
		var sigs []ShareSig
		for i, k := range keys {
			s, _ := u.Sign(k, i)
			sigs = append(sigs, s)
		}
		if err := u.AddSigs(sigs); err != nil {
			t.Fatal(err)
		}
		return u
	}
	f := upFunding(fund, 1, 1e8)

	// A zero value output, beside one that makes the fee what it should be.
	u := build(f)
	u.tx.TxOut = []*wire.TxOut{wire.NewTxOut(0, f.ChangeScript), wire.NewTxOut(1e8-5000, f.ChangeScript)}
	if err := u.SignInputs(f, 5000, secrets{fund}); !errors.Is(err, ErrInvalidUpdate) {
		t.Errorf("signed an update with a zero output: %v", err)
	}
	// No fee at all.
	u = build(f)
	u.tx.TxOut[0].Value += 5000
	if err := u.SignInputs(f, 5000, secrets{fund}); !errors.Is(err, ErrInvalidUpdate) {
		t.Errorf("signed an update with no fee: %v", err)
	}
	// A funder input of no value.
	u = build(f)
	zero := f
	zero.Inputs = []FundingInput{{OutPoint: f.Inputs[0].OutPoint, PkScript: f.Inputs[0].PkScript}}
	if err := u.SignInputs(zero, 5000, secrets{fund}); !errors.Is(err, ErrInvalidUpdate) {
		t.Errorf("signed with an input of no value: %v", err)
	}
	// An input claimed to hold nothing, beside another that makes up the sum.
	two := upFunding(fund, 2, 1e8)
	u = build(two)
	claim := two
	claim.Inputs = []FundingInput{two.Inputs[0], two.Inputs[1]}
	claim.Inputs[0].Value, claim.Inputs[1].Value = 1e8, 0
	if err := u.SignInputs(claim, 5000, secrets{fund}); !errors.Is(err, ErrInvalidUpdate) {
		t.Errorf("signed with an input claimed to hold nothing: %v", err)
	}
	// The largest input and the largest maximum fee are allowed.
	big := upFunding(fund, 1, maxMoney)
	u = build(big)
	if err := u.SignInputs(big, MaxUpdateFee, secrets{fund}); err != nil {
		t.Errorf("an input of the whole supply, the largest maximum fee: %v", err)
	}
}

// Dash Core lets the voting key be an owner's key; only refund and reward
// scripts may not pay it. A rule added here would refuse registrars Core accepts.
func TestRegistrarVotingKeyMayBeAnOwnerKey(t *testing.T) {
	m, keys := updateMN(t)
	c := RegistrarChange{PubKeyOperator: mustKey(t, operator2), KeyIDVoting: idFor(keys[1])}
	if _, err := NewRegistrarUpdate(m, c, upFunding(testKey(0x88), 1, 1e8), 5000); err != nil {
		t.Fatalf("an owner's key as the voting key: %v", err)
	}
}

func TestNewShareUpdateNeedsTheVotingKey(t *testing.T) {
	m, keys := updateMN(t)
	m.KeyIDVoting = [20]byte{}
	if _, err := NewShareUpdate(m, 0, scriptFor(testKey(0x99)), upFunding(keys[0], 1, 1e8), 5000); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("built an update without the voting key to check against: %v", err)
	}
}

// A signature that differs from one in the update would invalidate the funder's
// input signatures, so it is refused, while a repeat is harmless.
func TestAddSigsDoesNotReplace(t *testing.T) {
	_, keys, u, _, _, _ := updateFlow(t)
	s, err := u.Sign(keys[0], 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.AddSigs([]ShareSig{s}); err != nil {
		t.Fatal(err)
	}
	if err := u.AddSigs([]ShareSig{s}); err != nil {
		t.Fatalf("a repeat of a signature in the update: %v", err)
	}
	// The update holds another signature in the slot, as one by a signer with
	// random nonces would be, so a signature that differs must not replace it.
	u.pu.Sigs[0][1] ^= 1
	if err := u.sync(); err != nil {
		t.Fatal(err)
	}
	before, _ := u.Raw()
	if err := u.AddSigs([]ShareSig{s}); !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), "already signed") {
		t.Fatalf("replaced a signature: %v", err)
	}
	if after, _ := u.Raw(); !bytes.Equal(before, after) {
		t.Fatal("a refused signature changed the update")
	}
}

func TestSignInputsOutputsOverflow(t *testing.T) {
	m, keys := updateMN(t)
	fund := testKey(0x88)
	c := RegistrarChange{PubKeyOperator: mustKey(t, operator2), KeyIDVoting: keyID(0x67)}
	f := upFunding(fund, 1, 1e8)
	u, err := NewRegistrarUpdate(m, c, f, 5000)
	if err != nil {
		t.Fatal(err)
	}
	var sigs []ShareSig
	for i, k := range keys {
		s, _ := u.Sign(k, i)
		sigs = append(sigs, s)
	}
	if err := u.AddSigs(sigs); err != nil {
		t.Fatal(err)
	}
	// Enough outputs that their sum wraps around to just what makes a fee of
	// 5000, which an unchecked sum would sign.
	u.tx.TxOut = nil
	for range 8784 {
		u.tx.AddTxOut(wire.NewTxOut(maxMoney, f.ChangeScript))
	}
	u.tx.AddTxOut(wire.NewTxOut(344073809546616, f.ChangeScript))
	if err := u.SignInputs(f, 5000, secrets{fund}); err == nil || !strings.Contains(err.Error(), "outputs pay too much") {
		t.Fatalf("signed a transaction whose outputs overflow: %v", err)
	}

	// And the same for inputs the funder claims.
	u.tx.TxOut = []*wire.TxOut{wire.NewTxOut(1e8-5000, f.ChangeScript)}
	big := Funding{ChangeScript: f.ChangeScript}
	u.tx.TxIn = nil
	add := func(v int64, i int) {
		op := wire.OutPoint{Hash: chainhash.Hash{0xcc, byte(i), byte(i >> 8)}, Index: uint32(i)}
		u.tx.AddTxIn(wire.NewTxIn(&op, nil, nil))
		big.Inputs = append(big.Inputs, FundingInput{OutPoint: op, PkScript: f.Inputs[0].PkScript, Value: v})
	}
	for i := range 8784 {
		add(maxMoney, i)
	}
	add(344073809551616, 8784)
	if err := u.SignInputs(big, 5000, secrets{fund}); err == nil || !strings.Contains(err.Error(), "inputs hold too much") {
		t.Fatalf("signed with inputs whose sum overflows: %v", err)
	}
}
