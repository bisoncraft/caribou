//go:build manual

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/decred/base58"
)

// Manual, not-committed harness driver for the SPL token support added in
// df68381. Run against a live sol-harness simnet validator:
//
//	go test -tags manual -run TestManualSPLTokenRoundTrip -v ./wallet/asset/sol
//
// It hand-rolls SPL Token mint creation (System::CreateAccount +
// Token::InitializeMint2 + Token::MintTo) since no real USDC/USDT mint
// exists on a fresh local ledger, then drives the real TokenWallet.Send
// code path to move tokens and confirms receipt on the recipient's ATA.

const (
	sysCreateAccount   = 0
	splInitializeMint2 = 20
	splMintTo          = 7
	mintAccountSize    = 82
)

func createAccountInstruction(payer, newAccount [32]byte, lamports, space uint64, owner [32]byte) splInstruction {
	data := make([]byte, 4+8+8+32)
	binary.LittleEndian.PutUint32(data[0:4], sysCreateAccount)
	binary.LittleEndian.PutUint64(data[4:12], lamports)
	binary.LittleEndian.PutUint64(data[12:20], space)
	copy(data[20:52], owner[:])
	return splInstruction{
		programID: mustDecodePubKey(systemProgramID),
		accounts: []accountMeta{
			{pubkey: payer, isSigner: true, isWritable: true},
			{pubkey: newAccount, isSigner: true, isWritable: true},
		},
		data: data,
	}
}

func initializeMint2Instruction(mint [32]byte, decimals uint8, mintAuthority [32]byte) splInstruction {
	data := make([]byte, 0, 35)
	data = append(data, splInitializeMint2, decimals)
	data = append(data, mintAuthority[:]...)
	data = append(data, 0) // freeze authority: None
	return splInstruction{
		programID: mustDecodePubKey(tokenProgramID),
		accounts:  []accountMeta{{pubkey: mint, isWritable: true}},
		data:      data,
	}
}

func mintToInstruction(mint, destination, authority [32]byte, amount uint64) splInstruction {
	data := make([]byte, 9)
	data[0] = splMintTo
	binary.LittleEndian.PutUint64(data[1:9], amount)
	return splInstruction{
		programID: mustDecodePubKey(tokenProgramID),
		accounts: []accountMeta{
			{pubkey: mint, isWritable: true},
			{pubkey: destination, isWritable: true},
			{pubkey: authority, isSigner: true},
		},
		data: data,
	}
}

// signMultiSPLTx signs msg with each of signers, in the order they appear as
// writable-signer accounts in the compiled message, and wraps the result in
// a transaction.
func signMultiSPLTx(msg []byte, signers []ed25519.PrivateKey) []byte {
	tx := shortVec(len(signers))
	for _, sk := range signers {
		tx = append(tx, ed25519.Sign(sk, msg)...)
	}
	return append(tx, msg...)
}

func manualAirdrop(t *testing.T, rpc *rpcClient, addr string, lamports uint64) {
	t.Helper()
	var sig string
	err := rpc.call(context.Background(), "requestAirdrop", []any{addr, lamports, map[string]any{"commitment": "confirmed"}}, &sig)
	if err != nil {
		t.Fatalf("requestAirdrop: %v", err)
	}
	manualConfirm(t, rpc, sig)
}

