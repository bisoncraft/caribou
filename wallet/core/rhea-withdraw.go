// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// RHEA lending withdrawal: a business, signed by the lending identity and
// submitted through RHEA's relayer, that pays the relayer fee out of the
// plain supplied balance, withdraws the net amount, and transfers it to a
// 1Click deposit address that pays the wallet. Execution is attributed only
// to an exec carrying this operation's exact signature and business; the
// 1Click leg then follows the shared leg rules (intents-leg.go).

package core

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/encrypt"
	"github.com/bisoncraft/caribou/wallet/asset/near"
	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/intents"
	"github.com/bisoncraft/caribou/wallet/rhea"
)

const (
	// rheaFeeCapMicroUSD is the absolute relayer fee cap: 2 USD at the
	// fee token's price (see feeCapPrice).
	rheaFeeCapMicroUSD = 2_000_000
	// rheaResubmitInterval spaces resubmissions of an identical signed
	// request whose submission outcome is unknown.
	rheaResubmitInterval = time.Minute
	// rheaNotExecutedMargin is how far past a business deadline the final
	// chain time must be before an unconsumed nonce concludes NOT_EXECUTED.
	rheaNotExecutedMargin = 2 * time.Minute
	// rheaExecSearchRecords bounds the relayer history records one search
	// for an operation's exec reads.
	rheaExecSearchRecords = 500
	// rheaNEARChainID selects the deadline for a leg that starts on NEAR.
	rheaNEARChainID = 397
)

// rheaSubmitter is the relayer submission API.
type rheaSubmitter interface {
	Config(ctx context.Context) (*rhea.LendingConfig, error)
	Submit(ctx context.Context, mcaID string, req *rhea.SignedRequest) (string, error)
	BatchStatus(ctx context.Context, batchID string) ([]*rhea.BatchRequest, error)
}

// RheaWithdrawPreview is a preview of a withdrawal. Token amounts are
// market-token base units; atoms are destination wallet atoms.
type RheaWithdrawPreview struct {
	TokenID     string `json:"tokenID"`
	Symbol      string `json:"symbol"`
	Decimals    uint8  `json:"decimals"`
	DestAssetID uint32 `json:"destAssetID"`
	// Amount is the net amount withdrawn and sent; MaxAmount the largest
	// net amount withdrawable now.
	Amount    string `json:"amount"`
	MaxAmount string `json:"maxAmount"`
	// Fee is the relayer fee, paid out of the supplied balance first;
	// FeeCap the most Caribou signs, 2 USD at the price FeeCapSource names
	// (RheaFeeCapFiatRate or RheaFeeCapCatalog), and FeeUSD the fee at
	// that price.
	Fee                string  `json:"fee"`
	FeeUSD             float64 `json:"feeUSD"`
	FeeCap             string  `json:"feeCap"`
	FeeCapSource       string  `json:"feeCapSource"`
	Supplied           string  `json:"supplied"`
	AvailableLiquidity string  `json:"availableLiquidity"`
	RegisterMCA        bool    `json:"registerMCA"`
	// AtomsOut is the quoted delivery to the wallet; MinAtomsOut the least
	// a solver may deliver, the floor execute is bound to.
	AtomsOut     uint64    `json:"atomsOut"`
	MinAtomsOut  uint64    `json:"minAtomsOut"`
	AmountOutUSD string    `json:"amountOutUSD"`
	TimeEstimate int64     `json:"timeEstimate"`
	Block        RheaBlock `json:"block"`
}

// rheaWithdrawPlan is a withdrawal resolved against chain state, the relayer
// configuration, the catalog, and a quote.
type rheaWithdrawPlan struct {
	eng          *rheaEngine
	ieng         *intentsEngine
	market       *rhea.Market
	block        *near.BlockHeader
	identity     rheaIdentity
	mca          string
	nonce        uint64
	asset        *rhea.Asset
	decimals     uint8
	registerMCA  bool
	fee          *big.Int
	feeCap       *big.Int
	feeCapSource string
	feeCapPrice  float64 // USD per whole token, from feeCapSource
	feeUSD       float64
	supplied     *big.Int // plain supplied, internal units
	available    *big.Int // liquidity estimate, internal units
	net          *big.Int
	netMax       *big.Int
	wallet       *xcWallet
	dest         *intents.Token
	req          *intents.QuoteRequest
	resp         *intents.QuoteResponse
	atomsOut     uint64
	minAtomsOut  uint64
	params       *rhea.WithdrawParams
	business     *rhea.Business
}

// Fee cap price sources.
const (
	// RheaFeeCapFiatRate is Caribou's fiat rate for the fee token's
	// ticker.
	RheaFeeCapFiatRate = "fiatRate"
	// RheaFeeCapCatalog is the fee token's price in a fresh 1Click
	// catalog, used when no current fiat rate is known.
	RheaFeeCapCatalog = "catalog"
)

// feeCapPrice chooses the USD price (per whole token) that sets a 2 USD
// fee cap: Caribou's fiat rate for ticker when current, otherwise the
// price catalogPrice reads from the 1Click catalog. A withdrawal or sweep
// is refused only when neither is usable; symbol names the token in that
// refusal.
func (e *rheaEngine) feeCapPrice(ticker, symbol string, catalogPrice func() (float64, error)) (float64, string, error) {
	if rate, ok := e.tickerRate(ticker); ok {
		return rate, RheaFeeCapFiatRate, nil
	}
	price, err := catalogPrice()
	if err != nil {
		return 0, "", fmt.Errorf("no current USD rate for %s, and %v; the 2 USD fee cap cannot be set", symbol, err)
	}
	return price, RheaFeeCapCatalog, nil
}

// catalogPrice returns tok's price from a 1Click catalog that is within its
// freshness window. tok is the catalog entry for the fee token, matched by
// asset ID (nil when absent); symbol names the token in an error.
func catalogPrice(cat *intents.Catalog, tok *intents.Token, symbol string) (float64, error) {
	if tok == nil {
		return 0, fmt.Errorf("%s is not in the 1Click catalog", symbol)
	}
	if time.Since(cat.FetchedAt) >= intentsCatalogTTL {
		return 0, fmt.Errorf("the 1Click price of %s is not fresh", symbol)
	}
	if _, ok := rheaPrice(tok.Price); !ok {
		return 0, fmt.Errorf("the 1Click price of %s (%v) is not usable", symbol, tok.Price)
	}
	return tok.Price, nil
}

// rheaFeeCap converts the 2 USD cap to token base units at price (USD per
// whole token), rounding down so the cap is never worth more than 2 USD at
// the price.
func rheaFeeCap(price float64, decimals uint8) (*big.Int, error) {
	rate, ok := rheaPrice(price)
	if !ok {
		return nil, fmt.Errorf("no usable price for the fee cap (%v)", price)
	}
	return rheaUSDToUnits(rheaFeeCapMicroUSD, int(decimals), rate, false), nil
}

