// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"crypto/ed25519"
	"crypto/sha256"

	"github.com/decred/base58"
)

// devSigningSeedPhrase deterministically derives the mock's quote-signing
// keypair. The public half is pinned in the wallet as
// intents.SimnetSigningKey (a test asserts the two stay in sync), so a
// simnet wallet needs no per-run key exchange with the mock. The key
// authenticates nothing outside a local simnet session.
const devSigningSeedPhrase = "caribou simnet 1click dev signing key v1"

// DevSigningKey returns the deterministic dev signing keypair: the private
// key and the "ed25519:"-prefixed base58 public key.
func DevSigningKey() (ed25519.PrivateKey, string) {
	seed := sha256.Sum256([]byte(devSigningSeedPhrase))
	priv := ed25519.NewKeyFromSeed(seed[:])
	pub := priv.Public().(ed25519.PublicKey)
	return priv, "ed25519:" + base58.Encode(pub)
}
