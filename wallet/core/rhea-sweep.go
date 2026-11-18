// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// RHEA lending sweep: a signed business that moves an MCA's plain token
// balance back into supply or out to a wallet through 1Click, paying the
// relayer with a native NEAR GasPayment from the MCA's own NEAR balance.
// The balance swept is whatever the MCA holds at a final block: part of it
// may be attributed to earlier operations (STRANDED, PARTIAL, EXEC_FAILED
// records), and the rest is recovered balance owned by virtue of the MCA's
// binding to the identity, origin unknown. Sweeps share the withdrawal's
// submission, exec attribution, and 1Click leg machinery (rhea-withdraw.go).

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
	"github.com/bisoncraft/caribou/wallet/asset/near"
	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/intents"
	"github.com/bisoncraft/caribou/wallet/rhea"
)

// rheaGasFiatTicker is the ticker of the sweep relayer fee's token, NEAR.
const rheaGasFiatTicker = "NEAR"

// storageAmountPerByteYocto is NEAR's storage staking price in yoctoNEAR
// per byte; an account's NEAR balance below storage_usage at this price is
// locked.
var storageAmountPerByteYocto = new(big.Int).Exp(big.NewInt(10), big.NewInt(19), nil)

// RheaSweepPreview is a preview of a sweep. Token amounts are market-token
// base units; NEAR amounts are yoctoNEAR.
type RheaSweepPreview struct {
	TokenID  string `json:"tokenID"`
	Symbol   string `json:"symbol"`
	Decimals uint8  `json:"decimals"`
	// ToSupply is set for a sweep back into supply; otherwise DestAssetID
	// is the wallet asset 1Click pays.
	ToSupply    bool   `json:"toSupply"`
	DestAssetID uint32 `json:"destAssetID,omitempty"`
	// Balance is the MCA's plain balance of the token. Attributed is the
	// part earlier operations recorded as left in the account; the rest is
	// recovered balance of unknown origin.
	Balance    string `json:"balance"`
	Attributed string `json:"attributed"`
	Recovered  string `json:"recovered"`
	Amount     string `json:"amount"`
	// Fee is the relayer's GasPayment in yoctoNEAR from the MCA's NEAR
	// balance; FeeCap the most Caribou signs, 2 USD at the NEAR price
	// FeeCapSource names (RheaFeeCapFiatRate, or RheaFeeCapCatalog for the
	// catalog's wNEAR price); FeeInToken the fee valued in the token for
	// the 2 % cap; and NEARAvailable the MCA's unlocked NEAR.
	Fee           string    `json:"fee"`
	FeeCap        string    `json:"feeCap"`
	FeeCapSource  string    `json:"feeCapSource"`
	FeeInToken    string    `json:"feeInToken"`
	NEARAvailable string    `json:"nearAvailable"`
	AtomsOut      uint64    `json:"atomsOut,omitempty"`
	MinAtomsOut   uint64    `json:"minAtomsOut,omitempty"`
	TimeEstimate  int64     `json:"timeEstimate,omitempty"`
	Block         RheaBlock `json:"block"`
}

type rheaSweepPlan struct {
	eng          *rheaEngine
	ieng         *intentsEngine
	market       *rhea.Market
	block        *near.BlockHeader
	identity     rheaIdentity
	mca          string
	nonce        uint64
	decimals     uint8
	balance      *big.Int
	attributed   *big.Int
	covers       []string
	amount       *big.Int
	fee          *big.Int
	feeCap       *big.Int
	feeCapSource string
	feeCapPrice  float64 // USD per NEAR, from feeCapSource
	feeInToken   *big.Int
	nearValue    *big.Int
	nearAvail    *big.Int
	toSupply     bool
	destAssetID  uint32
	wallet       *xcWallet
	dest         *intents.Token
	req          *intents.QuoteRequest
	resp         *intents.QuoteResponse
	atomsOut     uint64
	minAtomsOut  uint64
	params       *rhea.SweepParams
	business     *rhea.Business
}

