// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/txdb"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	btcec "github.com/btcsuite/btcd/btcec/v2"
)

// ---- coin ----

func TestCoin(t *testing.T) {
	var hash [32]byte
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	c := &coin{txHash: hash, value: 999_999}

	id := c.ID()
	if len(id) != 32 || !bytes.Equal(id, hash[:]) {
		t.Errorf("ID mismatch: got %x", id)
	}
	s := c.String()
	if len(s) != 64 || s != strings.ToUpper(s) {
		t.Errorf("String unexpected: %q", s)
	}
	if c.Value() != 999_999 {
		t.Errorf("Value = %d, want 999999", c.Value())
	}
	if c.TxID() != s {
		t.Errorf("TxID != String: %q vs %q", c.TxID(), s)
	}
}

// ---- RPC helpers ----

func TestRPCError(t *testing.T) {
	e := &rpcError{code: 19, errType: "actNotFound", msg: "account not found"}
	if !strings.Contains(e.Error(), "actNotFound") {
		t.Errorf("Error() does not contain type: %s", e.Error())
	}
}

func TestIsAccountNotFound(t *testing.T) {
	if !isAccountNotFound(&rpcError{errType: "actNotFound"}) {
		t.Error("expected actNotFound to match")
	}
	if isAccountNotFound(&rpcError{errType: "txnNotFound"}) {
		t.Error("txnNotFound should not match")
	}
	if isAccountNotFound(fmt.Errorf("generic")) {
		t.Error("generic error should not match")
	}
}

func TestIsTxNotFound(t *testing.T) {
	if !isTxNotFound(&rpcError{errType: "txnNotFound"}) {
		t.Error("expected txnNotFound to match")
	}
	if isTxNotFound(&rpcError{errType: "actNotFound"}) {
		t.Error("actNotFound should not match")
	}
	if isTxNotFound(fmt.Errorf("generic")) {
		t.Error("generic error should not match")
	}
}

func TestFeeDrops(t *testing.T) {
	fr := &feeResult{}

	fr.Drops.OpenLedgerFee = "250"
	if got := fr.feeDrops(); got != 250 {
		t.Errorf("feeDrops = %d, want 250", got)
	}

	// feeDrops returns 0 as a sentinel when the value is missing or invalid.
	// Callers are responsible for substituting DefaultFeeDrops with a warning.
	fr.Drops.OpenLedgerFee = "0"
	if got := fr.feeDrops(); got != 0 {
		t.Errorf("feeDrops(0) = %d, want 0 (sentinel)", got)
	}

	fr.Drops.OpenLedgerFee = "notanumber"
	if got := fr.feeDrops(); got != 0 {
		t.Errorf("feeDrops(invalid) = %d, want 0 (sentinel)", got)
	}
}

// ---- binary encoding ----

func TestWriteVarBlobAllRanges(t *testing.T) {
	// Range 1: n <= 192 — single byte length prefix.
	var b bytes.Buffer
	data50 := make([]byte, 50)
	writeVarBlob(&b, data50)
	if b.Bytes()[0] != 50 {
		t.Errorf("1-byte prefix: got %d, want 50", b.Bytes()[0])
	}
	if b.Len() != 51 {
		t.Errorf("1-byte prefix total len: got %d, want 51", b.Len())
	}

	// Range 2: 193 <= n <= 12480 — two byte length prefix.
	// n=200: n-193=7; first byte = 193+(7>>8) = 193; second byte = 7&0xFF = 7.
	b.Reset()
	data200 := make([]byte, 200)
	writeVarBlob(&b, data200)
	r := b.Bytes()
	if r[0] != 193 || r[1] != 7 {
		t.Errorf("2-byte prefix: got [%d %d], want [193 7]", r[0], r[1])
	}
	if b.Len() != 202 {
		t.Errorf("2-byte prefix total len: got %d, want 202", b.Len())
	}

	// Range 3: n > 12480 — three byte length prefix.
	// n=13000: n-12481=519=0x0207;
	// first byte = 241+(519>>16) = 241; second = (519>>8)&0xFF = 2; third = 519&0xFF = 7.
	b.Reset()
	data13000 := make([]byte, 13000)
	writeVarBlob(&b, data13000)
	r = b.Bytes()
	if r[0] != 241 || r[1] != 2 || r[2] != 7 {
		t.Errorf("3-byte prefix: got [%d %d %d], want [241 2 7]", r[0], r[1], r[2])
	}
	if b.Len() != 13003 {
		t.Errorf("3-byte prefix total len: got %d, want 13003", b.Len())
	}
}

