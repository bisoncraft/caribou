// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset/keystore"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/decred/base58"
)

// --- mocks ---

type mockRPC struct {
	sigsFunc         func(before, until *string, limit int) ([]signatureInfo, error)
	txFunc           func(sig string) (*rpcTransaction, error)
	blockHeightFunc  func() (uint64, error)
	sigStatusesFunc  func(ids []string) ([]*signatureStatus, error)
	tokenBalanceFunc func(addr string) (uint64, bool, error)
	rentFunc         func(size uint64) (uint64, error)
	blockhashFunc    func() (string, uint64, error)
	sendTxFunc       func(txB []byte) (string, error)
	balanceFunc      func() (uint64, error)
}

func (m *mockRPC) getSignaturesForAddress(_ context.Context, _ string, before, until *string, limit int) ([]signatureInfo, error) {
	return m.sigsFunc(before, until, limit)
}
func (m *mockRPC) getTransaction(_ context.Context, sig string) (*rpcTransaction, error) {
	return m.txFunc(sig)
}
func (m *mockRPC) getSlot(_ context.Context) (uint64, error) { return 0, nil }
func (m *mockRPC) getBlockHeight(_ context.Context) (uint64, error) {
	if m.blockHeightFunc != nil {
		return m.blockHeightFunc()
	}
	return 0, nil
}
func (m *mockRPC) getBalance(_ context.Context, _ string) (uint64, error) {
	if m.balanceFunc != nil {
		return m.balanceFunc()
	}
	return 0, nil
}
func (m *mockRPC) getMinimumBalanceForRentExemption(_ context.Context, size uint64) (uint64, error) {
	if m.rentFunc != nil {
		return m.rentFunc(size)
	}
	return 0, nil
}
func (m *mockRPC) getFeeForMessage(_ context.Context, _ []byte) (uint64, error) { return 0, nil }
func (m *mockRPC) getLatestBlockhash(_ context.Context) (string, uint64, error) {
	if m.blockhashFunc != nil {
		return m.blockhashFunc()
	}
	return "", 0, nil
}
func (m *mockRPC) sendTransaction(_ context.Context, txB []byte) (string, error) {
	if m.sendTxFunc != nil {
		return m.sendTxFunc(txB)
	}
	return "", nil
}
func (m *mockRPC) getSignatureStatuses(_ context.Context, ids []string) ([]*signatureStatus, error) {
	if m.sigStatusesFunc != nil {
		return m.sigStatusesFunc(ids)
	}
	return nil, nil
}
func (m *mockRPC) getTokenAccountBalance(_ context.Context, addr string) (uint64, bool, error) {
	if m.tokenBalanceFunc != nil {
		return m.tokenBalanceFunc(addr)
	}
	return 0, false, nil
}

type mockTxDB struct {
	stored      []*txdb.WalletTransaction
	storeTxsErr error
	pending     []*txdb.WalletTransaction
}

func (m *mockTxDB) StoreTx(wt *txdb.WalletTransaction) error {
	m.stored = append(m.stored, wt)
	return nil
}
func (m *mockTxDB) StoreTxs(wts []*txdb.WalletTransaction) error {
	if m.storeTxsErr != nil {
		return m.storeTxsErr
	}
	m.stored = append(m.stored, wts...)
	return nil
}
func (m *mockTxDB) GetTx(_ string) (*txdb.WalletTransaction, error) { return nil, nil }
func (m *mockTxDB) GetTxs(_ *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	return nil, nil
}
func (m *mockTxDB) GetPendingTxs() ([]*txdb.WalletTransaction, error) { return m.pending, nil }
func (m *mockTxDB) GetAssetTxs(_ *uint32, _ *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	return nil, nil
}
func (m *mockTxDB) GetAssetPendingTxs(assetID *uint32) ([]*txdb.WalletTransaction, error) {
	var out []*txdb.WalletTransaction
	for _, tx := range m.pending {
		switch {
		case assetID == nil && tx.TokenID == nil:
			out = append(out, tx)
		case assetID != nil && tx.TokenID != nil && *tx.TokenID == *assetID:
			out = append(out, tx)
		}
	}
	return out, nil
}
func (m *mockTxDB) RemoveTx(_ string) error                 { return nil }
func (m *mockTxDB) Table(_ string) (txdb.StoreTable, error) { return nil, nil }
func (m *mockTxDB) AddTxIndex(_ string, _ func(*txdb.WalletTransaction) ([]byte, error)) (txdb.TxIndex, error) {
	return nil, nil
}
func (m *mockTxDB) AddUniqueTxIndex(_ string, _ func(*txdb.WalletTransaction) ([]byte, error)) (txdb.TxIndex, error) {
	return nil, nil
}

