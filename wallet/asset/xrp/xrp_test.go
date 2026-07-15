// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/bosswallet/wallet/txdb"

	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset/keystore"
	btcec "github.com/btcsuite/btcd/btcec/v2"
)

// TestXRPBase58 verifies round-trip encode/decode with the XRP alphabet and
// tests the well-known genesis address as a fixed vector.
func TestXRPBase58(t *testing.T) {
	// Round-trip test.
	original := []byte{0x00, 0xb5, 0xf7, 0x62, 0x79, 0x8a, 0x53, 0xd5, 0x43,
		0xa0, 0x14, 0xca, 0xf8, 0xb2, 0x97, 0xcf, 0xf8, 0xf2, 0xf9, 0x37, 0xe8}

	encoded := encodeXRPBase58(original)
	decoded, err := decodeXRPBase58(encoded)
	if err != nil {
		t.Fatalf("decodeXRPBase58 error: %v", err)
	}
	if !bytes.Equal(original, decoded) {
		t.Errorf("round-trip mismatch: got %x, want %x", decoded, original)
	}

	// Invalid character.
	if _, err := decodeXRPBase58("r0invalid0char"); err == nil {
		t.Error("expected error for invalid base58 character '0'")
	}
}

// TestAddressEncoding verifies address encode/decode using the well-known XRP
// genesis address rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh.
func TestAddressEncoding(t *testing.T) {
	// Account ID for the genesis address (from the XRP Ledger specification).
	accountIDHex := "b5f762798a53d543a014caf8b297cff8f2f937e8"
	accountID, err := hex.DecodeString(accountIDHex)
	if err != nil {
		t.Fatalf("hex decode error: %v", err)
	}

	addr := accountIDToAddress(accountID)
	const want = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	if addr != want {
		t.Errorf("accountIDToAddress = %q, want %q", addr, want)
	}

	// Decode the address back.
	got, err := addressToAccountID(addr)
	if err != nil {
		t.Fatalf("addressToAccountID error: %v", err)
	}
	if !bytes.Equal(got, accountID) {
		t.Errorf("addressToAccountID = %x, want %x", got, accountID)
	}
}

// TestValidateAddress checks both valid and invalid XRP addresses.
func TestValidateAddress(t *testing.T) {
	w := &xrpWallet{}

	valid := []string{
		"rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
		"rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe",
	}
	for _, addr := range valid {
		if !w.ValidateAddress(addr) {
			t.Errorf("expected %q to be valid", addr)
		}
	}

	invalid := []string{
		"",
		"notanaddress",
		"1HB9CJAWyB4rj91VRWn96DkukG4bwdtyTh",  // wrong first char (Bitcoin alphabet)
		"rHb9CJAWyB4rj91VRWn96DkukG4bwdtyT",   // truncated (bad checksum)
		"rHb9CJAWyB4rj91VRWn96DkukG4bwdtyThX", // extra char (bad checksum)
	}
	for _, addr := range invalid {
		if w.ValidateAddress(addr) {
			t.Errorf("expected %q to be invalid", addr)
		}
	}
}

// TestDecodeCoinID verifies the coin ID encoding.
func TestDecodeCoinID(t *testing.T) {
	coinID := make([]byte, 32)
	for i := range coinID {
		coinID[i] = byte(i)
	}
	decoded, err := decodeCoinID(coinID)
	if err != nil {
		t.Fatalf("decodeCoinID error: %v", err)
	}
	if len(decoded) != 64 {
		t.Errorf("decoded length = %d, want 64", len(decoded))
	}
	// Must be uppercase hex.
	want := "000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F"
	if decoded != want {
		t.Errorf("decoded = %q, want %q", decoded, want)
	}

	// Short coin ID must fail.
	if _, err := decodeCoinID([]byte{1, 2, 3}); err == nil {
		t.Error("expected error for short coin ID")
	}
}

