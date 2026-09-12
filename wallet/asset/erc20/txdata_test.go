// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package erc20

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestParseTransferData(t *testing.T) {
	recipient := common.HexToAddress("0x1111111111111111111111111111111111111111")
	amount := new(big.Int).Lsh(big.NewInt(1), 100)
	data, err := ERC20ABI.Pack("transfer", recipient, amount)
	if err != nil {
		t.Fatal(err)
	}
	gotRecipient, gotAmount, err := ParseTransferData(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotRecipient != recipient || gotAmount.Cmp(amount) != 0 {
		t.Fatalf("transfer = (%s, %s), want (%s, %s)", gotRecipient, gotAmount, recipient, amount)
	}
	for _, bad := range [][]byte{nil, data[:3], append(data, 0), append(data, make([]byte, 32)...)} {
		if _, _, err := ParseTransferData(bad); err == nil {
			t.Fatalf("ParseTransferData accepted malformed data %x", bad)
		}
	}
}

func TestParseTransferFromData(t *testing.T) {
	sender := common.HexToAddress("0x2222222222222222222222222222222222222222")
	recipient := common.HexToAddress("0x3333333333333333333333333333333333333333")
	amount := new(big.Int).Lsh(big.NewInt(1), 100)
	data, err := ERC20ABI.Pack("transferFrom", sender, recipient, amount)
	if err != nil {
		t.Fatal(err)
	}
	gotSender, gotRecipient, gotAmount, err := ParseTransferFromData(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotSender != sender || gotRecipient != recipient || gotAmount.Cmp(amount) != 0 {
		t.Fatalf("transferFrom = (%s, %s, %s)", gotSender, gotRecipient, gotAmount)
	}
	if _, _, _, err := ParseTransferFromData(data[:len(data)-32]); err == nil {
		t.Fatal("ParseTransferFromData accepted missing argument")
	}
	if _, _, _, err := ParseTransferFromData(append(data, make([]byte, 32)...)); err == nil {
		t.Fatal("ParseTransferFromData accepted extra argument")
	}
	if _, _, err := ParseTransferData(data); err == nil {
		t.Fatal("ParseTransferData accepted transferFrom calldata")
	}
}