// planWithdraw resolves a withdrawal of amount (token base units; empty for
// the most available) from a market's plain supplied balance to the
// destination wallet. The admission gate must be open. The fee is reserved
// first and capped; the business is built only for a committed quote.
func (c *Core) planWithdraw(ctx context.Context, tokenID string, destAssetID uint32, amount string, dry bool) (*rheaWithdrawPlan, error) {
	eng, err := c.rheaEngine()
	if err != nil {
		return nil, err
	}
	if eng.submitter == nil {
		return nil, errors.New("the RHEA relayer is not available")
	}
	ieng, err := eng.oneClick()
	if err != nil {
		return nil, err
	}
	p := &rheaWithdrawPlan{eng: eng, ieng: ieng}
	if p.market = eng.dep.Market(tokenID); p.market == nil {
		return nil, fmt.Errorf("%s is not a supported lending market", tokenID)
	}
	ids := eng.identities()
	if len(ids) == 0 {
		return nil, errors.New("log in to withdraw")
	}
	eng.viewed.Store(true)
	if p.block, err = eng.chain.FinalBlock(ctx); err != nil {
		return nil, fmt.Errorf("error reading the final block: %w", err)
	}
	B := p.block.Hash
	disc := eng.discover(ctx, ids, B)
	if disc.Status != RheaFound {
		return nil, fmt.Errorf("lending account discovery is %s", disc.Status)
	}
	p.mca = disc.found.MCA
	p.identity = rheaIdentity{Version: rhea.DerivationVersion(disc.found.Version), Identity: rhea.Identity(disc.found.Identity)}

	active, err := eng.db.ActiveRheaOps()
	if err != nil {
		return nil, fmt.Errorf("error loading lending operations: %w", err)
	}
	acct, err := eng.recordAccount(disc.found, p.block)
	if err != nil {
		return nil, fmt.Errorf("error recording the lending account: %w", err)
	}
	if p.nonce, _, err = eng.reader.Nonce(ctx, p.mca, B); err != nil {
		return nil, err
	}
	adm := rheaAdmission(&rheaAdmissionInput{
		discovery: disc.Status, mca: p.mca, account: acct, nonce: p.nonce,
		blockTime: p.block.Timestamp, now: eng.now(), activeOps: active,
	})
	if !adm.Open {
		if adm.RecoveryUnresolved {
			eng.poke()
		}
		return nil, fmt.Errorf("withdrawals are not admitted now: %s", strings.Join(adm.Reasons, "; "))
	}

	if p.asset, _, err = eng.reader.Asset(ctx, tokenID, B); err != nil {
		return nil, err
	}
	if !p.asset.Config.CanWithdraw {
		return nil, fmt.Errorf("the %s market is not allowing withdrawals", p.market.Symbol)
	}
	md, _, err := eng.reader.FTMetadata(ctx, tokenID, B)
	if err != nil {
		return nil, err
	}
	p.decimals = md.Decimals
	pos, _, err := eng.reader.AccountPositions(ctx, p.mca, B)
	if err != nil {
		return nil, err
	}
	p.supplied = new(big.Int)
	if pos != nil {
		p.supplied = pos.SuppliedBalance(tokenID)
	}
	p.available = p.asset.AvailableLiquidity()
	sb, _, err := eng.reader.StorageBalanceOf(ctx, tokenID, p.mca, B)
	if err != nil {
		return nil, err
	}
	p.registerMCA = sb == nil

	cfg, err := eng.submitter.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("relayer configuration: %w", err)
	}
	if cfg.CollectFeeAccountID != eng.dep.Relayer {
		return nil, fmt.Errorf("the relayer collects fees to %q, not %s", cfg.CollectFeeAccountID, eng.dep.Relayer)
	}
	gasFee := cfg.GasFee[tokenID]
	if gasFee == nil {
		return nil, fmt.Errorf("the relayer publishes no fee for %s", p.market.Symbol)
	}
	nearValue, _, err := eng.reader.NearValue(ctx, tokenID, B)
	if err != nil {
		return nil, err
	}
	storage := 1
	if p.registerMCA {
		storage = 2
	}
	if p.fee, err = rhea.RelayerFee(gasFee, nearValue, storage); err != nil {
		return nil, err
	}

	cat, err := ieng.freshCatalog(ctx)
	if err != nil {
		return nil, err
	}
	mtok := cat.ByAssetID(p.market.OneClickAssetID)
	if mtok == nil {
		return nil, fmt.Errorf("the %s market token is not in the swap catalog", p.market.Symbol)
	}
	if err := verifyMarketAsset(cat, mtok.AssetID, p.market); err != nil {
		return nil, err
	}
	price, source, err := eng.feeCapPrice(p.market.FiatTicker, p.market.Symbol, func() (float64, error) {
		return catalogPrice(cat, mtok, p.market.Symbol)
	})
	if err != nil {
		return nil, err
	}
	if p.feeCap, err = rheaFeeCap(price, p.decimals); err != nil {
		return nil, err
	}
	p.feeCapSource, p.feeCapPrice = source, price
	feeFloat, _ := new(big.Rat).SetFrac(p.fee, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(p.decimals)), nil)).Float64()
	p.feeUSD = feeFloat * price
	if p.fee.Cmp(p.feeCap) > 0 {
		return nil, fmt.Errorf("the relayer fee of %s exceeds the 2 USD cap (%s)", p.fee, p.feeCap)
	}

	// Fee first: the fee comes out of the plain supplied balance before the
	// net amount is sized, and the gross debit must fit both the balance
	// and the market's liquidity. Collateral is never touched.
	x := p.asset.Config.ExtraDecimals
	feeInternal := rhea.ToInternal(p.fee, x)
	limit := p.supplied
	if p.available.Cmp(limit) < 0 {
		limit = p.available
	}
	p.netMax = new(big.Int).Sub(limit, feeInternal)
	if p.netMax.Sign() <= 0 {
		return nil, fmt.Errorf("nothing is withdrawable from %s after the relayer fee of %s", p.market.Symbol, p.fee)
	}
	p.netMax = rhea.FromInternal(p.netMax, x)
	if amount == "" {
		p.net = new(big.Int).Set(p.netMax)
	} else if p.net, err = parseTokenAmount(amount); err != nil {
		return nil, err
	}
	if p.net.Sign() <= 0 {
		return nil, errors.New("nothing to withdraw")
	}
	if p.net.Cmp(p.netMax) > 0 {
		return nil, fmt.Errorf("at most %s can be withdrawn now", p.netMax)
	}
	if new(big.Int).Mul(p.fee, big.NewInt(50)).Cmp(p.net) > 0 {
		return nil, fmt.Errorf("the relayer fee of %s exceeds 2%% of %s; withdraw more", p.fee, p.net)
	}

	var found bool
	if p.wallet, found = c.wallet(destAssetID); !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(destAssetID))
	}
	if p.dest = resolveIntentsAsset(cat, destAssetID); p.dest == nil {
		return nil, fmt.Errorf("%s is not supported for cross-chain swaps", unbip(destAssetID))
	}
	recipient, err := intentsWalletAddress(p.wallet)
	if err != nil {
		return nil, fmt.Errorf("error getting the recipient address: %w", err)
	}
	// Withdrawal quotes carry no Caribou application fee (D2). The refund
	// goes back to the MCA, the only account the deposit can come from.
	p.req = &intents.QuoteRequest{
		Dry:               dry,
		SwapType:          intents.SwapTypeExactInput,
		SlippageTolerance: intentsDefaultSlippageBps,
		OriginAsset:       mtok.AssetID,
		DepositType:       intents.LocationOriginChain,
		DestinationAsset:  p.dest.AssetID,
		Amount:            p.net.String(),
		RefundTo:          p.mca,
		RefundType:        intents.LocationOriginChain,
		Recipient:         recipient,
		RecipientType:     intents.LocationDestinationChain,
		Deadline:          time.Now().Add(intentsDeadline(rheaNEARChainID)).UTC().Format("2006-01-02T15:04:05.000Z"),
		NoAppFee:          true,
	}
	if p.resp, err = ieng.client.Quote(ctx, p.req); err != nil {
		return nil, err
	}
	if err := verifyMarketAsset(cat, p.resp.Request.OriginAsset, p.market); err != nil {
		return nil, fmt.Errorf("quote origin asset: %w", err)
	}
	if err := verifyQuoteAsset(cat, p.resp.Request.DestinationAsset, destAssetID); err != nil {
		return nil, fmt.Errorf("quote destination asset: %w", err)
	}
	q := &p.resp.Quote
	in, err := parseTokenAmount(q.AmountIn)
	if err != nil || in.Cmp(p.net) != 0 {
		return nil, fmt.Errorf("the quote prices %s, not the %s withdrawn", q.AmountIn, p.net)
	}
	units := &p.wallet.Info().UnitInfo
	if p.atomsOut, err = fromIntentsAmount(q.AmountOut, units, p.dest.Decimals); err != nil {
		return nil, err
	}
	if p.minAtomsOut, err = fromIntentsAmount(q.MinAmountOut, units, p.dest.Decimals); err != nil {
		return nil, err
	}
	if dry {
		return p, nil
	}
	if q.DepositAddress == "" {
		return nil, errors.New("committed quote carries no deposit address")
	}
	if q.DepositMemo != "" {
		return nil, errors.New("memo-mode deposits are not supported")
	}
	p.params = &rhea.WithdrawParams{
		MCA:            p.mca,
		TokenID:        tokenID,
		ExtraDecimals:  x,
		Nonce:          p.nonce,
		BlockTime:      p.block.Timestamp,
		Deadline:       rhea.DeadlineFrom(p.block.Timestamp),
		Fee:            p.fee,
		FeeCap:         p.feeCap,
		Amount:         p.net,
		RegisterMCA:    p.registerMCA,
		DepositAddress: q.DepositAddress,
	}
	if p.business, err = rhea.BuildWithdraw(eng.dep, p.params); err != nil {
		return nil, err
	}
	return p, nil
}

