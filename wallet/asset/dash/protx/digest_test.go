// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package protx

import (
	"bytes"
	"errors"
	"math/big"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// The shared masternode vectors come from one registration (regshared) that
// dashd v24.0.0 built and its two share owners signed, then updated and
// dissolved. So the owner key ids that dis, dis_standby, upshare and
// upsharedreg must verify against are the share table of regshared.

// sharedRegistration returns the registration the other vectors act on: its
// share table and its txid, which is their proTxHash.
func sharedRegistration(t *testing.T) (*ProRegTx, *bisonwire.Tx) {
	t.Helper()

	_, tx := loadVector(t, "regshared")
	reg := new(ProRegTx)
	if err := Parse(tx.ExtraPayload, reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Shares) != 2 {
		t.Fatalf("%d shares, want 2", len(reg.Shares))
	}
	return reg, tx
}

// TestSharedRegConsentHash is a known-answer test: every join signature dashd
// produced must verify against the digest, for the share it is in the table,
// and the digest equals what dashd printed. A digest that bound the wrong
// fields or the wrong order would recover another key.
func TestSharedRegConsentHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		vector string
		// want is the digest as dashd prints it (byte reversed).
		want string
	}{
		// regshared's is the consentHash dashd returned from
		// shared_register_prepare.
		{"regshared", "59dca701984db15bc6f7164d90c587f77b3f0c07ffb6f7704a77f5fc7cddac6d"},
		{"regshared_nonet", "472ab869f2392cc6f21d43cf69a9edcacc6d03e818a240cf5f6ad823445ffaa6"},
	}
	for _, tc := range tests {
		t.Run(tc.vector, func(t *testing.T) {
			t.Parallel()

			_, tx := loadVector(t, tc.vector)
			var p ProRegTx
			if err := Parse(tx.ExtraPayload, &p); err != nil {
				t.Fatal(err)
			}
			got, err := p.SharedRegConsentHash(tx)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != tc.want {
				t.Errorf("consent hash %s, dashd %s", got, tc.want)
			}
			for i, s := range p.Shares {
				if err := VerifyCompact(got, s.KeyIDOwner, s.JoinSig[:]); err != nil {
					t.Errorf("join signature %d: %v", i, err)
				}
			}
		})
	}
}

