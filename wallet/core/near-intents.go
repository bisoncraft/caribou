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
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	IntentsStateQuoted        = db.IntentsStateQuoted
	IntentsStateDepositSent   = db.IntentsStateDepositSent
	IntentsStateExpired       = db.IntentsStateExpired
	IntentsStateDepositFailed = db.IntentsStateDepositFailed
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

	// reconstructing single-flights IntentsReconstruct: its probe loop
	// fires a request per recent recipient at the open status endpoint,
	// and concurrent runs would multiply the burst.
	reconstructing atomic.Bool

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
		proxied := ""
		if c.cfg.TorProxy != "" {
			proxied = " (proxied)"
		}
		logger.Infof("Cross-chain swap engine started: 1Click service at %s%s", client.BaseURL(), proxied)
		if fees := intentsAppFees(); len(fees) > 0 {
			logger.Debugf("Quotes carry a %d bps application fee to %s", fees[0].Fee, fees[0].Recipient)
		}
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
	if active, loaded := e.pollActive(ctx); loaded {
		if active == 0 {
			ticker.Stop()
			e.log.Debugf("No active swaps; poller parked")
		} else {
			e.log.Infof("Resumed polling for %d active swap(s)", active)
		}
	}
	for {
		select {
		case <-ticker.C:
			if active, loaded := e.pollActive(ctx); loaded && active == 0 {
				ticker.Stop()
				e.log.Debugf("No active swaps remain; poller parked")
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
			e.log.Meter("refresh-err:"+swap.DepositAddress, 2*time.Minute).
				Errorf("Error refreshing swap %s: %v", swap.DepositAddress, err)
		}
	}
	return active, true
}

// captureDepositFee copies the origin wallet's recorded network fee for
// the deposit transaction onto the swap record. Wallets that estimate fees
// at send time (EVM) refine the recorded fee with the receipt, so the fee
// is re-read on every poll while the swap is active; the record is written
// (and a note emitted, so a live UI refreshes) only when the value
// changes. See db.IntentsSwap.DepositTxFee for the denomination.
func (e *intentsEngine) captureDepositFee(ctx context.Context, swap *IntentsSwap) {
	if swap.DepositTxID == "" || swap.OriginAssetID == intentsAssetUnknown {
		return
	}
	w, found := e.c.wallet(swap.OriginAssetID)
	if !found {
		return
	}
	tx, err := w.WalletTransaction(ctx, swap.DepositTxID)
	if err != nil {
		e.log.Meter("fee-lookup:"+swap.DepositAddress, 2*time.Minute).
			Debugf("Fee lookup for deposit tx %s of swap %s failed: %v",
				swap.DepositTxID, swap.DepositAddress, err)
		return
	}
	if tx == nil || tx.Fees == 0 {
		return
	}
	// Fees are paid in the origin chain's native asset, so the USD value
	// prices the parent chain. A fee change reprices; an unchanged fee only
	// backfills a USD value that a price outage left uncaptured — it is
	// never repriced, so the record keeps the rate near the fee event.
	feeUSD := e.usdValue(ctx, intentsChainID(swap.OriginAssetID), tx.Fees)
	if tx.Fees == swap.DepositTxFee && (swap.USDDepositFee != 0 || feeUSD == 0) {
		return
	}

	e.swapMtx.Lock()
	fresh, err := e.db.IntentsSwap(swap.DepositAddress)
	if err != nil {
		e.swapMtx.Unlock()
		return
	}
	switch {
	case fresh.DepositTxFee != tx.Fees:
		fresh.DepositTxFee = tx.Fees
		fresh.USDDepositFee = feeUSD
	case fresh.USDDepositFee == 0 && feeUSD != 0:
		fresh.USDDepositFee = feeUSD
	default:
		e.swapMtx.Unlock()
		return
	}
	err = e.db.StoreIntentsSwap(fresh)
	e.swapMtx.Unlock()
	if err != nil {
		e.log.Errorf("Error storing deposit fee for swap %s: %v", swap.DepositAddress, err)
		return
	}
	e.log.Debugf("Deposit tx fee for swap %s: %d atoms ($%.2f)", swap.DepositAddress, tx.Fees, feeUSD)
	e.c.notify(newIntentsSwapNote(fresh, "Swap update", "Deposit fee recorded.", db.Data))
}