// RheaWithdrawPreview previews a withdrawal of amount (token base units, or
// empty for the most available) from a market to a wallet.
func (c *Core) RheaWithdrawPreview(ctx context.Context, tokenID string, destAssetID uint32, amount string) (*RheaWithdrawPreview, error) {
	p, err := c.planWithdraw(ctx, tokenID, destAssetID, amount, true)
	if err != nil {
		return nil, err
	}
	x := p.asset.Config.ExtraDecimals
	return &RheaWithdrawPreview{
		TokenID:            tokenID,
		Symbol:             p.market.Symbol,
		Decimals:           p.decimals,
		DestAssetID:        destAssetID,
		Amount:             p.net.String(),
		MaxAmount:          p.netMax.String(),
		Fee:                p.fee.String(),
		FeeUSD:             p.feeUSD,
		FeeCap:             p.feeCap.String(),
		FeeCapSource:       p.feeCapSource,
		Supplied:           rhea.FromInternal(p.supplied, x).String(),
		AvailableLiquidity: rhea.FromInternal(p.available, x).String(),
		RegisterMCA:        p.registerMCA,
		AtomsOut:           p.atomsOut,
		MinAtomsOut:        p.minAtomsOut,
		AmountOutUSD:       p.resp.Quote.AmountOutUsd,
		TimeEstimate:       p.resp.Quote.TimeEstimate,
		Block:              rheaBlock(p.block),
	}, nil
}

// withRheaKey derives an identity's key from the password-unlocked app seed,
// runs f with it, and zeroes the seed and key.
func (c *Core) withRheaKey(crypter encrypt.Crypter, id rheaIdentity, f func(*rhea.Key) error) error {
	if crypter == nil {
		return errors.New("the app password is required to sign for the lending account")
	}
	creds := c.creds()
	if creds == nil {
		return errors.New("no credentials stored")
	}
	appSeed, err := crypter.Decrypt(creds.EncSeed)
	if err != nil {
		return fmt.Errorf("app seed decryption error: %w", err)
	}
	evmSeed, _ := AssetSeedAndPass(rheaEVMSeedAssetID, appSeed)
	encode.ClearBytes(appSeed)
	defer encode.ClearBytes(evmSeed)
	key, err := rhea.DeriveKey(id.Version, evmSeed)
	if err != nil {
		return err
	}
	defer key.Zero()
	if key.Identity() != id.Identity {
		return fmt.Errorf("derived identity %s differs from %s", key.Identity(), id.Identity)
	}
	return f(key)
}

// checkPendingBusiness refuses a business for an MCA that already has one
// that may still be admitted. Checked when planning (the admission gate)
// and again under opMtx immediately before the operation is persisted.
func (e *rheaEngine) checkPendingBusiness(mca string) error {
	active, err := e.db.ActiveRheaOps()
	if err != nil {
		return fmt.Errorf("error loading lending operations: %w", err)
	}
	for _, op := range active {
		if op.MCA == mca && db.RheaPendingBusinessStates[op.State] {
			return fmt.Errorf("operation %s may still be admitted (%s)", op.ID, op.State)
		}
	}
	return nil
}

