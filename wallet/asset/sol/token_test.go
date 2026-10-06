// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/decred/base58"
)

func newTestTokenWallet(t *testing.T, rpc rpcI, db txdb.NetworkTxDB) *TokenWallet {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x05}, 32))
	pub := priv.Public().(ed25519.PublicKey)
	addr := base58.Encode(pub)
	parent := &Wallet{
		address: addr,
		pubKey:  pub,
		privKey: priv,
		txDB:    db,
		log:     util.Disabled,
	}
	parent.conn.Store(&conn{rpc: rpc, ctx: context.Background()})
	var owner [32]byte
	copy(owner[:], pub)
	mint := mustDecodePubKey(TokenUSDC.NetMints[util.Mainnet])
	ata, err := deriveATA(owner, mint)
	if err != nil {
		t.Fatalf("deriveATA: %v", err)
	}
	return &TokenWallet{
		parent:  parent,
		assetID: usdcTokenID,
		token:   TokenUSDC,
		mint:    mint,
		owner:   owner,
		ata:     ata,
		ataAddr: base58.Encode(ata[:]),
		txDB:    db,
	}
}

func TestTokenWalletDepositAddress(t *testing.T) {
	tw := newTestTokenWallet(t, &mockRPC{}, nil)
	addr, err := tw.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress error: %v", err)
	}
	// The deposit address must be the owner address, not the ATA: senders
	// derive the destination ATA from it themselves (matching Send and
	// ecosystem convention), so publishing the ATA would make senders
	// derive an ATA-of-ATA and strand funds.
	if addr != tw.parent.address {
		t.Fatalf("DepositAddress = %q, want %q", addr, tw.parent.address)
	}
	owns, err := tw.OwnsDepositAddress(addr)
	if err != nil || !owns {
		t.Fatalf("OwnsDepositAddress(%q) = %v, %v; want true, nil", addr, owns, err)
	}
	if owns2, _ := tw.OwnsDepositAddress(tw.ataAddr); owns2 {
		t.Fatal("token wallet should not own its own ATA as a deposit address")
	}
}