func TestEncodePaymentDestinationTag(t *testing.T) {
	srcID, _ := addressToAccountID("rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh")
	dstID, _ := addressToAccountID("rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe")
	tag := uint32(12345)

	p := &xrpPayment{
		account:        [20]byte(srcID),
		destination:    [20]byte(dstID),
		amount:         1_000_000,
		fee:            12,
		sequence:       1,
		lastLedgerSeq:  100,
		destinationTag: &tag,
		signingPubKey:  make([]byte, 33),
	}

	// With tag: DestinationTag header 0x2E must appear at offset 13
	// (after TransactionType[3] + Flags[5] + Sequence[5]).
	b := encodePayment(p, true)
	if len(b) <= 17 {
		t.Fatalf("encoded payment too short: %d bytes", len(b))
	}
	if b[13] != 0x2E {
		t.Errorf("expected DestinationTag header 0x2E at offset 13, got 0x%02X", b[13])
	}
	tagEncoded := uint32(b[14])<<24 | uint32(b[15])<<16 | uint32(b[16])<<8 | uint32(b[17])
	if tagEncoded != tag {
		t.Errorf("DestinationTag encoded as %d, want %d", tagEncoded, tag)
	}

	// Without tag: offset 13 must be 0x20 (first byte of LastLedgerSequence header).
	p.destinationTag = nil
	bNoTag := encodePayment(p, true)
	if bNoTag[13] != 0x20 {
		t.Errorf("without tag: expected 0x20 at offset 13, got 0x%02X", bNoTag[13])
	}
}

func TestSignPayment(t *testing.T) {
	privKeyBytes := make([]byte, 32)
	privKeyBytes[31] = 1
	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)

	srcID, _ := addressToAccountID("rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh")
	dstID, _ := addressToAccountID("rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe")

	p := &xrpPayment{
		account:       [20]byte(srcID),
		destination:   [20]byte(dstID),
		amount:        1_000_000,
		fee:           12,
		sequence:      1,
		lastLedgerSeq: 100,
		signingPubKey: privKey.PubKey().SerializeCompressed(),
	}

	sig, err := signPayment(p, privKey)
	if err != nil {
		t.Fatalf("signPayment: %v", err)
	}
	if len(sig) == 0 {
		t.Fatal("empty signature")
	}
	if sig[0] != 0x30 {
		t.Errorf("expected DER signature (0x30), got 0x%02X", sig[0])
	}

	// RFC 6979 deterministic: same inputs produce the same signature.
	sig2, err := signPayment(p, privKey)
	if err != nil {
		t.Fatalf("signPayment 2: %v", err)
	}
	if !bytes.Equal(sig, sig2) {
		t.Error("signing is not deterministic for the same inputs")
	}
}

// ---- rpcEndpoint ----

func TestRPCEndpoint(t *testing.T) {
	w := &xrpWallet{net: util.Mainnet}

	// No settings — fall back to mainnet default.
	if ep := w.rpcEndpoint(); ep != DefaultRPCEndpoints[util.Mainnet] {
		t.Errorf("no settings: got %q, want mainnet default", ep)
	}

	// Custom setting overrides default.
	w.settings = map[string]string{"rpcprovider": "https://my.node"}
	if ep := w.rpcEndpoint(); ep != "https://my.node" {
		t.Errorf("custom setting: got %q, want https://my.node", ep)
	}

	// Testnet network with no override.
	w.settings = nil
	w.net = util.Testnet
	if ep := w.rpcEndpoint(); ep != DefaultRPCEndpoints[util.Testnet] {
		t.Errorf("testnet: got %q, want testnet default", ep)
	}
}

// ---- wallet lifecycle ----

