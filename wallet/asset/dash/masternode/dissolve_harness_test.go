//go:build harness

package masternode

// This test checks that dissolutions built and signed by this package are
// accepted by Dash Core v24, and that the penalty rules agree: a unilateral
// dissolution is refused inside the early period without the penalty and
// accepted with it, and a standby that pays none is accepted after the period.
//
// It registers a shared masternode on the regtest harness, which is broadcast
// and mined (and stays registered). The dissolutions are only passed to
// testmempoolaccept, so none is broadcast.
//
// Prerequisites (run by the user): the Dash regtest harness from
// util/testing/dash with V24 active and wallet gamma funded with at least 1001
// DASH. The node is alpha on port 20777.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func mine(t *testing.T, n int) {
	t.Helper()
	a, _, _ := addr(t)
	if _, err := rpc(t, "generatetoaddress", n, a); err != nil {
		t.Fatal(err)
	}
}

// accepted reports whether Dash Core would accept raw into its mempool now,
// and why not.
func accepted(t *testing.T, raw []byte) (bool, string) {
	t.Helper()
	res, err := rpc(t, "testmempoolaccept", []string{hex.EncodeToString(raw)})
	if err != nil {
		t.Fatal(err)
	}
	var out []struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reject-reason"`
	}
	if err := json.Unmarshal(res, &out); err != nil || len(out) != 1 {
		t.Fatalf("testmempoolaccept: %s %v", res, err)
	}
	return out[0].Allowed, out[0].Reason
}

