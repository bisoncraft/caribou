// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// RHEA lending supply: a 1Click swap from a wallet asset to the market token
// on NEAR, delivered to the MCA (or, to create the MCA, to the account
// manager) with a recipient message that makes the account manager's
// pinned template supply it to Burrow. The 1Click leg follows the shared
// leg rules (intents-leg.go); the outcome is then attributed from the
// receipt trees of the NEAR transactions 1Click reports, never from
// balance changes.

package core

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	// NoteTypeRheaOp identifies lending operation notifications.
	NoteTypeRheaOp = "rheaop"
	// TopicRheaOpUpdate is emitted whenever a lending operation changes.
	TopicRheaOpUpdate Topic = "rheaopupdate"

	// rheaOpPollInterval is the cadence of the operation poller while any
	// operation is active.
	rheaOpPollInterval = 15 * time.Second
	// rheaVerifier is the intents verifier, the signer of 1Click delivery
	// transactions on NEAR.
	rheaVerifier = "intents.near"
)

// A creation supply's guaranteed delivery (the quote's minAmountOut) must
// reach the creation minimum: rheaCreationFeeMultiple times the creation
// fee, or the token amount worth rheaCreationMinMicroUSD at the market's
// fiat rate, whichever is more. The fee term leaves a supply at least the
// size of the fee at the delivery floor and absorbs a fee change before
// delivery; the USD term keeps an account from being created for a supply
// worth a few cents. Prices come from Caribou's fiat rates
// (marketUSDRate), without which no supply is planned.
const (
	rheaCreationFeeMultiple = 2
	rheaCreationMinMicroUSD = 5_000_000 // 5 USD
)

// RheaOpNote reports a new or updated lending operation.
type RheaOpNote struct {
	db.Notification
	Op *RheaOp `json:"op"`
}

func newRheaOpNote(op *RheaOp, subject, details string, severity db.Severity) *RheaOpNote {
	return &RheaOpNote{
		Notification: db.NewNotification(NoteTypeRheaOp, TopicRheaOpUpdate, subject, details, severity),
		Op:           op,
	}
}

// RheaSupplyPreview is a dry-quote preview of a supply. Token amounts are
// market-token base units.
type RheaSupplyPreview struct {
	SourceAssetID uint32 `json:"sourceAssetID"`
	TokenID       string `json:"tokenID"`
	Symbol        string `json:"symbol"`
	Decimals      int    `json:"decimals"`
	AtomsIn       uint64 `json:"atomsIn"`
	AmountInUSD   string `json:"amountInUSD"`
	// Delivered is the quoted delivery; MinDelivered the least a solver
	// may deliver, the floor execute is bound to.
	Delivered    string `json:"delivered"`
	MinDelivered string `json:"minDelivered"`
	AmountOutUSD string `json:"amountOutUSD"`
	// Creating is set when no MCA exists and the supply creates one.
	// CreationFee is then the one-time account creation fee, deducted from
	// the delivery and shown as its own line; MinSupplied is the supply
	// left at the delivery floor after it. Execute is bound to
	// CreationFee.Amount (maxCreationFee). CreationMinimum is the least
	// MinDelivered a creating supply accepts.
	Creating        bool                 `json:"creating"`
	MCA             string               `json:"mca,omitempty"`
	CreationFee     *RheaFeeLine         `json:"creationFee,omitempty"`
	CreationMinimum *RheaCreationMinimum `json:"creationMinimum,omitempty"`
	MinSupplied     string               `json:"minSupplied"`
	SupplyAPR       string               `json:"supplyAPR"`
	AppFees         []intents.AppFee     `json:"appFees"`
	TimeEstimate    int64                `json:"timeEstimate"`
	Block           RheaBlock            `json:"block"`
}

// RheaCreationMinimum is the least guaranteed delivery an account-creating
// supply accepts: twice the creation fee, or the token amount worth 5 USD
// at Caribou's fiat rate, whichever is more.
type RheaCreationMinimum struct {
	// Amount is in base units of the market token.
	Amount string `json:"amount"`
	// USD values Amount at the fiat rate.
	USD string `json:"usd"`
}

// rheaSupplyPlan is a supply resolved against chain state, the catalog,
// and a quote.
type rheaSupplyPlan struct {
	eng          *rheaEngine
	ieng         *intentsEngine
	market       *rhea.Market
	block        *near.BlockHeader
	identity     rheaIdentity
	mca          string // empty when creating
	creating     bool
	creationCost *rheaCreationCost
	creationFee  *big.Int
	creationMin  *big.Int
	// usdRate is the market token's fiat rate (USD per whole token).
	usdRate      float64
	recipient    string
	msg          string
	asset        *rhea.Asset
	wallet       *xcWallet
	source       *intents.Token
	dest         *intents.Token
	req          *intents.QuoteRequest
	resp         *intents.QuoteResponse
	delivered    *big.Int
	minDelivered *big.Int
}

// placeholderCreationSig stands in for the creation signature in a dry
// quote, which needs no password. It has the real signature's length, so
// the message priced is the size of the one executed.
var placeholderCreationSig = strings.Repeat("0", 130)