func manualConfirm(t *testing.T, rpc *rpcClient, sig string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		statuses, err := rpc.getSignatureStatuses(ctx, []string{sig})
		if err == nil && len(statuses) > 0 && statuses[0] != nil && statuses[0].ConfirmationStatus != "" {
			if statuses[0].Err != nil {
				t.Fatalf("tx %s failed on-chain: %v", sig, statuses[0].Err)
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("tx %s did not confirm in time", sig)
}

func TestManualSPLTokenRoundTrip(t *testing.T) {
	rpc := newRPCClient("http://127.0.0.1:18999", "ws://127.0.0.1:19000")
	ctx := context.Background()

	for _, tokenName := range []string{"USDC", "USDT"} {
		t.Run(tokenName, func(t *testing.T) {
			// --- fund a sender keypair ---
			senderPriv := ed25519.NewKeyFromSeed([]byte(tokenName + "-sender-seed-32-bytes-long!!!!!")[:32])
			senderPub := senderPriv.Public().(ed25519.PublicKey)
			var sender [32]byte
			copy(sender[:], senderPub)
			senderAddr := base58.Encode(senderPub)
			manualAirdrop(t, rpc, senderAddr, 5_000_000_000) // 5 SOL

			recipientPriv := ed25519.NewKeyFromSeed([]byte(tokenName + "-recipient-seed-32-bytes-long!!!")[:32])
			recipientPub := recipientPriv.Public().(ed25519.PublicKey)
			recipientAddr := base58.Encode(recipientPub)

			// --- create the mint (hand-rolled: CreateAccount + InitializeMint2) ---
			mintPriv := ed25519.NewKeyFromSeed([]byte(tokenName + "-mint-seed-32-bytes-long!!!!!!!!")[:32])
			mintPub := mintPriv.Public().(ed25519.PublicKey)
			var mint [32]byte
			copy(mint[:], mintPub)

			rentLamports, err := rpc.getMinimumBalanceForRentExemption(ctx, mintAccountSize)
			if err != nil {
				t.Fatalf("getMinimumBalanceForRentExemption: %v", err)
			}
			bh, _, err := rpc.getLatestBlockhash(ctx)
			if err != nil {
				t.Fatalf("getLatestBlockhash: %v", err)
			}
			var bhArr [32]byte
			copy(bhArr[:], base58.Decode(bh))

			createMintInstrs := []splInstruction{
				createAccountInstruction(sender, mint, rentLamports, mintAccountSize, mustDecodePubKey(tokenProgramID)),
				initializeMint2Instruction(mint, 6, sender),
			}
			createMintMsg := compileSPLMessage(sender, createMintInstrs, bhArr)
			createMintTx := signMultiSPLTx(createMintMsg, []ed25519.PrivateKey{senderPriv, mintPriv})
			sig, err := rpc.sendTransaction(ctx, createMintTx)
			if err != nil {
				t.Fatalf("sendTransaction (create mint): %v", err)
			}
			manualConfirm(t, rpc, sig)
			t.Logf("%s mint created: %s (tx %s)", tokenName, base58.Encode(mint[:]), sig)

			// --- create the sender's ATA and mint initial supply into it ---
			senderATA, err := deriveATA(sender, mint)
			if err != nil {
				t.Fatalf("deriveATA(sender): %v", err)
			}
			bh2, _, err := rpc.getLatestBlockhash(ctx)
			if err != nil {
				t.Fatalf("getLatestBlockhash: %v", err)
			}
			var bh2Arr [32]byte
			copy(bh2Arr[:], base58.Decode(bh2))
			const initialSupply = 50_000_000 // 50 tokens at 6 decimals
			mintToInstrs := []splInstruction{
				createAssociatedTokenAccountIdempotentInstruction(sender, senderATA, sender, mint),
				mintToInstruction(mint, senderATA, sender, initialSupply),
			}
			mintToMsg := compileSPLMessage(sender, mintToInstrs, bh2Arr)
			mintToTx, mintToSig := signSPLTx(mintToMsg, senderPriv)
			if _, err := rpc.sendTransaction(ctx, mintToTx); err != nil {
				t.Fatalf("sendTransaction (mint to sender): %v", err)
			}
			manualConfirm(t, rpc, mintToSig)
			t.Logf("%s minted %d atomic units to sender ATA %s (tx %s)", tokenName, initialSupply, base58.Encode(senderATA[:]), mintToSig)

			// --- drive the real production TokenWallet.Send path ---
			token := &Token{
				Name:     tokenName,
				Decimals: 6,
				NetMints: map[util.Network]string{util.Simnet: base58.Encode(mint[:])},
			}
			parent := &Wallet{
				address: senderAddr,
				pubKey:  senderPub,
				privKey: senderPriv,
				log:     util.Disabled,
				net:     util.Simnet,
			}
			var parentRPC rpcI = rpc
			parent.rpc.Store(&parentRPC)
			parent.ctx.Store(&ctx)
			tw := &TokenWallet{
				parent:  parent,
				assetID: 0,
				token:   token,
				mint:    mint,
				owner:   sender,
				ata:     senderATA,
				ataAddr: base58.Encode(senderATA[:]),
			}

			preBalance, err := tw.Balance()
			if err != nil {
				t.Fatalf("Balance (pre-send): %v", err)
			}
			t.Logf("%s sender balance before send: %d", tokenName, preBalance.Available)

			const sendAmount = 10_000_000 // 10 tokens
			coin, err := tw.Send(recipientAddr, sendAmount, 0)
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			t.Logf("%s Send submitted, coin value %d", tokenName, coin.Value())

			// --- confirm receipt on the recipient's ATA ---
			recipientOwner := mustDecodePubKey(recipientAddr)
			recipientATA, err := deriveATA(recipientOwner, mint)
			if err != nil {
				t.Fatalf("deriveATA(recipient): %v", err)
			}
			recipientATAAddr := base58.Encode(recipientATA[:])

			var recvAmount uint64
			var recvExists bool
			for i := 0; i < 20; i++ {
				recvAmount, recvExists, err = rpc.getTokenAccountBalance(ctx, recipientATAAddr)
				if err == nil && recvExists && recvAmount == sendAmount {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if err != nil {
				t.Fatalf("getTokenAccountBalance(recipient): %v", err)
			}
			if !recvExists {
				t.Fatalf("%s recipient ATA %s was not created", tokenName, recipientATAAddr)
			}
			if recvAmount != sendAmount {
				t.Fatalf("%s recipient balance = %d, want %d", tokenName, recvAmount, sendAmount)
			}
			t.Logf("%s CONFIRMED: recipient ATA %s received %d atomic units", tokenName, recipientATAAddr, recvAmount)
		})
	}
}
