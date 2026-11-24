// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

// fakeBackend is an in-memory backend that models the parts of the wallet
// database the sync code depends on: the scan queue, chain continuity
// checks, and rewinding. It holds no keys and finds no notes.
//
// Any method can be made to fail by setting an error for its name in fail.
type fakeBackend struct {
	mtx    sync.Mutex
	closed bool

	accts []Account
	// tip is the chain tip the wallet was told, valid if hasTip.
	tip    uint32
	hasTip bool
	// scanned maps the height of each scanned block to its hash.
	scanned map[uint32][]byte
	// minRewind is the lowest height the wallet can rewind to.
	minRewind uint32
	// noSafeRewind makes an invalid rewind report no safe height.
	noSafeRewind bool
	// rewinds records the height of each rewind request.
	rewinds []uint32

	// recoverUntil records the argument of each createAccount call.
	recoverUntil []*uint32

	// branchFrom, if not zero, is the height from which the wallet has
	// laterBranch as its consensus branch. Below it, and without it, the
	// branch is fakeBranch.
	branchFrom uint32

	// used holds, by account ID, how many addresses at the start of the
	// receive chain and of the change chain end with a used one.
	used map[string][2]uint32
	// spent holds the outputs that stored transactions spend, as
	// transaction ID and index.
	spent map[string]struct{}

	roots     []*subtreeRoots
	utxos     []utxo
	requests  []txRequest
	statuses  map[string]txStatus
	stored    []storedTx
	notified  []notifiedCheck
	scheduled map[string]time.Duration
	// created holds the transactions send created. sendFee is the fee of
	// every send and estimate, expiryDelta the number of blocks above the
	// chain tip at which a created transaction expires (zero: never), and
	// unreadable makes send report its transaction without its raw bytes.
	created     []fakeCreated
	sendFee     uint64
	expiryDelta uint32
	unreadable  bool
	// spendableValue is what spendable reports. A send max sends it less
	// leftBehind and the fee, and reports leftBehind as left. txsPerSend is
	// the number of transactions a send creates, one if zero.
	spendableValue SpendableValue
	leftBehind     LeftBehind
	txsPerSend     int

	// onScan, if set, is called with the backend locked at the start of
	// each scanBlocks call.
	onScan func(b *fakeBackend)

	// stuck makes scanBlocks accept a batch without recording it.
	stuck bool
	// batches records the first and last height of each scanned batch.
	batches [][2]uint32

	// fail maps a method name to the error it returns.
	fail map[string]error
	// failOnce maps a method name to an error it returns one time.
	failOnce map[string]error
	calls    map[string]int
}

type storedTx struct {
	data   []byte
	height *uint32
}

type notifiedCheck struct {
	req  txRequest
	asOf uint32
}

var _ backend = (*fakeBackend)(nil)

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		scanned:   make(map[uint32][]byte),
		statuses:  make(map[string]txStatus),
		scheduled: make(map[string]time.Duration),
		used:      make(map[string][2]uint32),
		spent:     make(map[string]struct{}),
		fail:      make(map[string]error),
		failOnce:  make(map[string]error),
		calls:     make(map[string]int),
	}
}

// enter records a call and returns the error the test set for the method.
// The caller holds mtx.
func (b *fakeBackend) enter(method string) error {
	b.calls[method]++
	if b.closed {
		return ErrClosed
	}
	if err, ok := b.failOnce[method]; ok {
		delete(b.failOnce, method)
		return err
	}
	return b.fail[method]
}

// set runs f with the backend locked.
func (b *fakeBackend) set(f func(b *fakeBackend)) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	f(b)
}

func (b *fakeBackend) callCount(method string) int {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return b.calls[method]
}

// birthdayLocked returns the earliest account birthday.
func (b *fakeBackend) birthdayLocked() (uint32, bool) {
	if len(b.accts) == 0 {
		return 0, false
	}
	birthday := b.accts[0].Birthday
	for _, a := range b.accts[1:] {
		birthday = min(birthday, a.Birthday)
	}
	return birthday, true
}

