// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build zec && cgo

package zeclib

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/internal/ffi"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

// With the Rust backend compiled in, the lifecycle and sync tests also run
// against a real wallet database.
func init() {
	backendKinds = append(backendKinds, backendKind{
		name: "ffi",
		open: func(t *testing.T) backend { return openTestBackend(t, nil) },
	})
}

// noSaplingSeed is a seed whose first ten diversifier indexes have no valid
// Sapling diversifier. An address with a Sapling receiver could not be taken
// from its window of unused transparent addresses.
var noSaplingSeed = bytes.Repeat([]byte{7}, 32)

// The lengths of regtest unified addresses by their receivers. An address
// is the Bech32m encoding of its receivers, each with a 2-byte header, and
// 16 bytes of padding: 43 bytes for an Orchard receiver, which is the one
// that receives into the Ironwood pool, and 20 for a transparent one. A
// Sapling receiver would add 45 bytes, which is 72 characters.
const (
	ironwoodTransparentAddressLen = 148
	ironwoodAddressLen            = 113
)

func openTestBackend(t *testing.T, seed []byte) backend {
	t.Helper()
	be, err := openBackend(filepath.Join(t.TempDir(), "wallet.sqlite"), testNetwork, seed)
	if err != nil {
		t.Fatalf("openBackend: %v", err)
	}
	t.Cleanup(func() { be.close() })
	return be
}

// testTreeState is the encoded tree state of the fake chain's block at
// height.
func testTreeState(t *testing.T, srv *fakeLwd, height uint32) []byte {
	t.Helper()
	state, err := srv.GetTreeState(context.Background(), &walletrpc.BlockID{Height: uint64(height)})
	if err != nil {
		t.Fatalf("GetTreeState(%d): %v", height, err)
	}
	encoded, err := proto.Marshal(state)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return encoded
}

// testBlocks is the encoded blocks of the fake chain from start through
// end.
func testBlocks(t *testing.T, srv *fakeLwd, start, end uint32) [][]byte {
	t.Helper()
	var blocks [][]byte
	srv.set(func(s *fakeLwd) {
		for h := start; h <= end; h++ {
			encoded, err := proto.Marshal(s.blocks[h])
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			blocks = append(blocks, encoded)
		}
	})
	return blocks
}

func TestOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.sqlite")
	cfg := &Config{DBPath: path, Network: testNetwork, Server: "127.0.0.1:1", Plaintext: true}

	w, err := Open(cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	account, err := w.CreateAccountWithTreeState(&AccountParams{Name: "a", Seed: testSeed},
		&walletrpc.TreeState{Height: 99, Hash: strings.Repeat("00", 32)}, 0)
	if err != nil {
		t.Fatalf("CreateAccountWithTreeState: %v", err)
	}
	if !strings.HasPrefix(account.UFVK, "uviewregtest1") || len(account.SeedFingerprint) != 32 {
		t.Errorf("account: %+v", account)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The account is there when the database is opened again, with the
	// seed or without it.
	for _, seed := range [][]byte{nil, testSeed} {
		w, err := Open(cfg, seed)
		if err != nil {
			t.Fatalf("Open again (seed %t): %v", seed != nil, err)
		}
		accounts, err := w.Accounts()
		if err != nil || len(accounts) != 1 || accounts[0].ID != account.ID || accounts[0].UFVK != account.UFVK {
			t.Errorf("accounts after reopening: %+v, %v", accounts, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	// A seed that is not the wallet's.
	if _, err := Open(cfg, noSaplingSeed); !errors.Is(err, ErrSeedNotRelevant) {
		t.Errorf("Open with a foreign seed: %v, want ErrSeedNotRelevant", err)
	}
	// Bad arguments.
	if _, err := Open(&Config{Network: testNetwork}, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Open without a path: %v", err)
	}
	if _, err := Open(cfg, []byte{1}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Open with a short seed: %v", err)
	}
	bad := *cfg
	withoutIronwood := AllUpgradesAt(1)
	withoutIronwood.NU6_3, withoutIronwood.NU7 = 0, 0
	bad.Network = Regtest(withoutIronwood)
	if _, err := Open(&bad, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Open with a network without the Ironwood pool: %v", err)
	}
	missing := *cfg
	missing.DBPath = filepath.Join(t.TempDir(), "no-such-dir", "wallet.sqlite")
	if _, err := Open(&missing, nil); !errors.Is(err, ErrDatabase) {
		t.Errorf("Open in a missing directory: %v", err)
	}
}

func TestOpenNetworks(t *testing.T) {
	for _, network := range []Network{Mainnet, Testnet, Regtest(AllUpgradesAt(5))} {
		w, err := Open(&Config{
			DBPath:  filepath.Join(t.TempDir(), "wallet.sqlite"),
			Network: network,
		}, nil)
		if err != nil {
			t.Fatalf("Open on %s: %v", network, err)
		}
		if w.Network() != network {
			t.Errorf("Network: %+v, want %+v", w.Network(), network)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func TestFFIAccountsAndAddresses(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := openTestBackend(t, nil)

	if _, ok, err := be.birthday(); err != nil || ok {
		t.Errorf("birthday of an empty wallet: %t, %v", ok, err)
	}
	if _, ok, err := be.maxScannedHeight(); err != nil || ok {
		t.Errorf("maxScannedHeight of an empty wallet: %t, %v", ok, err)
	}
	if hash, err := be.blockHash(100); err != nil || hash != nil {
		t.Errorf("blockHash of an unscanned block: %x, %v", hash, err)
	}
	if relevant, err := be.isSeedRelevant(testSeed); err != nil || relevant {
		t.Errorf("isSeedRelevant on an empty wallet: %t, %v", relevant, err)
	}

	until := uint32(140)
	account, err := be.createAccount("main", testSeed, testTreeState(t, srv, 99), &until, "ledger")
	if err != nil {
		t.Fatalf("createAccount: %v", err)
	}
	if account.Name != "main" || account.Birthday != 100 || !account.Derived || account.HDIndex != 0 ||
		account.KeySource != "ledger" || len(account.SeedFingerprint) != 32 ||
		!strings.HasPrefix(account.UFVK, "uviewregtest1") {
		t.Fatalf("account: %+v", account)
	}
	if birthday, ok, err := be.birthday(); err != nil || !ok || birthday != 100 {
		t.Errorf("birthday: %d, %t, %v", birthday, ok, err)
	}

	address, err := be.currentAddress(account.ID)
	if err != nil || !strings.HasPrefix(address, "uregtest1") {
		t.Fatalf("currentAddress: %q, %v", address, err)
	}
	// An Ironwood and a transparent receiver, and no Sapling receiver.
	if len(address) != ironwoodTransparentAddressLen {
		t.Errorf("the current address has %d characters, want %d: %s", len(address), ironwoodTransparentAddressLen, address)
	}
	if again, err := be.currentAddress(account.ID); err != nil || again != address {
		t.Errorf("currentAddress again: %q, %v", again, err)
	}
	transparent, err := transparentReceiver(testNetwork, address)
	if err != nil || !strings.HasPrefix(transparent, "tm") {
		t.Errorf("transparentReceiver: %q, %v", transparent, err)
	}
	receivers, err := be.transparentReceivers(account.ID)
	if err != nil || !slices.ContainsFunc(receivers, func(a watchedAddress) bool { return a.address == transparent }) {
		t.Errorf("transparentReceivers: %v, %v", receivers, err)
	}
	// One run of unused addresses on each chain, by chain and index.
	if len(receivers) != 15 {
		t.Fatalf("%d transparent receivers, want 15", len(receivers))
	}
	for i, a := range receivers {
		want := watchedAddress{address: a.address, change: i >= 10, index: uint32(i % 10), unused: true}
		if a != want {
			t.Errorf("receiver %d: %+v", i, a)
		}
	}

	shielded, err := be.nextAvailableAddress(account.ID, ReceiversShielded)
	if err != nil || shielded == address {
		t.Fatalf("nextAvailableAddress(shielded): %q, %v", shielded, err)
	}
	if len(shielded) != ironwoodAddressLen {
		t.Errorf("a shielded address has %d characters, want %d: %s", len(shielded), ironwoodAddressLen, shielded)
	}
	if none, err := transparentReceiver(testNetwork, shielded); err != nil || none != "" {
		t.Errorf("transparent receiver of a shielded address: %q, %v", none, err)
	}
	all, err := be.nextAvailableAddress(account.ID, ReceiversAll)
	if err != nil || all == address || len(all) != ironwoodTransparentAddressLen {
		t.Errorf("nextAvailableAddress(all): %q, %v", all, err)
	}
	// The newest address with both receivers is the current one.
	if current, err := be.currentAddress(account.ID); err != nil || current != all {
		t.Errorf("currentAddress after a new address: %q, %v, want %q", current, err, all)
	}

	// The receiver helper rejects what is not a unified address of the
	// network.
	for _, bad := range []string{"", "not an address", transparent} {
		if _, err := transparentReceiver(testNetwork, bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("transparentReceiver(%q): %v", bad, err)
		}
	}
	if _, err := transparentReceiver(Mainnet, address); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("transparentReceiver on another network: %v", err)
	}

	const unknown = "00000000-0000-0000-0000-000000000001"
	if _, err := be.currentAddress(unknown); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("currentAddress(unknown): %v", err)
	}
	if _, err := be.nextAvailableAddress(unknown, ReceiversAll); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("nextAvailableAddress(unknown): %v", err)
	}
	if _, err := be.transparentReceivers(unknown); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("transparentReceivers(unknown): %v", err)
	}
	if _, err := be.utxoQueryHeight(unknown); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("utxoQueryHeight(unknown): %v", err)
	}
	if _, err := be.currentAddress("not a uuid"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("currentAddress(malformed): %v", err)
	}

	// An account whose transparent window has no index with a Sapling
	// diversifier. Its addresses do not need one.
	second, err := be.createAccount("second", noSaplingSeed, testTreeState(t, srv, 99), nil, "")
	if err != nil {
		t.Fatalf("createAccount: %v", err)
	}
	if second.KeySource != "" {
		t.Errorf("key source of an account created without one: %q", second.KeySource)
	}
	if address, err := be.currentAddress(second.ID); err != nil || len(address) != ironwoodTransparentAddressLen {
		t.Errorf("currentAddress of the second account: %q, %v", address, err)
	}

	// The gap limit of transparent addresses: the window of unused ones
	// runs out when nothing is paid to them.
	var generated int
	for err = nil; err == nil && generated <= 20; generated++ {
		var address string
		address, err = be.nextAvailableAddress(second.ID, ReceiversAll)
		if err == nil && len(address) != ironwoodTransparentAddressLen {
			t.Errorf("address %d has %d characters: %s", generated, len(address), address)
		}
	}
	if !errors.Is(err, ErrAddressGapLimit) || generated < 2 {
		t.Errorf("after %d addresses: %v, want ErrAddressGapLimit", generated-1, err)
	}
	if _, err := be.nextAvailableAddress(second.ID, ReceiversShielded); err != nil {
		t.Errorf("a shielded address beyond the gap limit: %v", err)
	}

	accounts, err := be.accounts()
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts: %+v, %v", accounts, err)
	}
	for seed, want := range map[string]bool{string(testSeed): true, string(noSaplingSeed): true, strings.Repeat("x", 32): false} {
		if relevant, err := be.isSeedRelevant([]byte(seed)); err != nil || relevant != want {
			t.Errorf("isSeedRelevant: %t, %v, want %t", relevant, err, want)
		}
	}
	if _, err := be.isSeedRelevant([]byte{1}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("isSeedRelevant with a short seed: %v", err)
	}
}

func TestFFIScanAndRewind(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := openTestBackend(t, nil)
	account, err := be.createAccount("main", testSeed, testTreeState(t, srv, 99), nil, "")
	if err != nil {
		t.Fatalf("createAccount: %v", err)
	}

	if summary, err := be.summary(0); err != nil || summary != nil {
		t.Fatalf("summary before the first scan: %+v, %v", summary, err)
	}
	if err := be.updateChainTip(150); err != nil {
		t.Fatalf("updateChainTip: %v", err)
	}
	ranges, err := be.suggestScanRanges()
	if err != nil || len(ranges) == 0 {
		t.Fatalf("suggestScanRanges: %v, %v", ranges, err)
	}
	for _, r := range ranges {
		if r.start < 100 || r.end > 151 || r.start >= r.end || r.priority <= priorityScanned {
			t.Errorf("unexpected range %s", r)
		}
	}

	scanned, err := be.scanBlocks(testTreeState(t, srv, 99), testBlocks(t, srv, 100, 150))
	if err != nil {
		t.Fatalf("scanBlocks: %v", err)
	}
	if scanned.start != 100 || scanned.end != 151 {
		t.Errorf("scan summary: %+v", scanned)
	}
	if highest, ok, err := be.maxScannedHeight(); err != nil || !ok || highest != 150 {
		t.Errorf("maxScannedHeight: %d, %t, %v", highest, ok, err)
	}
	if hash, err := be.blockHash(150); err != nil || !bytes.Equal(hash, srv.hashAt(150)) {
		t.Errorf("blockHash(150): %x, %v", hash, err)
	}
	if ranges, err := be.suggestScanRanges(); err != nil || len(ranges) != 0 {
		t.Errorf("ranges after a full scan: %v, %v", ranges, err)
	}

	for _, minConf := range []uint32{0, 1, 5} {
		summary, err := be.summary(minConf)
		if err != nil || summary == nil {
			t.Fatalf("summary(%d): %+v, %v", minConf, summary, err)
		}
		if summary.ChainTip != 150 || summary.FullyScanned != 150 || summary.Recovering ||
			len(summary.Balances) != 1 || summary.Balances[0].AccountID != account.ID {
			t.Errorf("summary(%d): %+v", minConf, summary)
		}
	}

	// Bad scan input.
	if _, err := be.scanBlocks(testTreeState(t, srv, 150), nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("scanBlocks without blocks: %v", err)
	}
	if _, err := be.scanBlocks([]byte{0x0f, 0xff}, testBlocks(t, srv, 100, 100)); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("scanBlocks with a malformed tree state: %v", err)
	}
	if _, err := be.scanBlocks(testTreeState(t, srv, 99), [][]byte{{0x0f, 0xff}}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("scanBlocks with a malformed block: %v", err)
	}

	// Blocks of another chain do not connect.
	srv.reorg(5, 1)
	if err := be.updateChainTip(151); err != nil {
		t.Fatalf("updateChainTip: %v", err)
	}
	_, err = be.scanBlocks(testTreeState(t, srv, 150), testBlocks(t, srv, 151, 151))
	var discontinuity *chainContinuityError
	if !errors.As(err, &discontinuity) || discontinuity.height != 151 || discontinuity.reason == "" {
		t.Fatalf("scanBlocks on another chain: %v", err)
	}
	if !strings.Contains(discontinuity.Error(), "151") {
		t.Errorf("error text: %v", discontinuity)
	}

	// Rewinding below the fork and scanning the new chain reconnects. The
	// wallet rewinds to the nearest height at or below the request at which
	// it kept a checkpoint.
	rewound, err := be.rewindToHeight(140)
	if err != nil || rewound > 140 || rewound < 99 {
		t.Fatalf("rewindToHeight(140): %d, %v", rewound, err)
	}
	if highest, ok, err := be.maxScannedHeight(); err != nil || !ok || highest != rewound {
		t.Errorf("maxScannedHeight after the rewind: %d, %t, %v", highest, ok, err)
	}
	if err := be.updateChainTip(151); err != nil {
		t.Fatalf("updateChainTip: %v", err)
	}
	if _, err := be.scanBlocks(testTreeState(t, srv, rewound), testBlocks(t, srv, rewound+1, 151)); err != nil {
		t.Fatalf("scanBlocks after the rewind: %v", err)
	}
	if hash, err := be.blockHash(151); err != nil || !bytes.Equal(hash, srv.hashAt(151)) {
		t.Errorf("blockHash(151): %x, %v", hash, err)
	}

	// A rewind below anything the wallet kept.
	_, err = be.rewindToHeight(10)
	var invalid *rewindError
	if !errors.As(err, &invalid) || invalid.requested != 10 {
		t.Fatalf("rewindToHeight(10): %v", err)
	}
	if invalid.Error() == "" {
		t.Error("empty rewind error text")
	}
}