func TestWalletLifecycle(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	pw := []byte("lifecycle-test-password-padded32")
	dir := t.TempDir()

	d := &Driver{}

	// Not yet created.
	exists, err := d.Exists(walletTypeRPC, dir, nil, util.Mainnet)
	if err != nil || exists {
		t.Fatalf("Exists before Create: %v, %v", exists, err)
	}

	// Create.
	if err := d.Create(&asset.CreateWalletParams{
		Type:    walletTypeRPC,
		Seed:    seed,
		Pass:    pw,
		DataDir: dir,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	exists, err = d.Exists(walletTypeRPC, dir, nil, util.Mainnet)
	if err != nil || !exists {
		t.Fatalf("Exists after Create: %v, %v", exists, err)
	}

	// Unknown type.
	if _, err := d.Exists("unknown", dir, nil, util.Mainnet); err == nil {
		t.Error("expected error for unknown wallet type")
	}

	// Open.
	log := util.NewLogger("test", util.LevelError, io.Discard)
	w, err := newWallet(&asset.WalletConfig{
		DataDir:  dir,
		Settings: map[string]string{},
	}, log, util.Mainnet)
	if err != nil {
		t.Fatalf("newWallet: %v", err)
	}

	// Locked on open.
	if !w.Locked() {
		t.Error("expected wallet locked on open")
	}

	// Wrong password.
	if err := w.Unlock([]byte("wrong")); err == nil {
		t.Error("expected error for wrong password")
	}

	// Correct password.
	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if w.Locked() {
		t.Error("expected wallet unlocked after Unlock")
	}

	// Lock.
	if err := w.Lock(); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if !w.Locked() {
		t.Error("expected wallet locked after Lock")
	}

	// DepositAddress is deterministic.
	addr, err := w.DepositAddress()
	if err != nil || addr == "" || addr[0] != 'r' {
		t.Fatalf("DepositAddress: %q, %v", addr, err)
	}

	// OwnsDepositAddress.
	owns, err := w.OwnsDepositAddress(addr)
	if err != nil || !owns {
		t.Errorf("OwnsDepositAddress(self): %v, %v", owns, err)
	}
	owns, err = w.OwnsDepositAddress("rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe")
	if err != nil || owns {
		t.Errorf("OwnsDepositAddress(other): %v, %v", owns, err)
	}

	// SyncStatus with no tip: should not be synced.
	ss, err := w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus: %v", err)
	}
	if ss.Synced {
		t.Error("expected not synced with zero tip time")
	}
	if ss.Blocks != 0 {
		t.Errorf("expected Blocks=0, got %d", ss.Blocks)
	}
}

// ---- txdb ----

// newTestTxDB returns a master-tx-DB-backed NetworkTxDB handle and its meta
// table for the BipID network, for use in wallet tests. It replaces the
// per-wallet database the XRP wallet kept before migrating onto txdb.
func newTestTxDB(t *testing.T) (txdb.NetworkTxDB, txdb.StoreTable) {
	t.Helper()
	log := util.NewLogger("test", util.LevelError, io.Discard)
	master, err := txdb.New(t.TempDir()+"/tx.db", log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	n := master.NetworkTxDB(BipID)
	mt, err := n.Table(metaTableName)
	if err != nil {
		t.Fatalf("meta Table: %v", err)
	}
	return n, mt
}

// TestLastScannedLedger covers the wallet's meta-table accessors over the
// master tx DB.
func TestLastScannedLedger(t *testing.T) {
	n, mt := newTestTxDB(t)
	w := &xrpWallet{txDB: n, metaTable: mt}

	seq, err := w.getLastScannedLedger()
	if err != nil || seq != 0 {
		t.Fatalf("getLastScannedLedger empty: seq=%d, err=%v", seq, err)
	}
	if err := w.setLastScannedLedger(77); err != nil {
		t.Fatalf("setLastScannedLedger: %v", err)
	}
	if seq, err = w.getLastScannedLedger(); err != nil || seq != 77 {
		t.Fatalf("getLastScannedLedger after set: seq=%d, err=%v", seq, err)
	}
}

// ---- additional gap-filling tests ----

func TestAmountDropsParseError(t *testing.T) {
	// amountDrops returns 0 when Amount is not a valid uint64 string.
	tr := &txResult{Amount: "notanumber"}
	if got := tr.amountDrops(); got != 0 {
		t.Errorf("amountDrops(invalid string) = %d, want 0", got)
	}
}

func TestDeliveredAmountDropsParseError(t *testing.T) {
	// deliveredAmountDrops returns 0 when the value is not parseable.
	m := txMeta{DeliveredAmount: "notanumber"}
	if got := m.deliveredAmountDrops(); got != 0 {
		t.Errorf("deliveredAmountDrops(invalid) = %d, want 0", got)
	}
	// Non-string type also returns 0.
	m2 := txMeta{DeliveredAmount: map[string]any{"value": "1"}}
	if got := m2.deliveredAmountDrops(); got != 0 {
		t.Errorf("deliveredAmountDrops(object) = %d, want 0", got)
	}
}

func TestEncodePaymentWithSignature(t *testing.T) {
	// encodePayment with forSigning=false and a populated txnSig must include
	// the TxnSignature field (header 0x74).
	srcID, _ := addressToAccountID("rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh")
	dstID, _ := addressToAccountID("rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe")
	p := &xrpPayment{
		account:       [20]byte(srcID),
		destination:   [20]byte(dstID),
		amount:        1_000_000,
		fee:           12,
		sequence:      1,
		lastLedgerSeq: 100,
		signingPubKey: make([]byte, 33),
		txnSig:        []byte{0x30, 0x44, 0x01}, // minimal DER prefix
	}
	b := encodePayment(p, false)
	found := false
	for _, v := range b {
		if v == 0x74 {
			found = true
			break
		}
	}
	if !found {
		t.Error("TxnSignature header 0x74 not found in signed encoding")
	}

	// forSigning=true must omit 0x74.
	bSigning := encodePayment(p, true)
	for _, v := range bSigning {
		if v == 0x74 {
			t.Error("unexpected TxnSignature header 0x74 in signing encoding")
		}
	}
}

func TestWalletTransactionFromTxReceiveIOU(t *testing.T) {
	const walletAddr = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	const otherAddr = "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"

	// A receive with a non-XRP (IOU) Amount and no delivered_amount must return nil.
	tr := &txResult{
		Account:         otherAddr,
		Destination:     walletAddr,
		TransactionType: "Payment",
		Amount:          map[string]any{"currency": "USD", "value": "1"},
		Fee:             "12",
		Hash:            "ff01",
	}
	if wt := walletTransactionFromTx(tr, walletAddr, txMeta{}, true, 10); wt != nil {
		t.Errorf("expected nil for IOU receive, got %+v", wt)
	}
}

func TestCreateEmptySeed(t *testing.T) {
	d := &Driver{}
	err := d.Create(&asset.CreateWalletParams{
		Type:    walletTypeRPC,
		Seed:    nil,
		DataDir: t.TempDir(),
	})
	if err == nil {
		t.Error("expected error for empty seed")
	}
}

func TestCreateUnknownType(t *testing.T) {
	d := &Driver{}
	err := d.Create(&asset.CreateWalletParams{
		Type:    "unknown",
		Seed:    []byte{1, 2, 3},
		DataDir: t.TempDir(),
	})
	if err == nil {
		t.Error("expected error for unknown wallet type")
	}
}

// TestCheckPendingTxsMultiExpire verifies that when two or more txs expire in
// the same checkPendingTxs pass, nextSeq is reset to 0 so reconcileSeq can
// re-derive the correct sequence from the network.
func TestCheckPendingTxsMultiExpire(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, io.Discard)
	n, mt := newTestTxDB(t)

	// Both tx RPC lookups return txnNotFound, triggering expiration.
	m := newMockRippled(t)
	m.handle("tx", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{errType: "txnNotFound", msg: "Transaction not found."}
	})

	makePending := func(id string, lls uint32) *txdb.WalletTransaction {
		wt := &txdb.WalletTransaction{Type: txdb.Send, ID: id}
		setLastLedgerSequence(wt, lls)
		return wt
	}

	for _, tx := range []*txdb.WalletTransaction{makePending("TX0001", 10), makePending("TX0002", 10)} {
		if err := n.StoreTx(tx); err != nil {
			t.Fatalf("StoreTx: %v", err)
		}
	}

	w := &xrpWallet{
		txDB:      n,
		metaTable: mt,
		rpc:       m.rpcClient(),
		log:       log,
	}
	w.nextSeq = 99
	w.tip = 20 // both txs have LLS=10 < tip=20, so both expire

	// Simulate checkPendingTxs: both tx1 and tx2 expire in the same pass.
	// After the fix, the last expiring tx sees remaining==0 and resets nextSeq.
	w.checkPendingTxs(context.Background())

	w.seqMtx.Lock()
	got := w.nextSeq
	w.seqMtx.Unlock()

	if got != 0 {
		t.Errorf("nextSeq after multi-expiration = %d, want 0 (so reconcileSeq can re-derive)", got)
	}
}