func (b *fakeBackend) maxScannedLocked() (uint32, bool) {
	var highest uint32
	for h := range b.scanned {
		highest = max(highest, h)
	}
	return highest, len(b.scanned) > 0
}

// fakeCreated is a transaction the fake backend created.
type fakeCreated struct {
	txid   []byte
	raw    []byte
	expiry uint32
	// minedAt is the height the transaction is mined at, zero while it is
	// unmined.
	minedAt uint32
}

// fakeTxPrefix starts every raw transaction the fake backend creates. The
// fake server takes the transaction ID from the bytes that follow it.
const fakeTxPrefix = "faketx:"

// fakeTxID returns the ID of a raw transaction made by the fake backend.
func fakeTxID(raw []byte) ([]byte, bool) {
	if !bytes.HasPrefix(raw, []byte(fakeTxPrefix)) {
		return nil, false
	}
	return raw[len(fakeTxPrefix):], true
}

func (b *fakeBackend) estimateTransfer(accountID, address string, _ uint64, _ []byte, _ uint32) (*TransferEstimate, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("estimateTransfer"); err != nil {
		return nil, err
	}
	if !b.hasAccountLocked(accountID) {
		return nil, ErrAccountNotFound
	}
	return &TransferEstimate{Fee: b.sendFee, PaidAddress: address, PaidPool: PaidTransparent}, nil
}

// send creates a transaction that expires expiryDelta blocks above the
// chain tip, or never if expiryDelta is zero.
func (b *fakeBackend) send(accountID string, _ []byte, address string, _ uint64, _ []byte, maxFee uint64, _ uint32) (*sendResult, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("send"); err != nil {
		return nil, err
	}
	if !b.hasAccountLocked(accountID) {
		return nil, ErrAccountNotFound
	}
	if b.sendFee > maxFee {
		return nil, &FeeTooHighError{Fee: b.sendFee, MaxFee: maxFee}
	}
	return b.createLocked(address), nil
}

// createLocked creates the transactions of one send: txsPerSend of them, or
// one. The caller holds mtx.
func (b *fakeBackend) createLocked(address string) *sendResult {
	res := &sendResult{fee: b.sendFee, paidAddress: address, paidPool: PaidTransparent}
	for range max(b.txsPerSend, 1) {
		sum := sha256.Sum256(fmt.Appendf(nil, "fake transaction %d", len(b.created)))
		tx := fakeCreated{txid: sum[:], raw: append([]byte(fakeTxPrefix), sum[:]...)}
		if b.expiryDelta > 0 {
			tx.expiry = b.tip + b.expiryDelta
		}
		b.created = append(b.created, tx)
		created := createdTx{txid: tx.txid, raw: tx.raw}
		if b.unreadable {
			created.raw = nil
			res.readErr = "the transaction cannot be read back"
		}
		res.transactions = append(res.transactions, created)
	}
	return res
}

// sendMaxAmountLocked is what a send max would pay, as the real backend
// computes it: the spendable value, less what one transaction cannot take,
// less the fee, at least one zatoshi. The caller holds mtx.
func (b *fakeBackend) sendMaxAmountLocked() (uint64, error) {
	total := b.spendableValue.Ironwood + b.spendableValue.Transparent
	left := b.leftBehind.TransparentValue + b.leftBehind.NoteValue
	if left > total {
		panic("the fake leaves behind more than it holds")
	}
	taken := total - left
	if taken < b.sendFee+1 {
		return 0, &InsufficientFundsError{Available: taken, Required: b.sendFee + 1}
	}
	return taken - b.sendFee, nil
}