func TestFFITransparentAndTransactions(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := openTestBackend(t, nil)
	account, err := be.createAccount("main", testSeed, testTreeState(t, srv, 99), nil, "")
	if err != nil {
		t.Fatalf("createAccount: %v", err)
	}
	if err := be.updateChainTip(150); err != nil {
		t.Fatalf("updateChainTip: %v", err)
	}
	if _, err := be.scanBlocks(testTreeState(t, srv, 99), testBlocks(t, srv, 100, 150)); err != nil {
		t.Fatalf("scanBlocks: %v", err)
	}

	receivers, err := be.transparentReceivers(account.ID)
	if err != nil || len(receivers) == 0 {
		t.Fatalf("transparentReceivers: %v, %v", receivers, err)
	}
	if height, err := be.utxoQueryHeight(account.ID); err != nil || height > 150 {
		t.Errorf("utxoQueryHeight: %d, %v", height, err)
	}

	txid := bytes.Repeat([]byte{3}, 32)
	output := &utxo{txid: txid, index: 2, script: p2pkhScript(receivers[0].address), value: 70_000, height: 120}
	if err := be.putUTXO(output); err != nil {
		t.Fatalf("putUTXO: %v", err)
	}
	summary, err := be.summary(1)
	if err != nil || summary == nil || summary.Balances[0].Transparent.Total != 70_000 {
		t.Fatalf("summary after putUTXO: %+v, %v", summary, err)
	}
	bad := *output
	bad.txid = []byte{1, 2}
	if err := be.putUTXO(&bad); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("putUTXO with a short txid: %v", err)
	}
	bad = *output
	bad.script = []byte{0x6a}
	if err := be.putUTXO(&bad); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("putUTXO with an unspendable script: %v", err)
	}
	bad = *output
	bad.script = p2pkhScript("tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm")
	if err := be.putUTXO(&bad); !errors.Is(err, ErrDatabase) {
		t.Errorf("putUTXO to a foreign address: %v", err)
	}

	// A new wallet asks for the transactions of its ephemeral addresses.
	requests, err := be.transactionDataRequests()
	if err != nil || len(requests) == 0 {
		t.Fatalf("transactionDataRequests: %v, %v", requests, err)
	}
	var open *txRequest
	for i := range requests {
		r := &requests[i]
		if r.kind == txRequestAddress && !r.bounded {
			open = r
		}
		if r.key() == "" {
			t.Errorf("request without a key: %+v", r)
		}
	}
	if open == nil || !strings.HasPrefix(open.address, "tm") || !open.requestAt.IsZero() {
		t.Fatalf("no open-ended address request among %+v", requests)
	}
	if err := be.notifyAddressChecked(open, 150); err != nil {
		t.Errorf("notifyAddressChecked: %v", err)
	}
	at, ok, err := be.scheduleNextCheck(open.address, 10*time.Minute)
	if err != nil || !ok || at.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("scheduleNextCheck: %s, %t, %v", at, ok, err)
	}
	// The request now carries its scheduled time, and round-trips with it.
	requests, err = be.transactionDataRequests()
	if err != nil {
		t.Fatalf("transactionDataRequests: %v", err)
	}
	found := false
	for i := range requests {
		if requests[i].address == open.address {
			found = true
			if requests[i].requestAt.Unix() != at.Unix() {
				t.Errorf("scheduled for %s, the request says %s", at, requests[i].requestAt)
			}
			if err := be.notifyAddressChecked(&requests[i], 150); err != nil {
				t.Errorf("notifyAddressChecked of a scheduled request: %v", err)
			}
		}
	}
	if !found {
		t.Error("the scheduled request is gone")
	}
	if _, ok, err := be.scheduleNextCheck("tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm", time.Minute); err != nil || ok {
		t.Errorf("scheduleNextCheck of a foreign address: %t, %v", ok, err)
	}
	if _, _, err := be.scheduleNextCheck("nope", time.Minute); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("scheduleNextCheck of a malformed address: %v", err)
	}
	if _, _, err := be.scheduleNextCheck(open.address, -time.Minute); err != nil {
		t.Errorf("scheduleNextCheck with a negative offset: %v", err)
	}

	// A bounded request has to be closed at its last height.
	bounded := &txRequest{
		kind: txRequestAddress, address: receivers[0].address, start: 100, end: 151, bounded: true,
		txStatus: txStatusMined, outputStatus: outputStatusAll,
	}
	if err := be.notifyAddressChecked(bounded, 140); !errors.Is(err, ErrDatabase) {
		t.Errorf("notifyAddressChecked at the wrong height: %v", err)
	}
	if err := be.notifyAddressChecked(bounded, 150); err != nil {
		t.Errorf("notifyAddressChecked: %v", err)
	}
	for _, filter := range []txStatusFilter{txStatusMined, txStatusMempool, txStatusAll} {
		for _, out := range []outputStatusFilter{outputStatusUnspent, outputStatusAll} {
			r := *bounded
			r.txStatus, r.outputStatus = filter, out
			if err := be.notifyAddressChecked(&r, 150); err != nil {
				t.Errorf("notifyAddressChecked with filters %d, %d: %v", filter, out, err)
			}
		}
	}
	if err := be.notifyAddressChecked(&txRequest{kind: txRequestStatus, txid: txid}, 150); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("notifyAddressChecked of a status request: %v", err)
	}

	// Transaction statuses.
	for _, st := range []txStatus{{kind: txNotRecognized}, {kind: txNotInMainChain}, {kind: txMined, height: 120}} {
		if err := be.setTransactionStatus(txid, st); err != nil {
			t.Errorf("setTransactionStatus(%+v): %v", st, err)
		}
	}
	if err := be.setTransactionStatus(txid, txStatus{kind: 99}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("setTransactionStatus with an unknown kind: %v", err)
	}
	if err := be.setTransactionStatus([]byte{1}, txStatus{kind: txMined, height: 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("setTransactionStatus with a short txid: %v", err)
	}

	// Full transactions. emptyTx is a version 5 transaction without inputs
	// or outputs: header, lock time, expiry height, and empty bundles.
	emptyTx := []byte{
		0x05, 0x00, 0x00, 0x80, // version 5, overwintered
		0x0a, 0x27, 0xa7, 0x26, // version group ID
		0xb4, 0xd0, 0xd6, 0xc2, // consensus branch ID (NU5)
		0x00, 0x00, 0x00, 0x00, // lock time
		0x00, 0x00, 0x00, 0x00, // expiry height
		0x00, 0x00, // transparent inputs and outputs
		0x00, 0x00, // Sapling spends and outputs
		0x00, // Orchard actions
	}
	height := uint32(120)
	for _, mined := range []*uint32{nil, &height} {
		id, err := be.decryptAndStoreTransaction(emptyTx, mined)
		if err != nil || len(id) != 32 {
			t.Errorf("decryptAndStoreTransaction: %x, %v", id, err)
		}
	}
	if _, err := be.decryptAndStoreTransaction([]byte{1, 2, 3}, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("decryptAndStoreTransaction of garbage: %v", err)
	}
}

