// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"errors"
	"math/big"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func finTransferLog(t *testing.T, tokenAddress, recipient common.Address, amt *big.Int) *types.Log {
	t.Helper()
	data, err := omniFinTransferArgs.Pack(tokenAddress, amt, recipient, "fee.near")
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return &types.Log{
		Topics: []common.Hash{
			omniFinTransferTopic,
			{}, // originChain (indexed)
			{}, // originNonce (indexed)
		},
		Data: data,
	}
}

func TestParseNativeBridgePayout(t *testing.T) {
	us := common.HexToAddress("0x1000000000000000000000000000000000000001")
	them := common.HexToAddress("0x2000000000000000000000000000000000000002")
	token := common.HexToAddress("0x3000000000000000000000000000000000000003")
	wei := big.NewInt(1e15)

	receipt := func(logs ...*types.Log) *types.Receipt { return &types.Receipt{Logs: logs} }

	if got, ok := parseNativeBridgePayout(receipt(finTransferLog(t, common.Address{}, us, wei)), us); !ok || got.Cmp(wei) != 0 {
		t.Fatalf("native payout to us: got %v, %v", got, ok)
	}

	// Two payouts in one tx sum.
	if got, ok := parseNativeBridgePayout(receipt(
		finTransferLog(t, common.Address{}, us, wei),
		finTransferLog(t, common.Address{}, us, wei),
	), us); !ok || got.Cmp(new(big.Int).Add(wei, wei)) != 0 {
		t.Fatalf("summed payouts: got %v, %v", got, ok)
	}

	// A payout to someone else is not ours.
	if _, ok := parseNativeBridgePayout(receipt(finTransferLog(t, common.Address{}, them, wei)), us); ok {
		t.Fatal("attributed a payout to another recipient")
	}

	// A token payout is delivered by the token contract, not this event.
	if _, ok := parseNativeBridgePayout(receipt(finTransferLog(t, token, us, wei)), us); ok {
		t.Fatal("attributed a token payout as native")
	}

	// An unrelated event is ignored.
	unrelated := finTransferLog(t, common.Address{}, us, wei)
	unrelated.Topics[0] = common.HexToHash("0xdeadbeef")
	if _, ok := parseNativeBridgePayout(receipt(unrelated), us); ok {
		t.Fatal("attributed an unrelated event")
	}
}

// TestIncomingTxBaseUnsettled guards the provider states that arrive with a
// nil error: a nil receipt, and a receipt not yet bound to a block. Both must
// read as not-found — the recording retry loop keys on CoinNotFoundError —
// rather than dereferencing nil in a background goroutine.
func TestIncomingTxBaseUnsettled(t *testing.T) {
	w := &assetWallet{}
	if _, err := w.incomingTxBase(common.Hash{}, nil, 0); !errors.Is(err, asset.CoinNotFoundError) {
		t.Fatalf("nil receipt: err = %v, want CoinNotFoundError", err)
	}
	unbound := &types.Receipt{Status: types.ReceiptStatusSuccessful}
	if _, err := w.incomingTxBase(common.Hash{}, unbound, 0); !errors.Is(err, asset.CoinNotFoundError) {
		t.Fatalf("nil block number: err = %v, want CoinNotFoundError", err)
	}
}