func (b *fakeBackend) estimateSendMax(accountID, address string, _ []byte, _ uint32) (*SendMaxEstimate, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("estimateSendMax"); err != nil {
		return nil, err
	}
	if !b.hasAccountLocked(accountID) {
		return nil, ErrAccountNotFound
	}
	amount, err := b.sendMaxAmountLocked()
	if err != nil {
		return nil, err
	}
	return &SendMaxEstimate{
		Amount:      amount,
		Fee:         b.sendFee,
		PaidAddress: address,
		PaidPool:    PaidTransparent,
		LeftBehind:  b.leftBehind,
	}, nil
}

func (b *fakeBackend) sendMax(accountID string, _ []byte, address string, _ []byte, maxFee, expectedAmount uint64, _ uint32) (*sendMaxResult, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("sendMax"); err != nil {
		return nil, err
	}
	if !b.hasAccountLocked(accountID) {
		return nil, ErrAccountNotFound
	}
	amount, err := b.sendMaxAmountLocked()
	if err != nil {
		return nil, err
	}
	if b.sendFee > maxFee {
		return nil, &FeeTooHighError{Fee: b.sendFee, MaxFee: maxFee}
	}
	if expectedAmount != 0 && expectedAmount != amount {
		return nil, &AmountChangedError{Expected: expectedAmount, Actual: amount}
	}
	return &sendMaxResult{
		sendResult: *b.createLocked(address),
		amount:     amount,
		leftBehind: b.leftBehind,
	}, nil
}

func (b *fakeBackend) spendable(accountID string, _ uint32) (SpendableValue, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("spendable"); err != nil {
		return SpendableValue{}, err
	}
	if !b.hasAccountLocked(accountID) {
		return SpendableValue{}, ErrAccountNotFound
	}
	return b.spendableValue, nil
}

func (b *fakeBackend) rebroadcastInventory() ([]unminedTx, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("rebroadcastInventory"); err != nil {
		return nil, err
	}
	var txs []unminedTx
	for _, tx := range b.created {
		if tx.minedAt != 0 || (b.hasTip && tx.expiry != 0 && tx.expiry <= b.tip) {
			continue
		}
		txs = append(txs, unminedTx{txid: tx.txid, raw: tx.raw, expiryHeight: tx.expiry})
	}
	return txs, nil
}

// createdTxs returns the transactions the backend created.
func (b *fakeBackend) createdTxs() []fakeCreated {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return slices.Clone(b.created)
}

func (b *fakeBackend) ownsAddress(string) (string, bool, error) {
	return "", false, wrapf(ErrInternal, "the fake backend does not resolve addresses")
}

func (b *fakeBackend) close() error {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	b.closed = true
	return nil
}

func (b *fakeBackend) createAccount(name string, seed, treeState []byte, recoverUntil *uint32, keySource string) (*Account, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("createAccount"); err != nil {
		return nil, err
	}
	if len(seed) < 32 || len(seed) > 252 {
		return nil, wrapf(ErrInvalidArgument, "a seed is 32 to 252 bytes, got %d", len(seed))
	}
	var state walletrpc.TreeState
	if err := proto.Unmarshal(treeState, &state); err != nil {
		return nil, wrapf(ErrInvalidArgument, "tree state: %v", err)
	}
	index := uint32(len(b.accts))
	account := Account{
		ID:              fmt.Sprintf("00000000-0000-0000-0000-%012d", index),
		Name:            name,
		Birthday:        uint32(state.Height) + 1,
		UFVK:            fmt.Sprintf("uviewfake%d", index),
		Derived:         true,
		HDIndex:         index,
		SeedFingerprint: bytes.Repeat([]byte{seed[0]}, 32),
		KeySource:       keySource,
	}
	b.accts = append(b.accts, account)
	b.recoverUntil = append(b.recoverUntil, recoverUntil)
	if b.minRewind == 0 || uint32(state.Height) < b.minRewind {
		b.minRewind = uint32(state.Height)
	}
	return &account, nil
}

func (b *fakeBackend) accounts() ([]Account, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("accounts"); err != nil {
		return nil, err
	}
	return slices.Clone(b.accts), nil
}