// RheaWithdrawExecute signs and submits a withdrawal. It is bound to the
// preview the caller displayed: the committed quote must guarantee at least
// minAtomsOut, and the relayer fee may not exceed maxFee. Signing uses the
// lending identity, so the app password is always required. The signed
// request is persisted before it is submitted.
func (c *Core) RheaWithdrawExecute(pw []byte, tokenID string, destAssetID uint32, amount string, minAtomsOut uint64, maxFee string) (*RheaOp, error) {
	if minAtomsOut == 0 {
		return nil, errors.New("no previewed minimum output supplied")
	}
	feeBound, err := parseTokenAmount(maxFee)
	if err != nil {
		return nil, errors.New("no previewed fee supplied")
	}
	if len(pw) == 0 {
		return nil, errors.New("the app password is required to sign for the lending account")
	}
	crypter, err := c.encryptionKey(pw)
	if err != nil {
		return nil, fmt.Errorf("password error: %w", err)
	}
	defer crypter.Close()

	ctx := c.ctx
	p, err := c.planWithdraw(ctx, tokenID, destAssetID, amount, false)
	if err != nil {
		return nil, err
	}
	if p.fee.Cmp(feeBound) > 0 {
		return nil, fmt.Errorf("the relayer fee rose to %s since the preview; preview again", p.fee)
	}
	if p.minAtomsOut < minAtomsOut {
		units := &p.wallet.Info().UnitInfo
		return nil, fmt.Errorf("the rate moved: the committed quote guarantees only %s, below the previewed minimum of %s; preview again",
			units.FormatAtoms(p.minAtomsOut), units.FormatAtoms(minAtomsOut))
	}
	eng := p.eng
	opID := hex.EncodeToString(encode.RandomBytes(16))
	eng.log.Infof("Fee cap for %s %s in the %s market: relayer fee %s, cap %s, priced by %s at %v USD per %s",
		db.RheaOpWithdraw, opID, p.market.Symbol, p.fee, p.feeCap, p.feeCapSource, p.feeCapPrice, p.market.Symbol)
	var req *rhea.SignedRequest
	if err := c.withRheaKey(crypter, p.identity, func(k *rhea.Key) error {
		var err error
		req, err = k.SignWithdraw(eng.dep, p.business, p.params, eng.now())
		return err
	}); err != nil {
		return nil, err
	}
	reqJSON, err := req.MarshalJSON()
	if err != nil {
		return nil, err
	}
	envelope, err := p.resp.Envelope()
	if err != nil {
		return nil, fmt.Errorf("error persisting quote envelope: %w", err)
	}
	q := &p.resp.Quote
	legDeadline, err := time.Parse(time.RFC3339, q.Deadline)
	if err != nil {
		legDeadline = time.Now().Add(intentsDeadline(rheaNEARChainID))
	}
	op := &RheaOp{
		ID:                opID,
		Kind:              db.RheaOpWithdraw,
		State:             db.RheaStateSigned,
		AccountManager:    eng.dep.AccountManager,
		MCA:               p.mca,
		Identity:          string(p.identity.Identity),
		DerivationVersion: uint32(p.identity.Version),
		TokenID:           tokenID,
		WalletAssetID:     destAssetID,
		Amount:            p.net.String(),
		Fee:               p.fee.String(),
		GrossInternal:     rhea.ToInternal(new(big.Int).Add(p.net, p.fee), p.asset.Config.ExtraDecimals).String(),
		Leg: &db.RheaLeg{
			DepositAddress: q.DepositAddress,
			OriginAsset:    p.req.OriginAsset,
			DestAsset:      p.dest.AssetID,
			AmountIn:       q.AmountIn,
			AmountOut:      q.AmountOut,
			MinAmountOut:   q.MinAmountOut,
			Recipient:      p.req.Recipient,
			RefundTo:       p.mca,
			Deadline:       legDeadline,
			QuoteEnvelope:  envelope,
			Status:         IntentsStateQuoted,
		},
		Relayer: &db.RheaRelayerLeg{
			SignedRequest: reqJSON,
			Nonce:         p.nonce,
			Deadline:      p.params.Deadline,
		},
		USDOut:    parseUSD(q.AmountOutUsd),
		CreatedAt: time.Now(),
	}
	// One business per MCA in flight: recheck under opMtx, which every
	// operation write holds, so two concurrent withdrawals cannot both be
	// persisted and submitted.
	eng.opMtx.Lock()
	if err := eng.checkPendingBusiness(p.mca); err != nil {
		eng.opMtx.Unlock()
		return nil, err
	}
	if err := eng.db.StoreRheaOp(op); err != nil {
		eng.opMtx.Unlock()
		return nil, fmt.Errorf("error persisting lending operation: %w", err)
	}
	eng.opMtx.Unlock()
	eng.log.Infof("Signed withdrawal %s: %s %s to %s (fee %s, nonce %d, deadline %s)",
		op.ID, p.net, p.market.Symbol, unbip(destAssetID), p.fee, p.nonce, p.params.Deadline.UTC().Format(time.RFC3339))

	op = eng.submit(ctx, op, req)
	c.notify(newRheaOpNote(op, "Withdrawal submitted",
		fmt.Sprintf("Withdrawing %s from the %s lending market to %s.", p.net, p.market.Symbol, strings.ToUpper(unbip(destAssetID))), db.Poke))
	eng.pokeOps()
	return op, nil
}

// submit sends a persisted signed request to the relayer and records the
// outcome: SUBMITTED with the batch ID, or SUBMIT_UNKNOWN with the error,
// which is not evidence that nothing reached the chain.
func (e *rheaEngine) submit(ctx context.Context, op *RheaOp, req *rhea.SignedRequest) *RheaOp {
	batch, err := e.submitter.Submit(ctx, op.MCA, req)
	updated, uerr := e.updateOp(op.ID, func(o *RheaOp) bool {
		if !db.RheaPendingBusinessStates[o.State] {
			return false
		}
		if err != nil {
			o.State = db.RheaStateSubmitUnknown
			o.Relayer.RelayerError = err.Error()
			return true
		}
		o.State = db.RheaStateSubmitted
		o.Relayer.BatchID = batch
		o.Relayer.RelayerError = ""
		return true
	})
	if uerr != nil {
		e.log.Errorf("Error recording the submission of %s: %v", op.ID, uerr)
	}
	if err != nil {
		e.log.Warnf("Submission of withdrawal %s has an unknown outcome: %v", op.ID, err)
	} else {
		e.log.Infof("Withdrawal %s submitted: batch %s", op.ID, batch)
	}
	if updated != nil {
		return updated
	}
	return op
}

// rheaOurExec is an exec that carries an operation's exact signed business.
type rheaOurExec struct {
	txHash string
	tree   *rhea.TxTree
	call   *rhea.ExecCall
}

// failed reports whether the exec's own receipt failed: the MCA rejected the
// business, which consumed no nonce. An outcome not yet known is not a
// failure.
func (x *rheaOurExec) failed() bool {
	return x.call.Outcome != nil && x.call.Outcome.Status.Kind == rhea.StatusFailure
}