func TestFFISubtreeRoots(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := openTestBackend(t, nil)
	if _, err := be.createAccount("main", testSeed, testTreeState(t, srv, 99), nil, ""); err != nil {
		t.Fatalf("createAccount: %v", err)
	}
	root := func(n byte) subtreeRoot {
		hash := make([]byte, 32)
		hash[0] = n
		return subtreeRoot{hash: hash, completingHeight: uint32(n)}
	}
	if err := be.putSubtreeRoots(&subtreeRoots{}); err != nil {
		t.Fatalf("putSubtreeRoots without roots: %v", err)
	}
	err := be.putSubtreeRoots(&subtreeRoots{roots: []subtreeRoot{root(1), root(2), root(3), root(4)}})
	if err != nil {
		t.Fatalf("putSubtreeRoots: %v", err)
	}
	if err := be.updateChainTip(150); err != nil {
		t.Fatalf("updateChainTip: %v", err)
	}
	if _, err := be.scanBlocks(testTreeState(t, srv, 99), testBlocks(t, srv, 100, 150)); err != nil {
		t.Fatalf("scanBlocks: %v", err)
	}
	summary, err := be.summary(0)
	if err != nil || summary == nil {
		t.Fatalf("summary: %+v, %v", summary, err)
	}
	// The next request overlaps the last root the wallet holds.
	if summary.nextSubtree != 2 {
		t.Errorf("next subtree index: %d, want 2", summary.nextSubtree)
	}
	short := subtreeRoot{hash: []byte{1, 2, 3}}
	if err := be.putSubtreeRoots(&subtreeRoots{start: 4, roots: []subtreeRoot{short}}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("putSubtreeRoots with a short hash: %v", err)
	}
}

