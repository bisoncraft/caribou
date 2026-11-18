//go:build harness

package dash

// A check of the shared masternode wiring against the harness: the wallet
// builds its Manager on Connect from the real electrum client, a locked wallet
// can still refresh once the owner keys are cached, and an output the wallet has
// set aside with a lease is locked in its balance.
//
// Prerequisites are those of electrum_simnet_test.go.
//
//	go test -tags harness ./wallet/asset/dash/ -run TestDashSharedWallet

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/utxowallet/wtxmgr"
)

func TestDashSharedWalletWiring(t *testing.T) {
	alpha, alphaCM := newDashAlphaBackend(t)
	defer alphaCM.Disconnect()
	unlock(t, alpha)

	w, cm := newDashElectrumWallet(t)
	defer cm.Disconnect()
	dw, ok := w.(*dashWallet)
	if !ok {
		t.Fatalf("the electrum wallet on regtest is a %T", w)
	}
	waitDashSynced(t, w)

	mgr, err := dw.manager()
	if err != nil {
		t.Fatalf("no manager: %v", err)
	}

	// Unlocking caches the owner key ids, so that a locked wallet can scan. The
	// harness has shared registrations of other wallets, which a scan has to
	// match against keys it must not need to derive.
	unlock(t, w)
	if need, err := mgr.NeedsKeys(); err != nil || need {
		t.Fatalf("keys are not cached after the unlock: %v %v", need, err)
	}
	if err := dw.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh with the wallet locked: %v", err)
	}
	unlock(t, w)
	idx, id, err := mgr.ReserveOwner()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := mgr.OwnerIndex(id); !ok || got != idx {
		t.Fatalf("owner index of the reserved id: %d %v", got, ok)
	}

	// A leased output leaves what is available and is locked.
	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatal(err)
	}
	const fundAmt = 10
	if _, err := alpha.Send(addr, toSatoshiDash(fundAmt), 10); err != nil {
		t.Fatal(err)
	}
	if err := mineDashAlpha(); err != nil {
		t.Fatal(err)
	}
	before := waitDashBalanceAtLeast(t, w, toSatoshiDash(fundAmt))

	inner := dw.InnerWallet()
	utxos, err := inner.ListUnspent(1, 9999999, "")
	if err != nil || len(utxos) == 0 {
		t.Fatalf("no unspent output: %v", err)
	}
	var biggest = utxos[0]
	for _, u := range utxos {
		if u.Amount > biggest.Amount {
			biggest = u
		}
	}
	txHash, err := chainhash.NewHashFromStr(biggest.TxID)
	if err != nil {
		t.Fatal(err)
	}
	op := wire.OutPoint{Hash: *txHash, Index: biggest.Vout}
	if _, err := inner.LeaseOutput(wtxmgr.LockID{1}, op, time.Hour); err != nil {
		t.Fatal(err)
	}
	leasedAmt, err := btcutil.NewAmount(biggest.Amount)
	if err != nil {
		t.Fatal(err)
	}
	leased := uint64(leasedAmt)
	after, err := w.Balance()
	if err != nil {
		t.Fatal(err)
	}
	if after.Locked != before.Locked+leased || after.Available != before.Available-leased {
		t.Fatalf("before %+v, after %+v, leased %d", before, after, leased)
	}
	if err := inner.ReleaseOutput(wtxmgr.LockID{1}, op); err != nil {
		t.Fatal(err)
	}
	if again, err := w.Balance(); err != nil || again.Locked != before.Locked {
		t.Fatalf("after the release: %+v %v", again, err)
	}
}
