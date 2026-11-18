// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package rhea

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bisoncraft/caribou/wallet/asset/eth/evmkey"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Identity is an EVM identity as the account manager keys it: the address
// as 40 lowercase hex characters without 0x. get_mca_by_wallet is
// case-sensitive (a registered address looked up in upper or mixed case
// returns null), so the lowercase form is the only one ever registered or
// looked up.
type Identity string

// ParseIdentity parses an EVM address with or without 0x, in any case.
func ParseIdentity(s string) (Identity, error) {
	h := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(h) != 40 {
		return "", fmt.Errorf("identity %q is not a 20-byte hex address", s)
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", fmt.Errorf("identity %q is not hex: %w", s, err)
	}
	return Identity(strings.ToLower(h)), nil
}

// IdentityFromAddress returns the identity of an EVM address.
func IdentityFromAddress(a common.Address) Identity {
	return Identity(hex.EncodeToString(a[:]))
}

// Address returns the identity as an EVM address.
func (id Identity) Address() common.Address {
	return common.HexToAddress(string(id))
}

// Wallet returns the identity's wallet entry.
func (id Identity) Wallet() Wallet {
	return Wallet{Kind: WalletKindEVM, ID: string(id)}
}

// WalletKindEVM is the wallet kind of an EVM identity. MCAs also accept
// Solana, Bitcoin, Litecoin, Aptos, Sui, Zcash, WebAuthn, and Near kinds.
const WalletKindEVM = "EVM"

// Wallet is an identity entry as MCAs and the account manager encode it: a
// single-member object {"<Kind>":"<ID>"}.
type Wallet struct {
	Kind string
	ID   string
}

// MarshalJSON encodes the wallet as {"<Kind>":"<ID>"}.
func (w Wallet) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	if err := writeJSONString(&b, w.Kind); err != nil {
		return nil, err
	}
	b.WriteByte(':')
	if err := writeJSONString(&b, w.ID); err != nil {
		return nil, err
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON decodes a single-member wallet object.
func (w *Wallet) UnmarshalJSON(b []byte) error {
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("error decoding wallet: %w", err)
	}
	if len(m) != 1 {
		return fmt.Errorf("wallet object has %d members, want 1", len(m))
	}
	for k, v := range m {
		w.Kind, w.ID = k, v
	}
	return nil
}

// String returns the wallet's JSON encoding.
func (w Wallet) String() string {
	b, _ := w.MarshalJSON()
	return string(b)
}

// CreationMessage is the message an identity signs to be bound to a new
// MCA: the JSON wallet list [{"EVM":"<id>"}]. It carries no nonce,
// deadline, or MCA ID; a replay can only bind the same identity, which the
// account manager refuses once the identity is registered.
func CreationMessage(id Identity) []byte {
	return []byte(`[` + id.Wallet().String() + `]`)
}

// DerivationVersion identifies how an identity key is derived from the app
// seed. A released version is never changed; a new scheme is added as a
// new version and discovery checks every supported version.
type DerivationVersion uint32

// DerivationV1 is the EVM wallet key: m/44'/60'/0'/0/0 of the EVM asset
// seed, the key and address shared by Caribou's EVM wallets. Lending
// positions are therefore publicly linked to the EVM address.
const DerivationV1 DerivationVersion = 1

// DerivationVersions lists the supported derivation versions.
var DerivationVersions = []DerivationVersion{DerivationV1}

// Key is an identity's signing key. Signatures are produced only for the
// creation message and for businesses that pass validation (see
// SignWithdraw and SignSweep).
type Key struct {
	priv    *ecdsa.PrivateKey
	id      Identity
	version DerivationVersion
}

// DeriveKey derives the identity key for a derivation version from the EVM
// asset seed (the per-asset seed derived from the app seed for asset 60).
// The caller must Zero the key when done.
func DeriveKey(v DerivationVersion, evmAssetSeed []byte) (*Key, error) {
	switch v {
	case DerivationV1:
	default:
		return nil, fmt.Errorf("unknown derivation version %d", v)
	}
	raw, zero, err := evmkey.PrivKeyFromSeedAccount(evmAssetSeed, 0)
	if err != nil {
		return nil, fmt.Errorf("error deriving identity key: %w", err)
	}
	priv, err := crypto.ToECDSA(raw) // copies raw
	zero()
	if err != nil {
		return nil, fmt.Errorf("error parsing identity key: %w", err)
	}
	return &Key{
		priv:    priv,
		id:      IdentityFromAddress(crypto.PubkeyToAddress(priv.PublicKey)),
		version: v,
	}, nil
}

// DeriveIdentity derives the public identity for a derivation version.
func DeriveIdentity(v DerivationVersion, evmAssetSeed []byte) (Identity, error) {
	k, err := DeriveKey(v, evmAssetSeed)
	if err != nil {
		return "", err
	}
	defer k.Zero()
	return k.id, nil
}

// Identity returns the key's identity.
func (k *Key) Identity() Identity {
	return k.id
}

// Version returns the key's derivation version.
func (k *Key) Version() DerivationVersion {
	return k.version
}

// Zero clears the private scalar. The key cannot sign afterwards.
func (k *Key) Zero() {
	if k.priv == nil {
		return
	}
	if k.priv.D != nil {
		words := k.priv.D.Bits()
		for i := range words {
			words[i] = 0
		}
		k.priv.D.SetInt64(0)
	}
	k.priv = nil
}

// SignCreation signs the creation message binding this identity to a new
// MCA.
func (k *Key) SignCreation() (string, error) {
	return k.signPersonal(CreationMessage(k.id))
}

// signPersonal produces an EIP-191 personal_sign signature as MCAs expect
// it: 65 bytes r||s||v with v in {27, 28}, lowercase hex without 0x.
func (k *Key) signPersonal(msg []byte) (string, error) {
	if k.priv == nil {
		return "", errors.New("key has been zeroed")
	}
	sig, err := crypto.Sign(personalHash(msg), k.priv)
	if err != nil {
		return "", fmt.Errorf("error signing: %w", err)
	}
	sig[64] += 27
	return hex.EncodeToString(sig), nil
}

// personalHash is the EIP-191 version 0x45 digest:
// keccak256("\x19Ethereum Signed Message:\n" + decimal length + msg).
func personalHash(msg []byte) []byte {
	prefix := "\x19Ethereum Signed Message:\n" + strconv.Itoa(len(msg))
	return crypto.Keccak256([]byte(prefix), msg)
}

// RecoverPersonal recovers the identity that produced an EIP-191 signature
// over msg. The signature is 65 bytes r||s||v in hex, v in {27, 28}.
func RecoverPersonal(msg []byte, sigHex string) (Identity, error) {
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return "", fmt.Errorf("signature is not hex: %w", err)
	}
	if len(sig) != crypto.SignatureLength {
		return "", fmt.Errorf("signature is %d bytes, want %d", len(sig), crypto.SignatureLength)
	}
	if sig[64] != 27 && sig[64] != 28 {
		return "", fmt.Errorf("signature recovery byte %d, want 27 or 28", sig[64])
	}
	sig[64] -= 27
	pub, err := crypto.SigToPub(personalHash(msg), sig)
	if err != nil {
		return "", fmt.Errorf("error recovering signer: %w", err)
	}
	return IdentityFromAddress(crypto.PubkeyToAddress(*pub)), nil
}