type mockTable struct {
	data map[string][]byte
}

func newMockTable() *mockTable { return &mockTable{data: make(map[string][]byte)} }

func (t *mockTable) Get(k []byte) ([]byte, error) {
	v, ok := t.data[string(k)]
	if !ok {
		return nil, txdb.ErrKeyNotFound
	}
	return v, nil
}
func (t *mockTable) Set(k, v []byte, _ bool) error {
	t.data[string(k)] = v
	return nil
}
func (t *mockTable) Delete(k []byte) error {
	delete(t.data, string(k))
	return nil
}
func (t *mockTable) SetBatch(_ []txdb.BatchEntry, _ bool) error { return nil }
func (t *mockTable) AddIndex(_ string, _ txdb.IndexFunc) (txdb.StoreIndex, error) {
	return nil, nil
}
func (t *mockTable) AddUniqueIndex(_ string, _ txdb.IndexFunc) (txdb.StoreIndex, error) {
	return nil, nil
}
func (t *mockTable) Iterate(_ []byte, _ func(txdb.StoreIter) error, _ bool, _ []byte) error {
	return nil
}

// newTestWallet returns a Wallet wired with mock rpc, txdb, and meta table.
func newTestWallet(addr string, rpc rpcI, db txdb.NetworkTxDB, table txdb.StoreTable) *Wallet {
	return &Wallet{
		address:   addr,
		rpc:       rpc,
		txDB:      db,
		metaTable: table,
		log:       util.Disabled,
	}
}

// makeSig returns a minimal signatureInfo.
func makeSig(id string) signatureInfo { return signatureInfo{Signature: id} }

// makeWalletTx returns an rpcTransaction that walletTransactionFromRPC will
// classify as a receive for addr.
func makeWalletTx(addr, other string) *rpcTransaction {
	tx := &rpcTransaction{
		Slot: 1,
		Meta: &rpcTransactionMeta{
			Fee:          1000,
			PreBalances:  []uint64{0, 2_000_000},
			PostBalances: []uint64{500_000, 1_495_000},
		},
	}
	tx.Transaction.Signatures = []string{"sig"}
	tx.Transaction.Message.AccountKeys = []rpcAccountKey{
		{Pubkey: addr},
		{Pubkey: other},
	}
	tx.Transaction.Message.Instructions = []rpcInstruction{
		parsedInstruction(other, addr, 500_000),
	}
	return tx
}

func TestPrivKeyFromSeedDeterministic(t *testing.T) {
	seed := bytes.Repeat([]byte{0x01}, 32)
	priv1, zero1, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("privKeyFromSeed error: %v", err)
	}
	defer zero1()
	priv2, zero2, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("second privKeyFromSeed error: %v", err)
	}
	defer zero2()
	if !bytes.Equal(priv1.Seed(), priv2.Seed()) {
		t.Fatal("derived seeds differ for same mnemonic seed")
	}
	if len(priv1.Public().(ed25519.PublicKey)) != ed25519.PublicKeySize {
		t.Fatal("unexpected public key size")
	}
}

func TestValidateAddress(t *testing.T) {
	pub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x02}, 32)).Public().(ed25519.PublicKey)
	addr := base58.Encode(pub)
	w := &Wallet{address: addr}
	if !w.ValidateAddress(addr) {
		t.Fatal("expected generated address to validate")
	}
	if w.ValidateAddress("not-a-sol-address") {
		t.Fatal("expected bad address to fail validation")
	}
}

func TestDecodeCoinID(t *testing.T) {
	sig := bytes.Repeat([]byte{0x03}, 64)
	s, err := decodeCoinID(sig)
	if err != nil {
		t.Fatalf("decodeCoinID error: %v", err)
	}
	if s == "" {
		t.Fatal("empty signature string")
	}
}

// parsedInstruction builds a system/transfer instruction for tests.
func parsedInstruction(src, dst string, lamports uint64) rpcInstruction {
	return rpcInstruction{
		Program: "system",
		Parsed: &struct {
			Type string             `json:"type"`
			Info rpcInstructionInfo `json:"info"`
		}{
			Type: "transfer",
			Info: rpcInstructionInfo{Source: src, Destination: dst, Lamports: lamports},
		},
	}
}

// parsedSPLInstruction builds an spl-token transferChecked instruction for
// tests.
func parsedSPLInstruction(src, dst string, amount uint64) rpcInstruction {
	return rpcInstruction{
		Program: "spl-token",
		Parsed: &struct {
			Type string             `json:"type"`
			Info rpcInstructionInfo `json:"info"`
		}{
			Type: "transferChecked",
			Info: rpcInstructionInfo{
				Source:      src,
				Destination: dst,
				TokenAmount: &struct {
					Amount   string `json:"amount"`
					Decimals uint8  `json:"decimals"`
				}{Amount: fmt.Sprintf("%d", amount), Decimals: 6},
			},
		},
	}
}