func (b *fakeBackend) isSeedRelevant(seed []byte) (bool, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("isSeedRelevant"); err != nil {
		return false, err
	}
	for _, a := range b.accts {
		if len(seed) > 0 && a.SeedFingerprint[0] == seed[0] {
			return true, nil
		}
	}
	return false, nil
}

func (b *fakeBackend) hasAccountLocked(id string) bool {
	return slices.ContainsFunc(b.accts, func(a Account) bool { return a.ID == id })
}

func (b *fakeBackend) currentAddress(accountID string) (string, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("currentAddress"); err != nil {
		return "", err
	}
	if !b.hasAccountLocked(accountID) {
		return "", ErrAccountNotFound
	}
	return "ufake-" + accountID, nil
}

func (b *fakeBackend) nextAvailableAddress(accountID string, receivers ReceiverSet) (string, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("nextAvailableAddress"); err != nil {
		return "", err
	}
	if !b.hasAccountLocked(accountID) {
		return "", ErrAccountNotFound
	}
	return fmt.Sprintf("ufake-%s-%d-%d", accountID, receivers, b.calls["nextAvailableAddress"]), nil
}

func (b *fakeBackend) transparentReceivers(accountID string) ([]watchedAddress, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("transparentReceivers"); err != nil {
		return nil, err
	}
	if !b.hasAccountLocked(accountID) {
		return nil, ErrAccountNotFound
	}
	return b.watchedLocked(accountID), nil
}

// The fake backend watches transparent addresses the way the wallet
// database does: on each chain of an account, every address up to the last
// used one and a run of unused ones after it.
var fakeGaps = [2]uint32{10, 5}

// fakeAddress is a transparent address of an account of the fake backend.
func fakeAddress(accountID string, change bool, index uint32) string {
	chain := "r"
	if change {
		chain = "c"
	}
	return fmt.Sprintf("tfake-%s-%s%d", accountID, chain, index)
}

func (b *fakeBackend) watchedLocked(accountID string) []watchedAddress {
	var watched []watchedAddress
	for chain, gap := range fakeGaps {
		for i := uint32(0); i < b.used[accountID][chain]+gap; i++ {
			change := chain == 1
			watched = append(watched, watchedAddress{
				address: fakeAddress(accountID, change, i),
				change:  change,
				index:   i,
				unused:  i >= b.used[accountID][chain],
			})
		}
	}
	return watched
}

// markUsedLocked records that an output with the given script was stored.
func (b *fakeBackend) markUsedLocked(script []byte) {
	for i := range b.accts {
		id := b.accts[i].ID
		for _, a := range b.watchedLocked(id) {
			if !bytes.Equal(p2pkhScript(a.address), script) {
				continue
			}
			used := b.used[id]
			chain := 0
			if a.change {
				chain = 1
			}
			used[chain] = max(used[chain], a.index+1)
			b.used[id] = used
		}
	}
}

func (b *fakeBackend) summary(uint32) (*Summary, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("summary"); err != nil {
		return nil, err
	}
	birthday, ok := b.birthdayLocked()
	if !ok || !b.hasTip || len(b.scanned) == 0 {
		return nil, nil
	}
	fully := birthday - 1
	for {
		if _, ok := b.scanned[fully+1]; !ok {
			break
		}
		fully++
	}
	summary := &Summary{
		ChainTip:     b.tip,
		FullyScanned: fully,
		ScanProgress: Ratio{
			Numerator:   uint64(len(b.scanned)),
			Denominator: uint64(b.tip - min(b.tip, birthday) + 1),
		},
	}
	for _, a := range b.accts {
		var transparent uint64
		for _, u := range b.utxos {
			transparent += u.value
		}
		summary.Balances = append(summary.Balances, AccountBalance{
			AccountID:   a.ID,
			Transparent: PoolBalance{Spendable: transparent, Total: transparent},
			Total:       transparent,
		})
	}
	if len(b.roots) > 0 {
		last := b.roots[len(b.roots)-1]
		summary.nextSubtree = last.start + uint64(len(last.roots))
	}
	return summary, nil
}

