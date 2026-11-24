// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build zec && cgo

package zeclib

import (
	"errors"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/internal/ffi"
)

// ffiBackend is the Rust wallet database. It adapts the generated bindings
// to the backend interface: optional values become plain Go values, errors
// become the errors of this package, and a call after close returns
// ErrClosed where the bindings would panic.
type ffiBackend struct {
	// mtx guards wallet. A call holds it for reading for its whole
	// duration, so close waits for calls in flight before it releases the
	// Rust object.
	mtx    sync.RWMutex
	wallet *ffi.Wallet
}

var _ backend = (*ffiBackend)(nil)

// openBackend opens the wallet database at path, creating it if it does not
// exist, and migrates it to the current schema. seed may be nil.
func openBackend(path string, network Network, seed []byte) (backend, error) {
	var seedArg *[]byte
	if seed != nil {
		seedArg = &seed
	}
	wallet, err := ffi.WalletOpen(path, toFFINetwork(network), seedArg)
	if err != nil {
		return nil, convertError(err)
	}
	return &ffiBackend{wallet: wallet}, nil
}

// with runs f with the Rust wallet, unless the backend is closed.
func (b *ffiBackend) with(f func(w *ffi.Wallet) error) error {
	b.mtx.RLock()
	defer b.mtx.RUnlock()
	if b.wallet == nil {
		return ErrClosed
	}
	return convertError(f(b.wallet))
}

func (b *ffiBackend) close() error {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if b.wallet == nil {
		return nil
	}
	b.wallet.Destroy()
	b.wallet = nil
	return nil
}

func (b *ffiBackend) createAccount(name string, seed, treeState []byte, recoverUntil *uint32, keySource string) (*Account, error) {
	var keySourceArg *string
	if keySource != "" {
		keySourceArg = &keySource
	}
	var account *Account
	err := b.with(func(w *ffi.Wallet) error {
		a, err := w.CreateAccount(name, seed, treeState, recoverUntil, keySourceArg)
		if err != nil {
			return err
		}
		converted := fromFFIAccount(&a)
		account = &converted
		return nil
	})
	return account, err
}

func (b *ffiBackend) accounts() ([]Account, error) {
	var accounts []Account
	err := b.with(func(w *ffi.Wallet) error {
		list, err := w.Accounts()
		if err != nil {
			return err
		}
		accounts = make([]Account, len(list))
		for i := range list {
			accounts[i] = fromFFIAccount(&list[i])
		}
		return nil
	})
	return accounts, err
}

func (b *ffiBackend) isSeedRelevant(seed []byte) (relevant bool, err error) {
	err = b.with(func(w *ffi.Wallet) (err error) {
		relevant, err = w.IsSeedRelevant(seed)
		return err
	})
	return relevant, err
}

func (b *ffiBackend) currentAddress(accountID string) (address string, err error) {
	err = b.with(func(w *ffi.Wallet) (err error) {
		address, err = w.CurrentAddress(accountID)
		return err
	})
	return address, err
}

func (b *ffiBackend) nextAvailableAddress(accountID string, receivers ReceiverSet) (address string, err error) {
	set := ffi.ReceiverSetAll
	if receivers == ReceiversShielded {
		set = ffi.ReceiverSetShielded
	}
	err = b.with(func(w *ffi.Wallet) (err error) {
		address, err = w.NextAvailableAddress(accountID, set)
		return err
	})
	return address, err
}

func (b *ffiBackend) transparentReceivers(accountID string) (watched []watchedAddress, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		addresses, err := w.TransparentReceivers(accountID)
		if err != nil {
			return err
		}
		watched = make([]watchedAddress, len(addresses))
		for i, a := range addresses {
			watched[i] = watchedAddress{address: a.Address, change: a.Change, index: a.Index, unused: a.Unused}
		}
		return nil
	})
	return watched, err
}