// registerHarnessMN registers a two-share masternode on the harness: consent,
// Core signs the funding, broadcast, mined. The owners' keys are the test's.
func registerHarnessMN(t *testing.T) (*SharedMN, []*btcec.PrivateKey) {
	t.Helper()
	p, owns, keys := harnessProposal(t)

	// Register the masternode: consent, Core signs the funding, broadcast, mine.
	tx, err := p.Build(RegtestRules)
	if err != nil {
		t.Fatal(err)
	}
	var sigs []ShareSig
	for i := range owns {
		rv, err := Review(tx, RegtestRules, owns[i])
		if err != nil {
			t.Fatal(err)
		}
		s, err := rv.SignConsent(keys[i])
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	rv, err := Review(tx, RegtestRules, owns[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := rv.AddJoinSigs(sigs); err != nil {
		t.Fatal(err)
	}
	raw, err := rv.Raw()
	if err != nil {
		t.Fatal(err)
	}
	signedRaw, err := rpc(t, "signrawtransactionwithwallet", hex.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := json.Unmarshal(signedRaw, &signed); err != nil || !signed.Complete {
		t.Fatalf("funding not signed: %s %v", signedRaw, err)
	}
	var sent string
	r, err := rpc(t, "sendrawtransaction", signed.Hex)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(r, &sent); err != nil {
		t.Fatal(err)
	}
	mine(t, 1)

	txid, err := chainhash.NewHashFromStr(sent)
	if err != nil {
		t.Fatal(err)
	}
	info, err := rpc(t, "getrawtransaction", sent, true)
	if err != nil {
		t.Fatal(err)
	}
	var verbose struct {
		BlockHash string `json:"blockhash"`
	}
	if err := json.Unmarshal(info, &verbose); err != nil {
		t.Fatal(err)
	}
	hdr, err := rpc(t, "getblockheader", verbose.BlockHash)
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		Height int32 `json:"height"`
	}
	if err := json.Unmarshal(hdr, &header); err != nil {
		t.Fatal(err)
	}

	m := &SharedMN{
		ProTxHash:         *txid,
		Collateral:        wire.OutPoint{Hash: *txid, Index: uint32(len(tx.TxOut) - 1)},
		Shares:            p.Terms.Shares,
		RegisteredHeight:  header.Height,
		EarlyPeriodBlocks: p.Terms.EarlyPeriodBlocks,
		EarlyPenalty:      p.Terms.EarlyPenalty,
		PubKeyOperator:    p.Terms.PubKeyOperator,
		KeyIDVoting:       p.Terms.KeyIDVoting,
	}
	return m, keys
}

func TestDissolutionAcceptedByDashCore(t *testing.T) {
	m, keys := registerHarnessMN(t)
	tip := func() int32 {
		r, err := rpc(t, "getblockcount")
		if err != nil {
			t.Fatal(err)
		}
		var n int32
		if err := json.Unmarshal(r, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A dissolution is checked at the height of the next block.
	if h := tip() + 1; h-m.RegisteredHeight >= int32(m.EarlyPeriodBlocks) {
		t.Fatalf("the early period is already over at %d", h)
	}

	signAll := func(d *Dissolution) []byte {
		t.Helper()
		var sigs []ShareSig
		for i := range keys {
			s, err := d.Sign(keys[i], i)
			if err != nil {
				t.Fatal(err)
			}
			sigs = append(sigs, s)
		}
		if err := d.AddSigs(sigs); err != nil {
			t.Fatal(err)
		}
		raw, err := d.Raw()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	const fee = 100_000

	// Unanimous: every owner reviews what the actor built and signs.
	una, err := NewDissolution(m, DissolveParams{Actor: 0, Unanimous: true, Fee: fee})
	if err != nil {
		t.Fatal(err)
	}
	for i := range keys {
		rv, err := ReviewDissolution(una.tx, m, i, fee)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rv.Sign(keys[i], i); err != nil {
			t.Fatal(err)
		}
	}
	if ok, why := accepted(t, signAll(una)); !ok {
		t.Fatalf("Core refused the unanimous dissolution: %s", why)
	}

	// Unilateral inside the early period: refused without the penalty,
	// accepted with it.
	free, err := NewDissolution(m, DissolveParams{Actor: 1, Fee: fee})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := free.Sign(keys[1], 1)
	if err := free.AddSigs([]ShareSig{s}); err != nil {
		t.Fatal(err)
	}
	freeRaw, _ := free.Raw()
	if ok, why := accepted(t, freeRaw); ok || why != "bad-prodis-penalty-floor" {
		t.Fatalf("Core's verdict on a penalty-free one inside the early period: %v %q", ok, why)
	}
	paid, err := NewDissolution(m, DissolveParams{Actor: 1, Penalty: m.RequiredPenalty(tip()+1, false), Fee: fee})
	if err != nil {
		t.Fatal(err)
	}
	s, _ = paid.Sign(keys[1], 1)
	if err := paid.AddSigs([]ShareSig{s}); err != nil {
		t.Fatal(err)
	}
	paidRaw, _ := paid.Raw()
	if ok, why := accepted(t, paidRaw); !ok {
		t.Fatalf("Core refused a unilateral dissolution that pays the penalty: %s", why)
	}

	// The standby is the penalty-free one, signed now, valid after the period.
	sb, err := NewStandby(m, 1, keys[1], fee)
	if err != nil {
		t.Fatal(err)
	}
	sbRaw, _ := hex.DecodeString(sb.Tx)
	if !bytes.Equal(sbRaw, freeRaw) {
		t.Fatal("the standby is not the penalty-free dissolution")
	}
	// Pin the boundary: the next block is the last one inside the early
	// period, where Core must still want the penalty, and then the first one
	// at ValidFromHeight, where it must not.
	if tip()+2 > sb.ValidFromHeight {
		t.Fatalf("tip %d is already past %d", tip(), sb.ValidFromHeight-2)
	}
	mine(t, int(sb.ValidFromHeight-2-tip()))
	if ok, why := accepted(t, sbRaw); ok || why != "bad-prodis-penalty-floor" {
		t.Fatalf("Core's verdict on the standby one block early: %v %q", ok, why)
	}
	mine(t, 1)
	if tip()+1 != sb.ValidFromHeight {
		t.Fatalf("next block is %d, want %d", tip()+1, sb.ValidFromHeight)
	}
	if ok, why := accepted(t, sbRaw); !ok {
		t.Fatalf("Core refused the standby at the first valid height: %s", why)
	}
}
