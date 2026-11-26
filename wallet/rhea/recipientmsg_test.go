package rhea

import (
	"strings"
	"testing"
)

// TestRegisterAndSupplyMsgMatchesMainnet rebuilds the recipient message of
// the mainnet MCA creation 1aqcDa8Y… (rhea001594) from its identity and
// creation signature, and checks it byte for byte against the message the
// verifier delivered.
func TestRegisterAndSupplyMsgMatchesMainnet(t *testing.T) {
	tree, err := DecodeTxTree(readTestdata(t, "tx-create-supply-1aqcDa8Y.json"))
	if err != nil {
		t.Fatalf("DecodeTxTree: %v", err)
	}
	ws, err := tree.VerifierWithdrawals("intents.near")
	if err != nil || len(ws) != 1 {
		t.Fatalf("verifier withdrawals %v, %v", ws, err)
	}
	delivered := ws[0].Msg
	parsed, err := ParseRecipientMsg(delivered)
	if err != nil {
		t.Fatalf("ParseRecipientMsg: %v", err)
	}
	if parsed.Template != TemplateBurrowRegisterAndSupply || len(parsed.Wallets) != 1 || len(parsed.Signatures) != 1 {
		t.Fatalf("parsed %+v", parsed)
	}
	id := Identity(parsed.Wallets[0].ID)
	if id != "81a5d469623b3777b44c2c6f0d5748e9206b55f9" {
		t.Fatalf("identity %s", id)
	}
	msg, err := RegisterAndSupplyMsg(id, parsed.Signatures[0])
	if err != nil {
		t.Fatalf("RegisterAndSupplyMsg: %v", err)
	}
	if msg != delivered {
		t.Fatalf("message differs:\n%s\n%s", msg, delivered)
	}
	if parsed.String() != delivered {
		t.Fatalf("re-encoded message differs")
	}

	// A signature by another identity, or over another message, is
	// refused.
	if _, err := RegisterAndSupplyMsg("242c9fc3162321d326892cfe044fc0d9ffc3e9b0", parsed.Signatures[0]); err == nil {
		t.Fatalf("signature for another identity accepted")
	}
	if _, err := RegisterAndSupplyMsg(id, strings.Repeat("0", 130)); err == nil {
		t.Fatalf("garbage signature accepted")
	}
}

func TestSupplyMsg(t *testing.T) {
	id := Identity("75e1a0bbda90164958275d7e8e6007ec13bfe807")
	const want = `{"w":[{"EVM":"75e1a0bbda90164958275d7e8e6007ec13bfe807"}],"b":{"r":"BurrowSupply"}}`
	if got := SupplyMsg(id); got != want {
		t.Fatalf("SupplyMsg %s", got)
	}
	k := testKey(t)
	sig, err := k.SignCreation()
	if err != nil {
		t.Fatalf("SignCreation: %v", err)
	}
	msg, err := RegisterAndSupplyMsg(k.Identity(), sig)
	if err != nil {
		t.Fatalf("RegisterAndSupplyMsg: %v", err)
	}
	if want := `{"w":[{"EVM":"` + string(k.Identity()) + `"}],"b":{"r":"BurrowRegisterAndSupply"},"s":["` + sig + `"]}`; msg != want {
		t.Fatalf("message %s", msg)
	}
	for _, bad := range []string{`{"w":[],"b":{"r":"x"},"extra":1}`, `{"w":[{"EVM":"a","Near":"b"}],"b":{"r":"x"}}`, `not json`} {
		if _, err := ParseRecipientMsg(bad); err == nil {
			t.Fatalf("message %s accepted", bad)
		}
	}
}
