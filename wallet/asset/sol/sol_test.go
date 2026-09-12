// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/keystore"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/decred/base58"
)

// --- mocks ---

type mockRPC struct {
	sigsFunc         func(before, until *string, limit int) ([]signatureInfo, error)
	txFunc           func(sig string) (*rpcTransaction, error)
	finalTxFunc      func(sig string) (*rpcTransaction, error)
	blockHeightFunc  func() (uint64, error)
	sigStatusesFunc  func(ids []string) ([]*signatureStatus, error)
	tokenBalanceFunc func(addr string) (uint64, bool, error)
	rentFunc         func(size uint64) (uint64, error)
	blockhashFunc    func() (string, uint64, error)
	sendTxFunc       func(txB []byte) (string, error)
	balanceFunc      func() (uint64, error)
	feeForMsgFunc    func(msg []byte) (uint64, error)
}

func (m *mockRPC) getSignaturesForAddress(_ context.Context, _ string, before, until *string, limit int) ([]signatureInfo, error) {
	return m.sigsFunc(before, until, limit)
}
func (m *mockRPC) getTransaction(_ context.Context, sig string) (*rpcTransaction, error) {
	return m.txFunc(sig)
}

// getTransactionAtCommitment defaults to the "confirmed" behaviour so tests
// that do not exercise the finalized probe need no extra wiring. Setting
// finalTxFunc distinguishes the two commitments.
func (m *mockRPC) getTransactionAtCommitment(_ context.Context, sig, _ string) (*rpcTransaction, error) {
	if m.finalTxFunc != nil {
		return m.finalTxFunc(sig)
	}
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
func (m *mockRPC) getFeeForMessage(_ context.Context, msg []byte) (uint64, error) {
	if m.feeForMsgFunc != nil {
		return m.feeForMsgFunc(msg)
	}
	return 0, nil
}
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
	want := uint32(BipID)
	if assetID != nil {
		want = *assetID
	}
	var out []*txdb.WalletTransaction
	for _, tx := range m.pending {
		if tx.AssetID == want {
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

func (t *mockTable) Get(k txdb.KV, _ ...txdb.GetOption) ([]byte, error) {
	kB, err := kvstore.ParseKV(k)
	if err != nil {
		return nil, err
	}
	v, ok := t.data[string(kB)]
	if !ok {
		return nil, txdb.ErrKeyNotFound
	}
	return v, nil
}
func (t *mockTable) Set(k, v txdb.KV, _ ...txdb.SetOption) error {
	kB, err := kvstore.ParseKV(k)
	if err != nil {
		return err
	}
	vB, err := kvstore.ParseKV(v)
	if err != nil {
		return err
	}
	t.data[string(kB)] = vB
	return nil
}
func (t *mockTable) Delete(k txdb.KV) error {
	kB, err := kvstore.ParseKV(k)
	if err != nil {
		return err
	}
	delete(t.data, string(kB))
	return nil
}
func (t *mockTable) SetBatch(_ []txdb.BatchEntry, _ ...txdb.SetOption) error { return nil }
func (t *mockTable) UseDefaultSetOptions(_ ...txdb.SetOption)                {}
func (t *mockTable) UseDefaultIterationOptions(_ ...txdb.IterationOption)    {}
func (t *mockTable) AddIndex(_ string, _ txdb.IndexFunc) (txdb.StoreIndex, error) {
	return nil, nil
}
func (t *mockTable) AddUniqueIndex(_ string, _ txdb.IndexFunc) (txdb.StoreIndex, error) {
	return nil, nil
}
func (t *mockTable) Iterate(_ txdb.KV, _ func(txdb.StoreIter) error, _ ...txdb.IterationOption) error {
	return nil
}

// newTestWallet returns a Wallet wired with mock rpc, txdb, and meta table.
func newTestWallet(addr string, rpc rpcI, db txdb.NetworkTxDB, table txdb.StoreTable) *Wallet {
	w := &Wallet{
		address: addr,
		txDB:    db,
		log:     util.Disabled,
	}
	w.conn.Store(&conn{rpc: rpc, ctx: context.Background(), table: table})
	return w
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

// TestSyncHistoryBoundsFetchesOnFailure verifies that when the oldest
// signature in a batch fails to resolve, fetchTransactions' bounded-
// concurrency dispatch does not still issue a getTransaction call for
// every signature in the batch (up to 1000) before syncHistory discards
// all but the (empty) successful prefix — it must stop once the failure
// is observed, wasting at most fetchWorkers-1 calls, not the whole batch.
func TestSyncHistoryBoundsFetchesOnFailure(t *testing.T) {
	ctx := context.Background()
	const walletAddr = "walletAddr"
	const n = 40

	// newest-first, matching getSignaturesForAddress's convention; oldest
	// (processed first) is sig0.
	sigs := make([]signatureInfo, n)
	for i := 0; i < n; i++ {
		sigs[i] = makeSig(fmt.Sprintf("sig%d", n-1-i))
	}

	var calls atomic.Int32
	db := &mockTxDB{}
	table := newMockTable()
	rpc := &mockRPC{
		sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
			return sigs, nil
		},
		txFunc: func(sig string) (*rpcTransaction, error) {
			calls.Add(1)
			if sig == "sig0" {
				return nil, fmt.Errorf("boom")
			}
			return makeWalletTx(walletAddr, "other"), nil
		},
	}
	w := newTestWallet(walletAddr, rpc, db, table)

	if err := w.syncHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if got := int(calls.Load()); got > fetchWorkers {
		t.Fatalf("getTransaction called %d times after an oldest-first failure, want at most %d (fetchWorkers)", got, fetchWorkers)
	}
	if len(db.stored) != 0 {
		t.Fatalf("stored %d txs, want 0 (oldest signature failed)", len(db.stored))
	}
}

// TestFetchTransactionsBoundsFetchesOnMidBatchFailure verifies the
// fetchWorkers-1 waste bound holds for a failure well past the start of
// the batch, not just at index 0.
func TestFetchTransactionsBoundsFetchesOnMidBatchFailure(t *testing.T) {
	const n = 400
	const failAt = 300

	sigs := make([]string, n)
	for i := range sigs {
		sigs[i] = fmt.Sprintf("sig%d", i)
	}

	var calls atomic.Int32
	rpc := &mockRPC{
		txFunc: func(sig string) (*rpcTransaction, error) {
			calls.Add(1)
			if sig == sigs[failAt] {
				return nil, fmt.Errorf("boom")
			}
			return makeWalletTx("addr", "other"), nil
		},
	}

	fetchTransactions(context.Background(), rpc, sigs, true)
	if got := int(calls.Load()); got > failAt+fetchWorkers {
		t.Fatalf("getTransaction called %d times after a mid-batch failure at index %d, want at most %d (failAt+fetchWorkers)", got, failAt, failAt+fetchWorkers)
	}
}

// TestFetchTransactionsStopsOnCancelledContext verifies fetchTransactions
// dispatches no further requests once ctx is already cancelled, rather than
// still issuing every request in the batch (each of which fails fast, but
// still costs a goroutine, an HTTP request object, and a slot in the
// WaitGroup Connect's caller waits on during shutdown).
func TestFetchTransactionsStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int32
	rpc := &mockRPC{
		txFunc: func(string) (*rpcTransaction, error) {
			calls.Add(1)
			return nil, nil
		},
	}
	sigs := make([]string, 100)
	for i := range sigs {
		sigs[i] = fmt.Sprintf("sig%d", i)
	}

	results := fetchTransactions(ctx, rpc, sigs, true)
	if len(results) != len(sigs) {
		t.Fatalf("results len = %d, want %d", len(results), len(sigs))
	}
	if got := int(calls.Load()); got != 0 {
		t.Fatalf("getTransaction called %d times against an already-cancelled context, want 0", got)
	}
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

	t.Run("429 arms the shared scan backoff", func(t *testing.T) {
		db := &mockTxDB{}
		table := newMockTable()
		_ = table.Set(metaKey(BipID, beforeCursorField), []byte("sig501"), txdb.WithReplace())
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				return nil, fmt.Errorf("getSignaturesForAddress: Too many requests for a specific RPC call (429)")
			},
		}
		w := newTestWallet(walletAddr, rpc, db, table)

		if err := w.backfillHistory(ctx); err == nil {
			t.Fatal("expected a 429 error")
		}
		if !w.scanBackoff.blocked(time.Now()) {
			t.Fatal("a backfill 429 should arm the shared scan backoff")
		}
	})

	t.Run("fetches and stores sigs older than beforeCursor", func(t *testing.T) {
		db := &mockTxDB{}
		table := newMockTable()
		_ = table.Set(metaKey(BipID, beforeCursorField), []byte("sig501"), txdb.WithReplace())

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
		_ = table.Set(metaKey(BipID, beforeCursorField), []byte("sig501"), txdb.WithReplace())

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
		_ = table.Set(metaKey(BipID, beforeCursorField), []byte("sig4"), txdb.WithReplace())

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
		_ = table.Set(metaKey(BipID, beforeCursorField), []byte("sig501"), txdb.WithReplace())

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
		_ = table.Set(metaKey(BipID, beforeCursorField), []byte("sig4"), txdb.WithReplace())

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
		_ = table.Set(metaKey(BipID, beforeCursorField), []byte("sig501"), txdb.WithReplace())
		_ = table.Set(metaKey(BipID, backfillUntilField), []byte("sig100"), txdb.WithReplace())

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
		wt := &txdb.WalletTransaction{ID: id, NetworkID: BipID, AssetID: BipID}
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

	// A load-balanced endpoint can keep returning nil status for a transaction
	// that landed. getSignatureStatuses is the only other path that confirms a
	// pending record, so if this branch just skips, the send shows as pending
	// forever: PendingTransactions keeps listing it, Core's pendingTxs map is
	// only cleared by a Confirmed note, and every poll re-issues the fetch.
	t.Run("landed tx with persistently nil status is confirmed", func(t *testing.T) {
		tx := makePendingTx("txC", 100)
		db := &mockTxDB{pending: []*txdb.WalletTransaction{tx}}
		table := newMockTable()
		landed := &rpcTransaction{Slot: 4242, Meta: &rpcTransactionMeta{}}
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) { return nil, nil },
			txFunc:   func(_ string) (*rpcTransaction, error) { return landed, nil },
			sigStatusesFunc: func(ids []string) ([]*signatureStatus, error) {
				return []*signatureStatus{nil}, nil
			},
			blockHeightFunc: func() (uint64, error) { return 200, nil },
		}
		w := newTestWallet("addr", rpc, db, table)

		if err := w.checkPendingTxs(ctx); err != nil {
			t.Fatal(err)
		}
		if len(db.stored) != 1 {
			t.Fatalf("want 1 stored (confirmed), got %d", len(db.stored))
		}
		if !db.stored[0].Confirmed {
			t.Fatal("landed tx should be marked Confirmed")
		}
		if db.stored[0].Rejected {
			t.Fatal("landed tx should not be marked Rejected")
		}
		if db.stored[0].BlockNumber != 4242 {
			t.Fatalf("BlockNumber = %d, want 4242", db.stored[0].BlockNumber)
		}
	})

	// Landed at "confirmed" but not yet rooted: the record must stay pending
	// rather than be called final, which is the distinction checkPendingTxs
	// owns and the reason the history scan does not confirm records itself.
	t.Run("landed but unrooted tx stays pending", func(t *testing.T) {
		tx := makePendingTx("txD", 100)
		db := &mockTxDB{pending: []*txdb.WalletTransaction{tx}}
		table := newMockTable()
		rpc := &mockRPC{
			sigsFunc:    func(_, _ *string, _ int) ([]signatureInfo, error) { return nil, nil },
			txFunc:      func(_ string) (*rpcTransaction, error) { return &rpcTransaction{Slot: 7}, nil },
			finalTxFunc: func(_ string) (*rpcTransaction, error) { return nil, nil },
			sigStatusesFunc: func(ids []string) ([]*signatureStatus, error) {
				return []*signatureStatus{nil}, nil
			},
			blockHeightFunc: func() (uint64, error) { return 200, nil },
		}
		w := newTestWallet("addr", rpc, db, table)

		if err := w.checkPendingTxs(ctx); err != nil {
			t.Fatal(err)
		}
		if len(db.stored) != 0 {
			t.Fatalf("unrooted tx should stay pending, got %d stored", len(db.stored))
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

	nativePend := &txdb.WalletTransaction{ID: "native-pend", NetworkID: BipID, AssetID: BipID}
	setTxData(nativePend, solTxData{LastValidBlockHeight: 100})
	tokenPend := &txdb.WalletTransaction{ID: "token-pend", NetworkID: BipID, AssetID: tokenID}
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

// TestReconnectRace verifies that Balance and getCachedRentExemptReserve
// (exported/internal methods reachable from a UI-poll goroutine) can run
// concurrently with a reconnect rewriting rpc, ctx, and metaTable without
// racing, panicking, or ever observing a nil rpc — the exact hazard
// Core.Resume's re-run of Connect on the same wallet creates (e.g. mobile
// background resume). The race detector (-race) is what actually proves
// the absence of a data race here — a plain-field version would not
// compile against this test's Store calls, so this cannot be run against
// the pre-fix code, but it guards the atomic accessors going forward and
// documents the exact concurrent-access shape Connect's reconnect path
// creates.
func TestReconnectRace(t *testing.T) {
	w := newTestWallet("addr", &mockRPC{}, nil, newMockTable())

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Simulates repeated Connect calls rewriting the connection state,
	// the same writes Connect itself performs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			var rpc rpcI = &mockRPC{}
			ctx, cancel := context.WithCancel(context.Background())
			w.conn.Store(&conn{rpc: rpc, ctx: ctx, table: newMockTable()})
			cancel()
		}
	}()

	// Simulates a UI poll hitting Balance and a metaTable-backed read
	// concurrently with the above.
	for i := 0; i < 500; i++ {
		if _, err := w.Balance(); err != nil && err != errNotConnected {
			t.Fatalf("unexpected Balance error: %v", err)
		}
		if _, err := w.getCachedRentExemptReserve(); err != nil {
			t.Fatalf("unexpected getCachedRentExemptReserve error: %v", err)
		}
	}

	close(stop)
	wg.Wait()
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

// TestWalletTransactionFromRPCIgnoresTokenTransfers covers the record
// corruption where the native poll loop claimed an SPL token transfer. The
// token send carries no system/transfer instruction, so it used to reach the
// balance-delta fallback and return a native record (AssetID = BipID) under the
// same signature the token wallet stores its own record under. Because
// txdb.storeTx is a full replace keyed by the signature and both poll loops run
// every 2s, the two records overwrote each other indefinitely and the token
// send vanished from GetAssetTxs(&tokenID, ...).
func TestWalletTransactionFromRPCIgnoresTokenTransfers(t *testing.T) {
	const (
		walletAddr = "wallet"
		srcATA     = "srcATA"
		dstATA     = "dstATA"
	)

	makeTx := func(pre, post uint64) *rpcTransaction {
		tx := &rpcTransaction{
			Slot: 1,
			Meta: &rpcTransactionMeta{
				Fee:          5000,
				PreBalances:  []uint64{pre},
				PostBalances: []uint64{post},
			},
		}
		tx.Transaction.Signatures = []string{"sig1"}
		tx.Transaction.Message.AccountKeys = []rpcAccountKey{{Pubkey: walletAddr}}
		tx.Transaction.Message.Instructions = []rpcInstruction{
			parsedSPLInstruction(srcATA, dstATA, 2_500_000),
		}
		return tx
	}

	// Fee-only lamport delta: previously misparsed as a native SelfSend.
	if wt := walletTransactionFromRPC(makeTx(1_000_000, 995_000), walletAddr); wt != nil {
		t.Fatalf("token transfer parsed as native %v record of %d lamports", wt.Type, wt.Amount)
	}

	// Larger lamport delta (rent for a newly created destination ATA):
	// previously misparsed as a native Send of the rent. A token send only ever
	// debits SOL, so the token record is the whole story here.
	if wt := walletTransactionFromRPC(makeTx(1_000_000, 900_000), walletAddr); wt != nil {
		t.Fatalf("token transfer parsed as native %v record of %d lamports", wt.Type, wt.Amount)
	}
}

// TestWalletTransactionFromRPCRecordsSOLCredits covers the native record being
// suppressed for lamports no token record can represent. Yielding on the
// spl-token program alone dropped these transactions from history entirely:
// walletTransactionFromSPLRPC only claims transfer/transferChecked, so nothing
// else recorded them, while Balance() still moved.
func TestWalletTransactionFromRPCRecordsSOLCredits(t *testing.T) {
	const walletAddr = "wallet"

	makeTx := func(inst rpcInstruction, pre, post uint64) *rpcTransaction {
		tx := &rpcTransaction{
			Slot: 1,
			Meta: &rpcTransactionMeta{
				Fee:          5000,
				PreBalances:  []uint64{pre},
				PostBalances: []uint64{post},
			},
		}
		tx.Transaction.Signatures = []string{"sig1"}
		tx.Transaction.Message.AccountKeys = []rpcAccountKey{{Pubkey: walletAddr}}
		tx.Transaction.Message.Instructions = []rpcInstruction{inst}
		return tx
	}

	closeAccount := rpcInstruction{
		Program: "spl-token",
		Parsed: &struct {
			Type string             `json:"type"`
			Info rpcInstructionInfo `json:"info"`
		}{Type: "closeAccount"},
	}

	// Closing an ATA refunds its rent. The token parser returns nil for a
	// closeAccount, so the native record is the only place this can land.
	wt := walletTransactionFromRPC(makeTx(closeAccount, 1_000_000, 3_039_280), walletAddr)
	if wt == nil {
		t.Fatal("ATA rent refund produced no record")
	}
	if wt.Type != txdb.Receive {
		t.Fatalf("rent refund recorded as %v, want Receive", wt.Type)
	}
	if wt.Amount != 2_039_280 {
		t.Fatalf("rent refund amount = %d, want 2039280", wt.Amount)
	}

	// A credit alongside a token transfer is still native movement: our own
	// token sends only debit SOL, so an increase is something else.
	wt = walletTransactionFromRPC(makeTx(parsedSPLInstruction("srcATA", "dstATA", 100), 1_000_000, 1_500_000), walletAddr)
	if wt == nil {
		t.Fatal("SOL credit alongside a token transfer produced no record")
	}
	if wt.Type != txdb.Receive || wt.Amount != 500_000 {
		t.Fatalf("got %v of %d lamports, want Receive of 500000", wt.Type, wt.Amount)
	}
}

// TestSyncHistoryPreservesPendingData covers the history scan clobbering a
// Send's expiry data. walletTransactionFromRPC reports Confirmed
// unconditionally off a "confirmed"-commitment fetch and carries no RawTx, and
// StoreTxs replaces wholesale — so the 2s sync loop used to wipe the solTxData
// checkPendingTxs needs for expiry detection and call a
// confirmed-but-not-finalized send final.
func TestSyncHistoryPreservesPendingData(t *testing.T) {
	const (
		walletAddr = "wallet"
		otherAddr  = "other"
	)
	ctx := context.Background()

	pendingSend := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        "sig",
		Amount:    500_000,
		NetworkID: BipID,
		AssetID:   BipID,
		Confirmed: false,
		Confirms:  &txdb.Confirms{Current: 0, Target: 1},
	}
	setTxData(pendingSend, solTxData{
		LastValidBlockHeight: 400,
		SubmissionTime:       1_700_000_000,
		Blockhash:            "hash",
	})
	rawTx := pendingSend.RawTx

	db := &mockTxDB{pending: []*txdb.WalletTransaction{pendingSend}}
	rpc := &mockRPC{
		sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
			return []signatureInfo{makeSig("sig")}, nil
		},
		txFunc: func(_ string) (*rpcTransaction, error) {
			return makeWalletTx(walletAddr, otherAddr), nil
		},
	}
	w := newTestWallet(walletAddr, rpc, db, newMockTable())

	if err := w.syncHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if len(db.stored) != 1 {
		t.Fatalf("stored %d txs, want 1", len(db.stored))
	}
	got := db.stored[0]
	if got.Confirmed {
		t.Error("sync marked a still-pending send confirmed; checkPendingTxs owns that transition")
	}
	if !bytes.Equal(got.RawTx, rawTx) {
		t.Errorf("RawTx: want the pending record's expiry data %q, got %q", rawTx, got.RawTx)
	}
	if got.Confirms == nil || got.Confirms.Target != 1 {
		t.Errorf("Confirms: want the pending record's, got %+v", got.Confirms)
	}
}

// TestSyncHistoryKeepsScannedRejection guards the other direction of the merge.
// The scan is what first learns a pending send failed — it parses Meta.Err from
// the transaction itself, while the pending record still says false because
// nothing has reconciled it yet. Carrying the record's stale flag over the
// fresh parse reported a failed send as fine until the next checkPendingTxs
// poll happened to correct it.
func TestSyncHistoryKeepsScannedRejection(t *testing.T) {
	const (
		walletAddr = "wallet"
		otherAddr  = "other"
	)
	ctx := context.Background()

	pendingSend := &txdb.WalletTransaction{Type: txdb.Send, ID: "sig", Amount: 500_000, NetworkID: BipID, AssetID: BipID}
	setTxData(pendingSend, solTxData{LastValidBlockHeight: 400})

	db := &mockTxDB{pending: []*txdb.WalletTransaction{pendingSend}}
	rpc := &mockRPC{
		sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
			return []signatureInfo{makeSig("sig")}, nil
		},
		txFunc: func(_ string) (*rpcTransaction, error) {
			tx := makeWalletTx(walletAddr, otherAddr)
			tx.Meta.Err = map[string]any{"InstructionError": []any{0, "Custom"}}
			return tx, nil
		},
	}
	w := newTestWallet(walletAddr, rpc, db, newMockTable())

	if err := w.syncHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if len(db.stored) != 1 {
		t.Fatalf("stored %d txs, want 1", len(db.stored))
	}
	if !db.stored[0].Rejected {
		t.Error("the scan's parsed rejection was overwritten by the pending record's stale flag")
	}
	if db.stored[0].Confirmed {
		t.Error("sync marked a still-pending send confirmed")
	}
}

