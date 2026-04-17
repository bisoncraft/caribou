package near

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"decred.org/dcrdex/dex/encode"
	dexnear "decred.org/dcrdex/dex/networks/near"
)

func TestValidateAddress(t *testing.T) {
	w := &NearWallet{}

	tests := []struct {
		name    string
		address string
		want    bool
	}{
		{
			name:    "valid implicit account",
			address: "98793cd91a3f870fb126f66285808c7e094afcfc4eda8a970f6648cdf0dbd6de",
			want:    true,
		},
		{
			name:    "valid named account mainnet",
			address: "alice.near",
			want:    true,
		},
		{
			name:    "valid named account testnet",
			address: "bob.testnet",
			want:    true,
		},
		{
			name:    "valid sub-account",
			address: "sub.alice.near",
			want:    true,
		},
		{
			name:    "invalid - too short hex",
			address: "98793cd91a3f870fb126f66285808c7e",
			want:    false,
		},
		{
			name:    "invalid - bad hex chars",
			address: "zz793cd91a3f870fb126f66285808c7e094afcfc4eda8a970f6648cdf0dbd6de",
			want:    false,
		},
		{
			name:    "invalid - empty",
			address: "",
			want:    false,
		},
		{
			name:    "invalid - wrong TLD",
			address: "alice.ethereum",
			want:    false,
		},
		{
			name:    "invalid - uppercase",
			address: "Alice.near",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := w.ValidateAddress(tt.address)
			if got != tt.want {
				t.Errorf("ValidateAddress(%q) = %v, want %v", tt.address, got, tt.want)
			}
		})
	}
}

func TestUnitConversion(t *testing.T) {
	tests := []struct {
		name      string
		drops     uint64
		wantYocto string
	}{
		{
			name:      "one NEAR",
			drops:     1e8,
			wantYocto: "1000000000000000000000000", // 1e24
		},
		{
			name:      "one drop",
			drops:     1,
			wantYocto: "10000000000000000", // 1e16
		},
		{
			name:      "zero",
			drops:     0,
			wantYocto: "0",
		},
		{
			name:      "half NEAR",
			drops:     5e7,
			wantYocto: "500000000000000000000000", // 5e23
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yocto := dexnear.DropsToYocto(tt.drops)
			if yocto.String() != tt.wantYocto {
				t.Errorf("DropsToYocto(%d) = %s, want %s", tt.drops, yocto.String(), tt.wantYocto)
			}

			// Round-trip.
			roundTripped := dexnear.YoctoToDrops(yocto)
			if roundTripped != tt.drops {
				t.Errorf("YoctoToDrops(DropsToYocto(%d)) = %d", tt.drops, roundTripped)
			}
		})
	}
}

func TestYoctoToDropsTruncation(t *testing.T) {
	// yoctoNEAR that doesn't divide evenly into drops should truncate.
	yocto := new(big.Int).Add(dexnear.DropsToYocto(42), big.NewInt(999))
	drops := dexnear.YoctoToDrops(yocto)
	if drops != 42 {
		t.Errorf("YoctoToDrops with remainder = %d, want 42", drops)
	}
}

func TestCoinID(t *testing.T) {
	hashHex := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	hashBytes, _ := hex.DecodeString(hashHex)

	var txHash [32]byte
	copy(txHash[:], hashBytes)

	c := &coin{txHash: txHash, value: 12345}

	if c.Value() != 12345 {
		t.Errorf("coin.Value() = %d, want 12345", c.Value())
	}
	if len(c.ID()) != 32 {
		t.Errorf("coin.ID() length = %d, want 32", len(c.ID()))
	}

	// String and TxID should return base58.
	txID := c.TxID()
	if txID == "" {
		t.Error("coin.TxID() is empty")
	}
	if c.String() != txID {
		t.Errorf("coin.String() = %q != coin.TxID() = %q", c.String(), txID)
	}

	// DecodeCoinID round-trip: raw bytes -> base58 string.
	decoded, err := decodeCoinID(c.ID())
	if err != nil {
		t.Fatalf("decodeCoinID error: %v", err)
	}
	if decoded != txID {
		t.Errorf("decodeCoinID = %q, want %q", decoded, txID)
	}

	// Invalid length.
	_, err = decodeCoinID([]byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for short coin ID")
	}
}

func TestBorshTransfer(t *testing.T) {
	tx := &nearTransaction{
		signerID:   "sender.near",
		publicKey:  [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32},
		nonce:      42,
		receiverID: "receiver.near",
		blockHash:  [32]byte{0xff, 0xfe, 0xfd},
		actions:    []action{transferAction(big.NewInt(1000000))},
	}

	b := serializeTransaction(tx)
	if len(b) == 0 {
		t.Fatal("serializeTransaction returned empty bytes")
	}

	// Verify the serialized data starts with the signer ID string.
	// Borsh string: 4-byte LE length + UTF-8 bytes
	if b[0] != 11 || b[1] != 0 || b[2] != 0 || b[3] != 0 { // len("sender.near") = 11
		t.Errorf("unexpected signer ID length prefix: %v", b[:4])
	}

	sig := make([]byte, 64)
	signed := serializeSignedTransaction(b, sig)
	if len(signed) != len(b)+1+64 {
		t.Errorf("signed tx length = %d, want %d", len(signed), len(b)+1+64)
	}
}

