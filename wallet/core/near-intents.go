// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Cross-chain swaps through the NEAR Intents 1Click service. All
// Intents-specific Core functionality lives in this file; the API client
// is wallet/intents. Design and trust model: docs/near-intents-plan.md.
//
// A swap is identified by its single-use deposit address. Records persist
// in a dedicated key-value database (intents.db) through the state
// machine QUOTED → DEPOSIT_SENT → (service statuses) → SUCCESS | REFUNDED
// | FAILED, with EXPIRED for quotes that were never funded. Quote
// signatures are verified once, at quote time, and the signed envelope is
// persisted; the service's status endpoint re-serializes quotes in a form
// that cannot be re-verified.

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encrypt"
	"github.com/bisoncraft/caribou/util/lexi"
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

	// intentsFeeRecipient is the treasury account identity credited by the
	// service's appFees mechanism (an 0x-style secp256k1 identity inside
	// intents.near whose withdrawals are ERC-191-signed). Empty disables
	// the application fee; set at release. The key itself lives entirely
	// outside the repo.
	intentsFeeRecipient = ""

	// intentsDefaultSlippageBps is the default quote slippage tolerance.
	intentsDefaultSlippageBps = 100

	// intentsPollInterval is the cadence of the swap status poller while
	// any swap is active.
	intentsPollInterval = 15 * time.Second

	// intentsCatalogTTL is how long a fetched asset catalog stays fresh.
	intentsCatalogTTL = 15 * time.Minute

	// intentsDBFilename is the swap-record database, beside the main DB.
	intentsDBFilename = "intents.db"

	// intentsExpiryGrace pads the quote deadline before an unfunded quote
	// is locally marked EXPIRED.
	intentsExpiryGrace = 10 * time.Minute
)

// Engine-local swap states. All other states mirror the service statuses
// in the intents package (StatusProcessing, StatusSuccess, ...).
const (
	// IntentsStateQuoted: a committed quote exists but the deposit has not
	// been broadcast.
	IntentsStateQuoted = "QUOTED"
	// IntentsStateDepositSent: the origin-chain deposit is broadcast but
	// the service hasn't yet observed it.
	IntentsStateDepositSent = "DEPOSIT_SENT"
	// IntentsStateExpired: never funded and the deadline (plus grace) has
	// passed. Terminal, local-only.
	IntentsStateExpired = "EXPIRED"
)

// intentsSwapTerminal reports whether a swap record needs no further
// polling.
func intentsSwapTerminal(state string) bool {
	return state == IntentsStateExpired || intents.TerminalStatus(state)
}

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
	if d, ok := intentsDeadlines[originAssetID]; ok {
		return d
	}
	return 45 * time.Minute // account-based chains confirm in seconds
}

