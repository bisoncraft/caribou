// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/decred/base58"
)

func newTestTokenWallet(t *testing.T, rpc rpcI, db txdb.NetworkTxDB) *TokenWallet {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x05}, 32))
	pub := priv.Public().(ed25519.PublicKey)
	addr := base58.Encode(pub)
	parent := &Wallet{
		address: addr,
		pubKey:  pub,
		privKey: priv,
		rpc:     rpc,
		txDB:    db,
		log:     util.Disabled,
		ctx:     context.Background(),
	}
	var owner [32]byte
	copy(owner[:], pub)
	mint := mustDecodePubKey(TokenUSDC.NetMints[util.Mainnet])
	ata, err := deriveATA(owner, mint)
	if err != nil {
		t.Fatalf("deriveATA: %v", err)
	}
	return &TokenWallet{
		parent:  parent,
		assetID: usdcTokenID,
		token:   TokenUSDC,
		mint:    mint,
		owner:   owner,
		ata:     ata,
		ataAddr: base58.Encode(ata[:]),
		txDB:    db,
	}
}

func TestTokenWalletDepositAddress(t *testing.T) {
	tw := newTestTokenWallet(t, &mockRPC{}, nil)
	addr, err := tw.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress error: %v", err)
	}
	if addr != tw.ataAddr {
		t.Fatalf("DepositAddress = %q, want %q", addr, tw.ataAddr)
	}
	owns, err := tw.OwnsDepositAddress(addr)
	if err != nil || !owns {
		t.Fatalf("OwnsDepositAddress(%q) = %v, %v; want true, nil", addr, owns, err)
	}
	if owns2, _ := tw.OwnsDepositAddress(tw.parent.address); owns2 {
		t.Fatal("token wallet should not own the parent's native address as its deposit address")
	}
}

func TestTokenWalletBalance(t *testing.T) {
	rpc := &mockRPC{tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 42_000_000, true, nil }}
	tw := newTestTokenWallet(t, rpc, nil)
	bal, err := tw.Balance()
	if err != nil {
		t.Fatalf("Balance error: %v", err)
	}
	if bal.Available != 42_000_000 {
		t.Fatalf("Available = %d, want 42000000", bal.Available)
	}
}

func TestTokenWalletBalanceNoAccount(t *testing.T) {
	rpc := &mockRPC{tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 0, false, nil }}
	tw := newTestTokenWallet(t, rpc, nil)
	bal, err := tw.Balance()
	if err != nil {
		t.Fatalf("Balance error: %v", err)
	}
	if bal.Available != 0 {
		t.Fatalf("Available = %d, want 0 for a token account that has never received funds", bal.Available)
	}
}

func TestTokenWalletSend(t *testing.T) {
	recipientPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x06}, 32)).Public().(ed25519.PublicKey)
	recipientAddr := base58.Encode(recipientPub)

	var sentTxB []byte
	blockhash := base58.Encode(bytes.Repeat([]byte{0x08}, 32))
	rpc := &mockRPC{
		tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 10_000_000, true, nil },
		blockhashFunc:    func() (string, uint64, error) { return blockhash, 1000, nil },
		balanceFunc:      func() (uint64, error) { return 1_000_000_000, nil },
		sendTxFunc: func(txB []byte) (string, error) {
			sentTxB = txB
			return "sig", nil
		},
	}
	db := &mockTxDB{}
	tw := newTestTokenWallet(t, rpc, db)

	coin, err := tw.Send(recipientAddr, 1_000_000, 0)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if coin.Value() != 1_000_000 {
		t.Fatalf("coin value = %d, want 1000000", coin.Value())
	}
	if len(sentTxB) == 0 {
		t.Fatal("expected a transaction to be submitted")
	}
	if len(db.stored) != 1 {
		t.Fatalf("expected 1 stored tx, got %d", len(db.stored))
	}
	if db.stored[0].Type != txdb.Send {
		t.Fatalf("expected Send tx type, got %v", db.stored[0].Type)
	}
	if db.stored[0].Amount != 1_000_000 {
		t.Fatalf("stored amount = %d, want 1000000", db.stored[0].Amount)
	}
}

