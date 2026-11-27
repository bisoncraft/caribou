// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package dash

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/dash/masternode"
	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wtxmgr"
)

const (
	// offerLifetime is how long an offer that has signed nothing lasts, and so
	// how long its inputs are leased.
	offerLifetime = 72 * time.Hour
	// signedLifetime is how long an offer that has signed its inputs is kept
	// leased: the signatures stay good until an input is spent.
	signedLifetime = 30 * 24 * time.Hour
	// offerMaxFee is the most an offer pays beyond its share, in duffs.
	offerMaxFee = 100_000
	// offerDust is the smallest change worth making, which the funding leaves
	// room for.
	offerDust = 546
	// maxOfferInputs is the most inputs an offer funds with.
	maxOfferInputs = 20
	// maxBlobSize is the largest registration blob the wallet reads.
	maxBlobSize = 256 << 10
	// minFundingConf is the confirmations a funding output needs.
	minFundingConf = 1
	// registrationLabel labels the broadcast transaction.
	registrationLabel = "shared masternode registration"
	// proRegTxType is the type of a provider registration transaction.
	proRegTxType = 1
)

// fundingWallet is the part of the utxo wallet that offers use.
type fundingWallet interface {
	masternode.KeyDeriver
	ListUnspent(minconf, maxconf int32, account string) ([]*btcjson.ListUnspentResult, error)
	LeaseOutput(id wtxmgr.LockID, op wire.OutPoint, d time.Duration) (time.Time, error)
	ReleaseOutput(id wtxmgr.LockID, op wire.OutPoint) error
	NewAddress(account uint32, scope waddrmgr.KeyScope) (btcutil.Address, error)
	NewChangeAddress(account uint32, scope waddrmgr.KeyScope) (btcutil.Address, error)
	PrimaryScope() waddrmgr.KeyScope
	PrivKeyForAddress(a btcutil.Address) (*btcec.PrivateKey, error)
	PublishBisonTx(tx *bisonwire.Tx, label string) error
	Locked() bool
	// GetTransaction returns a transaction the wallet has, mined or not, and
	// wallet.ErrNoTx for one it does not.
	GetTransaction(txHash chainhash.Hash) (*wallet.GetTransactionResult, error)
}

// funding returns the wallet offers draw on.
func (dw *dashWallet) funding() (fundingWallet, error) {
	if dw.fw != nil {
		return dw.fw, nil
	}
	inner := dw.InnerWallet()
	if inner == nil {
		return nil, errors.New("wallet is not open")
	}
	return inner, nil
}

func (dw *dashWallet) clock() time.Time {
	if dw.now != nil {
		return dw.now()
	}
	return time.Now()
}

func (dw *dashWallet) rules() masternode.NetRules {
	switch dw.net.Name {
	case MainNetParams.Name:
		return masternode.MainnetRules
	case RegressionNetParams.Name:
		return masternode.RegtestRules
	}
	return masternode.TestnetRules
}

// walletSecrets signs a funding input with the key of its address.
type walletSecrets struct {
	fw  fundingWallet
	net *chaincfg.Params
}

func (s walletSecrets) GetKey(a btcutil.Address) (*btcec.PrivateKey, bool, error) {
	k, err := s.fw.PrivKeyForAddress(a)
	return k, true, err
}
func (s walletSecrets) GetScript(btcutil.Address) ([]byte, error) {
	return nil, errors.New("no scripts")
}
func (s walletSecrets) ChainParams() *chaincfg.Params { return s.net }

func parseOfferID(s string) ([32]byte, error) {
	var id [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, errors.New("invalid offer id")
	}
	copy(id[:], b)
	return id, nil
}

// manager and funding, and the lock every offer change holds: an offer's state
// is read, changed and stored as one step.
func (dw *dashWallet) offerParts() (*masternode.Manager, fundingWallet, error) {
	mgr, err := dw.manager()
	if err != nil {
		return nil, nil, err
	}
	fw, err := dw.funding()
	if err != nil {
		return nil, nil, err
	}
	return mgr, fw, nil
}

