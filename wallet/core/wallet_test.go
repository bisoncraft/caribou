// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"errors"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// walletOnly embeds asset.Wallet by interface, not by concrete type, so it
// promotes only the asset.Wallet method set — even if the wrapped value's
// concrete type also implements asset.TrustlineEstablisher, walletOnly does
// not, letting the test exercise xcWallet.establishTrustline's not-a-
// TrustlineEstablisher branch with the same TXCWallet test double.
type walletOnly struct {
	asset.Wallet
}

// establishTrustline must delegate to the wallet's TrustlineEstablisher
// implementation when it has one, and be a harmless no-op otherwise — most
// asset wallets (anything that isn't an XRPL issued-currency token) don't
// require this opt-in step at all.
func TestEstablishTrustlineDelegatesWhenSupported(t *testing.T) {
	xcw, tw := newTWallet(0)
	tw.trustlineErr = errors.New("boom")
	if err := xcw.establishTrustline(); err == nil || err.Error() != "boom" {
		t.Fatalf("establishTrustline() = %v, want the underlying TrustlineEstablisher error", err)
	}
	if tw.trustlineCalls != 1 {
		t.Fatalf("EstablishTrustline called %d times, want 1", tw.trustlineCalls)
	}

	tw.trustlineErr = nil
	if err := xcw.establishTrustline(); err != nil {
		t.Fatalf("establishTrustline() = %v, want nil", err)
	}
}

func TestEstablishTrustlineNoOpForNonEstablisher(t *testing.T) {
	xcw, tw := newTWallet(0)
	tw.trustlineErr = errors.New("must not be reached")
	xcw.Wallet = walletOnly{tw}
	if err := xcw.establishTrustline(); err != nil {
		t.Fatalf("establishTrustline() = %v, want nil no-op for a non-TrustlineEstablisher wallet", err)
	}
	if tw.trustlineCalls != 0 {
		t.Fatalf("EstablishTrustline called %d times, want 0", tw.trustlineCalls)
	}
}

func TestProcessWalletTransactionsClearsPendingAlias(t *testing.T) {
	w := &xcWallet{
		pendingTxs: map[string]*txdb.WalletTransaction{
			"0x0000018bcfe56800": {
				ID: "0x0000018bcfe56800",
			},
		},
	}

	confirmed := &txdb.WalletTransaction{
		ID:        "0xabc123",
		Confirmed: true,
		AdditionalData: map[string]string{
			"pendingID": "0x0000018bcfe56800",
		},
	}

	w.processWalletTransactions([]*txdb.WalletTransaction{confirmed})

	if _, found := w.pendingTxs["0x0000018bcfe56800"]; found {
		t.Fatal("pending alias was not removed")
	}
	if _, found := w.pendingTxs["0xabc123"]; found {
		t.Fatal("confirmed tx should not remain in pending map")
	}
}

// Lock must not hold w.mtx across the backend lock RPC. Callers pass
// multi-second timeouts (walletLockTimeout), and holding mtx for the whole RPC
// would stall every state read and balance update on the wallet for that long.
func TestLockDoesNotHoldMtxAcrossBackendRPC(t *testing.T) {
	xcw, tw := newTWallet(0)

	release := make(chan struct{})
	entered := make(chan struct{})
	tw.lockHook = func() {
		close(entered)
		<-release
	}

	lockDone := make(chan error, 1)
	go func() { lockDone <- xcw.Lock(time.Minute) }()

	<-entered // the backend Lock is now parked

	// A reader needing w.mtx must not wait on the parked RPC.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		xcw.encPW()
		xcw.setEncPW([]byte{0x01})
	}()

	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("mtx reader blocked behind the backend lock RPC")
	}

	close(release)
	if err := <-lockDone; err != nil {
		t.Fatalf("Lock error: %v", err)
	}

	// The cached password must still be cleared, and only after the RPC
	// returned: pw.Clear zeroes a shared backing array in place, so it has to
	// happen under the write lock rather than alongside the RPC.
	xcw.mtx.RLock()
	defer xcw.mtx.RUnlock()
	if xcw.pw != nil {
		t.Fatal("cached password not cleared on successful Lock")
	}
}

// Lock must leave the cached password in place when the backend lock fails, so
// refreshUnlock can still recover the wallet.
func TestLockKeepsCachedPWOnError(t *testing.T) {
	xcw, tw := newTWallet(0)
	tw.lockErr = errors.New("boom")

	if err := xcw.Lock(time.Minute); err == nil {
		t.Fatal("expected Lock error")
	}
	xcw.mtx.RLock()
	defer xcw.mtx.RUnlock()
	if len(xcw.pw) == 0 {
		t.Fatal("cached password cleared despite a failed backend lock")
	}
}

// A Disconnect that lands while a Connect is in flight finds nothing live to
// cancel — ConnectionMaster.On is false until the connector's Connect returns —
// so without the connGen guard the connect would go on to mark the wallet
// hookedUp, leaving it connected past the Disconnect that meant to end it.
// Disconnect must also not block behind the in-flight Connect: Logout calls it,
// and Logout must never stall on a connect.
func TestConnectUnwindsWhenDisconnectRaces(t *testing.T) {
	xcw, tw := newTWallet(0)
	xcw.hookedUp = false

	release := make(chan struct{})
	entered := make(chan struct{})
	tw.connectHook = func() {
		close(entered)
		<-release
	}

	connectDone := make(chan error, 1)
	go func() { connectDone <- xcw.Connect() }()

	<-entered // the connect is now parked inside the connector

	disconnected := make(chan struct{})
	go func() {
		defer close(disconnected)
		xcw.Disconnect()
	}()

	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Disconnect blocked behind an in-flight Connect")
	}

	close(release)
	if err := <-connectDone; !errors.Is(err, errConnectRacedDisconnect) {
		t.Fatalf("expected errConnectRacedDisconnect, got %v", err)
	}
	if xcw.connected() {
		t.Fatal("wallet left connected past a Disconnect that raced the connect")
	}
	if xcw.connector.On() {
		t.Fatal("connector left running past a Disconnect that raced the connect")
	}
}

// The guard must only fire on a real race: an uncontested Connect still
// completes and leaves the wallet connected.
func TestConnectSucceedsWithoutRacingDisconnect(t *testing.T) {
	xcw, _ := newTWallet(0)
	xcw.hookedUp = false

	if err := xcw.Connect(); err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	if !xcw.connected() {
		t.Fatal("wallet not connected after an uncontested Connect")
	}
	xcw.Disconnect()
}