// refreshSwap polls the service for one swap (the network call is made
// without holding swapMtx) and applies the result under the lock.
func (e *intentsEngine) refreshSwap(ctx context.Context, swap *IntentsSwap) error {
	e.captureDepositFee(ctx, swap)
	status, err := e.client.Status(ctx, swap.DepositAddress)
	if err != nil {
		if errors.Is(err, intents.ErrSwapNotFound) {
			// Not necessarily fatal: the service can lag a fresh quote. Only
			// a swap past its deadline is timed out.
			e.log.Meter("swap-unknown:"+swap.DepositAddress, 2*time.Minute).
				Debugf("Service does not recognize swap %s yet", swap.DepositAddress)
			if !e.applyDepositRejection(ctx, swap) {
				e.applyTimeout(swap.DepositAddress)
			}
			return nil
		}
		return err
	}

	if status.Status == intents.StatusPendingDeposit {
		// The service is still waiting; keep the local view (QUOTED or
		// DEPOSIT_SENT) unless the deadline has passed. A deposit the origin
		// chain rejected will never be observed — fail fast instead.
		if !e.applyDepositRejection(ctx, swap) {
			e.applyTimeout(swap.DepositAddress)
		}
		return nil
	}

	realizedAtoms, haveRealized := e.realizedAtomsOut(ctx, swap, status)
	// Convert and price a refund at capture time (the catalog can hit the
	// network, so this happens outside swapMtx, like the realized-fill
	// conversion).
	var refundedAtoms uint64
	var refundedUSD float64
	if status.Status == intents.StatusRefunded {
		refundedAtoms = e.intentsAtoms(ctx, swap.OriginAssetID, swap.OriginAsset, status.SwapDetails.RefundedAmount)
		refundedUSD = e.usdValue(ctx, swap.OriginAssetID, refundedAtoms)
	}
	updated, note := e.mergeStatus(swap.DepositAddress, status, realizedAtoms, haveRealized, refundedAtoms, refundedUSD)
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
		e.recordIncomingTxs(updated)
	} else {
		// Still non-terminal: run the deadline check every cycle, not only
		// on the PENDING_DEPOSIT branch above. The service can park a swap
		// in INCOMPLETE_DEPOSIT (underfunded deposit) with no service-side
		// timeout commitment, and without this the record would be polled
		// forever. States the service is actively driving (KNOWN_DEPOSIT_TX,
		// PROCESSING) are deliberately not in applyTimeout's switch, so the
		// call is a no-op for them.
		e.applyTimeout(swap.DepositAddress)
	}
	return nil
}

// recordIncomingTxs hands the service-reported payout (or refund) txids to
// the receiving wallet's transaction history when that wallet cannot
// observe incoming transfers on its own (asset.IncomingTxRecorder — the
// EVM wallets). Without this the credit updates the balance but never
// appears in the wallet's history. The service-reported amount rides
// along as the wallet's attribution fallback for payouts it cannot read
// from the chain (a contract-internal native transfer from an
// unrecognized bridge). Recording is idempotent wallet-side, so a
// repeated terminal update (e.g. a realized-fill refinement) is harmless.
func (e *intentsEngine) recordIncomingTxs(swap *IntentsSwap) {
	assetID, txids, amtHint := swap.DestAssetID, swap.DestinationTxs, swap.AtomsOut
	if swap.State == intents.StatusRefunded {
		assetID, txids, amtHint = swap.OriginAssetID, swap.RefundTxs, swap.RefundedAtoms
	} else if swap.State != intents.StatusSuccess {
		return
	}
	if len(txids) == 0 || assetID == intentsAssetUnknown {
		return
	}
	// A single reported txid carries the full amount; across several the
	// split is unknowable, so chain-side attribution alone decides.
	if len(txids) != 1 {
		amtHint = 0
	}
	wallet, found := e.c.wallet(assetID)
	if !found {
		return
	}
	rec, ok := wallet.Wallet.(asset.IncomingTxRecorder)
	if !ok {
		return // the wallet observes incoming transfers on its own
	}
	e.log.Debugf("Recording %d incoming %s tx(s) from swap %s (amount hint %d atoms)",
		len(txids), unbip(assetID), swap.DepositAddress, amtHint)
	// The payout is not always queryable the instant the service reports
	// it: the mock reports success as it hands the send to the node, and
	// a mainnet RPC provider can lag the service's view. Since a terminal
	// swap is never polled again, a one-shot attempt would drop the
	// record on any transient miss — so recording retries briefly in the
	// background, on the engine's context (the caller's may be an HTTP
	// request's).
	// beginTask, not a bare wg.Add: this is reachable from an HTTP-handler
	// goroutine (IntentsRefresh) that Run does not wait on, so an unguarded
	// Add can race the shutdown Wait.
	if !e.c.beginTask() {
		return
	}
	go func() {
		defer e.c.endTask()
		for _, txid := range txids {
			e.recordIncomingTxWithRetry(rec, assetID, txid, amtHint, swap.DepositAddress)
		}
	}()
}

// Retry schedule for recording an incoming tx the wallet's node cannot
// find yet.
const (
	incomingTxRetryDelay = 3 * time.Second
	incomingTxRetries    = 10
)