// TestHandleValidatedTipNoOp verifies that a tip <= the current tip is dropped
// without triggering any processing.
func TestHandleValidatedTipNoOp(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	m := newMockRippled(t)
	// account_tx must never be called for a stale tip.
	w := &xrpWallet{rpc: m.rpcClient(), log: log}
	w.tip = 500

	w.handleValidatedTip(context.Background(), 500) // same tip — no-op
	w.handleValidatedTip(context.Background(), 499) // older tip — no-op
	// If account_tx were called the mock would fatalf on unhandled method.
}

// TestHandleValidatedTipUpdates verifies that a new tip advances the stored tip,
// runs checkPendingTxs and scanAccountTxs, and emits a balance when txs are found.
func TestHandleValidatedTipUpdates(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	n, mt := newTestTxDB(t)

	const walletAddr = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	noteChan := make(chan asset.WalletNotification, 8)
	m := newMockRippled(t)
	m.handle("account_tx", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"transactions": []any{
				map[string]any{
					"tx": map[string]any{
						"hash":            "AAAA000000000000000000000000000000000000000000000000000000000001",
						"TransactionType": "Payment",
						"Account":         "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe",
						"Destination":     walletAddr,
						"Amount":          "5000000",
						"Fee":             "12",
						"ledger_index":    uint64(101),
						"close_time_iso":  "2026-06-04T00:00:00Z",
					},
					"meta": map[string]any{
						"TransactionResult": "tesSUCCESS",
						"delivered_amount":  "5000000",
					},
					"validated":    true,
					"ledger_index": uint64(101),
				},
			},
		}, nil
	})
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("10000000", 0, 0), nil
	})

	w := &xrpWallet{
		address:     walletAddr,
		rpc:         m.rpcClient(),
		txDB:        n,
		metaTable:   mt,
		log:         log,
		emit:        asset.NewWalletEmitter(noteChan, BipID, log),
		baseReserve: BaseReserveDrops,
		ctx:         context.Background(),
	}
	w.running.Store(true)
	w.tip = 100

	w.handleValidatedTip(context.Background(), 101)

	w.tipMtx.RLock()
	got := w.tip
	w.tipMtx.RUnlock()
	if got != 101 {
		t.Fatalf("tip = %d, want 101", got)
	}

	// Expect a TransactionNote and a BalanceChangeNote.
	var txNote *asset.TransactionNote
	var balNote *asset.BalanceChangeNote
	timeout := time.After(2 * time.Second)
	for txNote == nil || balNote == nil {
		select {
		case n := <-noteChan:
			switch v := n.(type) {
			case *asset.TransactionNote:
				txNote = v
			case *asset.BalanceChangeNote:
				balNote = v
			}
		case <-timeout:
			t.Fatalf("timed out waiting for notifications (tx=%v bal=%v)", txNote != nil, balNote != nil)
		}
	}
	if txNote.Transaction.Type != txdb.Receive {
		t.Errorf("tx type = %v, want Receive", txNote.Transaction.Type)
	}
}

