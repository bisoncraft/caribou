// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

const (
	// projectionTable is the wallet's table in the NetworkTxDB, and
	// watermarkKey the key of the watermark in it.
	projectionTable = "zec"
	watermarkKey    = "watermark"
	// catchUpBlocks is how far the chain has to have moved during one
	// stretch of syncing for its end to be followed by a full listing.
	catchUpBlocks = 100
	// listingRetryDelay is how long after a failed full listing no other
	// starts. Events in the meantime do their incremental work, so a new
	// transaction still shows.
	listingRetryDelay = time.Minute
)

// projection keeps the wallet's transactions in the NetworkTxDB, from which
// core reads the history. zeclib's history is the source; the NetworkTxDB
// holds a record of each transaction, written when it changes.
//
// What is read when:
//   - A full listing of zeclib's history: on the first connection after the
//     database is opened, after a catch-up of more than catchUpBlocks or a
//     recovery, after a rewind, and again after a listing that failed. It
//     ends with TransactionHistorySyncedNote.
//   - On every change zeclib reports: the transactions discovered after the
//     watermark, then the open rows.
//   - On every new chain tip, and when zeclib becomes synced or stops being
//     synced: the open rows.
//
// The open rows are those zeclib reports as open (OpenTransactions at
// finalConfirmations), those the full listing finds open or holds back, and
// those whose record could not be written. A row that was open and is no
// longer is read once more, so its final state is written.
//
// A full listing reads a page at a time, with the guard of the backend and
// the projection's lock taken for each page and released between them, so
// a send's record and a close are not kept waiting for a whole history.
type projection struct {
	// mtx orders the reads, comparisons and writes of the worker and of
	// Send: without it both could find a new transaction not stored, and
	// each report it as new. It guards the fields below it.
	mtx sync.Mutex
	// listed is the backend the last full listing read, so that the first
	// connection after an opening lists the history in full.
	listed backend
	// fullNeeded is set while a full listing is due: from a relist signal
	// until a full listing succeeds, through a failed or cancelled one and
	// a resume.
	fullNeeded bool
	// open holds the display IDs of the open rows.
	open map[string]struct{}
	// watermark is the greatest Seq projected, and loaded is set once it has
	// been read from the NetworkTxDB.
	watermark uint64
	loaded    bool
	// tip and synced are the chain tip and the synced state of the last open
	// pass.
	tip    uint32
	synced bool
	// listRetryAt is the time before which no full listing starts, after
	// one failed for a reason other than a close or a cancellation. The
	// retry is event-driven: no timer fires at the end of the wait, and the
	// listing runs on the first event after it. The wait belongs to the
	// instance, so it carries across a close and reopen of the same
	// instance.
	listRetryAt time.Time

	// relist is set by the zeclib callbacks when a catch-up or a recovery
	// ended or the wallet rewound, and moved into fullNeeded by the worker.
	// A callback only signals, and the worker reads the latest status, so a
	// short syncing to synced change, or a rewind and its rescan, could
	// otherwise pass unseen.
	relist atomic.Bool
}

// onSyncStatus returns the zeclib status callback: it latches the end of a
// catch-up and signals the worker. zeclib calls it from one goroutine, in
// order, so its state needs no lock.
func (w *zecWallet) onSyncStatus() func(zeclib.SyncStatus) {
	var syncing, recovered bool
	var from uint32
	return func(s zeclib.SyncStatus) {
		switch s.State {
		case zeclib.SyncSyncing:
			if !syncing {
				syncing, recovered, from = true, false, s.FullyScanned
			}
			recovered = recovered || s.Recovering
		case zeclib.SyncSynced:
			if syncing {
				syncing = false
				if recovered || s.ChainTip > from+catchUpBlocks {
					w.proj.relist.Store(true)
				}
			}
		}
		signal(w.statusSignal)
	}
}

// onRewind is the zeclib rewind callback. A transaction above the rewind
// height can be rescanned at another height before the worker runs, without
// ever being seen unmined, so the history is listed again in full.
func (w *zecWallet) onRewind(uint32) {
	w.proj.relist.Store(true)
	signal(w.statusSignal)
}