// planSupply resolves a supply: the account (existing, or created by this
// supply), the pinned templates, the market's deposit state and supply
// limit, the creation fee and minimum, and a verified quote. creationSig
// returns the creation signature for an identity, called only when
// creating.
func (c *Core) planSupply(ctx context.Context, sourceAssetID uint32, tokenID string, atoms uint64, dry bool,
	creationSig func(rheaIdentity) (string, error)) (*rheaSupplyPlan, error) {

	eng, err := c.rheaEngine()
	if err != nil {
		return nil, err
	}
	ieng, err := eng.oneClick()
	if err != nil {
		return nil, err
	}
	p := &rheaSupplyPlan{eng: eng, ieng: ieng}
	if p.market = eng.dep.Market(tokenID); p.market == nil {
		return nil, fmt.Errorf("%s is not a supported lending market", tokenID)
	}
	if p.usdRate, err = eng.marketUSDRate(p.market); err != nil {
		return nil, err
	}
	if atoms == 0 {
		return nil, errors.New("cannot supply zero")
	}
	ids := eng.identities()
	if len(ids) == 0 {
		return nil, errors.New("log in to supply")
	}
	if p.block, err = eng.chain.FinalBlock(ctx); err != nil {
		return nil, fmt.Errorf("error reading the final block: %w", err)
	}
	B := p.block.Hash

	disc := eng.discover(ctx, ids, B)
	switch disc.Status {
	case RheaFound:
		p.mca, p.recipient = disc.found.MCA, disc.found.MCA
		p.identity = rheaIdentity{Version: rhea.DerivationVersion(disc.found.Version), Identity: rhea.Identity(disc.found.Identity)}
	case RheaNone:
		p.creating = true
		p.identity = ids[len(ids)-1] // the newest derivation version
		p.recipient = eng.dep.AccountManager
		if err := eng.corroborateNoMCA(ctx, p.identity.Identity, B); err != nil {
			return nil, fmt.Errorf("cannot create a lending account: %w", err)
		}
	default:
		return nil, fmt.Errorf("lending account discovery is %s", disc.Status)
	}
	if p.creating {
		if err := eng.checkCreationGuard(p.identity); err != nil {
			return nil, err
		}
	}

	eng.viewed.Store(true) // supplying is lending use; see engaged
	if _, err := eng.reader.CheckSupplyTemplates(ctx, B); err != nil {
		return nil, fmt.Errorf("supply templates: %w", err)
	}
	if p.asset, _, err = eng.reader.Asset(ctx, tokenID, B); err != nil {
		return nil, err
	}
	if !p.asset.Config.CanDeposit {
		return nil, fmt.Errorf("the %s market is not accepting deposits", p.market.Symbol)
	}
	if p.creating {
		if p.creationCost, err = eng.creationCost(ctx, tokenID, B); err != nil {
			return nil, err
		}
		p.creationFee = p.creationCost.total()
	}

	cat, err := ieng.freshCatalog(ctx)
	if err != nil {
		return nil, err
	}
	if p.source = resolveIntentsAsset(cat, sourceAssetID); p.source == nil {
		return nil, fmt.Errorf("%s is not supported for cross-chain swaps", unbip(sourceAssetID))
	}
	if p.dest = cat.ByAssetID(p.market.OneClickAssetID); p.dest == nil {
		return nil, fmt.Errorf("the %s market token is not in the swap catalog", p.market.Symbol)
	}
	if err := verifyMarketAsset(cat, p.dest.AssetID, p.market); err != nil {
		return nil, err
	}
	var found bool
	if p.wallet, found = c.wallet(sourceAssetID); !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(sourceAssetID))
	}

	switch {
	case !p.creating:
		p.msg = rhea.SupplyMsg(p.identity.Identity)
	case dry:
		p.msg = (&rhea.RecipientMsg{
			Wallets:    []rhea.Wallet{p.identity.Identity.Wallet()},
			Template:   rhea.TemplateBurrowRegisterAndSupply,
			Signatures: []string{placeholderCreationSig},
		}).String()
	default:
		sig, err := creationSig(p.identity)
		if err != nil {
			return nil, err
		}
		if p.msg, err = rhea.RegisterAndSupplyMsg(p.identity.Identity, sig); err != nil {
			return nil, err
		}
	}

	amount, err := toIntentsAmount(atoms, &p.wallet.Info().UnitInfo, p.source.Decimals)
	if err != nil {
		return nil, err
	}
	refundTo, err := intentsWalletAddress(p.wallet)
	if err != nil {
		return nil, fmt.Errorf("error getting refund address: %w", err)
	}
	// The supply carries the default application fee: the request sets no
	// fees of its own, so the client attaches its configured ones.
	p.req = &intents.QuoteRequest{
		Dry:                dry,
		SwapType:           intents.SwapTypeExactInput,
		SlippageTolerance:  intentsDefaultSlippageBps,
		OriginAsset:        p.source.AssetID,
		DepositType:        intents.LocationOriginChain,
		DestinationAsset:   p.dest.AssetID,
		Amount:             amount,
		RefundTo:           refundTo,
		RefundType:         intents.LocationOriginChain,
		Recipient:          p.recipient,
		RecipientType:      intents.LocationDestinationChain,
		Deadline:           time.Now().Add(intentsDeadline(sourceAssetID)).UTC().Format("2006-01-02T15:04:05.000Z"),
		CustomRecipientMsg: p.msg,
	}
	if p.resp, err = ieng.client.Quote(ctx, p.req); err != nil {
		return nil, err
	}
	if err := verifyQuoteAsset(cat, p.resp.Request.OriginAsset, sourceAssetID); err != nil {
		return nil, fmt.Errorf("quote origin asset: %w", err)
	}
	if err := verifyMarketAsset(cat, p.resp.Request.DestinationAsset, p.market); err != nil {
		return nil, fmt.Errorf("quote destination asset: %w", err)
	}
	if p.delivered, err = parseTokenAmount(p.resp.Quote.AmountOut); err != nil {
		return nil, fmt.Errorf("bad quoted amountOut: %w", err)
	}
	if p.minDelivered, err = parseTokenAmount(p.resp.Quote.MinAmountOut); err != nil {
		return nil, fmt.Errorf("bad quoted minAmountOut: %w", err)
	}
	if p.minDelivered.Cmp(p.delivered) > 0 {
		return nil, errors.New("quote's minimum delivery exceeds its delivery")
	}

	if headroom := p.asset.SupplyHeadroom(); headroom != nil {
		if rhea.ToInternal(p.delivered, p.asset.Config.ExtraDecimals).Cmp(headroom) > 0 {
			return nil, fmt.Errorf("the %s market's supply limit leaves room for only %s", p.market.Symbol,
				rhea.FromInternal(headroom, p.asset.Config.ExtraDecimals))
		}
	}
	if p.creating {
		if p.creationMin, err = creationMinimum(p.creationFee, p.dest.Decimals, p.usdRate); err != nil {
			return nil, err
		}
		if p.minDelivered.Cmp(p.creationMin) < 0 {
			return nil, p.creationMinimumError()
		}
	}
	return p, nil
}

// creationMinimum returns the creation minimum for a creation fee in a
// token of decimals at price (USD per whole token).
func creationMinimum(fee *big.Int, decimals int, price float64) (*big.Int, error) {
	rate, ok := rheaPrice(price)
	if !ok || decimals < 0 {
		return nil, fmt.Errorf("no usable USD rate (%v) for the creation minimum", price)
	}
	least := new(big.Int).Mul(fee, big.NewInt(rheaCreationFeeMultiple))
	// The token amount worth the USD floor at the exact rate, rounded up so
	// the minimum is never worth less than the floor.
	if floor := rheaUSDToUnits(rheaCreationMinMicroUSD, decimals, rate, true); floor.Cmp(least) > 0 {
		least = floor
	}
	return least, nil
}

// creationMinimumError names the creation minimum a quote's guaranteed
// delivery falls short of.
func (p *rheaSupplyPlan) creationMinimumError() error {
	sym, d := p.market.Symbol, p.dest.Decimals
	return fmt.Errorf("a supply that creates the lending account must guarantee a delivery of at least %s %s ($%s), "+
		"the larger of twice the %s %s account creation fee and $%d; this quote guarantees %s %s; supply more",
		formatBaseUnits(p.creationMin, d), sym, rheaUSD(p.creationMin, d, p.usdRate),
		formatBaseUnits(p.creationFee, d), sym, rheaCreationMinMicroUSD/1_000_000,
		formatBaseUnits(p.minDelivered, d), sym)
}

// formatBaseUnits renders base units of a token with decimals as a decimal
// amount with trailing zeros trimmed.
func formatBaseUnits(v *big.Int, decimals int) string {
	if decimals <= 0 {
		return v.String()
	}
	q, r := new(big.Int).QuoRem(v, pow10Big(decimals), new(big.Int))
	if r.Sign() == 0 {
		return q.String()
	}
	frac := r.String()
	frac = strings.Repeat("0", decimals-len(frac)) + frac
	return q.String() + "." + strings.TrimRight(frac, "0")
}