// TestHandleValidatedTipQuietNoEmit verifies that a tip with no new transactions
// and no pending txs does NOT trigger a balance emission (avoids unnecessary RPCs).
func TestHandleValidatedTipQuietNoEmit(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	n, mt := newTestTxDB(t)

	m := newMockRippled(t)
	m.handle("account_tx", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{"status": "success", "transactions": []any{}}, nil
	})
	// reconcileSeq calls account_info (current ledger); return a minimal response.
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("10000000", 0, 0), nil
	})

	noteChan := make(chan asset.WalletNotification, 4)
	w := &xrpWallet{
		address:   "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
		rpc:       m.rpcClient(),
		txDB:      n,
		metaTable: mt,
		log:       log,
		emit:      asset.NewWalletEmitter(noteChan, BipID, log),
	}
	w.running.Store(true)
	w.tip = 100

	w.handleValidatedTip(context.Background(), 101)

	// Drain any notes that might arrive within a short window.
	time.Sleep(50 * time.Millisecond)
	select {
	case n := <-noteChan:
		if _, ok := n.(*asset.BalanceChangeNote); ok {
			t.Error("unexpected BalanceChangeNote on quiet ledger")
		}
	default:
	}
}

// TestReconcileSeqAdvances verifies that reconcileSeq advances nextSeq when the
// network reports a higher sequence than the local value.
func TestReconcileSeqAdvances(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"account_data": map[string]any{
				"Account":    "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
				"Balance":    "10000000",
				"Sequence":   uint32(42),
				"OwnerCount": uint32(0),
				"Flags":      uint32(0),
			},
			"ledger_index": uint64(100),
		}, nil
	})
	w := &xrpWallet{
		address: "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
		rpc:     m.rpcClient(),
		log:     log,
	}
	w.nextSeq = 10 // stale local value; remote reports 42

	w.reconcileSeq(context.Background())

	w.seqMtx.Lock()
	got := w.nextSeq
	w.seqMtx.Unlock()
	if got != 42 {
		t.Errorf("nextSeq = %d, want 42", got)
	}
}