// TestSharedRegConsentHashBinding pins which fields the consent digest covers,
// and why each matters: a signer consents to exactly what is bound, so a field
// that is not bound can be changed after every participant has signed. The
// unbound rows document what Dash Core leaves out, which the signer must check
// for itself.
func TestSharedRegConsentHashBinding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(p *ProRegTx, tx *wire.MsgTx)
		bound  bool
	}{
		// What every participant consents to.
		{"input outpoint: which funds are spent", func(p *ProRegTx, tx *wire.MsgTx) { tx.TxIn[0].PreviousOutPoint.Index++ }, true},
		{"input sequence: a BIP68 sequence would lock the registration", func(p *ProRegTx, tx *wire.MsgTx) { tx.TxIn[0].Sequence ^= 1 }, true},
		{"change output value: the change a participant gets back", func(p *ProRegTx, tx *wire.MsgTx) { tx.TxOut[0].Value-- }, true},
		{"change output script: where the change goes", func(p *ProRegTx, tx *wire.MsgTx) { tx.TxOut[0].PkScript[3] ^= 1 }, true},
		{"collateral output value: how much is locked", func(p *ProRegTx, tx *wire.MsgTx) { tx.TxOut[1].Value-- }, true},
		{"lock time", func(p *ProRegTx, tx *wire.MsgTx) { tx.LockTime++ }, true},
		{"transaction version", func(p *ProRegTx, tx *wire.MsgTx) { tx.Version = 2 }, true},
		{"mode", func(p *ProRegTx, tx *wire.MsgTx) { p.Mode = 1 }, true},
		{"masternode type", func(p *ProRegTx, tx *wire.MsgTx) { p.MNType = TypeEvo }, true},
		{"netInfo: where the node runs", func(p *ProRegTx, tx *wire.MsgTx) { p.NetInfo.Purposes[0].Entries[0].Port++ }, true},
		{"voting key: who votes", func(p *ProRegTx, tx *wire.MsgTx) { p.KeyIDVoting[0] ^= 1 }, true},
		{"operator key: who runs the node", func(p *ProRegTx, tx *wire.MsgTx) { p.PubKeyOperator[0] ^= 1 }, true},
		{"operator reward: the operator's cut", func(p *ProRegTx, tx *wire.MsgTx) { p.OperatorReward++ }, true},
		{"share amount: how much a participant contributes", func(p *ProRegTx, tx *wire.MsgTx) { p.Shares[0].Amount++ }, true},
		{"share refund script: where the principal returns", func(p *ProRegTx, tx *wire.MsgTx) { p.Shares[0].RefundScript[3] ^= 1 }, true},
		{"share reward script: where the rewards go", func(p *ProRegTx, tx *wire.MsgTx) { p.Shares[0].RewardScript[3] ^= 1 }, true},
		{"share owner key: who controls the share", func(p *ProRegTx, tx *wire.MsgTx) { p.Shares[0].KeyIDOwner[0] ^= 1 }, true},
		{"share order: which share is which", func(p *ProRegTx, tx *wire.MsgTx) { p.Shares[0], p.Shares[1] = p.Shares[1], p.Shares[0] }, true},
		{"share count", func(p *ProRegTx, tx *wire.MsgTx) { p.Shares = p.Shares[:1] }, true},
		{"early period: how long an exit is penalised", func(p *ProRegTx, tx *wire.MsgTx) { p.EarlyPeriodBlocks++ }, true},
		{"early penalty: what an early exit costs", func(p *ProRegTx, tx *wire.MsgTx) { p.EarlyPenalty++ }, true},

		// What Dash Core leaves out. A signer has to check these itself.
		{"collateral index is not bound", func(p *ProRegTx, tx *wire.MsgTx) { p.Collateral.Index++ }, false},
		{"join signatures are not bound: they are what is being made", func(p *ProRegTx, tx *wire.MsgTx) { p.Shares[0].JoinSig[0] ^= 1 }, false},
		{"owner key id is not bound: a shared registration has none", func(p *ProRegTx, tx *wire.MsgTx) { p.KeyIDOwner[0] ^= 1 }, false},
		{"payload signature is not bound", func(p *ProRegTx, tx *wire.MsgTx) { p.PayloadSig = []byte{1} }, false},
		{"payload inputs hash is not bound: consensus ties it to the inputs", func(p *ProRegTx, tx *wire.MsgTx) { p.InputsHash[0] ^= 1 }, false},
		{"input signature scripts are not bound", func(p *ProRegTx, tx *wire.MsgTx) { tx.TxIn[0].SignatureScript = []byte{1, 2, 3} }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, tx := loadVector(t, "regshared")
			var base ProRegTx
			if err := Parse(tx.ExtraPayload, &base); err != nil {
				t.Fatal(err)
			}
			want, err := base.SharedRegConsentHash(tx)
			if err != nil {
				t.Fatal(err)
			}

			var p ProRegTx
			if err := Parse(tx.ExtraPayload, &p); err != nil {
				t.Fatal(err)
			}
			msg := *tx
			msg.MsgTx = *tx.MsgTx.Copy()
			tc.mutate(&p, &msg.MsgTx)
			got, err := p.SharedRegConsentHash(&msg)
			if err != nil {
				t.Fatal(err)
			}
			if changed := got != want; changed != tc.bound {
				t.Fatalf("digest changed = %v, want %v", changed, tc.bound)
			}
		})
	}
}

func TestSharedRegConsentHashRefuses(t *testing.T) {
	t.Parallel()

	// A digest is for one transaction type: it must not be valid for another.
	_, typed := loadVector(t, "regshared")
	var reg ProRegTx
	if err := Parse(typed.ExtraPayload, &reg); err != nil {
		t.Fatal(err)
	}
	wrong := *typed
	wrong.Type = txTypeProDis
	if _, err := reg.SharedRegConsentHash(&wrong); !errors.Is(err, ErrWrongTxType) {
		t.Errorf("wrong type: %v, want ErrWrongTxType", err)
	}

	_, v2tx := loadVector(t, "regfund")
	var v2 ProRegTx
	if err := Parse(v2tx.ExtraPayload, &v2); err != nil {
		t.Fatal(err)
	}
	if _, err := v2.SharedRegConsentHash(v2tx); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("version 2: %v, want ErrUnsupportedVersion", err)
	}

	// An EvoNode registration at version 3 has no shares.
	_, evoTx := loadVector(t, "regevo")
	var evo ProRegTx
	if err := Parse(evoTx.ExtraPayload, &evo); err != nil {
		t.Fatal(err)
	}
	if _, err := evo.SharedRegConsentHash(evoTx); err == nil {
		t.Error("no shares: expected an error")
	}
}

