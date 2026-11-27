// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// chain is the history and send behavior the txFake backends of a test
// share, as the opens of one database would.
type chain struct {
	mtx     sync.Mutex
	records map[string]*zeclib.TxRecord
	nextSeq uint64

	// estimate answers EstimateTransfer for an amount.
	estimate func(amount uint64) (*zeclib.TransferEstimate, error)
	// sendMax answers EstimateSendMax.
	sendMax    *zeclib.SendMaxEstimate
	sendMaxErr error
	// sendErr fails Send and SendMax before anything is created, and
	// broadcastErr is the outcome of the first broadcast of one that is.
	sendErr, broadcastErr error
	// sendHook, if set, runs in Send and SendMax before the transaction is
	// created.
	sendHook func()
	// sent records the sends: amount, max fee and expected amount.
	sent []sentCall
	// sentRecord, if set, is the record a send creates, given its ID.
	sentRecord func(txid string, amount uint64) *zeclib.TxRecord

	estimates []uint64
	lookups   int

	// listHook, if set, runs at the start of each Transactions call with
	// its afterSeq, and listErr fails the next one.
	listHook func(afterSeq uint64)
	listErr  error
}

type sentCall struct {
	sendMax                  bool
	amount, maxFee, expected uint64
	seed                     []byte
}

func newChain() *chain {
	return &chain{records: make(map[string]*zeclib.TxRecord)}
}

// put adds or replaces a record. A new one gets the next Seq.
func (c *chain) put(r zeclib.TxRecord) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if old, ok := c.records[r.TxID]; ok {
		r.Seq = old.Seq
	} else if r.Seq == 0 {
		c.nextSeq++
		r.Seq = c.nextSeq
	} else {
		c.nextSeq = max(c.nextSeq, r.Seq)
	}
	c.records[r.TxID] = &r
}