// TestTokenWalletConnectReportsPeers guards against a token wallet's Send
// being permanently rejected by Core's no-connected-peers safety check. The
// token wallet has no independent connectivity probe of its own, so it must
// report peers once on Connect using the parent's already-verified RPC
// connection, or Core's per-asset peer counter for the token never leaves
// its initial "unknown" state.
func TestTokenWalletConnectReportsPeers(t *testing.T) {
	tw := newTestTokenWallet(t, &mockRPC{}, nil)
	var gotNumPeers uint32
	var calls int
	tw.peersChange = func(numPeers uint32, err error) {
		calls++
		gotNumPeers = numPeers
		if err != nil {
			t.Fatalf("peersChange error: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg, err := tw.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	cancel()
	wg.Wait()

	if calls != 1 {
		t.Fatalf("expected peersChange to be called once, got %d calls", calls)
	}
	if gotNumPeers != 1 {
		t.Fatalf("expected peersChange(1, nil), got peersChange(%d, ...)", gotNumPeers)
	}
}

// TestMetaKeyDistinctPerAsset guards against the scan-cursor collision bug.
// The native wallet and every token wallet on this network share one meta
// table, and they all store the same field names, so the asset scope in the
// key is the only thing keeping native/usdc/usdt from overwriting each other's
// lastScannedSignature cursor. A collision here would not error: one wallet
// would silently resume from another's cursor and report wrong history.
func TestMetaKeyDistinctPerAsset(t *testing.T) {
	native := metaKey(BipID, lastScannedSigField)
	usdc := metaKey(usdcTokenID, lastScannedSigField)
	usdt := metaKey(usdtTokenID, lastScannedSigField)

	for _, c := range []struct {
		a, b []byte
		desc string
	}{
		{usdc, usdt, "usdc vs usdt"},
		{usdc, native, "usdc vs native"},
		{usdt, native, "usdt vs native"},
	} {
		if bytes.Equal(c.a, c.b) {
			t.Errorf("%s meta keys collide, both = %x", c.desc, c.a)
		}
	}

	// Distinct fields for one asset must also not collide, and the asset scope
	// must not bleed into the next field: a same-length field name sharing a
	// prefix is the case a naive concatenation gets wrong.
	if bytes.Equal(metaKey(usdcTokenID, beforeCursorField), metaKey(usdcTokenID, backfillUntilField)) {
		t.Error("distinct fields for one asset collide")
	}
}

// TestUSDCNoTestnetMint guards against listing a mint for a cluster it
// doesn't actually exist on: Circle's only public test mint is on the
// devnet cluster, but DefaultRPCEndpoints[util.Testnet] points at the
// testnet cluster, so a util.Testnet mint entry here would make USDC dead
// on arrival there (balance always empty, sends rejected) instead of
// simply being unavailable, as OpenTokenWallet reports when unlisted.
func TestUSDCNoTestnetMint(t *testing.T) {
	if _, ok := TokenUSDC.NetMints[util.Testnet]; ok {
		t.Fatal("TokenUSDC must not list a util.Testnet mint: Circle has no public mint on the testnet cluster")
	}

	w := &Wallet{net: util.Testnet}
	if _, err := w.OpenTokenWallet(&asset.TokenConfig{AssetID: usdcTokenID}); err == nil {
		t.Fatal("expected OpenTokenWallet to fail for usdc on testnet (no mint), not silently succeed")
	}
}

func TestTokenWalletBalance(t *testing.T) {
	rpc := &mockRPC{tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 42_000_000, true, nil }}
	tw := newTestTokenWallet(t, rpc, nil)
	bal, err := tw.Balance()
	if err != nil {
		t.Fatalf("Balance error: %v", err)
	}
	if bal.Available != 42_000_000 {
		t.Fatalf("Available = %d, want 42000000", bal.Available)
	}
}

func TestTokenWalletBalanceNoAccount(t *testing.T) {
	rpc := &mockRPC{tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 0, false, nil }}
	tw := newTestTokenWallet(t, rpc, nil)
	bal, err := tw.Balance()
	if err != nil {
		t.Fatalf("Balance error: %v", err)
	}
	if bal.Available != 0 {
		t.Fatalf("Available = %d, want 0 for a token account that has never received funds", bal.Available)
	}
}

func TestTokenWalletSend(t *testing.T) {
	recipientPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x06}, 32)).Public().(ed25519.PublicKey)
	recipientAddr := base58.Encode(recipientPub)

	var sentTxB []byte
	blockhash := base58.Encode(bytes.Repeat([]byte{0x08}, 32))
	rpc := &mockRPC{
		tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 10_000_000, true, nil },
		blockhashFunc:    func() (string, uint64, error) { return blockhash, 1000, nil },
		balanceFunc:      func() (uint64, error) { return 1_000_000_000, nil },
		sendTxFunc: func(txB []byte) (string, error) {
			sentTxB = txB
			return "sig", nil
		},
	}
	db := &mockTxDB{}
	tw := newTestTokenWallet(t, rpc, db)

	coin, err := tw.Send(recipientAddr, 1_000_000, 0)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if coin.Value() != 1_000_000 {
		t.Fatalf("coin value = %d, want 1000000", coin.Value())
	}
	if len(sentTxB) == 0 {
		t.Fatal("expected a transaction to be submitted")
	}
	if len(db.stored) != 1 {
		t.Fatalf("expected 1 stored tx, got %d", len(db.stored))
	}
	if db.stored[0].Type != txdb.Send {
		t.Fatalf("expected Send tx type, got %v", db.stored[0].Type)
	}
	if db.stored[0].Amount != 1_000_000 {
		t.Fatalf("stored amount = %d, want 1000000", db.stored[0].Amount)
	}
	// The token's own AssetID must be set: token wallets share their parent
	// network's NetworkTxDB, and GetAssetTxs/GetAssetPendingTxs rely on it to
	// distinguish this record from the native wallet's (and sibling
	// tokens') transactions.
	if db.stored[0].AssetID != usdcTokenID {
		t.Fatalf("stored AssetID = %v, want %d", db.stored[0].AssetID, usdcTokenID)
	}
}

func TestTokenWalletSendInsufficientRentForNewATA(t *testing.T) {
	recipientPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x06}, 32)).Public().(ed25519.PublicKey)
	recipientAddr := base58.Encode(recipientPub)

	ownerPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x05}, 32)).Public().(ed25519.PublicKey)
	var owner [32]byte
	copy(owner[:], ownerPub)
	mint := mustDecodePubKey(TokenUSDC.NetMints[util.Mainnet])
	srcATA, err := deriveATA(owner, mint)
	if err != nil {
		t.Fatalf("deriveATA: %v", err)
	}
	srcATAAddr := base58.Encode(srcATA[:])

	blockhash := base58.Encode(bytes.Repeat([]byte{0x08}, 32))
	rpc := &mockRPC{
		tokenBalanceFunc: func(addr string) (uint64, bool, error) {
			if addr == srcATAAddr {
				return 10_000_000, true, nil
			}
			// The recipient's ATA does not exist yet, so a rent-exempt
			// reserve is required to create it.
			return 0, false, nil
		},
		rentFunc:      func(_ uint64) (uint64, error) { return 2_039_280, nil },
		blockhashFunc: func() (string, uint64, error) { return blockhash, 1000, nil },
		// Enough to cover the network fee alone, but not fee + rent.
		balanceFunc: func() (uint64, error) { return 6_000, nil },
	}
	tw := newTestTokenWallet(t, rpc, &mockTxDB{})

	_, err = tw.Send(recipientAddr, 1_000_000, 0)
	if err == nil {
		t.Fatal("expected an error when SOL balance can't cover new ATA rent")
	}
}