func TestFFIRecoveryProgress(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := openTestBackend(t, nil)
	until := uint32(140)
	if _, err := be.createAccount("restored", testSeed, testTreeState(t, srv, 99), &until, ""); err != nil {
		t.Fatalf("createAccount: %v", err)
	}
	if err := be.updateChainTip(150); err != nil {
		t.Fatalf("updateChainTip: %v", err)
	}
	if _, err := be.scanBlocks(testTreeState(t, srv, 99), testBlocks(t, srv, 100, 110)); err != nil {
		t.Fatalf("scanBlocks: %v", err)
	}
	summary, err := be.summary(0)
	if err != nil || summary == nil {
		t.Fatalf("summary: %+v, %v", summary, err)
	}
	if summary.FullyScanned != 110 || summary.ChainTip != 150 {
		t.Errorf("summary: %+v", summary)
	}
}

func TestFFIClosed(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := openTestBackend(t, nil)
	account, err := be.createAccount("main", testSeed, testTreeState(t, srv, 99), nil, "")
	if err != nil {
		t.Fatalf("createAccount: %v", err)
	}
	if err := be.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := be.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	id := account.ID
	calls := map[string]func() error{
		"createAccount": func() error {
			_, err := be.createAccount("x", testSeed, testTreeState(t, srv, 99), nil, "")
			return err
		},
		"accounts":                func() error { _, err := be.accounts(); return err },
		"isSeedRelevant":          func() error { _, err := be.isSeedRelevant(testSeed); return err },
		"currentAddress":          func() error { _, err := be.currentAddress(id); return err },
		"nextAvailableAddress":    func() error { _, err := be.nextAvailableAddress(id, ReceiversAll); return err },
		"transparentReceivers":    func() error { _, err := be.transparentReceivers(id); return err },
		"summary":                 func() error { _, err := be.summary(0); return err },
		"birthday":                func() error { _, _, err := be.birthday(); return err },
		"maxScannedHeight":        func() error { _, _, err := be.maxScannedHeight(); return err },
		"blockHash":               func() error { _, err := be.blockHash(1); return err },
		"putSubtreeRoots":         func() error { return be.putSubtreeRoots(&subtreeRoots{}) },
		"updateChainTip":          func() error { return be.updateChainTip(1) },
		"suggestScanRanges":       func() error { _, err := be.suggestScanRanges(); return err },
		"scanBlocks":              func() error { _, err := be.scanBlocks(nil, nil); return err },
		"rewindToHeight":          func() error { _, err := be.rewindToHeight(1); return err },
		"utxoQueryHeight":         func() error { _, err := be.utxoQueryHeight(id); return err },
		"putUTXO":                 func() error { return be.putUTXO(&utxo{}) },
		"transactionDataRequests": func() error { _, err := be.transactionDataRequests(); return err },
		"setTransactionStatus": func() error {
			return be.setTransactionStatus(make([]byte, 32), txStatus{kind: txMined})
		},
		"decryptAndStoreTransaction": func() error {
			_, err := be.decryptAndStoreTransaction(nil, nil)
			return err
		},
		"notifyAddressChecked": func() error {
			return be.notifyAddressChecked(&txRequest{kind: txRequestAddress}, 1)
		},
		"scheduleNextCheck": func() error {
			_, _, err := be.scheduleNextCheck("x", time.Second)
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrClosed) {
			t.Errorf("%s after close: %v, want ErrClosed", name, err)
		}
	}
}