// findOurExec reads a transaction and returns the exec on the operation's
// MCA that carries its exact signature and business, and the nonces of the
// admitted businesses the transaction executed on that MCA. A status that
// describes another transaction than the one asked for is an error.
func (e *rheaEngine) findOurExec(ctx context.Context, txHash string, op *RheaOp, sr *rhea.SignedRequest) (*rheaOurExec, []uint64, error) {
	raw, err := e.chain.TxStatus(ctx, txHash, e.dep.Relayer, near.TxWaitNone)
	if err != nil {
		return nil, nil, err
	}
	tree, err := rhea.DecodeTxTree(raw)
	if err != nil {
		return nil, nil, err
	}
	if tree.Hash != txHash {
		return nil, nil, fmt.Errorf("status for %s describes %s", txHash, tree.Hash)
	}
	calls, err := tree.ExecCalls(e.dep)
	if err != nil {
		return nil, nil, err
	}
	want, err := sr.Business.Canonical()
	if err != nil {
		return nil, nil, err
	}
	var found *rheaOurExec
	var nonces []uint64
	for _, c := range calls {
		if c.MCA != op.MCA {
			continue
		}
		if c.Outcome != nil && c.Outcome.Status.OK() {
			if n, err := c.Args.Business.NonceValue(); err == nil {
				nonces = append(nonces, n)
			}
		}
		if c.Args.Signature != sr.Signature {
			continue
		}
		got, err := c.Args.Business.Canonical()
		if err != nil || string(got) != string(want) {
			continue
		}
		if found != nil {
			return nil, nil, fmt.Errorf("transaction %s carries the business twice", txHash)
		}
		found = &rheaOurExec{txHash: txHash, tree: tree, call: c}
	}
	return found, nonces, nil
}

// searchExec looks through the MCA's relayer history for an exec carrying
// the operation's signed business. History is newest first, so the search
// stops at an admitted exec older than the operation's nonce.
func (e *rheaEngine) searchExec(ctx context.Context, op *RheaOp, sr *rhea.SignedRequest) (*rheaOurExec, error) {
	seen := make(map[string]bool)
	received := 0
	for page := 1; page <= rheaExecSearchRecords/rheaHistoryPageSize; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hp, err := e.history.History(ctx, op.MCA, page, rheaHistoryPageSize)
		if err != nil {
			return nil, fmt.Errorf("relayer history unavailable: %w", err)
		}
		if len(hp.Records) > rheaHistoryPageSize {
			return nil, fmt.Errorf("history page %d has %d records, more than requested", page, len(hp.Records))
		}
		fresh := 0
		for _, rec := range hp.Records {
			if received++; received > rheaExecSearchRecords {
				return nil, nil
			}
			key := rheaHistoryKey(rec)
			if seen[key] {
				continue
			}
			seen[key] = true
			fresh++
			if rec.Type != rhea.HistoryTypeRelayer {
				continue
			}
			x, nonces, err := e.findOurExec(ctx, rec.RequestHash, op, sr)
			if err != nil {
				continue
			}
			// A failed exec of the same business (an identical resubmission
			// rejected after the original executed) was not admitted; keep
			// looking for the admitted one.
			if x != nil && !x.failed() {
				return x, nil
			}
			for _, n := range nonces {
				if n < op.Relayer.Nonce {
					return nil, nil
				}
			}
		}
		if len(hp.Records) == 0 || fresh == 0 || page >= hp.TotalPage {
			return nil, nil
		}
	}
	return nil, nil
}

// pollWithdraw advances a withdrawal or sweep: submission until an exec
// carrying its business is attributed, then its 1Click leg. A sweep into
// supply has no leg and ends with its exec attribution.
func (e *rheaEngine) pollWithdraw(ctx context.Context, op *RheaOp) error {
	if op.Relayer == nil || (op.Leg == nil && op.Kind != db.RheaOpSweep) {
		return errors.New("withdrawal without a signed request or 1Click leg")
	}
	switch op.State {
	case db.RheaStateSigned, db.RheaStateSubmitted, db.RheaStateSubmitUnknown, db.RheaStateNonceConsumedUnattributed:
		return e.resolveSubmission(ctx, op)
	}
	if op.Kind == db.RheaOpSweep && op.State == db.RheaStateExecuted {
		return e.reattributeSweep(ctx, op)
	}
	if op.Leg == nil {
		return fmt.Errorf("sweep into supply in state %s", op.State)
	}
	if op.State == intents.StatusRefunded {
		return e.attributeWithdrawRefund(ctx, op)
	}
	return e.pollWithdrawLeg(ctx, op)
}