func TestTokenWalletSendInsufficientTokenBalance(t *testing.T) {
	recipientPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x06}, 32)).Public().(ed25519.PublicKey)
	recipientAddr := base58.Encode(recipientPub)
	rpc := &mockRPC{tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 100, true, nil }}
	tw := newTestTokenWallet(t, rpc, &mockTxDB{})
	_, err := tw.Send(recipientAddr, 1_000_000, 0)
	if !errors.Is(err, asset.ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestTokenWalletSendInvalidAddress(t *testing.T) {
	rpc := &mockRPC{tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 10_000_000, true, nil }}
	tw := newTestTokenWallet(t, rpc, &mockTxDB{})
	if _, err := tw.Send("not-an-address", 1_000_000, 0); err == nil {
		t.Fatal("expected error for invalid recipient address")
	}
}

func TestWalletTransactionFromSPLRPC(t *testing.T) {
	ataAddr := base58.Encode(bytes.Repeat([]byte{0x11}, 32))
	other := base58.Encode(bytes.Repeat([]byte{0x22}, 32))

	tx := &rpcTransaction{
		Slot: 42,
		Meta: &rpcTransactionMeta{Fee: 5000},
	}
	tx.Transaction.Signatures = []string{"sig1"}
	tx.Transaction.Message.Instructions = []rpcInstruction{
		parsedSPLInstruction(other, ataAddr, 2_500_000),
	}

	wt := walletTransactionFromSPLRPC(tx, ataAddr, usdcTokenID)
	if wt == nil {
		t.Fatal("expected a parsed wallet transaction")
	}
	if wt.Type != txdb.Receive {
		t.Fatalf("Type = %v, want Receive", wt.Type)
	}
	if wt.Amount != 2_500_000 {
		t.Fatalf("Amount = %d, want 2500000", wt.Amount)
	}
}

func TestWalletTransactionFromSPLRPCIrrelevant(t *testing.T) {
	ataAddr := base58.Encode(bytes.Repeat([]byte{0x11}, 32))
	a := base58.Encode(bytes.Repeat([]byte{0x22}, 32))
	b := base58.Encode(bytes.Repeat([]byte{0x33}, 32))

	tx := &rpcTransaction{
		Slot: 42,
		Meta: &rpcTransactionMeta{Fee: 5000},
	}
	tx.Transaction.Signatures = []string{"sig1"}
	tx.Transaction.Message.Instructions = []rpcInstruction{
		parsedSPLInstruction(a, b, 2_500_000),
	}

	if wt := walletTransactionFromSPLRPC(tx, ataAddr, usdcTokenID); wt != nil {
		t.Fatal("expected nil for a transaction not touching our ATA")
	}
}

// TestTokenWalletCheckPendingTxs guards against a token Send staying pending
// forever: before TokenWallet had its own checkPendingTxs, only syncHistory
// tracked confirmations, which never learns about a tx that expired without
// landing on-chain.
func TestTokenWalletCheckPendingTxs(t *testing.T) {
	ctx := context.Background()
	tokenID := usdcTokenID

	makePendingTx := func(id string, lastValidHeight uint64) *txdb.WalletTransaction {
		wt := &txdb.WalletTransaction{ID: id, NetworkID: BipID, AssetID: tokenID}
		setTxData(wt, solTxData{LastValidBlockHeight: lastValidHeight})
		return wt
	}

	t.Run("expired and absent on-chain marks tx rejected", func(t *testing.T) {
		tx := makePendingTx("tok-pend", 100)
		db := &mockTxDB{pending: []*txdb.WalletTransaction{tx}}
		rpc := &mockRPC{
			txFunc: func(_ string) (*rpcTransaction, error) {
				return nil, nil // confirmed absent
			},
			sigStatusesFunc: func(_ []string) ([]*signatureStatus, error) {
				return []*signatureStatus{nil}, nil
			},
			blockHeightFunc: func() (uint64, error) {
				return 200, nil // past LastValidBlockHeight
			},
		}
		tw := newTestTokenWallet(t, rpc, db)

		if err := tw.checkPendingTxs(ctx); err != nil {
			t.Fatal(err)
		}
		if len(db.stored) != 1 {
			t.Fatalf("want 1 stored (rejected), got %d", len(db.stored))
		}
		if !db.stored[0].Rejected {
			t.Fatal("tx should be marked Rejected")
		}
	})

	t.Run("still within window and no status leaves tx pending", func(t *testing.T) {
		tx := makePendingTx("tok-pend2", 100)
		db := &mockTxDB{pending: []*txdb.WalletTransaction{tx}}
		rpc := &mockRPC{
			sigStatusesFunc: func(_ []string) ([]*signatureStatus, error) {
				return []*signatureStatus{nil}, nil
			},
			blockHeightFunc: func() (uint64, error) {
				return 50, nil // still before LastValidBlockHeight
			},
		}
		tw := newTestTokenWallet(t, rpc, db)

		if err := tw.checkPendingTxs(ctx); err != nil {
			t.Fatal(err)
		}
		if len(db.stored) != 0 {
			t.Fatalf("tx should not have been stored while still within its valid window, got %d stored", len(db.stored))
		}
	})
}