func TestWalletTransactionFromRPC(t *testing.T) {
	walletAddr := base58.Encode(bytes.Repeat([]byte{0x11}, 32))
	other := base58.Encode(bytes.Repeat([]byte{0x22}, 32))

	makeTx := func(pre, post []uint64, keys []rpcAccountKey, insts []rpcInstruction) *rpcTransaction {
		tx := &rpcTransaction{
			Slot: 123,
			Meta: &rpcTransactionMeta{Fee: 5000, PreBalances: pre, PostBalances: post},
		}
		tx.Transaction.Signatures = []string{"sig"}
		tx.Transaction.Message.AccountKeys = keys
		tx.Transaction.Message.Instructions = insts
		return tx
	}

	twoKey := []rpcAccountKey{{Pubkey: walletAddr}, {Pubkey: other}}

	t.Run("send", func(t *testing.T) {
		tx := makeTx(
			[]uint64{2_000_000, 0},
			[]uint64{1_495_000, 500_000},
			twoKey,
			[]rpcInstruction{parsedInstruction(walletAddr, other, 500_000)},
		)
		wt := walletTransactionFromRPC(tx, walletAddr)
		if wt == nil {
			t.Fatal("got nil")
		}
		if wt.Type != txdb.Send {
			t.Fatalf("type: want Send, got %v", wt.Type)
		}
		if wt.Amount != 500_000 {
			t.Fatalf("amount: want 500000, got %d", wt.Amount)
		}
		if wt.Recipient == nil || *wt.Recipient != other {
			t.Fatal("recipient not set to destination")
		}
	})

	t.Run("receive", func(t *testing.T) {
		tx := makeTx(
			[]uint64{0, 2_000_000},
			[]uint64{500_000, 1_495_000},
			twoKey,
			[]rpcInstruction{parsedInstruction(other, walletAddr, 500_000)},
		)
		wt := walletTransactionFromRPC(tx, walletAddr)
		if wt == nil {
			t.Fatal("got nil")
		}
		if wt.Type != txdb.Receive {
			t.Fatalf("type: want Receive, got %v", wt.Type)
		}
		if wt.Amount != 500_000 {
			t.Fatalf("amount: want 500000, got %d", wt.Amount)
		}
		if wt.Recipient == nil || *wt.Recipient != walletAddr {
			t.Fatal("recipient not set to wallet address")
		}
	})

	t.Run("self-send", func(t *testing.T) {
		tx := makeTx(
			[]uint64{2_000_000},
			[]uint64{1_995_000},
			[]rpcAccountKey{{Pubkey: walletAddr}},
			[]rpcInstruction{parsedInstruction(walletAddr, walletAddr, 500_000)},
		)
		wt := walletTransactionFromRPC(tx, walletAddr)
		if wt == nil {
			t.Fatal("got nil")
		}
		if wt.Type != txdb.SelfSend {
			t.Fatalf("type: want SelfSend, got %v", wt.Type)
		}
	})

	t.Run("fallback receive", func(t *testing.T) {
		// No parsed instruction — wallet address gains lamports via balance delta.
		tx := makeTx(
			[]uint64{0, 2_000_000},
			[]uint64{500_000, 1_495_000},
			twoKey,
			nil,
		)
		wt := walletTransactionFromRPC(tx, walletAddr)
		if wt == nil {
			t.Fatal("got nil")
		}
		if wt.Type != txdb.Receive {
			t.Fatalf("type: want Receive, got %v", wt.Type)
		}
		if wt.Amount != 500_000 {
			t.Fatalf("amount: want 500000, got %d", wt.Amount)
		}
	})

	t.Run("fallback send", func(t *testing.T) {
		// No parsed instruction — wallet address loses lamports via balance delta.
		tx := makeTx(
			[]uint64{2_000_000, 0},
			[]uint64{1_495_000, 500_000},
			twoKey,
			nil,
		)
		wt := walletTransactionFromRPC(tx, walletAddr)
		if wt == nil {
			t.Fatal("got nil")
		}
		if wt.Type != txdb.Send {
			t.Fatalf("type: want Send, got %v", wt.Type)
		}
		// amount = pre-post-fee = 2_000_000-1_495_000-5_000 = 500_000
		if wt.Amount != 500_000 {
			t.Fatalf("amount: want 500000, got %d", wt.Amount)
		}
	})

	t.Run("wallet not in accounts returns nil", func(t *testing.T) {
		tx := makeTx(
			[]uint64{2_000_000},
			[]uint64{1_495_000},
			[]rpcAccountKey{{Pubkey: other}},
			nil,
		)
		if walletTransactionFromRPC(tx, walletAddr) != nil {
			t.Fatal("expected nil when wallet not in account keys")
		}
	})
}