// TestSyncHistoryConfirmsNewRecords is the counterpart: a signature with no
// existing record is history, not a live send, and must not be dragged into the
// pending set.
func TestSyncHistoryConfirmsNewRecords(t *testing.T) {
	const (
		walletAddr = "wallet"
		otherAddr  = "other"
	)
	db := &mockTxDB{}
	rpc := &mockRPC{
		sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
			return []signatureInfo{makeSig("sig")}, nil
		},
		txFunc: func(_ string) (*rpcTransaction, error) {
			return makeWalletTx(walletAddr, otherAddr), nil
		},
	}
	w := newTestWallet(walletAddr, rpc, db, newMockTable())

	if err := w.syncHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(db.stored) != 1 {
		t.Fatalf("stored %d txs, want 1", len(db.stored))
	}
	if !db.stored[0].Confirmed {
		t.Error("a newly scanned historical tx should be stored confirmed")
	}
}

// TestSyncHistoryEmitsBalanceChange guards against a detected incoming
// transfer showing up in the transaction list while the displayed balance
// never updates: the front end doesn't re-poll balances on its own, so
// storing a new tx must also push a fresh BalanceChangeNote.
func TestSyncHistoryEmitsBalanceChange(t *testing.T) {
	const (
		walletAddr = "wallet"
		otherAddr  = "other"
	)
	notes := make(chan asset.WalletNotification, 4)
	db := &mockTxDB{}
	rpc := &mockRPC{
		sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
			return []signatureInfo{makeSig("sig")}, nil
		},
		txFunc: func(_ string) (*rpcTransaction, error) {
			return makeWalletTx(walletAddr, otherAddr), nil
		},
		balanceFunc: func() (uint64, error) { return 1_000_000, nil },
	}
	w := newTestWallet(walletAddr, rpc, db, newMockTable())
	w.emit = asset.NewWalletEmitter(notes, BipID, util.Disabled)

	if err := w.syncHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-notes:
		bcn, ok := n.(*asset.BalanceChangeNote)
		if !ok {
			t.Fatalf("note type = %T, want *asset.BalanceChangeNote", n)
		}
		if bcn.Balance.Available != 1_000_000 {
			t.Errorf("emitted balance = %d, want 1000000", bcn.Balance.Available)
		}
	default:
		t.Fatal("expected a BalanceChangeNote after storing a new tx")
	}
}