// TestSharedMetaTableCursorsDoNotCollide exercises the accessors themselves,
// not just the key builder: the native wallet and two token wallets all share
// one meta table, so a token accessor that scoped its key to BipID instead of
// its own asset ID would compile, pass TestMetaKeyDistinctPerAsset, and still
// clobber the native cursor.
//
// The failure it guards against is silent. A wallet that reads back another
// asset's signature resumes its scan from the wrong point in the chain and
// reports incomplete or foreign transaction history, with no error anywhere.
func TestSharedMetaTableCursorsDoNotCollide(t *testing.T) {
	table := newMockTable()

	native := newTestWallet("nativeAddr", nil, nil, table)
	usdc := newTestTokenWallet(t, nil, nil)
	usdc.metaTable = table
	usdt := newTestTokenWallet(t, nil, nil)
	usdt.assetID = usdtTokenID
	usdt.metaTable = table

	if err := native.setLastScannedSignature("native-sig"); err != nil {
		t.Fatalf("native set: %v", err)
	}
	if err := usdc.setLastScannedSignature("usdc-sig"); err != nil {
		t.Fatalf("usdc set: %v", err)
	}
	if err := usdt.setLastScannedSignature("usdt-sig"); err != nil {
		t.Fatalf("usdt set: %v", err)
	}

	for _, c := range []struct {
		name string
		get  func() (string, error)
		want string
	}{
		{"native", native.getLastScannedSignature, "native-sig"},
		{"usdc", usdc.getLastScannedSignature, "usdc-sig"},
		{"usdt", usdt.getLastScannedSignature, "usdt-sig"},
	} {
		got, err := c.get()
		if err != nil {
			t.Fatalf("%s get: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s cursor = %q, want %q", c.name, got, c.want)
		}
	}

	// Three assets, three rows: no wallet overwrote another's.
	if len(table.data) != 3 {
		t.Errorf("meta table holds %d rows, want 3", len(table.data))
	}
}

// watchedAccounts is the set of accounts registered on c, for asserting where a
// watcher's registration currently sits.
func watchedAccounts(c *wsClient) []string {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	accounts := make([]string, 0, len(c.watched))
	for account := range c.watched {
		accounts = append(accounts, account)
	}
	return accounts
}

func wantWatched(t *testing.T, c *wsClient, what string, want ...string) {
	t.Helper()
	got := watchedAccounts(c)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s watches %v, want %v", what, got, want)
	}
}

// TestATAWatcherResync covers the registration following the parent from one
// client to the next. The parent installs a fresh client on every connect, and
// a token wallet outlives any one of them, so a registration left on a retired
// client is a wallet waiting on notifications nothing will ever send.
func TestATAWatcherResync(t *testing.T) {
	const ata = "token-account"
	var parent atomic.Pointer[wsClient]
	watcher := &ataWatcher{
		account:  ata,
		onChange: func() {},
		current:  parent.Load,
	}

	// No client yet, which is the ordinary state for a wallet whose parent has
	// no subscription endpoint configured. Nothing to register on, and nothing
	// may panic for the want of one.
	if got := watcher.resync(); got != nil {
		t.Fatalf("resync returned %p with no parent client, want nil", got)
	}

	first := newWSClient("", "", util.Disabled)
	parent.Store(first)
	if got := watcher.resync(); got != first {
		t.Fatalf("resync returned %p, want the parent's client %p", got, first)
	}
	wantWatched(t, first, "the first client", ata)

	// Idempotent: the loop calls this every pass, and re-registering each time
	// would churn subscribes on an unchanged connection.
	if got := watcher.resync(); got != first {
		t.Fatalf("second resync returned %p, want %p", got, first)
	}
	wantWatched(t, first, "the first client after a no-op resync", ata)

	// The parent reconnects. The registration has to move, and the old client
	// must be released — it may still be draining, and a registration it keeps
	// is a notification callback nobody is reading.
	second := newWSClient("", "", util.Disabled)
	parent.Store(second)
	if got := watcher.resync(); got != second {
		t.Fatalf("resync returned %p after the parent reconnected, want %p", got, second)
	}
	wantWatched(t, first, "the retired client")
	wantWatched(t, second, "the new client", ata)

	// The parent disconnects entirely, which is what Connect's leading
	// Store(nil) looks like from here.
	parent.Store(nil)
	if got := watcher.resync(); got != nil {
		t.Fatalf("resync returned %p after the parent dropped its client, want nil", got)
	}
	wantWatched(t, second, "the client the parent dropped")

	// stop is the deferred cleanup on the token wallet's own disconnect.
	third := newWSClient("", "", util.Disabled)
	parent.Store(third)
	watcher.resync()
	wantWatched(t, third, "the client at stop time", ata)
	watcher.stop()
	wantWatched(t, third, "the client after stop")
	// Idempotent too: a second stop must not unwatch on a client the watcher
	// has already released.
	watcher.stop()
}