func (b *ffiBackend) summary(minConfirmations uint32) (*Summary, error) {
	var summary *Summary
	err := b.with(func(w *ffi.Wallet) error {
		s, err := w.WalletSummary(optionalMinConf(minConfirmations))
		if err != nil || s == nil {
			return err
		}
		summary = fromFFISummary(s)
		return nil
	})
	return summary, err
}

func (b *ffiBackend) birthday() (height uint32, ok bool, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		h, err := w.BirthdayHeight()
		if err != nil || h == nil {
			return err
		}
		height, ok = *h, true
		return nil
	})
	return height, ok, err
}

func (b *ffiBackend) maxScannedHeight() (height uint32, ok bool, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		h, err := w.MaxScannedHeight()
		if err != nil || h == nil {
			return err
		}
		height, ok = *h, true
		return nil
	})
	return height, ok, err
}

func (b *ffiBackend) blockHash(height uint32) (hash []byte, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		h, err := w.BlockHash(height)
		if err != nil || h == nil {
			return err
		}
		hash = *h
		return nil
	})
	return hash, err
}

func (b *ffiBackend) putSubtreeRoots(roots *subtreeRoots) error {
	return b.with(func(w *ffi.Wallet) error {
		return w.PutSubtreeRoots(roots.start, toFFISubtreeRoots(roots.roots))
	})
}

func (b *ffiBackend) updateChainTip(height uint32) error {
	return b.with(func(w *ffi.Wallet) error {
		return w.UpdateChainTip(height)
	})
}

func (b *ffiBackend) suggestScanRanges() ([]scanRange, error) {
	var ranges []scanRange
	err := b.with(func(w *ffi.Wallet) error {
		list, err := w.SuggestScanRanges()
		if err != nil {
			return err
		}
		ranges = make([]scanRange, len(list))
		for i, r := range list {
			ranges[i] = scanRange{
				start:    r.Start,
				end:      r.End,
				priority: fromFFIPriority(r.Priority),
			}
		}
		return nil
	})
	return ranges, err
}

func (b *ffiBackend) scanBlocks(fromState []byte, blocks [][]byte) (*scanSummary, error) {
	var summary *scanSummary
	err := b.with(func(w *ffi.Wallet) error {
		s, err := w.ScanBlocks(fromState, blocks)
		if err != nil {
			return err
		}
		summary = &scanSummary{
			start: s.StartHeight,
			end:   s.EndHeight,
		}
		return nil
	})
	return summary, err
}

func (b *ffiBackend) rewindToHeight(height uint32) (rewound uint32, err error) {
	err = b.with(func(w *ffi.Wallet) (err error) {
		rewound, err = w.RewindToHeight(height)
		return err
	})
	return rewound, err
}

func (b *ffiBackend) consensusBranchID(height uint32) (branch uint32, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		branch = w.ConsensusBranchId(height)
		return nil
	})
	return branch, err
}

func (b *ffiBackend) utxoQueryHeight(accountID string) (height uint32, err error) {
	err = b.with(func(w *ffi.Wallet) (err error) {
		height, err = w.UtxoQueryHeight(accountID)
		return err
	})
	return height, err
}

func (b *ffiBackend) putUTXO(u *utxo) error {
	return b.with(func(w *ffi.Wallet) error {
		return w.PutUtxo(ffi.Utxo{
			Txid:     u.txid,
			Index:    u.index,
			Script:   u.script,
			ValueZat: u.value,
			Height:   u.height,
		})
	})
}

func (b *ffiBackend) transactionDataRequests() ([]txRequest, error) {
	var requests []txRequest
	err := b.with(func(w *ffi.Wallet) error {
		list, err := w.TransactionDataRequests()
		if err != nil {
			return err
		}
		requests = make([]txRequest, 0, len(list))
		for _, r := range list {
			req, err := fromFFITxRequest(r)
			if err != nil {
				return err
			}
			requests = append(requests, req)
		}
		return nil
	})
	return requests, err
}

