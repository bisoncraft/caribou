// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// ftWithdrawIntent is the verifier's ft_withdraw intent: burn the signer's
// NEP-245 balance backed by the NEP-141 token and transfer it to
// receiver_id. For a PoA-factory bridged token, receiver_id set to the
// token contract itself with a "WITHDRAW_TO:<external address>" memo
// triggers the bridge withdrawal to the external chain; other bridges may
// use different conventions, so verify per bridge (simulate first) before
// an external-chain withdrawal. A plain NEAR-account receiver needs no
// memo.
type ftWithdrawIntent struct {
	Intent     string `json:"intent"` // "ft_withdraw"
	Token      string `json:"token"`
	ReceiverID string `json:"receiver_id"`
	Amount     string `json:"amount"`
	Memo       string `json:"memo,omitempty"`
}

// sweepPayload is the DefusePayload the fee-account key signs. The JSON
// encoding of this struct is the exact byte string covered by the ERC-191
// signature; the verifier recovers the signer from the signature and
// requires it to match SignerID.
type sweepPayload struct {
	SignerID          string             `json:"signer_id"`
	VerifyingContract string             `json:"verifying_contract"`
	Deadline          string             `json:"deadline"` // RFC 3339
	Nonce             string             `json:"nonce"`    // base64, 32 bytes: 4-byte salt || 28 random
	Intents           []ftWithdrawIntent `json:"intents"`
}

// signedEnvelope is the execute_intents argument: a list of one signed
// MultiPayload in the verifier's erc191 arm.
type signedEnvelope struct {
	Signed []multiPayload `json:"signed"`
}

type multiPayload struct {
	Standard  string `json:"standard"` // "erc191"
	Payload   string `json:"payload"`
	Signature string `json:"signature"` // "secp256k1:" + base58(65 bytes)
}

// buildNonce returns a base64 nonce of the verifier's required form: the
// contract's current 4-byte salt followed by 28 random bytes. The salt
// binds the nonce to the verifier's active salt window; the random tail
// makes it unique so the intent cannot be replayed.
func buildNonce(saltHex string) (string, error) {
	salt, err := hex.DecodeString(saltHex)
	if err != nil || len(salt) != 4 {
		return "", fmt.Errorf("salt must be 8 hex characters (4 bytes), got %q", saltHex)
	}
	nonce := make([]byte, 32)
	copy(nonce, salt)
	if _, err := rand.Read(nonce[4:]); err != nil {
		return "", fmt.Errorf("error generating nonce: %w", err)
	}
	return base64.StdEncoding.EncodeToString(nonce), nil
}