// IntentsSwap is a persisted cross-chain swap record, keyed by its
// single-use deposit address. Amount strings are in the Intents-side base
// units; Atoms fields are wallet atomic units.
type IntentsSwap struct {
	DepositAddress string    `json:"depositAddress"`
	DepositMemo    string    `json:"depositMemo,omitempty"`
	State          string    `json:"state"`
	OriginAssetID  uint32    `json:"originAssetID"`
	DestAssetID    uint32    `json:"destAssetID"`
	OriginAsset    string    `json:"originAsset"` // Intents asset ID
	DestAsset      string    `json:"destAsset"`   // Intents asset ID
	AtomsIn        uint64    `json:"atomsIn"`
	AmountIn       string    `json:"amountIn"`
	AmountOut      string    `json:"amountOut"` // quoted, destination units
	AtomsOut       uint64    `json:"atomsOut"`  // quoted, destination wallet atoms
	Recipient      string    `json:"recipient"`
	RefundTo       string    `json:"refundTo"`
	Deadline       time.Time `json:"deadline"`
	TimeEstimate   int64     `json:"timeEstimate"` // seconds, from the quote

	DepositTxID    string   `json:"depositTxID,omitempty"`
	NearTxHashes   []string `json:"nearTxHashes,omitempty"`
	DestinationTxs []string `json:"destinationTxs,omitempty"`
	RefundTxs      []string `json:"refundTxs,omitempty"`
	RefundedAmount string   `json:"refundedAmount,omitempty"`
	RefundReason   string   `json:"refundReason,omitempty"`

	// AppFees is the service-rewritten fee breakdown echoed with the
	// quote: the application fee split plus the keyless surcharge entry.
	AppFees []intents.AppFee `json:"appFees,omitempty"`

	// QuoteEnvelope is the full signed /quote response, verified at quote
	// time and retained for audit. Reconstructed records (post-restore)
	// carry the status endpoint's re-serialized, non-verifiable copy and
	// set Reconstructed.
	QuoteEnvelope json.RawMessage `json:"quoteEnvelope,omitempty"`
	Reconstructed bool            `json:"reconstructed,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *IntentsSwap) MarshalBinary() ([]byte, error) {
	return json.Marshal(s)
}

func (s *IntentsSwap) UnmarshalBinary(b []byte) error {
	return json.Unmarshal(b, s)
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

// intentsEngine owns the swap-record database, the 1Click client, the
// cached asset catalog, and the status poller.
type intentsEngine struct {
	c      *Core
	log    util.Logger
	client *intents.Client
	db     *lexi.DB
	swaps  *lexi.Table

	catMtx  sync.Mutex
	catalog *intents.Catalog

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
		client, err := intents.NewClient(&intents.Config{AppFees: intentsAppFees()}, logger)
		if err != nil {
			logger.Errorf("Error creating 1Click client: %v", err)
			return
		}
		ldb, err := lexi.New(&lexi.Config{
			Path: filepath.Join(filepath.Dir(c.cfg.DBPath), intentsDBFilename),
			Log:  logger.SubLogger("DB"),
		})
		if err != nil {
			logger.Errorf("Error opening intents database: %v", err)
			return
		}
		// The DB gets its own context so it outlives the poller: writes
		// triggered by in-flight API methods during wind-down land before
		// the storage engine closes, mirroring the master tx DB pattern.
		// Cancellation must precede the Wait — lexi's Connect goroutine
		// exits only on context cancellation — so both run in one deferred
		// closure rather than as separate LIFO defers.
		dbCtx, stopDB := context.WithCancel(context.Background())
		dbWG, err := ldb.Connect(dbCtx)
		if err != nil {
			stopDB()
			logger.Errorf("Error connecting intents database: %v", err)
			return
		}
		defer func() {
			stopDB()
			dbWG.Wait()
		}()

		swaps, err := ldb.Table("swaps")
		if err != nil {
			logger.Errorf("Error creating swaps table: %v", err)
			return
		}

		eng := &intentsEngine{
			c:       c,
			log:     logger,
			client:  client,
			db:      ldb,
			swaps:   swaps,
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

// run resumes any active swaps and polls until the context dies.
func (e *intentsEngine) run(ctx context.Context) {
	e.pollActive(ctx)
	ticker := time.NewTicker(intentsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.pollActive(ctx)
		case <-e.trigger:
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

// allSwaps loads every persisted swap record.
func (e *intentsEngine) allSwaps() ([]*IntentsSwap, error) {
	var swaps []*IntentsSwap
	err := e.swaps.Iterate(nil, func(it *lexi.Iter) error {
		return it.V(func(vB []byte) error {
			swap := new(IntentsSwap)
			if err := swap.UnmarshalBinary(vB); err != nil {
				return fmt.Errorf("error decoding swap record: %w", err)
			}
			swaps = append(swaps, swap)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(swaps, func(i, j int) bool { return swaps[i].CreatedAt.After(swaps[j].CreatedAt) })
	return swaps, nil
}

// storeSwap persists a swap record, stamping UpdatedAt.
func (e *intentsEngine) storeSwap(swap *IntentsSwap) error {
	swap.UpdatedAt = time.Now()
	return e.swaps.Set([]byte(swap.DepositAddress), swap, lexi.WithReplace())
}

// pollActive refreshes all non-terminal swaps from the service.
func (e *intentsEngine) pollActive(ctx context.Context) {
	swaps, err := e.allSwaps()
	if err != nil {
		e.log.Errorf("Error loading swap records: %v", err)
		return
	}
	for _, swap := range swaps {
		if ctx.Err() != nil {
			return
		}
		if intentsSwapTerminal(swap.State) {
			continue
		}
		if err := e.refreshSwap(ctx, swap); err != nil {
			e.log.Errorf("Error refreshing swap %s: %v", swap.DepositAddress, err)
		}
	}
}

// refreshSwap polls the service for one swap and persists and announces
// any change.
func (e *intentsEngine) refreshSwap(ctx context.Context, swap *IntentsSwap) error {
	status, err := e.client.Status(ctx, swap.DepositAddress)
	if err != nil {
		if errors.Is(err, intents.ErrSwapNotFound) {
			// Not necessarily fatal: the service can lag a fresh quote. An
			// unfunded quote expires locally.
			e.maybeExpire(swap)
			return nil
		}
		return err
	}

	if status.Status == intents.StatusPendingDeposit {
		// The service is still waiting; keep the local view (QUOTED or
		// DEPOSIT_SENT), but check for expiry of unfunded quotes.
		e.maybeExpire(swap)
		return nil
	}
	newState := status.Status

	details := &status.SwapDetails
	changed := newState != swap.State ||
		len(details.NearTxHashes) != len(swap.NearTxHashes) ||
		len(details.DestinationChainTxs) != len(swap.DestinationTxs)

	if !changed {
		return nil
	}

	swap.State = newState
	swap.NearTxHashes = details.NearTxHashes
	swap.DestinationTxs = txHashes(details.DestinationChainTxs)
	swap.RefundedAmount = details.RefundedAmount
	swap.RefundReason = details.RefundReason
	// Origin-chain hashes include the refund transaction when one occurs.
	if newState == intents.StatusRefunded {
		swap.RefundTxs = txHashes(details.OriginChainTxs)
	}
	if err := e.storeSwap(swap); err != nil {
		return err
	}

	subject, severity := intentsNoteContent(newState)
	e.c.notify(newIntentsSwapNote(swap, subject,
		fmt.Sprintf("%s → %s swap %s", strings.ToUpper(unbip(swap.OriginAssetID)),
			strings.ToUpper(unbip(swap.DestAssetID)), strings.ToLower(strings.ReplaceAll(newState, "_", " "))),
		severity))

	if intentsSwapTerminal(newState) {
		e.c.updateAssetBalance(swap.DestAssetID)
		if newState == intents.StatusRefunded {
			e.c.updateAssetBalance(swap.OriginAssetID)
		}
	}
	return nil
}

// maybeExpire marks an unfunded quote EXPIRED after its deadline plus
// grace.
func (e *intentsEngine) maybeExpire(swap *IntentsSwap) {
	if swap.State != IntentsStateQuoted || time.Now().Before(swap.Deadline.Add(intentsExpiryGrace)) {
		return
	}
	swap.State = IntentsStateExpired
	if err := e.storeSwap(swap); err != nil {
		e.log.Errorf("Error storing expired swap %s: %v", swap.DepositAddress, err)
		return
	}
	e.c.notify(newIntentsSwapNote(swap, "Swap expired",
		"The quote expired before a deposit was sent. No funds moved.", db.WarningLevel))
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

// freshCatalog returns the asset catalog, fetching if the cache is stale.
func (e *intentsEngine) freshCatalog(ctx context.Context) (*intents.Catalog, error) {
	e.catMtx.Lock()
	defer e.catMtx.Unlock()
	if e.catalog != nil && time.Since(e.catalog.FetchedAt) < intentsCatalogTTL {
		return e.catalog, nil
	}
	cat, err := e.client.Catalog(ctx)
	if err != nil {
		if e.catalog != nil {
			e.log.Warnf("Error refreshing asset catalog, using stale: %v", err)
			return e.catalog, nil
		}
		return nil, fmt.Errorf("error fetching asset catalog: %w", err)
	}
	e.catalog = cat
	return cat, nil
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
// the wallet set. Only chain-native assets are supported until the token
// layer is wired.
func (c *Core) intentsPair(ctx context.Context, eng *intentsEngine, originAssetID, destAssetID uint32) (origin, dest *intents.Token, originWallet, destWallet *xcWallet, err error) {
	cat, err := eng.freshCatalog(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	origin, dest = cat.Native(originAssetID), cat.Native(destAssetID)
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
// service catalog with this build's chain mapping, flagged by wallet
// availability. Only chain natives until the token layer is wired.
func (c *Core) IntentsAssets(ctx context.Context) ([]*IntentsAsset, error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return nil, err
	}
	cat, err := eng.freshCatalog(ctx)
	if err != nil {
		return nil, err
	}
	var assets []*IntentsAsset
	for _, chainID := range cat.Chains() {
		tok := cat.Native(chainID)
		wallet, found := c.wallet(chainID)
		atomDecimals := tok.Decimals
		if unitInfo, err := asset.UnitInfo(chainID); err == nil {
			if exp, ok := pow10Exp(unitInfo.Conventional.ConversionFactor); ok {
				atomDecimals = exp
			}
		}
		assets = append(assets, &IntentsAsset{
			AssetID:      chainID,
			Symbol:       unbip(chainID),
			IntentsID:    tok.AssetID,
			Decimals:     tok.Decimals,
			AtomDecimals: atomDecimals,
			PriceUSD:     tok.Price,
			WalletReady:  found && wallet.connected(),
		})
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].AssetID < assets[j].AssetID })
	return assets, nil
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
func (c *Core) IntentsExecute(pw []byte, originAssetID, destAssetID uint32, atoms uint64, slippageBps int) (*IntentsSwap, error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return nil, err
	}
	if atoms == 0 {
		return nil, fmt.Errorf("cannot swap zero %s", unbip(originAssetID))
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
	deadline, err := time.Parse(time.RFC3339, quote.Deadline)
	if err != nil {
		e := eng
		e.log.Warnf("Unparseable quote deadline %q: %v", quote.Deadline, err)
		deadline = time.Now().Add(intentsDeadline(originAssetID))
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
	if err := eng.storeSwap(swap); err != nil {
		return nil, fmt.Errorf("error persisting swap record: %w", err)
	}

	coin, err := originWallet.Wallet.Send(quote.DepositAddress, atoms, originWallet.Wallet.FeeRate())
	if err != nil {
		// The record stays QUOTED and expires unfunded after the deadline.
		return nil, fmt.Errorf("deposit send failed (no funds moved, quote will expire): %w", err)
	}

	swap.State = IntentsStateDepositSent
	swap.DepositTxID = coin.TxID()
	if err := eng.storeSwap(swap); err != nil {
		eng.log.Errorf("Error updating swap record %s after deposit: %v", swap.DepositAddress, err)
	}
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
	return eng.allSwaps()
}

// IntentsRefresh polls the service for one swap immediately.
func (c *Core) IntentsRefresh(ctx context.Context, depositAddress string) (*IntentsSwap, error) {
	eng, err := c.intentsEngine()
	if err != nil {
		return nil, err
	}
	swap := new(IntentsSwap)
	if err := eng.swaps.Get([]byte(depositAddress), swap); err != nil {
		return nil, fmt.Errorf("no swap record for %s: %w", depositAddress, err)
	}
	if !intentsSwapTerminal(swap.State) {
		if err := eng.refreshSwap(ctx, swap); err != nil {
			return nil, err
		}
	}
	return swap, nil
}

// intentsReconstructProbeLimit caps, per wallet, how many recent outgoing
// transactions are probed against the service during reconstruction.
const intentsReconstructProbeLimit = 25

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

	known := make(map[string]bool)
	swaps, err := eng.allSwaps()
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
			swap, err := swapFromStatus(w.AssetID, *tx.Recipient, tx.ID, status)
			if err != nil {
				eng.log.Warnf("Error reconstructing swap %s: %v", *tx.Recipient, err)
				continue
			}
			if err := eng.storeSwap(swap); err != nil {
				return found, err
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

// swapFromStatus builds a Reconstructed swap record from a status
// response.
func swapFromStatus(originAssetID uint32, depositAddress, depositTxID string, status *intents.StatusResponse) (*IntentsSwap, error) {
	if status.QuoteResponse == nil {
		return nil, fmt.Errorf("status carries no quote")
	}
	echo := &status.QuoteResponse.Request
	quote := &status.QuoteResponse.Quote

	destAssetID, _ := intentsChainOf(echo.DestinationAsset)

	deadline, _ := time.Parse(time.RFC3339, quote.Deadline)
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
		AmountIn:       echo.Amount,
		AmountOut:      quote.AmountOut,
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

// intentsChainOf best-effort resolves the chain asset ID of an Intents
// asset ID via the catalog chain map, for reconstructed records where the
// original pair selection is unknown.
func intentsChainOf(intentsAssetID string) (uint32, bool) {
	// Intents asset IDs do not embed the chain slug uniformly; the pol/bsc
	// nep245 IDs and sol-suffixed nep141 IDs differ. Reconstruction leaves
	// the destination chain unset when it cannot be determined; the UI
	// falls back to the Intents asset ID string.
	switch {
	case strings.HasPrefix(intentsAssetID, "nep141:") && strings.HasSuffix(intentsAssetID, ".omft.near"):
		inner := strings.TrimSuffix(strings.TrimPrefix(intentsAssetID, "nep141:"), ".omft.near")
		if i := strings.IndexAny(inner, "-."); i > 0 {
			inner = inner[:i]
		}
		if id, ok := intents.ChainBipID(inner); ok {
			return id, true
		}
	case strings.HasPrefix(intentsAssetID, "nep141:"):
		return 397, true // NEAR-chain NEP-141 (wrap.near, native USDC, ...)
	}
	return 0, false
}
