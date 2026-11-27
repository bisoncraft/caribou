// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build zec && cgo && harness

package zeclib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sendWallet is a harness wallet with one account, its seed, and a way to
// make broadcasts fail.
type sendWallet struct {
	*harnessWallet
	account *Account
	seed    []byte

	// mtx guards failSends. While failSends is set, every broadcast the
	// wallet makes, by Send or by the sync loop, fails as if the server
	// could not be reached, and broadcasts counts the attempts.
	mtx        sync.Mutex
	failSends  bool
	broadcasts int
}

// errBroadcastBlocked is what a blocked broadcast returns.
var errBroadcastBlocked = errors.New("the test blocks broadcasts")

// flakySource is a chainSource whose broadcasts the test controls.
type flakySource struct {
	chainSource
	sw *sendWallet
}

func (f *flakySource) sendTransaction(ctx context.Context, raw []byte) error {
	f.sw.mtx.Lock()
	f.sw.broadcasts++
	fail := f.sw.failSends
	f.sw.mtx.Unlock()
	if fail {
		return errBroadcastBlocked
	}
	return f.chainSource.sendTransaction(ctx, raw)
}

func (sw *sendWallet) blockBroadcasts(block bool) {
	sw.mtx.Lock()
	sw.failSends = block
	sw.mtx.Unlock()
}

func (sw *sendWallet) broadcastCount() int {
	sw.mtx.Lock()
	defer sw.mtx.Unlock()
	return sw.broadcasts
}

// newSendWallet creates a wallet with a new account under the ZIP 315
// confirmations policy. It is not connected. Rebroadcast checks start one
// second after a send and are at most two seconds apart.
func newSendWallet(t *testing.T, name string) *sendWallet {
	t.Helper()
	hw := newHarnessWallet(t, func(c *Config) { c.MinConfirmations = 0 })
	sw := &sendWallet{harnessWallet: hw, seed: randomSeed(t)}
	dial := hw.w.dial
	hw.w.dial = func(e endpoint) (chainSource, error) {
		src, err := dial(e)
		if err != nil {
			return nil, err
		}
		return &flakySource{chainSource: src, sw: sw}, nil
	}
	hw.w.tune = func(o *syncOptions) {
		o.rebroadcastMinDelay = time.Second
		o.rebroadcastMaxDelay = 2 * time.Second
	}
	account, err := hw.w.CreateAccount(context.Background(), &AccountParams{Name: name, Seed: sw.seed})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	sw.account = account
	return sw
}

func (sw *sendWallet) address() string {
	sw.t.Helper()
	a, err := sw.w.Address(sw.account.ID)
	if err != nil {
		sw.t.Fatalf("Address: %v", err)
	}
	return a
}

func (sw *sendWallet) transparentAddress() string {
	sw.t.Helper()
	a, err := sw.w.TransparentReceiver(sw.address())
	if err != nil {
		sw.t.Fatalf("TransparentReceiver: %v", err)
	}
	return a
}

func (sw *sendWallet) spendable() SpendableValue {
	sw.t.Helper()
	v, err := sw.w.Spendable(sw.account.ID)
	if err != nil {
		sw.t.Fatalf("Spendable: %v", err)
	}
	return v
}

func (sw *sendWallet) balance(s *Summary) AccountBalance {
	b, _ := s.Balance(sw.account.ID)
	return b
}

// send pays an address with a fee limit of 0.01 ZEC.
func (sw *sendWallet) send(address string, zats uint64) *SendResult {
	sw.t.Helper()
	res, err := sw.w.Send(context.Background(), sw.account.ID, sw.seed, address, zats, nil, 1_000_000)
	if err != nil {
		sw.t.Fatalf("Send %d to %s: %v", zats, address, err)
	}
	sw.t.Logf("sent %d to %s: txids %v, fee %d, paid %s in %s, broadcast error %v",
		zats, address, res.TxIDs, res.Fee, res.PaidAddress, res.PaidPool, res.BroadcastErr)
	return res
}

// waitSpendable waits until ok accepts the account's spendable value, with
// the wallet synced to the alpha node's tip.
func (sw *sendWallet) waitSpendable(what string, ok func(SpendableValue) bool) SpendableValue {
	sw.t.Helper()
	var v SpendableValue
	sw.waitFor(what, func(s *Summary, st SyncStatus) bool {
		if st.State != SyncSynced || st.FullyScanned < harnessHeight(sw.t) {
			return false
		}
		v = sw.spendable()
		return ok(v)
	})
	return v
}

