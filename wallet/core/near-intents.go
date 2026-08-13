// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Cross-chain swaps through the NEAR Intents 1Click service. All
// Intents-specific Core functionality lives in this file; the API client
// is wallet/intents. Design and trust model: docs/near-intents-plan.md.
//
// A swap is identified by its single-use deposit address. Records persist
// in a table of the core database, indexed by creation time (for
// newest-first listing) and by activity (so the poller reads only swaps
// that still need polling), through the state machine QUOTED →
// DEPOSIT_SENT → (service statuses) → SUCCESS | REFUNDED | FAILED, with
// EXPIRED for quotes that were never funded. Quote signatures are verified
// once, at quote time, and the signed envelope is persisted; the service's
// status endpoint re-serializes quotes in a form that cannot be
// re-verified.

package core

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encrypt"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/intents"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

const (
	// NoteTypeIntentsSwap identifies cross-chain swap notifications.
	NoteTypeIntentsSwap = "intentsswap"

	// TopicIntentsSwapUpdate is emitted whenever a swap record changes.
	TopicIntentsSwapUpdate Topic = "intentsswapupdate"

	// intentsFeeRecipient is the identity of Bisoncraft's fee-collection
	// account inside the intents.near verifier, credited by the service's
	// appFees mechanism. The identity is an Ethereum-style address derived
	// from the offline fee-account key, whose ERC-191 signatures authorize
	// withdrawals; the key itself lives entirely outside the repo. Empty
	// disables the application fee.
	intentsFeeRecipient = "0x4bda081527f14e5ecb9a2fd7703759897c2860a6"

	// intentsDefaultSlippageBps is the default quote slippage tolerance.
	intentsDefaultSlippageBps = 100

	// intentsPollInterval is the cadence of the swap status poller while
	// any swap is active.
	intentsPollInterval = 15 * time.Second

	// intentsCatalogTTL is how long a fetched asset catalog stays fresh.
	intentsCatalogTTL = 15 * time.Minute

	// intentsExpiryGrace pads the quote deadline before an unfunded quote
	// is locally marked EXPIRED.
	intentsExpiryGrace = 10 * time.Minute
)

// IntentsSwap is the persisted cross-chain swap record. The type and its
// storage (including the engine-local states below) live in wallet/db; the
// state machine that drives it lives here.
type IntentsSwap = db.IntentsSwap

// Engine-local swap states. All other states mirror the service statuses
// in the intents package (StatusProcessing, StatusSuccess, ...).
const (
	IntentsStateQuoted      = db.IntentsStateQuoted
	IntentsStateDepositSent = db.IntentsStateDepositSent
	IntentsStateExpired     = db.IntentsStateExpired
)

// intentsDeadlines is the quote deadline (refund-activation time)
// requested per origin chain. The window must cover deposit confirmation
// on the origin chain; the service may extend it further.
var intentsDeadlines = map[uint32]time.Duration{
	0:   2 * time.Hour, // btc
	2:   time.Hour,     // ltc
	3:   time.Hour,     // doge
	5:   time.Hour,     // dash
	133: time.Hour,     // zec
	145: time.Hour,     // bch
}

func intentsDeadline(originAssetID uint32) time.Duration {
	if d, ok := intentsDeadlines[intentsChainID(originAssetID)]; ok {
		return d
	}
	return 45 * time.Minute // account-based chains confirm in seconds
}

// intentsChainID returns the chain a wallet asset settles on: the parent
// chain for tokens, the asset itself otherwise.
func intentsChainID(assetID uint32) uint32 {
	if tok := asset.TokenInfo(assetID); tok != nil {
		return tok.NetworkID
	}
	return assetID
}

// resolveIntentsAsset maps a wallet asset to its catalog entry: the chain
// native for chain assets, the contract-matched entry for tokens (matched
// on the token's contract address for the current network). nil means the
// asset is not tradable through the service.
func resolveIntentsAsset(cat *intents.Catalog, assetID uint32) *intents.Token {
	if tok := asset.TokenInfo(assetID); tok != nil {
		if tok.ContractAddress == "" {
			return nil
		}
		return cat.FindContract(tok.NetworkID, tok.ContractAddress)
	}
	return cat.Native(assetID)
}

// IntentsSwapNote reports a new or updated swap record.
type IntentsSwapNote struct {
	db.Notification
	Swap *IntentsSwap `json:"swap"`
}

func newIntentsSwapNote(swap *IntentsSwap, subject, details string, severity db.Severity) *IntentsSwapNote {
	return &IntentsSwapNote{
		Notification: db.NewNotification(NoteTypeIntentsSwap, TopicIntentsSwapUpdate, subject, details, severity),
		Swap:         swap,
	}
}

// intentsEngine owns the swap records (a table of the core database), the
// 1Click client, the cached asset catalog, and the status poller.
type intentsEngine struct {
	c      *Core
	log    util.Logger
	client *intents.Client
	db     db.DB
	ctx    context.Context // the engine's run context, for background refreshes

	catMtx       sync.Mutex
	catalog      *intents.Catalog
	catalogFetch bool // a background catalog refresh is in flight

	// swapMtx serializes the read-modify-write of a swap record so a stale
	// in-flight copy (a poll started before another writer) cannot clobber
	// a newer state. Network calls are made outside it.
	swapMtx sync.Mutex

	trigger chan struct{}
}

// intentsAppFees returns the configured application fee entries.
func intentsAppFees() []intents.AppFee {
	if intentsFeeRecipient == "" {
		return nil
	}
	return []intents.AppFee{{Recipient: intentsFeeRecipient, Fee: intents.DefaultAppFeeBps}}
}