// projectEvent says what made the worker project.
type projectEvent uint8

const (
	projectStart projectEvent = iota
	projectStatus
	projectChange
)

// project brings the NetworkTxDB up to date with zeclib after an event.
func (w *zecWallet) project(ctx context.Context, ev projectEvent) {
	if w.txDB == nil {
		return
	}
	full, err := w.projectStep(ev)
	if err == nil && full {
		err = w.fullListing(ctx)
		if err != nil && !stopped(ctx, err) {
			w.proj.mtx.Lock()
			w.proj.listRetryAt = now().Add(listingRetryDelay)
			w.proj.mtx.Unlock()
		}
	}
	if err != nil && !stopped(ctx, err) {
		w.log.Errorf("Updating the transaction history: %v", err)
	}
}

// errReopened is the error of a full listing whose database was closed and
// opened again under it.
var errReopened = errors.New("the database was reopened during the listing")

// stopped reports whether err ends a projection because the instance closed
// or the connection ended, which is no failure of the projection.
func stopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, zeclib.ErrClosed) || errors.Is(err, errClosed) ||
		errors.Is(err, errReopened)
}

// projectStep does the work of an event that is not a full listing, under
// the guard of the backend and the projection's lock, and reports whether a
// full listing is due.
func (w *zecWallet) projectStep(ev projectEvent) (full bool, err error) {
	be, account, release, err := w.backend()
	if err != nil {
		return false, err
	}
	defer release()
	p := &w.proj
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if err := w.loadWatermark(); err != nil {
		return false, fmt.Errorf("reading the history watermark: %w", err)
	}
	if p.relist.Swap(false) || p.listed != be {
		p.fullNeeded = true
	}
	// After a failed listing, events do their incremental work until the
	// retry time, as if no listing were due.
	if p.fullNeeded && !now().Before(p.listRetryAt) {
		return true, nil
	}
	st := be.SyncStatus()
	switch {
	case ev == projectChange:
		if err := w.listAfterWatermark(be, account.ID); err != nil {
			return false, err
		}
		return false, w.openPass(be, account.ID)
	case st.ChainTip != p.tip || (st.State == zeclib.SyncSynced) != p.synced:
		return false, w.openPass(be, account.ID)
	}
	return false, nil
}

// loadWatermark reads the watermark once. The caller holds proj.mtx.
func (w *zecWallet) loadWatermark() error {
	p := &w.proj
	if p.loaded {
		return nil
	}
	table, err := w.txDB.Table(projectionTable)
	if err != nil {
		return err
	}
	v, err := table.Get([]byte(watermarkKey))
	switch {
	case errors.Is(err, txdb.ErrKeyNotFound):
	case err != nil:
		return err
	case len(v) != 8:
		return fmt.Errorf("the watermark is %d bytes", len(v))
	default:
		p.watermark = binary.BigEndian.Uint64(v)
	}
	p.loaded = true
	return nil
}

// advanceWatermark raises the watermark to seq and stores it. The caller
// holds proj.mtx.
func (w *zecWallet) advanceWatermark(seq uint64) error {
	p := &w.proj
	if seq <= p.watermark {
		return nil
	}
	table, err := w.txDB.Table(projectionTable)
	if err != nil {
		return err
	}
	if err := table.Set([]byte(watermarkKey), binary.BigEndian.AppendUint64(nil, seq), txdb.WithReplace()); err != nil {
		return err
	}
	p.watermark = seq
	return nil
}

// fullListing reconciles every row of zeclib's history, a page at a time. A
// row it holds back, finds open, or cannot write joins the open rows as it
// is seen, so the rule that reads a row once more when it leaves them
// covers it. The listing reads one backend: the first page's. It stops when
// ctx is done, the instance closes, or the database is opened again under
// it. It succeeds, clears fullNeeded and sends TransactionHistorySyncedNote
// only if every row was written; otherwise the listing stays due.
func (w *zecWallet) fullListing(ctx context.Context) error {
	var after uint64
	var pinned backend
	failed := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, top, pageFailed, be, err := w.listPage(pinned, after)
		if err != nil {
			return err
		}
		pinned = be
		failed = failed || pageFailed
		after = max(after, top)
		if done {
			break
		}
	}
	if failed {
		return errors.New("some records could not be written; the history is listed again")
	}
	return w.finishListing(ctx, pinned)
}