func TestConvertError(t *testing.T) {
	if err := convertError(nil); err != nil {
		t.Errorf("convertError(nil): %v", err)
	}
	// The bindings return a typed nil pointer for success.
	var none *ffi.WalletError
	if err := convertError(none); err != nil {
		t.Errorf("convertError of a nil binding error: %v", err)
	}
	other := errors.New("not from the bindings")
	if err := convertError(other); err != other {
		t.Errorf("convertError passed through as %v", err)
	}

	sentinels := []struct {
		in   *ffi.WalletError
		want error
		text string
	}{
		{ffi.NewWalletErrorInvalidArgument("bad seed"), ErrInvalidArgument, "bad seed"},
		{ffi.NewWalletErrorDatabase("locked"), ErrDatabase, "locked"},
		{ffi.NewWalletErrorSeedRequired(), ErrSeedRequired, ""},
		{ffi.NewWalletErrorSeedNotRelevant(), ErrSeedNotRelevant, ""},
		{ffi.NewWalletErrorAccountNotFound(), ErrAccountNotFound, ""},
		{ffi.NewWalletErrorAddressGapLimit("index 10"), ErrAddressGapLimit, "index 10"},
		{ffi.NewWalletErrorInternal("panic: oops"), ErrInternal, "panic: oops"},
	}
	for _, tt := range sentinels {
		err := convertError(tt.in)
		if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.text) {
			t.Errorf("convertError(%v) = %v, want %v", tt.in, err, tt.want)
		}
		if strings.Contains(err.Error(), "WalletError") {
			t.Errorf("the generated error text leaks: %v", err)
		}
	}

	var continuity *chainContinuityError
	if err := convertError(ffi.NewWalletErrorChainContinuity(77, "prev hash")); !errors.As(err, &continuity) ||
		continuity.height != 77 || continuity.reason != "prev hash" {
		t.Errorf("chain continuity: %v", err)
	}
	var scan *scanError
	if err := convertError(ffi.NewWalletErrorScan(78, "tree size")); !errors.As(err, &scan) ||
		scan.height != 78 || scan.reason != "tree size" {
		t.Errorf("scan: %v", err)
	}
	var rewind *rewindError
	safe := uint32(90)
	if err := convertError(ffi.NewWalletErrorRewindInvalid(50, &safe)); !errors.As(err, &rewind) ||
		rewind.requested != 50 || !rewind.hasSafe || rewind.safe != 90 {
		t.Errorf("rewind with a safe height: %v", err)
	}
	if err := convertError(ffi.NewWalletErrorRewindInvalid(50, nil)); !errors.As(err, &rewind) ||
		rewind.requested != 50 || rewind.hasSafe {
		t.Errorf("rewind without a safe height: %v", err)
	}
}