// startIntentsEngine launches the swap engine. Failure to initialize
// disables cross-chain swaps for the session but is not fatal to Core.
func (c *Core) startIntentsEngine(ctx context.Context) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		logger := c.log.SubLogger("INTENTS")
		baseURL, signingKey := c.cfg.IntentsBaseURL, c.cfg.IntentsSigningKey
		if (baseURL != "" || signingKey != "") && c.net == util.Mainnet {
			// A config-injected endpoint or key on mainnet would bypass
			// the pinned quote-signing key and let the endpoint's quotes
			// redirect real deposits.
			logger.Warnf("Ignoring 1Click API overrides on mainnet")
			baseURL, signingKey = "", ""
		}
		client, err := intents.NewClient(&intents.Config{
			BaseURL:    baseURL,
			SigningKey: signingKey,
			AppFees:    intentsAppFees(),
			Proxy:      c.cfg.TorProxy,
		}, logger)
		if err != nil {
			logger.Errorf("Error creating 1Click client: %v", err)
			return
		}
		// Swap records live in the core database (c.db), whose close
		// Core.Run sequences after the poller (which runs under c.wg). The
		// API methods (Execute/Refresh/Reconstruct) run on HTTP-handler
		// goroutines that Core.Run does not wait on, so a shutdown racing a
		// store from one of them can drop that write — most consequentially
		// the DEPOSIT_SENT/txid update right after a deposit send. Such a
		// dropped record is recovered on the next launch by the resume poll
		// and, failing that, by reconstruction, so the loss is bounded to a
		// stale local state, never funds.
		eng := &intentsEngine{
			c:       c,
			log:     logger,
			client:  client,
			db:      c.db,
			ctx:     ctx,
			trigger: make(chan struct{}, 1),
		}
		c.intentsEng.Store(eng)
		eng.run(ctx)
	}()
}

// intentsEngine returns the running engine, or an error if cross-chain
// swaps are unavailable this session.
func (c *Core) intentsEngine() (*intentsEngine, error) {
	if eng, ok := c.intentsEng.Load().(*intentsEngine); ok {
		return eng, nil
	}
	return nil, errors.New("cross-chain swap engine not available")
}