func TestTokenWalletSendInsufficientRentForNewATA(t *testing.T) {
	recipientPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x06}, 32)).Public().(ed25519.PublicKey)
	recipientAddr := base58.Encode(recipientPub)

	ownerPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x05}, 32)).Public().(ed25519.PublicKey)
	var owner [32]byte
	copy(owner[:], ownerPub)
	mint := mustDecodePubKey(TokenUSDC.NetMints[util.Mainnet])
	srcATA, err := deriveATA(owner, mint)
	if err != nil {
		t.Fatalf("deriveATA: %v", err)
	}
	srcATAAddr := base58.Encode(srcATA[:])

	blockhash := base58.Encode(bytes.Repeat([]byte{0x08}, 32))
	rpc := &mockRPC{
		tokenBalanceFunc: func(addr string) (uint64, bool, error) {
			if addr == srcATAAddr {
				return 10_000_000, true, nil
			}
			// The recipient's ATA does not exist yet, so a rent-exempt
			// reserve is required to create it.
			return 0, false, nil
		},
		rentFunc:      func(_ uint64) (uint64, error) { return 2_039_280, nil },
		blockhashFunc: func() (string, uint64, error) { return blockhash, 1000, nil },
		// Enough to cover the network fee alone, but not fee + rent.
		balanceFunc: func() (uint64, error) { return 6_000, nil },
	}
	tw := newTestTokenWallet(t, rpc, &mockTxDB{})

	_, err = tw.Send(recipientAddr, 1_000_000, 0)
	if err == nil {
		t.Fatal("expected an error when SOL balance can't cover new ATA rent")
	}
}

func TestTokenWalletSendInsufficientTokenBalance(t *testing.T) {
	recipientPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x06}, 32)).Public().(ed25519.PublicKey)
	recipientAddr := base58.Encode(recipientPub)
	rpc := &mockRPC{tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 100, true, nil }}
	tw := newTestTokenWallet(t, rpc, &mockTxDB{})
	_, err := tw.Send(recipientAddr, 1_000_000, 0)
	if !errors.Is(err, asset.ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestTokenWalletSendInvalidAddress(t *testing.T) {
	rpc := &mockRPC{tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 10_000_000, true, nil }}
	tw := newTestTokenWallet(t, rpc, &mockTxDB{})
	if _, err := tw.Send("not-an-address", 1_000_000, 0); err == nil {
		t.Fatal("expected error for invalid recipient address")
	}
}

func TestWalletTransactionFromSPLRPC(t *testing.T) {
	ataAddr := base58.Encode(bytes.Repeat([]byte{0x11}, 32))
	other := base58.Encode(bytes.Repeat([]byte{0x22}, 32))

	tx := &rpcTransaction{
		Slot: 42,
		Meta: &rpcTransactionMeta{Fee: 5000},
	}
	tx.Transaction.Signatures = []string{"sig1"}
	tx.Transaction.Message.Instructions = []rpcInstruction{
		parsedSPLInstruction(other, ataAddr, 2_500_000),
	}

	wt := walletTransactionFromSPLRPC(tx, ataAddr)
	if wt == nil {
		t.Fatal("expected a parsed wallet transaction")
	}
	if wt.Type != txdb.Receive {
		t.Fatalf("Type = %v, want Receive", wt.Type)
	}
	if wt.Amount != 2_500_000 {
		t.Fatalf("Amount = %d, want 2500000", wt.Amount)
	}
}

func TestWalletTransactionFromSPLRPCIrrelevant(t *testing.T) {
	ataAddr := base58.Encode(bytes.Repeat([]byte{0x11}, 32))
	a := base58.Encode(bytes.Repeat([]byte{0x22}, 32))
	b := base58.Encode(bytes.Repeat([]byte{0x33}, 32))

	tx := &rpcTransaction{
		Slot: 42,
		Meta: &rpcTransactionMeta{Fee: 5000},
	}
	tx.Transaction.Signatures = []string{"sig1"}
	tx.Transaction.Message.Instructions = []rpcInstruction{
		parsedSPLInstruction(a, b, 2_500_000),
	}

	if wt := walletTransactionFromSPLRPC(tx, ataAddr); wt != nil {
		t.Fatal("expected nil for a transaction not touching our ATA")
	}
}