// finishListing marks a full listing of pinned done and sends the note, under
// the guard of the backend, only if pinned is still the open backend and
// ctx is not done: a close that came after the last page leaves the listing
// due and sends nothing.
func (w *zecWallet) finishListing(ctx context.Context, pinned backend) error {
	be, _, release, err := w.backend()
	if err != nil {
		return err
	}
	defer release()
	if be != pinned {
		return errReopened
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p := &w.proj
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if p.relist.Load() {
		// Another listing is due already.
		return nil
	}
	p.fullNeeded, p.listed = false, pinned
	if w.emit != nil {
		w.emit.TransactionHistorySyncedNote()
	}
	return nil
}

// listPage reconciles one page of the full listing, the rows after after,
// under the guard of the backend and the projection's lock. pinned, if set,
// is the backend of the listing's first page: a page from another backend
// is refused before anything is read. It reports whether the page was the
// last, the greatest Seq in it, whether a row failed, and the backend it
// read.
func (w *zecWallet) listPage(pinned backend, after uint64) (done bool, top uint64, failed bool, _ backend, err error) {
	be, account, release, err := w.backend()
	if err != nil {
		return false, 0, false, nil, err
	}
	defer release()
	if pinned != nil && be != pinned {
		return false, 0, false, nil, errReopened
	}
	p := &w.proj
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if p.open == nil {
		p.open = make(map[string]struct{})
	}
	listing, err := be.Transactions(account.ID, after, historyPage)
	if err != nil {
		return false, 0, false, be, err
	}
	synced := be.SyncStatus().State == zeclib.SyncSynced
	outcomes := w.reconcileAll(be, account.ID, listing.Records, listing.ChainTip, synced)
	for i := range listing.Records {
		r := &listing.Records[i]
		top = max(top, r.Seq)
		if outcomes[i] != reconciled || isOpen(r, listing.ChainTip) {
			p.open[r.TxID] = struct{}{}
		}
		failed = failed || outcomes[i] == reconcileFailed
	}
	if err := w.advanceWatermark(top); err != nil {
		return false, 0, false, be, err
	}
	return len(listing.Records) < historyPage, top, failed, be, nil
}

// listAfterWatermark reconciles the rows discovered after the watermark. A
// row held back, open, or not written joins the open rows. The caller holds
// proj.mtx.
func (w *zecWallet) listAfterWatermark(be backend, accountID string) error {
	p := &w.proj
	if p.open == nil {
		p.open = make(map[string]struct{})
	}
	synced := be.SyncStatus().State == zeclib.SyncSynced
	for {
		listing, err := be.Transactions(accountID, p.watermark, historyPage)
		if err != nil {
			return err
		}
		var top uint64
		outcomes := w.reconcileAll(be, accountID, listing.Records, listing.ChainTip, synced)
		for i := range listing.Records {
			r := &listing.Records[i]
			top = max(top, r.Seq)
			if outcomes[i] != reconciled || isOpen(r, listing.ChainTip) {
				p.open[r.TxID] = struct{}{}
			}
		}
		if err := w.advanceWatermark(top); err != nil {
			return err
		}
		if len(listing.Records) < historyPage {
			return nil
		}
	}
}

// openPass reconciles the open rows. A row that was open and is no longer is
// read once more and reconciled; it stays open if it is held back or its
// record could not be written. The caller holds proj.mtx.
func (w *zecWallet) openPass(be backend, accountID string) error {
	p := &w.proj
	listing, err := be.OpenTransactions(accountID, finalConfirmations)
	if err != nil {
		return err
	}
	synced := be.SyncStatus().State == zeclib.SyncSynced
	open := make(map[string]struct{}, len(listing.Records))
	w.reconcileAll(be, accountID, listing.Records, listing.ChainTip, synced)
	for i := range listing.Records {
		open[listing.Records[i].TxID] = struct{}{}
	}
	var left []zeclib.TxRecord
	var leftTip uint32
	for id := range p.open {
		if _, still := open[id]; still {
			continue
		}
		r, tip, err := be.Transaction(accountID, id)
		if err != nil {
			return err
		}
		if r != nil {
			left = append(left, *r)
			leftTip = tip
		}
	}
	for i, outcome := range w.reconcileAll(be, accountID, left, leftTip, synced) {
		if outcome != reconciled {
			open[left[i].TxID] = struct{}{}
		}
	}
	p.open = open
	p.tip, p.synced = listing.ChainTip, synced
	return nil
}

// recordSent reconciles a transaction a send created and adds it to the open
// rows. Errors are logged: the send has happened, and the worker repairs the
// record later.
func (w *zecWallet) recordSent(be backend, accountID, txid string) {
	if w.txDB == nil {
		return
	}
	p := &w.proj
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if p.open == nil {
		p.open = make(map[string]struct{})
	}
	p.open[txid] = struct{}{}
	r, tip, err := be.Transaction(accountID, txid)
	if err != nil || r == nil {
		w.log.Errorf("Reading sent transaction %s for the history: %v", txid, err)
		return
	}
	w.reconcileAll(be, accountID, []zeclib.TxRecord{*r}, tip, be.SyncStatus().State == zeclib.SyncSynced)
}

// isOpen reports whether a row can still change: zeclib's criteria for
// OpenTransactions at finalConfirmations, against the listing's tip.
func isOpen(r *zeclib.TxRecord, tip uint32) bool {
	return r.MinedHeight == nil || confirmations(r, tip) <= finalConfirmations || r.BlockTime == nil ||
		!r.RawPresent || (r.TotalSpent > 0 && r.Fee == nil)
}

// confirmations counts a mined row's confirmations at tip: zero when it is
// unmined or mined above the tip, which a status reply newer than the
// listing can give.
func confirmations(r *zeclib.TxRecord, tip uint32) uint32 {
	if r.MinedHeight == nil || *r.MinedHeight > tip {
		return 0
	}
	return tip - *r.MinedHeight + 1
}

// reconcileOutcome is what became of a row.
type reconcileOutcome uint8

const (
	// reconciled: its record is written, or was already up to date.
	reconciled reconcileOutcome = iota
	// reconcileHeld: it is held back and not written.
	reconcileHeld
	// reconcileFailed: its record could not be read or written.
	reconcileFailed
)

// reconcileAll writes the records of rows that changed, in one write, with a
// TransactionNote for each. It returns each row's outcome. Errors are
// logged; a failed row is the caller's to keep open. The caller holds the
// guard of be and proj.mtx.
func (w *zecWallet) reconcileAll(be backend, accountID string, records []zeclib.TxRecord, tip uint32, synced bool) []reconcileOutcome {
	// The ownership of each address is looked up once per call. A failed
	// lookup is logged once and fails every row that needed it: a row written
	// on a guess could report a payment to the wallet itself as a send.
	type ownership struct {
		owned bool
		err   error
	}
	looked := make(map[string]ownership)
	owns := func(address string) (bool, error) {
		if o, ok := looked[address]; ok {
			return o.owned, o.err
		}
		v, err := w.ownsAddress(be, accountID, address)
		if err != nil {
			w.log.Errorf("Checking whether %s is the wallet's: %v", shortAddress(address), err)
		}
		looked[address] = ownership{v, err}
		return v, err
	}
	outcomes := make([]reconcileOutcome, len(records))
	type change struct {
		i     int
		isNew bool
	}
	var changes []change
	var batch []*txdb.WalletTransaction
	for i := range records {
		wt, err := w.recordOf(&records[i], tip, synced, owns)
		if err != nil {
			outcomes[i] = reconcileFailed
			continue
		}
		if wt == nil {
			outcomes[i] = reconcileHeld
			continue
		}
		stored, err := w.txDB.GetTx(wt.ID)
		if err != nil {
			w.log.Errorf("Reading the record of transaction %s: %v", wt.ID, err)
			outcomes[i] = reconcileFailed
			continue
		}
		if stored != nil && sameRecord(stored, wt) {
			continue
		}
		if stored != nil && stored.Confirmed && !wt.Confirmed {
			w.log.Warnf("Transaction %s, recorded as final, changed after a chain reorganization: block %d to %d",
				wt.ID, stored.BlockNumber, wt.BlockNumber)
		}
		changes = append(changes, change{i: i, isNew: stored == nil})
		batch = append(batch, wt)
	}
	if len(batch) == 0 {
		return outcomes
	}
	if err := w.txDB.StoreTxs(batch); err != nil {
		w.log.Errorf("Storing %d transaction records: %v", len(batch), err)
		for _, c := range changes {
			outcomes[c.i] = reconcileFailed
		}
		return outcomes
	}
	if w.emit != nil {
		for j, c := range changes {
			w.emit.TransactionNote(batch[j], c.isNew)
		}
	}
	return outcomes
}

// sameRecord reports whether two records say the same.
func sameRecord(a, b *txdb.WalletTransaction) bool {
	x, y := *a, *b
	x.RawTx, y.RawTx = nil, nil
	if len(x.AdditionalData) == 0 {
		x.AdditionalData = nil
	}
	if len(y.AdditionalData) == 0 {
		y.AdditionalData = nil
	}
	return reflect.DeepEqual(x, y)
}

// recordOf builds the record of a row, or returns nil for a row held back:
// one the account funded whose full transaction the wallet lacks. Until it
// has it, the wallet cannot tell a send from a payment to itself.
//
// The classification looks at ToWallet and Address, never IsChange, which
// does not tell change from a payment to one of the account's addresses:
//   - Not funded by the account: a Receive of TotalReceived.
//   - Funded, with an output that is not the wallet's: a Send of those
//     outputs to their address.
//   - Funded, every output the wallet's: a SelfSend of the outputs that carry
//     an address, or of TotalReceived when none does, as for a shielding.
//
// Change is an output to the wallet without an address. owns, if set, says
// whether the wallet owns an address.
//
// The Recipient is zeclib's TxOutput.Address of the paid output. In a
// transaction this wallet created it is the address the payment named, which
// for a payment to itself can have a Sapling receiver if the user paid that
// form. In one it did not (a receive, or any transaction as a restored wallet
// or a second device on the same seed sees it) it is the receiver that was
// paid alone: for a shielded output the Orchard-only form, never with a
// Sapling receiver, for a transparent one the t-address. So a shielded
// payment to itself shows the address named on the wallet that made it and
// the Orchard-only receiver on a restored one, and a payment to someone else
// the address named and the receiver alone: the same receiver, different
// strings. A transparent payment to itself is the t-address on both.
func (w *zecWallet) recordOf(r *zeclib.TxRecord, tip uint32, synced bool,
	owns func(string) (bool, error)) (*txdb.WalletTransaction, error) {
	funded := r.TotalSpent > 0
	if funded && !r.RawPresent {
		return nil, nil
	}
	if funded && owns != nil {
		// zeclib marks an output of the account's own transaction to one of
		// its addresses as the wallet's only once it has scanned the block.
		// An output to an address the wallet owns, by the rule of
		// OwnsDepositAddress, is the wallet's from the start, so a payment
		// to itself is a SelfSend from the moment it is sent.
		outputs := slices.Clone(r.Outputs)
		for i := range outputs {
			if outputs[i].ToWallet || outputs[i].Address == "" {
				continue
			}
			owned, err := owns(outputs[i].Address)
			if err != nil {
				return nil, err
			}
			outputs[i].ToWallet = owned
		}
		rec := *r
		rec.Outputs = outputs
		r = &rec
	}
	wt := &txdb.WalletTransaction{ID: r.TxID, NetworkID: BipID, AssetID: BipID}
	data := make(map[string]string)
	var paid []zeclib.TxOutput
	switch {
	case !funded:
		wt.Type = txdb.Receive
		wt.Amount = r.TotalReceived
		for _, o := range r.Outputs {
			if o.ToWallet {
				paid = append(paid, o)
			}
		}
	case slices.ContainsFunc(r.Outputs, func(o zeclib.TxOutput) bool { return !o.ToWallet }):
		wt.Type = txdb.Send
		for _, o := range r.Outputs {
			if !o.ToWallet {
				wt.Amount += o.Value
				paid = append(paid, o)
			}
		}
	default:
		wt.Type = txdb.SelfSend
		for _, o := range r.Outputs {
			if o.ToWallet && o.Address != "" {
				wt.Amount += o.Value
				paid = append(paid, o)
			}
		}
		if len(paid) == 0 {
			wt.Amount = r.TotalReceived
		}
	}
	for _, o := range paid {
		if o.Address != "" {
			a := o.Address
			wt.Recipient = &a
			break
		}
	}
	if funded {
		if r.Fee != nil {
			wt.Fees = *r.Fee
			if wt.Type == txdb.Send && r.BalanceDelta != -int64(wt.Amount+wt.Fees) {
				w.log.Warnf("Transaction %s: the balance changed by %d, not by the amount %d and the fee %d",
					r.TxID, r.BalanceDelta, wt.Amount, wt.Fees)
			}
		} else {
			data["feeUnknown"] = "true"
		}
	}
	for _, o := range paid {
		if memo, ok := memoText(o.Memo); ok {
			data["memo"] = memo
			break
		}
	}
	if r.MinedHeight != nil {
		wt.BlockNumber = uint64(*r.MinedHeight)
	}
	if r.BlockTime != nil {
		wt.Timestamp = *r.BlockTime
	}
	confs := min(confirmations(r, tip), finalConfirmations)
	wt.Confirms = &txdb.Confirms{Current: confs, Target: finalConfirmations}
	wt.Confirmed = confs >= finalConfirmations
	// zeclib can only tell that an unmined transaction expired once it has
	// scanned to the tip.
	if r.ExpiredUnmined && r.MinedHeight == nil && synced {
		wt.Confirmed, wt.Rejected, wt.Fees = true, true, 0
		delete(data, "feeUnknown")
	}
	if len(data) > 0 {
		wt.AdditionalData = data
	}
	return wt, nil
}

// memoText returns the memo of an output for display: text when it is text
// (ZIP 302: a first byte up to 0xF4 and valid UTF-8), hexadecimal otherwise,
// with its trailing zeros trimmed. An empty memo (0xF6 then zeros) and no
// memo give false.
func memoText(memo []byte) (string, bool) {
	m := bytes.TrimRight(memo, "\x00")
	if len(m) == 0 || (len(m) == 1 && m[0] == 0xF6) {
		return "", false
	}
	if m[0] <= 0xF4 && utf8.Valid(m) {
		return string(m), true
	}
	return hex.EncodeToString(m), true
}

// TxHistory returns the history core asks for from the NetworkTxDB. A nil
// request is an empty result.
func (w *zecWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if req == nil {
		return &txdb.TxHistoryResponse{}, nil
	}
	if w.txDB == nil {
		return nil, errors.New("no transaction database")
	}
	return w.txDB.GetAssetTxs(nil, req)
}

// WalletTransaction returns the record of a transaction by its ID in display
// order, or in wire order, which core can pass as a coin ID.
func (w *zecWallet) WalletTransaction(_ context.Context, txID string) (*txdb.WalletTransaction, error) {
	if w.txDB == nil {
		return nil, errors.New("no transaction database")
	}
	id := strings.ToLower(txID)
	wt, err := w.txDB.GetTx(id)
	if err != nil || wt != nil {
		return wt, err
	}
	if b, err := hex.DecodeString(id); err == nil && len(b) == 32 {
		if wt, err := w.txDB.GetTx(displayTxID(b)); err != nil || wt != nil {
			return wt, err
		}
	}
	return nil, asset.CoinNotFoundError
}

// PendingTransactions returns the records that are not final.
func (w *zecWallet) PendingTransactions(context.Context) []*txdb.WalletTransaction {
	if w.txDB == nil {
		return nil
	}
	txs, err := w.txDB.GetAssetPendingTxs(nil)
	if err != nil {
		w.log.Errorf("Reading the pending transactions: %v", err)
		return nil
	}
	return txs
}
