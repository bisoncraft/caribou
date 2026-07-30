// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Command treasurykey generates and validates the fee-account keypair —
// the "treasury" of the command name is Bisoncraft's fee-collection
// account inside the NEAR Intents verifier contract, intents.near.
//
// NEAR Intents derives an account identity from a public key with no
// on-chain registration: a secp256k1 key yields a 0x-style identity — the
// standard Ethereum address (the last 20 bytes of the Keccak-256 hash of
// the uncompressed public key). That identity, in lowercase, is the value
// configured as the appFees recipient (intentsFeeRecipient in
// wallet/core/near-intents.go). Accrued fees are withdrawn from the
// verifier later by signing intents with this same key under ERC-191.
//
// The private key must never be committed or stored in the repository.
// This tool prints it once, for the operator to move to cold storage;
// run it on an air-gapped machine for a production fee-account key.
//
// Subcommands:
//
//	gen                generate a new keypair and print the key and identity
//	derive             print the identity for a private key read from stdin
//	verify <identity>  confirm a private key (from stdin) maps to identity
//	check <identity>   validate a 0x identity's format and show its checksum
//
// derive and verify read the private key only from standard input, never
// from a command-line argument, so the key is never recorded in the shell
// history or exposed in the process list. Provide it by redirecting a file
// (treasurykey derive < treasury.key) or by typing it at the prompt.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = cmdGen()
	case "derive":
		err = cmdDerive(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `treasurykey — generate and validate the NEAR Intents fee-account key

The "treasury" of the command name is Bisoncraft's fee-collection account
inside the intents.near verifier. Its identity is an Ethereum-style
address derived from the keypair; the private key authorizes withdrawals
from the account and never appears in the repository or configuration.

Usage:
  treasurykey gen                generate a new keypair; print the private key and identity
  treasurykey derive             print the identity for a private key read from stdin
  treasurykey verify <identity>  confirm a private key (from stdin) maps to <identity>
  treasurykey check <identity>   validate a 0x identity's format and show its EIP-55 checksum

derive and verify read the private key only from stdin — never from a
command-line argument — so it is never recorded in the shell history or
exposed in the process list. Provide the key by redirecting a file:

  treasurykey derive < treasury.key

or by running the command and typing the key at the prompt. Generate a
production key on an air-gapped machine and store the printed key offline.
`)
}

// identities prints the two forms of a fee-account identity: the lowercase
// form used as the configured recipient (a NEAR account ID cannot contain
// uppercase), and the EIP-55 checksummed form for visual verification.
func identities(addr common.Address) {
	fmt.Printf("Fee-account identity (config value, appFees recipient):\n  %s\n", strings.ToLower(addr.Hex()))
	fmt.Printf("Checksummed form (for visual verification only):\n  %s\n", addr.Hex())
}

func cmdGen() error {
	priv, err := crypto.GenerateKey()
	if err != nil {
		return fmt.Errorf("key generation failed: %w", err)
	}
	addr := crypto.PubkeyToAddress(priv.PublicKey)

	fmt.Println("=== NEAR Intents fee-account key ===")
	fmt.Println()
	fmt.Println("PRIVATE KEY — store offline, never commit or paste into config:")
	fmt.Printf("  %s\n", hex.EncodeToString(crypto.FromECDSA(priv)))
	fmt.Println()
	identities(addr)
	fmt.Println()
	fmt.Println("Set the lowercase identity as intentsFeeRecipient in")
	fmt.Println("wallet/core/near-intents.go. The private key is required only to")
	fmt.Println("withdraw accrued fees from the verifier; keep it in cold storage.")
	return nil
}

func cmdDerive(args []string) error {
	if len(args) != 0 {
		return errKeyIsStdin
	}
	priv, err := readPrivKey()
	if err != nil {
		return err
	}
	identities(crypto.PubkeyToAddress(priv.PublicKey))
	return nil
}

func cmdVerify(args []string) error {
	if len(args) != 1 {
		// A second argument would be the private key; the key must come from
		// stdin so it is not recorded in the shell history.
		return fmt.Errorf("verify takes only the <identity> argument; %w", errKeyIsStdin)
	}
	want, err := parseIdentity(args[0])
	if err != nil {
		return err
	}
	priv, err := readPrivKey()
	if err != nil {
		return err
	}
	got := crypto.PubkeyToAddress(priv.PublicKey)
	if got != want {
		return fmt.Errorf("MISMATCH: key derives %s, not %s", strings.ToLower(got.Hex()), strings.ToLower(want.Hex()))
	}
	fmt.Printf("OK: private key maps to %s\n", strings.ToLower(want.Hex()))
	return nil
}

func cmdCheck(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("check requires exactly one <identity> argument")
	}
	addr, err := parseIdentity(args[0])
	if err != nil {
		return err
	}
	identities(addr)
	return nil
}

// parseIdentity validates a 0x-style identity string and returns the
// address. It rejects anything that is not exactly 20 hex-encoded bytes
// with a 0x prefix, so a typo or truncated value is caught rather than
// silently zero-padded.
func parseIdentity(s string) (common.Address, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return common.Address{}, fmt.Errorf("identity must start with 0x")
	}
	body := s[2:]
	if len(body) != 40 {
		return common.Address{}, fmt.Errorf("identity must be 40 hex characters after 0x, got %d", len(body))
	}
	if _, err := hex.DecodeString(body); err != nil {
		return common.Address{}, fmt.Errorf("identity is not valid hex: %w", err)
	}
	return common.HexToAddress(s), nil
}

// errKeyIsStdin explains that the private key must be supplied on stdin
// rather than as an argument, so it is not recorded in the shell history.
var errKeyIsStdin = fmt.Errorf("the private key must be supplied on stdin, not as an argument, so it is not recorded in shell history; redirect a file (treasurykey derive < treasury.key) or type it at the prompt")

// readPrivKey reads a hex-encoded secp256k1 private key from stdin,
// tolerating a leading 0x and surrounding whitespace. When stdin is a
// terminal it prompts, so an interactive run does not appear to hang.
func readPrivKey() (*ecdsa.PrivateKey, error) {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "Enter the private key (hex) and press Enter: ")
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) == "" {
		if err != nil {
			return nil, fmt.Errorf("no private key on stdin; %w", errKeyIsStdin)
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