// resolveSubmission determines what became of a signed withdrawal. An exec
// carrying the exact signed business, found through the relayer's batch or
// history and read from the chain, is attributed. Without one, the final
// chain state decides: a consumed nonce with no attributable exec is
// NONCE_CONSUMED_UNATTRIBUTED; an unconsumed nonce at a final block past the
// deadline plus a margin is NOT_EXECUTED (flagged unconfirmed pending the
// funded U5 check); before the deadline an unknown submission is resubmitted
// identically, which cannot execute twice.
func (e *rheaEngine) resolveSubmission(ctx context.Context, op *RheaOp) error {
	sr, err := rhea.ParseSignedRequest(op.Relayer.SignedRequest)
	if err != nil {
		return fmt.Errorf("persisted signed request: %w", err)
	}
	candidates := []string{}
	if op.Relayer.TxHash != "" {
		candidates = append(candidates, op.Relayer.TxHash)
	}
	batchDone := false
	if op.Relayer.BatchID != "" {
		reqs, err := e.submitter.BatchStatus(ctx, op.Relayer.BatchID)
		if err != nil {
			e.log.Meter("batch:"+op.ID, 10*time.Minute).Warnf("Relayer batch status for %s unavailable: %v", op.ID, err)
		}
		for _, r := range reqs {
			if r.Result != nil && r.Result.TxHash != "" && r.Result.TxHash != op.Relayer.TxHash {
				candidates = append(candidates, r.Result.TxHash)
			}
			if r.BatchStatus == rhea.BatchComplete {
				batchDone = true
				if r.Result != nil && r.Result.TxHash == "" && (r.Result.TxErrMsg != "" || r.Result.OtherErrMsg != "") {
					msg := strings.TrimSpace(r.Result.TxErrMsg + " " + r.Result.OtherErrMsg)
					_, _ = e.updateOp(op.ID, func(o *RheaOp) bool {
						if o.State != db.RheaStateSubmitted {
							return false
						}
						o.State = db.RheaStateSubmitUnknown
						o.Relayer.RelayerError = msg
						return true
					})
				}
			}
		}
	}
	if fresh, err := e.db.RheaOp(op.ID); err == nil {
		op = fresh
	}
	if !resolvingSubmission(op) {
		return nil
	}
	for _, h := range candidates {
		x, _, err := e.findOurExec(ctx, h, op, sr)
		if err != nil {
			e.log.Meter("exec:"+op.ID+h, 10*time.Minute).Warnf("Exec %s for withdrawal %s unreadable: %v", h, op.ID, err)
			continue
		}
		if x != nil && !x.failed() {
			return e.attributeExec(ctx, op, x)
		}
	}

	block, err := e.chain.FinalBlock(ctx)
	if err != nil {
		return err
	}
	nonce, _, err := e.reader.Nonce(ctx, op.MCA, block.Hash)
	if err != nil {
		return err
	}
	switch {
	case nonce > op.Relayer.Nonce:
		// The history search backs off while it keeps finding nothing.
		if op.State == db.RheaStateNonceConsumedUnattributed && !e.attributionDue(op.ID) {
			return nil
		}
		x, err := e.searchExec(ctx, op, sr)
		if err != nil {
			return err
		}
		if x != nil && !x.failed() {
			e.clearAttributionBackoff(op.ID)
			return e.attributeExec(ctx, op, x)
		}
		e.deferAttribution(op.ID)
		updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
			if o.State == db.RheaStateNonceConsumedUnattributed || !resolvingSubmission(o) {
				return false
			}
			o.State = db.RheaStateNonceConsumedUnattributed
			o.Notes = fmt.Sprintf("nonce %d was consumed by an exec that does not carry this signature, or one not yet found", o.Relayer.Nonce)
			return true
		})
		if err != nil {
			return err
		}
		if updated != nil {
			e.log.Warnf("Withdrawal %s: %s", updated.ID, updated.Notes)
			e.notifyWithdraw(updated)
		}
		return nil
	case nonce < op.Relayer.Nonce:
		return fmt.Errorf("the MCA's nonce %d is below the signed nonce %d", nonce, op.Relayer.Nonce)
	}
	// The nonce is unconsumed at the final block.
	if block.Timestamp.After(op.Relayer.Deadline.Add(rheaNotExecutedMargin)) {
		updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
			if !resolvingSubmission(o) || o.State == db.RheaStateNonceConsumedUnattributed {
				return false
			}
			o.State = db.RheaStateNotExecuted
			o.Relayer.Unconfirmed = true
			o.Notes = fmt.Sprintf("nonce %d unconsumed at final block %d, past the deadline", o.Relayer.Nonce, block.Height)
			return true
		})
		if err != nil {
			return err
		}
		if updated != nil {
			e.log.Infof("Withdrawal %s not executed: %s", updated.ID, updated.Notes)
			e.notifyWithdraw(updated)
		}
		return nil
	}
	if !block.Timestamp.Before(op.Relayer.Deadline) {
		return nil // past the deadline, inside the margin: wait
	}
	resubmit := op.State == db.RheaStateSigned || op.State == db.RheaStateSubmitUnknown ||
		(op.State == db.RheaStateSubmitted && batchDone && len(candidates) == 0)
	if !resubmit || !e.resubmitDue(op.ID) {
		return nil
	}
	e.log.Infof("Resubmitting withdrawal %s (identical signed request)", op.ID)
	e.submit(ctx, op, sr)
	return nil
}

// resolvingSubmission reports whether an operation is still waiting for its
// business's outcome.
func resolvingSubmission(op *RheaOp) bool {
	return db.RheaPendingBusinessStates[op.State] || op.State == db.RheaStateNonceConsumedUnattributed
}

// resubmitDue rate-limits identical resubmissions per operation.
func (e *rheaEngine) resubmitDue(id string) bool {
	e.attrMtx.Lock()
	defer e.attrMtx.Unlock()
	key := "resubmit:" + id
	if next, ok := e.attrNext[key]; ok && e.now().Before(next) {
		return false
	}
	e.attrNext[key] = e.now().Add(rheaResubmitInterval)
	return true
}

// attributeExec records the outcome of an exec that carries the operation's
// business. The tree must be final and fully reported; until it is, the
// exec is recorded and the operation waits. Evidence is read only from the
// exec's own receipt subtree, so sibling work in the same transaction is
// never credited to it:
//
//   - sent: NEP-141 transfers from the MCA to the deposit address. Any sent
//     amount funds the 1Click leg, which is then tracked whatever else
//     failed (DELIVERING); with nothing sent the operation is EXEC_FAILED.
//   - the fee: a transfer from Burrow to the relayer of the fee, credited
//     only with Burrow's withdraw_succeeded for the MCA of the fee in
//     internal units.
//   - tokens left in the MCA: transfers from Burrow into the MCA (the
//     withdrawal may withdraw less than its maximum) less what was sent,
//     recorded as STRANDED funds. Burrow's withdraw_succeeded events must
//     account for the fee and everything transferred into the MCA;
//     otherwise the discrepancy is noted.
func (e *rheaEngine) attributeExec(ctx context.Context, op *RheaOp, x *rheaOurExec) error {
	complete := x.tree.Complete()
	if complete == nil {
		complete = x.tree.EvidenceAvailable()
	}
	if complete != nil || x.call.Outcome == nil || !x.call.Outcome.Status.OK() {
		if complete == nil {
			complete = errors.New("the exec outcome is not known")
		}
		_, err := e.updateOp(op.ID, func(o *RheaOp) bool {
			if !resolvingSubmission(o) || o.Relayer.TxHash == x.txHash {
				return false
			}
			o.Relayer.TxHash = x.txHash
			o.Notes = fmt.Sprintf("exec %s found; receipts pending: %v", x.txHash, complete)
			return true
		})
		return err
	}
	if op.Kind == db.RheaOpSweep {
		return e.attributeSweepExec(ctx, op, x)
	}
	sub := x.tree.Subtree(x.call.ReceiptID)
	xfers, err := x.tree.FTTransfers()
	if err != nil {
		return err
	}
	evs, err := x.tree.BurrowEvents(e.dep.Burrow)
	if err != nil {
		return err
	}
	fee, err := parseTokenAmount(op.Fee)
	if err != nil {
		return fmt.Errorf("bad persisted fee: %w", err)
	}
	asset, _, err := e.reader.Asset(ctx, op.TokenID, "")
	if err != nil {
		return fmt.Errorf("market %s: %w", op.TokenID, err)
	}
	xd := asset.Config.ExtraDecimals
	sum := func(from, to string) (*big.Int, int) {
		v, n := new(big.Int), 0
		for _, t := range xfers {
			if sub[t.ReceiptID] && t.Token == op.TokenID && t.From == from && t.To == to {
				v.Add(v, t.Amount)
				n++
			}
		}
		return v, n
	}
	sent, _ := sum(op.MCA, op.Leg.DepositAddress)
	inMCA, _ := sum(e.dep.Burrow, op.MCA)
	feeMoved, feeTransfers := sum(e.dep.Burrow, e.dep.Relayer)
	succeeded := new(big.Int)
	feeSucceeded := false
	for _, ev := range evs {
		if !sub[ev.ReceiptID] || ev.Event != rhea.BurrowEventWithdrawSucceeded || ev.AccountID != op.MCA || ev.TokenID != op.TokenID {
			continue
		}
		succeeded.Add(succeeded, ev.Amount)
		if ev.Amount.Cmp(rhea.ToInternal(fee, xd)) == 0 {
			feeSucceeded = true
		}
	}
	feePaid := feeTransfers == 1 && feeMoved.Cmp(fee) == 0 && feeSucceeded
	var notes []string
	notes = append(notes, fmt.Sprintf("fee paid %t, withdrawn into the account %s, sent %s", feePaid, inMCA, sent))
	accounted := rhea.ToInternal(inMCA, xd)
	if feePaid {
		accounted.Add(accounted, rhea.ToInternal(fee, xd))
	}
	if succeeded.Cmp(accounted) != 0 {
		notes = append(notes, fmt.Sprintf("Burrow recorded %s internal units withdrawn, transfers account for %s", succeeded, accounted))
	}
	left := new(big.Int).Sub(inMCA, sent)
	if left.Sign() < 0 {
		notes = append(notes, fmt.Sprintf("%s more was sent than withdrawn, from the account's own balance", new(big.Int).Neg(left)))
		left.SetInt64(0)
	}
	updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
		if !resolvingSubmission(o) {
			return false
		}
		o.Relayer.TxHash = x.txHash
		if feePaid {
			o.Fees = fee.String()
		}
		if left.Sign() > 0 {
			o.Refunds = append(o.Refunds, db.RheaRefund{Account: o.MCA, Amount: left.String()})
			o.Stranded = sumRefunds(o.Refunds, o.MCA).String()
			notes = append(notes, fmt.Sprintf("%s left in the account", left))
		}
		o.Notes = strings.Join(notes, "; ")
		if sent.Sign() > 0 {
			o.State = db.RheaStateDelivering
			o.Leg.Status = IntentsStateDepositSent
			o.Leg.DepositTxID = x.txHash
			return true
		}
		o.State = db.RheaStateExecFailed
		return true
	})
	if err != nil {
		return err
	}
	if updated != nil {
		e.log.Infof("Withdrawal %s exec %s: %s (%s)", updated.ID, x.txHash, updated.State, updated.Notes)
		e.notifyWithdraw(updated)
		if updated.State == db.RheaStateDelivering {
			return e.pollWithdrawLeg(ctx, updated)
		}
	}
	return nil
}