func TestSyncHistory(t *testing.T) {
	ctx := context.Background()
	const walletAddr = "walletAddr"
	const otherAddr = "otherAddr"

	t.Run("cursor stops at first fetch error", func(t *testing.T) {
		// sigs returned newest-first: [C, B, A]. B fetch fails.
		// Oldest-first processing: A ok, B error -> break.
		// Expected cursor = A; only A stored; C never reached.
		db := &mockTxDB{}
		table := newMockTable()
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				return []signatureInfo{makeSig("C"), makeSig("B"), makeSig("A")}, nil
			},
			txFunc: func(sig string) (*rpcTransaction, error) {
				if sig == "B" {
					return nil, fmt.Errorf("timeout")
				}
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.syncHistory(ctx); err != nil {
			t.Fatal(err)
		}
		cursor, _ := w.getLastScannedSignature()
		if cursor != "A" {
			t.Fatalf("cursor: want A, got %q", cursor)
		}
		if len(db.stored) != 1 {
			t.Fatalf("stored %d txs, want 1", len(db.stored))
		}
	})

	t.Run("cursor advances to newest when all succeed", func(t *testing.T) {
		db := &mockTxDB{}
		table := newMockTable()
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				return []signatureInfo{makeSig("C"), makeSig("B"), makeSig("A")}, nil
			},
			txFunc: func(_ string) (*rpcTransaction, error) {
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.syncHistory(ctx); err != nil {
			t.Fatal(err)
		}
		cursor, _ := w.getLastScannedSignature()
		if cursor != "C" {
			t.Fatalf("cursor: want C, got %q", cursor)
		}
		if len(db.stored) != 3 {
			t.Fatalf("stored %d txs, want 3", len(db.stored))
		}
	})

	t.Run("nil rpc response stops cursor like an error", func(t *testing.T) {
		// B returns (nil, nil) — no error but also no tx. Should stop at A.
		db := &mockTxDB{}
		table := newMockTable()
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				return []signatureInfo{makeSig("C"), makeSig("B"), makeSig("A")}, nil
			},
			txFunc: func(sig string) (*rpcTransaction, error) {
				if sig == "B" {
					return nil, nil
				}
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.syncHistory(ctx); err != nil {
			t.Fatal(err)
		}
		cursor, _ := w.getLastScannedSignature()
		if cursor != "A" {
			t.Fatalf("cursor: want A, got %q", cursor)
		}
	})

	t.Run("irrelevant tx still advances cursor", func(t *testing.T) {
		// B's tx doesn't include walletAddr so walletTransactionFromRPC returns
		// nil, but the sig was fetched successfully and should not stall cursor.
		db := &mockTxDB{}
		table := newMockTable()
		irrelevant := &rpcTransaction{
			Slot: 1,
			Meta: &rpcTransactionMeta{Fee: 1000, PreBalances: []uint64{2_000_000}, PostBalances: []uint64{1_495_000}},
		}
		irrelevant.Transaction.Signatures = []string{"sigB"}
		irrelevant.Transaction.Message.AccountKeys = []rpcAccountKey{{Pubkey: otherAddr}}
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				return []signatureInfo{makeSig("C"), makeSig("B"), makeSig("A")}, nil
			},
			txFunc: func(sig string) (*rpcTransaction, error) {
				if sig == "B" {
					return irrelevant, nil
				}
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.syncHistory(ctx); err != nil {
			t.Fatal(err)
		}
		cursor, _ := w.getLastScannedSignature()
		if cursor != "C" {
			t.Fatalf("cursor: want C, got %q", cursor)
		}
		if len(db.stored) != 2 {
			t.Fatalf("stored %d txs, want 2 (A and C, B irrelevant)", len(db.stored))
		}
	})

	t.Run("page cap sets beforeCursor to oldest sig", func(t *testing.T) {
		db := &mockTxDB{}
		table := newMockTable()
		// 1000 sigs, newest-first: sig1000 .. sig1.
		allSigs := make([]signatureInfo, 1000)
		for i := range allSigs {
			allSigs[i] = makeSig(fmt.Sprintf("sig%04d", 1000-i))
		}
		pageNum := 0
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				start := pageNum * 100
				pageNum++
				return allSigs[start : start+100], nil
			},
			txFunc: func(_ string) (*rpcTransaction, error) {
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.syncHistory(ctx); err != nil {
			t.Fatal(err)
		}
		bc, _ := w.getBeforeCursor()
		if bc != "sig0001" {
			t.Fatalf("beforeCursor: want sig0001 (oldest), got %q", bc)
		}
		cursor, _ := w.getLastScannedSignature()
		if cursor != "sig1000" {
			t.Fatalf("cursor: want sig1000 (newest), got %q", cursor)
		}
	})

	t.Run("no page cap leaves beforeCursor unset", func(t *testing.T) {
		db := &mockTxDB{}
		table := newMockTable()
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				return []signatureInfo{makeSig("C"), makeSig("B"), makeSig("A")}, nil
			},
			txFunc: func(_ string) (*rpcTransaction, error) {
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.syncHistory(ctx); err != nil {
			t.Fatal(err)
		}
		bc, _ := w.getBeforeCursor()
		if bc != "" {
			t.Fatalf("beforeCursor: want empty, got %q", bc)
		}
	})

	t.Run("second run retries sigs skipped by cursor error", func(t *testing.T) {
		// First run: [C, B, A], B fails -> cursor=A.
		// Second run: until=A returns [C, B]; both succeed -> cursor=C.
		db := &mockTxDB{}
		table := newMockTable()
		callCount := 0
		rpc := &mockRPC{
			sigsFunc: func(_, until *string, _ int) ([]signatureInfo, error) {
				callCount++
				if callCount == 1 {
					return []signatureInfo{makeSig("C"), makeSig("B"), makeSig("A")}, nil
				}
				if until == nil || *until != "A" {
					return nil, fmt.Errorf("unexpected until: %v", until)
				}
				return []signatureInfo{makeSig("C"), makeSig("B")}, nil
			},
			txFunc: func(sig string) (*rpcTransaction, error) {
				if sig == "B" && callCount == 1 {
					return nil, fmt.Errorf("timeout")
				}
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.syncHistory(ctx); err != nil {
			t.Fatal(err)
		}
		cursor, _ := w.getLastScannedSignature()
		if cursor != "A" {
			t.Fatalf("after first run: cursor want A, got %q", cursor)
		}

		if err := w.syncHistory(ctx); err != nil {
			t.Fatal(err)
		}
		cursor, _ = w.getLastScannedSignature()
		if cursor != "C" {
			t.Fatalf("after second run: cursor want C, got %q", cursor)
		}
	})
}

func TestBackfillHistory(t *testing.T) {
	ctx := context.Background()
	const walletAddr = "walletAddr"
	const otherAddr = "otherAddr"

	t.Run("no beforeCursor is a no-op", func(t *testing.T) {
		db := &mockTxDB{}
		table := newMockTable()
		called := false
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				called = true
				return nil, nil
			},
			txFunc: func(_ string) (*rpcTransaction, error) { return nil, nil },
		}
		w := newTestWallet(walletAddr, rpc, db, table)
		if err := w.backfillHistory(ctx); err != nil {
			t.Fatal(err)
		}
		if called {
			t.Fatal("getSignaturesForAddress called unexpectedly")
		}
	})

	t.Run("fetches and stores sigs older than beforeCursor", func(t *testing.T) {
		db := &mockTxDB{}
		table := newMockTable()
		_ = table.Set([]byte(beforeCursorKey), []byte("sig501"), true)

		rpc := &mockRPC{
			sigsFunc: func(before, until *string, _ int) ([]signatureInfo, error) {
				if before == nil || *before != "sig501" {
					return nil, fmt.Errorf("unexpected before: %v", before)
				}
				if until != nil {
					return nil, fmt.Errorf("unexpected until: %v", until)
				}
				return []signatureInfo{makeSig("sig500"), makeSig("sig499"), makeSig("sig1")}, nil
			},
			txFunc: func(_ string) (*rpcTransaction, error) {
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.backfillHistory(ctx); err != nil {
			t.Fatal(err)
		}
		if len(db.stored) != 3 {
			t.Fatalf("stored %d txs, want 3", len(db.stored))
		}
		bc, _ := w.getBeforeCursor()
		if bc != "" {
			t.Fatalf("beforeCursor should be cleared after completing backfill, got %q", bc)
		}
	})

	t.Run("page-capped backfill advances beforeCursor", func(t *testing.T) {
		db := &mockTxDB{}
		table := newMockTable()
		_ = table.Set([]byte(beforeCursorKey), []byte("sig501"), true)

		// 1000 sigs, newest-first in the old range: sig500 .. sig-499.
		allSigs := make([]signatureInfo, 1000)
		for i := range allSigs {
			allSigs[i] = makeSig(fmt.Sprintf("sig%04d", 500-i))
		}
		pageNum := 0
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				start := pageNum * 100
				pageNum++
				return allSigs[start : start+100], nil
			},
			txFunc: func(_ string) (*rpcTransaction, error) {
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.backfillHistory(ctx); err != nil {
			t.Fatal(err)
		}
		bc, _ := w.getBeforeCursor()
		// Should have advanced to the oldest sig in this backfill batch.
		if bc == "" || bc == "sig501" {
			t.Fatalf("beforeCursor should advance past sig501, got %q", bc)
		}
	})

	t.Run("fetch errors skip individual sigs without stopping", func(t *testing.T) {
		db := &mockTxDB{}
		table := newMockTable()
		_ = table.Set([]byte(beforeCursorKey), []byte("sig4"), true)

		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				return []signatureInfo{makeSig("sig3"), makeSig("sig2"), makeSig("sig1")}, nil
			},
			txFunc: func(sig string) (*rpcTransaction, error) {
				if sig == "sig2" {
					return nil, fmt.Errorf("timeout")
				}
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.backfillHistory(ctx); err != nil {
			t.Fatal(err)
		}
		// sig1 and sig3 stored; sig2 skipped but did not abort the loop.
		if len(db.stored) != 2 {
			t.Fatalf("stored %d txs, want 2 (sig2 error skipped)", len(db.stored))
		}
	})

	t.Run("StoreTxs failure leaves beforeCursor unchanged for retry", func(t *testing.T) {
		// pageCapped=true: beforeCursor must not advance when StoreTxs fails,
		// so the same batch is retried on the next call.
		db := &mockTxDB{storeTxsErr: fmt.Errorf("disk full")}
		table := newMockTable()
		_ = table.Set([]byte(beforeCursorKey), []byte("sig501"), true)

		allSigs := make([]signatureInfo, 1000)
		for i := range allSigs {
			allSigs[i] = makeSig(fmt.Sprintf("sig%04d", 500-i))
		}
		pageNum := 0
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				start := pageNum * 100
				pageNum++
				return allSigs[start : start+100], nil
			},
			txFunc: func(_ string) (*rpcTransaction, error) {
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.backfillHistory(ctx); err == nil {
			t.Fatal("expected StoreTxs error to propagate")
		}
		bc, _ := w.getBeforeCursor()
		if bc != "sig501" {
			t.Fatalf("beforeCursor should remain sig501 after store failure, got %q", bc)
		}
	})

	t.Run("StoreTxs failure on final page leaves beforeCursor for retry", func(t *testing.T) {
		// pageCapped=false (last page < 100): beforeCursor must not be cleared
		// when StoreTxs fails, so the tail is retried next run.
		db := &mockTxDB{storeTxsErr: fmt.Errorf("disk full")}
		table := newMockTable()
		_ = table.Set([]byte(beforeCursorKey), []byte("sig4"), true)

		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				return []signatureInfo{makeSig("sig3"), makeSig("sig2"), makeSig("sig1")}, nil
			},
			txFunc: func(_ string) (*rpcTransaction, error) {
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.backfillHistory(ctx); err == nil {
			t.Fatal("expected StoreTxs error to propagate")
		}
		bc, _ := w.getBeforeCursor()
		if bc != "sig4" {
			t.Fatalf("beforeCursor should remain sig4 after store failure, got %q", bc)
		}
	})

	t.Run("backfillUntil is passed as until and cleared on completion", func(t *testing.T) {
		db := &mockTxDB{}
		table := newMockTable()
		_ = table.Set([]byte(beforeCursorKey), []byte("sig501"), true)
		_ = table.Set([]byte(backfillUntilKey), []byte("sig100"), true)

		var gotUntil *string
		rpc := &mockRPC{
			sigsFunc: func(before, until *string, _ int) ([]signatureInfo, error) {
				gotUntil = until
				if before == nil || *before != "sig501" {
					return nil, fmt.Errorf("unexpected before: %v", before)
				}
				return []signatureInfo{makeSig("sig500"), makeSig("sig499")}, nil
			},
			txFunc: func(_ string) (*rpcTransaction, error) {
				return makeWalletTx(walletAddr, otherAddr), nil
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.backfillHistory(ctx); err != nil {
			t.Fatal(err)
		}
		if gotUntil == nil || *gotUntil != "sig100" {
			v := "<nil>"
			if gotUntil != nil {
				v = *gotUntil
			}
			t.Fatalf("until: want sig100, got %s", v)
		}
		// backfillUntil must be cleared once backfill completes.
		bu, _ := w.getBackfillUntil()
		if bu != "" {
			t.Fatalf("backfillUntil should be cleared after completion, got %q", bu)
		}
	})
}

func TestCheckPendingTxs(t *testing.T) {
	ctx := context.Background()

	makePendingTx := func(id string, lastValidHeight uint64) *txdb.WalletTransaction {
		wt := &txdb.WalletTransaction{ID: id}
		setTxData(wt, solTxData{LastValidBlockHeight: lastValidHeight})
		return wt
	}

	t.Run("transient getTransaction error leaves tx pending", func(t *testing.T) {
		tx := makePendingTx("txA", 100)
		db := &mockTxDB{pending: []*txdb.WalletTransaction{tx}}
		table := newMockTable()
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) { return nil, nil },
			txFunc: func(_ string) (*rpcTransaction, error) {
				return nil, fmt.Errorf("rpc timeout")
			},
			sigStatusesFunc: func(ids []string) ([]*signatureStatus, error) {
				// nil status: tx not yet seen by this node
				return []*signatureStatus{nil}, nil
			},
			blockHeightFunc: func() (uint64, error) {
				return 200, nil // past LastValidBlockHeight
			},
		}
		w := newTestWallet("addr", rpc, db, table)

		if err := w.checkPendingTxs(ctx); err != nil {
			t.Fatal(err)
		}
		// StoreTx must not have been called (tx stays pending).
		if len(db.stored) != 0 {
			t.Fatalf("tx should not have been stored on transient error, got %d stored", len(db.stored))
		}
	})

	t.Run("confirmed absent on-chain marks tx rejected", func(t *testing.T) {
		tx := makePendingTx("txB", 100)
		db := &mockTxDB{pending: []*txdb.WalletTransaction{tx}}
		table := newMockTable()
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) { return nil, nil },
			txFunc: func(_ string) (*rpcTransaction, error) {
				return nil, nil // nil, nil = confirmed absent
			},
			sigStatusesFunc: func(ids []string) ([]*signatureStatus, error) {
				return []*signatureStatus{nil}, nil
			},
			blockHeightFunc: func() (uint64, error) {
				return 200, nil
			},
		}
		w := newTestWallet("addr", rpc, db, table)

		if err := w.checkPendingTxs(ctx); err != nil {
			t.Fatal(err)
		}
		if len(db.stored) != 1 {
			t.Fatalf("want 1 stored (rejected), got %d", len(db.stored))
		}
		if !db.stored[0].Rejected {
			t.Fatal("tx should be marked Rejected")
		}
	})
}