// recordIncomingTxWithRetry records one incoming tx, retrying not-found
// responses on the assumption that the just-reported payout has not yet
// propagated to the wallet's node. Any other error, or exhaustion, logs a
// warning and gives up.
func (e *intentsEngine) recordIncomingTxWithRetry(rec asset.IncomingTxRecorder, assetID uint32, txid string, amtHint uint64, depositAddr string) {
	for i := 0; ; i++ {
		err := rec.RecordIncomingTx(e.ctx, txid, amtHint)
		if err == nil {
			e.log.Debugf("Recorded incoming %s tx %s from swap %s (attempt %d)",
				unbip(assetID), txid, depositAddr, i+1)
			return
		}
		if !errors.Is(err, asset.CoinNotFoundError) || i >= incomingTxRetries {
			e.log.Warnf("Error recording incoming %s tx %s from swap %s: %v",
				unbip(assetID), txid, depositAddr, err)
			return
		}
		select {
		case <-e.ctx.Done():
			return
		case <-time.After(incomingTxRetryDelay):
		}
	}
}

// parseUSD parses a service-reported USD amount string, returning 0 for
// absent, malformed, or non-finite values.
func parseUSD(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}
	return v
}

// intentsAtoms converts a base-unit amount of an Intents asset to wallet
// atoms of the given wallet asset, or 0 if it cannot be resolved. Called
// outside swapMtx: the catalog fetch can hit the network.
func (e *intentsEngine) intentsAtoms(ctx context.Context, walletAssetID uint32, intentsAssetID, amount string) uint64 {
	if amount == "" || walletAssetID == intentsAssetUnknown {
		return 0
	}
	unitInfo, err := asset.UnitInfo(walletAssetID)
	if err != nil {
		return 0
	}
	cat, err := e.freshCatalog(ctx)
	if err != nil {
		return 0
	}
	tok := cat.ByAssetID(intentsAssetID)
	if tok == nil {
		return 0
	}
	atoms, err := fromIntentsAmount(amount, &unitInfo, tok.Decimals)
	if err != nil {
		return 0
	}
	return atoms
}

