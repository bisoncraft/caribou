// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"
	"fmt"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

// historyPage is the number of transactions read from zeclib at a time.
const historyPage = 200

// now is the clock that address expiry is compared with.
var now = time.Now

// setAddress caches the deposit address and the addresses of its receivers.
func (w *zecWallet) setAddress(address string) {
	owned := map[string]struct{}{address: {}}
	p, err := parseAddress(w.netCfg.network, address)
	if err != nil {
		w.log.Debugf("Parsing the deposit address: %v", err)
	} else {
		for _, r := range p.Receivers {
			if r.Address != "" {
				owned[r.Address] = struct{}{}
			}
		}
	}
	w.cache.mtx.Lock()
	w.cache.address = address
	w.cache.owned = owned
	w.cache.mtx.Unlock()
}

// DepositAddress returns the current deposit address: a unified address with
// an Ironwood and a transparent receiver. It changes only when NewAddress
// derives a new one, after the current one has received a payment.
func (w *zecWallet) DepositAddress() (string, error) {
	w.cache.mtx.Lock()
	address := w.cache.address
	w.cache.mtx.Unlock()
	if address == "" {
		return "", errClosed
	}
	return address, nil
}

// NewAddress returns a deposit address that has not received a payment. Core
// calls it each time the receive view opens. It returns the current address
// while that is unused, by AddressUsed's rule (no output in the history paid
// either receiver), and derives a new one only after the current one has been
// used. Every address handed out therefore stays inside the library's
// transparent gap window, which is what makes a restore find a payment to it.
//
// The library refuses a new address once gap-limit derived addresses in a row
// have no mined use (zcash_client_sqlite find_gap_start over
// v_address_first_use, whose uses are received outputs of every pool, the
// Ironwood pool included). Deriving only after a use keeps the wallet clear
// of that in normal operation. If the library still refuses with the gap-limit
// error after the history shows a use (its record of use is by mined height
// and address row, the history's by the scanned outputs and their receivers),
// NewAddress returns the current address with a nil error. That address has
// been used, so this is address reuse, and payments to its transparent
// receiver are linked on chain; it is logged as a warning once per address.
// No other error is treated this way.
//
// "Unused" is judged on the history the wallet has scanned. During a restore,
// or while a payment is unconfirmed (the wallet sees no mempool), an address
// that has been paid can still be handed out.
//
// The check reads the whole history, a page at a time with the guard taken
// per page as AddressUsed does, on every call; there is no cache. Calls are
// serialized by newAddrMtx from the check through the derivation and the
// cache's update, so two requests cannot both derive from the same used
// address. The derivation and the cache's update happen under one guard, so a
// Close cannot finish in between and leave the address in a closed instance.
func (w *zecWallet) NewAddress() (_ string, err error) {
	defer func() { err = w.explain(err) }()
	w.newAddrMtx.Lock()
	defer w.newAddrMtx.Unlock()
	current, err := w.DepositAddress()
	if err != nil {
		return "", err
	}
	used, err := w.AddressUsed(current)
	if err != nil {
		return "", err
	}
	if !used {
		return current, nil
	}
	be, account, release, err := w.backend()
	if err != nil {
		return "", err
	}
	defer release()
	address, err := be.NewAddress(account.ID, zeclib.ReceiversAll)
	if errors.Is(err, zeclib.ErrAddressGapLimit) {
		if last := w.gapLogged.Swap(&current); last == nil || *last != current {
			w.log.Warnf("The deposit address, which has received a payment, is handed out again: "+
				"no new address with a transparent receiver can be derived until one of the unused ones "+
				"receives a payment, and payments to its transparent receiver are linked on chain: %v", err)
		}
		return current, nil
	}
	if err != nil {
		return "", err
	}
	w.setAddress(address)
	return address, nil
}

