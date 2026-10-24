// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package wallet

import (
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/wire"
)

// quarterlyCheckpoints returns params with checkpoints at heights 1000, 2000
// and 3000, mined 90 days apart starting at first.
func quarterlyCheckpoints(first time.Time) *netparams.ChainParams {
	cps := make([]netparams.Checkpoint, 3)
	for i := range cps {
		cps[i] = netparams.Checkpoint{
			Height: int32(i+1) * 1000,
			Header: &wire.BlockHeader{Timestamp: first.Add(time.Duration(i) * 90 * 24 * time.Hour)},
		}
	}
	return &netparams.ChainParams{Checkpoints: cps}
}

func TestAnchorCheckpoint(t *testing.T) {
	first := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC)
	params := quarterlyCheckpoints(first)
	second := params.Checkpoints[1].Header.Timestamp
	afterNewest := first.Add(365 * 24 * time.Hour)
	block := func(height int32) *waddrmgr.BlockStamp {
		return &waddrmgr.BlockStamp{Height: height}
	}

	tests := []struct {
		name          string
		params        *netparams.ChainParams
		birthday      time.Time
		birthdayBlock *waddrmgr.BlockStamp
		want          int32 // -1 for no anchor
	}{
		{"no checkpoints", &netparams.ChainParams{}, afterNewest, nil, -1},
		{"birthday before the first checkpoint", params, first.Add(-24 * time.Hour), nil, 1000},
		{"birthday a delta after a checkpoint", params, second.Add(birthdayBlockDelta), nil, 2000},
		{"birthday within a delta of a checkpoint", params, second.Add(birthdayBlockDelta - time.Second), nil, 1000},
		{"birthday after the newest checkpoint", params, afterNewest, nil, 3000},
		{"birthday block at the newest checkpoint", params, afterNewest, block(3000), 3000},
		{"birthday block caps the anchor", params, afterNewest, block(2999), 2000},
		{"time test applies under the birthday block", params, second.Add(time.Hour), block(5000), 1000},
		{"birthday block below the first checkpoint", params, afterNewest, block(500), 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cp := anchorCheckpoint(tc.params, tc.birthday, tc.birthdayBlock)
			got := int32(-1)
			if cp != nil {
				got = cp.Height
			}
			if got != tc.want {
				t.Fatalf("anchor = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestHeaderAnchor checks that the wallet caps the anchor at its stored
// birthday block once one is set.
func TestHeaderAnchor(t *testing.T) {
	w, cleanup := testWallet(t)
	defer cleanup()

	// The newest checkpoint is two days older than the birthday, so the
	// time test alone selects it.
	w.chainParams = quarterlyCheckpoints(w.Manager.Birthday().Add(-182 * 24 * time.Hour))
	anchor, err := w.headerAnchor()
	if err != nil {
		t.Fatalf("headerAnchor: %v", err)
	}
	if anchor == nil || anchor.Height != 3000 {
		t.Fatalf("anchor without a birthday block = %v, want height 3000", anchor)
	}

	err = walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return w.Manager.SetBirthdayBlock(ns, waddrmgr.BlockStamp{Height: 2500}, true)
	})
	if err != nil {
		t.Fatalf("SetBirthdayBlock: %v", err)
	}
	anchor, err = w.headerAnchor()
	if err != nil {
		t.Fatalf("headerAnchor: %v", err)
	}
	if anchor == nil || anchor.Height != 2000 {
		t.Fatalf("anchor under birthday block 2500 = %v, want height 2000", anchor)
	}
}

// TestNewElectrumBackendAnchor checks that the anchor sets the height the
// electrum header store starts at.
func TestNewElectrumBackendAnchor(t *testing.T) {
	np := assets.BTCParams["mainnet"]
	newest := &np.Checkpoints[len(np.Checkpoints)-1]
	for _, tc := range []struct {
		name   string
		anchor *netparams.Checkpoint
		want   int32
	}{
		{"no anchor", nil, np.Checkpoints[0].Height},
		{"newest checkpoint", newest, newest.Height},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := newElectrumBackend(np, tc.anchor, t.TempDir(), "")
			if err != nil {
				t.Fatalf("newElectrumBackend: %v", err)
			}
			if got := b.(*electrumWalletBackend).EarliestBlockHeight(); got != tc.want {
				t.Fatalf("EarliestBlockHeight = %d, want %d", got, tc.want)
			}
		})
	}
}
