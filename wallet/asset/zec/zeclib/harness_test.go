// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build zec && cgo && harness

// These tests run against the Zcash simnet harness, util/testing/zec, which
// has to be running. They use the alpha node's light-client gRPC server and
// the harness control scripts in ~/dextest/zec/harness-ctl. They mine blocks,
// spend from the alpha funder wallet, and reorganize the chain by a few
// blocks, so they should not share a harness with tests that count on its
// state. The background miner of the harness does not disturb them.
//
//	go test -tags zec,harness -run TestHarness ./wallet/asset/zec/zeclib/

package zeclib

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

const (
	harnessAlphaServer = "127.0.0.1:33770"
	harnessBetaServer  = "127.0.0.1:33771"
	// harnessTimeout bounds each wait for the wallet to reach a state.
	harnessTimeout = 2 * time.Minute
	zatsPerZEC     = 100_000_000
)

// harnessNetwork matches the harness nodes: regtest with every upgrade
// active from height 1.
var harnessNetwork = Regtest(AllUpgradesAt(1))

// harnessCtlMtx serializes use of the control scripts. The funder wallet
// they spend from cannot be used by two processes at once.
var harnessCtlMtx sync.Mutex

// harnessCtl runs a harness control script and returns its trimmed output.
func harnessCtl(t *testing.T, script string, args ...string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home directory: %v", err)
	}
	dir := filepath.Join(home, "dextest", "zec", "harness-ctl")
	if _, err := os.Stat(filepath.Join(dir, script)); err != nil {
		t.Fatalf("harness script not found (is the zec harness running?): %v", err)
	}
	harnessCtlMtx.Lock()
	defer harnessCtlMtx.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "./"+script, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", script, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// harnessHeight returns the height of the alpha node's chain tip.
func harnessHeight(t *testing.T) uint32 {
	t.Helper()
	out := harnessCtl(t, "alpha", "getblockcount")
	height, err := strconv.ParseUint(out, 10, 32)
	if err != nil {
		t.Fatalf("getblockcount returned %q", out)
	}
	return uint32(height)
}

// harnessMine mines n blocks on the alpha node.
func harnessMine(t *testing.T, n int) {
	t.Helper()
	harnessCtl(t, "mine-alpha", strconv.Itoa(n))
}

// harnessSend pays an address from the alpha funder and returns the
// transaction ID. The payment is in the mempool of the alpha node.
func harnessSend(t *testing.T, address string, zats uint64) string {
	t.Helper()
	amount := fmt.Sprintf("%d.%08d", zats/zatsPerZEC, zats%zatsPerZEC)
	out := harnessCtl(t, "send-alpha", address, amount)
	lines := strings.Split(out, "\n")
	txid := strings.TrimSpace(lines[len(lines)-1])
	if len(txid) != 64 {
		t.Fatalf("send-alpha did not end with a transaction ID:\n%s", out)
	}
	return txid
}

// harnessWallet is a wallet connected to the harness.
type harnessWallet struct {
	t        *testing.T
	w        *Wallet
	cfg      *Config
	statuses *statusRecorder
}