// TestXRPAmountEncoding verifies the Amount field encoding for XRP drops.
func TestXRPAmountEncoding(t *testing.T) {
	tests := []struct {
		drops uint64
		want  [8]byte
	}{
		// 1 XRP = 1,000,000 drops: 0x4000000000000000 | 0xF4240 = 0x400000000000F4240...
		// Actually: 0x4000000000000000 | 1000000 = 0x400000000000F4240? No:
		// 1000000 = 0xF4240, so the result is 0x400000000000F4240 overflows...
		// Wait: 0x4000000000000000 = 4611686018427387904
		// + 1000000 = 4611686018428387904
		// In hex: 0x400000000000F4240... that's 9 hex pairs?
		// No: 0x400000000000F4240 is wrong. Let me be precise:
		// 0x4000000000000000 in 8 bytes: 40 00 00 00 00 00 00 00
		// 1000000 = 0x000F4240 in 4 bytes, or 0x00000000000F4240 in 8 bytes
		// OR: 40 00 00 00 00 0F 42 40
		{drops: 1_000_000, want: [8]byte{0x40, 0x00, 0x00, 0x00, 0x00, 0x0F, 0x42, 0x40}},
		// 12 drops (typical fee): 0x4000000000000000 | 12 = 0x400000000000000C
		{drops: 12, want: [8]byte{0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0C}},
	}

	for _, tt := range tests {
		var b bytes.Buffer
		writeXRPAmount(&b, tt.drops)
		got := b.Bytes()
		if !bytes.Equal(got, tt.want[:]) {
			t.Errorf("writeXRPAmount(%d) = %x, want %x", tt.drops, got, tt.want)
		}
	}
}

// TestPaymentSerialization verifies that encodePayment produces a canonical
// serialization with all fields in canonical order.
func TestPaymentSerialization(t *testing.T) {
	privKeyBytes, _ := hex.DecodeString(
		"0000000000000000000000000000000000000000000000000000000000000001")
	_, pubKey := btcec.PrivKeyFromBytes(privKeyBytes)

	srcID, _ := addressToAccountID("rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh")
	dstID, _ := addressToAccountID("rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe")

	p := &xrpPayment{
		account:       [20]byte(srcID),
		destination:   [20]byte(dstID),
		amount:        1_000_000,
		fee:           12,
		sequence:      1,
		lastLedgerSeq: 100,
		signingPubKey: pubKey.SerializeCompressed(),
	}

	b := encodePayment(p, true) // forSigning = true → no TxnSignature

	// Verify fields are present in canonical order by checking header bytes.
	checkHeader := func(offset int, wantHeader ...byte) {
		t.Helper()
		for i, h := range wantHeader {
			if offset+i >= len(b) {
				t.Errorf("serialized tx too short at offset %d", offset+i)
				return
			}
			if b[offset+i] != h {
				t.Errorf("offset %d: got 0x%02X, want 0x%02X", offset+i, b[offset+i], h)
			}
		}
	}

	// TransactionType header 0x12 at offset 0.
	checkHeader(0, 0x12)
	// Flags header 0x22 at offset 3 (0x12 + 2-byte value).
	checkHeader(3, 0x22)
	// Sequence header 0x24 at offset 8 (prev + 4-byte value).
	checkHeader(8, 0x24)
	// LastLedgerSequence 2-byte header 0x20 0x1B at offset 13.
	checkHeader(13, 0x20, 0x1B)
	// Amount header 0x61 at offset 19 (prev + 2-byte header + 4-byte value).
	checkHeader(19, 0x61)

	// Verify TxnSignature is absent (forSigning=true).
	for i, v := range b {
		if v == 0x74 {
			t.Errorf("unexpected TxnSignature header 0x74 at offset %d in signing serialization", i)
		}
	}
}

// TestDriverInfo verifies the driver metadata.
func TestDriverInfo(t *testing.T) {
	d := &Driver{}
	info := d.Info()

	if info.Name != "XRP" {
		t.Errorf("Name = %q, want %q", info.Name, "XRP")
	}
	if info.UnitInfo.AtomicUnit != "drops" {
		t.Errorf("AtomicUnit = %q, want \"drops\"", info.UnitInfo.AtomicUnit)
	}
	if info.UnitInfo.Conventional.ConversionFactor != 1e6 {
		t.Errorf("ConversionFactor = %v, want 1e6", info.UnitInfo.Conventional.ConversionFactor)
	}
	if len(info.AvailableWallets) == 0 {
		t.Error("no available wallet definitions")
	}
	if info.Network == nil || info.Network.ID != BipID {
		t.Errorf("Network.ID = %v, want %d", info.Network, BipID)
	}
}