func pow10Big(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// minSupplied is the supply left at the delivery floor.
func (p *rheaSupplyPlan) minSupplied() *big.Int {
	if !p.creating {
		return new(big.Int).Set(p.minDelivered)
	}
	v := new(big.Int).Sub(p.minDelivered, p.creationFee)
	if v.Sign() < 0 {
		v.SetInt64(0)
	}
	return v
}

func parseTokenAmount(s string) (*big.Int, error) {
	if s == "" {
		return nil, errors.New("empty amount")
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Sign() < 0 {
		return nil, fmt.Errorf("bad amount %q", s)
	}
	return v, nil
}

// verifyMarketAsset checks that an Intents asset ID resolves, through the
// catalog, to the market token held on NEAR. The service normalizes asset
// IDs between schemes, and the catalog lists some tokens under more than
// one chain (ZEC on Zcash and on NEAR), so the entry's chain and contract
// are both checked.
func verifyMarketAsset(cat *intents.Catalog, assetID string, m *rhea.Market) error {
	tok := cat.ByAssetID(assetID)
	if tok == nil || tok.Blockchain != "near" || tok.ContractAddress != m.TokenID {
		return fmt.Errorf("%q is not %s on NEAR", assetID, m.TokenID)
	}
	return nil
}

// corroborateNoMCA requires every configured endpoint to answer null for the
// identity at the named block before an MCA is created. A disagreement,
// error, or unreachable endpoint blocks creation: a second creation for an
// identity already bound would take the account manager's refund path.
func (e *rheaEngine) corroborateNoMCA(ctx context.Context, id rhea.Identity, blockHash string) error {
	args, err := json.Marshal(map[string]rhea.Wallet{"wallet": id.Wallet()})
	if err != nil {
		return err
	}
	res, err := e.chain.CallFunctionAll(ctx, e.dep.AccountManager, "get_mca_by_wallet", args, blockHash)
	if err != nil {
		return fmt.Errorf("absence of an account is not corroborated: %w", err)
	}
	if len(res) == 0 {
		return errors.New("absence of an account is not corroborated: no endpoints")
	}
	for _, r := range res {
		if r.BlockHash != blockHash || !bytes.Equal(bytes.TrimSpace(r.Result), []byte("null")) {
			return errors.New("an endpoint reports an account for this identity")
		}
	}
	return nil
}

// checkCreationGuard refuses a second creation supply for an identity
// while an earlier one is unresolved locally. It is checked when planning
// and again under opMtx immediately before the operation is persisted.
func (e *rheaEngine) checkCreationGuard(id rheaIdentity) error {
	active, err := e.db.ActiveRheaOps()
	if err != nil {
		return fmt.Errorf("error loading lending operations: %w", err)
	}
	for _, op := range active {
		if op.Kind == db.RheaOpSupply && op.Creation && op.Identity == string(id.Identity) {
			return fmt.Errorf("an account creation supply (%s) is still in progress", op.ID)
		}
	}
	return nil
}

// rheaCreationCost is the token cost of creating an MCA from a supply, in
// its two parts: the account manager's fixed creation fee
// (get_create_mca_fee) and the token value of the new account's initial
// NEAR balance (get_mca_init_balance at get_near_value, rounded up).
type rheaCreationCost struct {
	fixed   *big.Int
	funding *big.Int
	// fundingNEAR is the initial NEAR balance funded, in yoctoNEAR.
	fundingNEAR *big.Int
}

func (c *rheaCreationCost) total() *big.Int {
	return new(big.Int).Add(c.fixed, c.funding)
}

// creationCost reads the creation cost of an MCA for a supply of tokenID
// at blockHash.
func (e *rheaEngine) creationCost(ctx context.Context, tokenID, blockHash string) (*rheaCreationCost, error) {
	fee, _, err := e.reader.CreateMCAFee(ctx, tokenID, blockHash)
	if err != nil {
		return nil, fmt.Errorf("account creation fee: %w", err)
	}
	nearValue, _, err := e.reader.NearValue(ctx, tokenID, blockHash)
	if err != nil {
		return nil, fmt.Errorf("near value: %w", err)
	}
	if nearValue.Sign() <= 0 {
		return nil, errors.New("near value is not positive")
	}
	initBalance, _, err := e.reader.MCAInitBalance(ctx, blockHash)
	if err != nil {
		return nil, fmt.Errorf("account initial balance: %w", err)
	}
	return &rheaCreationCost{
		fixed:       fee,
		funding:     ceilDivBig(new(big.Int).Mul(initBalance, nearValue), yoctoPerNEARBig),
		fundingNEAR: initBalance,
	}, nil
}

// RheaFeeLine is a fee presented as its own line, never folded into
// another amount. Amounts are base units of the fee's token.
type RheaFeeLine struct {
	TokenID  string `json:"tokenID"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
	// Amount is the fee; empty when Unavailable.
	Amount string `json:"amount,omitempty"`
	// USD values Amount at Caribou's fiat rate; empty when Unavailable.
	USD string `json:"usd"`
	// OneTime marks a fee charged once (account creation), not with every
	// operation.
	OneTime    bool                `json:"oneTime"`
	Components []*RheaFeeComponent `json:"components,omitempty"`
	// Unavailable explains why the fee could not be read, or why the
	// market is unavailable (no current USD rate).
	Unavailable string `json:"unavailable,omitempty"`
	// CreationMinimum is the least guaranteed delivery of a first supply
	// to the market. It is set on the account view's creation fee lines;
	// the supply preview carries its own.
	CreationMinimum *RheaCreationMinimum `json:"creationMinimum,omitempty"`
}

// Fee component kinds.
const (
	// RheaFeeFixed is the account manager's fixed creation fee
	// (get_create_mca_fee).
	RheaFeeFixed = "fixed"
	// RheaFeeAccountFunding is the new account's initial NEAR balance,
	// paid in the token at the account manager's near value.
	RheaFeeAccountFunding = "accountFunding"
)

// RheaFeeComponent is one part of a fee line, in the line's token.
type RheaFeeComponent struct {
	Kind   string `json:"kind"`
	Amount string `json:"amount"`
	USD    string `json:"usd"`
	// NEAR is the NEAR amount an accountFunding component funds, in
	// yoctoNEAR.
	NEAR string `json:"near,omitempty"`
}

// creationFeeLine presents a creation cost as a one-time fee line. price
// is the token's fiat rate (USD per whole token).
func creationFeeLine(m *rhea.Market, decimals int, c *rheaCreationCost, price float64) *RheaFeeLine {
	usd := func(amt *big.Int) string {
		return rheaUSD(amt, decimals, price)
	}
	total := c.total()
	return &RheaFeeLine{
		TokenID:  m.TokenID,
		Symbol:   m.Symbol,
		Decimals: decimals,
		Amount:   total.String(),
		USD:      usd(total),
		OneTime:  true,
		Components: []*RheaFeeComponent{
			{Kind: RheaFeeFixed, Amount: c.fixed.String(), USD: usd(c.fixed)},
			{Kind: RheaFeeAccountFunding, Amount: c.funding.String(), USD: usd(c.funding), NEAR: c.fundingNEAR.String()},
		},
	}
}

// rheaNoRateError refuses a supply for want of a current USD rate for
// symbol.
func rheaNoRateError(symbol string) error {
	return fmt.Errorf("no current USD rate for %s; supplying is unavailable until rates refresh", symbol)
}

// tickerRate returns Caribou's fiat rate (USD per whole unit) for a
// normalized ticker, which is absent when no rate source has a fresh value
// for any registered asset of the ticker. ok is false without a usable
// rate.
func (e *rheaEngine) tickerRate(ticker string) (rate float64, ok bool) {
	if e.tickerRates == nil || ticker == "" {
		return 0, false
	}
	if rate, ok = e.tickerRates()[ticker]; !ok {
		return 0, false
	}
	if _, ok := rheaPrice(rate); !ok {
		return 0, false
	}
	return rate, true
}

// marketUSDRate returns Caribou's fiat rate for one whole market token, the
// rate of the market's FiatTicker, or rheaNoRateError without a usable one.
// Supplies require it; withdrawals and sweeps prefer it for their fee caps
// (feeCapPrice); reconciliation and recovery never read it.
func (e *rheaEngine) marketUSDRate(m *rhea.Market) (float64, error) {
	if rate, ok := e.tickerRate(m.FiatTicker); ok {
		return rate, nil
	}
	return 0, rheaNoRateError(m.Symbol)
}

// rheaUSD values amount (base units of a token with decimals) at price
// (USD per whole token) as a two-decimal USD string, rounded to the
// nearest cent, or "" when the price is unusable.
func rheaUSD(amount *big.Int, decimals int, price float64) string {
	if amount == nil || decimals < 0 {
		return ""
	}
	rate, ok := rheaPrice(price)
	if !ok {
		return ""
	}
	// cents = amount × rate × 100 / 10^decimals, rounded half up.
	num := new(big.Int).Mul(amount, rate.Num())
	num.Mul(num, big.NewInt(100))
	den := new(big.Int).Mul(pow10Big(decimals), rate.Denom())
	num.Add(num, new(big.Int).Quo(den, big.NewInt(2)))
	cents := num.Quo(num, den)
	dollars, c := new(big.Int).QuoRem(cents, big.NewInt(100), new(big.Int))
	return fmt.Sprintf("%s.%02d", dollars, c.Int64())
}

// rheaPrice returns the exact rational value of a price (USD per whole
// token), reporting false when the price is not finite or lies outside one
// micro-USD to 1e12 USD. Conversions built on it are exact up to their
// final rounding, so a rounding direction chosen for a bound holds at the
// price itself.
func rheaPrice(price float64) (*big.Rat, bool) {
	if math.IsNaN(price) || price < 1e-6 || price > 1e12 {
		return nil, false
	}
	return new(big.Rat).SetFloat64(price), true
}

// rheaUSDToUnits converts microUSD to base units of a token with decimals
// at rate (from rheaPrice), rounding up when up and down otherwise.
func rheaUSDToUnits(microUSD int64, decimals int, rate *big.Rat, up bool) *big.Int {
	// units = microUSD × 10^decimals / (10^6 × rate)
	num := new(big.Int).Mul(big.NewInt(microUSD), pow10Big(decimals))
	num.Mul(num, rate.Denom())
	den := new(big.Int).Mul(big.NewInt(1_000_000), rate.Num())
	if up {
		return ceilDivBig(num, den)
	}
	return num.Quo(num, den)
}

var yoctoPerNEARBig = new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)

func ceilDivBig(a, b *big.Int) *big.Int {
	q, m := new(big.Int).DivMod(a, b, new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// RheaSupplyPreview obtains a dry quote for a supply of atoms of the
// source wallet asset to a market.
func (c *Core) RheaSupplyPreview(ctx context.Context, sourceAssetID uint32, tokenID string, atoms uint64) (*RheaSupplyPreview, error) {
	p, err := c.planSupply(ctx, sourceAssetID, tokenID, atoms, true, nil)
	if err != nil {
		return nil, err
	}
	q := &p.resp.Quote
	atomsIn, err := fromIntentsAmount(q.AmountIn, &p.wallet.Info().UnitInfo, p.source.Decimals)
	if err != nil {
		return nil, err
	}
	pv := &RheaSupplyPreview{
		SourceAssetID: sourceAssetID,
		TokenID:       tokenID,
		Symbol:        p.market.Symbol,
		Decimals:      p.dest.Decimals,
		AtomsIn:       atomsIn,
		AmountInUSD:   q.AmountInUsd,
		Delivered:     p.delivered.String(),
		MinDelivered:  p.minDelivered.String(),
		AmountOutUSD:  q.AmountOutUsd,
		Creating:      p.creating,
		MCA:           p.mca,
		MinSupplied:   p.minSupplied().String(),
		SupplyAPR:     p.asset.SupplyAPR,
		AppFees:       p.resp.Request.AppFees,
		TimeEstimate:  q.TimeEstimate,
		Block:         rheaBlock(p.block),
	}
	if p.creating {
		pv.CreationFee = creationFeeLine(p.market, p.dest.Decimals, p.creationCost, p.usdRate)
		pv.CreationMinimum = &RheaCreationMinimum{
			Amount: p.creationMin.String(),
			USD:    rheaUSD(p.creationMin, p.dest.Decimals, p.usdRate),
		}
	}
	return pv, nil
}

// rheaCreationSig signs the creation message for an identity with the key
// derived from the password-unlocked app seed.
func (c *Core) rheaCreationSig(crypter encrypt.Crypter, id rheaIdentity) (string, error) {
	if crypter == nil {
		return "", errors.New("the app password is required to create the lending account")
	}
	var sig string
	err := c.withRheaKey(crypter, id, func(k *rhea.Key) error {
		var err error
		sig, err = k.SignCreation()
		return err
	})
	return sig, err
}

// RheaSupplyExecute obtains a committed quote and funds it. It is bound to
// the terms of the preview the caller displayed: the committed quote's
// minimum delivery must clear minDelivered, the account state must be the
// one previewed (creating or not), and a creation fee may not exceed
// maxCreationFee. Planning again requires a current fiat rate, and the
// creation minimum is re-checked against the committed quote at that rate,
// before anything is persisted or sent. Creating the account signs with
// the lending identity, so it requires the app password whether or not
// sends do.
func (c *Core) RheaSupplyExecute(pw []byte, sourceAssetID uint32, tokenID string, atoms uint64, minDelivered string, creating bool, maxCreationFee string) (*RheaOp, error) {
	floor, err := parseTokenAmount(minDelivered)
	if err != nil || floor.Sign() == 0 {
		return nil, errors.New("no previewed minimum delivery supplied")
	}
	var maxFee *big.Int
	if creating {
		if maxFee, err = parseTokenAmount(maxCreationFee); err != nil {
			return nil, errors.New("no previewed creation fee supplied")
		}
	}
	var crypter encrypt.Crypter
	if len(pw) > 0 {
		if crypter, err = c.encryptionKey(pw); err != nil {
			return nil, fmt.Errorf("password error: %w", err)
		}
		defer crypter.Close()
	}
	if creating && crypter == nil {
		return nil, errors.New("the app password is required to create the lending account")
	}

	ctx := c.ctx
	p, err := c.planSupply(ctx, sourceAssetID, tokenID, atoms, false, func(id rheaIdentity) (string, error) {
		return c.rheaCreationSig(crypter, id)
	})
	if err != nil {
		return nil, err
	}
	if p.creating != creating {
		return nil, errors.New("the lending account changed since the preview; preview again")
	}
	if p.creating && p.creationFee.Cmp(maxFee) > 0 {
		return nil, fmt.Errorf("the account creation fee rose to %s since the preview; preview again", p.creationFee)
	}
	if p.minDelivered.Cmp(floor) < 0 {
		return nil, fmt.Errorf("the rate moved: the committed quote guarantees only %s, below the previewed minimum of %s; preview again",
			p.minDelivered, floor)
	}
	quote := &p.resp.Quote
	if quote.DepositAddress == "" {
		return nil, errors.New("committed quote carries no deposit address")
	}
	if quote.DepositMemo != "" {
		return nil, errors.New("memo-mode deposits are not supported")
	}
	units := &p.wallet.Info().UnitInfo
	committedIn, err := fromIntentsAmountCeil(quote.AmountIn, units, p.source.Decimals)
	if err != nil {
		return nil, fmt.Errorf("bad committed amountIn: %w", err)
	}
	if committedIn > atoms {
		return nil, fmt.Errorf("the rate moved: the committed quote requires %s, above the authorized input of %s; preview again",
			units.FormatAtoms(committedIn), units.FormatAtoms(atoms))
	}
	if err := c.connectAndUnlock(crypter, p.wallet); err != nil {
		return nil, err
	}
	if err := p.wallet.checkPeersAndSyncStatus(); err != nil {
		return nil, err
	}
	feeRate, err := c.legDepositFeeRate(p.wallet, quote.DepositAddress, atoms)
	if err != nil {
		return nil, err
	}
	envelope, err := p.resp.Envelope()
	if err != nil {
		return nil, fmt.Errorf("error persisting quote envelope: %w", err)
	}
	deadline, err := time.Parse(time.RFC3339, quote.Deadline)
	if err != nil {
		p.eng.log.Warnf("Unparseable quote deadline %q: %v", quote.Deadline, err)
		deadline = time.Now().Add(intentsDeadline(sourceAssetID))
	}

	op := &RheaOp{
		ID:                hex.EncodeToString(encode.RandomBytes(16)),
		Kind:              db.RheaOpSupply,
		State:             IntentsStateQuoted,
		AccountManager:    p.eng.dep.AccountManager,
		MCA:               p.mca,
		Identity:          string(p.identity.Identity),
		DerivationVersion: uint32(p.identity.Version),
		TokenID:           tokenID,
		WalletAssetID:     sourceAssetID,
		Amount:            p.delivered.String(),
		Creation:          p.creating,
		MinDelivered:      p.minDelivered.String(),
		Leg: &db.RheaLeg{
			DepositAddress:     quote.DepositAddress,
			OriginAsset:        p.source.AssetID,
			DestAsset:          p.dest.AssetID,
			AmountIn:           quote.AmountIn,
			AmountOut:          quote.AmountOut,
			MinAmountOut:       quote.MinAmountOut,
			Recipient:          p.recipient,
			CustomRecipientMsg: p.msg,
			RefundTo:           p.req.RefundTo,
			Deadline:           deadline,
			QuoteEnvelope:      envelope,
			Status:             IntentsStateQuoted,
		},
		USDIn:     parseUSD(quote.AmountInUsd),
		USDOut:    parseUSD(quote.AmountOutUsd),
		CreatedAt: time.Now(),
	}
	if p.creating {
		op.CreationFee = p.creationFee.String()
	}
	eng := p.eng
	_, err = p.ieng.fundLeg(ctx, &legFunding{
		wallet:         p.wallet,
		depositAddress: quote.DepositAddress,
		atoms:          atoms,
		feeRate:        feeRate,
		persist: func() error {
			eng.opMtx.Lock()
			defer eng.opMtx.Unlock()
			// Every operation write holds opMtx, so rechecking the creation
			// guard here makes check-and-store atomic: of two concurrent
			// creation supplies for one identity, only the first to persist
			// is funded.
			if op.Creation {
				if err := eng.checkCreationGuard(p.identity); err != nil {
					return err
				}
			}
			if err := eng.db.StoreRheaOp(op); err != nil {
				return fmt.Errorf("error persisting lending operation: %w", err)
			}
			eng.log.Infof("Committed supply %s: %s to %s %s (guaranteed delivery %s), deposit address %s, deadline %s",
				op.ID, units.FormatAtoms(atoms), p.market.Symbol, p.recipient, p.minDelivered,
				quote.DepositAddress, deadline.Format(time.RFC3339))
			return nil
		},
		recordSent: func(txID string, fee uint64, feeUSD float64) {
			eng.opMtx.Lock()
			defer eng.opMtx.Unlock()
			fresh, err := eng.db.RheaOp(op.ID)
			if err != nil {
				// The deposit moved; record it on the local copy.
				eng.log.Errorf("Error reloading lending operation %s after deposit: %v", op.ID, err)
				fresh = op
			}
			if fresh.State == IntentsStateQuoted {
				fresh.State = IntentsStateDepositSent
				fresh.Leg.Status = IntentsStateDepositSent
			}
			if fresh.Leg.DepositTxID == "" {
				fresh.Leg.DepositTxID = txID
			}
			if fresh.Leg.DepositTxFee == 0 {
				fresh.Leg.DepositTxFee, fresh.Leg.USDDepositFee = fee, feeUSD
			}
			if err := eng.db.StoreRheaOp(fresh); err != nil {
				eng.log.Errorf("Error storing lending operation %s after deposit: %v", op.ID, err)
			}
			op = fresh
		},
	})
	if err != nil {
		eng.pokeOps()
		return nil, err
	}
	eng.log.Infof("Supply %s deposit broadcast: %s tx %s", op.ID, unbip(sourceAssetID), op.Leg.DepositTxID)
	c.notify(newRheaOpNote(op, "Supply deposit sent",
		fmt.Sprintf("Supplying %s to the %s lending market. Deposit %s broadcast.",
			strings.ToUpper(unbip(sourceAssetID)), p.market.Symbol, op.Leg.DepositTxID), db.Poke))
	c.updateAssetBalance(sourceAssetID)
	eng.pokeOps()
	return op, nil
}

// opLeg returns a supply's 1Click leg. Until delivery the operation's
// state is the leg state.
func opLeg(op *RheaOp) *intentsLeg {
	return &intentsLeg{
		State:        op.State,
		Inferred:     op.Leg.Inferred,
		InferredFrom: op.Leg.InferredFrom,
		Deadline:     op.Leg.Deadline,
		DepositTxID:  op.Leg.DepositTxID,
		RefundReason: op.Leg.RefundReason,
	}
}

// setOpLeg copies a leg back to its operation. A delivered leg (1Click
// SUCCESS) moves the operation to DELIVERED for attribution.
func setOpLeg(op *RheaOp, l *intentsLeg) {
	op.Leg.Status = l.State
	op.Leg.Inferred = l.Inferred
	op.Leg.InferredFrom = l.InferredFrom
	op.Leg.RefundReason = l.RefundReason
	if l.State == intents.StatusSuccess {
		op.State = db.RheaStateDelivered
	} else {
		op.State = l.State
	}
}

// supplyAttributing reports whether a supply's leg is done and its outcome
// is being attributed from receipts.
func supplyAttributing(op *RheaOp) bool {
	return op.State == db.RheaStateDelivered || op.State == db.RheaStateDeliveryUnresolved
}

// updateOp applies f to the stored operation under opMtx and stores it when
// f reports a change. It returns the stored operation, or nil when nothing
// changed.
func (e *rheaEngine) updateOp(id string, f func(op *RheaOp) bool) (*RheaOp, error) {
	e.opMtx.Lock()
	defer e.opMtx.Unlock()
	op, err := e.db.RheaOp(id)
	if err != nil {
		return nil, err
	}
	if !f(op) {
		return nil, nil
	}
	if err := e.db.StoreRheaOp(op); err != nil {
		return nil, err
	}
	return op, nil
}

// pollOps drives every active operation and returns how many were active,
// so the caller can park the poller when none are. loaded is false when the
// active set could not be read.
func (e *rheaEngine) pollOps(ctx context.Context) (active int, loaded bool) {
	ops, err := e.db.ActiveRheaOps()
	if err != nil {
		e.log.Errorf("Error loading active lending operations: %v", err)
		return 0, false
	}
	for _, op := range ops {
		if ctx.Err() != nil {
			return active, true
		}
		active++
		var err error
		switch op.Kind {
		case db.RheaOpSupply:
			err = e.pollSupply(ctx, op)
		case db.RheaOpWithdraw, db.RheaOpSweep:
			err = e.pollWithdraw(ctx, op)
		}
		if err != nil {
			e.log.Meter("op-err:"+op.ID, 2*time.Minute).Errorf("Error refreshing lending operation %s: %v", op.ID, err)
		}
	}
	return active, true
}

// pollSupply advances a supply: its 1Click leg under the shared leg rules,
// then attribution once delivered.
func (e *rheaEngine) pollSupply(ctx context.Context, op *RheaOp) error {
	if op.Leg == nil {
		return errors.New("supply without a 1Click leg")
	}
	if supplyAttributing(op) {
		return e.pollAttributing(ctx, op)
	}
	if opLeg(op).retireDue(e.now()) {
		_, err := e.updateOp(op.ID, func(o *RheaOp) bool {
			l := opLeg(o)
			if !l.retire() {
				return false
			}
			setOpLeg(o, l)
			e.log.Infof("Supply %s retired as %s: no service status superseded the inferred state", o.ID, o.State)
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
	if err != nil || status.Status == intents.StatusPendingDeposit {
		if !e.applyOpRejection(ctx, op) {
			e.applyOpTimeout(op.ID)
		}
		return nil
	}
	updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
		l := opLeg(o)
		if supplyAttributing(o) || !l.canAdopt(status.Status) {
			return false
		}
		hashes := status.SwapDetails.NearTxHashes
		changed := l.State != status.Status || l.Inferred || len(hashes) != len(o.Leg.NearTxHashes)
		if !changed {
			return false
		}
		prev := o.State
		l.adopt(status.Status)
		if r := status.SwapDetails.RefundReason; r != "" {
			l.RefundReason = r
		}
		setOpLeg(o, l)
		o.Leg.NearTxHashes = hashes
		if o.State != prev {
			e.log.Infof("Supply %s: %s → %s", o.ID, prev, o.State)
		}
		return true
	})
	if err != nil {
		return err
	}
	if updated != nil {
		e.notifyOp(updated)
		if updated.State == intents.StatusRefunded && e.c != nil {
			e.c.updateAssetBalance(updated.WalletAssetID)
		}
		if updated.State == db.RheaStateDelivered {
			return e.attributeOp(ctx, updated)
		}
	}
	if fresh, err := e.db.RheaOp(op.ID); err == nil && !legTerminal(fresh.State) && !supplyAttributing(fresh) {
		e.applyOpTimeout(op.ID)
	}
	return nil
}

func (e *rheaEngine) notifyOp(op *RheaOp) {
	subject := "Supply update"
	severity := db.Data
	switch op.State {
	case db.RheaStateSupplied:
		subject, severity = "Supplied", db.Success
	case db.RheaStatePartial, db.RheaStateStranded:
		subject, severity = "Supply needs attention", db.WarningLevel
	case db.RheaStateDeliveryUnresolved, intents.StatusFailed:
		subject, severity = "Supply needs attention", db.ErrorLevel
	case intents.StatusRefunded:
		subject, severity = "Supply refunded", db.WarningLevel
	case IntentsStateExpired:
		subject, severity = "Supply expired", db.WarningLevel
	}
	e.notify(newRheaOpNote(op, subject, fmt.Sprintf("Supply %s: %s", op.ID,
		strings.ToLower(strings.ReplaceAll(op.State, "_", " "))), severity))
}

// applyOpRejection fails a supply whose deposit the origin chain rejected.
func (e *rheaEngine) applyOpRejection(ctx context.Context, op *RheaOp) bool {
	if e.c == nil || !e.c.depositRejected(ctx, opLeg(op), op.WalletAssetID) {
		return false
	}
	updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
		l := opLeg(o)
		if !l.applyRejection() {
			return false
		}
		setOpLeg(o, l)
		return true
	})
	if err != nil || updated == nil {
		return false
	}
	e.log.Warnf("Supply %s marked DEPOSIT_FAILED: deposit tx %s was rejected on-chain", op.ID, op.Leg.DepositTxID)
	e.notifyOp(updated)
	e.c.updateAssetBalance(op.WalletAssetID)
	return true
}

// applyOpTimeout applies the shared deadline timeout to a supply's leg.
func (e *rheaEngine) applyOpTimeout(id string) {
	var outcome legTimeout
	updated, err := e.updateOp(id, func(o *RheaOp) bool {
		if supplyAttributing(o) {
			return false
		}
		l := opLeg(o)
		if outcome = l.applyTimeout(e.now()); outcome == legTimeoutNone {
			return false
		}
		setOpLeg(o, l)
		return true
	})
	if err != nil {
		e.log.Errorf("Error applying the timeout to supply %s: %v", id, err)
		return
	}
	if updated != nil {
		e.log.Warnf("Supply %s timed out: %s (%s)", id, updated.State, updated.Leg.RefundReason)
		e.notifyOp(updated)
	}
}

// pollAttributing keeps a delivered supply's 1Click view current while its
// outcome is attributed. A corrected list of NEAR transactions replaces the
// old one and restarts attribution, which otherwise backs off while it
// stays unresolved. A 1Click refund reported after the delivery (the
// recipient rejected it and the verifier refunded the deposit, U3) ends the
// operation as REFUNDED only on positive evidence: a unique, final, fully
// readable delivery transaction whose verifier → recipient transfer was
// returned in full (RheaLeg.VerifierRefunded) and no receipts showing the
// recipient kept tokens (RheaLeg.Received). Every currently reported
// transaction is examined before the refund is considered; missing or
// unreadable evidence is never taken as proof nothing was received.
func (e *rheaEngine) pollAttributing(ctx context.Context, op *RheaOp) error {
	ieng, err := e.oneClick()
	if err != nil {
		return err
	}
	status, err := ieng.client.Status(ctx, op.Leg.DepositAddress)
	if err != nil {
		e.log.Meter("attr-status:"+op.ID, 10*time.Minute).Warnf("1Click status for supply %s unavailable: %v", op.ID, err)
		status = nil
	}
	if status != nil {
		hs := status.SwapDetails.NearTxHashes
		updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
			if !supplyAttributing(o) || len(hs) == 0 || sameStrings(hs, o.Leg.NearTxHashes) {
				return false
			}
			o.Leg.NearTxHashes = hs
			// The refund proof holds only for the transaction set it was
			// established on.
			o.Leg.VerifierRefunded = false
			return true
		})
		if err != nil {
			return err
		}
		if updated != nil {
			op = updated
			e.clearAttributionBackoff(op.ID)
		}
	}
	if e.attributionDue(op.ID) {
		if err := e.attributeOp(ctx, op); err != nil {
			return err
		}
	}
	if status == nil || status.Status != intents.StatusRefunded {
		return nil
	}
	var refunded bool
	updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
		if !supplyAttributing(o) || !o.Leg.VerifierRefunded || o.Leg.Received {
			return false
		}
		o.State = intents.StatusRefunded
		o.Leg.Status = intents.StatusRefunded
		if r := status.SwapDetails.RefundReason; r != "" {
			o.Leg.RefundReason = r
		}
		o.Notes = "the delivery was returned to the verifier and 1Click refunded the deposit"
		refunded = true
		return true
	})
	if err != nil {
		return err
	}
	if !refunded {
		if fresh, err := e.db.RheaOp(op.ID); err == nil && supplyAttributing(fresh) {
			e.log.Meter("attr-refund:"+op.ID, 30*time.Minute).Warnf(
				"1Click reports supply %s refunded, but no attributable receipts show the delivery returned to the verifier; keeping it unresolved", op.ID)
		}
		return nil
	}
	e.clearAttributionBackoff(updated.ID)
	e.log.Infof("Supply %s refunded: the verifier took the delivery back and 1Click refunded the deposit", updated.ID)
	e.notifyOp(updated)
	if e.c != nil {
		e.c.updateAssetBalance(updated.WalletAssetID)
	}
	return nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Attribution backoff: an unresolved attribution is retried after a delay
// that doubles from rheaAttrMinDelay to rheaAttrMaxDelay, and immediately
// when the transaction list changes.
const (
	rheaAttrMinDelay = 30 * time.Second
	rheaAttrMaxDelay = 10 * time.Minute
)

func (e *rheaEngine) attributionDue(id string) bool {
	e.attrMtx.Lock()
	defer e.attrMtx.Unlock()
	next, ok := e.attrNext[id]
	return !ok || !e.now().Before(next)
}

func (e *rheaEngine) deferAttribution(id string) {
	e.attrMtx.Lock()
	defer e.attrMtx.Unlock()
	d := e.attrDelay[id] * 2
	if d < rheaAttrMinDelay {
		d = rheaAttrMinDelay
	}
	if d > rheaAttrMaxDelay {
		d = rheaAttrMaxDelay
	}
	e.attrDelay[id] = d
	e.attrNext[id] = e.now().Add(d)
}

func (e *rheaEngine) clearAttributionBackoff(id string) {
	e.attrMtx.Lock()
	defer e.attrMtx.Unlock()
	delete(e.attrDelay, id)
	delete(e.attrNext, id)
}

// supplyAttribution is the outcome of a delivery read from receipts.
type supplyAttribution struct {
	State     string
	MCA       string
	Delivered *big.Int
	Fees      *big.Int
	Supplied  *big.Int
	Stranded  *big.Int
	Refunds   []db.RheaRefund
	Reason    string
	// Received is set when the receipts show the recipient kept some of
	// the delivery, which a later 1Click refund would contradict.
	Received bool
	// VerifierRefunded is set when the receipts show the verifier →
	// recipient transfer returned in full, the evidence a later 1Click
	// refund needs.
	VerifierRefunded bool
}

// supplyAttributionParams names what a delivery must show.
type supplyAttributionParams struct {
	dep           *rhea.Deployment
	verifier      string
	recipient     string // the MCA, or the account manager for a creation
	mca           string // the existing MCA, empty for a creation
	tokenID       string
	msg           string
	extraDecimals uint8
}

func unresolved(reason string, args ...any) *supplyAttribution {
	return &supplyAttribution{State: db.RheaStateDeliveryUnresolved, Reason: fmt.Sprintf(reason, args...)}
}

// attributeSupply reads a delivery's outcome from the receipt tree of the
// NEAR transaction that carried it. The tree must be final and fully
// reported. Every hop the delivery must have made has to show its
// ft_transfer_call resolution; a missing one is missing evidence, never
// full use. Outcomes:
//
//   - SUPPLIED: everything that reached the MCA was supplied, and Burrow's
//     deposit event for the MCA matches.
//   - PARTIAL: part was supplied and the rest refunded into the MCA.
//   - STRANDED: nothing was supplied and everything that reached the MCA
//     was refunded into it.
//   - DELIVERY_UNRESOLVED: anything else, including refunds that went to
//     the verifier or the account manager rather than the MCA.
func attributeSupply(tree *rhea.TxTree, p *supplyAttributionParams) *supplyAttribution {
	if err := tree.Complete(); err != nil {
		return unresolved("delivery receipts not final: %v", err)
	}
	if err := tree.EvidenceAvailable(); err != nil {
		return unresolved("delivery receipts not fully reported: %v", err)
	}
	ws, err := tree.VerifierWithdrawals(p.verifier)
	if err != nil {
		return unresolved("verifier events unreadable: %v", err)
	}
	var w *rhea.VerifierWithdrawal
	for _, x := range ws {
		if x.ReceiverID == p.recipient && x.Token == p.tokenID && x.Msg == p.msg {
			if w != nil {
				return unresolved("more than one delivery to %s in %s", p.recipient, tree.Hash)
			}
			w = x
		}
	}
	if w == nil {
		return unresolved("no delivery to %s with this operation's message in %s", p.recipient, tree.Hash)
	}
	res, err := tree.ResolvedTransfers()
	if err != nil {
		return unresolved("transfer resolutions unavailable: %v", err)
	}
	find := func(sender, receiver string) (*rhea.ResolvedTransfer, error) {
		var hit *rhea.ResolvedTransfer
		for _, r := range res {
			if r.Token == p.tokenID && r.SenderID == sender && r.ReceiverID == receiver {
				if hit != nil {
					return nil, fmt.Errorf("more than one %s → %s transfer", sender, receiver)
				}
				hit = r
			}
		}
		if hit == nil {
			return nil, fmt.Errorf("missing evidence: no resolution for the %s → %s transfer", sender, receiver)
		}
		return hit, nil
	}
	out := &supplyAttribution{Delivered: w.Amount, Fees: new(big.Int)}

	hop1, err := find(p.verifier, p.recipient)
	if err != nil {
		return unresolved("%v", err)
	}
	if hop1.Amount.Cmp(w.Amount) != 0 {
		return unresolved("delivery transfer of %s does not match the withdrawal of %s", hop1.Amount, w.Amount)
	}
	received := hop1.Used.Sign() > 0
	if r := hop1.Refunded(); r.Sign() > 0 {
		x := unresolved("%s refunded to the verifier", r)
		x.Refunds = []db.RheaRefund{{Account: p.verifier, Amount: r.String()}}
		x.Received = received
		x.VerifierRefunded = !received
		return x
	}
	out.Received = received
	// Every outcome past this point has the recipient keeping tokens.
	unresolvedReceived := func(reason string, args ...any) *supplyAttribution {
		x := unresolved(reason, args...)
		x.Received = true
		return x
	}

	mca, reached := p.mca, hop1.Used
	if p.mca == "" {
		fees, err := tree.AccountManagerFees(p.dep.AccountManager)
		if err != nil {
			return unresolvedReceived("account manager events unreadable: %v", err)
		}
		for _, f := range fees {
			if f.TokenID != p.tokenID {
				return unresolvedReceived("account manager fee in %s", f.TokenID)
			}
			out.Fees.Add(out.Fees, f.Amount)
		}
		var hop2 *rhea.ResolvedTransfer
		for _, r := range res {
			if r.Token == p.tokenID && r.SenderID == p.dep.AccountManager && p.dep.IsMCA(r.ReceiverID) {
				if hop2 != nil {
					return unresolvedReceived("more than one transfer from the account manager to an MCA")
				}
				hop2 = r
			}
		}
		if hop2 == nil {
			return unresolvedReceived("missing evidence: no resolution for the transfer to the new account")
		}
		mca = hop2.ReceiverID
		want := new(big.Int).Sub(hop1.Used, out.Fees)
		if hop2.Amount.Cmp(want) != 0 {
			return unresolvedReceived("transfer to the new account of %s does not match the delivery net of fees, %s", hop2.Amount, want)
		}
		if r := hop2.Refunded(); r.Sign() > 0 {
			x := unresolvedReceived("%s returned to the account manager", r)
			x.Refunds = []db.RheaRefund{{Account: p.dep.AccountManager, Amount: r.String()}}
			x.MCA = mca
			return x
		}
		reached = hop2.Used
	}
	out.MCA = mca
	if reached.Sign() == 0 {
		return unresolvedReceived("nothing reached the account")
	}
	hop3, err := find(mca, p.dep.Burrow)
	if err != nil {
		x := unresolvedReceived("%v", err)
		x.MCA = mca
		return x
	}
	if hop3.Amount.Cmp(reached) != 0 {
		x := unresolvedReceived("supply transfer of %s does not match the %s that reached the account", hop3.Amount, reached)
		x.MCA = mca
		return x
	}
	evs, err := tree.BurrowEvents(p.dep.Burrow)
	if err != nil {
		return unresolvedReceived("Burrow events unreadable: %v", err)
	}
	deposited := new(big.Int)
	for _, ev := range evs {
		if ev.Event == rhea.BurrowEventDeposit && ev.AccountID == mca && ev.TokenID == p.tokenID {
			deposited.Add(deposited, ev.Amount)
		}
	}
	if deposited.Cmp(rhea.ToInternal(hop3.Used, p.extraDecimals)) != 0 {
		x := unresolvedReceived("Burrow recorded a deposit of %s internal units for %s supplied", deposited, hop3.Used)
		x.MCA = mca
		return x
	}
	out.Supplied = new(big.Int).Set(hop3.Used)
	out.Stranded = hop3.Refunded()
	switch {
	case out.Stranded.Sign() == 0:
		out.State = db.RheaStateSupplied
	case out.Supplied.Sign() > 0:
		out.State = db.RheaStatePartial
		out.Refunds = []db.RheaRefund{{Account: mca, Amount: out.Stranded.String()}}
		out.Reason = fmt.Sprintf("%s supplied, %s refunded into the account", out.Supplied, out.Stranded)
	default:
		out.State = db.RheaStateStranded
		out.Refunds = []db.RheaRefund{{Account: mca, Amount: out.Stranded.String()}}
		out.Reason = fmt.Sprintf("%s refunded into the account", out.Stranded)
	}
	return out
}

// attributeOp attributes a delivered supply from the receipt trees of the
// NEAR transactions 1Click reported for it, refreshing the list from 1Click
// when it is empty.
func (e *rheaEngine) attributeOp(ctx context.Context, op *RheaOp) error {
	hashes := op.Leg.NearTxHashes
	if len(hashes) == 0 {
		ieng, err := e.oneClick()
		if err != nil {
			return err
		}
		status, err := ieng.client.Status(ctx, op.Leg.DepositAddress)
		if err != nil {
			return err
		}
		hashes = status.SwapDetails.NearTxHashes
		if len(hashes) > 0 {
			if _, err := e.updateOp(op.ID, func(o *RheaOp) bool {
				o.Leg.NearTxHashes = hashes
				return true
			}); err != nil {
				return err
			}
		}
	}
	extra := uint8(0)
	if a, _, err := e.reader.Asset(ctx, op.TokenID, ""); err == nil {
		extra = a.Config.ExtraDecimals
	} else {
		return fmt.Errorf("market %s: %w", op.TokenID, err)
	}
	params := &supplyAttributionParams{
		dep:           e.dep,
		verifier:      rheaVerifier,
		recipient:     op.Leg.Recipient,
		mca:           op.MCA,
		tokenID:       op.TokenID,
		msg:           op.Leg.CustomRecipientMsg,
		extraDecimals: extra,
	}
	if op.Creation {
		params.mca = ""
	}
	// Every reported transaction is read, strictly: the delivery must be
	// carried by exactly one of them, as a delivery of this operation's
	// token to its recipient with its message, and every other reported
	// transaction must be established as something else by a final, fully
	// reported tree. A transaction of
	// unknown role (unreadable, malformed, or a status naming a different
	// transaction than the one asked for) keeps the attribution unresolved.
	// The only transaction excluded without reading is the operation's own
	// deposit, which the wallet sent. Evidence that the recipient kept
	// tokens is taken from every candidate, even when the selection is
	// ambiguous.
	var matches []*rhea.TxTree
	var errs []string
	seen := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		if seen[h] || (op.Leg.DepositTxID != "" && h == op.Leg.DepositTxID) {
			continue
		}
		seen[h] = true
		raw, err := e.chain.TxStatus(ctx, h, rheaVerifier, near.TxWaitNone)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h, err))
			continue
		}
		tree, err := rhea.DecodeTxTree(raw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h, err))
			continue
		}
		if tree.Hash != h {
			errs = append(errs, fmt.Sprintf("%s: status describes %s", h, tree.Hash))
			continue
		}
		ws, err := tree.VerifierWithdrawals(rheaVerifier)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h, err))
			continue
		}
		carries := false
		for _, w := range ws {
			if w.ReceiverID == params.recipient && w.Token == params.tokenID && w.Msg == params.msg {
				carries = true
				break
			}
		}
		if carries {
			matches = append(matches, tree)
			continue
		}
		// Only a final, fully reported tree establishes that a transaction
		// is not the delivery: a pending one may yet log it.
		if err := tree.Complete(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: not final: %v", h, err))
			continue
		}
		if err := tree.EvidenceAvailable(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h, err))
			continue
		}
	}
	candidates := make([]*supplyAttribution, len(matches))
	received := false
	for i, m := range matches {
		candidates[i] = attributeSupply(m, params)
		received = received || candidates[i].Received
	}
	var result *supplyAttribution
	switch {
	case len(errs) > 0:
		result = unresolved("%d reported NEAR transactions could not be established (first: %s)", len(errs), errs[0])
	case len(matches) == 0:
		reason := "no reported NEAR transaction shows the delivery"
		if len(hashes) == 0 {
			reason = "1Click reported no NEAR transactions for the delivery"
		}
		result = unresolved("%s", reason)
	case len(matches) > 1:
		result = unresolved("%d reported NEAR transactions carry this delivery", len(matches))
	default:
		result = candidates[0]
	}
	result.Received = result.Received || received
	if len(matches) != 1 || len(errs) > 0 {
		result.VerifierRefunded = false
	}
	if result.State == db.RheaStateDeliveryUnresolved {
		e.deferAttribution(op.ID)
	} else {
		e.clearAttributionBackoff(op.ID)
	}
	updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
		if !supplyAttributing(o) {
			return false
		}
		changed := o.State != result.State || o.Notes != result.Reason
		o.State = result.State
		o.Notes = result.Reason
		if result.MCA != "" && o.MCA == "" {
			o.MCA = result.MCA
		}
		if result.Delivered != nil {
			o.Delivered = result.Delivered.String()
		}
		if result.Fees != nil {
			o.Fees = result.Fees.String()
		}
		if result.Supplied != nil {
			o.Supplied = result.Supplied.String()
		}
		if result.Stranded != nil && result.Stranded.Sign() > 0 {
			o.Stranded = result.Stranded.String()
		}
		if result.Received && !o.Leg.Received {
			o.Leg.Received = true
			changed = true
		}
		// The refund proof is recomputed by every attribution: an
		// unresolved or ambiguous one clears it. Received stays set.
		if o.Leg.VerifierRefunded != result.VerifierRefunded {
			o.Leg.VerifierRefunded = result.VerifierRefunded
			changed = true
		}
		o.Refunds = result.Refunds
		return changed
	})
	if err != nil {
		return err
	}
	if updated != nil {
		if updated.State == db.RheaStateDeliveryUnresolved {
			e.log.Warnf("Supply %s delivery unresolved: %s", updated.ID, updated.Notes)
		} else {
			e.log.Infof("Supply %s %s: delivered %s, fees %s, supplied %s to %s",
				updated.ID, updated.State, updated.Delivered, updated.Fees, updated.Supplied, updated.MCA)
		}
		e.notifyOp(updated)
	}
	return nil
}