// TestCheckPendingTxsIgnoresTokenPending guards against the native wallet's
// checkPendingTxs reconciling a token wallet's pending transactions: before
// scoping GetPendingTxs to the native asset, this loop would fetch a token
// tx's on-chain data and reparse it via walletTransactionFromRPC from the
// native wallet's own lamport-transfer perspective, storing wrong data over
// the token record and misattributing the confirmation note to SOL.
func TestCheckPendingTxsIgnoresTokenPending(t *testing.T) {
	ctx := context.Background()
	tokenID := usdcTokenID

	nativePend := &txdb.WalletTransaction{ID: "native-pend"}
	setTxData(nativePend, solTxData{LastValidBlockHeight: 100})
	tokenPend := &txdb.WalletTransaction{ID: "token-pend", TokenID: &tokenID}
	setTxData(tokenPend, solTxData{LastValidBlockHeight: 100})

	db := &mockTxDB{pending: []*txdb.WalletTransaction{nativePend, tokenPend}}
	table := newMockTable()
	var queriedIDs []string
	rpc := &mockRPC{
		sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) { return nil, nil },
		txFunc: func(_ string) (*rpcTransaction, error) {
			return nil, fmt.Errorf("rpc timeout") // transient; leaves tx pending either way
		},
		sigStatusesFunc: func(ids []string) ([]*signatureStatus, error) {
			queriedIDs = append(queriedIDs, ids...)
			statuses := make([]*signatureStatus, len(ids))
			return statuses, nil
		},
	}
	w := newTestWallet("addr", rpc, db, table)

	if err := w.checkPendingTxs(ctx); err != nil {
		t.Fatal(err)
	}
	if len(queriedIDs) != 1 || queriedIDs[0] != "native-pend" {
		t.Fatalf("getSignatureStatuses queried %v, want only [native-pend]", queriedIDs)
	}
}