// sweepMoved reports whether a sweep's exec moved its tokens out of the
// MCA's plain balance: supplied, or sent to its 1Click deposit address.
func sweepMoved(op *RheaOp) bool {
	return op.Kind == db.RheaOpSweep && (op.Supplied != "" || (op.Leg != nil && op.Leg.DepositTxID != ""))
}

// attributedStranded totals what operations recorded as left in the MCA's
// plain balance of a token and no sweep has since moved, with the IDs of
// those operations. A sweep that moved its tokens settles every operation
// it covers; whatever the sweep itself left in the account (a partial
// supply, a refunded leg) is its own STRANDED record.
func attributedStranded(ops []*RheaOp, mca, tokenID string) (*big.Int, []string) {
	swept := make(map[string]bool)
	for _, op := range ops {
		if sweepMoved(op) {
			for _, id := range op.Covers {
				swept[id] = true
			}
		}
	}
	v := new(big.Int)
	var ids []string
	for _, op := range ops {
		if op.MCA != mca || op.TokenID != tokenID || op.Stranded == "" || swept[op.ID] {
			continue
		}
		if a, err := parseTokenAmount(op.Stranded); err == nil && a.Sign() > 0 {
			v.Add(v, a)
			ids = append(ids, op.ID)
		}
	}
	return v, ids
}

// supplyDelivering refuses a sweep of a token while a supply of it to the
// MCA is delivered but not yet attributed: the delivery can still change
// the MCA's balance of the token.
func supplyDelivering(active []*RheaOp, mca, tokenID string) error {
	for _, op := range active {
		if op.Kind == db.RheaOpSupply && op.TokenID == tokenID && supplyAttributing(op) && (op.MCA == mca || op.MCA == "") {
			return fmt.Errorf("supply %s of this token is still being delivered", op.ID)
		}
	}
	return nil
}