// TestSyncHistoryNoNewTxsDoesNotEmitBalanceChange is the counterpart: a scan
// with nothing new to store should not push a balance note.
func TestSyncHistoryNoNewTxsDoesNotEmitBalanceChange(t *testing.T) {
	const walletAddr = "wallet"
	notes := make(chan asset.WalletNotification, 4)
	rpc := &mockRPC{
		sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) { return nil, nil },
	}
	w := newTestWallet(walletAddr, rpc, &mockTxDB{}, newMockTable())
	w.emit = asset.NewWalletEmitter(notes, BipID, util.Disabled)

	if err := w.syncHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-notes:
		t.Fatalf("unexpected note %T when nothing new was stored", n)
	default:
	}
}

// TestScanBackoff exercises the cooldown in isolation: escalation on repeated
// rate limits, capping, and reset on success.
func TestScanBackoff(t *testing.T) {
	now := time.Now()
	var b scanBackoff

	if b.blocked(now) {
		t.Fatal("fresh backoff should not be blocked")
	}

	b.rateLimited(now)
	if !b.blocked(now) {
		t.Fatal("should be blocked immediately after a rate limit")
	}
	if !b.blocked(now.Add(scanBackoffMin - time.Second)) {
		t.Fatal("should still be blocked just before the min delay elapses")
	}
	if b.blocked(now.Add(scanBackoffMin + time.Second)) {
		t.Fatal("should no longer be blocked once the min delay elapses")
	}

	// A second consecutive rate limit doubles the delay.
	b.rateLimited(now)
	if b.blocked(now.Add(2*scanBackoffMin + time.Second)) {
		t.Fatal("second rate limit should double the delay to 2x min")
	}
	if !b.blocked(now.Add(2*scanBackoffMin - time.Second)) {
		t.Fatal("should still be blocked just before the doubled delay elapses")
	}

	// Escalation caps at scanBackoffMax regardless of further rate limits.
	for i := 0; i < 10; i++ {
		b.rateLimited(now)
	}
	if b.delay != scanBackoffMax {
		t.Fatalf("delay should cap at %v, got %v", scanBackoffMax, b.delay)
	}

	// Success clears the cooldown entirely, so the next rate limit restarts
	// at scanBackoffMin rather than continuing to escalate.
	b.succeeded()
	if b.blocked(now) {
		t.Fatal("succeeded should clear an active cooldown")
	}
	b.rateLimited(now)
	if b.blocked(now.Add(scanBackoffMin + time.Second)) {
		t.Fatal("rate limit after succeeded should restart at scanBackoffMin, not the prior escalated delay")
	}
}