// run resumes any active swaps and polls until the context dies. The
// ticker is parked while no swap is active so an idle wallet does not
// re-scan the record table forever; poke() (on a new swap) resumes it.
func (e *intentsEngine) run(ctx context.Context) {
	ticker := time.NewTicker(intentsPollInterval)
	defer ticker.Stop()
	// Park only on a successful load that found nothing active. A load
	// error leaves the ticker running so the next tick retries — parking on
	// a transient failure would strand genuinely active swaps.
	if active, loaded := e.pollActive(ctx); loaded && active == 0 {
		ticker.Stop()
	}
	for {
		select {
		case <-ticker.C:
			if active, loaded := e.pollActive(ctx); loaded && active == 0 {
				ticker.Stop()
			}
		case <-e.trigger:
			ticker.Reset(intentsPollInterval)
			e.pollActive(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// poke nudges the poller without waiting for the next tick.
func (e *intentsEngine) poke() {
	select {
	case e.trigger <- struct{}{}:
	default:
	}
}

// pollActive refreshes all non-terminal swaps from the service and returns
// the number of active swaps found, so the caller can park the poller when
// none remain. loaded is false when the active set could not be read, so
// the caller can keep polling rather than mistake the failure for an empty
// set and park.
func (e *intentsEngine) pollActive(ctx context.Context) (active int, loaded bool) {
	swaps, err := e.db.ActiveIntentsSwaps()
	if err != nil {
		e.log.Errorf("Error loading active swap records: %v", err)
		return 0, false
	}
	for _, swap := range swaps {
		if ctx.Err() != nil {
			return active, true
		}
		active++
		if err := e.refreshSwap(ctx, swap); err != nil {
			e.log.Errorf("Error refreshing swap %s: %v", swap.DepositAddress, err)
		}
	}
	return active, true
}

// refreshSwap polls the service for one swap (the network call is made
// without holding swapMtx) and applies the result under the lock.
func (e *intentsEngine) refreshSwap(ctx context.Context, swap *IntentsSwap) error {
	status, err := e.client.Status(ctx, swap.DepositAddress)
	if err != nil {
		if errors.Is(err, intents.ErrSwapNotFound) {
			// Not necessarily fatal: the service can lag a fresh quote. Only
			// a swap past its deadline is timed out.
			e.applyTimeout(swap.DepositAddress)
			return nil
		}
		return err
	}

	if status.Status == intents.StatusPendingDeposit {
		// The service is still waiting; keep the local view (QUOTED or
		// DEPOSIT_SENT) unless the deadline has passed.
		e.applyTimeout(swap.DepositAddress)
		return nil
	}

	realizedAtoms, haveRealized := e.realizedAtomsOut(ctx, swap, status)
	updated, note := e.mergeStatus(swap.DepositAddress, status, realizedAtoms, haveRealized)
	if note != nil {
		e.c.notify(note)
	}
	if updated != nil && updated.Terminal() {
		if updated.DestAssetID != intentsAssetUnknown {
			e.c.updateAssetBalance(updated.DestAssetID)
		}
		if updated.State == intents.StatusRefunded {
			e.c.updateAssetBalance(updated.OriginAssetID)
		}
	}
	return nil
}

// realizedAtomsOut converts the realized fill reported in a status
// response's swap details to destination-wallet atoms. ok is false when
// the details carry no fill, the fill matches the recorded amount, or the
// destination chain or its decimals cannot be resolved — in which case
// the recorded amounts are left in place. Called outside swapMtx: the
// catalog fetch can hit the network.
func (e *intentsEngine) realizedAtomsOut(ctx context.Context, swap *IntentsSwap, status *intents.StatusResponse) (atoms uint64, ok bool) {
	out := status.SwapDetails.AmountOut
	if out == "" || out == swap.AmountOut || swap.DestAssetID == intentsAssetUnknown {
		return 0, false
	}
	unitInfo, err := asset.UnitInfo(swap.DestAssetID)
	if err != nil {
		return 0, false
	}
	cat, err := e.freshCatalog(ctx)
	if err != nil {
		return 0, false
	}
	tok := cat.ByAssetID(swap.DestAsset)
	if tok == nil {
		return 0, false
	}
	atoms, err = fromIntentsAmount(out, &unitInfo, tok.Decimals)
	if err != nil {
		e.log.Warnf("Unconvertible realized amountOut %q for swap %s: %v", out, swap.DepositAddress, err)
		return 0, false
	}
	return atoms, true
}

// mergeStatus reloads the record under swapMtx, applies the service status
// when it is a genuine change and the record is not already terminal,
// persists it, and returns the updated record and a notification (both nil
// when nothing changed). Reloading under the lock prevents a stale caller
// copy from regressing a newer state. realizedAtoms (with haveRealized)
// is the swap details' realized fill in destination atoms, replacing the
// quote-time output so history shows what was actually delivered.
func (e *intentsEngine) mergeStatus(depositAddress string, status *intents.StatusResponse, realizedAtoms uint64, haveRealized bool) (*IntentsSwap, Notification) {
	e.swapMtx.Lock()
	defer e.swapMtx.Unlock()

	swap, err := e.db.IntentsSwap(depositAddress)
	if err != nil {
		e.log.Errorf("Error reloading swap %s: %v", depositAddress, err)
		return nil, nil
	}
	if swap.Terminal() {
		return nil, nil // never regress a terminal record
	}

	newState := status.Status
	details := &status.SwapDetails
	// Recheck the realized fill against the reloaded record, not the
	// caller's copy.
	realized := haveRealized && details.AmountOut != swap.AmountOut
	changed := newState != swap.State || realized ||
		len(details.NearTxHashes) != len(swap.NearTxHashes) ||
		len(details.DestinationChainTxs) != len(swap.DestinationTxs) ||
		details.RefundedAmount != swap.RefundedAmount ||
		details.RefundReason != swap.RefundReason
	if !changed {
		return nil, nil
	}

	swap.State = newState
	if realized {
		swap.AmountOut = details.AmountOut
		swap.AtomsOut = realizedAtoms
	}
	swap.NearTxHashes = details.NearTxHashes
	swap.DestinationTxs = txHashes(details.DestinationChainTxs)
	swap.RefundedAmount = details.RefundedAmount
	swap.RefundReason = details.RefundReason
	// Origin-chain hashes include the refund transaction when one occurs.
	if newState == intents.StatusRefunded {
		swap.RefundTxs = txHashes(details.OriginChainTxs)
	}
	if err := e.db.StoreIntentsSwap(swap); err != nil {
		e.log.Errorf("Error storing swap %s: %v", depositAddress, err)
		return nil, nil
	}

	subject, severity := intentsNoteContent(newState)
	note := newIntentsSwapNote(swap, subject,
		fmt.Sprintf("%s → %s swap %s",
			intentsAssetLabel(swap.OriginAssetID, swap.OriginAsset),
			intentsAssetLabel(swap.DestAssetID, swap.DestAsset),
			strings.ToLower(strings.ReplaceAll(newState, "_", " "))),
		severity)
	return swap, note
}

// applyTimeout moves a swap that never progressed to a terminal state once
// its deadline (plus grace) has passed, so a swap the service never
// advances is not polled forever. A quote that was never funded becomes
// EXPIRED (no funds moved). A deposit the service never observed by the
// deadline becomes FAILED — the deadline is also the deposit window, so a
// late deposit will not be accepted for this quote; because a deposit
// transaction may have left the wallet, the message points to escalation
// rather than claiming no funds moved. The record is reloaded under
// swapMtx so a concurrent writer's newer state is not overwritten.
func (e *intentsEngine) applyTimeout(depositAddress string) {
	e.swapMtx.Lock()
	swap, err := e.db.IntentsSwap(depositAddress)
	if err != nil {
		e.swapMtx.Unlock()
		return
	}
	// A zero deadline (a record that predates deadline persistence, or a
	// reconstruction fallback that failed) can never prove a timeout.
	if swap.Deadline.IsZero() || time.Now().Before(swap.Deadline.Add(intentsExpiryGrace)) {
		e.swapMtx.Unlock()
		return
	}
	var note Notification
	switch swap.State {
	case IntentsStateQuoted:
		swap.State = IntentsStateExpired
		if err := e.db.StoreIntentsSwap(swap); err == nil {
			note = newIntentsSwapNote(swap, "Swap expired",
				"The quote expired with no deposit recorded.", db.WarningLevel)
		} else {
			e.log.Errorf("Error storing expired swap %s: %v", depositAddress, err)
		}
	case IntentsStateDepositSent:
		swap.State = intents.StatusFailed
		swap.RefundReason = "the service did not observe a deposit before the quote deadline"
		if err := e.db.StoreIntentsSwap(swap); err == nil {
			note = newIntentsSwapNote(swap, "Swap needs attention",
				"The service did not observe the deposit before the deadline. If a deposit left your wallet, check its transaction and the service status page.", db.ErrorLevel)
		} else {
			e.log.Errorf("Error storing timed-out swap %s: %v", depositAddress, err)
		}
	}
	e.swapMtx.Unlock()

	if note != nil {
		e.c.notify(note)
	}
}

func txHashes(refs []intents.TxHashRef) []string {
	if len(refs) == 0 {
		return nil
	}
	hashes := make([]string, 0, len(refs))
	for _, r := range refs {
		hashes = append(hashes, r.Hash)
	}
	return hashes
}

func intentsNoteContent(state string) (subject string, severity db.Severity) {
	switch state {
	case intents.StatusSuccess:
		return "Swap complete", db.Success
	case intents.StatusRefunded:
		return "Swap refunded", db.WarningLevel
	case intents.StatusFailed:
		return "Swap failed", db.ErrorLevel
	case intents.StatusIncompleteDeposit:
		return "Swap deposit incomplete", db.WarningLevel
	default:
		return "Swap update", db.Data
	}
}

// freshCatalog returns the asset catalog. A fresh cache is returned
// directly. A stale-but-present cache is returned immediately while a
// single background refresh runs, so callers on the swap UI path never
// block on the network. Only a cold cache blocks on the fetch.
func (e *intentsEngine) freshCatalog(ctx context.Context) (*intents.Catalog, error) {
	e.catMtx.Lock()
	cached := e.catalog
	if cached != nil && time.Since(cached.FetchedAt) < intentsCatalogTTL {
		e.catMtx.Unlock()
		return cached, nil
	}
	if cached != nil {
		// Serve stale, refresh in the background (at most one in flight).
		if !e.catalogFetch {
			e.catalogFetch = true
			go e.refreshCatalog()
		}
		e.catMtx.Unlock()
		return cached, nil
	}
	e.catMtx.Unlock()

	// Cold cache: block on the first fetch.
	cat, err := e.client.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("error fetching asset catalog: %w", err)
	}
	e.catMtx.Lock()
	e.catalog = cat
	e.catMtx.Unlock()
	return cat, nil
}

// refreshCatalog fetches the catalog on the engine's context and updates
// the cache. It runs in the background from freshCatalog; catalogFetch
// guards against concurrent refreshes.
func (e *intentsEngine) refreshCatalog() {
	defer func() {
		e.catMtx.Lock()
		e.catalogFetch = false
		e.catMtx.Unlock()
	}()
	cat, err := e.client.Catalog(e.ctx)
	if err != nil {
		e.log.Warnf("Error refreshing asset catalog, keeping stale: %v", err)
		return
	}
	e.catMtx.Lock()
	e.catalog = cat
	e.catMtx.Unlock()
}

// pow10Exp returns n as an exponent of 10, or ok=false if n is not a
// power of ten.
func pow10Exp(n uint64) (exp int, ok bool) {
	if n == 0 {
		return 0, false
	}
	for n > 1 {
		if n%10 != 0 {
			return 0, false
		}
		n /= 10
		exp++
	}
	return exp, true
}

// walletDecimalExp returns the wallet's atomic-unit decimal exponent.
// Wallet atomic units and Intents-side base units may use different
// scales for the same asset (e.g. NEAR: 1e8 drops vs 24 decimals);
// toIntentsAmount scales wallet atoms to the token's decimals and
// fromIntentsAmount is the inverse, truncating.
func walletDecimalExp(unitInfo *util.UnitInfo) (int, error) {
	walletExp, ok := pow10Exp(unitInfo.Conventional.ConversionFactor)
	if !ok {
		return 0, fmt.Errorf("conversion factor %d is not a power of ten",
			unitInfo.Conventional.ConversionFactor)
	}
	return walletExp, nil
}

func toIntentsAmount(atoms uint64, unitInfo *util.UnitInfo, decimals int) (string, error) {
	walletExp, err := walletDecimalExp(unitInfo)
	if err != nil {
		return "", err
	}
	v := new(big.Int).SetUint64(atoms)
	diff := decimals - walletExp
	switch {
	case diff > 0:
		v.Mul(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(diff)), nil))
	case diff < 0:
		v.Quo(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-diff)), nil))
	}
	return v.String(), nil
}