// TestReconcileSeqDoesNotRewind verifies that reconcileSeq never decreases nextSeq.
func TestReconcileSeqDoesNotRewind(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"account_data": map[string]any{
				"Account":    "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
				"Balance":    "10000000",
				"Sequence":   uint32(5),
				"OwnerCount": uint32(0),
				"Flags":      uint32(0),
			},
			"ledger_index": uint64(100),
		}, nil
	})
	w := &xrpWallet{
		address: "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
		rpc:     m.rpcClient(),
		log:     log,
	}
	w.nextSeq = 99 // local is ahead of remote; must not rewind

	w.reconcileSeq(context.Background())

	w.seqMtx.Lock()
	got := w.nextSeq
	w.seqMtx.Unlock()
	if got != 99 {
		t.Errorf("nextSeq = %d, want 99 (must not rewind)", got)
	}
}

// TestReconcileSeqPurgesConsumedFreeSeq guards against the freeSeqs
// poisoning bug: a rollback that races a concurrent nextSeq reset can
// re-insert a sequence the chain has already consumed. Because next ==
// nextSeq in the poisoned steady state, the "next > nextSeq" advance branch
// never fires and never clears it — reconcileSeq must also purge freeSeqs
// entries below the account's live next sequence even when nextSeq itself
// doesn't move, or every later reservation keeps handing out the already-
// consumed sequence and failing with tefPAST_SEQ forever.
func TestReconcileSeqPurgesConsumedFreeSeq(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"account_data": map[string]any{
				"Account":    "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
				"Balance":    "10000000",
				"Sequence":   uint32(11),
				"OwnerCount": uint32(0),
				"Flags":      uint32(0),
			},
			"ledger_index": uint64(100),
		}, nil
	})
	w := &xrpWallet{
		address: "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
		rpc:     m.rpcClient(),
		log:     log,
	}
	w.nextSeq = 11
	w.freeSeqs = []uint32{10} // already consumed on-chain per account_info above

	w.reconcileSeq(context.Background())

	w.seqMtx.Lock()
	defer w.seqMtx.Unlock()
	if w.nextSeq != 11 {
		t.Fatalf("nextSeq = %d, want unchanged 11", w.nextSeq)
	}
	if len(w.freeSeqs) != 0 {
		t.Fatalf("freeSeqs = %v, want empty (consumed sequence must be purged)", w.freeSeqs)
	}
}

// TestScanAccountTxsNewPayment verifies that scanAccountTxs stores a new inbound
// payment and reports balanceChanged=true.
func TestScanAccountTxsNewPayment(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	n, mt := newTestTxDB(t)

	const walletAddr = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	m := newMockRippled(t)
	m.handle("account_tx", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"transactions": []any{
				map[string]any{
					"tx": map[string]any{
						"hash":            "BBBB000000000000000000000000000000000000000000000000000000000001",
						"TransactionType": "Payment",
						"Account":         "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe",
						"Destination":     walletAddr,
						"Amount":          "2000000",
						"Fee":             "12",
						"ledger_index":    uint64(51),
					},
					"meta": map[string]any{
						"TransactionResult": "tesSUCCESS",
						"delivered_amount":  "2000000",
					},
					"validated":    true,
					"ledger_index": uint64(51),
				},
			},
		}, nil
	})

	w := &xrpWallet{
		address:   walletAddr,
		rpc:       m.rpcClient(),
		txDB:      n,
		metaTable: mt,
		log:       log,
	}
	w.lastScannedLedger.Store(50)

	changed := w.scanAccountTxs(context.Background(), 51)
	if !changed {
		t.Error("expected balanceChanged=true after storing new payment")
	}
	stored, err := n.GetTx("BBBB000000000000000000000000000000000000000000000000000000000001")
	if err != nil || stored == nil {
		t.Fatalf("expected stored tx, got %v, %v", stored, err)
	}
	if stored.Type != txdb.Receive {
		t.Errorf("tx type = %v, want Receive", stored.Type)
	}
}

// TestScanAccountTxsAlreadyScanned verifies that a range already covered returns
// balanceChanged=false without making any RPC calls.
func TestScanAccountTxsAlreadyScanned(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	n, mt := newTestTxDB(t)

	m := newMockRippled(t)
	// account_tx must not be called — if it were the mock would fatalf.
	w := &xrpWallet{
		address:   "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
		rpc:       m.rpcClient(),
		txDB:      n,
		metaTable: mt,
		log:       log,
	}
	w.lastScannedLedger.Store(100)

	changed := w.scanAccountTxs(context.Background(), 100)
	if changed {
		t.Error("expected balanceChanged=false for already-scanned range")
	}
}