// sumRefunds totals the refunds recorded into an account.
func sumRefunds(refunds []db.RheaRefund, account string) *big.Int {
	v := new(big.Int)
	for _, r := range refunds {
		if r.Account != account {
			continue
		}
		if a, err := parseTokenAmount(r.Amount); err == nil {
			v.Add(v, a)
		}
	}
	return v
}

// withdrawLeg returns a withdrawal's 1Click leg. Its state is the leg
// status, separate from the operation state.
func withdrawLeg(op *RheaOp) *intentsLeg {
	return &intentsLeg{
		State:        op.Leg.Status,
		Inferred:     op.Leg.Inferred,
		InferredFrom: op.Leg.InferredFrom,
		Deadline:     op.Leg.Deadline,
		DepositTxID:  op.Leg.DepositTxID,
		RefundReason: op.Leg.RefundReason,
	}
}

// setWithdrawLeg copies a leg back to its withdrawal and derives the
// operation state: SUCCESS is WITHDRAWN, a refund is REFUNDED until the
// refund into the MCA is attributed, FAILED is FAILED, and anything else is
// DELIVERING.
func setWithdrawLeg(op *RheaOp, l *intentsLeg) {
	op.Leg.Status = l.State
	op.Leg.Inferred = l.Inferred
	op.Leg.InferredFrom = l.InferredFrom
	op.Leg.RefundReason = l.RefundReason
	switch l.State {
	case intents.StatusSuccess:
		op.State = db.RheaStateWithdrawn
	case intents.StatusRefunded:
		op.State = intents.StatusRefunded
	case intents.StatusFailed:
		op.State = intents.StatusFailed
	default:
		op.State = db.RheaStateDelivering
	}
}

// pollWithdrawLeg advances a withdrawal's 1Click leg under the shared leg
// rules. The deposit was made by the MCA, so there is no wallet deposit to
// check for rejection.
func (e *rheaEngine) pollWithdrawLeg(ctx context.Context, op *RheaOp) error {
	if withdrawLeg(op).retireDue(e.now()) {
		_, err := e.updateOp(op.ID, func(o *RheaOp) bool {
			l := withdrawLeg(o)
			if !l.retire() {
				return false
			}
			setWithdrawLeg(o, l)
			return true
		})
		return err
	}
	ieng, err := e.oneClick()
	if err != nil {
		return err
	}
	status, err := ieng.client.Status(ctx, op.Leg.DepositAddress)
	if err != nil && !errors.Is(err, intents.ErrSwapNotFound) {
		return err
	}
	if err == nil && status.Status != intents.StatusPendingDeposit {
		updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
			l := withdrawLeg(o)
			if !l.canAdopt(status.Status) {
				return false
			}
			hashes := status.SwapDetails.NearTxHashes
			if l.State == status.Status && !l.Inferred && sameStrings(hashes, o.Leg.NearTxHashes) {
				return false
			}
			l.adopt(status.Status)
			if r := status.SwapDetails.RefundReason; r != "" {
				l.RefundReason = r
			}
			setWithdrawLeg(o, l)
			o.Leg.NearTxHashes = hashes
			return true
		})
		if err != nil {
			return err
		}
		if updated != nil {
			e.notifyWithdraw(updated)
			switch updated.State {
			case db.RheaStateWithdrawn:
				if e.c != nil {
					e.c.updateAssetBalance(updated.WalletAssetID)
				}
				return nil
			case intents.StatusRefunded:
				return e.attributeWithdrawRefund(ctx, updated)
			}
		}
	}
	var outcome legTimeout
	updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
		if o.State != db.RheaStateDelivering {
			return false
		}
		l := withdrawLeg(o)
		if outcome = l.applyTimeout(e.now()); outcome == legTimeoutNone {
			return false
		}
		setWithdrawLeg(o, l)
		return true
	})
	if err != nil {
		return err
	}
	if updated != nil {
		e.log.Warnf("Withdrawal %s leg timed out: %s (%s)", updated.ID, updated.State, updated.Leg.RefundReason)
		e.notifyWithdraw(updated)
	}
	return nil
}

