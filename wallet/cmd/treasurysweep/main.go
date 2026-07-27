// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Command treasurysweep builds, signs, and simulates NEAR Intents verifier
// withdrawals for the fee treasury.
//
// Application fees accrue as NEP-245 multi-token balances credited to the
// treasury identity (an 0x-style secp256k1 account) inside the verifier
// contract, intents.near. Withdrawing them requires an intent signed by the
// treasury key under ERC-191 and submitted to the verifier's
// execute_intents method by any funded NEAR account — the submitter only
// pays gas; authorization comes entirely from the signature.
//
// Subcommands:
//
//	balances   list the treasury's nonzero verifier balances
//	build      construct an unsigned ft_withdraw payload
//	sign       sign a payload with the treasury key (offline-capable)
//	simulate   dry-run a signed payload against the verifier
//
// A typical sweep is balances → build → sign → simulate → submit. The
// build step needs the network (it fetches the verifier's current nonce
// salt) unless --salt is given, and sign is fully offline, so the two can
// run on different machines: build the payload online, carry it to the
// air-gapped machine holding the treasury key, sign there, and carry the
// signed artifact back. The private key is only ever read from standard
// input, never from an argument or file flag, so it is not recorded in
// shell history or the process list.
//
// Submission is deliberately out of scope: the signed artifact is the
// exact JSON argument for execute_intents, and any NEAR CLI or wallet with
// a funded account can submit it. sign prints a ready-to-run command.
package main

import (
	"fmt"
	"os"
)

const verifierContract = "intents.near"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "balances":
		err = cmdBalances(os.Args[2:])
	case "build":
		err = cmdBuild(os.Args[2:])
	case "sign":
		err = cmdSign(os.Args[2:])
	case "simulate":
		err = cmdSimulate(os.Args[2:])
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
	fmt.Fprint(os.Stderr, `treasurysweep — build, sign, and simulate NEAR Intents treasury withdrawals

Usage:
  treasurysweep balances --treasury <0x identity> [--rpc URL]
      List the treasury's nonzero NEP-245 balances inside intents.near,
      resolved against the live 1Click asset catalog.

  treasurysweep build --treasury <0x identity> --token <nep141:...>
      --amount <base units> --receiver <NEAR account> [--memo <memo>]
      [--deadline <duration>] [--salt <8 hex>] [--rpc URL] [--out <file>]
      Construct the unsigned withdrawal payload. The verifier requires
      nonces prefixed with its current salt, fetched over RPC unless
      --salt is given (for offline builds). Without --memo the tokens
      land at a NEAR account. For a PoA-factory bridged token, the
      external-chain withdrawal form is --receiver <token contract>
      --memo WITHDRAW_TO:<external address>; other bridges may differ,
      so simulate before submitting an external withdrawal.

  treasurysweep sign --payload <file> [--out <file>]
      Sign the payload with the treasury private key, read from stdin,
      and write the execute_intents argument JSON. Runs fully offline.
      The recovered signer is checked against the payload's signer_id.

  treasurysweep simulate --signed <file> [--rpc URL]
      Dry-run the signed withdrawal against the verifier's
      simulate_intents view method and print the result.

The signed output is the verbatim argument for intents.near
execute_intents; submit it from any funded NEAR account.
`)
}