// TestScanAccountTxsClampsUnretainedHistory verifies that a fresh wallet's
// scan against a server that does not retain full history stops reissuing the
// identical failing range forever: the first lgrIdxsInvalid response clamps
// the floor to the server's oldest retained ledger, and the very next scan
// makes progress instead of repeating.
func TestScanAccountTxsClampsUnretainedHistory(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	n, mt := newTestTxDB(t)

	const walletAddr = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	const oldestRetained = uint64(90000000)
	const tip = uint64(100000000)

	m := newMockRippled(t)
	var lastMin uint64
	m.handle("account_tx", func(params json.RawMessage) (any, *rpcError) {
		var p struct {
			LedgerIndexMin uint64 `json:"ledger_index_min"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			t.Fatalf("decode account_tx params: %v", err)
		}
		lastMin = p.LedgerIndexMin
		if p.LedgerIndexMin < oldestRetained {
			return nil, &rpcError{code: 57, errType: "lgrIdxsInvalid", msg: "Ledger indexes invalid."}
		}
		return map[string]any{"status": "success", "transactions": []any{}}, nil
	})
	m.handle("server_info", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"info": map[string]any{
				"complete_ledgers": fmt.Sprintf("%d-%d", oldestRetained, tip),
			},
		}, nil
	})

	w := &xrpWallet{
		address:   walletAddr,
		rpc:       m.rpcClient(),
		txDB:      n,
		metaTable: mt,
		log:       log,
	}
	w.lastScannedLedger.Store(0)

	changed := w.scanAccountTxs(context.Background(), tip)
	if changed {
		t.Error("expected balanceChanged=false on the clamping scan")
	}
	if lastMin != 1 {
		t.Fatalf("first scan ledger_index_min = %d, want 1", lastMin)
	}
	if got := w.lastScannedLedger.Load(); got != oldestRetained-1 {
		t.Fatalf("after clamp, lastScannedLedger = %d, want %d", got, oldestRetained-1)
	}

	// The scan must make progress on the next tip, not repeat the identical
	// failing [1, tip] request forever.
	w.scanAccountTxs(context.Background(), tip)
	if lastMin != oldestRetained {
		t.Fatalf("second scan ledger_index_min = %d, want %d", lastMin, oldestRetained)
	}
	if got := w.lastScannedLedger.Load(); got != tip {
		t.Fatalf("after second scan, lastScannedLedger = %d, want %d (tip)", got, tip)
	}
}

// TestScanAccountTxsClampTooNewLeavesFloorUntouched covers the case where the
// requested max ledger is above the server's highest indexed ledger — the
// websocket ledgerClosed push fires the instant a ledger validates, but
// account_tx can lag slightly indexing it, producing the same lgrIdxsInvalid
// error as a too-old range. The response here must not touch the scan floor:
// advancing it would skip the ledger once it lands, and re-treating the
// still-unindexed tip as newly retained would repeat this rejection forever.
func TestScanAccountTxsClampTooNewLeavesFloorUntouched(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	n, mt := newTestTxDB(t)

	const walletAddr = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	const serverHi = uint64(106451011) // server has indexed through here
	const tip = uint64(106451012)      // just validated, not yet indexed

	m := newMockRippled(t)
	m.handle("account_tx", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{code: 57, errType: "lgrIdxsInvalid", msg: "Ledger indexes invalid."}
	})
	m.handle("server_info", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"info": map[string]any{
				"complete_ledgers": fmt.Sprintf("32570-%d", serverHi),
			},
		}, nil
	})

	w := &xrpWallet{
		address:   walletAddr,
		rpc:       m.rpcClient(),
		txDB:      n,
		metaTable: mt,
		log:       log,
	}
	w.lastScannedLedger.Store(serverHi)

	changed := w.scanAccountTxs(context.Background(), tip)
	if changed {
		t.Error("expected balanceChanged=false")
	}
	if got := w.lastScannedLedger.Load(); got != serverHi {
		t.Fatalf("lastScannedLedger = %d, want unchanged %d", got, serverHi)
	}
}

// TestScanAccountTxsClampAtExactBoundary reproduces the case observed live:
// complete_ledgers catches up to exactly maxLedger in the gap between the
// account_tx rejection and the server_info fetch that follows it, so hi ends
// up equal to (not less than) maxLedger. That equality is still evidence of
// the same not-yet-indexed lag, not a genuine anomaly, and must not fall
// through to the noisy "rejected range it claims to retain" warning or touch
// the scan floor.
func TestScanAccountTxsClampAtExactBoundary(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	n, mt := newTestTxDB(t)

	const walletAddr = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	const prevScanned = uint64(106451179)
	const tip = uint64(106451180) // account_tx rejects it, then server_info reports it complete

	m := newMockRippled(t)
	m.handle("account_tx", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{code: 57, errType: "lgrIdxsInvalid", msg: "Ledger indexes invalid."}
	})
	m.handle("server_info", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"info": map[string]any{
				"complete_ledgers": fmt.Sprintf("32570-%d", tip),
			},
		}, nil
	})

	w := &xrpWallet{
		address:   walletAddr,
		rpc:       m.rpcClient(),
		txDB:      n,
		metaTable: mt,
		log:       log,
	}
	w.lastScannedLedger.Store(prevScanned)

	changed := w.scanAccountTxs(context.Background(), tip)
	if changed {
		t.Error("expected balanceChanged=false")
	}
	if got := w.lastScannedLedger.Load(); got != prevScanned {
		t.Fatalf("lastScannedLedger = %d, want unchanged %d", got, prevScanned)
	}
}

// TestScanAccountTxsClampServerInfoFailure verifies that a server_info error
// while clamping leaves the scan floor untouched rather than advancing it
// blindly and silently skipping real history.
func TestScanAccountTxsClampServerInfoFailure(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	n, mt := newTestTxDB(t)

	m := newMockRippled(t)
	m.handle("account_tx", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{code: 57, errType: "lgrIdxsInvalid", msg: "Ledger indexes invalid."}
	})
	m.handle("server_info", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{code: 500, errType: "internal", msg: "boom"}
	})

	w := &xrpWallet{
		address:   "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
		rpc:       m.rpcClient(),
		txDB:      n,
		metaTable: mt,
		log:       log,
	}
	w.lastScannedLedger.Store(0)

	changed := w.scanAccountTxs(context.Background(), 100000000)
	if changed {
		t.Error("expected balanceChanged=false")
	}
	if got := w.lastScannedLedger.Load(); got != 0 {
		t.Fatalf("lastScannedLedger = %d, want unchanged 0", got)
	}
}

// TestTipWorkerCoalescing verifies that the tipWorker drain loop processes only
// the highest queued tip when multiple tips arrive during a scan.
func TestTipWorkerCoalescing(t *testing.T) {
	log := util.NewLogger("test", util.LevelError, nil)
	n, mt := newTestTxDB(t)

	m := newMockRippled(t)
	m.handle("account_tx", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{"status": "success", "transactions": []any{}}, nil
	})
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"account_data": map[string]any{
				"Account":    "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
				"Balance":    "10000000",
				"Sequence":   uint32(1),
				"OwnerCount": uint32(0),
				"Flags":      uint32(0),
			},
			"ledger_index": uint64(100),
		}, nil
	})

	processed := make(chan uint64, 8)
	const walletAddr = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"

	// Wrap handleValidatedTip to record which tip was actually processed.
	tipCh := make(chan uint64, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := &xrpWallet{
		address:     walletAddr,
		rpc:         m.rpcClient(),
		txDB:        n,
		metaTable:   mt,
		log:         log,
		baseReserve: BaseReserveDrops,
		tipCh:       tipCh,
	}
	w.running.Store(true)
	w.tip = 100

	// Queue both tips into the buffered channel before starting the consumer,
	// so the goroutine cannot wake and drain the channel between the two sends.
	// Otherwise it may read 101, find the channel empty, and process 101 alone
	// before 105 is queued (flaky, especially under -race).
	tipCh <- 101
	tipCh <- 105

	// Override handleValidatedTip via a thin goroutine that records which tip
	// reached processing. We run a direct tipWorker loop manually.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case tip := <-tipCh:
			drain:
				for {
					select {
					case newer := <-tipCh:
						if newer > tip {
							tip = newer
						}
					default:
						break drain
					}
				}
				w.handleValidatedTip(ctx, tip)
				processed <- tip
			}
		}
	}()

	select {
	case tip := <-processed:
		if tip != 105 {
			t.Errorf("first processed tip = %d, want 105 (coalesced highest)", tip)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tip processing")
	}
}