// TestATAWatcherLivenessFollowsTheParent is the reason resync returns the
// client rather than the loop reading the parent's pointer itself. Liveness has
// to be read off the client actually carrying this wallet's registration. A
// loop that resynced but then measured the parent's current client would see a
// live connection this ATA is not subscribed on and hold the slow cadence,
// which is the failure the resync exists to prevent, arrived at by a different
// route.
func TestATAWatcherLivenessFollowsTheParent(t *testing.T) {
	const ata = "token-account"
	var parent atomic.Pointer[wsClient]
	watcher := &ataWatcher{
		account:  ata,
		onChange: func() {},
		current:  parent.Load,
	}

	live := newWSClient("", "", util.Disabled)
	parent.Store(live)
	cur := watcher.resync()
	// Set after registering, since watch recomputes live from the subscription
	// state. What is under test is which client the interval is read off, not
	// how a client earns live in the first place.
	live.live.Store(true)
	if got := refreshInterval(cur, false); got != subscribedPollInterval {
		t.Fatalf("interval on a live client is %v, want %v", got, subscribedPollInterval)
	}

	// A fresh client from a reconnect is not live until its subscribes land, so
	// the poll has to tighten back up. Reading the old client instead would
	// hold the slow cadence on a connection this ATA is no longer on.
	reconnected := newWSClient("", "", util.Disabled)
	parent.Store(reconnected)
	cur = watcher.resync()
	if cur != reconnected {
		t.Fatalf("resync returned %p, want the reconnected client %p", cur, reconnected)
	}
	if got := refreshInterval(cur, false); got != pollInterval {
		t.Fatalf("interval after a reconnect is %v, want %v", got, pollInterval)
	}

	// And with no client at all.
	parent.Store(nil)
	if got := refreshInterval(watcher.resync(), false); got != pollInterval {
		t.Fatalf("interval with no client is %v, want %v", got, pollInterval)
	}
}

func runTokenRefreshLoop(t *testing.T, wake <-chan struct{}, minGap time.Duration,
	interval func() time.Duration, pass func(context.Context)) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tokenRefreshLoop(ctx, wake, minGap, interval, pass)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("tokenRefreshLoop did not return on context cancellation")
		}
	}
}

// TestTokenRefreshLoopResyncsEveryPass pins the per-pass hook. The interval
// callback is where the watcher is resynced, so a loop that consulted it once
// and cached the result would leave the registration on whichever client the
// parent happened to have at Connect.
func TestTokenRefreshLoopResyncsEveryPass(t *testing.T) {
	var intervals atomic.Int64
	passes := make(chan struct{}, 8)
	stop := runTokenRefreshLoop(t, nil, 0,
		func() time.Duration {
			intervals.Add(1)
			return 10 * time.Millisecond
		},
		func(context.Context) { passes <- struct{}{} })
	defer stop()

	for i := range 3 {
		select {
		case <-passes:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for pass %d", i+1)
		}
	}
	if got := intervals.Load(); got < 3 {
		t.Fatalf("interval consulted %d times across 3 passes, want at least 3", got)
	}
}