func fromIntentsAmount(amount string, unitInfo *util.UnitInfo, decimals int) (uint64, error) {
	walletExp, err := walletDecimalExp(unitInfo)
	if err != nil {
		return 0, err
	}
	v, ok := new(big.Int).SetString(amount, 10)
	if !ok {
		return 0, fmt.Errorf("bad amount %q", amount)
	}
	diff := decimals - walletExp
	switch {
	case diff > 0:
		v.Quo(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(diff)), nil))
	case diff < 0:
		v.Mul(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-diff)), nil))
	}
	if !v.IsUint64() {
		return 0, fmt.Errorf("amount %s overflows atomic units", amount)
	}
	return v.Uint64(), nil
}

// intentsWalletAddress returns the wallet's own deposit address.
func intentsWalletAddress(w *xcWallet) (string, error) {
	if addr := w.currentDepositAddress(); addr != "" {
		return addr, nil
	}
	return w.refreshDepositAddress()
}

// intentsPair resolves both legs of a swap pair against the catalog and
// the wallet set. Chain natives resolve via the catalog's native entry;
// tokens resolve by contract address.
func (c *Core) intentsPair(ctx context.Context, eng *intentsEngine, originAssetID, destAssetID uint32) (origin, dest *intents.Token, originWallet, destWallet *xcWallet, err error) {
	cat, err := eng.freshCatalog(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	origin, dest = resolveIntentsAsset(cat, originAssetID), resolveIntentsAsset(cat, destAssetID)
	if origin == nil {
		return nil, nil, nil, nil, fmt.Errorf("%s is not supported for cross-chain swaps", unbip(originAssetID))
	}
	if dest == nil {
		return nil, nil, nil, nil, fmt.Errorf("%s is not supported for cross-chain swaps", unbip(destAssetID))
	}
	originWallet, found := c.wallet(originAssetID)
	if !found {
		return nil, nil, nil, nil, newError(missingWalletErr, "no wallet found for %s", unbip(originAssetID))
	}
	destWallet, found = c.wallet(destAssetID)
	if !found {
		return nil, nil, nil, nil, newError(missingWalletErr, "no wallet found for %s", unbip(destAssetID))
	}
	return origin, dest, originWallet, destWallet, nil
}

// intentsQuoteRequest assembles a quote request for a pair. The refund and
// recipient addresses are always the wallets' own (seed-derived), so every
// value path terminates at recoverable addresses.
func (c *Core) intentsQuoteRequest(dry bool, origin, dest *intents.Token, originWallet, destWallet *xcWallet, atoms uint64, slippageBps int) (*intents.QuoteRequest, error) {
	amount, err := toIntentsAmount(atoms, &originWallet.Info().UnitInfo, origin.Decimals)
	if err != nil {
		return nil, err
	}
	refundTo, err := intentsWalletAddress(originWallet)
	if err != nil {
		return nil, fmt.Errorf("error getting refund address: %w", err)
	}
	recipient, err := intentsWalletAddress(destWallet)
	if err != nil {
		return nil, fmt.Errorf("error getting recipient address: %w", err)
	}
	if slippageBps <= 0 {
		slippageBps = intentsDefaultSlippageBps
	}
	return &intents.QuoteRequest{
		Dry:               dry,
		SwapType:          intents.SwapTypeExactInput,
		SlippageTolerance: slippageBps,
		OriginAsset:       origin.AssetID,
		DepositType:       intents.LocationOriginChain,
		DestinationAsset:  dest.AssetID,
		Amount:            amount,
		RefundTo:          refundTo,
		RefundType:        intents.LocationOriginChain,
		Recipient:         recipient,
		RecipientType:     intents.LocationDestinationChain,
		Deadline:          time.Now().Add(intentsDeadline(originWallet.AssetID)).UTC().Format("2006-01-02T15:04:05.000Z"),
	}, nil
}

// IntentsAsset describes one asset available for cross-chain swaps.
// AtomDecimals is the decimal exponent of the wallet's atomic unit, which
// is what the atoms fields of IntentsPreview and IntentsSwap are expressed
// in. It can differ from Decimals (the Intents-side scale, e.g. NEAR: 8
// wallet drops vs 24 Intents decimals); consumers must use AtomDecimals to
// convert those atoms to display units, not Decimals.
type IntentsAsset struct {
	AssetID      uint32  `json:"assetID"`
	Symbol       string  `json:"symbol"`
	IntentsID    string  `json:"intentsID"`
	Decimals     int     `json:"decimals"`
	AtomDecimals int     `json:"atomDecimals"`
	PriceUSD     float64 `json:"priceUSD"`
	WalletReady  bool    `json:"walletReady"`
}

// IntentsAssets returns the swappable assets: the intersection of the
// service catalog with this build's chain mapping and registered tokens,
// flagged by wallet availability. A token is included when its contract
// address on the current network appears in the service catalog.
func (c *Core) IntentsAssets(ctx context.Context) ([]*IntentsAsset, error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return nil, err
	}
	cat, err := eng.freshCatalog(ctx)
	if err != nil {
		return nil, err
	}
	entry := func(assetID uint32, tok *intents.Token) *IntentsAsset {
		wallet, found := c.wallet(assetID)
		atomDecimals := tok.Decimals
		if unitInfo, err := asset.UnitInfo(assetID); err == nil {
			if exp, ok := pow10Exp(unitInfo.Conventional.ConversionFactor); ok {
				atomDecimals = exp
			}
		}
		return &IntentsAsset{
			AssetID:      assetID,
			Symbol:       unbip(assetID),
			IntentsID:    tok.AssetID,
			Decimals:     tok.Decimals,
			AtomDecimals: atomDecimals,
			PriceUSD:     tok.Price,
			WalletReady:  found && wallet.connected(),
		}
	}
	var assets []*IntentsAsset
	for _, chainID := range cat.Chains() {
		assets = append(assets, entry(chainID, cat.Native(chainID)))
		ra := asset.Asset(chainID)
		if ra == nil {
			continue
		}
		for tokenID, rtok := range ra.Tokens {
			if catTok := cat.FindContract(chainID, rtok.ContractAddress); catTok != nil {
				assets = append(assets, entry(tokenID, catTok))
			}
		}
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].AssetID < assets[j].AssetID })
	return assets, nil
}