// attributeWithdrawRefund attributes a refunded withdrawal leg. 1Click
// refunds to the MCA; the refund is STRANDED funds only on positive evidence
// that it was paid to the MCA and stayed there:
//
//   - 1Click reports the refunded amount;
//   - every reported transaction is established (hash-bound, final, fully
//     reported), except the withdrawal's own exec, whose role is known;
//   - exactly one verifier payout (dip4 ft_withdraw) of the token to the
//     MCA, for the refunded amount, made as a plain transfer (no message,
//     so no MCA code ran on receipt);
//   - the NEP-141 transfer from the verifier to the MCA for that amount in
//     the payout's own receipt subtree;
//   - no transfer of the token out of the MCA in any reported transaction.
//
// Until then the operation stays REFUNDED and active, retried with backoff.
func (e *rheaEngine) attributeWithdrawRefund(ctx context.Context, op *RheaOp) error {
	ieng, err := e.oneClick()
	if err != nil {
		return err
	}
	status, err := ieng.client.Status(ctx, op.Leg.DepositAddress)
	if err != nil {
		return err
	}
	hashes := status.SwapDetails.NearTxHashes
	if !sameStrings(hashes, op.Leg.NearTxHashes) {
		e.clearAttributionBackoff(op.ID)
		if updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
			o.Leg.NearTxHashes = hashes
			return true
		}); err == nil && updated != nil {
			op = updated
		}
	}
	if !e.attributionDue(op.ID) {
		return nil
	}
	reason, amount := e.refundEvidence(ctx, op, hashes, status.SwapDetails.RefundedAmount)
	updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
		if o.State != intents.StatusRefunded {
			return false
		}
		if reason != "" {
			if o.Notes == reason {
				return false
			}
			o.Notes = reason
			return true
		}
		o.State = db.RheaStateStranded
		o.Refunds = append(o.Refunds, db.RheaRefund{Account: o.MCA, Amount: amount.String()})
		o.Stranded = sumRefunds(o.Refunds, o.MCA).String()
		o.Notes = fmt.Sprintf("1Click refunded %s into the account", amount)
		return true
	})
	if err != nil {
		return err
	}
	if reason != "" {
		e.deferAttribution(op.ID)
	} else {
		e.clearAttributionBackoff(op.ID)
	}
	if updated != nil && updated.State == db.RheaStateStranded {
		e.log.Infof("Withdrawal %s refund attributed: %s", updated.ID, updated.Notes)
		e.notifyWithdraw(updated)
	}
	return nil
}

// refundEvidence checks the refund evidence described at
// attributeWithdrawRefund. It returns the refunded amount, or the reason
// the refund cannot be attributed yet.
func (e *rheaEngine) refundEvidence(ctx context.Context, op *RheaOp, hashes []string, refunded string) (string, *big.Int) {
	amount, err := parseTokenAmount(refunded)
	if err != nil || amount.Sign() == 0 {
		return "1Click reported no refunded amount", nil
	}
	type payout struct {
		tree *rhea.TxTree
		w    *rhea.VerifierWithdrawal
	}
	var payouts []payout
	var trees []*rhea.TxTree
	seen := map[string]bool{}
	for _, h := range hashes {
		if seen[h] || h == op.Relayer.TxHash {
			continue
		}
		seen[h] = true
		raw, err := e.chain.TxStatus(ctx, h, rheaVerifier, near.TxWaitNone)
		if err != nil {
			return fmt.Sprintf("reported NEAR transaction %s could not be read: %v", h, err), nil
		}
		tree, err := rhea.DecodeTxTree(raw)
		if err != nil {
			return fmt.Sprintf("reported NEAR transaction %s is malformed: %v", h, err), nil
		}
		if tree.Hash != h {
			return fmt.Sprintf("the status for %s describes %s", h, tree.Hash), nil
		}
		if err := tree.Complete(); err != nil {
			return fmt.Sprintf("reported NEAR transaction %s is not final: %v", h, err), nil
		}
		if err := tree.EvidenceAvailable(); err != nil {
			return fmt.Sprintf("reported NEAR transaction %s: %v", h, err), nil
		}
		ws, err := tree.VerifierWithdrawals(rheaVerifier)
		if err != nil {
			return fmt.Sprintf("reported NEAR transaction %s: %v", h, err), nil
		}
		for _, w := range ws {
			if w.ReceiverID == op.MCA && w.Token == op.TokenID {
				payouts = append(payouts, payout{tree: tree, w: w})
			}
		}
		trees = append(trees, tree)
	}
	switch {
	case len(payouts) == 0:
		return "no reported NEAR transaction shows the refund paid to the account", nil
	case len(payouts) > 1:
		return fmt.Sprintf("%d payouts to the account; the refund is ambiguous", len(payouts)), nil
	}
	p := payouts[0]
	if p.w.Amount.Cmp(amount) != 0 {
		return fmt.Sprintf("the payout of %s does not match the refunded %s", p.w.Amount, amount), nil
	}
	if p.w.Msg != "" {
		return "the refund was paid with a message, which runs the account's code; not attributed", nil
	}
	sub := p.tree.Subtree(p.w.ReceiptID)
	xfers, err := p.tree.FTTransfers()
	if err != nil {
		return fmt.Sprintf("transfers unreadable: %v", err), nil
	}
	in := 0
	for _, t := range xfers {
		if sub[t.ReceiptID] && t.Token == op.TokenID && t.From == rheaVerifier && t.To == op.MCA && t.Amount.Cmp(amount) == 0 {
			in++
		}
	}
	if in != 1 {
		return "the payout's transfer into the account is not in its receipts", nil
	}
	for _, tree := range trees {
		xs, err := tree.FTTransfers()
		if err != nil {
			return fmt.Sprintf("transfers unreadable: %v", err), nil
		}
		for _, t := range xs {
			if t.Token == op.TokenID && t.From == op.MCA {
				return fmt.Sprintf("the refund moved on from the account (%s to %s)", t.Amount, t.To), nil
			}
		}
	}
	return "", amount
}

func (e *rheaEngine) notifyWithdraw(op *RheaOp) {
	kind := "Withdrawal"
	if op.Kind == db.RheaOpSweep {
		kind = "Sweep"
	}
	subject, severity := kind+" update", db.Data
	switch op.State {
	case db.RheaStateWithdrawn:
		subject, severity = "Withdrawn", db.Success
	case db.RheaStateSupplied:
		subject, severity = "Supplied", db.Success
	case db.RheaStatePartial:
		subject, severity = kind+" partly supplied", db.WarningLevel
	case db.RheaStateNotExecuted:
		subject, severity = kind+" not executed", db.WarningLevel
	case db.RheaStateExecFailed, db.RheaStateNonceConsumedUnattributed, intents.StatusFailed:
		subject, severity = kind+" needs attention", db.ErrorLevel
	case db.RheaStateStranded, intents.StatusRefunded:
		subject, severity = kind+" refunded to the account", db.WarningLevel
	}
	e.notify(newRheaOpNote(op, subject, fmt.Sprintf("%s %s: %s", kind, op.ID,
		strings.ToLower(strings.ReplaceAll(op.State, "_", " "))), severity))
}