func TestFFIConversions(t *testing.T) {
	priorities := map[ffi.ScanPriority]scanPriority{
		ffi.ScanPriorityIgnored: priorityIgnored, ffi.ScanPriorityScanned: priorityScanned,
		ffi.ScanPriorityHistoric: priorityHistoric, ffi.ScanPriorityOpenAdjacent: priorityOpenAdjacent,
		ffi.ScanPriorityFoundNote: priorityFoundNote, ffi.ScanPriorityChainTip: priorityChainTip,
		ffi.ScanPriorityVerify: priorityVerify,
	}
	for in, want := range priorities {
		if got := fromFFIPriority(in); got != want {
			t.Errorf("priority %d: %s, want %s", in, got, want)
		}
	}
	for _, f := range []txStatusFilter{txStatusMined, txStatusMempool, txStatusAll} {
		if got := fromFFITxStatusFilter(toFFITxStatusFilter(f)); got != f {
			t.Errorf("transaction status filter %d round-trips to %d", f, got)
		}
	}
	for _, f := range []outputStatusFilter{outputStatusUnspent, outputStatusAll} {
		if got := fromFFIOutputStatusFilter(toFFIOutputStatusFilter(f)); got != f {
			t.Errorf("output status filter %d round-trips to %d", f, got)
		}
	}

	id := bytes.Repeat([]byte{4}, 32)
	end, at := uint32(200), uint64(1_700_000_000)
	requests := []struct {
		in   ffi.TransactionDataRequest
		want txRequest
	}{
		{ffi.TransactionDataRequestGetStatus{Txid: id}, txRequest{kind: txRequestStatus, txid: id}},
		{ffi.TransactionDataRequestEnhancement{Txid: id}, txRequest{kind: txRequestEnhancement, txid: id}},
		{
			ffi.TransactionDataRequestTransactionsInvolvingAddress{
				Address: "tm1", BlockRangeStart: 100, BlockRangeEnd: &end, RequestAt: &at,
				TxStatusFilter:     ffi.TransactionStatusFilterMined,
				OutputStatusFilter: ffi.OutputStatusFilterAll,
			},
			txRequest{
				kind: txRequestAddress, address: "tm1", start: 100, end: 200, bounded: true,
				requestAt: time.Unix(1_700_000_000, 0), txStatus: txStatusMined, outputStatus: outputStatusAll,
			},
		},
		{
			ffi.TransactionDataRequestTransactionsInvolvingAddress{
				Address: "tm2", BlockRangeStart: 5,
				TxStatusFilter:     ffi.TransactionStatusFilterAll,
				OutputStatusFilter: ffi.OutputStatusFilterUnspent,
			},
			txRequest{kind: txRequestAddress, address: "tm2", start: 5, txStatus: txStatusAll, outputStatus: outputStatusUnspent},
		},
	}
	for _, tt := range requests {
		got, err := fromFFITxRequest(tt.in)
		if err != nil {
			t.Errorf("fromFFITxRequest(%+v): %v", tt.in, err)
			continue
		}
		if got.key() != tt.want.key() || got.kind != tt.want.kind || !bytes.Equal(got.txid, tt.want.txid) ||
			!got.requestAt.Equal(tt.want.requestAt) || got.txStatus != tt.want.txStatus ||
			got.outputStatus != tt.want.outputStatus {
			t.Errorf("fromFFITxRequest(%+v) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
	if _, err := fromFFITxRequest(nil); !errors.Is(err, ErrInternal) {
		t.Errorf("fromFFITxRequest(nil): %v", err)
	}

	// The zero height of the Go schedule is the absent height of the Rust
	// one.
	network := toFFINetwork(Regtest(ActivationHeights{Overwinter: 1, Sapling: 2})).(ffi.NetworkRegtest)
	if network.Heights.Overwinter == nil || *network.Heights.Overwinter != 1 ||
		network.Heights.Sapling == nil || *network.Heights.Sapling != 2 ||
		network.Heights.Nu5 != nil || network.Heights.Nu7 != nil {
		t.Errorf("regtest heights: %+v", network.Heights)
	}
	if _, ok := toFFINetwork(Mainnet).(ffi.NetworkMainnet); !ok {
		t.Error("mainnet does not convert to mainnet")
	}
	if _, ok := toFFINetwork(Testnet).(ffi.NetworkTestnet); !ok {
		t.Error("testnet does not convert to testnet")
	}
}

func TestWalletReceivers(t *testing.T) {
	e := newEnv(t, backendKinds[len(backendKinds)-1], 150, nil)
	account := e.createAccount(100)
	address, err := e.w.Address(account.ID)
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	if len(address) != ironwoodTransparentAddressLen {
		t.Errorf("Address has %d characters, want %d: %s", len(address), ironwoodTransparentAddressLen, address)
	}
	transparent, err := e.w.TransparentReceiver(address)
	if err != nil || !strings.HasPrefix(transparent, "tm") {
		t.Errorf("TransparentReceiver: %q, %v", transparent, err)
	}
	addresses, err := e.w.TransparentAddresses(account.ID)
	if err != nil || !slices.Contains(addresses, transparent) {
		t.Errorf("TransparentAddresses: %v, %v", addresses, err)
	}
	if _, err := e.w.TransparentReceiver("nope"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("TransparentReceiver of a malformed address: %v", err)
	}
	shielded, err := e.w.NewAddress(account.ID, ReceiversShielded)
	if err != nil || len(shielded) != ironwoodAddressLen {
		t.Errorf("NewAddress(shielded): %q, %v", shielded, err)
	}
}

// TestEphemeralAddressChecksAreScheduled checks that the standing checks of
// a wallet's ephemeral addresses are made once and then left until their
// scheduled time.
func TestEphemeralAddressChecksAreScheduled(t *testing.T) {
	e := newEnv(t, backendKinds[len(backendKinds)-1], 150, nil)
	e.createAccount(100)
	// The wallet draws the delay of each next check from an exponential
	// distribution, which has no lower bound. With the default mean of
	// minutes, about one run in a hundred would see a check come due
	// within the test. A mean of a century makes that one in billions.
	tune := e.w.tune
	e.w.tune = func(o *syncOptions) {
		tune(o)
		o.addressCheckInterval = 100 * 365 * 24 * time.Hour
	}
	e.connect()
	e.waitSynced(150)

	checks := e.srv.callCount("GetTaddressTxids")
	if checks == 0 {
		t.Fatal("no address was checked")
	}
	e.srv.mine(3)
	e.waitSynced(153)
	polls := e.srv.callCount("GetLatestBlock")
	eventually(t, "a few polls", func() bool { return e.srv.callCount("GetLatestBlock") >= polls+5 })
	if got := e.srv.callCount("GetTaddressTxids"); got != checks {
		t.Errorf("addresses were checked %d more times before their scheduled time", got-checks)
	}
}

// The wallet keeps the time of the next check of an address in whole seconds
// and moves it only once that second has passed. A check made within the
// scheduled second leaves the request standing, so it would be repeated in
// every sync cycle of that second.
func TestDueAddressChecksAreMadeOncePerSecond(t *testing.T) {
	e := newEnv(t, backendKinds[len(backendKinds)-1], 150, nil)
	e.createAccount(100)
	// With a mean delay of a second, checks come due throughout the test.
	tune := e.w.tune
	e.w.tune = func(o *syncOptions) {
		tune(o)
		o.addressCheckInterval = time.Second
	}
	start := time.Now()
	e.connect()
	e.waitSynced(150)
	addresses := e.srv.callCount("GetTaddressTxids")
	if addresses == 0 {
		t.Fatal("no address was checked")
	}

	eventually(t, "two seconds of polling", func() bool { return time.Since(start) > 2200*time.Millisecond })
	checks := e.srv.callCount("GetTaddressTxids")
	// An address is checked at most once in any one second, however many
	// sync cycles the second has.
	seconds := int(time.Now().Unix()-start.Unix()) + 1
	if limit := addresses * seconds; checks > limit {
		t.Errorf("%d address checks in %d seconds for %d addresses, want at most %d", checks, seconds, addresses, limit)
	}
	if checks == addresses {
		t.Error("no check came due")
	}
}

// TestWalkCoversUnusedAddressesBeyondTheGap restores a seed whose default
// address lies beyond the first gap of unused transparent addresses. Such an
// account watches 11 unused receive addresses from the start, one more than
// the gap. The first of them holds an output from below the account
// birthday, which only the history of the address shows.
func TestWalkCoversUnusedAddressesBeyondTheGap(t *testing.T) {
	kind := backendKinds[len(backendKinds)-1]
	scratch := newUsedAddresses(t, kind, 1).scratch
	e := newEnv(t, kind, 200, func(cfg *Config) { cfg.MinConfirmations = 1 })
	account, err := e.w.CreateAccount(context.Background(), &AccountParams{
		Name: "no sapling", Seed: noSaplingSeed, Birthday: 100, Restore: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	e.account = account
	watched, err := e.be.transparentReceivers(account.ID)
	if err != nil {
		t.Fatalf("transparentReceivers: %v", err)
	}
	receive := 0
	for _, a := range watched {
		if !a.unused {
			t.Errorf("address %s of a new account is not unused", a.address)
		}
		if !a.change {
			receive++
		}
	}
	if receive != 11 {
		t.Fatalf("%d receive addresses, want 11", receive)
	}

	const value = 77_000
	address := watched[0].address
	paying := testTx(
		[]testOutPoint{{txid: bytes.Repeat([]byte{0xac}, 32)}},
		[]testOutput{{script: p2pkhScript(address), value: value}},
	)
	txid, err := scratch.decryptAndStoreTransaction(paying, nil)
	if err != nil {
		t.Fatalf("decryptAndStoreTransaction: %v", err)
	}
	e.srv.set(func(s *fakeLwd) {
		s.addrTxs[address] = []*walletrpc.RawTransaction{{Data: paying, Height: 50}}
		s.utxos[address] = []*walletrpc.GetAddressUtxosReply{{
			Address: address, Txid: txid, Script: p2pkhScript(address), ValueZat: value, Height: 50,
		}}
	})

	e.connect()
	e.waitSynced(200)
	eventually(t, "the output at the first address", func() bool {
		return transparentBalance(t, e) == value
	})
}