func (b *ffiBackend) setTransactionStatus(txid []byte, status txStatus) error {
	var s ffi.TransactionStatus
	switch status.kind {
	case txNotRecognized:
		s = ffi.TransactionStatusTxidNotRecognized{}
	case txNotInMainChain:
		s = ffi.TransactionStatusNotInMainChain{}
	case txMined:
		s = ffi.TransactionStatusMined{Height: status.height}
	default:
		return wrapf(ErrInvalidArgument, "unknown transaction status %d", status.kind)
	}
	return b.with(func(w *ffi.Wallet) error {
		return w.SetTransactionStatus(txid, s)
	})
}

func (b *ffiBackend) decryptAndStoreTransaction(tx, txid []byte, minedHeight *uint32) (id []byte, err error) {
	var want *[]byte
	if txid != nil {
		want = &txid
	}
	err = b.with(func(w *ffi.Wallet) (err error) {
		id, err = w.DecryptAndStoreTransaction(tx, want, minedHeight)
		return err
	})
	return id, err
}

func (b *ffiBackend) notifyAddressChecked(req *txRequest, asOfHeight uint32) error {
	if req.kind != txRequestAddress {
		return wrapf(ErrInvalidArgument, "not a request for the transactions of an address")
	}
	r := ffi.TransactionDataRequestTransactionsInvolvingAddress{
		Address:            req.address,
		BlockRangeStart:    req.start,
		TxStatusFilter:     toFFITxStatusFilter(req.txStatus),
		OutputStatusFilter: toFFIOutputStatusFilter(req.outputStatus),
	}
	if req.bounded {
		end := req.end
		r.BlockRangeEnd = &end
	}
	if !req.requestAt.IsZero() {
		at := uint64(req.requestAt.Unix())
		r.RequestAt = &at
	}
	return b.with(func(w *ffi.Wallet) error {
		return w.NotifyAddressChecked(r, asOfHeight)
	})
}

func (b *ffiBackend) scheduleNextCheck(address string, offset time.Duration) (at time.Time, ok bool, err error) {
	seconds := uint32(max(offset, 0) / time.Second)
	err = b.with(func(w *ffi.Wallet) error {
		t, err := w.ScheduleNextCheck(address, seconds)
		if err != nil || t == nil {
			return err
		}
		at, ok = time.Unix(int64(*t), 0), true
		return nil
	})
	return at, ok, err
}

// optionalMinConf converts a minimum number of confirmations, where zero
// selects the default policy, for the bindings.
func optionalMinConf(n uint32) *uint32 {
	if n == 0 {
		return nil
	}
	return &n
}

// optionalBytes converts a byte slice, where nil means none, for the
// bindings.
func optionalBytes(b []byte) *[]byte {
	if b == nil {
		return nil
	}
	return &b
}

// fromFFIPaidPool converts a pool of the bindings. A pool this package does
// not know is an error, so that a new one cannot pass for another.
func fromFFIPaidPool(p ffi.PaidPool) (PaidPool, error) {
	switch p {
	case ffi.PaidPoolIronwood:
		return PaidIronwood, nil
	case ffi.PaidPoolTransparent:
		return PaidTransparent, nil
	default:
		return 0, wrapf(ErrInternal, "unknown paid pool %d", p)
	}
}

func (b *ffiBackend) missingTransactions(txids [][]byte) (missing [][]byte, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		missing, err = w.MissingTransactions(txids)
		return err
	})
	return missing, err
}

func (b *ffiBackend) estimateTransfer(accountID, address string, amount uint64, memo []byte, minConfirmations uint32) (*TransferEstimate, error) {
	var est *TransferEstimate
	err := b.with(func(w *ffi.Wallet) error {
		e, err := w.EstimateTransfer(accountID, address, amount, optionalBytes(memo), optionalMinConf(minConfirmations))
		if err != nil {
			return err
		}
		pool, err := fromFFIPaidPool(e.PaidPool)
		if err != nil {
			return err
		}
		est = &TransferEstimate{Fee: e.Fee, PaidAddress: e.PaidAddress, PaidPool: pool}
		return nil
	})
	return est, err
}

