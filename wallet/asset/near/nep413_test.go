package near

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

func TestNEP413Serialize(t *testing.T) {
	var nonce [32]byte
	for i := range nonce {
		nonce[i] = byte(i)
	}
	p := &NEP413Payload{
		Message:   "hi",
		Recipient: "intents.near",
		Nonce:     nonce,
	}

	// Expected layout, constructed independently: u32 tag (2^31+413 LE),
	// Borsh string "hi", 32 nonce bytes, Borsh string "intents.near",
	// Option<String>::None.
	var want []byte
	tag := make([]byte, 4)
	binary.LittleEndian.PutUint32(tag, 2147484061)
	want = append(want, tag...)
	want = append(want, 2, 0, 0, 0)
	want = append(want, "hi"...)
	want = append(want, nonce[:]...)
	want = append(want, 12, 0, 0, 0)
	want = append(want, "intents.near"...)
	want = append(want, 0)

	if got := p.serialize(); !bytes.Equal(got, want) {
		t.Fatalf("serialized payload mismatch:\n got %x\nwant %x", got, want)
	}

	// With a callback URL the option byte flips and the string follows.
	p.CallbackURL = "https://x.test"
	withCB := p.serialize()
	tail := append([]byte{1, 14, 0, 0, 0}, "https://x.test"...)
	if !bytes.Equal(withCB, append(append([]byte{}, want[:len(want)-1]...), tail...)) {
		t.Fatalf("callback serialization mismatch: %x", withCB)
	}
}

func TestSignMessageNEP413(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	w := &NearWallet{pubKey: pub}

	p := &NEP413Payload{Message: `{"signer_id":"x","deadline":"y","intents":[]}`, Recipient: "intents.near"}

	if _, _, err := w.SignMessageNEP413(p); err == nil {
		t.Fatalf("expected error signing with locked wallet")
	}

	w.privKey = priv
	gotPub, sig, err := w.SignMessageNEP413(p)
	if err != nil {
		t.Fatalf("SignMessageNEP413: %v", err)
	}
	if !gotPub.Equal(pub) {
		t.Fatalf("wrong public key returned")
	}
	hash := sha256.Sum256(p.serialize())
	if !ed25519.Verify(pub, hash[:], sig) {
		t.Fatalf("signature does not verify")
	}
}