// usdValue prices wallet atoms of an asset in USD from the catalog's spot
// price, returning 0 when the asset, its unit info, or a price cannot be
// resolved. Called outside swapMtx: the catalog fetch can hit the network.
func (e *intentsEngine) usdValue(ctx context.Context, assetID uint32, atoms uint64) float64 {
	if atoms == 0 || assetID == intentsAssetUnknown {
		return 0
	}
	unitInfo, err := asset.UnitInfo(assetID)
	if err != nil {
		return 0
	}
	cat, err := e.freshCatalog(ctx)
	if err != nil {
		return 0
	}
	tok := resolveIntentsAsset(cat, assetID)
	if tok == nil || tok.Price <= 0 {
		return 0
	}
	return float64(atoms) / float64(unitInfo.Conventional.ConversionFactor) * tok.Price
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
// refundedAtoms and refundedUSD are the refunded amount converted to origin
// wallet atoms and priced at capture time, applied on a transition to
// REFUNDED (all computed by the caller, outside the lock).
func (e *intentsEngine) mergeStatus(depositAddress string, status *intents.StatusResponse, realizedAtoms uint64, haveRealized bool, refundedAtoms uint64, refundedUSD float64) (*IntentsSwap, Notification) {
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

	prevState := swap.State
	swap.State = newState
	if realized {
		swap.AmountOut = details.AmountOut
		swap.AtomsOut = realizedAtoms
	}
	swap.NearTxHashes = details.NearTxHashes
	swap.DestinationTxs = txHashes(details.DestinationChainTxs)
	swap.RefundedAmount = details.RefundedAmount
	swap.RefundReason = details.RefundReason
	// The service's own execution-time pricing supersedes the quote-time
	// values captured at execute.
	if usd := parseUSD(details.AmountInUsd); usd != 0 {
		swap.USDIn = usd
	}
	if usd := parseUSD(details.AmountOutUsd); usd != 0 {
		swap.USDOut = usd
	}
	// The service's own refund pricing is preferred; the catalog-derived
	// value computed by the caller covers its absence.
	if newState == intents.StatusRefunded {
		swap.RefundTxs = refundTxHashes(details.OriginChainTxs, swap.DepositTxID)
		swap.RefundedAtoms = refundedAtoms
		if usd := parseUSD(details.RefundedAmountUsd); usd != 0 {
			swap.USDRefunded = usd
		} else if refundedUSD != 0 {
			swap.USDRefunded = refundedUSD
		}
	}
	if err := e.db.StoreIntentsSwap(swap); err != nil {
		e.log.Errorf("Error storing swap %s: %v", depositAddress, err)
		return nil, nil
	}
	if newState != prevState {
		e.log.Infof("Swap %s (%s → %s): %s → %s", depositAddress,
			intentsAssetLabel(swap.OriginAssetID, swap.OriginAsset),
			intentsAssetLabel(swap.DestAssetID, swap.DestAsset),
			prevState, newState)
	} else {
		e.log.Debugf("Swap %s details updated in state %s (%d NEAR txs, %d destination txs)",
			depositAddress, newState, len(swap.NearTxHashes), len(swap.DestinationTxs))
	}
	if realized {
		e.log.Infof("Swap %s realized output: %d atoms (service-reported %s)",
			depositAddress, realizedAtoms, details.AmountOut)
	}
	if newState == intents.StatusRefunded && swap.RefundReason != "" {
		e.log.Infof("Swap %s refund reason: %s", depositAddress, swap.RefundReason)
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

// applyDepositRejection fails a DEPOSIT_SENT swap whose deposit transaction
// was mined but rejected by the origin chain. The service will never observe
// a deposit, so waiting out the quote deadline only hides the failure; the
// deposited funds never left the wallet, though the network fee was spent.
// The on-chain check is made without holding swapMtx; the record is reloaded
// under the lock before the transition. Returns true when the swap was
// transitioned, making the deadline timeout check moot.
func (e *intentsEngine) applyDepositRejection(ctx context.Context, swap *IntentsSwap) bool {
	if swap.State != IntentsStateDepositSent || swap.DepositTxID == "" {
		return false
	}
	w, found := e.c.wallet(swap.OriginAssetID)
	if !found {
		return false
	}
	wt, err := w.WalletTransaction(ctx, swap.DepositTxID)
	if err != nil || wt == nil || !wt.Rejected {
		return false
	}

	e.swapMtx.Lock()
	fresh, err := e.db.IntentsSwap(swap.DepositAddress)
	if err != nil || fresh.State != IntentsStateDepositSent {
		e.swapMtx.Unlock()
		return false
	}
	fresh.State = IntentsStateDepositFailed
	fresh.RefundReason = "the deposit transaction was rejected by the origin chain; the deposited funds never left the wallet"
	var note Notification
	if err := e.db.StoreIntentsSwap(fresh); err == nil {
		e.log.Warnf("Swap %s marked DEPOSIT_FAILED: deposit tx %s was rejected on-chain",
			fresh.DepositAddress, fresh.DepositTxID)
		note = newIntentsSwapNote(fresh, "Swap deposit failed",
			"The deposit transaction was rejected on-chain. The deposited funds never left the wallet, though the network fee was spent.", db.ErrorLevel)
	} else {
		e.log.Errorf("Error storing deposit-failed swap %s: %v", fresh.DepositAddress, err)
	}
	e.swapMtx.Unlock()

	if note != nil {
		e.c.notify(note)
	}
	// The wallet showed the deposit as a pending deduction; refresh so the
	// balance reflects that the funds never left.
	e.c.updateAssetBalance(swap.OriginAssetID)
	return true
}

// applyTimeout moves a swap that never progressed to a terminal state once
// its deadline (plus grace) has passed, so a swap the service never
// advances is not polled forever. A quote that was never funded becomes
// EXPIRED (no funds moved). A deposit the service never observed by the
// deadline becomes FAILED — the deadline is also the deposit window, so a
// late deposit will not be accepted for this quote; because a deposit
// transaction may have left the wallet, the message points to escalation
// rather than claiming no funds moved. An INCOMPLETE_DEPOSIT the service
// has not refunded by the deadline (its documented behavior for
// underfunded deposits) also becomes FAILED with an escalation message.
// KNOWN_DEPOSIT_TX and PROCESSING are deliberately absent from the
// switch: the service holds the deposit and is actively driving those to
// SUCCESS or REFUNDED, and a local FAILED would misreport funds in
// flight. The record is reloaded under swapMtx so a concurrent writer's
// newer state is not overwritten.
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
			e.log.Infof("Swap %s expired: quote deadline %s passed with no deposit sent",
				depositAddress, swap.Deadline.Format(time.RFC3339))
			note = newIntentsSwapNote(swap, "Swap expired",
				"The quote expired with no deposit recorded.", db.WarningLevel)
		} else {
			e.log.Errorf("Error storing expired swap %s: %v", depositAddress, err)
		}
	case IntentsStateDepositSent:
		swap.State = intents.StatusFailed
		swap.RefundReason = "the service did not observe a deposit before the quote deadline"
		if err := e.db.StoreIntentsSwap(swap); err == nil {
			e.log.Warnf("Swap %s marked FAILED: deposit %s not observed by the service before deadline %s",
				depositAddress, swap.DepositTxID, swap.Deadline.Format(time.RFC3339))
			note = newIntentsSwapNote(swap, "Swap needs attention",
				"The service did not observe the deposit before the deadline. If a deposit left your wallet, check its transaction and the service status page.", db.ErrorLevel)
		} else {
			e.log.Errorf("Error storing timed-out swap %s: %v", depositAddress, err)
		}
	case intents.StatusIncompleteDeposit:
		swap.State = intents.StatusFailed
		swap.RefundReason = "the service reported the deposit incomplete and did not refund it before the quote deadline"
		if err := e.db.StoreIntentsSwap(swap); err == nil {
			e.log.Warnf("Swap %s marked FAILED: incomplete deposit not refunded by the service before deadline %s",
				depositAddress, swap.Deadline.Format(time.RFC3339))
			note = newIntentsSwapNote(swap, "Swap needs attention",
				"The service reported the deposit as incomplete and did not refund it before the deadline. Check the swap on the service status page.", db.ErrorLevel)
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

// refundTxHashes extracts the refund transactions from a refunded swap's
// origin-chain hashes, which include the user's own deposit transaction
// alongside the refund. The deposit does not belong on the refund list: it
// is already tracked on the record, it would zero the single-txid amount
// hint for incoming-tx recording, and it would render a second time in the
// UI labeled as a refund.
func refundTxHashes(refs []intents.TxHashRef, depositTxID string) []string {
	hashes := make([]string, 0, len(refs))
	for _, r := range refs {
		if depositTxID != "" && strings.EqualFold(r.Hash, depositTxID) {
			continue
		}
		hashes = append(hashes, r.Hash)
	}
	if len(hashes) == 0 {
		return nil
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
		// beginTask, like recordIncomingTxs: shutdown must be able to wait
		// on the fetch, and during shutdown no refresh starts — the stale
		// copy serves.
		if !e.catalogFetch && e.c.beginTask() {
			e.catalogFetch = true
			e.log.Debugf("Asset catalog stale; serving cached copy and refreshing in the background")
			go func() {
				defer e.c.endTask()
				e.refreshCatalog()
			}()
		}
		e.catMtx.Unlock()
		return cached, nil
	}
	e.catMtx.Unlock()

	// Cold cache: block on the first fetch.
	e.log.Debugf("Fetching asset catalog (no cached copy)")
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
	return fromIntentsAmountRound(amount, unitInfo, decimals, false)
}

// fromIntentsAmountCeil converts like fromIntentsAmount but rounds up when
// the Intents scale is finer than the wallet's atomic unit. Deposit sizing
// uses it: the service's amountIn is not quantized to wallet atoms, so a
// truncated conversion can come up a wallet atom short of the quote and the
// deposit is never recognized as complete.
func fromIntentsAmountCeil(amount string, unitInfo *util.UnitInfo, decimals int) (uint64, error) {
	return fromIntentsAmountRound(amount, unitInfo, decimals, true)
}

func fromIntentsAmountRound(amount string, unitInfo *util.UnitInfo, decimals int, roundUp bool) (uint64, error) {
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
		den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(diff)), nil)
		if roundUp {
			v.Add(v, new(big.Int).Sub(den, big.NewInt(1)))
		}
		v.Quo(v, den)
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
// value path terminates at recoverable addresses. atoms is denominated in
// the origin wallet's atomic unit for an EXACT_INPUT quote and the
// destination wallet's for EXACT_OUTPUT, matching the service's amount
// semantics for each swap type.
func (c *Core) intentsQuoteRequest(dry, exactOutput bool, origin, dest *intents.Token, originWallet, destWallet *xcWallet, atoms uint64, slippageBps int) (*intents.QuoteRequest, error) {
	swapType := intents.SwapTypeExactInput
	amountWallet, amountToken := originWallet, origin
	if exactOutput {
		swapType = intents.SwapTypeExactOutput
		amountWallet, amountToken = destWallet, dest
	}
	amount, err := toIntentsAmount(atoms, &amountWallet.Info().UnitInfo, amountToken.Decimals)
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
		SwapType:          swapType,
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

// IntentsPreview obtains a dry quote for a prospective swap. atoms is the
// origin amount for an exact-input preview, the desired destination amount
// for exact-output.
func (c *Core) IntentsPreview(ctx context.Context, originAssetID, destAssetID uint32, atoms uint64, slippageBps int, exactOutput bool) (*IntentsPreview, error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return nil, err
	}
	origin, dest, originWallet, destWallet, err := c.intentsPair(ctx, eng, originAssetID, destAssetID)
	if err != nil {
		return nil, err
	}
	req, err := c.intentsQuoteRequest(true, exactOutput, origin, dest, originWallet, destWallet, atoms, slippageBps)
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
	// The quoted input, not the request echo: for an exact-output quote the
	// service computes it, and for exact-input it echoes the request. The
	// exact-output conversion rounds up to match the execute-time deposit
	// sizing — this value is the ceiling the user accepts, and a truncated
	// ceiling would reject a committed quote at an unmoved rate.
	convertIn := fromIntentsAmount
	if exactOutput {
		convertIn = fromIntentsAmountCeil
	}
	atomsIn, err := convertIn(resp.Quote.AmountIn, &originWallet.Info().UnitInfo, origin.Decimals)
	if err != nil {
		return nil, err
	}

	eng.log.Debugf("Preview %s → %s: in %d atoms, out %d atoms (min %d), %s USD → %s USD",
		unbip(originAssetID), unbip(destAssetID), atomsIn, atomsOut, minAtomsOut,
		resp.Quote.AmountInUsd, resp.Quote.AmountOutUsd)

	return &IntentsPreview{
		AtomsIn:      atomsIn,
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

// checkQuoteBounds rejects a committed quote whose terms fall outside the
// preview-derived bounds the user accepted: the quote's guaranteed
// minimum output must clear the previewed floor, and for an exact-output
// swap the required deposit must not exceed the previewed ceiling. These
// comparisons are the only check between a moved rate and a swap
// executed against terms the user never saw, so they live in their own
// function for direct unit testing.
func checkQuoteBounds(exactOutput bool, minCommittedOut, minAtomsOut, sendAtoms, maxAtomsIn uint64, originUnits, destUnits *util.UnitInfo) error {
	if minCommittedOut < minAtomsOut {
		return fmt.Errorf("the rate moved: the committed quote guarantees only %s, below the quoted minimum of %s; get a fresh quote",
			destUnits.FormatAtoms(minCommittedOut), destUnits.FormatAtoms(minAtomsOut))
	}
	if exactOutput && sendAtoms > maxAtomsIn {
		return fmt.Errorf("the rate moved: the committed quote requires %s, above the quoted maximum of %s; get a fresh quote",
			originUnits.FormatAtoms(sendAtoms), originUnits.FormatAtoms(maxAtomsIn))
	}
	return nil
}

// IntentsExecute obtains a committed quote and funds it: verifies and
// persists the quote, sends the origin-chain deposit from the origin
// wallet, and hands tracking to the status poller. The returned record is
// in state DEPOSIT_SENT on success. If the deposit send fails the record
// remains QUOTED and expires unfunded; no service-side action is needed.
//
// atoms is the origin amount for an exact-input swap, the desired
// destination amount for exact-output. Each direction echoes the accept
// bound from the IntentsPreview the caller displayed, and a committed
// quote that falls outside it is rejected before any funds move (the
// caller should obtain a fresh preview) — a swap cannot be executed
// against terms the user never saw:
//
//   - Exact-input: minAtomsOut, the preview's MinAtomsOut. The committed
//     quote's own guaranteed minimum (its minAmountOut, the least a solver
//     may deliver) must clear it.
//   - Exact-output: maxAtomsIn, the preview's AtomsIn. The committed
//     quote's required deposit (its amountIn, which the wallet will fund)
//     must not exceed it.
func (c *Core) IntentsExecute(pw []byte, originAssetID, destAssetID uint32, atoms uint64, slippageBps int, exactOutput bool, minAtomsOut, maxAtomsIn uint64) (*IntentsSwap, error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return nil, err
	}
	if atoms == 0 {
		if exactOutput {
			return nil, fmt.Errorf("cannot swap for zero %s", unbip(destAssetID))
		}
		return nil, fmt.Errorf("cannot swap zero %s", unbip(originAssetID))
	}
	if exactOutput {
		if maxAtomsIn == 0 {
			return nil, fmt.Errorf("no previewed maximum input supplied")
		}
		// The service fixes the output at the requested amount; the floor
		// check below still applies against it.
		minAtomsOut = atoms
	} else if minAtomsOut == 0 {
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

	req, err := c.intentsQuoteRequest(false, exactOutput, origin, dest, originWallet, destWallet, atoms, slippageBps)
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
	//
	// For EXACT_OUTPUT this check requires that the service not apply the
	// slippage tolerance to minAmountOut. Verified against the production
	// service (dry quote, 2026-09-03): an exact-output quote with a
	// nonzero slippageTolerance returns minAmountOut equal to the
	// requested output, with slippage expressed on the input side
	// (minAmountIn below amountIn) — matching the simnet mock.
	minCommittedOut, err := fromIntentsAmount(quote.MinAmountOut, &destWallet.Info().UnitInfo, dest.Decimals)
	if err != nil {
		return nil, fmt.Errorf("bad committed minAmountOut: %w", err)
	}
	// The deposit funds exactly what the committed quote prices. For
	// exact-input that is the requested amount (the quote echoes it; the
	// service's amount may sit a truncation below when the wallet's atomic
	// unit is finer than the Intents scale, and over-depositing the dust
	// is harmless). For exact-output the service computes it, and the
	// ceiling the user accepted caps it. The conversion rounds up: the
	// service's amountIn is not quantized to wallet atoms, and a deposit
	// truncated below it is never recognized as complete — the swap would
	// sit INCOMPLETE_DEPOSIT until the deadline refund.
	sendAtoms := atoms
	if exactOutput {
		sendAtoms, err = fromIntentsAmountCeil(quote.AmountIn, &originWallet.Info().UnitInfo, origin.Decimals)
		if err != nil {
			return nil, fmt.Errorf("bad committed amountIn: %w", err)
		}
		if sendAtoms == 0 {
			return nil, fmt.Errorf("committed quote prices a zero deposit")
		}
	}
	if err := checkQuoteBounds(exactOutput, minCommittedOut, minAtomsOut, sendAtoms, maxAtomsIn,
		&originWallet.Info().UnitInfo, &destWallet.Info().UnitInfo); err != nil {
		return nil, err
	}
	eng.log.Debugf("Committed quote %s: guaranteed min out %d atoms (accepted floor %d), deposit %d atoms",
		quote.DepositAddress, minCommittedOut, minAtomsOut, sendAtoms)
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
		if err := c.validateSendFeeRate(originWallet, quote.DepositAddress, sendAtoms, feeRate, false); err != nil {
			return nil, fmt.Errorf("deposit fee rate rejected: %w", err)
		}
		eng.log.Debugf("Using fee rate %d for the %s deposit", feeRate, unbip(originAssetID))
	}

	swap := &IntentsSwap{
		DepositAddress: quote.DepositAddress,
		State:          IntentsStateQuoted,
		OriginAssetID:  originAssetID,
		DestAssetID:    destAssetID,
		OriginAsset:    origin.AssetID,
		DestAsset:      dest.AssetID,
		AtomsIn:        sendAtoms,
		AmountIn:       quote.AmountIn,
		AmountOut:      quote.AmountOut,
		AtomsOut:       atomsOut,
		USDIn:          parseUSD(quote.AmountInUsd),
		USDOut:         parseUSD(quote.AmountOutUsd),
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
	eng.log.Infof("Committed swap quote %s: %s → %s (guaranteed min %s), deadline %s",
		quote.DepositAddress,
		originWallet.Info().UnitInfo.FormatAtoms(sendAtoms),
		destWallet.Info().UnitInfo.FormatAtoms(atomsOut),
		destWallet.Info().UnitInfo.FormatAtoms(minCommittedOut),
		deadline.Format(time.RFC3339))

	coin, err := originWallet.Wallet.Send(quote.DepositAddress, sendAtoms, feeRate)
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

	// The wallet's history has the send's network fee — exact for UTXO
	// wallets, an estimate for wallets that price fees at send time (EVM),
	// which the poller re-captures until the transaction settles. A lookup
	// miss leaves the fee zero for the poller to fill. The fee is priced in
	// USD immediately, while the catalog rate is closest to the fee event.
	var depositFee uint64
	var depositFeeUSD float64
	if tx, err := originWallet.WalletTransaction(ctx, coin.TxID()); err == nil && tx != nil && tx.Fees > 0 {
		depositFee = tx.Fees
		depositFeeUSD = eng.usdValue(ctx, intentsChainID(originAssetID), depositFee)
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
		if fresh.DepositTxFee == 0 {
			fresh.DepositTxFee = depositFee
			fresh.USDDepositFee = depositFeeUSD
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
		swap.DepositTxFee = depositFee
		swap.USDDepositFee = depositFeeUSD
		if err := eng.db.StoreIntentsSwap(swap); err != nil {
			eng.log.Errorf("Error storing swap record %s after deposit: %v", swap.DepositAddress, err)
		}
	}
	eng.swapMtx.Unlock()
	eng.log.Infof("Swap %s deposit broadcast: %s tx %s (fee %d atoms)",
		swap.DepositAddress, unbip(originAssetID), swap.DepositTxID, swap.DepositTxFee)
	if err := eng.client.SubmitDeposit(ctx, swap.DepositAddress, swap.DepositTxID); err != nil {
		eng.log.Warnf("Deposit hint submission failed (service will detect on-chain): %v", err)
	} else {
		eng.log.Debugf("Deposit txid hint submitted for swap %s", swap.DepositAddress)
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
	if !eng.reconstructing.CompareAndSwap(false, true) {
		return 0, errors.New("a reconstruction run is already in progress")
	}
	defer eng.reconstructing.Store(false)

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
	eng.log.Debugf("Probing recent outgoing transactions for lost swap records (%d already known)", len(known))

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
			// Be polite to the open endpoint: space every probe, hit or
			// miss. Nearly all probed recipients are not deposit
			// addresses, so a throttle only on stored hits would leave
			// the common path an unthrottled burst.
			select {
			case <-time.After(250 * time.Millisecond):
			case <-ctx.Done():
				return found, ctx.Err()
			}
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
			// An origin that resolves to a different chain than the probed
			// wallet means the address matched a swap that did not originate
			// here (EVM address formats collide across chains), so the probed
			// transaction is not this swap's deposit. Un-mark the address so
			// the right chain's wallet can probe it later in the walk.
			if intentsChainID(swap.OriginAssetID) != intentsChainID(w.AssetID) {
				eng.log.Debugf("Skipping probed address %s: origin asset %s is not on the %s chain",
					*tx.Recipient, swap.OriginAsset, unbip(w.AssetID))
				delete(known, *tx.Recipient)
				continue
			}
			// The probed history transaction is the deposit send; its
			// recorded fee is the only shot at the fee for a swap that is
			// already terminal (the poller will never revisit it).
			swap.DepositTxFee = tx.Fees
			stored, err := eng.storeReconstructed(swap)
			if err != nil {
				return found, err
			}
			if !stored {
				continue // a live record for this address appeared mid-run
			}
			found++
			eng.log.Infof("Recovered swap record %s (%s → %s, state %s)", swap.DepositAddress,
				intentsAssetLabel(swap.OriginAssetID, swap.OriginAsset),
				intentsAssetLabel(swap.DestAssetID, swap.DestAsset), swap.State)
			c.notify(newIntentsSwapNote(swap, "Swap record recovered",
				fmt.Sprintf("Recovered a swap to deposit address %s.", swap.DepositAddress), db.Poke))
			// A record rebuilt already terminal is never polled again, so
			// refreshSwap's payout recording will not run for it. Hand the
			// service-reported txids to the receiving wallet here — this is
			// the seed-restore case IncomingTxRecorder exists for. The call
			// is a no-op for non-terminal states, and recording is
			// idempotent wallet-side.
			eng.recordIncomingTxs(swap)
		}
	}
	if found > 0 {
		eng.log.Infof("Reconstruction recovered %d swap record(s)", found)
		eng.poke()
	} else {
		eng.log.Debugf("Reconstruction found no unknown swap records")
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
// response. Both sides are resolved through the catalog (resolveChain);
// an unresolved destination is recorded as intentsAssetUnknown rather
// than 0, which would be misread as BTC. The origin must be resolved
// too, not taken from the probed wallet: an EVM parent wallet's history
// includes its tokens' sends, so a token-origin swap surfaces under the
// parent's probe, and recording it as the parent's native asset would
// convert amounts at the wrong unit scale. An unresolvable origin (e.g.
// a since-delisted asset) falls back to probedAssetID with zero atoms
// fields.
func swapFromStatus(resolveChain intentsChainResolver, probedAssetID uint32, depositAddress, depositTxID string, status *intents.StatusResponse) (*IntentsSwap, error) {
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
	originAssetID := probedAssetID
	var originTok *intents.Token
	if id, tok, ok := resolveChain(echo.OriginAsset); ok {
		originAssetID, originTok = id, tok
	}

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

	// The re-serialized quote preserves the service's original USD pricing,
	// superseded by the swap details' execution-time pricing when reported.
	// USDDepositFee and USDRefunded stay zero: their event-time rates are
	// unknowable after the fact.
	usdIn, usdOut := parseUSD(quote.AmountInUsd), parseUSD(quote.AmountOutUsd)
	if usd := parseUSD(status.SwapDetails.AmountInUsd); usd != 0 {
		usdIn = usd
	}
	if usd := parseUSD(status.SwapDetails.AmountOutUsd); usd != 0 {
		usdOut = usd
	}

	return &IntentsSwap{
		DepositAddress: depositAddress,
		DepositMemo:    quote.DepositMemo,
		State:          state,
		OriginAssetID:  originAssetID,
		DestAssetID:    destAssetID,
		OriginAsset:    echo.OriginAsset,
		DestAsset:      echo.DestinationAsset,
		// The quote's own AmountIn, never echo.Amount: for an EXACT_OUTPUT
		// request the echoed amount is the destination amount, and recording
		// it as the input would misprice the origin side. For exact-input the
		// two agree. This mirrors what IntentsExecute records.
		AtomsIn:        atoms(originAssetID, originTok, quote.AmountIn),
		AmountIn:       quote.AmountIn,
		AmountOut:      amountOut,
		AtomsOut:       atoms(destAssetID, destTok, amountOut),
		USDIn:          usdIn,
		USDOut:         usdOut,
		Recipient:      echo.Recipient,
		RefundTo:       echo.RefundTo,
		Deadline:       deadline,
		TimeEstimate:   quote.TimeEstimate,
		AppFees:        echo.AppFees,
		DepositTxID:    depositTxID,
		NearTxHashes:   status.SwapDetails.NearTxHashes,
		DestinationTxs: txHashes(status.SwapDetails.DestinationChainTxs),
		RefundTxs:      refundTxHashes(status.SwapDetails.OriginChainTxs, depositTxID),
		RefundedAmount: status.SwapDetails.RefundedAmount,
		RefundedAtoms:  atoms(originAssetID, originTok, status.SwapDetails.RefundedAmount),
		RefundReason:   status.SwapDetails.RefundReason,
		QuoteEnvelope:  envelope,
		Reconstructed:  true,
		CreatedAt:      time.Now(),
	}, nil
}