// TestTokenRefreshLoopWakes covers the notification path: a wake runs a pass
// without waiting out the interval. The interval is an hour, so only a wake can
// drive this at all.
func TestTokenRefreshLoopWakes(t *testing.T) {
	wake := make(chan struct{}, 1)
	passes := make(chan struct{}, 4)
	stop := runTokenRefreshLoop(t, wake, 0,
		func() time.Duration { return time.Hour },
		func(context.Context) { passes <- struct{}{} })
	defer stop()

	for i := range 2 {
		signalRefresh(wake)
		select {
		case <-passes:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for pass %d", i+1)
		}
	}
	select {
	case <-passes:
		t.Fatal("loop ran a pass with no wake and no elapsed interval")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestTokenRefreshLoopTimesFromPassEnd is the property that separates this loop
// from refreshLoop. Each pass runs longer than the interval, so timing from the
// start would put the deadline in the past on every cycle and page a signature
// history back to back with no rest — which is what a token wallet must not do
// on a phone, having no tip age to protect in exchange.
func TestTokenRefreshLoopTimesFromPassEnd(t *testing.T) {
	const (
		interval = 150 * time.Millisecond
		passTime = 250 * time.Millisecond
		passes   = 3
	)
	gaps := make(chan time.Duration, passes)
	var lastEnd time.Time
	stop := runTokenRefreshLoop(t, nil, 0,
		func() time.Duration { return interval },
		func(context.Context) {
			if !lastEnd.IsZero() {
				gaps <- time.Since(lastEnd)
			}
			time.Sleep(passTime)
			lastEnd = time.Now()
		})
	defer stop()

	for i := range passes - 1 {
		select {
		case gap := <-gaps:
			if gap < interval {
				t.Fatalf("pass %d started %v after the previous one ended, want at "+
					"least %v of rest", i+2, gap, interval)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for gap %d", i+1)
		}
	}
}

// TestTokenRefreshLoopWakeSpacing covers the floor. A notification landing while
// a pass is running cannot be coalesced away — the loop has already drained the
// channel — so without a floor an ATA taking a burst of transfers would page
// its history back to back. The floor must cost nothing in the ordinary case: a
// wake arriving with the loop idle still runs at once.
func TestTokenRefreshLoopWakeSpacing(t *testing.T) {
	const minGap = 300 * time.Millisecond
	wake := make(chan struct{}, 1)
	ends := make(chan time.Time, 4)
	starts := make(chan time.Time, 4)
	var passNum int
	stop := runTokenRefreshLoop(t, wake, minGap,
		// An hour, so only a wake can drive this.
		func() time.Duration { return time.Hour },
		func(context.Context) {
			starts <- time.Now()
			passNum++
			if passNum == 1 {
				signalRefresh(wake)
				time.Sleep(20 * time.Millisecond)
			}
			ends <- time.Now()
		})
	defer stop()

	recv := func(ch <-chan time.Time, what string) time.Time {
		t.Helper()
		select {
		case v := <-ch:
			return v
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
			return time.Time{}
		}
	}

	signalRefresh(wake)
	recv(starts, "the first pass")
	firstEnd := recv(ends, "the first pass to finish")
	second := recv(starts, "the pass the mid-pass wake asked for")
	// From the end of the previous pass, which is this loop's anchor.
	if gap := second.Sub(firstEnd); gap < minGap {
		t.Fatalf("the next pass started %v after the previous one ended, want at "+
			"least %v; a wake taken during a pass is running the next one back "+
			"to back", gap, minGap)
	}
	recv(ends, "the second pass to finish")
	select {
	case <-starts:
		t.Fatal("loop ran a pass with no wake and no elapsed interval")
	case <-time.After(100 * time.Millisecond):
	}

	// Idle for longer than the floor, so the next wake owes it nothing.
	time.Sleep(minGap)
	signaled := time.Now()
	signalRefresh(wake)
	if delay := recv(starts, "the idle wake").Sub(signaled); delay >= minGap {
		t.Fatalf("wake took %v to run with the loop idle, want immediate; the "+
			"floor is being charged from the wake rather than from the last pass", delay)
	}
}

// TestTokenSyncHistorySharesParentBackoff proves the SOL rate-limit cooldown
// is shared across the native wallet and its token wallets: they all scan
// the same endpoint, so a 429 on any one of them must suppress the others,
// not just the wallet that hit it.
func TestTokenSyncHistorySharesParentBackoff(t *testing.T) {
	ctx := context.Background()

	t.Run("parent backoff blocks the token scan", func(t *testing.T) {
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				t.Fatal("token scan should not reach the RPC while the parent is backed off")
				return nil, nil
			},
		}
		tw := newTestTokenWallet(t, rpc, &mockTxDB{})
		tw.parent.scanBackoff.rateLimited(time.Now())

		if err := tw.syncHistory(ctx); err != nil {
			t.Fatalf("blocked syncHistory should return nil, got %v", err)
		}
	})

	t.Run("token 429 arms the parent backoff", func(t *testing.T) {
		rpc := &mockRPC{
			sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
				return nil, fmt.Errorf("getSignaturesForAddress: Too many requests for a specific RPC call (429)")
			},
		}
		tw := newTestTokenWallet(t, rpc, &mockTxDB{})
		// Give the parent its own txDB so its syncHistory does not
		// short-circuit on the nil-txDB check before reaching the backoff gate.
		tw.parent.txDB = &mockTxDB{}

		if err := tw.syncHistory(ctx); err == nil {
			t.Fatal("expected a 429 error")
		}
		if !tw.parent.scanBackoff.blocked(time.Now()) {
			t.Fatal("a token-wallet 429 should arm the shared parent backoff")
		}
		// If the gate did not hold, the parent scan would reach the same mock
		// RPC and surface its 429 error instead of returning nil.
		if err := tw.parent.syncHistory(ctx); err != nil {
			t.Fatalf("blocked native syncHistory should return nil, got %v", err)
		}
	})
}