func (b *ffiBackend) send(accountID string, seed []byte, address string, amount uint64, memo []byte, maxFee uint64, minConfirmations uint32) (*sendResult, error) {
	var res *sendResult
	err := b.with(func(w *ffi.Wallet) error {
		r, err := w.Send(accountID, seed, address, amount, optionalBytes(memo), maxFee, optionalMinConf(minConfirmations))
		if err != nil {
			return err
		}
		res = fromFFISendResult(&r)
		return nil
	})
	return res, err
}

func fromFFILeftBehind(l ffi.LeftBehind) LeftBehind {
	return LeftBehind{
		TransparentCount: l.TransparentCount,
		TransparentValue: l.TransparentValue,
		NoteCount:        l.NoteCount,
		NoteValue:        l.NoteValue,
	}
}

// fromFFISendResult converts the result of a send. The transactions exist
// once it is called, so a pool this package does not know cannot fail the
// conversion and lose their IDs. It is left as the zero PaidPool, which reads
// as unknown.
func fromFFISendResult(r *ffi.SendResult) *sendResult {
	pool, _ := fromFFIPaidPool(r.PaidPool)
	res := &sendResult{fee: r.Fee, paidAddress: r.PaidAddress, paidPool: pool}
	if r.ReadError != nil {
		res.readErr = *r.ReadError
	}
	for _, tx := range r.Transactions {
		created := createdTx{txid: tx.Txid}
		if tx.Raw != nil {
			created.raw = *tx.Raw
		}
		res.transactions = append(res.transactions, created)
	}
	return res
}

func (b *ffiBackend) estimateSendMax(accountID, address string, memo []byte, minConfirmations uint32) (*SendMaxEstimate, error) {
	var est *SendMaxEstimate
	err := b.with(func(w *ffi.Wallet) error {
		e, err := w.EstimateSendMax(accountID, address, optionalBytes(memo), optionalMinConf(minConfirmations))
		if err != nil {
			return err
		}
		pool, err := fromFFIPaidPool(e.PaidPool)
		if err != nil {
			return err
		}
		est = &SendMaxEstimate{
			Amount:      e.Amount,
			Fee:         e.Fee,
			PaidAddress: e.PaidAddress,
			PaidPool:    pool,
			LeftBehind:  fromFFILeftBehind(e.LeftBehind),
		}
		return nil
	})
	return est, err
}

func (b *ffiBackend) sendMax(accountID string, seed []byte, address string, memo []byte, maxFee, expectedAmount uint64, minConfirmations uint32) (*sendMaxResult, error) {
	var expected *uint64
	if expectedAmount != 0 {
		expected = &expectedAmount
	}
	var res *sendMaxResult
	err := b.with(func(w *ffi.Wallet) error {
		r, err := w.SendMax(accountID, seed, address, optionalBytes(memo), maxFee, expected, optionalMinConf(minConfirmations))
		if err != nil {
			return err
		}
		res = &sendMaxResult{
			sendResult: *fromFFISendResult(&r.Result),
			amount:     r.Amount,
			leftBehind: fromFFILeftBehind(r.LeftBehind),
		}
		return nil
	})
	return res, err
}

func (b *ffiBackend) spendable(accountID string, minConfirmations uint32) (v SpendableValue, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		s, err := w.Spendable(accountID, optionalMinConf(minConfirmations))
		if err != nil {
			return err
		}
		v = SpendableValue{Ironwood: s.Ironwood, Transparent: s.Transparent}
		return nil
	})
	return v, err
}