// verifyQuoteAssets checks that the assets echoed in a quote response
// resolve, through the catalog, to the origin and destination chains that
// were requested. The quote signature proves the service signed what it
// received, not that it received what was sent; a request tamperer could
// otherwise redirect the swap to a different (worthless) token on the same
// chain, leaving the recipient address valid and every signed field intact.
// Resolution goes through the catalog, so the service's normalization
// between ID schemes (nep141 ↔ 1cs_v1) does not cause false rejections.
func (e *intentsEngine) verifyQuoteAssets(ctx context.Context, resp *intents.QuoteResponse, originAssetID, destAssetID uint32) error {
	cat, err := e.freshCatalog(ctx)
	if err != nil {
		return err
	}
	if err := verifyQuoteAsset(cat, resp.Request.OriginAsset, originAssetID); err != nil {
		return fmt.Errorf("quote origin asset: %w", err)
	}
	if err := verifyQuoteAsset(cat, resp.Request.DestinationAsset, destAssetID); err != nil {
		return fmt.Errorf("quote destination asset: %w", err)
	}
	return nil
}

// verifyQuoteAsset checks that one echoed Intents asset ID resolves,
// through the catalog, to the wallet asset that was requested: the chain's
// native entry for chain assets, or a token entry with the requested
// token's contract address (matched case-insensitively) for tokens.
func verifyQuoteAsset(cat *intents.Catalog, echoedID string, walletAssetID uint32) error {
	chainID, native, ok := cat.AssetChain(echoedID)
	if !ok || chainID != intentsChainID(walletAssetID) {
		return fmt.Errorf("%q does not match requested %s", echoedID, unbip(walletAssetID))
	}
	tok := asset.TokenInfo(walletAssetID)
	if tok == nil {
		if !native {
			return fmt.Errorf("%q is not %s's native asset", echoedID, unbip(walletAssetID))
		}
		return nil
	}
	echoed := cat.ByAssetID(echoedID)
	if native || echoed == nil || !strings.EqualFold(echoed.ContractAddress, tok.ContractAddress) {
		return fmt.Errorf("%q is not the %s token contract %s", echoedID, unbip(walletAssetID), tok.ContractAddress)
	}
	return nil
}

// IntentsPreview is a dry-quote price preview. No deposit address is
// created and no compliance screening runs.
type IntentsPreview struct {
	AtomsIn      uint64           `json:"atomsIn"`
	AtomsOut     uint64           `json:"atomsOut"`
	MinAtomsOut  uint64           `json:"minAtomsOut"`
	AmountInUSD  string           `json:"amountInUSD"`
	AmountOutUSD string           `json:"amountOutUSD"`
	MinAtomsIn   uint64           `json:"minAtomsIn"`
	TimeEstimate int64            `json:"timeEstimate"`
	AppFees      []intents.AppFee `json:"appFees"`
	RefundFee    string           `json:"refundFee"`
	WithdrawFee  string           `json:"withdrawFee"`
}

// IntentsPreview obtains a dry quote for a prospective swap.
func (c *Core) IntentsPreview(ctx context.Context, originAssetID, destAssetID uint32, atoms uint64, slippageBps int) (*IntentsPreview, error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return nil, err
	}
	origin, dest, originWallet, destWallet, err := c.intentsPair(ctx, eng, originAssetID, destAssetID)
	if err != nil {
		return nil, err
	}
	req, err := c.intentsQuoteRequest(true, origin, dest, originWallet, destWallet, atoms, slippageBps)
	if err != nil {
		return nil, err
	}
	resp, err := eng.client.Quote(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := eng.verifyQuoteAssets(ctx, resp, originAssetID, destAssetID); err != nil {
		return nil, err
	}

	destUnits := &destWallet.Info().UnitInfo
	atomsOut, err := fromIntentsAmount(resp.Quote.AmountOut, destUnits, dest.Decimals)
	if err != nil {
		return nil, err
	}
	minAtomsOut, err := fromIntentsAmount(resp.Quote.MinAmountOut, destUnits, dest.Decimals)
	if err != nil {
		return nil, err
	}
	minAtomsIn, err := fromIntentsAmount(resp.Quote.MinAmountIn, &originWallet.Info().UnitInfo, origin.Decimals)
	if err != nil {
		return nil, err
	}

	return &IntentsPreview{
		AtomsIn:      atoms,
		AtomsOut:     atomsOut,
		MinAtomsOut:  minAtomsOut,
		AmountInUSD:  resp.Quote.AmountInUsd,
		AmountOutUSD: resp.Quote.AmountOutUsd,
		MinAtomsIn:   minAtomsIn,
		TimeEstimate: resp.Quote.TimeEstimate,
		AppFees:      resp.Request.AppFees,
		RefundFee:    resp.Quote.RefundFee,
		WithdrawFee:  resp.Quote.WithdrawFee,
	}, nil
}