// TestTokenAccountRentCached guards against re-fetching a cluster-wide
// constant on every send: getMinimumBalanceForRentExemption must only be
// called once across repeated tokenAccountRent calls on the same wallet.
func TestTokenAccountRentCached(t *testing.T) {
	calls := 0
	rpc := &mockRPC{
		rentFunc: func(size uint64) (uint64, error) {
			calls++
			if size != splTokenAccountSize {
				t.Fatalf("rent query size = %d, want %d", size, splTokenAccountSize)
			}
			return 2_039_280, nil
		},
	}
	w := newTestWallet("addr", rpc, nil, nil)

	for i := 0; i < 3; i++ {
		rent, err := w.tokenAccountRent(context.Background())
		if err != nil {
			t.Fatalf("tokenAccountRent: %v", err)
		}
		if rent != 2_039_280 {
			t.Fatalf("rent = %d, want 2039280", rent)
		}
	}
	if calls != 1 {
		t.Fatalf("getMinimumBalanceForRentExemption called %d times, want 1", calls)
	}
}

func TestKeyFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sol-keyfile.json")

	_, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pw := []byte("sol-keyfile-test-password-32byte")

	pubKey := privKey.Public().(ed25519.PublicKey)
	if err := keystore.Save(path, privKey.Seed(), pubKey, pw); err != nil {
		t.Fatalf("keystore.Save: %v", err)
	}

	gotPub, err := keystore.PubKey(path)
	if err != nil {
		t.Fatalf("keystore.PubKey: %v", err)
	}
	if !bytes.Equal(gotPub, pubKey) {
		t.Fatal("stored pubkey does not match original")
	}

	seed, err := keystore.Open(path, pw)
	if err != nil {
		t.Fatalf("keystore.Open: %v", err)
	}
	if !bytes.Equal(seed, privKey.Seed()) {
		t.Fatal("decrypted seed does not match original")
	}
}