func (b *ffiBackend) rebroadcastInventory() (txs []unminedTx, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		list, err := w.RebroadcastInventory()
		if err != nil {
			return err
		}
		for _, tx := range list {
			txs = append(txs, unminedTx{txid: tx.Txid, raw: tx.Raw, expiryHeight: tx.ExpiryHeight})
		}
		return nil
	})
	return txs, err
}

func (b *ffiBackend) ownsAddress(address string) (accountID string, ok bool, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		id, err := w.OwnsAddress(address)
		if err != nil || id == nil {
			return err
		}
		accountID, ok = *id, true
		return nil
	})
	return accountID, ok, err
}

func fromFFIRecord(r *ffi.TxRecord) (TxRecord, error) {
	rec := TxRecord{
		Seq:             r.Seq,
		TxID:            displayTxID(r.Txid),
		MinedHeight:     r.MinedHeight,
		BlockTime:       r.BlockTime,
		ExpiryHeight:    r.ExpiryHeight,
		ExpiredUnmined:  r.ExpiredUnmined,
		CreatedByWallet: r.CreatedByWallet,
		RawPresent:      r.RawPresent,
		BalanceDelta:    r.BalanceDelta,
		TotalSpent:      r.TotalSpent,
		TotalReceived:   r.TotalReceived,
		Fee:             r.Fee,
		IsShielding:     r.IsShielding,
	}
	for _, o := range r.Outputs {
		var pool OutputPool
		switch o.Pool {
		case ffi.OutputPoolTransparent:
			pool = OutputTransparent
		case ffi.OutputPoolSapling:
			pool = OutputSapling
		case ffi.OutputPoolOrchard:
			pool = OutputOrchard
		case ffi.OutputPoolIronwood:
			pool = OutputIronwood
		default:
			return TxRecord{}, wrapf(ErrInternal, "unknown output pool %d", o.Pool)
		}
		out := TxOutput{
			Pool:        pool,
			OutputIndex: o.OutputIndex,
			Value:       o.Value,
			ToWallet:    o.ToWallet,
			IsChange:    o.IsChange,
		}
		if o.Address != nil {
			out.Address = *o.Address
		}
		if o.Memo != nil {
			out.Memo = *o.Memo
		}
		rec.Outputs = append(rec.Outputs, out)
	}
	return rec, nil
}

func fromFFIListing(l *ffi.TxListing) (*TxListing, error) {
	out := &TxListing{}
	if l.ChainTip != nil {
		out.ChainTip = *l.ChainTip
	}
	for i := range l.Records {
		rec, err := fromFFIRecord(&l.Records[i])
		if err != nil {
			return nil, err
		}
		out.Records = append(out.Records, rec)
	}
	return out, nil
}

func (b *ffiBackend) transactions(accountID string, afterSeq uint64, limit uint32) (listing *TxListing, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		l, err := w.Transactions(accountID, afterSeq, limit)
		if err != nil {
			return err
		}
		listing, err = fromFFIListing(&l)
		return err
	})
	return listing, err
}

func (b *ffiBackend) transaction(accountID string, txid []byte) (record *TxRecord, tip uint32, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		l, err := w.Transaction(accountID, txid)
		if err != nil {
			return err
		}
		if l.ChainTip != nil {
			tip = *l.ChainTip
		}
		if l.Record != nil {
			rec, err := fromFFIRecord(l.Record)
			if err != nil {
				return err
			}
			record = &rec
		}
		return nil
	})
	return record, tip, err
}

func (b *ffiBackend) openTransactions(accountID string, depth uint32) (listing *TxListing, err error) {
	err = b.with(func(w *ffi.Wallet) error {
		l, err := w.OpenTransactions(accountID, depth)
		if err != nil {
			return err
		}
		listing, err = fromFFIListing(&l)
		return err
	})
	return listing, err
}