// IntentsExecute obtains a committed quote and funds it: verifies and
// persists the quote, sends the origin-chain deposit from the origin
// wallet, and hands tracking to the status poller. The returned record is
// in state DEPOSIT_SENT on success. If the deposit send fails the record
// remains QUOTED and expires unfunded; no service-side action is needed.
//
// minAtomsOut is the MinAtomsOut of the IntentsPreview the caller
// displayed, echoed back as the accept bound: a committed quote whose own
// guaranteed minimum (its minAmountOut, the least a solver may deliver)
// falls below it is rejected before any funds move, and the caller should
// obtain a fresh preview. It is required; a swap cannot be executed
// against terms the user never saw.
func (c *Core) IntentsExecute(pw []byte, originAssetID, destAssetID uint32, atoms uint64, slippageBps int, minAtomsOut uint64) (*IntentsSwap, error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return nil, err
	}
	if atoms == 0 {
		return nil, fmt.Errorf("cannot swap zero %s", unbip(originAssetID))
	}
	if minAtomsOut == 0 {
		return nil, fmt.Errorf("no previewed minimum output supplied")
	}
	var crypter encrypt.Crypter
	if len(pw) > 0 {
		var err error
		crypter, err = c.encryptionKey(pw)
		if err != nil {
			return nil, fmt.Errorf("password error: %w", err)
		}
		defer crypter.Close()
	}

	ctx := c.ctx
	origin, dest, originWallet, destWallet, err := c.intentsPair(ctx, eng, originAssetID, destAssetID)
	if err != nil {
		return nil, err
	}
	if err := c.connectAndUnlock(crypter, originWallet); err != nil {
		return nil, err
	}
	if err := originWallet.checkPeersAndSyncStatus(); err != nil {
		return nil, err
	}

	req, err := c.intentsQuoteRequest(false, origin, dest, originWallet, destWallet, atoms, slippageBps)
	if err != nil {
		return nil, err
	}
	resp, err := eng.client.Quote(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := eng.verifyQuoteAssets(ctx, resp, originAssetID, destAssetID); err != nil {
		return nil, err
	}
	quote := &resp.Quote
	if quote.DepositAddress == "" {
		return nil, fmt.Errorf("committed quote carries no deposit address")
	}
	if quote.DepositMemo != "" {
		// None of the wallet's chains use memo-mode deposits. Refusing here
		// beats sending a deposit that cannot be attributed.
		return nil, fmt.Errorf("memo-mode deposits are not supported")
	}

	envelope, err := resp.Envelope()
	if err != nil {
		return nil, fmt.Errorf("error persisting quote envelope: %w", err)
	}
	atomsOut, err := fromIntentsAmount(quote.AmountOut, &destWallet.Info().UnitInfo, dest.Decimals)
	if err != nil {
		return nil, err
	}
	// The committed quote is a fresh quote, not the previewed one. Honor
	// the terms the user actually accepted: what a solver may actually
	// deliver is bounded by the committed quote's own minAmountOut, so
	// that — not the committed amountOut, which sits a full slippage
	// tolerance above it — must clear the "Min received" floor the user
	// was shown. Reject before anything is persisted or sent; the
	// unfunded quote expires service-side with no consequence.
	minCommittedOut, err := fromIntentsAmount(quote.MinAmountOut, &destWallet.Info().UnitInfo, dest.Decimals)
	if err != nil {
		return nil, fmt.Errorf("bad committed minAmountOut: %w", err)
	}
	if minCommittedOut < minAtomsOut {
		destUnits := &destWallet.Info().UnitInfo
		return nil, fmt.Errorf("the rate moved: the committed quote guarantees only %s, below the quoted minimum of %s; get a fresh quote",
			destUnits.FormatAtoms(minCommittedOut), destUnits.FormatAtoms(minAtomsOut))
	}
	deadline, err := time.Parse(time.RFC3339, quote.Deadline)
	if err != nil {
		eng.log.Warnf("Unparseable quote deadline %q: %v", quote.Deadline, err)
		deadline = time.Now().Add(intentsDeadline(originAssetID))
	}

	// Resolve the deposit fee rate before persisting anything, so a
	// fee-rate failure leaves no orphaned QUOTED record. Only UTXO wallets
	// price a transaction to a specific rate; account-based wallets compute
	// their own fees, so the rate is zeroed. A zero live estimate for a
	// UTXO wallet means the send must not proceed with a made-up rate, and
	// the rate is bounded like a plain send (ceiling, live-rate band, and
	// mainnet USD cap) so a poisoned fee source cannot broadcast a runaway
	// fee the user never sees.
	var feeRate uint64
	if originWallet.Info().BlockchainClass == asset.BlockchainClassUTXO {
		if feeRate = originWallet.usableFeeRate(); feeRate == 0 {
			return nil, fmt.Errorf("no live fee-rate estimate for %s deposit", unbip(originAssetID))
		}
		if err := c.validateSendFeeRate(originWallet, quote.DepositAddress, atoms, feeRate, false); err != nil {
			return nil, fmt.Errorf("deposit fee rate rejected: %w", err)
		}
	}

	swap := &IntentsSwap{
		DepositAddress: quote.DepositAddress,
		State:          IntentsStateQuoted,
		OriginAssetID:  originAssetID,
		DestAssetID:    destAssetID,
		OriginAsset:    origin.AssetID,
		DestAsset:      dest.AssetID,
		AtomsIn:        atoms,
		AmountIn:       resp.Request.Amount,
		AmountOut:      quote.AmountOut,
		AtomsOut:       atomsOut,
		Recipient:      req.Recipient,
		RefundTo:       req.RefundTo,
		Deadline:       deadline,
		TimeEstimate:   quote.TimeEstimate,
		AppFees:        resp.Request.AppFees,
		QuoteEnvelope:  envelope,
		CreatedAt:      time.Now(),
	}
	if err := eng.db.StoreIntentsSwap(swap); err != nil {
		return nil, fmt.Errorf("error persisting swap record: %w", err)
	}

	coin, err := originWallet.Wallet.Send(quote.DepositAddress, atoms, feeRate)
	if err != nil {
		// The wallet recorded no send, so the record stays QUOTED and
		// expires unfunded; poke the poller so its deadline timeout runs
		// even when the ticker was parked with no other active swap. The
		// message claims only that — a Send contract that errored after
		// broadcasting would still be observed by the service through the
		// deposit address, and in any case pays the user's own recipient.
		eng.poke()
		return nil, fmt.Errorf("deposit send failed (no deposit was recorded, the quote will expire): %w", err)
	}

	// Record the deposit under swapMtx, reloading so a concurrent poll that
	// already advanced the state is not regressed to DEPOSIT_SENT.
	eng.swapMtx.Lock()
	fresh, err := eng.db.IntentsSwap(swap.DepositAddress)
	if err == nil {
		if fresh.State == IntentsStateQuoted {
			fresh.State = IntentsStateDepositSent
		}
		if fresh.DepositTxID == "" {
			fresh.DepositTxID = coin.TxID()
		}
		if err := eng.db.StoreIntentsSwap(fresh); err != nil {
			eng.log.Errorf("Error updating swap record %s after deposit: %v", swap.DepositAddress, err)
		}
		swap = fresh
	} else {
		// The reload failed, so a concurrent writer's state cannot be
		// honored — but the deposit was broadcast and must be recorded.
		// The local record still holds every field (including CreatedAt);
		// persist it as DEPOSIT_SENT with the txid. Without this the record
		// stays QUOTED with no txid, and a later timeout would report "no
		// funds moved" for a deposit that did move.
		eng.log.Errorf("Error reloading swap record %s after deposit: %v", swap.DepositAddress, err)
		swap.State = IntentsStateDepositSent
		swap.DepositTxID = coin.TxID()
		if err := eng.db.StoreIntentsSwap(swap); err != nil {
			eng.log.Errorf("Error storing swap record %s after deposit: %v", swap.DepositAddress, err)
		}
	}
	eng.swapMtx.Unlock()
	if err := eng.client.SubmitDeposit(ctx, swap.DepositAddress, swap.DepositTxID); err != nil {
		eng.log.Warnf("Deposit hint submission failed (service will detect on-chain): %v", err)
	}

	c.notify(newIntentsSwapNote(swap, "Swap deposit sent",
		fmt.Sprintf("Swapping %s for %s. Deposit %s broadcast.",
			strings.ToUpper(unbip(originAssetID)), strings.ToUpper(unbip(destAssetID)), swap.DepositTxID),
		db.Poke))
	c.updateAssetBalance(originAssetID)
	eng.poke()

	return swap, nil
}