// planSweep resolves a sweep of amount (token base units, empty for the
// whole plain balance) of a token in the MCA, back into supply when dest is
// nil, otherwise to the wallet asset.
func (c *Core) planSweep(ctx context.Context, tokenID string, dest *uint32, amount string, dry bool) (*rheaSweepPlan, error) {
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
	p := &rheaSweepPlan{eng: eng, ieng: ieng, toSupply: dest == nil}
	if dest != nil {
		p.destAssetID = *dest
	}
	if p.market = eng.dep.Market(tokenID); p.market == nil {
		return nil, fmt.Errorf("%s is not a supported lending market", tokenID)
	}
	ids := eng.identities()
	if len(ids) == 0 {
		return nil, errors.New("log in to sweep")
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
		return nil, fmt.Errorf("sweeps are not admitted now: %s", strings.Join(adm.Reasons, "; "))
	}
	if err := supplyDelivering(active, p.mca, tokenID); err != nil {
		return nil, err
	}

	md, _, err := eng.reader.FTMetadata(ctx, tokenID, B)
	if err != nil {
		return nil, err
	}
	p.decimals = md.Decimals
	if p.balance, _, err = eng.reader.FTBalance(ctx, tokenID, p.mca, B); err != nil {
		return nil, err
	}
	all, err := eng.db.RheaOps()
	if err != nil {
		return nil, fmt.Errorf("error loading lending operations: %w", err)
	}
	p.attributed, p.covers = attributedStranded(all, p.mca, tokenID)
	if p.attributed.Cmp(p.balance) > 0 {
		p.attributed = new(big.Int).Set(p.balance)
	}
	if amount == "" {
		p.amount = new(big.Int).Set(p.balance)
	} else if p.amount, err = parseTokenAmount(amount); err != nil {
		return nil, err
	}
	if p.amount.Sign() <= 0 {
		return nil, fmt.Errorf("the account holds no %s to sweep", p.market.Symbol)
	}
	if p.amount.Cmp(p.balance) > 0 {
		return nil, fmt.Errorf("the account holds only %s", p.balance)
	}
	// A sweep settles the attributed records only when it moves all of
	// them.
	if p.amount.Cmp(p.attributed) < 0 {
		p.covers = nil
	}
	if p.toSupply {
		asset, _, err := eng.reader.Asset(ctx, tokenID, B)
		if err != nil {
			return nil, err
		}
		if !asset.Config.CanDeposit {
			return nil, fmt.Errorf("the %s market is not accepting deposits", p.market.Symbol)
		}
		if h := asset.SupplyHeadroom(); h != nil && rhea.ToInternal(p.amount, asset.Config.ExtraDecimals).Cmp(h) > 0 {
			return nil, fmt.Errorf("the %s market's supply limit has no room for %s", p.market.Symbol, p.amount)
		}
		pos, _, err := eng.reader.AccountPositions(ctx, p.mca, B)
		if err != nil {
			return nil, err
		}
		if pos == nil {
			return nil, errors.New("the account is not registered with Burrow")
		}
	}

	// The relayer fee is a native NEAR GasPayment from the MCA's own NEAR:
	// the relayer's NEAR fee plus 0.00125 NEAR per storage deposit in the
	// business (the deposit address's registration on a sweep out, none on
	// a sweep into supply). The MCA must also cover the business's
	// attached deposits from its unlocked NEAR, without counting the
	// deposit the relayer attaches to exec, which the signature does not
	// bind.
	storage, deposits := 0, big.NewInt(1) // ft_transfer_call's one yocto
	if !p.toSupply {
		storage = 1
		deposits.SetString(rhea.StorageDeposit, 10)
		deposits.Add(deposits, big.NewInt(1)) // registration + ft_transfer
	}
	cfg, err := eng.submitter.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("relayer configuration: %w", err)
	}
	if cfg.CollectFeeAccountID != eng.dep.Relayer {
		return nil, fmt.Errorf("the relayer collects fees to %q, not %s", cfg.CollectFeeAccountID, eng.dep.Relayer)
	}
	gasFee := cfg.GasFee[rhea.GasTokenNEAR]
	if gasFee == nil {
		return nil, errors.New("the relayer publishes no NEAR fee")
	}
	if p.fee, err = rhea.RelayerFee(gasFee, yoctoPerNEARBig, storage); err != nil {
		return nil, err
	}
	acctView, err := eng.chain.ViewAccount(ctx, p.mca, B)
	if err != nil {
		return nil, err
	}
	if acctView.BlockHash != B {
		return nil, fmt.Errorf("account %s read at block %s, requested %s", p.mca, acctView.BlockHash, B)
	}
	nearBal, err := parseTokenAmount(acctView.Amount)
	if err != nil {
		return nil, fmt.Errorf("bad account balance: %w", err)
	}
	locked := new(big.Int).Mul(new(big.Int).SetUint64(acctView.StorageUsage), storageAmountPerByteYocto)
	p.nearAvail = new(big.Int).Sub(nearBal, locked)
	if p.nearAvail.Sign() < 0 {
		p.nearAvail.SetInt64(0)
	}
	if need := new(big.Int).Add(p.fee, deposits); p.nearAvail.Cmp(need) < 0 {
		return nil, fmt.Errorf("the account's unlocked NEAR (%s yocto) cannot pay the relayer fee and deposits of %s", p.nearAvail, need)
	}

	// The catalog is read when the cap needs its wNEAR price or a sweep out
	// needs a quote, at most once.
	var cat *intents.Catalog
	catalog := func() (*intents.Catalog, error) {
		if cat == nil {
			fetched, err := ieng.freshCatalog(ctx)
			if err != nil {
				return nil, fmt.Errorf("the 1Click catalog is unavailable: %w", err)
			}
			cat = fetched
		}
		return cat, nil
	}
	nearPrice, source, err := eng.feeCapPrice(rheaGasFiatTicker, "NEAR", func() (float64, error) {
		ct, err := catalog()
		if err != nil {
			return 0, err
		}
		return catalogPrice(ct, ct.ByAssetID("nep141:"+eng.dep.WrappedNEAR), "wNEAR")
	})
	if err != nil {
		return nil, err
	}
	if p.feeCap, err = rheaFeeCap(nearPrice, 24); err != nil {
		return nil, err
	}
	p.feeCapSource, p.feeCapPrice = source, nearPrice
	if p.fee.Cmp(p.feeCap) > 0 {
		return nil, fmt.Errorf("the relayer fee of %s yoctoNEAR exceeds the 2 USD cap", p.fee)
	}
	if tokenID == eng.dep.WrappedNEAR {
		p.feeInToken = new(big.Int).Set(p.fee)
	} else {
		if p.nearValue, _, err = eng.reader.NearValue(ctx, tokenID, B); err != nil {
			return nil, err
		}
		p.feeInToken = ceilDivBig(new(big.Int).Mul(p.fee, p.nearValue), yoctoPerNEARBig)
	}
	if new(big.Int).Mul(p.feeInToken, big.NewInt(50)).Cmp(p.amount) > 0 {
		return nil, fmt.Errorf("the relayer fee, worth %s, exceeds 2%% of %s", p.feeInToken, p.amount)
	}

	var depositAddress string
	if !p.toSupply {
		cat, err := catalog()
		if err != nil {
			return nil, err
		}
		mtok := cat.ByAssetID(p.market.OneClickAssetID)
		if mtok == nil {
			return nil, fmt.Errorf("the %s market token is not in the swap catalog", p.market.Symbol)
		}
		var found bool
		if p.wallet, found = c.wallet(p.destAssetID); !found {
			return nil, newError(missingWalletErr, "no wallet found for %s", unbip(p.destAssetID))
		}
		if p.dest = resolveIntentsAsset(cat, p.destAssetID); p.dest == nil {
			return nil, fmt.Errorf("%s is not supported for cross-chain swaps", unbip(p.destAssetID))
		}
		recipient, err := intentsWalletAddress(p.wallet)
		if err != nil {
			return nil, fmt.Errorf("error getting the recipient address: %w", err)
		}
		p.req = &intents.QuoteRequest{
			Dry:               dry,
			SwapType:          intents.SwapTypeExactInput,
			SlippageTolerance: intentsDefaultSlippageBps,
			OriginAsset:       mtok.AssetID,
			DepositType:       intents.LocationOriginChain,
			DestinationAsset:  p.dest.AssetID,
			Amount:            p.amount.String(),
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
		if err := verifyQuoteAsset(cat, p.resp.Request.DestinationAsset, p.destAssetID); err != nil {
			return nil, fmt.Errorf("quote destination asset: %w", err)
		}
		q := &p.resp.Quote
		in, err := parseTokenAmount(q.AmountIn)
		if err != nil || in.Cmp(p.amount) != 0 {
			return nil, fmt.Errorf("the quote prices %s, not the %s swept", q.AmountIn, p.amount)
		}
		units := &p.wallet.Info().UnitInfo
		if p.atomsOut, err = fromIntentsAmount(q.AmountOut, units, p.dest.Decimals); err != nil {
			return nil, err
		}
		if p.minAtomsOut, err = fromIntentsAmount(q.MinAmountOut, units, p.dest.Decimals); err != nil {
			return nil, err
		}
		if !dry {
			if q.DepositAddress == "" || q.DepositMemo != "" {
				return nil, errors.New("committed quote carries no usable deposit address")
			}
			depositAddress = q.DepositAddress
		}
	}
	if dry {
		return p, nil
	}
	p.params = &rhea.SweepParams{
		MCA:            p.mca,
		TokenID:        tokenID,
		Nonce:          p.nonce,
		BlockTime:      p.block.Timestamp,
		Deadline:       rhea.DeadlineFrom(p.block.Timestamp),
		GasToken:       rhea.GasTokenNEAR,
		Fee:            p.fee,
		FeeCap:         p.feeCap,
		NearValue:      p.nearValue,
		Amount:         p.amount,
		DepositAddress: depositAddress,
	}
	if p.business, err = rhea.BuildSweep(eng.dep, p.params); err != nil {
		return nil, err
	}
	return p, nil
}