// OwnsDepositAddress reports whether funds sent to address reach the wallet.
// An address that does not decode for the network is not the wallet's. A
// unified address is the wallet's only if every receiver a sender could pay
// is the wallet's: zeclib matches an address on any one receiver, and an
// address that joins the wallet's transparent receiver to someone else's
// Orchard receiver would be paid at the Orchard receiver. An address with a
// Sapling receiver, and a Sapling or TEX address, is not the wallet's.
func (w *zecWallet) OwnsDepositAddress(address string) (_ bool, err error) {
	defer func() { err = w.explain(err) }()
	if _, err := parseAddress(w.netCfg.network, address); err != nil {
		if errors.Is(err, zeclib.ErrInvalidArgument) {
			return false, nil
		}
		return false, err
	}
	be, account, release, err := w.backend()
	if err != nil {
		return false, err
	}
	defer release()
	return w.ownsAddress(be, account.ID, address)
}

// ownsAddress is OwnsDepositAddress for a caller that holds the guard of the
// backend.
func (w *zecWallet) ownsAddress(be backend, accountID, address string) (bool, error) {
	p, err := parseAddress(w.netCfg.network, address)
	if err != nil {
		if errors.Is(err, zeclib.ErrInvalidArgument) {
			return false, nil
		}
		return false, err
	}
	w.cache.mtx.Lock()
	current := address == w.cache.address
	owned := w.cache.owned
	w.cache.mtx.Unlock()
	if current {
		return true, nil
	}
	var parts []string
	switch p.Kind {
	case zeclib.AddressTransparent:
		parts = []string{address}
	case zeclib.AddressUnified:
		for _, r := range p.Receivers {
			if r.Kind == zeclib.ReceiverSapling {
				return false, nil
			}
			if r.Address != "" {
				parts = append(parts, r.Address)
			}
		}
	}
	if len(parts) == 0 {
		return false, nil
	}
	for _, part := range parts {
		if _, ok := owned[part]; ok {
			continue
		}
		id, ok, err := be.OwnsAddress(part)
		if errors.Is(err, zeclib.ErrInvalidArgument) {
			return false, nil
		}
		if err != nil || !ok || id != accountID {
			return false, err
		}
	}
	return true, nil
}

// receiverFor returns the address of the receiver of address that an output
// of pool paid: for a unified address the receiver-only address of the
// receiver of that pool, otherwise the address itself. Empty when the
// address has no receiver of the pool or does not parse.
func (w *zecWallet) receiverFor(address string, pool zeclib.OutputPool, parsed map[string]*zeclib.ParsedAddress) string {
	p, ok := parsed[address]
	if !ok {
		p, _ = parseAddress(w.netCfg.network, address)
		parsed[address] = p
	}
	if p == nil {
		return ""
	}
	if p.Kind != zeclib.AddressUnified {
		return address
	}
	want := zeclib.ReceiverOrchard
	switch pool {
	case zeclib.OutputTransparent:
		want = zeclib.ReceiverTransparent
	case zeclib.OutputSapling:
		want = zeclib.ReceiverSapling
	}
	for _, r := range p.Receivers {
		if r.Kind == want {
			return r.Address
		}
	}
	return ""
}

// AddressUsed reports whether an output paid to the address, or to one of
// its receivers, is in the wallet's history. Receivers are compared on both
// sides: the history records a received output under the address the wallet
// generated, often the full unified address, and the output's pool says
// which of its receivers was paid. It reads the whole history, a page at a
// time, so its cost grows with the history.
func (w *zecWallet) AddressUsed(address string) (_ bool, err error) {
	defer func() { err = w.explain(err) }()
	parsed := make(map[string]*zeclib.ParsedAddress)
	p, err := w.decodeAddress(address)
	if err != nil {
		return false, err
	}
	parsed[address] = p
	targets := make(map[string]struct{})
	if p.Kind == zeclib.AddressUnified {
		for _, r := range p.Receivers {
			if r.Address != "" {
				targets[r.Address] = struct{}{}
			}
		}
	} else {
		targets[address] = struct{}{}
	}
	var after uint64
	for {
		be, account, release, err := w.backend()
		if err != nil {
			return false, err
		}
		listing, err := be.Transactions(account.ID, after, historyPage)
		release()
		if err != nil {
			return false, err
		}
		for i := range listing.Records {
			r := &listing.Records[i]
			after = max(after, r.Seq)
			for _, o := range r.Outputs {
				if !o.ToWallet || o.Address == "" {
					continue
				}
				if _, hit := targets[w.receiverFor(o.Address, o.Pool, parsed)]; hit {
					return true, nil
				}
			}
		}
		if len(listing.Records) < historyPage {
			return false, nil
		}
	}
}