// IntentsSwaps returns all persisted swap records, newest first.
func (c *Core) IntentsSwaps() ([]*IntentsSwap, error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return nil, err
	}
	return eng.db.IntentsSwaps()
}

// IntentsRefresh polls the service for one swap immediately.
func (c *Core) IntentsRefresh(ctx context.Context, depositAddress string) (*IntentsSwap, error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return nil, err
	}
	swap, err := eng.db.IntentsSwap(depositAddress)
	if err != nil {
		return nil, fmt.Errorf("no swap record for %s: %w", depositAddress, err)
	}
	if !swap.Terminal() {
		if err := eng.refreshSwap(ctx, swap); err != nil {
			return nil, err
		}
		// refreshSwap persists any status change to its own reloaded copy;
		// re-read so the caller receives the refreshed record rather than
		// the pre-refresh one.
		if swap, err = eng.db.IntentsSwap(depositAddress); err != nil {
			return nil, fmt.Errorf("error reloading swap record for %s: %w", depositAddress, err)
		}
	}
	return swap, nil
}

// intentsReconstructProbeLimit caps, per wallet, how many recent outgoing
// transactions are probed against the service during reconstruction.
const intentsReconstructProbeLimit = 25

// storeReconstructed persists a rebuilt swap record only when no record for
// its deposit address already exists, and reports whether it stored one.
// Reconstruction probes against a snapshot of known addresses taken at the
// start of the run, so a swap created live in between could otherwise be
// clobbered here — its signature-verified quote envelope replaced by the
// non-verifiable status-endpoint copy, its CreatedAt reset, and a
// concurrent state update regressed. A live record is authoritative over a
// rebuilt one, so the existence check runs under swapMtx at store time.
func (e *intentsEngine) storeReconstructed(swap *IntentsSwap) (stored bool, err error) {
	e.swapMtx.Lock()
	defer e.swapMtx.Unlock()
	switch _, err := e.db.IntentsSwap(swap.DepositAddress); {
	case err == nil:
		return false, nil // already tracked; leave the live record intact
	case !errors.Is(err, db.ErrIntentsSwapNotFound):
		return false, err
	}
	if err := e.db.StoreIntentsSwap(swap); err != nil {
		return false, err
	}
	return true, nil
}