func TestTokenWalletEstimateSendTxFee(t *testing.T) {
	recipientPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x06}, 32)).Public().(ed25519.PublicKey)
	recipientAddr := base58.Encode(recipientPub)

	blockhash := base58.Encode(bytes.Repeat([]byte{0x08}, 32))

	// Guards against pricing the fee-estimate message with a placeholder
	// all-zero blockhash: getFeeForMessage resolves the fee against a real,
	// recent blockhash, so a fake one silently degrades every estimate to
	// StandardSendFee — and Send (which does use the real blockhash) can then
	// reject with insufficient balance after the estimate said it was fine.
	t.Run("fee message carries the real blockhash", func(t *testing.T) {
		var gotMsg []byte
		rpc := &mockRPC{
			tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 10_000_000, true, nil },
			balanceFunc:      func() (uint64, error) { return 1_000_000_000, nil },
			blockhashFunc:    func() (string, uint64, error) { return blockhash, 1000, nil },
			feeForMsgFunc: func(msg []byte) (uint64, error) {
				gotMsg = msg
				return 6_000, nil
			},
		}
		tw := newTestTokenWallet(t, rpc, &mockTxDB{})

		if _, _, err := tw.EstimateSendTxFee(recipientAddr, 1_000_000, 0, false, false); err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if !bytes.Contains(gotMsg, base58.Decode(blockhash)) {
			t.Error("fee-pricing message does not contain the live blockhash")
		}
	})

	t.Run("existing destination ATA: fee only", func(t *testing.T) {
		rpc := &mockRPC{
			tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 10_000_000, true, nil },
			balanceFunc:      func() (uint64, error) { return 1_000_000_000, nil },
			feeForMsgFunc:    func(_ []byte) (uint64, error) { return 6_000, nil },
			blockhashFunc:    func() (string, uint64, error) { return blockhash, 1000, nil },
		}
		tw := newTestTokenWallet(t, rpc, &mockTxDB{})

		fee, valid, err := tw.EstimateSendTxFee(recipientAddr, 1_000_000, 0, false, false)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if !valid {
			t.Error("expected isValidAddress=true")
		}
		if fee != 6_000 {
			t.Errorf("fee = %d, want 6000 (no rent, ATA already exists)", fee)
		}
	})

	t.Run("missing destination ATA: fee plus rent", func(t *testing.T) {
		// newTestTokenWallet derives the owner from a fixed 0x05 seed; mirror
		// that here so this mock can tell the wallet's own ATA (has a
		// balance) from the recipient's (does not exist yet) by address.
		ownerPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x05}, 32)).Public().(ed25519.PublicKey)
		var owner [32]byte
		copy(owner[:], ownerPub)
		mint := mustDecodePubKey(TokenUSDC.NetMints[util.Mainnet])
		srcATA, err := deriveATA(owner, mint)
		if err != nil {
			t.Fatalf("deriveATA: %v", err)
		}
		srcATAAddr := base58.Encode(srcATA[:])

		rpc := &mockRPC{
			tokenBalanceFunc: func(addr string) (uint64, bool, error) {
				if addr == srcATAAddr {
					return 10_000_000, true, nil
				}
				return 0, false, nil // recipient's ATA does not exist yet
			},
			balanceFunc:   func() (uint64, error) { return 1_000_000_000, nil },
			feeForMsgFunc: func(_ []byte) (uint64, error) { return 6_000, nil },
			rentFunc:      func(_ uint64) (uint64, error) { return 2_039_280, nil },
			blockhashFunc: func() (string, uint64, error) { return blockhash, 1000, nil },
		}
		tw := newTestTokenWallet(t, rpc, &mockTxDB{})

		fee, valid, err := tw.EstimateSendTxFee(recipientAddr, 1_000_000, 0, false, false)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if !valid {
			t.Error("expected isValidAddress=true")
		}
		const want = 6_000 + 2_039_280
		if fee != want {
			t.Errorf("fee = %d, want %d (fee + new-ATA rent)", fee, want)
		}
	})

	t.Run("insufficient SOL for fee and rent", func(t *testing.T) {
		// Token balance is ample and the destination ATA is missing (needs
		// rent), so this isolates the SOL-side shortfall specifically —
		// distinct from "insufficient token balance" below.
		ownerPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x05}, 32)).Public().(ed25519.PublicKey)
		var owner [32]byte
		copy(owner[:], ownerPub)
		mint := mustDecodePubKey(TokenUSDC.NetMints[util.Mainnet])
		srcATA, err := deriveATA(owner, mint)
		if err != nil {
			t.Fatalf("deriveATA: %v", err)
		}
		srcATAAddr := base58.Encode(srcATA[:])

		rpc := &mockRPC{
			tokenBalanceFunc: func(addr string) (uint64, bool, error) {
				if addr == srcATAAddr {
					return 10_000_000, true, nil
				}
				return 0, false, nil // recipient's ATA does not exist yet
			},
			balanceFunc:   func() (uint64, error) { return 6_000, nil }, // covers fee alone, not rent
			feeForMsgFunc: func(_ []byte) (uint64, error) { return 6_000, nil },
			rentFunc:      func(_ uint64) (uint64, error) { return 2_039_280, nil },
			blockhashFunc: func() (string, uint64, error) { return blockhash, 1000, nil },
		}
		tw := newTestTokenWallet(t, rpc, &mockTxDB{})

		_, valid, err := tw.EstimateSendTxFee(recipientAddr, 1_000_000, 0, false, false)
		if err == nil {
			t.Fatal("expected an insufficient-SOL error")
		}
		if valid {
			t.Error("expected isValidAddress=false alongside the SOL-shortfall error")
		}
	})

	t.Run("insufficient token balance", func(t *testing.T) {
		rpc := &mockRPC{
			tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 100, true, nil },
			balanceFunc:      func() (uint64, error) { return 1_000_000_000, nil },
		}
		tw := newTestTokenWallet(t, rpc, &mockTxDB{})

		_, _, err := tw.EstimateSendTxFee(recipientAddr, 1_000_000, 0, false, false)
		if err != asset.ErrInsufficientBalance {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
	})

	t.Run("invalid address reported via isValidAddress, not an error", func(t *testing.T) {
		rpc := &mockRPC{
			balanceFunc:   func() (uint64, error) { return 1_000_000_000, nil },
			feeForMsgFunc: func(_ []byte) (uint64, error) { return 6_000, nil },
			blockhashFunc: func() (string, uint64, error) { return blockhash, 1000, nil },
		}
		tw := newTestTokenWallet(t, rpc, &mockTxDB{})
		// Own token balance is 0 by default (no tokenBalanceFunc), so send 0
		// to avoid tripping the balance check ahead of the address check.
		fee, valid, err := tw.EstimateSendTxFee("not-a-sol-address", 0, 0, false, false)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if valid {
			t.Error("expected isValidAddress=false")
		}
		if fee != 6_000 {
			t.Errorf("fee = %d, want 6000", fee)
		}
	})
}