// ValidateAddress reports whether the wallet can pay the address: it decodes
// for the network, the wallet pays it, and it has not expired at the chain
// tip or by the clock. An absent expiry is none; a present zero is a value.
// Whether the address expires before a transaction would is left to the
// estimate.
func (w *zecWallet) ValidateAddress(address string) bool {
	p, err := parseAddress(w.netCfg.network, address)
	if err != nil || !p.Payable {
		return false
	}
	if p.ExpiryHeight != nil {
		if tip := w.cache.chainTip(); tip > *p.ExpiryHeight {
			return false
		}
	}
	if p.ExpiryTime != nil && uint64(max(now().Unix(), 0)) > *p.ExpiryTime {
		return false
	}
	return true
}

// decodeAddress parses an address for the network. An address that does not
// decode fails with errBadAddress, zeclib's error kept in the chain.
func (w *zecWallet) decodeAddress(address string) (*zeclib.ParsedAddress, error) {
	p, err := parseAddress(w.netCfg.network, address)
	if errors.Is(err, zeclib.ErrInvalidArgument) {
		return nil, fmt.Errorf("%w: %w", errBadAddress, err)
	}
	return p, err
}

// errNotPayable is the error of an address none of whose receivers the wallet
// can pay: a Sapling or TEX address, or a unified address with no Orchard and
// no transparent receiver.
var errNotPayable = errors.New("not an address this wallet pays")

// AddressReceivers lists the receivers of an address the wallet can pay. It
// reads the address alone, through zeclib's parser, which takes no lock and
// needs no open database, so it answers for a closed wallet too.
//
// The Ironwood receiver is the unified address reduced to its Orchard
// receiver, expiry kept: the same string the history records as the
// recipient of a shielded receive, which differs from the deposit address the
// wallet handed out. The transparent receiver is the bare t-address, or for
// a unified address with an expiry, a unified address reduced to the
// transparent receiver and the expiry, not a bare t-address. The wallet's own
// addresses have no expiry. A Sapling receiver is left out: the wallet
// neither hands one out nor pays one. A transparent address is its own one
// receiver. The receivers come shielded first, whatever the order of the
// encoding. Preferred marks the receiver a payment from this wallet would go
// to: Orchard when there is one, else transparent. The expiry is reported,
// not checked: an expired address still lists its receivers.
func (w *zecWallet) AddressReceivers(address string) (_ *asset.AddressReceivers, err error) {
	defer func() { err = w.explain(err) }()
	p, err := w.decodeAddress(address)
	if err != nil {
		return nil, err
	}
	if !p.Payable {
		return nil, fmt.Errorf("%w: %s", errNotPayable, p.Unpayable)
	}
	ar := new(asset.AddressReceivers)
	switch p.Kind {
	case zeclib.AddressTransparent:
		ar.Receivers = []*asset.AddressReceiver{{
			Kind: asset.ReceiverTransparent, Pool: "Transparent", Address: address, Preferred: true,
		}}
	case zeclib.AddressUnified:
		for _, r := range p.Receivers {
			switch r.Kind {
			case zeclib.ReceiverOrchard:
				ar.Receivers = append(ar.Receivers, &asset.AddressReceiver{
					Kind: asset.ReceiverShielded, Pool: "Ironwood", Address: r.Address,
					Preferred: p.PaidPool == zeclib.PaidIronwood,
				})
			case zeclib.ReceiverTransparent:
				ar.Receivers = append(ar.Receivers, &asset.AddressReceiver{
					Kind: asset.ReceiverTransparent, Pool: "Transparent", Address: r.Address,
					Preferred: p.PaidPool == zeclib.PaidTransparent,
				})
			}
		}
	}
	if len(ar.Receivers) == 0 {
		return nil, fmt.Errorf("%w: no receiver the wallet pays", errNotPayable)
	}
	if p.ExpiryHeight != nil {
		h := uint64(*p.ExpiryHeight)
		ar.ExpiryHeight = &h
	}
	if p.ExpiryTime != nil {
		t := *p.ExpiryTime
		ar.ExpiryTime = &t
	}
	return ar, nil
}
