// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
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
		txDB:    db,
		log:     util.Disabled,
	}
	parent.conn.Store(&conn{rpc: rpc, ctx: context.Background()})
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
	// The deposit address must be the owner address, not the ATA: senders
	// derive the destination ATA from it themselves (matching Send and
	// ecosystem convention), so publishing the ATA would make senders
	// derive an ATA-of-ATA and strand funds.
	if addr != tw.parent.address {
		t.Fatalf("DepositAddress = %q, want %q", addr, tw.parent.address)
	}
	owns, err := tw.OwnsDepositAddress(addr)
	if err != nil || !owns {
		t.Fatalf("OwnsDepositAddress(%q) = %v, %v; want true, nil", addr, owns, err)
	}
	if owns2, _ := tw.OwnsDepositAddress(tw.ataAddr); owns2 {
		t.Fatal("token wallet should not own its own ATA as a deposit address")
	}
}

// TestMetaKeyDistinctPerAsset guards against the scan-cursor collision bug.
// The native wallet and every token wallet on this network share one meta
// table, and they all store the same field names, so the asset scope in the
// key is the only thing keeping native/usdc/usdt from overwriting each other's
// lastScannedSignature cursor. A collision here would not error: one wallet
// would silently resume from another's cursor and report wrong history.
func TestMetaKeyDistinctPerAsset(t *testing.T) {
	native := metaKey(BipID, lastScannedSigField)
	usdc := metaKey(usdcTokenID, lastScannedSigField)
	usdt := metaKey(usdtTokenID, lastScannedSigField)

	for _, c := range []struct {
		a, b []byte
		desc string
	}{
		{usdc, usdt, "usdc vs usdt"},
		{usdc, native, "usdc vs native"},
		{usdt, native, "usdt vs native"},
	} {
		if bytes.Equal(c.a, c.b) {
			t.Errorf("%s meta keys collide, both = %x", c.desc, c.a)
		}
	}

	// Distinct fields for one asset must also not collide, and the asset scope
	// must not bleed into the next field: a same-length field name sharing a
	// prefix is the case a naive concatenation gets wrong.
	if bytes.Equal(metaKey(usdcTokenID, beforeCursorField), metaKey(usdcTokenID, backfillUntilField)) {
		t.Error("distinct fields for one asset collide")
	}
}