// TestSharedRegConsentHashPlatformNodeID covers the EvoNode branch of the
// consent digest, which no real vector reaches (Dash Core v24.0.0 does not allow
// shares on an EvoNode, so its source is the only reference): the Platform node
// id is bound for an EvoNode and ignored for a regular masternode, as Dash Core
// writes it only for an EvoNode.
func TestSharedRegConsentHashPlatformNodeID(t *testing.T) {
	t.Parallel()

	_, tx := loadVector(t, "regshared")
	hash := func(mnType uint16, id byte) chainhash.Hash {
		t.Helper()
		var p ProRegTx
		if err := Parse(tx.ExtraPayload, &p); err != nil {
			t.Fatal(err)
		}
		p.MNType = mnType
		p.PlatformNodeID[0] = id
		h, err := p.SharedRegConsentHash(tx)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	if hash(TypeEvo, 0) == hash(TypeEvo, 1) {
		t.Error("the Platform node id is not bound for an EvoNode")
	}
	if hash(TypeRegular, 0) != hash(TypeRegular, 1) {
		t.Error("the Platform node id changes the digest of a regular masternode")
	}
}

// TestProDisTxSignHash is a known-answer test: the signatures dashd put on a
// unanimous dissolution (one per share, in order) and on a unilateral one (by
// the actor share) must verify against the digest for the owners of the share
// table.
func TestProDisTxSignHash(t *testing.T) {
	t.Parallel()

	reg, regTx := sharedRegistration(t)
	tests := []struct {
		vector string
		want   string
	}{
		{"dis", "30dc7e5d97abb1995266d820064bed79e968f7d063ca5b9eb5879bbff6a42740"},
		{"dis_standby", "541d661cb87fb64e02119566d2f2ea16b3ad5ce1f10779c83d0f161c82e90249"},
	}
	for _, tc := range tests {
		t.Run(tc.vector, func(t *testing.T) {
			t.Parallel()

			_, tx := loadVector(t, tc.vector)
			var p ProDisTx
			if err := Parse(tx.ExtraPayload, &p); err != nil {
				t.Fatal(err)
			}
			if p.ProTxHash != regTx.TxHash() {
				t.Fatalf("proTxHash %s is not the registration %s", p.ProTxHash, regTx.TxHash())
			}
			got, err := p.SignHash(tx)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != tc.want {
				t.Errorf("sign hash %s, dashd %s", got, tc.want)
			}

			for i, sig := range p.Sigs {
				// One signature is the actor's, and one per share is in share
				// order. The standby vector's actor is share 0, which is also
				// its signature's position, so it cannot tell the two mappings
				// apart: that the actor's key signs a unilateral dissolution
				// rests on Dash Core's specialtxman.cpp, which verifies it
				// against shares[actorIndex].
				owner := reg.Shares[i].KeyIDOwner
				if len(p.Sigs) == 1 {
					owner = reg.Shares[p.ActorIndex].KeyIDOwner
				}
				if err := VerifyCompact(got, owner, sig[:]); err != nil {
					t.Errorf("signature %d: %v", i, err)
				}
			}
		})
	}
}

// TestProDisTxSignHashBinding pins what a dissolution signature covers, and
// why: the signer consents to where the principal goes, so the outputs and the
// funds spent are bound, and the signature count is bound so a unanimous
// signature cannot be reused as a unilateral one.
func TestProDisTxSignHashBinding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(p *ProDisTx, tx *wire.MsgTx)
	}{
		{"signature count selects unilateral or unanimous", func(p *ProDisTx, tx *wire.MsgTx) { p.Sigs = p.Sigs[:1] }},
		{"actor: who pays the fee and is penalised", func(p *ProDisTx, tx *wire.MsgTx) { p.ActorIndex ^= 1 }},
		{"proTxHash: which masternode", func(p *ProDisTx, tx *wire.MsgTx) { p.ProTxHash[0] ^= 1 }},
		{"output value: what a share is refunded", func(p *ProDisTx, tx *wire.MsgTx) { tx.TxOut[0].Value-- }},
		{"output script: where a share is refunded", func(p *ProDisTx, tx *wire.MsgTx) { tx.TxOut[0].PkScript[3] ^= 1 }},
		{"input outpoint: which collateral is spent", func(p *ProDisTx, tx *wire.MsgTx) { tx.TxIn[0].PreviousOutPoint.Index++ }},
		{"input sequence", func(p *ProDisTx, tx *wire.MsgTx) { tx.TxIn[0].Sequence ^= 1 }},
		{"lock time", func(p *ProDisTx, tx *wire.MsgTx) { tx.LockTime++ }},
		{"transaction version", func(p *ProDisTx, tx *wire.MsgTx) { tx.Version = 2 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, tx := loadVector(t, "dis")
			var base ProDisTx
			if err := Parse(tx.ExtraPayload, &base); err != nil {
				t.Fatal(err)
			}
			want, err := base.SignHash(tx)
			if err != nil {
				t.Fatal(err)
			}

			p := base
			msg := *tx
			msg.MsgTx = *tx.MsgTx.Copy()
			tc.mutate(&p, &msg.MsgTx)
			got, err := p.SignHash(&msg)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatal("digest did not change")
			}
		})
	}

	// The signatures themselves are not covered: they are what is made.
	_, tx := loadVector(t, "dis")
	var p ProDisTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	want, err := p.SignHash(tx)
	if err != nil {
		t.Fatal(err)
	}
	p.Sigs[0][0] ^= 1
	got, err := p.SignHash(tx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Error("the digest depends on a signature")
	}
}