func (b *fakeBackend) birthday() (uint32, bool, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("birthday"); err != nil {
		return 0, false, err
	}
	birthday, ok := b.birthdayLocked()
	return birthday, ok, nil
}

func (b *fakeBackend) maxScannedHeight() (uint32, bool, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("maxScannedHeight"); err != nil {
		return 0, false, err
	}
	h, ok := b.maxScannedLocked()
	return h, ok, nil
}

func (b *fakeBackend) blockHash(height uint32) ([]byte, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("blockHash"); err != nil {
		return nil, err
	}
	return b.scanned[height], nil
}

func (b *fakeBackend) putSubtreeRoots(roots *subtreeRoots) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("putSubtreeRoots"); err != nil {
		return err
	}
	copied := *roots
	b.roots = append(b.roots, &copied)
	return nil
}

func (b *fakeBackend) updateChainTip(height uint32) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("updateChainTip"); err != nil {
		return err
	}
	// Like the real wallet, the tip never moves down: a lower tip is not
	// taken as a reorganization, and the ranges already queued above it
	// stay queued.
	if b.hasTip && height < b.tip {
		return nil
	}
	b.tip, b.hasTip = height, true
	return nil
}

func (b *fakeBackend) suggestScanRanges() ([]scanRange, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("suggestScanRanges"); err != nil {
		return nil, err
	}
	birthday, ok := b.birthdayLocked()
	if !ok || !b.hasTip {
		return nil, nil
	}
	// One range per run of unscanned heights, the highest run first, as the
	// wallet prefers the blocks nearest the tip.
	var ranges []scanRange
	for h := birthday; h <= b.tip; h++ {
		if _, done := b.scanned[h]; done {
			continue
		}
		start := h
		for h <= b.tip {
			if _, done := b.scanned[h]; done {
				break
			}
			h++
		}
		ranges = append(ranges, scanRange{start: start, end: h, priority: priorityHistoric})
	}
	slices.Reverse(ranges)
	if len(ranges) > 0 {
		ranges[0].priority = priorityChainTip
	}
	return ranges, nil
}

func (b *fakeBackend) scanBlocks(fromState []byte, blocks [][]byte) (*scanSummary, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("scanBlocks"); err != nil {
		return nil, err
	}
	if b.onScan != nil {
		b.onScan(b)
	}
	var state walletrpc.TreeState
	if err := proto.Unmarshal(fromState, &state); err != nil {
		return nil, wrapf(ErrInvalidArgument, "tree state: %v", err)
	}
	if len(blocks) == 0 {
		return nil, wrapf(ErrInvalidArgument, "no blocks to scan")
	}
	stateHash, err := hex.DecodeString(state.Hash)
	if err != nil {
		return nil, wrapf(ErrInvalidArgument, "tree state: %v", err)
	}
	slices.Reverse(stateHash)

	// The parent of the first block is the scanned block below it, if the
	// wallet has one, and otherwise the block of the tree state.
	height := uint32(state.Height)
	parent := stateHash
	if stored, ok := b.scanned[height]; ok {
		parent = stored
	}
	batch := make(map[uint32][]byte, len(blocks))
	for i, encoded := range blocks {
		var block walletrpc.CompactBlock
		if err := proto.Unmarshal(encoded, &block); err != nil {
			return nil, wrapf(ErrInvalidArgument, "compact block %d: %v", i, err)
		}
		height++
		if block.Height != uint64(height) {
			if i == 0 {
				return nil, wrapf(ErrInvalidArgument, "the first block is at height %d", block.Height)
			}
			return nil, &chainContinuityError{height: uint32(block.Height), reason: "height gap"}
		}
		if !bytes.Equal(block.PrevHash, parent) {
			return nil, &chainContinuityError{height: height, reason: "previous hash mismatch"}
		}
		batch[height] = block.Hash
		parent = block.Hash
	}
	if !b.stuck {
		maps.Copy(b.scanned, batch)
	}
	b.batches = append(b.batches, [2]uint32{uint32(state.Height) + 1, height})
	return &scanSummary{
		start: uint32(state.Height) + 1,
		end:   height + 1,
	}, nil
}