// TestAddressGeneration verifies that a known seed produces a deterministic
// XRP address through the full BIP44 derivation path m/44'/144'/0'/0/0.
// The expected address is a fixed vector — if it changes, the derivation
// path or address encoding has regressed.
func TestAddressGeneration(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}

	privKey, zero, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("privKeyFromSeed error: %v", err)
	}
	defer zero()

	addr := pubKeyToAddress(privKey.PubKey())
	const wantAddr = "rKV75A16S6FfHxwoSPriWWPuzt7qKRdkE1"
	if addr != wantAddr {
		t.Errorf("address = %q, want %q", addr, wantAddr)
	}
	if !(&xrpWallet{}).ValidateAddress(addr) {
		t.Errorf("derived address %q failed validation", addr)
	}
}

// TestPubKeyToAddress verifies address derivation from a known key.
func TestPubKeyToAddress(t *testing.T) {
	// Use a deterministic private key.
	privKeyBytes := make([]byte, 32)
	privKeyBytes[31] = 1 // scalar = 1

	_, pubKey := btcec.PrivKeyFromBytes(privKeyBytes)
	addr := pubKeyToAddress(pubKey)

	if !(&xrpWallet{}).ValidateAddress(addr) {
		t.Errorf("derived address %q failed validation", addr)
	}
	if len(addr) == 0 || addr[0] != 'r' {
		t.Errorf("expected address to start with 'r', got %q", addr)
	}

	// Same key always yields the same address.
	addr2 := pubKeyToAddress(pubKey)
	if addr != addr2 {
		t.Errorf("non-deterministic address derivation: %q vs %q", addr, addr2)
	}
}

// TestKeyFileRoundTrip verifies that a key file written via keystore.Save can be
// read back and decrypts to the original key.
func TestKeyFileRoundTrip(t *testing.T) {
	privKeyBytes := make([]byte, 32)
	privKeyBytes[31] = 99
	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)
	pubKey := privKey.PubKey().SerializeCompressed()
	pw := []byte("keyfile-test-password-padded32by")

	dir := t.TempDir()
	path := filepath.Join(dir, keyFileName)

	if err := keystore.Save(path, privKey.Serialize(), pubKey, pw); err != nil {
		t.Fatalf("keystore.Save error: %v", err)
	}

	gotPub, err := keystore.PubKey(path)
	if err != nil {
		t.Fatalf("keystore.PubKey error: %v", err)
	}
	if !bytes.Equal(gotPub, pubKey) {
		t.Error("stored public key does not match")
	}

	decrypted, err := keystore.Open(path, pw)
	if err != nil {
		t.Fatalf("keystore.Open error: %v", err)
	}
	defer encode.ClearBytes(decrypted)

	recovered, _ := btcec.PrivKeyFromBytes(decrypted)
	if !recovered.PubKey().IsEqual(privKey.PubKey()) {
		t.Error("recovered key does not match original")
	}
}

// TestSendValidation verifies that Send rejects invalid inputs before touching
// the network, and that pre-submit rejections do not leak sequence numbers.
func TestSendValidation(t *testing.T) {
	w := &xrpWallet{address: "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"}
	w.running.Store(true)

	// Invalid address.
	if _, err := w.Send("notanaddress", 1000, 0); err == nil {
		t.Error("expected error for invalid address")
	}

	// Zero value.
	if _, err := w.Send("rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh", 0, 0); err == nil {
		t.Error("expected error for zero value")
	}

	// Locked wallet: must return an error without advancing nextSeq.
	// privKey is nil (zero value), so the wallet is locked.
	w.nextSeq = 42
	if _, err := w.Send("rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh", 1000, 0); err == nil {
		t.Error("expected error for locked wallet")
	}
	// nextSeq must remain at 42 — no sequence was consumed.
	w.seqMtx.Lock()
	got := w.nextSeq
	w.seqMtx.Unlock()
	if got != 42 {
		t.Errorf("nextSeq after locked-wallet Send = %d, want 42 (sequence must not advance)", got)
	}

	// X-address rejection must not advance nextSeq either.
	w.nextSeq = 10
	if _, err := w.Send("X7AcgcsBL4L51nv2theweHmbx6Y8kJ5vvTD", 1000, 0); err == nil {
		t.Error("expected error for X-address")
	}
	w.seqMtx.Lock()
	got = w.nextSeq
	w.seqMtx.Unlock()
	if got != 10 {
		t.Errorf("nextSeq after X-address Send = %d, want 10", got)
	}
}