// TestUSDCNoTestnetMint guards against listing a mint for a cluster it
// doesn't actually exist on: Circle's only public test mint is on the
// devnet cluster, but DefaultRPCEndpoints[util.Testnet] points at the
// testnet cluster, so a util.Testnet mint entry here would make USDC dead
// on arrival there (balance always empty, sends rejected) instead of
// simply being unavailable, as OpenTokenWallet reports when unlisted.
func TestUSDCNoTestnetMint(t *testing.T) {
	if _, ok := TokenUSDC.NetMints[util.Testnet]; ok {
		t.Fatal("TokenUSDC must not list a util.Testnet mint: Circle has no public mint on the testnet cluster")
	}

	w := &Wallet{net: util.Testnet}
	if _, err := w.OpenTokenWallet(&asset.TokenConfig{AssetID: usdcTokenID}); err == nil {
		t.Fatal("expected OpenTokenWallet to fail for usdc on testnet (no mint), not silently succeed")
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
	// TokenID must be set: token wallets share their parent network's
	// NetworkTxDB, and GetAssetTxs/GetAssetPendingTxs rely on it to
	// distinguish this record from the native wallet's (and sibling
	// tokens') transactions.
	if db.stored[0].TokenID == nil || *db.stored[0].TokenID != usdcTokenID {
		t.Fatalf("stored TokenID = %v, want %d", db.stored[0].TokenID, usdcTokenID)
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

	wt := walletTransactionFromSPLRPC(tx, ataAddr, usdcTokenID)
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

	if wt := walletTransactionFromSPLRPC(tx, ataAddr, usdcTokenID); wt != nil {
		t.Fatal("expected nil for a transaction not touching our ATA")
	}
}

// TestTokenWalletCheckPendingTxs guards against a token Send staying pending
// forever: before TokenWallet had its own checkPendingTxs, only syncHistory
// tracked confirmations, which never learns about a tx that expired without
// landing on-chain.
func TestTokenWalletCheckPendingTxs(t *testing.T) {
	ctx := context.Background()
	tokenID := usdcTokenID

	makePendingTx := func(id string, lastValidHeight uint64) *txdb.WalletTransaction {
		wt := &txdb.WalletTransaction{ID: id, TokenID: &tokenID}
		setTxData(wt, solTxData{LastValidBlockHeight: lastValidHeight})
		return wt
	}

	t.Run("expired and absent on-chain marks tx rejected", func(t *testing.T) {
		tx := makePendingTx("tok-pend", 100)
		db := &mockTxDB{pending: []*txdb.WalletTransaction{tx}}
		rpc := &mockRPC{
			txFunc: func(_ string) (*rpcTransaction, error) {
				return nil, nil // confirmed absent
			},
			sigStatusesFunc: func(_ []string) ([]*signatureStatus, error) {
				return []*signatureStatus{nil}, nil
			},
			blockHeightFunc: func() (uint64, error) {
				return 200, nil // past LastValidBlockHeight
			},
		}
		tw := newTestTokenWallet(t, rpc, db)

		if err := tw.checkPendingTxs(ctx); err != nil {
			t.Fatal(err)
		}
		if len(db.stored) != 1 {
			t.Fatalf("want 1 stored (rejected), got %d", len(db.stored))
		}
		if !db.stored[0].Rejected {
			t.Fatal("tx should be marked Rejected")
		}
	})

	t.Run("still within window and no status leaves tx pending", func(t *testing.T) {
		tx := makePendingTx("tok-pend2", 100)
		db := &mockTxDB{pending: []*txdb.WalletTransaction{tx}}
		rpc := &mockRPC{
			sigStatusesFunc: func(_ []string) ([]*signatureStatus, error) {
				return []*signatureStatus{nil}, nil
			},
			blockHeightFunc: func() (uint64, error) {
				return 50, nil // still before LastValidBlockHeight
			},
		}
		tw := newTestTokenWallet(t, rpc, db)

		if err := tw.checkPendingTxs(ctx); err != nil {
			t.Fatal(err)
		}
		if len(db.stored) != 0 {
			t.Fatalf("tx should not have been stored while still within its valid window, got %d stored", len(db.stored))
		}
	})
}

// TestSharedMetaTableCursorsDoNotCollide exercises the accessors themselves,
// not just the key builder: the native wallet and two token wallets all share
// one meta table, so a token accessor that scoped its key to BipID instead of
// its own asset ID would compile, pass TestMetaKeyDistinctPerAsset, and still
// clobber the native cursor.
//
// The failure it guards against is silent. A wallet that reads back another
// asset's signature resumes its scan from the wrong point in the chain and
// reports incomplete or foreign transaction history, with no error anywhere.
func TestSharedMetaTableCursorsDoNotCollide(t *testing.T) {
	table := newMockTable()

	native := newTestWallet("nativeAddr", nil, nil, table)
	usdc := newTestTokenWallet(t, nil, nil)
	usdc.metaTable = table
	usdt := newTestTokenWallet(t, nil, nil)
	usdt.assetID = usdtTokenID
	usdt.metaTable = table

	if err := native.setLastScannedSignature("native-sig"); err != nil {
		t.Fatalf("native set: %v", err)
	}
	if err := usdc.setLastScannedSignature("usdc-sig"); err != nil {
		t.Fatalf("usdc set: %v", err)
	}
	if err := usdt.setLastScannedSignature("usdt-sig"); err != nil {
		t.Fatalf("usdt set: %v", err)
	}

	for _, c := range []struct {
		name string
		get  func() (string, error)
		want string
	}{
		{"native", native.getLastScannedSignature, "native-sig"},
		{"usdc", usdc.getLastScannedSignature, "usdc-sig"},
		{"usdt", usdt.getLastScannedSignature, "usdt-sig"},
	} {
		got, err := c.get()
		if err != nil {
			t.Fatalf("%s get: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s cursor = %q, want %q", c.name, got, c.want)
		}
	}

	// Three assets, three rows: no wallet overwrote another's.
	if len(table.data) != 3 {
		t.Errorf("meta table holds %d rows, want 3", len(table.data))
	}
}