func TestWalletTransactionFromRPCFallbackSelfSend(t *testing.T) {
	const walletAddr = "wallet"
	// No parsed instructions — forces the balance-delta fallback.
	tx := &rpcTransaction{
		Slot: 1,
		Meta: &rpcTransactionMeta{
			Fee:          5000,
			PreBalances:  []uint64{1_000_000},
			PostBalances: []uint64{995_000},
		},
	}
	tx.Transaction.Signatures = []string{"sig1"}
	tx.Transaction.Message.AccountKeys = []rpcAccountKey{{Pubkey: walletAddr}}

	wt := walletTransactionFromRPC(tx, walletAddr)
	if wt == nil {
		t.Fatal("expected non-nil WalletTransaction")
	}
	if wt.Type != txdb.SelfSend {
		t.Fatalf("type: want SelfSend, got %v", wt.Type)
	}
	if wt.Recipient == nil || *wt.Recipient != walletAddr {
		t.Fatalf("recipient: want %q, got %v", walletAddr, wt.Recipient)
	}
}

func TestShortVec(t *testing.T) {
	tests := []struct {
		v    int
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{255, []byte{0xff, 0x01}},
		{300, []byte{0xac, 0x02}},
	}
	for _, tc := range tests {
		got := shortVec(tc.v)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("shortVec(%d) = %x, want %x", tc.v, got, tc.want)
		}
	}
	if shortVec(-1) != nil {
		t.Error("shortVec(-1) should return nil")
	}
}