// waitInventory waits until the wallet lists n transactions for rebroadcast.
func (sw *sendWallet) waitInventory(n int) {
	sw.t.Helper()
	sw.waitFor(fmt.Sprintf("an inventory of %d", n), func(*Summary, SyncStatus) bool {
		inv, err := sw.w.be.rebroadcastInventory()
		return err == nil && len(inv) == n
	})
}

// nodeTx is a transaction as the alpha node decodes it.
type nodeTx struct {
	Height *uint32 `json:"height"`
	Expiry uint32  `json:"expiryheight"`
	Vin    []json.RawMessage
	Vout   []struct {
		ValueZat     uint64 `json:"valueZat"`
		ScriptPubKey struct {
			Addresses []string `json:"addresses"`
		} `json:"scriptPubKey"`
	} `json:"vout"`
	Sapling  []json.RawMessage                   `json:"vShieldedOutput"`
	Orchard  struct{ Actions []json.RawMessage } `json:"orchard"`
	Ironwood struct{ Actions []json.RawMessage } `json:"ironwood"`
}

// harnessTx returns a transaction as the alpha node knows it, and false if
// the node does not know it.
func harnessTx(t *testing.T, txid string) (*nodeTx, bool) {
	t.Helper()
	home, _ := os.UserHomeDir()
	cmd := exec.Command("./alpha", "getrawtransaction", txid, "1")
	cmd.Dir = filepath.Join(home, "dextest", "zec", "harness-ctl")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No such mempool or main chain transaction") ||
			strings.Contains(string(out), "error") {
			return nil, false
		}
		t.Fatalf("getrawtransaction %s: %v\n%s", txid, err, out)
	}
	var tx nodeTx
	if err := json.Unmarshal(out, &tx); err != nil {
		t.Fatalf("getrawtransaction %s: %v\n%s", txid, err, out)
	}
	return &tx, true
}