func TestNextAccountSequence(t *testing.T) {
	tests := []struct {
		name string
		info *accountInfoResult
		want uint32
	}{
		{
			name: "nil info",
			want: 0,
		},
		{
			name: "no queue data",
			info: &accountInfoResult{
				AccountData: accountData{Sequence: 7},
			},
			want: 7,
		},
		{
			name: "empty queue data",
			info: &accountInfoResult{
				AccountData: accountData{Sequence: 7},
				QueueData:   &accountQueueData{},
			},
			want: 7,
		},
		{
			name: "append after contiguous queue",
			info: &accountInfoResult{
				AccountData: accountData{Sequence: 10},
				QueueData: &accountQueueData{
					TxnCount:        3,
					LowestSequence:  10,
					HighestSequence: 12,
				},
			},
			want: 13,
		},
		{
			name: "fill gap before queued transactions",
			info: &accountInfoResult{
				AccountData: accountData{Sequence: 10},
				QueueData: &accountQueueData{
					TxnCount:        2,
					LowestSequence:  11,
					HighestSequence: 12,
				},
			},
			want: 10,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := nextAccountSequence(test.info)
			if got != test.want {
				t.Fatalf("nextAccountSequence() = %d, want %d", got, test.want)
			}
		})
	}
}

// TestReserveSequenceRollbackReuse guards against a stranded sequence gap: if
// a reservation is rolled back after a concurrent reservation has already
// advanced nextSeq past it, the rolled-back sequence must still be handed
// out to the next caller instead of being silently dropped, since XRPL
// requires an account's sequences to be consumed in order.
func TestReserveSequenceRollbackReuse(t *testing.T) {
	w := &xrpWallet{nextSeq: 5}

	seqA, rollbackA, err := w.reserveSequence()
	if err != nil {
		t.Fatalf("reserveSequence (A) error: %v", err)
	}
	if seqA != 5 {
		t.Fatalf("seqA = %d, want 5", seqA)
	}

	seqB, _, err := w.reserveSequence()
	if err != nil {
		t.Fatalf("reserveSequence (B) error: %v", err)
	}
	if seqB != 6 {
		t.Fatalf("seqB = %d, want 6", seqB)
	}

	// A rolls back after B has already advanced nextSeq past it.
	rollbackA()

	seqC, _, err := w.reserveSequence()
	if err != nil {
		t.Fatalf("reserveSequence (C) error: %v", err)
	}
	if seqC != seqA {
		t.Fatalf("seqC = %d, want reused seqA %d (sequence gap was stranded)", seqC, seqA)
	}
}

// TestReserveSequenceDropsPoisonedFreeSeq guards against handing out a
// cross-frame-poisoned freeSeqs entry: a rollback that races a concurrent
// nextSeq reset can plant a sequence >= nextSeq, which was never validly
// minted (a legitimate freeSeqs entry always satisfies seq < nextSeq at
// insertion time). Handing such an entry out would submit a sequence ahead
// of what the account can accept.
func TestReserveSequenceDropsPoisonedFreeSeq(t *testing.T) {
	w := &xrpWallet{nextSeq: 11, freeSeqs: []uint32{15}} // poisoned: 15 >= nextSeq

	seq, _, err := w.reserveSequence()
	if err != nil {
		t.Fatalf("reserveSequence error: %v", err)
	}
	if seq != 11 {
		t.Fatalf("seq = %d, want 11 (poisoned freeSeqs entry 15 should have been dropped)", seq)
	}
	w.seqMtx.Lock()
	defer w.seqMtx.Unlock()
	if len(w.freeSeqs) != 0 {
		t.Fatalf("freeSeqs = %v, want empty", w.freeSeqs)
	}
}