// RheaSweepPreview previews a sweep of amount (token base units, empty for
// the whole plain balance) of a token in the MCA, back into supply when
// destAssetID is nil, otherwise to that wallet asset.
func (c *Core) RheaSweepPreview(ctx context.Context, tokenID string, destAssetID *uint32, amount string) (*RheaSweepPreview, error) {
	p, err := c.planSweep(ctx, tokenID, destAssetID, amount, true)
	if err != nil {
		return nil, err
	}
	pv := &RheaSweepPreview{
		TokenID:       tokenID,
		Symbol:        p.market.Symbol,
		Decimals:      p.decimals,
		ToSupply:      p.toSupply,
		DestAssetID:   p.destAssetID,
		Balance:       p.balance.String(),
		Attributed:    p.attributed.String(),
		Recovered:     new(big.Int).Sub(p.balance, p.attributed).String(),
		Amount:        p.amount.String(),
		Fee:           p.fee.String(),
		FeeCap:        p.feeCap.String(),
		FeeCapSource:  p.feeCapSource,
		FeeInToken:    p.feeInToken.String(),
		NEARAvailable: p.nearAvail.String(),
		Block:         rheaBlock(p.block),
	}
	if p.resp != nil {
		pv.AtomsOut, pv.MinAtomsOut, pv.TimeEstimate = p.atomsOut, p.minAtomsOut, p.resp.Quote.TimeEstimate
	}
	return pv, nil
}

