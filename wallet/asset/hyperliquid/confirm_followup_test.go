// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/txdb"
)

// storePending inserts a pending send with a nonce-based ID and returns its ID.
func storePending(t *testing.T, w *HyperWallet, nonce, amount uint64, recipient string) string {
	t.Helper()
	id := fmt.Sprintf("0x%016x", nonce)
	rcpt := recipient
	tx := &dbTx{
		WalletTransaction: &txdb.WalletTransaction{
			Type:      txdb.Send,
			ID:        id,
			Amount:    amount,
			Recipient: &rcpt,
			AdditionalData: map[string]string{
				"nonce": strconv.FormatUint(nonce, 10),
			},
		},
		SubmissionTime: uint64(time.Now().Unix()),
	}
	if err := w.txDB.Load().storeTx(tx); err != nil {
		t.Fatalf("storeTx: %v", err)
	}
	return id
}

// TestConfirmPendingSendExactNonce verifies that when the ledger delta carries a
// nonce, the matching send is chosen by exact nonce — even when a different
// pending send (same amount + recipient) is closer in time.
func TestConfirmPendingSendExactNonce(t *testing.T) {
	w := newTestWalletWithDB(t)
	const recipient = "0x1111111111111111111111111111111111111111"
	const amount = uint64(5e8)

	nonceA := uint64(1700000001000)
	nonceB := uint64(1700000002000)
	idA := storePending(t, w, nonceA, amount, recipient)
	idB := storePending(t, w, nonceB, amount, recipient)

	// Event time is closest to nonce A, but the nonce names B. Exact match wins.
	realHash := "0x" + fmt.Sprintf("%064x", 7)
	update := ledgerUpdate{
		Time: nonceA + 100,
		Hash: realHash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "5",
			User:        w.address,
			Destination: recipient,
			Nonce:       &nonceB,
		},
	}
	w.confirmPendingSend(update, amount, w.txDB.Load())

	confirmed, _ := w.txDB.Load().getTx(realHash)
	if confirmed == nil {
		t.Fatal("confirmed tx not found")
	}
	if confirmed.AdditionalData[pendingIDKey] != idB {
		t.Fatalf("confirmed alias = %q, want B (%q) — exact nonce ignored", confirmed.AdditionalData[pendingIDKey], idB)
	}
	if a, _ := w.txDB.Load().getTx(idA); a == nil {
		t.Error("pending A should remain (it was not the nonce match)")
	}
	if b, _ := w.txDB.Load().getTx(idB); b != nil {
		t.Error("pending B should have been consumed")
	}
}

// TestConfirmPendingSendFeeAndTimestamp verifies the confirmed send records the
// fee and timestamp from the ledger event.
func TestConfirmPendingSendFeeAndTimestamp(t *testing.T) {
	w := newTestWalletWithDB(t)
	const recipient = "0x2222222222222222222222222222222222222222"
	const amount = uint64(5e8)
	nonce := uint64(1700000003000)
	storePending(t, w, nonce, amount, recipient)

	realHash := "0x" + fmt.Sprintf("%064x", 9)
	update := ledgerUpdate{
		Time: nonce + 500,
		Hash: realHash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "5",
			User:        w.address,
			Destination: recipient,
			Fee:         "0.01",
			Nonce:       &nonce,
		},
	}
	w.confirmPendingSend(update, amount, w.txDB.Load())

	confirmed, _ := w.txDB.Load().getTx(realHash)
	if confirmed == nil {
		t.Fatal("confirmed tx not found")
	}
	if confirmed.Fees != 1e6 {
		t.Errorf("Fees = %d, want %d", confirmed.Fees, uint64(1e6))
	}
	if want := (nonce + 500) / 1000; confirmed.Timestamp != want {
		t.Errorf("Timestamp = %d, want %d", confirmed.Timestamp, want)
	}
}

// TestReceivePopulatesTimestamp verifies an incoming transfer records a non-zero
// Timestamp from the ledger event time.
func TestReceivePopulatesTimestamp(t *testing.T) {
	w := newTestWalletWithDB(t)
	hash := "0x" + fmt.Sprintf("%064x", 11)
	eventMS := uint64(1700000005000)
	update := ledgerUpdate{
		Time: eventMS,
		Hash: hash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "3",
			User:        "0x3333333333333333333333333333333333333333",
			Destination: w.address,
		},
	}
	w.processLedgerUpdate(update)

	rcv, _ := w.txDB.Load().getTx(hash)
	if rcv == nil {
		t.Fatal("receive tx not stored")
	}
	if want := eventMS / 1000; rcv.Timestamp != want {
		t.Errorf("Timestamp = %d, want %d", rcv.Timestamp, want)
	}
}