// TestSharedPayloadHashes is a known-answer test for types 11 and 12: the
// signature of a share update is by the owner of that share, and a registrar
// update is signed by every owner in share order. The hash is also the double
// SHA-256 of the bytes as received without the signatures, which is the rule
// that a hash over a foreign payload covers the received bytes.
func TestSharedPayloadHashes(t *testing.T) {
	t.Parallel()

	reg, _ := sharedRegistration(t)

	t.Run("ProUpShareTx", func(t *testing.T) {
		t.Parallel()

		_, tx := loadVector(t, "upshare")
		var p ProUpShareTx
		if err := Parse(tx.ExtraPayload, &p); err != nil {
			t.Fatal(err)
		}
		got, err := p.PayloadHash()
		if err != nil {
			t.Fatal(err)
		}
		if want := "fae2e65e21384e65b17f62629a0283f7fcba176bcf42841e8e6e58ef07a15664"; got.String() != want {
			t.Errorf("payload hash %s, dashd %s", got, want)
		}
		if err := VerifyCompact(got, reg.Shares[p.ShareIndex].KeyIDOwner, p.Sig); err != nil {
			t.Errorf("signature: %v", err)
		}

		// The raw bytes without the trailing signature (a length byte and
		// 65 bytes).
		raw := tx.ExtraPayload[:len(tx.ExtraPayload)-1-CompactSigSize]
		if got != chainhash.DoubleHashH(raw) {
			t.Error("payload hash is not the hash of the received bytes without the signature")
		}

		// Everything but the signature is covered.
		for name, mutate := range map[string]func(q *ProUpShareTx){
			"proTxHash":    func(q *ProUpShareTx) { q.ProTxHash[0] ^= 1 },
			"share index":  func(q *ProUpShareTx) { q.ShareIndex ^= 1 },
			"reward":       func(q *ProUpShareTx) { q.ScriptReward[3] ^= 1 },
			"inputs hash":  func(q *ProUpShareTx) { q.InputsHash[0] ^= 1 },
			"payload only": func(q *ProUpShareTx) {},
		} {
			q := p
			q.ScriptReward = bytes.Clone(p.ScriptReward)
			mutate(&q)
			h, _ := q.PayloadHash()
			if changed := h != got; changed != (name != "payload only") {
				t.Errorf("%s: digest changed = %v", name, changed)
			}
		}
		q := p
		q.Sig = bytes.Clone(p.Sig)
		q.Sig[0] ^= 1
		if h, _ := q.PayloadHash(); h != got {
			t.Error("the digest depends on the signature")
		}
	})

	t.Run("ProUpSharedRegTx", func(t *testing.T) {
		t.Parallel()

		_, tx := loadVector(t, "upsharedreg")
		var p ProUpSharedRegTx
		if err := Parse(tx.ExtraPayload, &p); err != nil {
			t.Fatal(err)
		}
		got, err := p.PayloadHash()
		if err != nil {
			t.Fatal(err)
		}
		if want := "926d29ddf2b5c96edbbfdce85beace49935b5180cd9ebf8659172928732ead04"; got.String() != want {
			t.Errorf("payload hash %s, dashd %s", got, want)
		}
		if len(p.Sigs) != len(reg.Shares) {
			t.Fatalf("%d signatures for %d shares", len(p.Sigs), len(reg.Shares))
		}
		for i, sig := range p.Sigs {
			if err := VerifyCompact(got, reg.Shares[i].KeyIDOwner, sig[:]); err != nil {
				t.Errorf("signature %d: %v", i, err)
			}
		}

		// The raw bytes up to the signature count: version, proTxHash,
		// operator key, voting key and inputs hash.
		const bodyLen = 2 + chainhash.HashSize + OperatorPubKeySize + KeyIDSize + chainhash.HashSize
		if got != chainhash.DoubleHashH(tx.ExtraPayload[:bodyLen]) {
			t.Error("payload hash is not the hash of the received bytes without the signatures")
		}

		for name, mutate := range map[string]func(q *ProUpSharedRegTx){
			"proTxHash":    func(q *ProUpSharedRegTx) { q.ProTxHash[0] ^= 1 },
			"operator key": func(q *ProUpSharedRegTx) { q.PubKeyOperator[0] ^= 1 },
			"voting key":   func(q *ProUpSharedRegTx) { q.KeyIDVoting[0] ^= 1 },
			"inputs hash":  func(q *ProUpSharedRegTx) { q.InputsHash[0] ^= 1 },
		} {
			q := p
			mutate(&q)
			if h, _ := q.PayloadHash(); h == got {
				t.Errorf("%s: digest did not change", name)
			}
		}
		q := p
		q.Sigs = [][CompactSigSize]byte{{1}}
		if h, _ := q.PayloadHash(); h != got {
			t.Error("the digest depends on the signatures")
		}
	})
}