func (b *fakeBackend) rewindToHeight(height uint32) (uint32, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	b.rewinds = append(b.rewinds, height)
	if err := b.enter("rewindToHeight"); err != nil {
		return 0, err
	}
	highest, ok := b.maxScannedLocked()
	if ok && height < highest && height < b.minRewind {
		return 0, &rewindError{requested: height, safe: b.minRewind, hasSafe: !b.noSafeRewind}
	}
	// A transaction mined above the rewind height is unmined again.
	for i := range b.created {
		if b.created[i].minedAt > height {
			b.created[i].minedAt = 0
		}
	}
	if !ok || height >= highest {
		return highest, nil
	}
	for h := range b.scanned {
		if h > height {
			delete(b.scanned, h)
		}
	}
	// Rewinding shortens the scan queue to the rewind height.
	b.tip = height
	return height, nil
}

// fakeBranch is the consensus branch of the fake backend, and laterBranch
// the one it has from fakeBackend.branchFrom on.
const (
	fakeBranch  = 0x37a5165b
	laterBranch = 0x77190ad9
)

func (b *fakeBackend) consensusBranchID(height uint32) (uint32, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("consensusBranchID"); err != nil {
		return 0, err
	}
	if b.branchFrom != 0 && height >= b.branchFrom {
		return laterBranch, nil
	}
	return fakeBranch, nil
}

func (b *fakeBackend) utxoQueryHeight(accountID string) (uint32, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("utxoQueryHeight"); err != nil {
		return 0, err
	}
	if !b.hasAccountLocked(accountID) {
		return 0, ErrAccountNotFound
	}
	birthday, _ := b.birthdayLocked()
	return birthday, nil
}

func (b *fakeBackend) missingTransactions(txids [][]byte) ([][]byte, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("missingTransactions"); err != nil {
		return nil, err
	}
	held := make(map[string]struct{})
	for _, tx := range b.stored {
		id := sha256.Sum256(tx.data)
		held[string(id[:])] = struct{}{}
	}
	for _, tx := range b.created {
		held[string(tx.txid)] = struct{}{}
	}
	var missing [][]byte
	for _, txid := range txids {
		if _, ok := held[string(txid)]; !ok {
			missing = append(missing, txid)
		}
	}
	return missing, nil
}

// The fake backend keeps no history: its listings are empty.
func (b *fakeBackend) transactions(accountID string, _ uint64, _ uint32) (*TxListing, error) {
	return b.emptyListing("transactions", accountID)
}

func (b *fakeBackend) transaction(accountID string, _ []byte) (*TxRecord, uint32, error) {
	l, err := b.emptyListing("transaction", accountID)
	if err != nil {
		return nil, 0, err
	}
	return nil, l.ChainTip, nil
}

func (b *fakeBackend) openTransactions(accountID string, _ uint32) (*TxListing, error) {
	return b.emptyListing("openTransactions", accountID)
}

func (b *fakeBackend) emptyListing(method, accountID string) (*TxListing, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter(method); err != nil {
		return nil, err
	}
	if !b.hasAccountLocked(accountID) {
		return nil, ErrAccountNotFound
	}
	return &TxListing{ChainTip: b.tip}, nil
}