// cmdBuild constructs the unsigned withdrawal payload string.
func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	treasury := fs.String("treasury", "", "fee-account identity (0x...)")
	token := fs.String("token", "", "verifier token ID to withdraw (nep141:<contract>)")
	amount := fs.String("amount", "", "amount in the token's base units")
	receiver := fs.String("receiver", "", "receiving NEAR account, or the token contract for a bridged external withdrawal")
	memo := fs.String("memo", "", "withdrawal memo; WITHDRAW_TO:<address> for a bridged external withdrawal")
	deadline := fs.Duration("deadline", time.Hour, "how long the signed intent stays valid")
	saltFlag := fs.String("salt", "", "verifier nonce salt (8 hex chars); fetched via RPC when empty")
	rpcURL := fs.String("rpc", "", "NEAR RPC endpoint override")
	out := fs.String("out", "", "output file for the payload (default stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	signer, err := parseIdentity(*treasury)
	if err != nil {
		return fmt.Errorf("--treasury: %w", err)
	}
	contract, ok := strings.CutPrefix(*token, "nep141:")
	if !ok || contract == "" {
		return fmt.Errorf("--token must be a nep141:<contract> verifier token ID, got %q", *token)
	}
	if !isDigits(*amount) {
		return fmt.Errorf("--amount must be a base-unit integer, got %q", *amount)
	}
	if *receiver == "" {
		return fmt.Errorf("--receiver is required")
	}
	if strings.HasPrefix(*memo, "WITHDRAW_TO:") && *receiver != contract {
		return fmt.Errorf("a WITHDRAW_TO memo requires --receiver to be the token contract %s", contract)
	}

	salt := *saltFlag
	if salt == "" {
		if salt, err = currentSalt(*rpcURL); err != nil {
			return fmt.Errorf("error fetching verifier salt (pass --salt to build offline): %w", err)
		}
	}
	nonce, err := buildNonce(salt)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(&sweepPayload{
		SignerID:          signer,
		VerifyingContract: verifierContract,
		Deadline:          time.Now().UTC().Add(*deadline).Format(time.RFC3339),
		Nonce:             nonce,
		Intents: []ftWithdrawIntent{{
			Intent:     "ft_withdraw",
			Token:      contract,
			ReceiverID: *receiver,
			Amount:     *amount,
			Memo:       *memo,
		}},
	})
	if err != nil {
		return fmt.Errorf("error encoding payload: %w", err)
	}

	if *out != "" {
		if err := os.WriteFile(*out, payload, 0600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Unsigned payload written to %s. Sign it with:\n  treasurysweep sign --payload %s < treasury.key\n", *out, *out)
		return nil
	}
	fmt.Printf("%s\n", payload)
	return nil
}

// cmdSign signs a payload file and writes the execute_intents argument.
func cmdSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	payloadPath := fs.String("payload", "", "file holding the unsigned payload from build")
	out := fs.String("out", "", "output file for the signed execute_intents argument (default stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *payloadPath == "" {
		return fmt.Errorf("--payload is required")
	}
	raw, err := os.ReadFile(*payloadPath)
	if err != nil {
		return err
	}
	// The signature covers the payload string byte-exactly. A trailing
	// newline from an editor is stripped, and the stripped form is both
	// signed and embedded, so the two can never diverge.
	payload := []byte(strings.TrimRight(string(raw), "\r\n"))

	var p sweepPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("payload is not a valid sweep payload: %w", err)
	}
	if p.VerifyingContract != verifierContract {
		return fmt.Errorf("payload verifying_contract is %q, not %s", p.VerifyingContract, verifierContract)
	}
	if deadline, err := time.Parse(time.RFC3339, p.Deadline); err != nil {
		return fmt.Errorf("unparseable payload deadline %q: %w", p.Deadline, err)
	} else if time.Now().After(deadline) {
		return fmt.Errorf("payload deadline %s has already passed; rebuild", p.Deadline)
	}

	priv, err := readPrivKey()
	if err != nil {
		return err
	}
	sig, err := signPayload(payload, priv)
	if err != nil {
		return err
	}
	// Prove the key matches the payload's signer before emitting anything:
	// recover the identity exactly as the verifier will.
	recovered, err := recoverIdentity(payload, sig)
	if err != nil {
		return err
	}
	if recovered != strings.ToLower(p.SignerID) {
		return fmt.Errorf("MISMATCH: the supplied key signs as %s, but the payload's signer_id is %s", recovered, p.SignerID)
	}

	envelope, err := json.MarshalIndent(&signedEnvelope{Signed: []multiPayload{{
		Standard:  "erc191",
		Payload:   string(payload),
		Signature: sig,
	}}}, "", "  ")
	if err != nil {
		return fmt.Errorf("error encoding signed envelope: %w", err)
	}

	if *out != "" {
		if err := os.WriteFile(*out, envelope, 0600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Signed by %s. Dry-run with:\n  treasurysweep simulate --signed %s\nSubmit from any funded NEAR account, e.g.:\n  near contract call-function as-transaction %s execute_intents file-args %s prepaid-gas '100.0 Tgas' attached-deposit '0 NEAR' sign-as <submitter.near> network-config mainnet\n",
			recovered, *out, verifierContract, *out)
		return nil
	}
	fmt.Printf("%s\n", envelope)
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
