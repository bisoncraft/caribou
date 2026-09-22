// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"bytes"
	"testing"

	"github.com/bisoncraft/caribou/util/encode"
	"github.com/ethereum/go-ethereum/common"
)

func TestFundingCoinID(t *testing.T) {
	t.Parallel()
	// Decode and encode fundingCoinID
	var address [20]byte
	copy(address[:], encode.RandomBytes(20))
	originalFundingCoin := &fundingCoin{
		addr: address,
		amt:  100,
	}
	encodedFundingCoin := originalFundingCoin.RecoveryID()
	decodedFundingCoin, err := decodeFundingCoin(encodedFundingCoin)
	if err != nil {
		t.Fatalf("unexpected error decoding swap coin: %v", err)
	}
	if !bytes.Equal(originalFundingCoin.addr[:], decodedFundingCoin.addr[:]) {
		t.Fatalf("expected address to be equal before and after decoding")
	}
	if originalFundingCoin.amt != decodedFundingCoin.amt {
		t.Fatalf("expected amount to be equal before and after decoding")
	}

	// Decode amount coin id with incorrect length
	fundingCoinID := make([]byte, 35)
	copy(fundingCoinID, encode.RandomBytes(35))
	if _, err := decodeFundingCoin(fundingCoinID); err == nil {
		t.Fatalf("expected error decoding amount coin ID with incorrect length")
	}
}

func TestFundingCoinAccessors(t *testing.T) {
	t.Parallel()
	addr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	c := createFundingCoin(addr, 100)
	if got := c.Value(); got != 100 {
		t.Errorf("Value() = %d, want 100", got)
	}
	if got := c.TxID(); got != "" {
		t.Errorf("TxID() = %q, want empty string", got)
	}
	if got := string(c.ID()); got != addr.String() {
		t.Errorf("ID() = %q, want %q", got, addr.String())
	}
}

func TestTokenFundingCoin(t *testing.T) {
	t.Parallel()
	addr := common.HexToAddress("0x2222222222222222222222222222222222222222")
	c := createTokenFundingCoin(addr, 100, 5)

	if got := c.Value(); got != 100 {
		t.Errorf("Value() = %d, want 100", got)
	}
	if got := c.Fees(); got != 5 {
		t.Errorf("Fees() = %d, want 5", got)
	}
	if got := c.TxID(); got != "" {
		t.Errorf("TxID() = %q, want empty string", got)
	}
	if got := string(c.ID()); got != addr.String() {
		t.Errorf("ID() = %q, want %q", got, addr.String())
	}

	encoded := c.RecoveryID()
	decoded, err := decodeTokenFundingCoin(encoded)
	if err != nil {
		t.Fatalf("unexpected error decoding token funding coin: %v", err)
	}
	if decoded.addr != c.addr || decoded.amt != c.amt || decoded.fees != c.fees {
		t.Fatalf("decoded %+v, want %+v", decoded, c)
	}

	if _, err := decodeTokenFundingCoin(make([]byte, tokenFundingCoinIDSize-1)); err == nil {
		t.Fatal("expected error decoding token funding coin ID with incorrect length")
	}
}