// TestTokenWalletSyncHistoryEmitsBalanceChange guards against a detected
// incoming token transfer showing up in the transaction list while the
// displayed balance never updates: the front end doesn't re-poll balances
// on its own, so storing a new tx must also push a fresh BalanceChangeNote.
func TestTokenWalletSyncHistoryEmitsBalanceChange(t *testing.T) {
	notes := make(chan asset.WalletNotification, 4)
	otherAddr := base58.Encode(bytes.Repeat([]byte{0x22}, 32))

	rpc := &mockRPC{
		sigsFunc: func(_, _ *string, _ int) ([]signatureInfo, error) {
			return []signatureInfo{makeSig("sig")}, nil
		},
		tokenBalanceFunc: func(_ string) (uint64, bool, error) { return 2_500_000, true, nil },
	}
	tw := newTestTokenWallet(t, rpc, &mockTxDB{})
	// txFunc is set after construction since it needs tw.ataAddr as the
	// transfer's destination.
	rpc.txFunc = func(_ string) (*rpcTransaction, error) {
		tx := &rpcTransaction{Slot: 1, Meta: &rpcTransactionMeta{Fee: 5000}}
		tx.Transaction.Signatures = []string{"sig"}
		tx.Transaction.Message.Instructions = []rpcInstruction{
			parsedSPLInstruction(otherAddr, tw.ataAddr, 2_500_000),
		}
		return tx, nil
	}
	tw.emit = asset.NewWalletEmitter(notes, usdcTokenID, util.Disabled)

	if err := tw.syncHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-notes:
		bcn, ok := n.(*asset.BalanceChangeNote)
		if !ok {
			t.Fatalf("note type = %T, want *asset.BalanceChangeNote", n)
		}
		if bcn.Balance.Available != 2_500_000 {
			t.Errorf("emitted balance = %d, want 2500000", bcn.Balance.Available)
		}
	default:
		t.Fatal("expected a BalanceChangeNote after storing a new tx")
	}
}