// releaseInputs releases the leases of an offer's inputs. An input that is no
// longer in the wallet, because it was spent, is not an error.
func releaseInputs(fw fundingWallet, o *masternode.Offer) error {
	var errs []error
	for _, fi := range o.Inputs {
		err := fw.ReleaseOutput(wtxmgr.LockID(o.ID), fi.OutPoint)
		// An input that is gone, or leased to another offer since this
		// offer's lease lapsed, is no longer this offer's to release.
		if err != nil && !errors.Is(err, wtxmgr.ErrUnknownOutput) && !errors.Is(err, wtxmgr.ErrOutputUnlockNotAllowed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CreateMasternodeOffer commits amount to a registration someone else builds.
func (dw *dashWallet) CreateMasternodeOffer(ctx context.Context, amount uint64) (*asset.MasternodeOffer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mgr, fw, err := dw.offerParts()
	if err != nil {
		return nil, err
	}
	if amount < masternode.MinShareAmount || amount > masternode.CollateralAmount-masternode.MinShareAmount {
		return nil, fmt.Errorf("a share is %d to %d duffs", int64(masternode.MinShareAmount),
			int64(masternode.CollateralAmount-masternode.MinShareAmount))
	}
	if fw.Locked() {
		return nil, errors.New("wallet is locked")
	}
	dw.offerMu.Lock()
	defer dw.offerMu.Unlock()

	// Everything that can fail without leaving a mark comes first.
	inputs, err := selectFunding(fw, int64(amount)+offerMaxFee+offerDust)
	if err != nil {
		return nil, err
	}
	scope := fw.PrimaryScope()
	refund, err := fw.NewAddress(0, scope)
	if err != nil {
		return nil, fmt.Errorf("refund address: %w", err)
	}
	reward, err := fw.NewAddress(0, scope)
	if err != nil {
		return nil, fmt.Errorf("reward address: %w", err)
	}
	change, err := fw.NewChangeAddress(0, scope)
	if err != nil {
		return nil, fmt.Errorf("change address: %w", err)
	}
	var scripts [3][]byte
	for i, a := range []btcutil.Address{refund, reward, change} {
		if scripts[i], err = txscript.PayToAddrScript(a); err != nil || !txscript.IsPayToPubKeyHash(scripts[i]) {
			return nil, errors.New("the wallet made an address that is not pay to key hash")
		}
	}
	// A reserved owner index is never reused, even if the offer fails below.
	idx, keyID, err := mgr.ReserveOwner()
	if err != nil {
		return nil, fmt.Errorf("reserving an owner key: %w", err)
	}

	now := dw.clock()
	o := &masternode.Offer{
		CreatedAt: now.Unix(), ExpiresAt: now.Add(offerLifetime).Unix(),
		State: masternode.OfferOpen, Amount: int64(amount), MaxFee: offerMaxFee,
		OwnerIndex: idx, OwnerKeyID: keyID,
		RefundScript: scripts[0], RewardScript: scripts[1], ChangeScript: scripts[2],
		Inputs: inputs,
	}
	if _, err := rand.Read(o.ID[:]); err != nil {
		return nil, err
	}
	var leased []masternode.FundingInput
	undo := func() {
		if rerr := releaseInputs(fw, &masternode.Offer{ID: o.ID, Inputs: leased}); rerr != nil {
			dw.log.Errorf("Releasing the leases of a failed offer: %v", rerr)
		}
	}
	for _, fi := range inputs {
		if _, err := fw.LeaseOutput(wtxmgr.LockID(o.ID), fi.OutPoint, offerLifetime); err != nil {
			undo()
			return nil, fmt.Errorf("leasing %v: %w", fi.OutPoint, err)
		}
		leased = append(leased, fi)
	}
	if err := mgr.Store().PutOffer(o); err != nil {
		undo()
		return nil, fmt.Errorf("storing the offer: %w", err)
	}
	return dw.toAssetOffer(o, string(masternode.OfferOpen)), nil
}

// selectFunding picks pay to key hash outputs, largest first, until they hold
// target. It picks few inputs: a wallet in dust is told so.
func selectFunding(fw fundingWallet, target int64) ([]masternode.FundingInput, error) {
	unspent, err := fw.ListUnspent(minFundingConf, 9999999, "")
	if err != nil {
		return nil, fmt.Errorf("listing funds: %w", err)
	}
	var coins []masternode.FundingInput
	for _, u := range unspent {
		h, err := chainhash.NewHashFromStr(u.TxID)
		if err != nil {
			continue
		}
		script, err := hex.DecodeString(u.ScriptPubKey)
		if err != nil || !txscript.IsPayToPubKeyHash(script) {
			continue
		}
		amt, err := btcutil.NewAmount(u.Amount)
		if err != nil || amt <= 0 {
			continue
		}
		coins = append(coins, masternode.FundingInput{
			OutPoint: wire.OutPoint{Hash: *h, Index: u.Vout},
			PkScript: script, Value: int64(amt),
		})
	}
	slices.SortFunc(coins, func(a, b masternode.FundingInput) int {
		switch {
		case a.Value > b.Value:
			return -1
		case a.Value < b.Value:
			return 1
		}
		return bytes.Compare(a.OutPoint.Hash[:], b.OutPoint.Hash[:])
	})
	var picked []masternode.FundingInput
	var total int64
	for _, c := range coins {
		if len(picked) == maxOfferInputs {
			break
		}
		picked = append(picked, c)
		total += c.Value
		if total >= target {
			return picked, nil
		}
	}
	return nil, fmt.Errorf("not enough funds in %d or fewer pay to key hash outputs: need %d, have %d", maxOfferInputs, target, total)
}

// MasternodeOffers lists the offers. An open or consented offer past its
// expiry is released and dropped first.
func (dw *dashWallet) MasternodeOffers(ctx context.Context) ([]*asset.MasternodeOffer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mgr, fw, err := dw.offerParts()
	if err != nil {
		return nil, err
	}
	dw.offerMu.Lock()
	defer dw.offerMu.Unlock()
	all, err := mgr.Store().Offers()
	if err != nil {
		return nil, err
	}
	registered := ownersOf(mgr)
	now := dw.clock().Unix()
	var out []*asset.MasternodeOffer
	reaped := false
	for _, o := range all {
		state := string(o.State)
		switch {
		case registered[o.OwnerKeyID]:
			state = "registered"
		case now >= o.ExpiresAt && (o.State == masternode.OfferOpen || o.State == masternode.OfferConsented):
			if err := releaseInputs(fw, o); err != nil {
				return nil, err
			}
			if err := mgr.Store().DeleteOffer(o.ID); err != nil {
				return nil, err
			}
			reaped = true
			continue
		case now >= o.ExpiresAt && o.State == masternode.OfferSigned:
			state = "expired"
		}
		out = append(out, dw.toAssetOffer(o, state))
	}
	// The inputs of an offer that expired are available again.
	if reaped {
		dw.PushBalance()
	}
	slices.SortFunc(out, func(a, b *asset.MasternodeOffer) int {
		if a.CreatedAt != b.CreatedAt {
			return int(a.CreatedAt - b.CreatedAt)
		}
		return bytes.Compare([]byte(a.ID), []byte(b.ID))
	})
	return out, nil
}

// ownersOf is the owner key ids of the shares the wallet has registered.
func ownersOf(mgr *masternode.Manager) map[[protx.KeyIDSize]byte]bool {
	m := make(map[[protx.KeyIDSize]byte]bool)
	for _, o := range mgr.List() {
		for _, i := range o.Shares {
			m[o.MN.Shares[i].KeyIDOwner] = true
		}
	}
	return m
}

func (dw *dashWallet) toAssetOffer(o *masternode.Offer, state string) *asset.MasternodeOffer {
	out := &asset.MasternodeOffer{
		ID: hex.EncodeToString(o.ID[:]), State: state,
		CreatedAt: o.CreatedAt, ExpiresAt: o.ExpiresAt,
		Amount: uint64(o.Amount), MaxFee: uint64(o.MaxFee),
		OwnerAddress:  dw.keyIDAddr(o.OwnerKeyID),
		RefundAddress: dw.addrOf(o.RefundScript),
		RewardAddress: dw.addrOf(o.RewardScript),
		ChangeAddress: dw.addrOf(o.ChangeScript),
	}
	if o.TxID != (chainhash.Hash{}) {
		out.TxID = o.TxID.String()
	}
	for _, fi := range o.Inputs {
		out.Inputs = append(out.Inputs, fi.OutPoint.String())
		out.InputsValue += uint64(fi.Value)
	}
	return out
}

// keyIDAddr is the pay to key hash address of a key id.
func (dw *dashWallet) keyIDAddr(id [protx.KeyIDSize]byte) string {
	a, err := btcutil.NewAddressPubKeyHash(id[:], dw.net)
	if err != nil {
		return hex.EncodeToString(id[:])
	}
	return a.EncodeAddress()
}

// addrOf is the address of a pay to key hash script, or the script in hex.
func (dw *dashWallet) addrOf(script []byte) string {
	if txscript.IsPayToPubKeyHash(script) {
		if a, err := btcutil.NewAddressPubKeyHash(script[3:23], dw.net); err == nil {
			return a.EncodeAddress()
		}
	}
	return hex.EncodeToString(script)
}

// CancelMasternodeOffer releases an offer's inputs and deletes it. It does not
// take back signatures the offer gave: a signed registration can still be
// broadcast by anyone who has it until one of its inputs is spent, and
// releasing the inputs does not spend them.
func (dw *dashWallet) CancelMasternodeOffer(ctx context.Context, offerID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := parseOfferID(offerID)
	if err != nil {
		return err
	}
	mgr, fw, err := dw.offerParts()
	if err != nil {
		return err
	}
	dw.offerMu.Lock()
	defer dw.offerMu.Unlock()
	o, err := mgr.Store().GetOffer(id)
	if err != nil {
		return err
	}
	if ownersOf(mgr)[o.OwnerKeyID] {
		return errors.New("the masternode is registered")
	}
	// A registration that was sent may be mined, or may mine, and the offer is
	// what the wallet shows it by until it does. One the wallet no longer has
	// was refused or dropped, and the offer can go: the track loop stops
	// waiting for what the wallet no longer has.
	if o.State == masternode.OfferBroadcast {
		if err := registrationGone(fw, o.TxID); err != nil {
			return err
		}
	}
	if err := releaseInputs(fw, o); err != nil {
		return err
	}
	return mgr.Store().DeleteOffer(id)
}

// registrationGone returns nil if the wallet no longer has the registration,
// and an error if it does, or could not tell. The wallet drops an unmined
// transaction only when its own publish is refused, not when another
// participant spends their input, so one still stored is sent to the backend
// again: the wallet drops it if the backend refuses it, and it is still
// pending if the backend takes it. One the backend says is already in a block
// stays stored, as a pending one, until the wallet has the block.
func registrationGone(fw fundingWallet, h chainhash.Hash) error {
	txid := h.String()
	res, err := fw.GetTransaction(h)
	if errors.Is(err, wallet.ErrNoTx) {
		return nil
	}
	if err != nil {
		return err
	}
	if res.BlockHash != nil {
		return fmt.Errorf("the registration %s is mined", txid)
	}
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(res.Summary.Transaction)); err != nil {
		return err
	}
	// A refusal is how a registration that can never mine is found out: the
	// wallet drops it, and the lookup below says so.
	pubErr := fw.PublishBisonTx(tx, registrationLabel)
	_, err = fw.GetTransaction(h)
	switch {
	case errors.Is(err, wallet.ErrNoTx):
		return nil
	case err != nil:
		return err
	case pubErr != nil:
		return fmt.Errorf("the registration %s is pending: it was broadcast and the wallet still has it, but the backend did not refuse it: %w", txid, pubErr)
	}
	return fmt.Errorf("the registration %s is pending: it was broadcast, the wallet still has it and the backend accepted it or already has it", txid)
}

// usableOffer loads an offer that can still review and sign.
func (dw *dashWallet) usableOffer(mgr *masternode.Manager, offerID string) (*masternode.Offer, error) {
	id, err := parseOfferID(offerID)
	if err != nil {
		return nil, err
	}
	o, err := mgr.Store().GetOffer(id)
	if err != nil {
		return nil, err
	}
	if dw.clock().Unix() >= o.ExpiresAt {
		return nil, errors.New("the offer has expired")
	}
	return o, nil
}

func parseRegBlob(blob string) (*bisonwire.Tx, []masternode.ShareSig, error) {
	if len(blob) > maxBlobSize {
		return nil, nil, fmt.Errorf("the registration is larger than %d bytes", maxBlobSize)
	}
	return masternode.ParseBlob([]byte(blob))
}

// ReviewMasternodeRegistration checks a registration against an offer.
func (dw *dashWallet) ReviewMasternodeRegistration(ctx context.Context, offerID, blob string) (*asset.MasternodeRegReview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mgr, _, err := dw.offerParts()
	if err != nil {
		return nil, err
	}
	dw.offerMu.Lock()
	defer dw.offerMu.Unlock()
	o, err := dw.usableOffer(mgr, offerID)
	if err != nil {
		return nil, err
	}
	tx, sigs, err := parseRegBlob(blob)
	if err != nil {
		return nil, err
	}
	rv, err := o.Review(tx, dw.rules())
	if err != nil {
		return nil, err
	}
	// The signatures that come with the blob must be good.
	if err := rv.AddJoinSigs(sigs); err != nil {
		return nil, err
	}
	// A signed offer signs again only a complete registration, so a review of
	// another says so now instead of when it is asked to sign.
	if o.State == masternode.OfferSigned && rv.Complete() != nil {
		return nil, errors.New("the offer has signed: review the complete registration")
	}
	return dw.toAssetReview(o, rv), nil
}

func (dw *dashWallet) toAssetReview(o *masternode.Offer, rv *masternode.OfferReview) *asset.MasternodeRegReview {
	out := &asset.MasternodeRegReview{
		ReviewID:        hex.EncodeToString(rv.ID[:]),
		OfferID:         hex.EncodeToString(o.ID[:]),
		ShareIndex:      rv.ShareIndex,
		Terms:           dw.toAssetTerms(&rv.Terms, rv.ShareIndex),
		CollateralIndex: rv.CollateralIndex,
		FeePortion:      uint64(rv.FeePortion),
		SignedShares:    rv.SignedShares(),
	}
	for _, o := range rv.OtherOutputs {
		out.OtherOutputs = append(out.OtherOutputs, &asset.MasternodeOutput{
			Value: uint64(o.Value), Script: hex.EncodeToString(o.PkScript),
		})
	}
	return out
}

func (dw *dashWallet) toAssetTerms(t *masternode.Terms, own int) asset.MasternodeTerms {
	out := asset.MasternodeTerms{
		Keys:              *dw.keysOf(t.PubKeyOperator, t.KeyIDVoting),
		OperatorReward:    t.OperatorReward,
		EarlyPeriodBlocks: t.EarlyPeriodBlocks,
		EarlyPenalty:      uint64(t.EarlyPenalty),
	}
	for i, s := range t.Shares {
		out.Shares = append(out.Shares, &asset.MasternodeShare{
			Amount: uint64(s.Amount), Own: i == own,
			RefundScript: hex.EncodeToString(s.RefundScript),
			RewardScript: hex.EncodeToString(s.RewardScript),
		})
	}
	for _, p := range t.NetInfo.Purposes {
		for _, e := range p.Entries {
			out.NetInfo = append(out.NetInfo, netEntryString(p.ID, &e))
		}
	}
	return out
}

func netEntryString(purpose uint8, e *protx.NetEntry) string {
	host := e.Domain
	if ip, ok := netip.AddrFromSlice(e.Addr); ok {
		host = ip.String()
	}
	return strconv.Itoa(int(purpose)) + ":" + net.JoinHostPort(host, strconv.Itoa(int(e.Port)))
}

// SignMasternodeRegistration signs the registration that reviewID was returned
// for. Its state is stored before any signature is returned, so a failure
// leaves nothing signed that the wallet does not know of.
func (dw *dashWallet) SignMasternodeRegistration(ctx context.Context, offerID, blob, reviewID string) (*asset.MasternodeRegSigned, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mgr, fw, err := dw.offerParts()
	if err != nil {
		return nil, err
	}
	if fw.Locked() {
		return nil, errors.New("wallet is locked")
	}
	dw.offerMu.Lock()
	defer dw.offerMu.Unlock()
	o, err := dw.usableOffer(mgr, offerID)
	if err != nil {
		return nil, err
	}
	tx, sigs, err := parseRegBlob(blob)
	if err != nil {
		return nil, err
	}
	rv, err := o.Review(tx, dw.rules())
	if err != nil {
		return nil, err
	}
	if got := hex.EncodeToString(rv.ID[:]); got != reviewID {
		return nil, masternode.ErrReviewChanged
	}
	if err := rv.AddJoinSigs(sigs); err != nil {
		return nil, err
	}
	key, err := masternode.OwnerKey(fw, dw.coinType, o.OwnerIndex)
	if err != nil {
		return nil, fmt.Errorf("owner key: %w", err)
	}
	defer key.Zero()
	own, err := rv.SignConsent(key)
	if err != nil {
		return nil, err
	}
	if err := rv.AddJoinSigs([]masternode.ShareSig{own}); err != nil {
		return nil, err
	}

	complete := rv.Complete() == nil
	// An offer that has signed never goes back to consented: its input
	// signatures are out, and a blob without every join signature would drop
	// the digest guard below and hide the signed registration from broadcast.
	if !complete && o.State == masternode.OfferSigned {
		return nil, errors.New("the offer has signed: send the complete registration")
	}
	if complete {
		if err := rv.SignInputs(walletSecrets{fw: fw, net: dw.net}); err != nil {
			return nil, err
		}
		raw, err := rv.Raw()
		if err != nil {
			return nil, err
		}
		signed := &bisonwire.Tx{Chain: "dash"}
		if err := signed.Deserialize(bytes.NewReader(raw)); err != nil {
			return nil, err
		}
		digest, err := masternode.SignedDigest(signed)
		if err != nil {
			return nil, err
		}
		// A retry of a signature that was given must give the same one.
		if o.State == masternode.OfferSigned && digest != o.SignedDigest {
			return nil, errors.New("the offer signed a different registration")
		}
		o.SignedDigest = digest
		// The signatures stay good until an input is spent: keep the inputs
		// leased for as long as the registration can be broadcast. A retry
		// keeps the first expiry and leases only until it.
		now := dw.clock()
		expires := now.Add(signedLifetime).Unix()
		if o.State == masternode.OfferSigned {
			expires = o.ExpiresAt
		}
		lease := time.Unix(expires, 0).Sub(now)
		for _, fi := range o.Inputs {
			if _, err := fw.LeaseOutput(wtxmgr.LockID(o.ID), fi.OutPoint, lease); err != nil {
				return nil, fmt.Errorf("keeping %v leased: %w", fi.OutPoint, err)
			}
		}
		o.State = masternode.OfferSigned
		o.ConsentHash = rv.ConsentHash
		o.ExpiresAt = expires
	} else {
		o.State = masternode.OfferConsented
		o.ConsentHash = rv.ConsentHash
	}
	// What can fail is done before the state is stored, so that after it only
	// losing the response can leave a signature out that the retry must get
	// back: signing a signed offer returns the same blob.
	b, err := rv.Blob()
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	if err := mgr.Store().PutOffer(o); err != nil {
		return nil, fmt.Errorf("storing the offer: %w", err)
	}
	return &asset.MasternodeRegSigned{Blob: string(data), Complete: complete}, nil
}

// BroadcastMasternodeRegistration broadcasts the registration the offer signed,
// once every input has its signature.
func (dw *dashWallet) BroadcastMasternodeRegistration(ctx context.Context, offerID, blob string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mgr, fw, err := dw.offerParts()
	if err != nil {
		return "", err
	}
	id, err := parseOfferID(offerID)
	if err != nil {
		return "", err
	}
	dw.offerMu.Lock()
	defer dw.offerMu.Unlock()
	o, err := mgr.Store().GetOffer(id)
	if err != nil {
		return "", err
	}
	if o.State != masternode.OfferSigned && o.State != masternode.OfferBroadcast {
		return "", fmt.Errorf("the offer is %s, not signed", o.State)
	}
	tx, _, err := parseRegBlob(blob)
	if err != nil {
		return "", err
	}
	digest, err := masternode.SignedDigest(tx)
	if err != nil {
		return "", err
	}
	if digest != o.SignedDigest {
		return "", errors.New("this is not the registration the offer signed")
	}
	for i, in := range tx.TxIn {
		if len(in.SignatureScript) == 0 {
			return "", fmt.Errorf("input %d is not signed", i)
		}
	}
	if err := fw.PublishBisonTx(tx, registrationLabel); err != nil {
		// The wallet records a transaction before it sends it, which tells the
		// masternode tracking, and takes it out again when it is refused, which
		// the track loop sees: it stops waiting for what the wallet dropped.
		return "", err
	}
	o.State, o.TxID = masternode.OfferBroadcast, tx.TxHash()
	if err := mgr.Store().PutOffer(o); err != nil {
		return o.TxID.String(), fmt.Errorf("the registration %s was broadcast but the offer was not updated: %w", o.TxID, err)
	}
	dw.PushBalance()
	return o.TxID.String(), nil
}
