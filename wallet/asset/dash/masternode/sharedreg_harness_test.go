//go:build harness

package masternode

// This test checks that a registration built, reviewed and consented to by this
// package is accepted by Dash Core v24, which checks every join signature, the
// collateral and the share table. Dash Core signs the funding inputs from its
// wallet (this test holds no wallet keys) and the result is only passed to
// testmempoolaccept: nothing is broadcast or mined.
//
// Prerequisites (run by the user): the Dash regtest harness from
// util/testing/dash with V24 active and wallet gamma funded with at least 1001
// DASH. The node is alpha on port 20777.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
)

// harnessProposal makes a two-share registration funded by wallet gamma's
// outputs, with owner keys held by the test.
func harnessProposal(t *testing.T) (*Proposal, []Own, []*btcec.PrivateKey) {
	t.Helper()
	if _, err := rpc(t, "walletpassphrase", "abc", 600); err != nil {
		t.Logf("walletpassphrase: %v", err)
	}
	r, err := rpc(t, "listunspent")
	if err != nil {
		t.Fatal(err)
	}
	var utxos []struct {
		TxID         string  `json:"txid"`
		Vout         uint32  `json:"vout"`
		Amount       float64 `json:"amount"`
		ScriptPubKey string  `json:"scriptPubKey"`
		Spendable    bool    `json:"spendable"`
	}
	if err := json.Unmarshal(r, &utxos); err != nil {
		t.Fatal(err)
	}
	sort.Slice(utxos, func(i, j int) bool { return utxos[i].Amount > utxos[j].Amount })

	// Two shares of 500 DASH, each funded by whole outputs of the wallet.
	const fee = 20_000
	amounts := []int64{500e8, 500e8}
	var funding []Funding
	next := 0
	for i, amt := range amounts {
		var f Funding
		var have int64
		for next < len(utxos) && have < amt+fee {
			u := utxos[next]
			next++
			if !u.Spendable {
				continue
			}
			h, err := chainhash.NewHashFromStr(u.TxID)
			if err != nil {
				t.Fatal(err)
			}
			script, err := hex.DecodeString(u.ScriptPubKey)
			if err != nil {
				t.Fatal(err)
			}
			if len(script) != 25 { // P2PKH only
				continue
			}
			v := int64(math.Round(u.Amount * 1e8))
			f.Inputs = append(f.Inputs, FundingInput{OutPoint: wire.OutPoint{Hash: *h, Index: u.Vout}, PkScript: script, Value: v})
			have += v
		}
		if have < amt+fee {
			t.Skipf("wallet gamma cannot fund share %d", i)
		}
		_, f.ChangeScript, _ = addr(t)
		funding = append(funding, f)
	}

	opJSON, err := rpc(t, "bls", "generate")
	if err != nil {
		t.Fatal(err)
	}
	var opKey struct {
		Public string `json:"public"`
	}
	if err := json.Unmarshal(opJSON, &opKey); err != nil {
		t.Fatal(err)
	}
	_, _, votingID := addr(t)

	p := &Proposal{Fee: fee, Funding: funding}
	p.Terms.NetInfo, _ = CoreP2PNetInfo()
	p.Terms.KeyIDVoting = votingID
	p.Terms.EarlyPeriodBlocks = 10
	p.Terms.EarlyPenalty = DefaultEarlyPenalty(amounts)
	op, _ := hex.DecodeString(opKey.Public)
	copy(p.Terms.PubKeyOperator[:], op)

	var ownerKeys []*btcec.PrivateKey
	var owns []Own
	for i, amt := range amounts {
		key, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		ownerKeys = append(ownerKeys, key)
		_, refund, _ := addr(t)
		id := idFor(key)
		p.Terms.Shares = append(p.Terms.Shares, protx.Share{Amount: amt, RefundScript: refund, KeyIDOwner: id})
		owns = append(owns, Own{
			ShareIndex: i, Fee: fee,
			Inputs: funding[i].Inputs, ChangeScript: funding[i].ChangeScript,
		})
	}

	for i := range owns {
		owns[i].Terms = p.Terms
	}
	return p, owns, ownerKeys
}

func TestRegistrationAcceptedByDashCore(t *testing.T) {
	p, owns, ownerKeys := harnessProposal(t)
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
		s, err := rv.SignConsent(ownerKeys[i])
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
	if err := rv.Complete(); err != nil {
		t.Fatal(err)
	}
	raw0, err := rv.Raw()
	if err != nil {
		t.Fatal(err)
	}
	buf := bytes.NewBuffer(raw0)

	// Negative control, so acceptance below means something: a registration
	// whose terms changed after consent (the voting key here) must be refused
	// by Core, because the join signatures no longer match.
	tampered := bytes.Replace(buf.Bytes(), p.Terms.KeyIDVoting[:], bytes.Repeat([]byte{0x77}, 20), 1)
	if bytes.Equal(tampered, buf.Bytes()) {
		t.Fatal("the voting key was not found in the transaction")
	}
	if tr, err := rpc(t, "signrawtransactionwithwallet", hex.EncodeToString(tampered)); err == nil {
		var ts struct {
			Hex string `json:"hex"`
		}
		_ = json.Unmarshal(tr, &ts)
		res, err := rpc(t, "testmempoolaccept", []string{ts.Hex})
		if err == nil && bytes.Contains(res, []byte(`"allowed":true`)) {
			t.Fatalf("Core accepted a registration whose terms changed after consent: %s", res)
		}
	}

	// Dash Core signs the funding inputs from its wallet.
	signedRaw, err := rpc(t, "signrawtransactionwithwallet", hex.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := json.Unmarshal(signedRaw, &signed); err != nil {
		t.Fatal(err)
	}
	if !signed.Complete {
		t.Fatalf("Core's wallet could not sign the funding inputs: %s", signedRaw)
	}

	res, err := rpc(t, "testmempoolaccept", []string{signed.Hex})
	if err != nil {
		t.Fatal(err)
	}
	var accept []struct {
		Allowed      bool   `json:"allowed"`
		RejectReason string `json:"reject-reason"`
	}
	if err := json.Unmarshal(res, &accept); err != nil {
		t.Fatal(err)
	}
	if len(accept) != 1 || !accept[0].Allowed {
		t.Fatalf("Dash Core rejected the registration: %s", res)
	}

	// The same transaction must be a valid one for this package to parse.
	raw, _ := hex.DecodeString(signed.Hex)
	back := &bisonwire.Tx{Chain: "dash"}
	if err := back.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
}