// TestProDisTxSignHashRefuses pins that a dissolution digest is only for a
// dissolution transaction and for a signature count that fits the payload.
func TestProDisTxSignHashRefuses(t *testing.T) {
	t.Parallel()

	_, dis := loadVector(t, "dis")
	var p ProDisTx
	if err := Parse(dis.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	wrong := *dis
	wrong.Type = txTypeProReg
	if _, err := p.SignHash(&wrong); !errors.Is(err, ErrWrongTxType) {
		t.Errorf("wrong type: %v, want ErrWrongTxType", err)
	}

	p.Sigs = make([][CompactSigSize]byte, 256)
	if _, err := p.SignHash(dis); !errors.Is(err, ErrFieldTooLong) {
		t.Errorf("256 signatures: %v, want ErrFieldTooLong", err)
	}
}

func TestSharedDigestsRefuseOtherVersions(t *testing.T) {
	t.Parallel()

	if _, err := (&ProDisTx{Version: 2}).SignHash(&bisonwire.Tx{Type: txTypeProDis}); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("ProDisTx: %v", err)
	}
	if _, err := (&ProUpShareTx{Version: 2}).PayloadHash(); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("ProUpShareTx: %v", err)
	}
	if _, err := (&ProUpSharedRegTx{Version: 2}).PayloadHash(); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("ProUpSharedRegTx: %v", err)
	}
}

// TestVerifyCompactRejects pins what VerifyCompact refuses, each of which closes
// a way to change a signature without the key: the wrong length, a header
// outside 27 to 34 (the recovery rejects it), and a high S (the same signature
// with S negated), which is this wallet's policy and not known to be a Dash
// Core rule.
func TestVerifyCompactRejects(t *testing.T) {
	t.Parallel()

	reg, _ := sharedRegistration(t)
	_, tx := loadVector(t, "regshared")
	hash, err := reg.SharedRegConsentHash(tx)
	if err != nil {
		t.Fatal(err)
	}
	owner, good := reg.Shares[0].KeyIDOwner, reg.Shares[0].JoinSig
	if err := VerifyCompact(hash, owner, good[:]); err != nil {
		t.Fatalf("the real signature: %v", err)
	}

	withHeader := func(h byte) []byte {
		s := bytes.Clone(good[:])
		s[0] = h
		return s
	}
	// Negating S and flipping the parity of the recovery id (the header is 27,
	// plus 4 for a compressed key, plus the id) is a second valid signature by
	// the same key, so only the low-S rule rejects it. TestVerifyCompactHighS
	// checks that it is one.
	highS := highSForm(good[:])

	other := owner
	other[0] ^= 1
	tests := []struct {
		name  string
		keyID [KeyIDSize]byte
		sig   []byte
	}{
		{"another key id", other, good[:]},
		{"header below 27", owner, withHeader(26)},
		{"header above 34", owner, withHeader(35)},
		{"header with a high bit set", owner, withHeader(good[0] + 8)},
		{"too short", owner, good[:CompactSigSize-1]},
		{"too long", owner, append(bytes.Clone(good[:]), 0)},
		{"empty", owner, nil},
		{"high S", owner, highS},
		{"all zero", owner, make([]byte, CompactSigSize)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := VerifyCompact(hash, tc.keyID, tc.sig); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("error %v, want ErrBadSignature", err)
			}
		})
	}
}