// parseAddress parses an address with the Rust library, which takes no
// database.
func parseAddress(network Network, address string) (*ParsedAddress, error) {
	p, err := ffi.ParseAddress(toFFINetwork(network), address)
	if err != nil {
		return nil, convertError(err)
	}
	out := &ParsedAddress{Payable: p.Payable}
	switch p.Kind {
	case ffi.AddressKindTransparent:
		out.Kind = AddressTransparent
	case ffi.AddressKindUnified:
		out.Kind = AddressUnified
	case ffi.AddressKindSapling:
		out.Kind = AddressSapling
	case ffi.AddressKindTex:
		out.Kind = AddressTex
	default:
		return nil, wrapf(ErrInternal, "unknown address kind %d", p.Kind)
	}
	for _, r := range p.Receivers {
		rec := Receiver{}
		switch r.Kind {
		case ffi.ReceiverKindOrchard:
			rec.Kind = ReceiverOrchard
		case ffi.ReceiverKindSapling:
			rec.Kind = ReceiverSapling
		case ffi.ReceiverKindTransparent:
			rec.Kind = ReceiverTransparent
		default:
			return nil, wrapf(ErrInternal, "unknown receiver kind %d", r.Kind)
		}
		if r.Address != nil {
			rec.Address = *r.Address
		}
		out.Receivers = append(out.Receivers, rec)
	}
	out.ExpiryHeight = p.ExpiryHeight
	out.ExpiryTime = p.ExpiryTime
	if p.PaidPool != nil {
		pool, err := fromFFIPaidPool(*p.PaidPool)
		if err != nil {
			return nil, err
		}
		out.PaidPool = pool
	}
	if p.PaidAddress != nil {
		out.PaidAddress = *p.PaidAddress
	}
	if p.Unpayable != nil {
		out.Unpayable = *p.Unpayable
	}
	return out, nil
}

// transparentReceiver returns the transparent receiver of a unified address
// as a transparent address, or the empty string if the address has none.
func transparentReceiver(network Network, unifiedAddress string) (string, error) {
	addr, err := ffi.TransparentReceiver(toFFINetwork(network), unifiedAddress)
	if err != nil || addr == nil {
		return "", convertError(err)
	}
	return *addr, nil
}

// convertError translates an error of the bindings into an error of this
// package. The bindings return a typed nil for success, which must not leak
// into an error interface.
func convertError(err error) error {
	if err == nil {
		return nil
	}
	var walletErr *ffi.WalletError
	if !errors.As(err, &walletErr) {
		return err
	}
	if walletErr == nil {
		return nil
	}
	switch e := walletErr.Unwrap().(type) {
	case *ffi.WalletErrorInvalidArgument:
		return wrapf(ErrInvalidArgument, "%s", e.Message)
	case *ffi.WalletErrorDatabase:
		return wrapf(ErrDatabase, "%s", e.Message)
	case *ffi.WalletErrorSeedRequired:
		return ErrSeedRequired
	case *ffi.WalletErrorSeedNotRelevant:
		return ErrSeedNotRelevant
	case *ffi.WalletErrorAccountNotFound:
		return ErrAccountNotFound
	case *ffi.WalletErrorAddressGapLimit:
		return wrapf(ErrAddressGapLimit, "%s", e.Message)
	case *ffi.WalletErrorChainContinuity:
		return &chainContinuityError{height: e.AtHeight, reason: e.Message}
	case *ffi.WalletErrorRewindInvalid:
		re := &rewindError{requested: e.RequestedHeight}
		if e.SafeHeight != nil {
			re.safe, re.hasSafe = *e.SafeHeight, true
		}
		return re
	case *ffi.WalletErrorScan:
		return &scanError{height: e.AtHeight, reason: e.Message}
	case *ffi.WalletErrorInsufficientFunds:
		return &InsufficientFundsError{Available: e.Available, Required: e.Required}
	case *ffi.WalletErrorNotSynced:
		return wrapf(ErrNotSynced, "%s", e.Message)
	case *ffi.WalletErrorFeeTooHigh:
		return &FeeTooHighError{Fee: e.Fee, MaxFee: e.MaxFee}
	case *ffi.WalletErrorAmountChanged:
		return &AmountChangedError{Expected: e.Expected, Actual: e.Actual}
	case *ffi.WalletErrorTooLarge:
		return wrapf(ErrTooLarge, "%s", e.Message)
	case *ffi.WalletErrorUnsupportedRecipient:
		return wrapf(ErrUnsupportedRecipient, "%s", e.Message)
	case *ffi.WalletErrorAddressExpired:
		return wrapf(ErrAddressExpired, "%s", e.Message)
	case *ffi.WalletErrorAmountBelowMinimum:
		return &AmountBelowMinimumError{Minimum: e.Minimum}
	case *ffi.WalletErrorMemoNotAllowed:
		return ErrMemoNotAllowed
	case *ffi.WalletErrorSeedMismatch:
		return ErrSeedMismatch
	case *ffi.WalletErrorSend:
		return wrapf(ErrSend, "%s", e.Message)
	case *ffi.WalletErrorInternal:
		return wrapf(ErrInternal, "%s", e.Message)
	default:
		return wrapf(ErrInternal, "unrecognized backend error: %v", err)
	}
}

