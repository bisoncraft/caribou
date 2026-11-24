// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build zec && cgo

package zeclib

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
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
	var emptyTxid []byte
	for _, mined := range []*uint32{nil, &height} {
		id, err := be.decryptAndStoreTransaction(emptyTx, nil, mined)
		if err != nil || len(id) != 32 {
			t.Errorf("decryptAndStoreTransaction: %x, %v", id, err)
		}
		emptyTxid = id
	}
	if _, err := be.decryptAndStoreTransaction([]byte{1, 2, 3}, nil, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("decryptAndStoreTransaction of garbage: %v", err)
	}
	// A transaction is stored as the one asked for only if it has its ID.
	if id, err := be.decryptAndStoreTransaction(emptyTx, emptyTxid, &height); err != nil || !bytes.Equal(id, emptyTxid) {
		t.Errorf("decryptAndStoreTransaction with its own ID: %x, %v", id, err)
	}
	other := bytes.Repeat([]byte{0xbb}, 32)
	if _, err := be.decryptAndStoreTransaction(emptyTx, other, &height); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("decryptAndStoreTransaction with another ID: %v", err)
	}
	if _, err := be.decryptAndStoreTransaction(emptyTx, []byte{1}, &height); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("decryptAndStoreTransaction with a short ID: %v", err)
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
			_, err := be.decryptAndStoreTransaction(nil, nil, nil)
			return err
		},
		"notifyAddressChecked": func() error {
			return be.notifyAddressChecked(&txRequest{kind: txRequestAddress}, 1)
		},
		"scheduleNextCheck": func() error {
			_, _, err := be.scheduleNextCheck("x", time.Second)
			return err
		},
		"estimateTransfer": func() error {
			_, err := be.estimateTransfer(id, testRecipient, 1, nil, 0)
			return err
		},
		"send": func() error {
			_, err := be.send(id, testSeed, testRecipient, 1, nil, 1, 0)
			return err
		},
		"estimateSendMax": func() error {
			_, err := be.estimateSendMax(id, testRecipient, nil, 0)
			return err
		},
		"sendMax": func() error {
			_, err := be.sendMax(id, testSeed, testRecipient, nil, 1, 0, 0)
			return err
		},
		"spendable":            func() error { _, err := be.spendable(id, 0); return err },
		"rebroadcastInventory": func() error { _, err := be.rebroadcastInventory(); return err },
		"missingTransactions":  func() error { _, err := be.missingTransactions([][]byte{make([]byte, 32)}); return err },
		"transactions":         func() error { _, err := be.transactions(id, 0, 1); return err },
		"transaction":          func() error { _, _, err := be.transaction(id, make([]byte, 32)); return err },
		"openTransactions":     func() error { _, err := be.openTransactions(id, 1); return err },
		"ownsAddress":          func() error { _, _, err := be.ownsAddress(testRecipient); return err },
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
		{ffi.NewWalletErrorNotSynced("no anchor"), ErrNotSynced, "no anchor"},
		{ffi.NewWalletErrorTooLarge("3000000 bytes"), ErrTooLarge, "3000000 bytes"},
		{ffi.NewWalletErrorUnsupportedRecipient("a TEX address"), ErrUnsupportedRecipient, "a TEX address"},
		{ffi.NewWalletErrorMemoNotAllowed(), ErrMemoNotAllowed, ""},
		{ffi.NewWalletErrorSeedMismatch(), ErrSeedMismatch, ""},
		{ffi.NewWalletErrorSend("multi-step"), ErrSend, "multi-step"},
		{ffi.NewWalletErrorAddressExpired("at height 9"), ErrAddressExpired, "at height 9"},
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

	var funds *InsufficientFundsError
	if err := convertError(ffi.NewWalletErrorInsufficientFunds(5, 10_005)); !errors.As(err, &funds) ||
		funds.Available != 5 || funds.Required != 10_005 || !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("insufficient funds: %v", err)
	}
	var fee *FeeTooHighError
	if err := convertError(ffi.NewWalletErrorFeeTooHigh(15_000, 10_000)); !errors.As(err, &fee) ||
		fee.Fee != 15_000 || fee.MaxFee != 10_000 || !errors.Is(err, ErrFeeTooHigh) {
		t.Errorf("fee too high: %v", err)
	}
	var below *AmountBelowMinimumError
	if err := convertError(ffi.NewWalletErrorAmountBelowMinimum(54)); !errors.As(err, &below) ||
		below.Minimum != 54 || !errors.Is(err, ErrAmountBelowMinimum) {
		t.Errorf("amount below the minimum: %v", err)
	}
	if p, err := fromFFIPaidPool(ffi.PaidPoolIronwood); err != nil || p != PaidIronwood {
		t.Errorf("Ironwood: %v %v", p, err)
	}
	if p, err := fromFFIPaidPool(ffi.PaidPoolTransparent); err != nil || p != PaidTransparent {
		t.Errorf("transparent: %v %v", p, err)
	}
	if _, err := fromFFIPaidPool(ffi.PaidPool(99)); !errors.Is(err, ErrInternal) {
		t.Errorf("an unknown pool: %v", err)
	}
	var changed *AmountChangedError
	if err := convertError(ffi.NewWalletErrorAmountChanged(5, 6)); !errors.As(err, &changed) ||
		changed.Expected != 5 || changed.Actual != 6 || !errors.Is(err, ErrAmountChanged) {
		t.Errorf("amount changed: %v", err)
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
// the gap. The first of them holds an output that only the history of the
// address shows: the server's list of unspent outputs does not have it.
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
	if _, err := scratch.decryptAndStoreTransaction(paying, nil, nil); err != nil {
		t.Fatalf("decryptAndStoreTransaction: %v", err)
	}
	e.srv.set(func(s *fakeLwd) {
		s.addrTxs[address] = []*walletrpc.RawTransaction{{Data: paying, Height: 120}}
	})

	e.connect()
	e.waitSynced(200)
	eventually(t, "the output at the first address", func() bool {
		return transparentBalance(t, e) == value
	})
}