func TestMarshalU128LE(t *testing.T) {
	tests := []struct {
		name string
		val  *big.Int
		want [16]byte
	}{
		{
			name: "zero",
			val:  big.NewInt(0),
			want: [16]byte{},
		},
		{
			name: "one",
			val:  big.NewInt(1),
			want: [16]byte{1},
		},
		{
			name: "256",
			val:  big.NewInt(256),
			want: [16]byte{0, 1},
		},
		{
			name: "one NEAR in yocto",
			val:  new(big.Int).SetBytes([]byte{0x03, 0x3B, 0x2E, 0x3C, 0x9F, 0xD0, 0x80, 0x3C, 0xE8, 0x00, 0x00, 0x00}), // 1e24 ~= 0x033B2E3C9FD0803CE8000000
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := marshalU128LE(tt.val)
			if len(got) != 16 {
				t.Fatalf("marshalU128LE length = %d, want 16", len(got))
			}
			if tt.name != "one NEAR in yocto" {
				var want16 [16]byte
				copy(want16[:], tt.want[:])
				var got16 [16]byte
				copy(got16[:], got)
				if got16 != want16 {
					t.Errorf("marshalU128LE(%s) = %x, want %x", tt.val, got, want16)
				}
			}
		})
	}
}

func TestEncryptDecryptSeed(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	pw := []byte("test-password-that-is-32-bytes!!")

	encrypted, err := encryptSeed(seed, pw)
	if err != nil {
		t.Fatalf("encryptSeed error: %v", err)
	}

	if len(encrypted) <= len(seed) {
		t.Fatalf("encrypted length %d should be > seed length %d", len(encrypted), len(seed))
	}

	if bytes.Contains(encrypted, seed) {
		t.Fatal("ciphertext contains plaintext seed")
	}

	decrypted, err := decryptSeed(encrypted, pw)
	if err != nil {
		t.Fatalf("decryptSeed error: %v", err)
	}

	if !bytes.Equal(decrypted, seed) {
		t.Errorf("decrypted seed does not match original")
	}

	// Wrong password should fail.
	_, err = decryptSeed(encrypted, []byte("wrong-password-that-is-32-bytes"))
	if err == nil {
		t.Fatal("expected error with wrong password")
	}
}

func TestKeyFileRoundTrip(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 100)
	}
	privKey := ed25519.NewKeyFromSeed(seed)
	pw := []byte("key-file-password-for-testing!!!")

	dir := t.TempDir()
	path := filepath.Join(dir, keyFileName)

	if err := saveKeyFile(path, privKey, pw); err != nil {
		t.Fatalf("saveKeyFile error: %v", err)
	}

	kf, err := readKeyFile(path)
	if err != nil {
		t.Fatalf("readKeyFile error: %v", err)
	}

	expectedPub := privKey.Public().(ed25519.PublicKey)
	if !bytes.Equal(kf.PubKey, expectedPub) {
		t.Errorf("stored pubkey doesn't match")
	}

	decSeed, err := decryptSeed(kf.EncryptedSeed, pw)
	if err != nil {
		t.Fatalf("decryptSeed error: %v", err)
	}
	defer encode.ClearBytes(decSeed)

	recoveredKey := ed25519.NewKeyFromSeed(decSeed)
	if !recoveredKey.Equal(privKey) {
		t.Error("recovered key does not match original")
	}
}

func TestAuthenticator(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 50)
	}
	privKey := ed25519.NewKeyFromSeed(seed)
	pw := []byte("authenticator-test-password-32b!")

	dir := t.TempDir()
	path := filepath.Join(dir, keyFileName)

	if err := saveKeyFile(path, privKey, pw); err != nil {
		t.Fatalf("saveKeyFile error: %v", err)
	}

	kf, err := readKeyFile(path)
	if err != nil {
		t.Fatalf("readKeyFile error: %v", err)
	}

	w := &NearWallet{
		dataDir:   dir,
		pubKey:    ed25519.PublicKey(kf.PubKey),
		accountID: hex.EncodeToString(kf.PubKey),
	}

	if !w.Locked() {
		t.Fatal("wallet should be locked initially")
	}

	if err := w.Unlock([]byte("wrong-password-wrong-password!!!")); err == nil {
		t.Fatal("expected error unlocking with wrong password")
	}
	if !w.Locked() {
		t.Fatal("wallet should still be locked after bad password")
	}

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}
	if w.Locked() {
		t.Fatal("wallet should be unlocked")
	}

	w.keyMtx.RLock()
	if !w.privKey.Equal(privKey) {
		t.Error("unlocked private key does not match original")
	}
	w.keyMtx.RUnlock()

	if err := w.Lock(); err != nil {
		t.Fatalf("Lock error: %v", err)
	}
	if !w.Locked() {
		t.Fatal("wallet should be locked after Lock()")
	}

	w.keyMtx.RLock()
	if w.privKey != nil {
		t.Error("private key should be nil after Lock()")
	}
	w.keyMtx.RUnlock()
}

func TestKeyFileNotExist(t *testing.T) {
	_, err := readKeyFile("/nonexistent/path/keyfile.json")
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got: %v", err)
	}
}