// waitNodeTx waits until the alpha node knows a transaction, and until it
// is mined if mined is set.
func waitNodeTx(t *testing.T, txid string, mined bool) *nodeTx {
	t.Helper()
	deadline := time.Now().Add(harnessTimeout)
	for {
		if tx, ok := harnessTx(t, txid); ok && (!mined || tx.Height != nil) {
			return tx
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node does not have transaction %s (mined %v)", txid, mined)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// fund pays the wallet from the alpha funder, mines the payments and waits
// until the wallet can spend them: Ironwood notes after 10 confirmations,
// transparent outputs at once.
func (sw *sendWallet) fund(shielded []uint64, transparent []uint64) {
	sw.t.Helper()
	for _, zats := range shielded {
		harnessSend(sw.t, sw.address(), zats)
	}
	for _, zats := range transparent {
		harnessSend(sw.t, sw.transparentAddress(), zats)
	}
	var wantIronwood, wantTransparent uint64
	for _, z := range shielded {
		wantIronwood += z
	}
	for _, z := range transparent {
		wantTransparent += z
	}
	blocks := 1
	if len(shielded) > 0 {
		blocks = 10
	}
	harnessMine(sw.t, blocks)
	sw.waitSpendable("the funds", func(v SpendableValue) bool {
		return v.Ironwood == wantIronwood && v.Transparent == wantTransparent
	})
}

// TestHarnessSendUnifiedAndTransparent pays a unified address and a
// transparent address from Ironwood notes, and compares the spendable value
// with the summary while the change of the sends matures.
func TestHarnessSendUnifiedAndTransparent(t *testing.T) {
	sw := newSendWallet(t, "sender")
	bystander := newSendWallet(t, "bystander")
	sw.connect()
	sw.fund([]uint64{100_000_000, 100_000_000}, nil)

	est, err := sw.w.EstimateTransfer(sw.account.ID, bystander.address(), 10_000_000, nil)
	if err != nil || est.Fee != 10_000 || est.PaidPool != PaidIronwood || est.PaidAddress != bystander.address() {
		t.Fatalf("estimate to the unified address: %+v, %v", est, err)
	}
	est, err = sw.w.EstimateTransfer(sw.account.ID, bystander.transparentAddress(), 5_000_000, nil)
	if err != nil || est.Fee != 15_000 || est.PaidPool != PaidTransparent {
		t.Fatalf("estimate to the transparent address: %+v, %v", est, err)
	}

	toUnified := sw.send(bystander.address(), 10_000_000)
	toTransparent := sw.send(bystander.transparentAddress(), 5_000_000)
	for _, res := range []*SendResult{toUnified, toTransparent} {
		if res.BroadcastErr != nil || len(res.TxIDs) != 1 {
			t.Fatalf("send: %+v", res)
		}
	}
	if toUnified.PaidPool != PaidIronwood || toUnified.PaidAddress != bystander.address() || toUnified.Fee != 10_000 {
		t.Errorf("send to the unified address: %+v", toUnified)
	}
	if toTransparent.PaidPool != PaidTransparent || toTransparent.PaidAddress != bystander.transparentAddress() || toTransparent.Fee != 15_000 {
		t.Errorf("send to the transparent address: %+v", toTransparent)
	}
	// Both are pending: their inputs are spent and nothing is spendable.
	if v := sw.spendable(); v.Ironwood != 0 {
		t.Errorf("spendable with both notes spent: %+v", v)
	}
	if inv, _ := sw.w.be.rebroadcastInventory(); len(inv) != 2 {
		t.Errorf("inventory of %d after two sends", len(inv))
	}

	harnessMine(t, 1)
	ua := waitNodeTx(t, toUnified.TxIDs[0], true)
	ta := waitNodeTx(t, toTransparent.TxIDs[0], true)
	if len(ua.Sapling) != 0 || len(ua.Orchard.Actions) != 0 || len(ua.Ironwood.Actions) != 2 || len(ua.Vout) != 0 {
		t.Errorf("send to the unified address as mined: %+v", ua)
	}
	if len(ta.Sapling) != 0 || len(ta.Vout) != 1 || ta.Vout[0].ValueZat != 5_000_000 ||
		ta.Vout[0].ScriptPubKey.Addresses[0] != bystander.transparentAddress() {
		t.Errorf("send to the transparent address as mined: %+v", ta)
	}
	sw.waitInventory(0)

	// The change matures. The spendable value is what selection can take,
	// which can lag the summary's spendable balance.
	change := uint64(100_000_000-10_010_000) + (100_000_000 - 5_015_000)
	for conf := 1; ; conf++ {
		s := sw.waitSynced(harnessHeight(t))
		v := sw.spendable()
		b := sw.balance(s)
		t.Logf("change at %d confirmations: spendable %d, summary spendable %d, change pending %d, value pending %d",
			conf, v.Ironwood, b.Ironwood.Spendable, b.Ironwood.ChangePending, b.Ironwood.ValuePending)
		if v.Ironwood > b.Ironwood.Spendable {
			t.Errorf("the spendable value exceeds the summary's at %d confirmations", conf)
		}
		if v.Ironwood == change {
			break
		}
		if conf > 6 {
			t.Fatalf("the change is not spendable after %d confirmations", conf)
		}
		harnessMine(t, 1)
	}
}

// TestHarnessSendUnifiedWithoutOrchard pays a unified address that has a
// Sapling and a transparent receiver and no Orchard receiver. The payment
// goes to the transparent receiver.
func TestHarnessSendUnifiedWithoutOrchard(t *testing.T) {
	sw := newSendWallet(t, "sender")
	sw.connect()
	sw.fund([]uint64{100_000_000}, nil)

	out := harnessCtl(t, "wallet-alpha", "generate-address", "--pool", "sapling", "--pool", "transparent")
	var ua string
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "uregtest1") {
			ua = field
		}
	}
	receiver, err := sw.w.TransparentReceiver(ua)
	if err != nil || receiver == "" {
		t.Fatalf("the transparent receiver of %s: %q, %v", ua, receiver, err)
	}
	est, err := sw.w.EstimateTransfer(sw.account.ID, ua, 2_000_000, nil)
	if err != nil || est.PaidPool != PaidTransparent || est.PaidAddress != receiver {
		t.Fatalf("estimate: %+v, %v", est, err)
	}
	if _, err := sw.w.EstimateTransfer(sw.account.ID, ua, 2_000_000, []byte("memo")); !errors.Is(err, ErrMemoNotAllowed) {
		t.Errorf("a memo for the transparent receiver: %v", err)
	}
	res := sw.send(ua, 2_000_000)
	if res.PaidPool != PaidTransparent || res.PaidAddress != receiver || res.BroadcastErr != nil {
		t.Errorf("send: %+v", res)
	}
	harnessMine(t, 1)
	tx := waitNodeTx(t, res.TxIDs[0], true)
	if len(tx.Sapling) != 0 || len(tx.Vout) != 1 || tx.Vout[0].ScriptPubKey.Addresses[0] != receiver || tx.Vout[0].ValueZat != 2_000_000 {
		t.Errorf("as mined: %+v", tx)
	}
}

// TestHarnessSendFromTransparent pays from a wallet that holds transparent
// outputs only. The transfer spends them as soon as the wallet knows of them
// and shields the change. An output of 5,000 zatoshis or less is never
// spendable.
func TestHarnessSendFromTransparent(t *testing.T) {
	sw := newSendWallet(t, "sender")
	bystander := newSendWallet(t, "bystander")
	sw.connect()
	sw.fund(nil, []uint64{50_000_000})

	res := sw.send(bystander.address(), 20_000_000)
	if res.BroadcastErr != nil || res.Fee != 15_000 {
		t.Fatalf("send: %+v", res)
	}
	harnessMine(t, 1)
	tx := waitNodeTx(t, res.TxIDs[0], true)
	if len(tx.Vin) != 1 || len(tx.Vout) != 0 || len(tx.Ironwood.Actions) != 2 {
		t.Errorf("as mined: %+v", tx)
	}
	sw.waitInventory(0)

	// An output at the marginal fee is counted as uneconomic by the summary
	// and is not spendable.
	harnessSend(t, sw.transparentAddress(), 5_000)
	harnessMine(t, 1)
	s := sw.waitFor("the output of 5,000", func(s *Summary, st SyncStatus) bool {
		return st.State == SyncSynced && sw.balance(s).Transparent.Uneconomic == 5_000
	})
	b := sw.balance(s)
	t.Logf("transparent balance with an output of 5,000: %+v; spendable %+v", b.Transparent, sw.spendable())
	if b.Transparent.Spendable != 0 || sw.spendable().Transparent != 0 {
		t.Errorf("an output of 5,000 counts as spendable: summary %+v, spendable %+v", b.Transparent, sw.spendable())
	}
}

// TestHarnessRebroadcast sends with the first broadcast failing. The sync
// loop broadcasts the transaction again and it is mined.
func TestHarnessRebroadcast(t *testing.T) {
	sw := newSendWallet(t, "sender")
	bystander := newSendWallet(t, "bystander")
	sw.connect()
	sw.fund([]uint64{100_000_000}, nil)

	sw.blockBroadcasts(true)
	res := sw.send(bystander.address(), 1_000_000)
	if !errors.Is(res.BroadcastErr, errBroadcastBlocked) || len(res.TxIDs) != 1 {
		t.Fatalf("send with the broadcast blocked: %+v", res)
	}
	if _, ok := harnessTx(t, res.TxIDs[0]); ok {
		t.Fatalf("the node has a transaction that was not broadcast")
	}
	sw.blockBroadcasts(false)
	tx := waitNodeTx(t, res.TxIDs[0], false)
	t.Logf("the node has the transaction after %d broadcasts; mined %v", sw.broadcastCount(), tx.Height != nil)
	harnessMine(t, 1)
	waitNodeTx(t, res.TxIDs[0], true)
	sw.waitInventory(0)
}

// TestHarnessSendMinedAgainAfterReorg mines a send and replaces its block.
// The send is mined again on the new chain, brought back either by the node
// itself or by the wallet's rebroadcast; the test logs which. The
// deterministic test of the wallet's own path is TestRebroadcastAfterRewind.
func TestHarnessSendMinedAgainAfterReorg(t *testing.T) {
	sw := newSendWallet(t, "sender")
	bystander := newSendWallet(t, "bystander")
	sw.connect()
	sw.fund([]uint64{100_000_000}, nil)

	res := sw.send(bystander.address(), 1_000_000)
	harnessMine(t, 1)
	first := waitNodeTx(t, res.TxIDs[0], true)
	sw.waitInventory(0)
	before := sw.broadcastCount()

	harnessCtl(t, "reorg", "1")
	tip := harnessHeight(t)
	t.Logf("mined at %d, chain replaced up to %d", *first.Height, tip)
	sw.waitFor("the send unmined or mined again", func(*Summary, SyncStatus) bool {
		inv, err := sw.w.be.rebroadcastInventory()
		if err == nil && len(inv) == 1 {
			return true
		}
		tx, ok := harnessTx(t, res.TxIDs[0])
		return ok && tx.Height != nil
	})
	// The node may have mined it again on its own. Otherwise the wallet
	// broadcasts it again, and the next block holds it.
	waitNodeTx(t, res.TxIDs[0], false)
	harnessMine(t, 1)
	again := waitNodeTx(t, res.TxIDs[0], true)
	sw.waitInventory(0)
	t.Logf("mined again at %d; the wallet broadcast it %d times after the reorg", *again.Height, sw.broadcastCount()-before)
}

// TestHarnessSendExpires sends a transaction that never reaches the node.
// Once the chain passes its expiry height the wallet stops listing it and
// its inputs are spendable again.
func TestHarnessSendExpires(t *testing.T) {
	sw := newSendWallet(t, "sender")
	bystander := newSendWallet(t, "bystander")
	sw.connect()
	sw.fund([]uint64{100_000_000}, nil)

	sw.blockBroadcasts(true)
	res := sw.send(bystander.address(), 1_000_000)
	inv, err := sw.w.be.rebroadcastInventory()
	if err != nil || len(inv) != 1 {
		t.Fatalf("inventory: %d, %v", len(inv), err)
	}
	expiry := inv[0].expiryHeight
	tip := harnessHeight(t)
	t.Logf("transaction %s expires at %d, tip %d", res.TxIDs[0], expiry, tip)
	if expiry <= tip {
		t.Fatalf("the transaction expires at %d, at or below the tip %d", expiry, tip)
	}
	if v := sw.spendable(); v.Ironwood != 0 {
		t.Fatalf("the input of a pending send is spendable: %+v", v)
	}

	// With the tip one block below the expiry height, the next block can
	// still hold the transaction.
	harnessMine(t, int(expiry-1-tip))
	sw.waitSynced(expiry - 1)
	if inv, _ := sw.w.be.rebroadcastInventory(); len(inv) != 1 {
		t.Errorf("not listed with the tip below its expiry height: %d", len(inv))
	}
	if v := sw.spendable(); v.Ironwood != 0 {
		t.Errorf("the input is spendable before the expiry: %+v", v)
	}
	// With the tip at the expiry height, no block can hold it: it leaves
	// the inventory and its input is spendable again.
	harnessMine(t, 1)
	sw.waitSpendable("the input released", func(v SpendableValue) bool { return v.Ironwood == 100_000_000 })
	if inv, _ := sw.w.be.rebroadcastInventory(); len(inv) != 0 {
		t.Errorf("listed with the tip at its expiry height: %d", len(inv))
	}
	if _, ok := harnessTx(t, res.TxIDs[0]); ok {
		t.Errorf("the node has the blocked transaction")
	}
	t.Logf("%d broadcasts were attempted", sw.broadcastCount())
	// The history marks it expired once the wallet has scanned past it.
	harnessMine(t, 1)
	sw.waitSynced(harnessHeight(t))
	r, _, err := sw.w.Transaction(sw.account.ID, res.TxIDs[0])
	if err != nil || r == nil || !r.ExpiredUnmined || r.MinedHeight != nil {
		t.Errorf("the expired transaction's record: %+v, %v", r, err)
	}
}

// sendMax sends everything to an address with a fee limit of 0.01 ZEC.
func (sw *sendWallet) sendMax(address string) *SendMaxResult {
	sw.t.Helper()
	res, err := sw.w.SendMax(context.Background(), sw.account.ID, sw.seed, address, nil, 1_000_000, 0)
	if err != nil {
		sw.t.Fatalf("SendMax to %s: %v", address, err)
	}
	sw.t.Logf("sent max %d to %s: txids %v, fee %d, left behind %+v, broadcast error %v",
		res.Amount, address, res.TxIDs, res.Fee, res.LeftBehind, res.BroadcastErr)
	return res
}

// TestHarnessSendMaxMixedToTransparent sends everything from a wallet with
// two Ironwood notes and a transparent output to a transparent address.
func TestHarnessSendMaxMixedToTransparent(t *testing.T) {
	sw := newSendWallet(t, "sender")
	bystander := newSendWallet(t, "bystander")
	sw.connect()
	sw.fund([]uint64{30_000_000, 20_000_000}, []uint64{10_000_000})

	est, err := sw.w.EstimateSendMax(sw.account.ID, bystander.transparentAddress(), nil)
	if err != nil {
		t.Fatalf("EstimateSendMax: %v", err)
	}
	// Two Ironwood actions for the two spends, and one transparent input
	// against one transparent output.
	const fee = 15_000
	if est.Fee != fee || est.Amount != 60_000_000-fee || est.PaidPool != PaidTransparent || est.LeftBehind != (LeftBehind{}) {
		t.Fatalf("estimate: %+v", est)
	}
	res := sw.sendMax(bystander.transparentAddress())
	if res.Amount != est.Amount || res.Fee != fee || res.BroadcastErr != nil {
		t.Fatalf("send max: %+v", res)
	}
	harnessMine(t, 1)
	tx := waitNodeTx(t, res.TxIDs[0], true)
	if len(tx.Vin) != 1 || len(tx.Vout) != 1 || tx.Vout[0].ValueZat != res.Amount || len(tx.Ironwood.Actions) != 2 {
		t.Errorf("as mined: %+v", tx)
	}
	sw.waitInventory(0)
	if v := sw.spendable(); v != (SpendableValue{}) {
		t.Errorf("spendable after a send max: %+v", v)
	}
}

// TestHarnessSendMaxTransparentToIronwood sends everything from a wallet
// with transparent outputs only to a unified address.
func TestHarnessSendMaxTransparentToIronwood(t *testing.T) {
	sw := newSendWallet(t, "sender")
	bystander := newSendWallet(t, "bystander")
	sw.connect()
	sw.fund(nil, []uint64{10_000_000, 5_000_000})

	res := sw.sendMax(bystander.address())
	// Two Ironwood actions for the one output, and two transparent inputs.
	const fee = 20_000
	if res.Fee != fee || res.Amount != 15_000_000-fee || res.PaidPool != PaidIronwood {
		t.Fatalf("send max: %+v", res)
	}
	harnessMine(t, 1)
	tx := waitNodeTx(t, res.TxIDs[0], true)
	if len(tx.Vin) != 2 || len(tx.Vout) != 0 || len(tx.Ironwood.Actions) != 2 {
		t.Errorf("as mined: %+v", tx)
	}
	sw.waitInventory(0)
}

// TestHarnessSendMaxOracleWallets builds wallets with one, two and three
// Ironwood notes and no transparent funds, sends everything from the last,
// and, with ZECLIB_ORACLE_DIR set, leaves copies of the wallet databases
// there for the Rust test oracle_parity_without_transparent_funds, which
// compares send max with librustzcash's propose_send_max_transfer on them.
func TestHarnessSendMaxOracleWallets(t *testing.T) {
	dir := os.Getenv("ZECLIB_ORACLE_DIR")
	bystander := newSendWallet(t, "bystander")
	for notes := 1; notes <= 3; notes++ {
		sw := newSendWallet(t, fmt.Sprintf("%d notes", notes))
		stop := sw.connect()
		var funding []uint64
		for i := range notes {
			funding = append(funding, uint64(10_000_000*(i+1)))
		}
		sw.fund(funding, nil)
		est, err := sw.w.EstimateSendMax(sw.account.ID, bystander.address(), nil)
		if err != nil {
			t.Fatalf("EstimateSendMax with %d notes: %v", notes, err)
		}
		t.Logf("%d notes: amount %d, fee %d", notes, est.Amount, est.Fee)
		actions := max(notes, 2)
		var total uint64
		for _, z := range funding {
			total += z
		}
		if est.Fee != uint64(5_000*actions) || est.Amount != total-est.Fee {
			t.Errorf("%d notes: %+v", notes, est)
		}
		stop()
		if dir != "" {
			if err := sw.w.Close(); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(sw.cfg.DBPath)
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, fmt.Sprintf("oracle-%d-notes.sqlite", notes))
			if err := os.WriteFile(out, b, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s", out)
		}
	}
}