// TestFFISend runs the send calls of the Rust backend against the fake
// chain, whose blocks hold nothing: every payment fails on funds, after the
// checks that come first.
func TestFFISend(t *testing.T) {
	kind := backendKinds[len(backendKinds)-1]
	e := newEnv(t, kind, 150, nil)
	account := e.createAccount(100)
	e.connect()
	e.waitSynced(150)
	address, err := e.w.Address(account.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.w.EstimateTransfer(account.ID, address, 100_000, nil); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("estimate: %v", err)
	}
	if _, err := e.w.EstimateTransfer(account.ID, testRecipient, 100_000, []byte{}); !errors.Is(err, ErrMemoNotAllowed) {
		t.Errorf("an empty memo to a transparent address: %v", err)
	}
	var funds *InsufficientFundsError
	res, err := e.w.Send(context.Background(), account.ID, testSeed, address, 100_000, nil, 20_000)
	if !errors.As(err, &funds) || funds.Available != 0 || res != nil {
		t.Errorf("send: %+v, %v", res, err)
	}
	other := bytes.Repeat([]byte{9}, 32)
	if _, err := e.w.Send(context.Background(), account.ID, other, address, 100_000, nil, 20_000); !errors.Is(err, ErrSeedMismatch) {
		t.Errorf("send with another seed: %v", err)
	}
	if _, err := e.w.Send(context.Background(), account.ID, testSeed, "zregtestsapling1uwt3xqagtnemsucergl4f5lcjj0qsh4hu2avqs6gp3fq62yke38dcyxg5ddewwx5pnjrkfmrr6w", 1, nil, 20_000); !errors.Is(err, ErrUnsupportedRecipient) {
		t.Errorf("send to a Sapling address: %v", err)
	}
	if v, err := e.w.Spendable(account.ID); err != nil || v != (SpendableValue{}) {
		t.Errorf("spendable: %+v, %v", v, err)
	}
	if inv, err := e.be.rebroadcastInventory(); err != nil || len(inv) != 0 {
		t.Errorf("inventory: %d, %v", len(inv), err)
	}
	if _, err := e.w.EstimateSendMax(account.ID, address, nil); !errors.As(err, &funds) || funds.Available != 0 {
		t.Errorf("estimate send max: %v", err)
	}
	if res, err := e.w.SendMax(context.Background(), account.ID, testSeed, address, nil, 20_000, 0); !errors.Is(err, ErrInsufficientFunds) || res != nil {
		t.Errorf("send max: %+v, %v", res, err)
	}
	if _, err := e.w.SendMax(context.Background(), account.ID, other, address, nil, 20_000, 0); !errors.Is(err, ErrSeedMismatch) {
		t.Errorf("send max with another seed: %v", err)
	}
	if _, err := e.w.EstimateTransfer(account.ID, testRecipient, 53, nil); !errors.Is(err, ErrAmountBelowMinimum) {
		t.Errorf("53 zatoshis to a transparent address: %v", err)
	}
	if id, ok, err := e.w.OwnsAddress(address); err != nil || !ok || id != account.ID {
		t.Errorf("own address: %q %v %v", id, ok, err)
	}
	if _, ok, err := e.w.OwnsAddress(testRecipient); err != nil || ok {
		t.Errorf("foreign address: %v %v", ok, err)
	}
}