func (b *fakeBackend) putUTXO(u *utxo) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("putUTXO"); err != nil {
		return err
	}
	// As the Rust backend, an output is checked against its transaction
	// when the transaction is held.
	for _, tx := range b.stored {
		if id := sha256.Sum256(tx.data); !bytes.Equal(id[:], u.txid) {
			continue
		}
		if _, pays, ok := parseTestTx(tx.data); ok {
			if int(u.index) >= len(pays) || pays[u.index].value != u.value || !bytes.Equal(pays[u.index].script, u.script) {
				return fmt.Errorf("%w: the transaction has no such output", ErrInvalidArgument)
			}
		}
		break
	}
	b.markUsedLocked(u.script)
	if _, gone := b.spent[fmt.Sprintf("%x:%d", u.txid, u.index)]; gone {
		return nil
	}
	for i := range b.utxos {
		if bytes.Equal(b.utxos[i].txid, u.txid) && b.utxos[i].index == u.index {
			b.utxos[i] = *u
			return nil
		}
	}
	b.utxos = append(b.utxos, *u)
	return nil
}

func (b *fakeBackend) transactionDataRequests() ([]txRequest, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("transactionDataRequests"); err != nil {
		return nil, err
	}
	return slices.Clone(b.requests), nil
}

func (b *fakeBackend) setTransactionStatus(txid []byte, status txStatus) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("setTransactionStatus"); err != nil {
		return err
	}
	b.statuses[string(txid)] = status
	// A created transaction reported mined leaves the inventory until a
	// rewind below its height.
	for i := range b.created {
		if bytes.Equal(b.created[i].txid, txid) && status.kind == txMined {
			b.created[i].minedAt = status.height
		}
	}
	return nil
}

// fakeTxid returns the ID that the fake backend gives a transaction.
func fakeTxid(tx []byte) []byte {
	id := sha256.Sum256(tx)
	return id[:]
}

func (b *fakeBackend) decryptAndStoreTransaction(tx, txid []byte, minedHeight *uint32) ([]byte, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("decryptAndStoreTransaction"); err != nil {
		return nil, err
	}
	id := fakeTxid(tx)
	if txid != nil && !bytes.Equal(txid, id) {
		return nil, wrapf(ErrInvalidArgument, "the transaction is %x, not %x", id, txid)
	}
	b.stored = append(b.stored, storedTx{data: tx, height: minedHeight})
	// A transaction made by testTx uses the addresses it pays, holds
	// outputs for them, and spends the outputs it names.
	if spends, pays, ok := parseTestTx(tx); ok {
		watched := make(map[string]struct{})
		for i := range b.accts {
			for _, a := range b.watchedLocked(b.accts[i].ID) {
				watched[string(p2pkhScript(a.address))] = struct{}{}
			}
		}
		for index, out := range pays {
			if _, ok := watched[string(out.script)]; !ok {
				continue
			}
			b.markUsedLocked(out.script)
			known := slices.ContainsFunc(b.utxos, func(u utxo) bool {
				return bytes.Equal(u.txid, id) && u.index == uint32(index)
			})
			if !known && minedHeight != nil {
				b.utxos = append(b.utxos, utxo{
					txid: id, index: uint32(index), script: out.script, value: out.value, height: *minedHeight,
				})
			}
		}
		for _, in := range spends {
			b.spent[fmt.Sprintf("%x:%d", in.txid, in.index)] = struct{}{}
		}
		b.utxos = slices.DeleteFunc(b.utxos, func(u utxo) bool {
			_, gone := b.spent[fmt.Sprintf("%x:%d", u.txid, u.index)]
			return gone
		})
	}
	return id, nil
}

func (b *fakeBackend) notifyAddressChecked(req *txRequest, asOfHeight uint32) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("notifyAddressChecked"); err != nil {
		return err
	}
	b.notified = append(b.notified, notifiedCheck{req: *req, asOf: asOfHeight})
	return nil
}

func (b *fakeBackend) scheduleNextCheck(address string, offset time.Duration) (time.Time, bool, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.enter("scheduleNextCheck"); err != nil {
		return time.Time{}, false, err
	}
	b.scheduled[address] = offset
	return time.Now().Add(offset), true, nil
}