// openHarnessWallet opens a wallet database at path against the alpha node.
func openHarnessWallet(t *testing.T, path string, adjust func(*Config)) *harnessWallet {
	t.Helper()
	hw := &harnessWallet{t: t, statuses: newStatusRecorder()}
	hw.cfg = &Config{
		DBPath:           path,
		Network:          harnessNetwork,
		Server:           harnessAlphaServer,
		Plaintext:        true,
		Logger:           newTestLogger(t),
		OnSyncStatus:     hw.statuses.record,
		PollInterval:     250 * time.Millisecond,
		MinConfirmations: 1,
	}
	if adjust != nil {
		adjust(hw.cfg)
	}
	w, err := Open(hw.cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	hw.w = w
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return hw
}

// newHarnessWallet opens a new wallet in a temporary directory.
func newHarnessWallet(t *testing.T, adjust func(*Config)) *harnessWallet {
	t.Helper()
	return openHarnessWallet(t, filepath.Join(t.TempDir(), "wallet.sqlite"), adjust)
}

// harnessBystander returns a unified address that no wallet under test and
// no funder of the harness follows.
func harnessBystander(t *testing.T) string {
	t.Helper()
	hw := newHarnessWallet(t, nil)
	account, err := hw.w.CreateAccount(context.Background(), &AccountParams{Name: "bystander", Seed: randomSeed(t)})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	address, err := hw.w.Address(account.ID)
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	return address
}

// harnessIronwoodRoot returns the root of the Ironwood note commitment tree
// after the block at height on the alpha node.
func harnessIronwoodRoot(t *testing.T, height uint32) string {
	t.Helper()
	out := harnessCtl(t, "alpha", "z_gettreestate", strconv.FormatUint(uint64(height), 10))
	var state struct {
		Ironwood struct {
			Commitments struct {
				FinalRoot string `json:"finalRoot"`
			} `json:"commitments"`
		} `json:"ironwood"`
	}
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		t.Fatalf("z_gettreestate %d: %v\n%s", height, err, out)
	}
	if state.Ironwood.Commitments.FinalRoot == "" {
		t.Fatalf("z_gettreestate %d has no Ironwood root:\n%s", height, out)
	}
	return state.Ironwood.Commitments.FinalRoot
}

func randomSeed(t *testing.T) []byte {
	t.Helper()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return seed
}

// connect starts syncing and returns a function that stops it.
func (hw *harnessWallet) connect() (stop func()) {
	hw.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	wg, err := hw.w.Connect(ctx)
	if err != nil {
		cancel()
		hw.t.Fatalf("Connect: %v", err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			waitGroup(hw.t, wg)
		})
	}
	hw.t.Cleanup(stop)
	return stop
}