// TestVerifyCompactKeepsRecoveryCause pins that a signature that cannot be
// recovered reports why, and not only that it is bad: the error wraps both
// ErrBadSignature and the recovery error, so a caller can match either.
func TestVerifyCompactKeepsRecoveryCause(t *testing.T) {
	t.Parallel()

	// All zero is not a signature: r and s are zero, which recovery refuses.
	err := VerifyCompact(chainhash.Hash{}, [KeyIDSize]byte{}, append([]byte{27}, make([]byte, CompactSigSize-1)...))
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("error %v, want ErrBadSignature", err)
	}
	multi, ok := err.(interface{ Unwrap() []error })
	if !ok || len(multi.Unwrap()) != 2 {
		t.Fatalf("error %v does not wrap the recovery error as well", err)
	}
}

// highSForm returns the other signature by the same key over the same digest:
// S replaced by the group order minus S, and the parity of the recovery id
// flipped, since negating S negates the point the key is recovered from.
func highSForm(sig []byte) []byte {
	out := bytes.Clone(sig)
	s := new(big.Int).SetBytes(out[33:])
	s.Sub(btcec.S256().N, s).FillBytes(out[33:])
	out[0] = 27 + ((out[0] - 27) ^ 1)
	return out
}

// TestVerifyCompactHighS guards the test above against passing for the wrong
// reason: the high-S form of a real signature must recover the same key, so
// that the low-S rule is the only thing that rejects it.
func TestVerifyCompactHighS(t *testing.T) {
	t.Parallel()

	reg, _ := sharedRegistration(t)
	_, tx := loadVector(t, "regshared")
	hash, err := reg.SharedRegConsentHash(tx)
	if err != nil {
		t.Fatal(err)
	}
	share := reg.Shares[0]
	high := highSForm(share.JoinSig[:])
	if new(big.Int).SetBytes(high[33:]).Cmp(halfOrder) <= 0 {
		t.Fatal("the constructed S is not high")
	}

	// Without the rule it is a valid signature by the share owner.
	pub, compressed, err := ecdsa.RecoverCompact(high, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	serialized := pub.SerializeUncompressed()
	if compressed {
		serialized = pub.SerializeCompressed()
	}
	if !bytes.Equal(btcutil.Hash160(serialized), share.KeyIDOwner[:]) {
		t.Fatal("the high-S form recovers another key, so it does not test the low-S rule")
	}
	if err := VerifyCompact(hash, share.KeyIDOwner, high); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a high-S signature was accepted: %v", err)
	}
}

func TestSignCompact(t *testing.T) {
	t.Parallel()

	var raw [32]byte
	for i := range raw {
		raw[i] = byte(0x40 + i)
	}
	key, _ := btcec.PrivKeyFromBytes(raw[:])
	var keyID [KeyIDSize]byte
	copy(keyID[:], btcutil.Hash160(key.PubKey().SerializeCompressed()))
	hash := chainhash.DoubleHashH([]byte("sign me"))

	sig := SignCompact(key, hash)
	if sig[0] < 31 || sig[0] > 34 {
		t.Errorf("header %d is not for a compressed key", sig[0])
	}
	if err := VerifyCompact(hash, keyID, sig[:]); err != nil {
		t.Errorf("a signature made here does not verify: %v", err)
	}
	if again := SignCompact(key, hash); again != sig {
		t.Error("signing is not deterministic")
	}

	other := chainhash.DoubleHashH([]byte("another digest"))
	if err := VerifyCompact(other, keyID, sig[:]); !errors.Is(err, ErrBadSignature) {
		t.Errorf("the signature verified for another digest: %v", err)
	}
}