func (c *chain) sorted() []zeclib.TxRecord {
	out := make([]zeclib.TxRecord, 0, len(c.records))
	for _, r := range c.records {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// txFake is a fakeBackend with the history and the sends of a chain.
type txFake struct {
	*fakeBackend
	c *chain
}

func (b *txFake) tip() uint32 {
	return b.SyncStatus().ChainTip
}

func (b *txFake) Transactions(_ string, afterSeq uint64, limit uint32) (*zeclib.TxListing, error) {
	tip := b.tip()
	b.c.mtx.Lock()
	hook, err := b.c.listHook, b.c.listErr
	b.c.listErr = nil
	b.c.mtx.Unlock()
	if hook != nil {
		hook(afterSeq)
	}
	if err != nil {
		return nil, err
	}
	b.c.mtx.Lock()
	defer b.c.mtx.Unlock()
	l := &zeclib.TxListing{ChainTip: tip}
	for _, r := range b.c.sorted() {
		if r.Seq > afterSeq && len(l.Records) < int(limit) {
			l.Records = append(l.Records, r)
		}
	}
	return l, nil
}

func (b *txFake) Transaction(_ string, txid string) (*zeclib.TxRecord, uint32, error) {
	tip := b.tip()
	b.c.mtx.Lock()
	defer b.c.mtx.Unlock()
	b.c.lookups++
	if r, ok := b.c.records[txid]; ok {
		cp := *r
		return &cp, tip, nil
	}
	return nil, tip, nil
}

// OpenTransactions applies zeclib's criteria.
func (b *txFake) OpenTransactions(_ string, depth uint32) (*zeclib.TxListing, error) {
	tip := b.tip()
	b.c.mtx.Lock()
	defer b.c.mtx.Unlock()
	l := &zeclib.TxListing{ChainTip: tip}
	for _, r := range b.c.sorted() {
		// At most depth confirmations, which a row mined above the tip
		// has as well.
		open := r.MinedHeight == nil || *r.MinedHeight+depth > tip || r.BlockTime == nil ||
			!r.RawPresent || (r.TotalSpent > 0 && r.Fee == nil)
		if open {
			l.Records = append(l.Records, r)
		}
	}
	return l, nil
}

func (b *txFake) EstimateTransfer(_, _ string, amount uint64, _ []byte) (*zeclib.TransferEstimate, error) {
	b.c.mtx.Lock()
	defer b.c.mtx.Unlock()
	b.c.estimates = append(b.c.estimates, amount)
	if b.c.estimate == nil {
		return &zeclib.TransferEstimate{Fee: 10_000}, nil
	}
	return b.c.estimate(amount)
}

func (b *txFake) EstimateSendMax(string, string, []byte) (*zeclib.SendMaxEstimate, error) {
	b.c.mtx.Lock()
	defer b.c.mtx.Unlock()
	if b.c.sendMax == nil {
		return nil, b.c.sendMaxErr
	}
	est := *b.c.sendMax
	return &est, b.c.sendMaxErr
}

func (b *txFake) send(sendMax bool, seed []byte, amount, maxFee, expected uint64) (zeclib.SendResult, error) {
	b.c.mtx.Lock()
	hook := b.c.sendHook
	b.c.mtx.Unlock()
	if hook != nil {
		hook()
	}
	b.c.mtx.Lock()
	b.c.sent = append(b.c.sent, sentCall{sendMax: sendMax, amount: amount, maxFee: maxFee, expected: expected,
		seed: append([]byte(nil), seed...)})
	if b.c.sendErr != nil {
		err := b.c.sendErr
		b.c.mtx.Unlock()
		return zeclib.SendResult{}, err
	}
	txid := fmt.Sprintf("%064x", 0xabc000+len(b.c.sent))
	mk := b.c.sentRecord
	broadcastErr := b.c.broadcastErr
	b.c.mtx.Unlock()
	if mk != nil {
		if r := mk(txid, amount); r != nil {
			b.c.put(*r)
		}
	}
	return zeclib.SendResult{TxIDs: []string{txid}, Fee: maxFee, BroadcastErr: broadcastErr}, nil
}

func (b *txFake) Send(_ context.Context, _ string, seed []byte, _ string, amount uint64, _ []byte, maxFee uint64) (*zeclib.SendResult, error) {
	res, err := b.send(false, seed, amount, maxFee, 0)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (b *txFake) SendMax(_ context.Context, _ string, seed []byte, _ string, _ []byte, maxFee, expected uint64) (*zeclib.SendMaxResult, error) {
	res, err := b.send(true, seed, expected, maxFee, expected)
	if err != nil {
		return nil, err
	}
	return &zeclib.SendMaxResult{SendResult: res, Amount: expected}, nil
}

// faultyTxDB is a NetworkTxDB whose reads and writes of records can be made
// to fail a number of times.
type faultyTxDB struct {
	txdb.NetworkTxDB
	mtx                 sync.Mutex
	failGets, failStore int
}

func (f *faultyTxDB) GetTx(id string) (*txdb.WalletTransaction, error) {
	f.mtx.Lock()
	fail := f.failGets > 0
	if fail {
		f.failGets--
	}
	f.mtx.Unlock()
	if fail {
		return nil, errors.New("injected read failure")
	}
	return f.NetworkTxDB.GetTx(id)
}

func (f *faultyTxDB) StoreTxs(wts []*txdb.WalletTransaction) error {
	f.mtx.Lock()
	fail := f.failStore > 0
	if fail {
		f.failStore--
	}
	f.mtx.Unlock()
	if fail {
		return errors.New("injected write failure")
	}
	return f.NetworkTxDB.StoreTxs(wts)
}

func (f *faultyTxDB) fail(gets, stores int) {
	f.mtx.Lock()
	f.failGets, f.failStore = gets, stores
	f.mtx.Unlock()
}

// txEnv is an env whose wallet has a real NetworkTxDB and whose backends
// share a chain. The wallet's NetworkTxDB is faulty; ntx reads it directly.
type txEnv struct {
	*env
	c      *chain
	db     *txdb.TxDB
	ntx    txdb.NetworkTxDB
	faulty *faultyTxDB
}

func newTxEnv(t *testing.T) *txEnv {
	t.Helper()
	e := newEnv(t)
	c := newChain()
	prev := openBackend
	openBackend = func(cfg *zeclib.Config, seed []byte) (backend, error) {
		be, err := prev(cfg, seed)
		if err != nil {
			return nil, err
		}
		return &txFake{fakeBackend: be.(*fakeBackend), c: c}, nil
	}
	t.Cleanup(func() { openBackend = prev })
	db, err := txdb.New(filepath.Join(t.TempDir(), "txdb"), util.Disabled)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ntx := db.NetworkTxDB(BipID)
	te := &txEnv{env: e, c: c, db: db, ntx: ntx, faulty: &faultyTxDB{NetworkTxDB: ntx}}
	te.w = te.newTxInstance()
	return te
}

// newTxInstance builds an instance with the NetworkTxDB.
func (e *txEnv) newTxInstance() *zecWallet {
	e.t.Helper()
	w, err := newWallet(&asset.WalletConfig{
		Type:        walletTypeNative,
		DataDir:     e.dir,
		Emit:        asset.NewWalletEmitter(e.notes.ch, BipID, util.Disabled),
		PeersChange: func(uint32, error) {},
		NetworkTxDB: e.faulty,
	}, util.NewLogger("ZEC", util.LevelTrace, e.log), util.Simnet)
	if err != nil {
		e.t.Fatalf("newWallet: %v", err)
	}
	e.t.Cleanup(func() {
		if !e.abandoned.Load() {
			w.Close()
		}
	})
	return w
}

// stored returns the record of a transaction in the NetworkTxDB.
func (e *txEnv) stored(txid string) *txdb.WalletTransaction {
	e.t.Helper()
	wt, err := e.ntx.GetTx(txid)
	if err != nil {
		e.t.Fatalf("GetTx: %v", err)
	}
	return wt
}

// txNotes returns the transaction notes sent so far, other notes dropped.
func (e *txEnv) txNotes() (out []*asset.TransactionNote) {
	for _, n := range e.notes.drain() {
		if tn, ok := n.(*asset.TransactionNote); ok {
			out = append(out, tn)
		}
	}
	return out
}

func u32(v uint32) *uint32 { return &v }
func u64(v uint64) *uint64 { return &v }

// txid makes a display transaction ID.
func txid(n int) string {
	return hex.EncodeToString([]byte(fmt.Sprintf("%032d", n)))
}