func TestSignTransferRoundTrip(t *testing.T) {
	// Generate a key pair to use as sender.
	_, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fromAddr := base58.Encode(privKey.Public().(ed25519.PublicKey))

	_, toPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	toAddr := base58.Encode(toPriv.Public().(ed25519.PublicKey))

	// A blockhash is just a base58-encoded 32-byte value on Solana.
	blockhash := base58.Encode(bytes.Repeat([]byte{0x42}, 32))

	msg, tx, sig := signTransfer(fromAddr, toAddr, blockhash, 1_000_000, privKey)

	if len(msg) == 0 {
		t.Fatal("empty message")
	}
	if len(tx) == 0 {
		t.Fatal("empty transaction")
	}
	if sig == "" {
		t.Fatal("empty signature")
	}

	// The signature must verify against the message using the sender's key.
	sigBytes := base58.Decode(sig)
	if !ed25519.Verify(privKey.Public().(ed25519.PublicKey), msg, sigBytes) {
		t.Fatal("signature does not verify against message")
	}

	// The transaction bytes must contain the message.
	if !bytes.Contains(tx, msg) {
		t.Fatal("transaction bytes do not contain the message")
	}
}

func TestIsNotFoundRPCError(t *testing.T) {
	if !isNotFoundRPCError(fmt.Errorf("Transaction not found (-32020)")) {
		t.Error("expected true for -32020 error")
	}
	if isNotFoundRPCError(fmt.Errorf("some other rpc error (-32001)")) {
		t.Error("expected false for non-32020 error")
	}
}