// TestSyncHistoryRateLimitBackoff proves the 429 cooldown suppresses request
// cadence (not just returns an error) while leaving on-demand balance/tip
// calls ungated, then recovers once the scan-class call succeeds again.
func TestSyncHistoryRateLimitBackoff(t *testing.T) {
	const walletAddr = "walletAddr"
	callCount := 0
	rpc := &mockRPC{
		sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
			callCount++
			return nil, fmt.Errorf("getSignaturesForAddress: Too many requests for a specific RPC call (429)")
		},
		balanceFunc: func() (uint64, error) { return 1_000_000, nil },
	}
	w := newTestWallet(walletAddr, rpc, &mockTxDB{}, newMockTable())
	ctx := context.Background()

	if err := w.syncHistory(ctx); err == nil {
		t.Fatal("expected a 429 error on the first scan")
	}
	if callCount != 1 {
		t.Fatalf("callCount = %d, want 1", callCount)
	}

	// Blocked: the second call must not reach the RPC at all.
	if err := w.syncHistory(ctx); err != nil {
		t.Fatalf("blocked syncHistory should return nil, got %v", err)
	}
	if callCount != 1 {
		t.Fatalf("callCount after blocked scan = %d, want still 1", callCount)
	}

	// Balance and tip stay ungated by the scan backoff.
	if _, err := rpc.getBalance(ctx, w.address); err != nil {
		t.Fatalf("getBalance should be ungated by scan backoff: %v", err)
	}
	if err := w.updateTip(ctx); err != nil {
		t.Fatalf("updateTip should be ungated by scan backoff: %v", err)
	}

	// Recovery: once the backoff clears, the scan reaches the RPC again.
	w.scanBackoff.succeeded()
	rpc.sigsFunc = func(_, _ *string, _ int) ([]signatureInfo, error) {
		callCount++
		return nil, nil
	}
	if err := w.syncHistory(ctx); err != nil {
		t.Fatalf("syncHistory after recovery: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("callCount after recovery = %d, want 2", callCount)
	}
}