// TestEveryErrorIsMapped makes sure that every variant of the Rust error type
// has its own Go error. The variants are read from the constructors in the
// generated bindings, so a variant added to the Rust crate fails this test
// until it is mapped here and in convertError.
func TestEveryErrorIsMapped(t *testing.T) {
	src, err := os.ReadFile("internal/ffi/ffi.go")
	if err != nil {
		t.Fatal(err)
	}
	variants := regexp.MustCompile(`(?m)^func NewWalletError(\w+)\(`).FindAllStringSubmatch(string(src), -1)
	safe := uint32(1)
	mapped := map[string]struct {
		in   *ffi.WalletError
		want func(error) bool
	}{
		"InvalidArgument":      {ffi.NewWalletErrorInvalidArgument("x"), is(ErrInvalidArgument)},
		"Database":             {ffi.NewWalletErrorDatabase("x"), is(ErrDatabase)},
		"SeedRequired":         {ffi.NewWalletErrorSeedRequired(), is(ErrSeedRequired)},
		"SeedNotRelevant":      {ffi.NewWalletErrorSeedNotRelevant(), is(ErrSeedNotRelevant)},
		"AccountNotFound":      {ffi.NewWalletErrorAccountNotFound(), is(ErrAccountNotFound)},
		"AddressGapLimit":      {ffi.NewWalletErrorAddressGapLimit("x"), is(ErrAddressGapLimit)},
		"ChainContinuity":      {ffi.NewWalletErrorChainContinuity(1, "x"), as[*chainContinuityError]},
		"RewindInvalid":        {ffi.NewWalletErrorRewindInvalid(1, &safe), as[*rewindError]},
		"Scan":                 {ffi.NewWalletErrorScan(1, "x"), as[*scanError]},
		"InsufficientFunds":    {ffi.NewWalletErrorInsufficientFunds(1, 2), is(ErrInsufficientFunds)},
		"NotSynced":            {ffi.NewWalletErrorNotSynced("x"), is(ErrNotSynced)},
		"FeeTooHigh":           {ffi.NewWalletErrorFeeTooHigh(2, 1), is(ErrFeeTooHigh)},
		"UnsupportedRecipient": {ffi.NewWalletErrorUnsupportedRecipient("x"), is(ErrUnsupportedRecipient)},
		"TooLarge":             {ffi.NewWalletErrorTooLarge("x"), is(ErrTooLarge)},
		"AmountChanged":        {ffi.NewWalletErrorAmountChanged(1, 2), is(ErrAmountChanged)},
		"AddressExpired":       {ffi.NewWalletErrorAddressExpired("x"), is(ErrAddressExpired)},
		"AmountBelowMinimum":   {ffi.NewWalletErrorAmountBelowMinimum(54), is(ErrAmountBelowMinimum)},
		"MemoNotAllowed":       {ffi.NewWalletErrorMemoNotAllowed(), is(ErrMemoNotAllowed)},
		"SeedMismatch":         {ffi.NewWalletErrorSeedMismatch(), is(ErrSeedMismatch)},
		"Send":                 {ffi.NewWalletErrorSend("x"), is(ErrSend)},
		"Internal":             {ffi.NewWalletErrorInternal("x"), is(ErrInternal)},
	}
	if len(variants) == 0 {
		t.Fatal("no error constructors found in the bindings")
	}
	for _, v := range variants {
		name := v[1]
		m, ok := mapped[name]
		if !ok {
			t.Errorf("WalletError variant %s has no case in this test", name)
			continue
		}
		err := convertError(m.in)
		if !m.want(err) {
			t.Errorf("WalletError variant %s converts to %v", name, err)
		}
		if name != "Internal" && errors.Is(err, ErrInternal) {
			t.Errorf("WalletError variant %s is not mapped: %v", name, err)
		}
	}
}

func is(target error) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) }
}