// RheaSweepExecute signs and submits a sweep, bound to the preview the
// caller displayed: the fee may not exceed maxFee and, for a sweep to a
// wallet, the committed quote must guarantee at least minAtomsOut. The app
// password is always required.
func (c *Core) RheaSweepExecute(pw []byte, tokenID string, destAssetID *uint32, amount string, minAtomsOut uint64, maxFee string) (*RheaOp, error) {
	feeBound, err := parseTokenAmount(maxFee)
	if err != nil {
		return nil, errors.New("no previewed fee supplied")
	}
	if destAssetID != nil && minAtomsOut == 0 {
		return nil, errors.New("no previewed minimum output supplied")
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
	p, err := c.planSweep(ctx, tokenID, destAssetID, amount, false)
	if err != nil {
		return nil, err
	}
	if p.fee.Cmp(feeBound) > 0 {
		return nil, fmt.Errorf("the relayer fee rose to %s since the preview; preview again", p.fee)
	}
	if !p.toSupply && p.minAtomsOut < minAtomsOut {
		units := &p.wallet.Info().UnitInfo
		return nil, fmt.Errorf("the rate moved: the committed quote guarantees only %s, below the previewed minimum of %s; preview again",
			units.FormatAtoms(p.minAtomsOut), units.FormatAtoms(minAtomsOut))
	}
	eng := p.eng
	opID := hex.EncodeToString(encode.RandomBytes(16))
	eng.log.Infof("Fee cap for %s %s in the %s market: relayer fee %s yoctoNEAR, cap %s yoctoNEAR, priced by %s at %v USD per NEAR",
		db.RheaOpSweep, opID, p.market.Symbol, p.fee, p.feeCap, p.feeCapSource, p.feeCapPrice)
	var req *rhea.SignedRequest
	if err := c.withRheaKey(crypter, p.identity, func(k *rhea.Key) error {
		var err error
		req, err = k.SignSweep(eng.dep, p.business, p.params, eng.now())
		return err
	}); err != nil {
		return nil, err
	}
	reqJSON, err := req.MarshalJSON()
	if err != nil {
		return nil, err
	}
	op := &RheaOp{
		ID:                opID,
		Kind:              db.RheaOpSweep,
		State:             db.RheaStateSigned,
		AccountManager:    eng.dep.AccountManager,
		MCA:               p.mca,
		Identity:          string(p.identity.Identity),
		DerivationVersion: uint32(p.identity.Version),
		TokenID:           tokenID,
		Amount:            p.amount.String(),
		Fee:               p.fee.String(),
		GasToken:          rhea.GasTokenNEAR,
		Covers:            p.covers,
		Relayer: &db.RheaRelayerLeg{
			SignedRequest: reqJSON,
			Nonce:         p.nonce,
			Deadline:      p.params.Deadline,
		},
		CreatedAt: time.Now(),
	}
	if !p.toSupply {
		envelope, err := p.resp.Envelope()
		if err != nil {
			return nil, fmt.Errorf("error persisting quote envelope: %w", err)
		}
		q := &p.resp.Quote
		legDeadline, err := time.Parse(time.RFC3339, q.Deadline)
		if err != nil {
			legDeadline = time.Now().Add(intentsDeadline(rheaNEARChainID))
		}
		op.WalletAssetID = p.destAssetID
		op.Leg = &db.RheaLeg{
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
		}
		op.USDOut = parseUSD(q.AmountOutUsd)
	}
	eng.opMtx.Lock()
	if err := eng.checkPendingBusiness(p.mca); err != nil {
		eng.opMtx.Unlock()
		return nil, err
	}
	active, err := eng.db.ActiveRheaOps()
	if err == nil {
		err = supplyDelivering(active, p.mca, tokenID)
	}
	if err != nil {
		eng.opMtx.Unlock()
		return nil, err
	}
	if err := eng.db.StoreRheaOp(op); err != nil {
		eng.opMtx.Unlock()
		return nil, fmt.Errorf("error persisting lending operation: %w", err)
	}
	eng.opMtx.Unlock()
	target := "supply"
	if !p.toSupply {
		target = strings.ToUpper(unbip(p.destAssetID))
	}
	eng.log.Infof("Signed sweep %s: %s %s to %s (fee %s yoctoNEAR, nonce %d)", op.ID, p.amount, p.market.Symbol, target, p.fee, p.nonce)
	op = eng.submit(ctx, op, req)
	c.notify(newRheaOpNote(op, "Sweep submitted",
		fmt.Sprintf("Moving %s %s from the lending account to %s.", p.amount, p.market.Symbol, target), db.Poke))
	eng.pokeOps()
	return op, nil
}

// sweepResolving reports whether a sweep still awaits its exec's outcome:
// submission not yet resolved, or an attributed exec whose evidence did not
// yet settle the outcome (EXECUTED).
func sweepResolving(op *RheaOp) bool {
	return resolvingSubmission(op) || (op.Kind == db.RheaOpSweep && op.State == db.RheaStateExecuted)
}

// reattributeSweep retries a sweep whose attributed exec left the outcome
// unresolved, with backoff.
func (e *rheaEngine) reattributeSweep(ctx context.Context, op *RheaOp) error {
	if !e.attributionDue(op.ID) {
		return nil
	}
	sr, err := rhea.ParseSignedRequest(op.Relayer.SignedRequest)
	if err != nil {
		return fmt.Errorf("persisted signed request: %w", err)
	}
	x, _, err := e.findOurExec(ctx, op.Relayer.TxHash, op, sr)
	if err == nil && x == nil {
		err = fmt.Errorf("exec %s does not carry the sweep's business", op.Relayer.TxHash)
	}
	if err == nil {
		if err = x.tree.Complete(); err == nil {
			err = x.tree.EvidenceAvailable()
		}
	}
	if err != nil {
		e.deferAttribution(op.ID)
		return err
	}
	return e.attributeExec(ctx, op, x)
}

// attributeSweepExec records the outcome of an exec carrying a sweep's
// business, from the exec's own receipt subtree. Only positive evidence
// ends the sweep:
//
//   - a sweep to a wallet: any amount sent from the MCA to the deposit
//     address funds the 1Click leg (DELIVERING); nothing sent, a failed
//     step, and none of the token leaving the MCA is EXEC_FAILED;
//   - a sweep into supply: exactly one resolution of the MCA → Burrow
//     transfer of the signed amount, with Burrow's deposit event for the
//     MCA equal to the amount used. All used is SUPPLIED, part PARTIAL
//     (the rest refunded into the MCA), none EXEC_FAILED (all returned).
//     No resolution, a failed step, and none of the token leaving the MCA
//     is EXEC_FAILED.
//
// Anything else (missing, ambiguous, or contradictory evidence) leaves the
// sweep EXECUTED: active, with the exec recorded, and retried with backoff.
// The GasPayment is credited when the subtree holds a native NEAR transfer
// of the fee from the MCA to the relayer.
func (e *rheaEngine) attributeSweepExec(ctx context.Context, op *RheaOp, x *rheaOurExec) error {
	sub := x.tree.Subtree(x.call.ReceiptID)
	fee, err := parseTokenAmount(op.Fee)
	if err != nil {
		return fmt.Errorf("bad persisted fee: %w", err)
	}
	amount, err := parseTokenAmount(op.Amount)
	if err != nil {
		return fmt.Errorf("bad persisted amount: %w", err)
	}
	gasPaid, failedStep := false, false
	for _, r := range x.tree.Receipts() {
		if !sub[r.ID] || r.Outcome == nil {
			continue
		}
		if r.PredecessorID == op.MCA && r.Outcome.Status.Kind == rhea.StatusFailure {
			failedStep = true
		}
		if r.PredecessorID != op.MCA || r.ReceiverID != e.dep.Relayer || !r.Outcome.Status.OK() {
			continue
		}
		for _, a := range r.Actions {
			if a.Kind == "Transfer" && a.Deposit != nil && a.Deposit.Cmp(fee) == 0 {
				gasPaid = true
			}
		}
	}
	xfers, err := x.tree.FTTransfers()
	if err != nil {
		return err
	}
	// left is everything of the token leaving the MCA in the subtree.
	left := new(big.Int)
	for _, t := range xfers {
		if sub[t.ReceiptID] && t.Token == op.TokenID && t.From == op.MCA {
			left.Add(left, t.Amount)
		}
	}
	nothingMoved := failedStep && left.Sign() == 0
	notes := []string{fmt.Sprintf("gas paid %t", gasPaid)}
	state := db.RheaStateExecuted
	var supplied, stranded *big.Int
	if op.Leg != nil {
		sent := new(big.Int)
		for _, t := range xfers {
			if sub[t.ReceiptID] && t.Token == op.TokenID && t.From == op.MCA && t.To == op.Leg.DepositAddress {
				sent.Add(sent, t.Amount)
			}
		}
		notes = append(notes, fmt.Sprintf("sent %s", sent))
		switch {
		case sent.Sign() > 0:
			if sent.Cmp(amount) != 0 {
				notes = append(notes, fmt.Sprintf("the signed amount was %s", amount))
			}
			state = db.RheaStateDelivering
		case nothingMoved:
			notes = append(notes, "a step failed and nothing left the account")
			state = db.RheaStateExecFailed
		default:
			notes = append(notes, fmt.Sprintf("nothing reached the deposit address, %s left the account, failed step %t; unresolved", left, failedStep))
		}
	} else {
		res, err := x.tree.ResolvedTransfers()
		if err != nil {
			return err
		}
		var hops []*rhea.ResolvedTransfer
		for _, r := range res {
			if sub[r.ReceiptID] && r.Token == op.TokenID && r.SenderID == op.MCA && r.ReceiverID == e.dep.Burrow {
				hops = append(hops, r)
			}
		}
		evs, err := x.tree.BurrowEvents(e.dep.Burrow)
		if err != nil {
			return err
		}
		asset, _, err := e.reader.Asset(ctx, op.TokenID, "")
		if err != nil {
			return fmt.Errorf("market %s: %w", op.TokenID, err)
		}
		deposited := new(big.Int)
		for _, ev := range evs {
			if sub[ev.ReceiptID] && ev.Event == rhea.BurrowEventDeposit && ev.AccountID == op.MCA && ev.TokenID == op.TokenID {
				deposited.Add(deposited, ev.Amount)
			}
		}
		switch {
		case len(hops) == 0 && nothingMoved:
			notes = append(notes, "the supply transfer failed and nothing left the account")
			state = db.RheaStateExecFailed
		case len(hops) == 0:
			notes = append(notes, fmt.Sprintf("no supply transfer resolution in the exec, %s left the account, failed step %t; unresolved", left, failedStep))
		case len(hops) > 1:
			notes = append(notes, fmt.Sprintf("%d supply transfer resolutions; unresolved", len(hops)))
		case hops[0].Amount.Cmp(amount) != 0:
			notes = append(notes, fmt.Sprintf("supply transfer of %s, not the signed %s; unresolved", hops[0].Amount, amount))
		case deposited.Cmp(rhea.ToInternal(hops[0].Used, asset.Config.ExtraDecimals)) != 0:
			notes = append(notes, fmt.Sprintf("Burrow recorded a deposit of %s internal units for %s used; unresolved", deposited, hops[0].Used))
		case hops[0].Used.Sign() == 0:
			notes = append(notes, "the supply was returned to the account in full")
			state = db.RheaStateExecFailed
		default:
			hop := hops[0]
			supplied = new(big.Int).Set(hop.Used)
			if r := hop.Refunded(); r.Sign() > 0 {
				state, stranded = db.RheaStatePartial, r
				notes = append(notes, fmt.Sprintf("%s supplied, %s refunded into the account", supplied, r))
			} else {
				state = db.RheaStateSupplied
				notes = append(notes, fmt.Sprintf("%s supplied", supplied))
			}
		}
	}
	noteText := strings.Join(notes, "; ")
	updated, err := e.updateOp(op.ID, func(o *RheaOp) bool {
		if !sweepResolving(o) {
			return false
		}
		if state == db.RheaStateExecuted && o.State == state && o.Relayer.TxHash == x.txHash && o.Notes == noteText {
			return false
		}
		o.Relayer.TxHash = x.txHash
		if gasPaid {
			o.Fees = fee.String()
		}
		if supplied != nil {
			o.Supplied = supplied.String()
		}
		if stranded != nil {
			o.Refunds = append(o.Refunds, db.RheaRefund{Account: o.MCA, Amount: stranded.String()})
			o.Stranded = sumRefunds(o.Refunds, o.MCA).String()
		}
		o.Notes = noteText
		o.State = state
		if state == db.RheaStateDelivering {
			o.Leg.Status = IntentsStateDepositSent
			o.Leg.DepositTxID = x.txHash
		}
		return true
	})
	if err != nil {
		return err
	}
	if state == db.RheaStateExecuted {
		e.deferAttribution(op.ID)
	} else {
		e.clearAttributionBackoff(op.ID)
	}
	if updated != nil {
		e.log.Infof("Sweep %s exec %s: %s (%s)", updated.ID, x.txHash, updated.State, updated.Notes)
		e.notifyWithdraw(updated)
		if updated.State == db.RheaStateDelivering {
			return e.pollWithdrawLeg(ctx, updated)
		}
	}
	return nil
}