func toFFINetwork(n Network) ffi.Network {
	switch n.Type {
	case NetworkMainnet:
		return ffi.NetworkMainnet{}
	case NetworkTestnet:
		return ffi.NetworkTestnet{}
	default:
		h := n.Regtest
		return ffi.NetworkRegtest{Heights: ffi.ActivationHeights{
			Overwinter: optionalHeight(h.Overwinter),
			Sapling:    optionalHeight(h.Sapling),
			Blossom:    optionalHeight(h.Blossom),
			Heartwood:  optionalHeight(h.Heartwood),
			Canopy:     optionalHeight(h.Canopy),
			Nu5:        optionalHeight(h.NU5),
			Nu6:        optionalHeight(h.NU6),
			Nu61:       optionalHeight(h.NU6_1),
			Nu62:       optionalHeight(h.NU6_2),
			Nu63:       optionalHeight(h.NU6_3),
			Nu7:        optionalHeight(h.NU7),
		}}
	}
}

// optionalHeight maps the zero height, which means "never", to nil.
func optionalHeight(h uint32) *uint32 {
	if h == 0 {
		return nil
	}
	return &h
}

func fromFFIAccount(a *ffi.Account) Account {
	account := Account{
		ID:       a.Uuid,
		Birthday: a.BirthdayHeight,
	}
	if a.Name != nil {
		account.Name = *a.Name
	}
	if a.Ufvk != nil {
		account.UFVK = *a.Ufvk
	}
	if a.HdAccountIndex != nil {
		account.Derived = true
		account.HDIndex = *a.HdAccountIndex
	}
	if a.SeedFingerprint != nil {
		account.SeedFingerprint = *a.SeedFingerprint
	}
	if a.KeySource != nil {
		account.KeySource = *a.KeySource
	}
	return account
}

func fromFFIPoolBalance(b ffi.PoolBalance) PoolBalance {
	return PoolBalance{
		Spendable:     b.Spendable,
		ChangePending: b.ChangePendingConfirmation,
		ValuePending:  b.ValuePendingSpendability,
		Uneconomic:    b.Uneconomic,
		Locked:        b.Locked,
		Total:         b.Total,
	}
}

