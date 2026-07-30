// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package main

import (
	"bufio"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/decred/base58"
	"github.com/ethereum/go-ethereum/crypto"
)

// erc191Prehash returns the digest the verifier expects a signature over:
// keccak256("\x19Ethereum Signed Message:\n" + decimal byte length + msg),
// per ERC-191 / personal_sign. The verifier hashes the payload string
// byte-exactly, so callers must sign and transmit the identical bytes.
func erc191Prehash(msg []byte) []byte {
	return crypto.Keccak256([]byte(fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(msg))), msg)
}

// signPayload signs the payload string and returns the verifier's wire
// encoding: "secp256k1:" + base58 of the 65-byte r||s||v signature. The
// recovery byte stays raw (0 or 1, no +27 legacy offset) and the signature
// is low-S normalized — both as go-ethereum's crypto.Sign produces and the
// verifier's malleability guard requires.
func signPayload(payload []byte, priv *ecdsa.PrivateKey) (string, error) {
	sig, err := crypto.Sign(erc191Prehash(payload), priv)
	if err != nil {
		return "", fmt.Errorf("signing failed: %w", err)
	}
	return "secp256k1:" + base58.Encode(sig), nil
}

// recoverIdentity recovers the 0x-style signer identity — the Ethereum
// address of the recovered public key — from a payload string and a
// "secp256k1:"-encoded signature.
func recoverIdentity(payload []byte, sigStr string) (string, error) {
	s := strings.TrimPrefix(sigStr, "secp256k1:")
	sig := base58.Decode(s)
	if len(sig) != 65 {
		return "", fmt.Errorf("signature decodes to %d bytes, want 65", len(sig))
	}
	pub, err := crypto.SigToPub(erc191Prehash(payload), sig)
	if err != nil {
		return "", fmt.Errorf("public key recovery failed: %w", err)
	}
	return strings.ToLower(crypto.PubkeyToAddress(*pub).Hex()), nil
}

// parseIdentity validates an 0x-style fee-account identity and returns it
// lowercased (a NEAR account ID cannot contain uppercase).
func parseIdentity(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return "", fmt.Errorf("identity must start with 0x")
	}
	body := s[2:]
	if len(body) != 40 {
		return "", fmt.Errorf("identity must be 40 hex characters after 0x, got %d", len(body))
	}
	if _, err := hex.DecodeString(body); err != nil {
		return "", fmt.Errorf("identity is not valid hex: %w", err)
	}
	return strings.ToLower(s), nil
}

// readPrivKey reads a hex-encoded secp256k1 private key from stdin,
// tolerating a leading 0x and surrounding whitespace. When stdin is a
// terminal it prompts, so an interactive run does not appear to hang.
func readPrivKey() (*ecdsa.PrivateKey, error) {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "Enter the fee-account private key (hex) and press Enter: ")
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) == "" {
		if err != nil {
			return nil, fmt.Errorf("no private key on stdin; redirect a file (treasurysweep sign ... < treasury.key) or type it at the prompt")
		}
		return nil, fmt.Errorf("empty private key on stdin")
	}
	h := strings.TrimSpace(line)
	h = strings.TrimPrefix(h, "0x")
	h = strings.TrimPrefix(h, "0X")
	priv, err := crypto.HexToECDSA(h)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	return priv, nil
}
