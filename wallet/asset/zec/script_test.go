// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org

package zec

import "testing"

func TestTransparentTxFeesZIP317(t *testing.T) {
	// Below grace actions floor: still charged for 2 actions.
	if got := TransparentTxFeesZIP317(1, 1); got != 2*5000 {
		t.Errorf("TransparentTxFeesZIP317(1,1) = %d, want %d", got, 2*5000)
	}
	// Larger inputs/outputs than the grace floor.
	got := TransparentTxFeesZIP317(150*3, 34*3)
	if got != 3*5000 {
		t.Errorf("TransparentTxFeesZIP317(450,102) = %d, want %d", got, 3*5000)
	}
}

func TestTxFeesZIP317(t *testing.T) {
	tests := []struct {
		name                                                                     string
		txInSize, txOutSize, nSpendSapling, nOutSapling, nJoinSplit, nActOrchard uint64
		want                                                                     uint64
	}{
		{"all zero, floor to grace actions", 0, 0, 0, 0, 0, 0, 2 * 5000},
		{"sapling spends dominate", 0, 0, 5, 1, 0, 0, 5 * 5000},
		{"orchard actions dominate", 0, 0, 0, 0, 0, 4, 4 * 5000},
		{"join split doubles", 0, 0, 0, 0, 3, 0, 6 * 5000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TxFeesZIP317(tt.txInSize, tt.txOutSize, tt.nSpendSapling, tt.nOutSapling, tt.nJoinSplit, tt.nActOrchard)
			if got != tt.want {
				t.Errorf("TxFeesZIP317() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRequiredOrderFunds(t *testing.T) {
	const swapVal = 1e8
	single := RequiredOrderFunds(swapVal, 1, 150, 1)
	if single <= swapVal {
		t.Errorf("RequiredOrderFunds(maxSwaps=1) = %d, should exceed swapVal %d", single, uint64(swapVal))
	}

	multi := RequiredOrderFunds(swapVal, 1, 150, 3)
	if multi <= single {
		t.Errorf("RequiredOrderFunds(maxSwaps=3) = %d, should exceed maxSwaps=1 result %d", multi, single)
	}
}

func TestMinHTLCSize(t *testing.T) {
	// Larger redeem fees should increase the minimum HTLC size by exactly
	// the fee difference.
	base := MinHTLCSize(0)
	withFee := MinHTLCSize(1000)
	if withFee != base+1000 {
		t.Errorf("MinHTLCSize(1000) - MinHTLCSize(0) = %d, want 1000", withFee-base)
	}
	if base == 0 {
		t.Errorf("MinHTLCSize(0) = 0, want > 0")
	}
}

func TestMinLotSize(t *testing.T) {
	if got := MinLotSize(1000); got == 0 {
		t.Errorf("MinLotSize(1000) = 0, want > 0")
	}
}