// IntentsReconstruct rebuilds swap records lost with local state (e.g.
// after a seed restore) by probing the service's status endpoint with the
// recipients of recent outgoing transactions. Deposit addresses are
// unique per swap, so a hit identifies the swap unambiguously. Records
// rebuilt this way carry the service's re-serialized quote, which is not
// signature-verifiable, and are flagged Reconstructed.
func (c *Core) IntentsReconstruct(ctx context.Context) (found int, err error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return 0, err
	}

	cat, err := eng.freshCatalog(ctx)
	if err != nil {
		return 0, err
	}
	resolveChain := func(assetID string) (uint32, *intents.Token, bool) {
		chainID, native, ok := cat.AssetChain(assetID)
		if !ok {
			return 0, nil, false
		}
		tok := cat.ByAssetID(assetID)
		if native {
			return chainID, tok, true
		}
		// A token side maps to the registered wallet token with the same
		// contract on that chain, when one exists.
		if ra := asset.Asset(chainID); ra != nil {
			for tokenID, rtok := range ra.Tokens {
				if strings.EqualFold(rtok.ContractAddress, tok.ContractAddress) {
					return tokenID, tok, true
				}
			}
		}
		return 0, nil, false
	}

	known := make(map[string]bool)
	swaps, err := eng.db.IntentsSwaps()
	if err != nil {
		return 0, err
	}
	for _, s := range swaps {
		known[s.DepositAddress] = true
	}

	for _, w := range c.xcWallets() {
		if !w.connected() {
			continue
		}
		resp, err := w.TxHistory(&txdb.TxHistoryRequest{N: intentsReconstructProbeLimit})
		if err != nil {
			eng.log.Warnf("Error reading %s tx history: %v", unbip(w.AssetID), err)
			continue
		}
		for _, tx := range resp.Txs {
			if ctx.Err() != nil {
				return found, ctx.Err()
			}
			if tx.Type != txdb.Send || tx.Recipient == nil || known[*tx.Recipient] {
				continue
			}
			known[*tx.Recipient] = true // probe each address once

			status, err := eng.client.Status(ctx, *tx.Recipient)
			if errors.Is(err, intents.ErrSwapNotFound) {
				continue
			}
			if err != nil {
				eng.log.Warnf("Status probe failed for %s: %v", *tx.Recipient, err)
				continue
			}
			swap, err := swapFromStatus(resolveChain, w.AssetID, *tx.Recipient, tx.ID, status)
			if err != nil {
				eng.log.Warnf("Error reconstructing swap %s: %v", *tx.Recipient, err)
				continue
			}
			stored, err := eng.storeReconstructed(swap)
			if err != nil {
				return found, err
			}
			if !stored {
				continue // a live record for this address appeared mid-run
			}
			found++
			c.notify(newIntentsSwapNote(swap, "Swap record recovered",
				fmt.Sprintf("Recovered a swap to deposit address %s.", swap.DepositAddress), db.Poke))

			// Be polite to the open endpoint.
			select {
			case <-time.After(250 * time.Millisecond):
			case <-ctx.Done():
				return found, ctx.Err()
			}
		}
	}
	if found > 0 {
		eng.poke()
	}
	return found, nil
}

// intentsAssetUnknown marks a swap side whose chain could not be resolved
// from the catalog (e.g. a since-delisted asset in a reconstructed
// record). It is deliberately not a valid BIP-44 ID, so it is never
// confused with a real chain such as BTC (0).
const intentsAssetUnknown = ^uint32(0)

// intentsAssetLabel returns a display label for a swap-side asset,
// preferring the wallet symbol and falling back to the Intents asset ID
// string when the chain is unknown (some reconstructed records).
func intentsAssetLabel(assetID uint32, intentsAssetID string) string {
	if assetID == intentsAssetUnknown {
		return intentsAssetID
	}
	return strings.ToUpper(unbip(assetID))
}

// intentsChainResolver maps an Intents asset ID to its wallet chain asset
// ID and the catalog token (for its decimals). Implemented over the
// catalog by IntentsReconstruct.
type intentsChainResolver func(intentsAssetID string) (uint32, *intents.Token, bool)

// swapFromStatus builds a Reconstructed swap record from a status
// response. The destination chain is resolved through the catalog
// (resolveChain); an unresolved destination is recorded as
// intentsAssetUnknown rather than 0, which would be misread as BTC.
func swapFromStatus(resolveChain intentsChainResolver, originAssetID uint32, depositAddress, depositTxID string, status *intents.StatusResponse) (*IntentsSwap, error) {
	if status.QuoteResponse == nil {
		return nil, fmt.Errorf("status carries no quote")
	}
	echo := &status.QuoteResponse.Request
	quote := &status.QuoteResponse.Quote

	destAssetID := intentsAssetUnknown
	var destTok *intents.Token
	if id, tok, ok := resolveChain(echo.DestinationAsset); ok {
		destAssetID, destTok = id, tok
	}
	// The origin chain is already known (it's the wallet whose history was
	// probed); the catalog lookup is only for the token's decimals.
	_, originTok, _ := resolveChain(echo.OriginAsset)

	// Reconstruction is best-effort: an unresolvable side or a bad amount
	// string leaves the atoms field zero rather than dropping the record.
	atoms := func(assetID uint32, tok *intents.Token, amt string) uint64 {
		if assetID == intentsAssetUnknown || tok == nil || amt == "" {
			return 0
		}
		unitInfo, err := asset.UnitInfo(assetID)
		if err != nil {
			return 0
		}
		v, err := fromIntentsAmount(amt, &unitInfo, tok.Decimals)
		if err != nil {
			return 0
		}
		return v
	}

	// Prefer the realized fill over the quoted output, as mergeStatus does
	// for live records: history shows what was actually delivered. A
	// reconstructed record's AmountOut already matching the details means a
	// later poll's realizedAtomsOut will never revisit it, so it must be
	// converted here.
	amountOut := quote.AmountOut
	if status.SwapDetails.AmountOut != "" {
		amountOut = status.SwapDetails.AmountOut
	}

	// The status endpoint's re-serialized quote is the copy least likely
	// to carry a well-formed deadline. A parse failure must not produce a
	// zero Deadline: applyTimeout would read it as long past and
	// immediately fail the swap, terminally. Mirror IntentsExecute's
	// fallback instead.
	deadline, err := time.Parse(time.RFC3339, quote.Deadline)
	if err != nil {
		deadline = time.Now().Add(intentsDeadline(originAssetID))
	}
	state := status.Status
	if state == intents.StatusPendingDeposit {
		state = IntentsStateDepositSent
	}
	envelope, err := status.QuoteResponse.Envelope()
	if err != nil {
		envelope = nil
	}

	return &IntentsSwap{
		DepositAddress: depositAddress,
		DepositMemo:    quote.DepositMemo,
		State:          state,
		OriginAssetID:  originAssetID,
		DestAssetID:    destAssetID,
		OriginAsset:    echo.OriginAsset,
		DestAsset:      echo.DestinationAsset,
		AtomsIn:        atoms(originAssetID, originTok, echo.Amount),
		AmountIn:       echo.Amount,
		AmountOut:      amountOut,
		AtomsOut:       atoms(destAssetID, destTok, amountOut),
		Recipient:      echo.Recipient,
		RefundTo:       echo.RefundTo,
		Deadline:       deadline,
		TimeEstimate:   quote.TimeEstimate,
		AppFees:        echo.AppFees,
		DepositTxID:    depositTxID,
		NearTxHashes:   status.SwapDetails.NearTxHashes,
		DestinationTxs: txHashes(status.SwapDetails.DestinationChainTxs),
		RefundedAmount: status.SwapDetails.RefundedAmount,
		RefundReason:   status.SwapDetails.RefundReason,
		QuoteEnvelope:  envelope,
		Reconstructed:  true,
		CreatedAt:      time.Now(),
	}, nil
}