func TestEstimateSendTxFee(t *testing.T) {
	walletAddr := base58.Encode(bytes.Repeat([]byte{0x11}, 32))
	destAddr := base58.Encode(bytes.Repeat([]byte{0x22}, 32))

	t.Run("uses live fee when available", func(t *testing.T) {
		rpc := &mockRPC{
			balanceFunc:   func() (uint64, error) { return 1_000_000, nil },
			feeForMsgFunc: func(_ []byte) (uint64, error) { return 7_000, nil },
		}
		w := newTestWallet(walletAddr, rpc, nil, nil)
		fee, valid, err := w.EstimateSendTxFee(destAddr, 1_000, 0, false, false)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if !valid {
			t.Error("expected isValidAddress=true")
		}
		if fee != 7_000 {
			t.Errorf("fee = %d, want 7000 (live fee)", fee)
		}
	})

	t.Run("falls back to default fee when live fee unavailable", func(t *testing.T) {
		rpc := &mockRPC{
			balanceFunc:   func() (uint64, error) { return 1_000_000, nil },
			feeForMsgFunc: func(_ []byte) (uint64, error) { return 0, fmt.Errorf("rpc down") },
		}
		w := newTestWallet(walletAddr, rpc, nil, nil)
		fee, _, err := w.EstimateSendTxFee(destAddr, 1_000, 0, false, false)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if fee != DefaultFee {
			t.Errorf("fee = %d, want DefaultFee (%d)", fee, DefaultFee)
		}
	})

	t.Run("invalid address reported via isValidAddress, not an error", func(t *testing.T) {
		rpc := &mockRPC{balanceFunc: func() (uint64, error) { return 1_000_000, nil }}
		w := newTestWallet(walletAddr, rpc, nil, nil)
		fee, valid, err := w.EstimateSendTxFee("not-a-sol-address", 1_000, 0, false, false)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if valid {
			t.Error("expected isValidAddress=false")
		}
		if fee == 0 {
			t.Error("expected a non-zero fee even for an invalid address")
		}
	})

	t.Run("insufficient balance for value+fee", func(t *testing.T) {
		rpc := &mockRPC{balanceFunc: func() (uint64, error) { return 1_000, nil }}
		w := newTestWallet(walletAddr, rpc, nil, nil)
		_, _, err := w.EstimateSendTxFee(destAddr, 1_000_000, 0, false, false)
		if err != asset.ErrInsufficientBalance {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
	})

	t.Run("insufficient balance to keep rent-exempt reserve", func(t *testing.T) {
		rpc := &mockRPC{
			balanceFunc: func() (uint64, error) { return 10_000, nil },
			rentFunc:    func(_ uint64) (uint64, error) { return 5_000, nil },
		}
		w := newTestWallet(walletAddr, rpc, nil, nil)
		// value+fee leaves less than the 5000 reserve behind.
		_, _, err := w.EstimateSendTxFee(destAddr, 4_000, 0, false, false)
		if err != asset.ErrInsufficientBalance {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
	})

	t.Run("maxWithdraw with empty address estimates the Max-button fee", func(t *testing.T) {
		rpc := &mockRPC{balanceFunc: func() (uint64, error) { return 1_000_000, nil }}
		w := newTestWallet(walletAddr, rpc, nil, nil)
		fee, valid, err := w.EstimateSendTxFee("", 1_000_000, 0, false, true)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if valid {
			t.Error("expected isValidAddress=false for an empty address")
		}
		if fee == 0 {
			t.Error("expected a non-zero fee")
		}
	})

	t.Run("errors when not connected", func(t *testing.T) {
		w := newTestWallet(walletAddr, nil, nil, nil)
		if _, _, err := w.EstimateSendTxFee(destAddr, 1_000, 0, false, false); err == nil {
			t.Fatal("expected an error when rpc is nil")
		}
	})
}

// TestCallDetectsHTTPStatus429 guards against a provider (or a proxy/CDN in
// front of one) that returns HTTP 429 with a plain-text or HTML body rather
// than a well-formed JSON-RPC error object. Before checking resp.StatusCode,
// that body would fail to unmarshal and surface only as an opaque decode
// error — never recognized as rate-limited, so the backoff this is meant to
// arm would never trigger.
func TestCallDetectsHTTPStatus429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("<html><body>Too Many Requests</body></html>"))
	}))
	defer srv.Close()

	rpc := newRPCClient(srv.URL)
	err := rpc.call(context.Background(), "getSlot", nil, nil)
	if err == nil {
		t.Fatal("expected an error from the 429 response")
	}
	if !isRateLimitedRPCError(err) {
		t.Errorf("isRateLimitedRPCError(%v) = false, want true", err)
	}
}

func TestCallResponseSizeLimit(t *testing.T) {
	const prefix = `{"jsonrpc":"2.0","result":"`
	const suffix = `"}`
	for _, test := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "at limit", size: rpcResponseSizeLimit},
		{name: "over limit", size: rpcResponseSizeLimit + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := prefix + strings.Repeat("x", test.size-len(prefix)-len(suffix)) + suffix
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(body))
			}))
			defer srv.Close()

			var result string
			err := newRPCClient(srv.URL).call(context.Background(), "getSlot", nil, &result)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "exceeds") {
					t.Fatalf("call error = %v, want response-size error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if len(result) != test.size-len(prefix)-len(suffix) {
				t.Fatalf("result length = %d, want %d", len(result), test.size-len(prefix)-len(suffix))
			}
		})
	}
}