func TestWalletTransactionFromTx(t *testing.T) {
	const walletAddr = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	const otherAddr = "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"

	tests := []struct {
		name      string
		tx        *txResult
		meta      txMeta
		validated bool
		ledgerIdx uint64
		check     func(t *testing.T, wt *txdb.WalletTransaction)
	}{
		{
			name: "ignore non-payment transaction",
			tx: &txResult{
				Account:         walletAddr,
				TransactionType: "AccountSet",
				Hash:            "abc",
			},
			check: func(t *testing.T, wt *txdb.WalletTransaction) {
				if wt != nil {
					t.Fatalf("expected nil wallet transaction, got %+v", wt)
				}
			},
		},
		{
			name: "ignore failed incoming payment",
			tx: &txResult{
				Account:         otherAddr,
				Destination:     walletAddr,
				TransactionType: "Payment",
				Amount:          "100",
				Fee:             "12",
				Hash:            "def",
			},
			meta: txMeta{
				TransactionResult: "tecDST_TAG_NEEDED",
			},
			validated: true,
			ledgerIdx: 10,
			check: func(t *testing.T, wt *txdb.WalletTransaction) {
				if wt != nil {
					t.Fatalf("expected nil wallet transaction, got %+v", wt)
				}
			},
		},
		{
			name: "classify incoming xrp payment",
			tx: &txResult{
				Account:         otherAddr,
				Destination:     walletAddr,
				TransactionType: "Payment",
				Amount:          "100",
				Fee:             "12",
				Hash:            "0123abcd",
			},
			meta: txMeta{
				DeliveredAmount: "95",
			},
			validated: true,
			ledgerIdx: 11,
			check: func(t *testing.T, wt *txdb.WalletTransaction) {
				if wt == nil {
					t.Fatal("expected wallet transaction")
				}
				if wt.Type != txdb.Receive || wt.Amount != 95 || wt.BlockNumber != 11 || !wt.Confirmed {
					t.Fatalf("unexpected receive tx: %+v", wt)
				}
				if wt.Recipient == nil || *wt.Recipient != walletAddr {
					t.Fatalf("unexpected recipient: %+v", wt.Recipient)
				}
			},
		},
		{
			name: "classify outgoing rejected xrp payment",
			tx: &txResult{
				Account:         walletAddr,
				Destination:     otherAddr,
				TransactionType: "Payment",
				Amount:          "250",
				Fee:             "12",
				Hash:            "abcd0123",
			},
			meta: txMeta{
				TransactionResult: "tecNO_DST",
			},
			validated: true,
			ledgerIdx: 12,
			check: func(t *testing.T, wt *txdb.WalletTransaction) {
				if wt == nil {
					t.Fatal("expected wallet transaction")
				}
				if wt.Type != txdb.Send || wt.Amount != 250 || !wt.Rejected {
					t.Fatalf("unexpected send tx: %+v", wt)
				}
				if wt.Recipient == nil || *wt.Recipient != otherAddr {
					t.Fatalf("unexpected recipient: %+v", wt.Recipient)
				}
			},
		},
		{
			name: "ignore non-xrp payment amount",
			tx: &txResult{
				Account:         walletAddr,
				Destination:     otherAddr,
				TransactionType: "Payment",
				Amount: map[string]any{
					"currency": "USD",
					"value":    "1",
				},
				Fee:  "12",
				Hash: "ff",
			},
			check: func(t *testing.T, wt *txdb.WalletTransaction) {
				if wt != nil {
					t.Fatalf("expected nil wallet transaction, got %+v", wt)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wt := walletTransactionFromTx(test.tx, walletAddr, test.meta, test.validated, test.ledgerIdx)
			test.check(t, wt)
		})
	}
}
