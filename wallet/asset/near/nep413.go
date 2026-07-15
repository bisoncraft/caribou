// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
)

// nep413PayloadTag is the Borsh prefix distinguishing NEP-413 off-chain
// message payloads from on-chain transactions: 2^31 + 413. On-chain
// transactions serialize an account ID string first, whose 4-byte length
// prefix can never have the high bit set, so a signature over a tagged
// payload can never be replayed as a transaction signature.
const nep413PayloadTag = 1<<31 + 413

// NEP413Payload is an off-chain message to be signed under NEP-413
// (https://github.com/near/NEPs/blob/master/neps/nep-0413.md). Recipient
// binds the signature to its intended verifier (e.g. a contract account
// ID), and Nonce is a caller-supplied 32-byte challenge preventing replay.
type NEP413Payload struct {
	Message     string
	Recipient   string
	Nonce       [32]byte
	CallbackURL string // optional
}

// serialize Borsh-encodes the tagged payload per NEP-413: u32 tag, then
// {message: String, nonce: [u8; 32], recipient: String,
// callbackUrl: Option<String>}.
func (p *NEP413Payload) serialize() []byte {
	b := appendU32LE(nil, nep413PayloadTag)
	b = appendString(b, p.Message)
	b = append(b, p.Nonce[:]...)
	b = appendString(b, p.Recipient)
	if p.CallbackURL != "" {
		b = append(b, 1)
		b = appendString(b, p.CallbackURL)
	} else {
		b = append(b, 0)
	}
	return b
}

// signingHash returns the SHA-256 digest that is signed for the payload.
func (p *NEP413Payload) signingHash() [32]byte {
	return sha256.Sum256(p.serialize())
}

// SignMessageNEP413 signs an off-chain message with the wallet's full-access
// ed25519 key per NEP-413, returning the public key and the 64-byte
// signature. The wallet must be unlocked. Verifiers (e.g. the NEAR Intents
// verifier contract) accept these signatures from implicit accounts with no
// prior on-chain registration.
func (w *NearWallet) SignMessageNEP413(p *NEP413Payload) (pubKey ed25519.PublicKey, sig []byte, err error) {
	w.keyMtx.RLock()
	defer w.keyMtx.RUnlock()
	if w.privKey == nil {
		return nil, nil, fmt.Errorf("wallet is locked")
	}
	hash := p.signingHash()
	return w.pubKey, ed25519.Sign(w.privKey, hash[:]), nil
}