func fromFFISummary(s *ffi.WalletSummary) *Summary {
	summary := &Summary{
		Balances:     make([]AccountBalance, len(s.AccountBalances)),
		ChainTip:     s.ChainTipHeight,
		FullyScanned: s.FullyScannedHeight,
		ScanProgress: Ratio(s.ScanProgress),
		nextSubtree:  s.NextSubtreeIndex,
	}
	if s.RecoveryProgress != nil {
		summary.RecoveryProgress = Ratio(*s.RecoveryProgress)
		// A wallet that is not being recovered reports an empty ratio.
		summary.Recovering = s.RecoveryProgress.Denominator != 0
	}
	for i, b := range s.AccountBalances {
		summary.Balances[i] = AccountBalance{
			AccountID:   b.AccountUuid,
			Ironwood:    fromFFIPoolBalance(b.Ironwood),
			Transparent: fromFFIPoolBalance(b.Transparent),
			Coinbase:    fromFFIPoolBalance(b.Coinbase),
			Total:       b.Total,
		}
	}
	return summary
}

func fromFFIPriority(p ffi.ScanPriority) scanPriority {
	switch p {
	case ffi.ScanPriorityIgnored:
		return priorityIgnored
	case ffi.ScanPriorityScanned:
		return priorityScanned
	case ffi.ScanPriorityHistoric:
		return priorityHistoric
	case ffi.ScanPriorityOpenAdjacent:
		return priorityOpenAdjacent
	case ffi.ScanPriorityFoundNote:
		return priorityFoundNote
	case ffi.ScanPriorityChainTip:
		return priorityChainTip
	default:
		return priorityVerify
	}
}

func toFFISubtreeRoots(roots []subtreeRoot) []ffi.SubtreeRoot {
	out := make([]ffi.SubtreeRoot, len(roots))
	for i, r := range roots {
		out[i] = ffi.SubtreeRoot{
			RootHash:              r.hash,
			CompletingBlockHeight: r.completingHeight,
		}
	}
	return out
}

func fromFFITxRequest(r ffi.TransactionDataRequest) (txRequest, error) {
	switch r := r.(type) {
	case ffi.TransactionDataRequestGetStatus:
		return txRequest{kind: txRequestStatus, txid: r.Txid}, nil
	case ffi.TransactionDataRequestEnhancement:
		return txRequest{kind: txRequestEnhancement, txid: r.Txid}, nil
	case ffi.TransactionDataRequestTransactionsInvolvingAddress:
		req := txRequest{
			kind:         txRequestAddress,
			address:      r.Address,
			start:        r.BlockRangeStart,
			txStatus:     fromFFITxStatusFilter(r.TxStatusFilter),
			outputStatus: fromFFIOutputStatusFilter(r.OutputStatusFilter),
		}
		if r.BlockRangeEnd != nil {
			req.end, req.bounded = *r.BlockRangeEnd, true
		}
		if r.RequestAt != nil {
			req.requestAt = time.Unix(int64(*r.RequestAt), 0)
		}
		return req, nil
	default:
		return txRequest{}, wrapf(ErrInternal, "unrecognized transaction data request %T", r)
	}
}

func fromFFITxStatusFilter(f ffi.TransactionStatusFilter) txStatusFilter {
	switch f {
	case ffi.TransactionStatusFilterMined:
		return txStatusMined
	case ffi.TransactionStatusFilterMempool:
		return txStatusMempool
	default:
		return txStatusAll
	}
}

func toFFITxStatusFilter(f txStatusFilter) ffi.TransactionStatusFilter {
	switch f {
	case txStatusMined:
		return ffi.TransactionStatusFilterMined
	case txStatusMempool:
		return ffi.TransactionStatusFilterMempool
	default:
		return ffi.TransactionStatusFilterAll
	}
}

func fromFFIOutputStatusFilter(f ffi.OutputStatusFilter) outputStatusFilter {
	if f == ffi.OutputStatusFilterUnspent {
		return outputStatusUnspent
	}
	return outputStatusAll
}

func toFFIOutputStatusFilter(f outputStatusFilter) ffi.OutputStatusFilter {
	if f == outputStatusUnspent {
		return ffi.OutputStatusFilterUnspent
	}
	return ffi.OutputStatusFilterAll
}