func as[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

// A coinbase output paid to a wallet address is recorded through the funding
// fetch and is never selectable by a payment, while a regular output beside
// it is.
func TestFFICoinbaseOutputIsNotSpendable(t *testing.T) {
	kind := backendKinds[len(backendKinds)-1]
	e, address := fundEnv(t, kind)
	plantCoinbase(t, e, kind, address, 50_000, 120)
	plantFunded(t, e, kind, address, 30_000, 121, 9)
	e.connect()
	e.waitSynced(150)
	eventually(t, "both outputs recorded", func() bool { return transparentBalance(t, e) == 80_000 })
	v, err := e.w.Spendable(e.account.ID)
	if err != nil || v.Transparent != 30_000 {
		t.Fatalf("spendable: %+v, %v", v, err)
	}
	summary, err := e.w.Summary()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := summary.Balance(e.account.ID)
	if b.Transparent.Total != 30_000 || b.Coinbase.Total != 50_000 || b.Total != 80_000 {
		t.Errorf("the summary does not keep coinbase apart: %+v", b)
	}
}

func TestFFIHistoryAndParse(t *testing.T) {
	kind := backendKinds[len(backendKinds)-1]
	e, address := fundEnv(t, kind)
	txid := plantFunded(t, e, kind, address, 70_000, 120, 4)
	e.connect()
	e.waitSynced(150)
	eventually(t, "the output recorded", func() bool { return transparentBalance(t, e) == 70_000 })

	listing, err := e.w.Transactions(e.account.ID, 0, 10)
	if err != nil || listing.ChainTip != 150 || len(listing.Records) != 1 {
		t.Fatalf("Transactions: %+v, %v", listing, err)
	}
	r := listing.Records[0]
	if r.TxID != displayTxID(txid) || r.MinedHeight == nil || *r.MinedHeight != 120 || r.BlockTime == nil ||
		!r.RawPresent || r.CreatedByWallet || r.Fee != nil || r.TotalReceived != 70_000 {
		t.Errorf("record: %+v", r)
	}
	if len(r.Outputs) != 1 || r.Outputs[0].Pool != OutputTransparent || r.Outputs[0].Address != address || !r.Outputs[0].ToWallet {
		t.Errorf("outputs: %+v", r.Outputs)
	}
	got, tip, err := e.w.Transaction(e.account.ID, r.TxID)
	if err != nil || got == nil || got.Seq != r.Seq || tip != 150 {
		t.Errorf("Transaction: %+v %d %v", got, tip, err)
	}
	if got, _, err := e.w.Transaction(e.account.ID, strings.Repeat("00", 32)); err != nil || got != nil {
		t.Errorf("an unknown transaction: %+v %v", got, err)
	}
	if _, _, err := e.w.Transaction(e.account.ID, "zz"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a malformed ID: %v", err)
	}
	open, err := e.w.OpenTransactions(e.account.ID, 31)
	if err != nil || len(open.Records) != 1 {
		t.Errorf("open at 31 confirmations: %+v, %v", open, err)
	}
	if open, err := e.w.OpenTransactions(e.account.ID, 30); err != nil || len(open.Records) != 0 {
		t.Errorf("open at 30: %+v, %v", open, err)
	}

	unified, err := e.w.Address(e.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseAddress(testNetwork, unified)
	if err != nil || p.Kind != AddressUnified || !p.Payable || p.PaidPool != PaidIronwood || len(p.Receivers) != 2 {
		t.Fatalf("ParseAddress: %+v, %v", p, err)
	}
	if p.Receivers[0].Kind != ReceiverOrchard || p.Receivers[1].Kind != ReceiverTransparent || p.Receivers[1].Address != address {
		t.Errorf("receivers: %+v", p.Receivers)
	}
	if p.ExpiryHeight != nil || p.ExpiryTime != nil {
		t.Errorf("an address without an expiry has one: %v %v", p.ExpiryHeight, p.ExpiryTime)
	}
	if id, ok, err := e.w.OwnsAddress(p.Receivers[0].Address); err != nil || !ok || id != e.account.ID {
		t.Errorf("the Orchard receiver is not the wallet's: %q %v %v", id, ok, err)
	}
	p, err = ParseAddress(testNetwork, address)
	if err != nil || p.Kind != AddressTransparent || p.PaidPool != PaidTransparent || p.PaidAddress != address {
		t.Errorf("a transparent address: %+v, %v", p, err)
	}
	if _, err := ParseAddress(testNetwork, "nonsense"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("junk: %v", err)
	}
}