// waitFor polls the wallet until ok accepts its summary.
func (hw *harnessWallet) waitFor(what string, ok func(*Summary, SyncStatus) bool) *Summary {
	hw.t.Helper()
	deadline := time.Now().Add(harnessTimeout)
	for {
		summary, err := hw.w.Summary()
		if err != nil {
			hw.t.Fatalf("Summary: %v", err)
		}
		status := hw.w.SyncStatus()
		if summary != nil && ok(summary, status) {
			return summary
		}
		if time.Now().After(deadline) {
			hw.t.Fatalf("timed out waiting for %s; status %+v, summary %+v", what, status, summary)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitSynced waits until the wallet is synced to at least the given height.
func (hw *harnessWallet) waitSynced(height uint32) *Summary {
	hw.t.Helper()
	return hw.waitFor(fmt.Sprintf("sync to height %d", height), func(s *Summary, st SyncStatus) bool {
		return st.State == SyncSynced && st.Err == nil && st.FullyScanned >= height && s.FullyScanned >= height
	})
}

// TestHarnessServer checks the behavior of the harness's light-client server
// that the wallet depends on.
func TestHarnessServer(t *testing.T) {
	ctx := context.Background()
	c, err := dialLightwalletd(&Config{Server: harnessAlphaServer, Plaintext: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()

	info, err := c.info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	t.Logf("server: vendor %q version %q chain %q height %d sapling activation %d",
		info.Vendor, info.Version, info.ChainName, info.BlockHeight, info.SaplingActivationHeight)
	if !harnessNetwork.matchesChainName(info.ChainName) {
		t.Errorf("chain name %q does not match the regtest network", info.ChainName)
	}

	tip, err := c.latestBlock(ctx)
	if err != nil {
		t.Fatalf("latestBlock: %v", err)
	}
	block, err := c.block(ctx, uint32(tip.Height))
	if err != nil {
		t.Fatalf("block: %v", err)
	}
	t.Logf("tip %d: block ID hash %x, compact block hash %x", tip.Height, tip.Hash, block.Hash)
	if !sameHash(tip.Hash, block.Hash) {
		t.Errorf("the tip's block ID hash is not the compact block's hash")
	}
	if !bytes.Equal(tip.Hash, block.Hash) {
		t.Logf("the block ID hash is in the reverse byte order of the compact block hash")
	}
	parent, err := c.block(ctx, uint32(tip.Height)-1)
	if err != nil {
		t.Fatalf("block: %v", err)
	}
	if !bytes.Equal(block.PrevHash, parent.Hash) {
		t.Errorf("a block's previous hash is not its parent's hash")
	}

	// The tree state of the genesis block is not served, which is why the
	// wallet builds the state below Sapling activation itself.
	if _, err := c.treeState(ctx, 0); err == nil {
		t.Logf("the server serves a tree state for height 0")
	}
	state, err := c.treeState(ctx, uint32(tip.Height))
	if err != nil {
		t.Fatalf("treeState: %v", err)
	}
	if state.Height != tip.Height || len(state.Hash) != 64 {
		t.Errorf("tree state: %+v", state)
	}

	roots, err := c.subtreeRoots(ctx, walletrpc.ShieldedProtocol_ironwood, 0)
	if err != nil {
		t.Errorf("subtreeRoots: %v", err)
	}
	t.Logf("%d Ironwood subtree roots", len(roots))

	// A transaction the node has never seen.
	unknown := bytes.Repeat([]byte{0x5a}, 32)
	tx, err := c.transaction(ctx, unknown)
	if err != nil || tx != nil {
		t.Errorf("unknown transaction: %+v, %v", tx, err)
	}

	// The transparent queries, for an address with history: the miner's.
	const miner = "tmUV6poBiYV5thY9QeovauFFYUMAejmfKNh"
	outputs := 0
	err = c.addressUTXOs(ctx, []string{miner}, 0, func(*walletrpc.GetAddressUtxosReply) error {
		outputs++
		return nil
	})
	if err != nil || outputs == 0 {
		t.Errorf("addressUTXOs: %d outputs, %v", outputs, err)
	}
	txs := 0
	err = c.addressTransactions(ctx, miner, uint32(tip.Height)-5, uint32(tip.Height), func(tx *walletrpc.RawTransaction) error {
		txs++
		if minedHeight(tx) == nil {
			t.Errorf("a mined transaction without a height: %d", tx.Height)
		}
		return nil
	})
	if err != nil {
		t.Errorf("addressTransactions: %v", err)
	}
	t.Logf("miner address: %d unspent outputs, %d transactions in the last 6 blocks", outputs, txs)
}

// TestHarnessSyncAndReceive creates a wallet, syncs it, and receives funds
// in the Ironwood pool and at a transparent address.
func TestHarnessSyncAndReceive(t *testing.T) {
	seed := randomSeed(t)
	hw := newHarnessWallet(t, nil)
	ctx := context.Background()

	tip := harnessHeight(t)
	account, err := hw.w.CreateAccount(ctx, &AccountParams{Name: "harness", Seed: seed})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if want := tip - min(tip, newAccountDepth); account.Birthday < want || account.Birthday > want+5 {
		t.Errorf("birthday %d with the tip at %d", account.Birthday, tip)
	}
	address, err := hw.w.Address(account.ID)
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	if !strings.HasPrefix(address, "uregtest1") {
		t.Fatalf("unified address: %s", address)
	}
	transparent, err := hw.w.TransparentReceiver(address)
	if err != nil || !strings.HasPrefix(transparent, "tm") {
		t.Fatalf("TransparentReceiver: %q, %v", transparent, err)
	}

	hw.connect()
	summary := hw.waitSynced(tip)
	if balance, ok := summary.Balance(account.ID); !ok || balance.Total != 0 {
		t.Fatalf("balance of a new account: %+v", balance)
	}

	// A payment to the unified address lands in the Ironwood pool.
	const toUnified = 150_000_000
	harnessSend(t, address, toUnified)
	harnessMine(t, 1)
	summary = hw.waitFor("the unified payment", func(s *Summary, _ SyncStatus) bool {
		b, _ := s.Balance(account.ID)
		return b.Ironwood.Total == toUnified
	})
	balance, _ := summary.Balance(account.ID)
	if balance.Total != toUnified || balance.Transparent.Total != 0 {
		t.Errorf("balance after the unified payment: %+v", balance)
	}

	// Payments to a new address without a transparent receiver and to the
	// transparent receiver of the first one, in one block.
	shielded, err := hw.w.NewAddress(account.ID, ReceiversShielded)
	if err != nil || shielded == address {
		t.Fatalf("NewAddress: %q, %v", shielded, err)
	}
	const toShielded, toTransparent = 25_000_000, 50_000_000
	const toIronwood = toUnified + toShielded
	harnessSend(t, shielded, toShielded)
	harnessSend(t, transparent, toTransparent)
	harnessMine(t, 1)
	summary = hw.waitFor("the shielded and transparent payments", func(s *Summary, _ SyncStatus) bool {
		b, _ := s.Balance(account.ID)
		return b.Ironwood.Total == toIronwood && b.Transparent.Total == toTransparent
	})
	balance, _ = summary.Balance(account.ID)
	if want := uint64(toIronwood + toTransparent); balance.Total != want {
		t.Errorf("total balance %d, want %d: %+v", balance.Total, want, balance)
	}

	// With one confirmation required, everything mined is spendable once
	// the wallet has scanned it.
	harnessMine(t, 1)
	summary = hw.waitFor("spendable balances", func(s *Summary, st SyncStatus) bool {
		b, _ := s.Balance(account.ID)
		return st.State == SyncSynced && b.Ironwood.Spendable == toIronwood
	})
	balance, _ = summary.Balance(account.ID)
	t.Logf("balances: %+v", balance)
	if status := hw.w.SyncStatus(); status.Err != nil || status.ScanProgress.Fraction() != 1 {
		t.Errorf("status when synced: %+v", status)
	}

	t.Run("restore from seed", func(t *testing.T) {
		restored := newHarnessWallet(t, nil)
		again, err := restored.w.CreateAccount(ctx, &AccountParams{
			Name: "restored", Seed: seed, Birthday: account.Birthday, Restore: true,
		})
		if err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
		// The same seed gives the same keys, under a new account ID.
		if again.UFVK != account.UFVK || again.ID == account.ID {
			t.Errorf("restored account: %+v, original %+v", again, account)
		}
		if addr, err := restored.w.Address(again.ID); err != nil || addr != address {
			t.Errorf("restored address: %q, %v", addr, err)
		}
		restored.connect()
		summary := restored.waitFor("the restored balance", func(s *Summary, st SyncStatus) bool {
			b, _ := s.Balance(again.ID)
			return st.State == SyncSynced && b.Total == balance.Total
		})
		got, _ := summary.Balance(again.ID)
		if got.Ironwood.Total != toIronwood || got.Transparent.Total != toTransparent {
			t.Errorf("restored balances: %+v", got)
		}
	})

	t.Run("a birthday after the funds finds nothing", func(t *testing.T) {
		late := newHarnessWallet(t, nil)
		again, err := late.w.CreateAccount(ctx, &AccountParams{
			Name: "late", Seed: seed, Birthday: harnessHeight(t),
		})
		if err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
		late.connect()
		summary := late.waitSynced(harnessHeight(t))
		got, _ := summary.Balance(again.ID)
		// The shielded funds were received before the birthday. The
		// transparent output is found by address, whatever the birthday.
		if got.Ironwood.Total != 0 {
			t.Errorf("balances with a late birthday: %+v", got)
		}
	})
}

// TestHarnessRestart checks that a wallet keeps its state across a stop, a
// close and a reopen, and resumes from where it was.
func TestHarnessRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.sqlite")
	seed := randomSeed(t)
	ctx := context.Background()

	hw := openHarnessWallet(t, path, nil)
	account, err := hw.w.CreateAccount(ctx, &AccountParams{Name: "restart", Seed: seed})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	address, err := hw.w.Address(account.ID)
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	stop := hw.connect()
	hw.waitSynced(harnessHeight(t))

	const amount = 12_345_678
	harnessSend(t, address, amount)
	harnessMine(t, 2)
	hw.waitFor("the payment", func(s *Summary, _ SyncStatus) bool {
		b, _ := s.Balance(account.ID)
		return b.Ironwood.Spendable == amount
	})

	// Stop syncing. Blocks mined now are not seen.
	stop()
	if got := hw.w.SyncStatus(); got.State != SyncStopped {
		t.Fatalf("status after stopping: %+v", got)
	}
	stoppedAt := hw.w.SyncStatus().FullyScanned
	harnessMine(t, 3)
	time.Sleep(time.Second)
	if got := hw.w.SyncStatus().FullyScanned; got != stoppedAt {
		t.Fatalf("the wallet scanned to %d while stopped at %d", got, stoppedAt)
	}

	// Connect again: it catches up.
	hw.connect()
	hw.waitSynced(stoppedAt + 3)

	// Close and reopen the database. No seed is needed.
	if err := hw.w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	harnessMine(t, 2)
	reopened := openHarnessWallet(t, path, nil)
	accounts, err := reopened.w.Accounts()
	if err != nil || len(accounts) != 1 || accounts[0].ID != account.ID {
		t.Fatalf("accounts after reopening: %+v, %v", accounts, err)
	}
	// The balance is there before the wallet connects.
	summary, err := reopened.w.Summary()
	if err != nil || summary == nil {
		t.Fatalf("Summary after reopening: %+v, %v", summary, err)
	}
	if b, _ := summary.Balance(account.ID); b.Ironwood.Total != amount {
		t.Errorf("balance after reopening: %+v", b)
	}
	if relevant, err := reopened.w.IsSeedRelevant(seed); err != nil || !relevant {
		t.Errorf("IsSeedRelevant: %t, %v", relevant, err)
	}

	reopened.connect()
	summary = reopened.waitSynced(stoppedAt + 5)
	if b, _ := summary.Balance(account.ID); b.Ironwood.Spendable != amount {
		t.Errorf("balance after resuming: %+v", b)
	}
}

// TestHarnessReorg checks that the wallet follows a chain reorganization
// that removes a payment it had received.
func TestHarnessReorg(t *testing.T) {
	seed := randomSeed(t)
	hw := newHarnessWallet(t, nil)
	ctx := context.Background()
	account, err := hw.w.CreateAccount(ctx, &AccountParams{Name: "reorg", Seed: seed})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	address, err := hw.w.Address(account.ID)
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	hw.connect()
	hw.waitSynced(harnessHeight(t))

	// A payment that stays, buried under a few blocks.
	const kept = 30_000_000
	harnessSend(t, address, kept)
	harnessMine(t, 4)
	hw.waitFor("the first payment", func(s *Summary, _ SyncStatus) bool {
		b, _ := s.Balance(account.ID)
		return b.Ironwood.Spendable == kept
	})

	// A payment in a block that is about to be replaced, and that cannot be
	// mined again afterwards. zebrad retries a transaction that was sent to
	// it until the transaction is mined or expires, so a payment from a
	// replaced block is back in the mempool within seconds if the new chain
	// accepts it. This one is built on a block whose coinbase goes into the
	// Ironwood pool, and that block is replaced as well. The anchor of the
	// payment, the Ironwood root after that block, never exists on the new
	// chain.
	harnessCtl(t, "mine-to", harnessBystander(t), "1")
	anchorHeight := harnessHeight(t)
	anchor := harnessIronwoodRoot(t, anchorHeight)
	if anchor == harnessIronwoodRoot(t, anchorHeight-1) {
		t.Fatalf("the coinbase of block %d did not change the Ironwood tree", anchorHeight)
	}
	const dropped = 7_000_000
	harnessSend(t, address, dropped)
	harnessMine(t, 1)
	before := harnessHeight(t)
	hw.waitFor("the second payment", func(s *Summary, st SyncStatus) bool {
		b, _ := s.Balance(account.ID)
		return b.Ironwood.Total == kept+dropped && st.FullyScanned == before
	})

	// The last two blocks, the one with the coinbase and the one with the
	// payment, are replaced by three.
	harnessCtl(t, "reorg", "2")
	if harnessIronwoodRoot(t, anchorHeight) == anchor {
		t.Fatalf("block %d has the same Ironwood root after the reorg", anchorHeight)
	}
	after := harnessHeight(t)
	if after != before+1 {
		t.Fatalf("height after the reorg: %d, want %d", after, before+1)
	}

	// The wallet does not forget the transaction when it rewinds. It holds
	// it as unmined, and its value as pending, until the transaction's
	// expiry height has passed.
	summary := hw.waitFor("the wallet to follow the reorg", func(s *Summary, st SyncStatus) bool {
		b, _ := s.Balance(account.ID)
		return st.State == SyncSynced && st.FullyScanned == after && b.Ironwood.Spendable == kept &&
			b.Ironwood.ValuePending == dropped
	})
	if b, _ := summary.Balance(account.ID); b.Total != kept+dropped {
		t.Errorf("balance after the reorg: %+v", b)
	}
	if status := hw.w.SyncStatus(); status.Err != nil {
		t.Errorf("status after the reorg: %+v", status)
	}

	// The wallet keeps working on the new chain.
	const later = 9_000_000
	harnessSend(t, address, later)
	harnessMine(t, 2)
	hw.waitFor("a payment on the new chain", func(s *Summary, _ SyncStatus) bool {
		b, _ := s.Balance(account.ID)
		return b.Ironwood.Spendable == kept+later
	})

	// Past the expiry height of the dropped transaction, 40 blocks after it
	// was made, its value is gone.
	//
	// The total falls as soon as the wallet hears of a tip above the expiry
	// height. The rest is spendable again once the new blocks are scanned.
	harnessMine(t, 45)
	hw.waitFor("the dropped payment to expire", func(s *Summary, _ SyncStatus) bool {
		b, _ := s.Balance(account.ID)
		return b.Ironwood.Total == kept+later && b.Ironwood.Spendable == kept+later
	})
}

// TestHarnessLifecycle stops a wallet in the middle of its first sync and
// lets it finish after a restart, against each of the two nodes.
func TestHarnessLifecycle(t *testing.T) {
	seed := randomSeed(t)
	path := filepath.Join(t.TempDir(), "wallet.sqlite")
	ctx := context.Background()
	slow := func(cfg *Config) { cfg.BatchSize = 3 }

	hw := openHarnessWallet(t, path, slow)
	// The earliest birthday the chain allows: the harness activates every
	// upgrade, NU6.3 among them, at height 1.
	account, err := hw.w.CreateAccount(ctx, &AccountParams{Name: "full", Seed: seed, Birthday: 1})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if account.Birthday != 1 {
		t.Fatalf("birthday: %d", account.Birthday)
	}
	tip := harnessHeight(t)

	stop := hw.connect()
	partial := hw.statuses.wait(t, "partial progress", func(s SyncStatus) bool {
		return s.State == SyncSyncing && s.FullyScanned > 0
	})
	start := time.Now()
	stop()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("stopping in mid-sync took %s", elapsed)
	}
	stopped := hw.w.SyncStatus()
	if stopped.State != SyncStopped || stopped.FullyScanned >= tip {
		t.Fatalf("status after stopping in mid-sync: %+v (was %+v)", stopped, partial)
	}
	t.Logf("stopped at height %d of %d", stopped.FullyScanned, tip)

	// A second Connect resumes, and a context that is already done stops it
	// at once without harm.
	done, cancel := context.WithCancel(ctx)
	cancel()
	wg, err := hw.w.Connect(done)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitGroup(t, wg)

	hw.statuses.reset()
	hw.connect()
	hw.waitSynced(tip)

	// The same database against the other node.
	if err := hw.w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	harnessMine(t, 2)
	beta := openHarnessWallet(t, path, func(cfg *Config) { cfg.Server = harnessBetaServer })
	beta.connect()
	beta.waitSynced(tip + 2)

	// Closing a connected wallet stops it.
	if err := beta.w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := beta.w.Summary(); !errors.Is(err, ErrClosed) {
		t.Errorf("Summary after Close: %v", err)
	}
}
