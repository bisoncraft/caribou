// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

const walletTypeToken = "token"

var _ asset.TokenMaster = (*xrpWallet)(nil)

// registerToken registers an XRPL issued currency with the asset package for
// the given networks.
func registerToken(tokenID uint32, desc string, allowedNets ...util.Network) {
	token, found := Tokens[tokenID]
	if !found {
		panic("token " + strconv.Itoa(int(tokenID)) + " not known")
	}
	netAddrs := make(map[util.Network]string)
	for net, issuer := range token.NetIssuers {
		if !slices.Contains(allowedNets, net) {
			continue
		}
		netAddrs[net] = issuer
	}
	asset.RegisterToken(tokenID, &util.Token{
		NetworkID: BipID,
		Name:      token.Name,
		UnitInfo:  token.UnitInfo,
	}, &asset.WalletDefinition{
		Type:        walletTypeToken,
		Tab:         "XRP token",
		Description: desc,
	}, netAddrs)
}

// CreateTokenWallet just checks that the token exists. Creation is purely
// local: the trustline that lets the account receive the token is a
// separate, user-initiated step (see TokenWallet.EstablishTrustline).
func (w *xrpWallet) CreateTokenWallet(tokenID uint32, _ map[string]string) error {
	if Tokens[tokenID] == nil {
		return fmt.Errorf("token not found for asset ID %d", tokenID)
	}
	return nil
}

// OpenTokenWallet opens an issued-currency wallet sharing this wallet's keys
// and RPC connection.
func (w *xrpWallet) OpenTokenWallet(cfg *asset.TokenConfig) (asset.Wallet, error) {
	token, found := Tokens[cfg.AssetID]
	if !found {
		return nil, fmt.Errorf("token %d not found", cfg.AssetID)
	}
	issuerAddr, ok := token.NetIssuers[w.net]
	if !ok {
		return nil, fmt.Errorf("no %s network issuer for token %s (%d)", w.net, token.Name, cfg.AssetID)
	}
	currency, err := encodeCurrencyCode(token.Currency)
	if err != nil {
		return nil, fmt.Errorf("encodeCurrencyCode: %w", err)
	}
	issuerAccountID, err := addressToAccountID(issuerAddr)
	if err != nil {
		return nil, fmt.Errorf("error decoding issuer address: %w", err)
	}
	return &TokenWallet{
		parent:      w,
		assetID:     cfg.AssetID,
		token:       token,
		issuerAddr:  issuerAddr,
		currency:    currency,
		issuer:      [20]byte(issuerAccountID),
		emit:        cfg.Emit,
		peersChange: cfg.PeersChange,
		txDB:        cfg.NetworkTxDB,
		wi: asset.WalletInfo{
			Name:            token.Name,
			UnitInfo:        token.UnitInfo,
			BlockchainClass: asset.BlockchainClassAccount,
			Network:         w.Info().Network,
		},
	}, nil
}

// TokenWallet is an XRPL issued-currency wallet sharing its parent Wallet's
// keys, address, and RPC connection. The deposit address is the same as the
// native XRP address; the recipient must have an established trustline with
// the issuer before receiving funds.
type TokenWallet struct {
	parent      *xrpWallet
	assetID     uint32
	token       *Token
	issuerAddr  string
	currency    [20]byte
	issuer      [20]byte
	emit        *asset.WalletEmitter
	peersChange func(uint32, error)
	txDB        txdb.NetworkTxDB
	wi          asset.WalletInfo

	// trustline caches the last derived trust-line state so TrustlineStatus
	// never touches the network. Every account_lines query this wallet makes
	// (Balance, Connect, EstablishTrustline) feeds it through
	// updateTrustlineState, which announces changes; see TrustlineStatus.
	trustlineMtx   sync.Mutex
	trustline      asset.TrustlineState
	trustlineKnown bool
}

var _ asset.Wallet = (*TokenWallet)(nil)
var _ asset.TxFeeEstimator = (*TokenWallet)(nil)
var _ asset.TrustlineEstablisher = (*TokenWallet)(nil)

func (w *TokenWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	if w.parent.ctx == nil || w.parent.ctx.Err() != nil {
		return nil, fmt.Errorf("parent wallet not connected")
	}
	// The token wallet shares the parent's already-verified RPC/WS connection
	// and has no independent connectivity probe of its own, so report peers
	// once here. Without this, Core's per-asset peer counter for this token
	// never leaves its initial "unknown" state and Send is permanently
	// rejected by the no-connected-peers safety check.
	if w.peersChange != nil {
		w.peersChange(1, nil)
	}
	// The parent's scanner and checkPendingTxs are what observe this token's
	// transactions — they walk the one account both wallets share — so they
	// need this wallet to announce them under the right asset ID, and to
	// refresh this token's balance whenever the account's changes.
	w.parent.registerTokenWallet(w)
	// Seed the trust-line cache so TrustlineStatus is meaningful from the
	// first wallet-state read. A failed query leaves the cache unknown, which
	// reports NotApproved until the next balance refresh; establishing then
	// is still safe because EstablishTrustline re-checks the ledger.
	if !w.parent.accountStateKnown() {
		if _, err := w.parent.balance(false); err != nil {
			w.parent.log.Warnf("Error priming account state for %s trust-line status: %v", w.token.Name, err)
		}
	}
	if line, err := w.lookupTrustline(w.parent.ctx, w.parent.address); err != nil {
		w.parent.log.Warnf("Error querying %s trust line at connect: %v", w.token.Name, err)
	} else {
		w.updateTrustlineState(line)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		w.parent.unregisterTokenWallet(w.assetID)
	}()
	return &wg, nil
}

func (w *TokenWallet) Info() *asset.WalletInfo {
	wi := w.wi
	return &wi
}

// Balance queries the trustline balance for this token's (currency, issuer)
// pair. A missing trustline (the account has never opted in to hold this
// currency) is reported as a zero balance, not an error. The query doubles as
// the trust-line status refresh.
func (w *TokenWallet) Balance() (*asset.Balance, error) {
	line, err := w.lookupTrustline(w.parent.ctx, w.parent.address)
	if err != nil {
		return nil, err
	}
	w.updateTrustlineState(line)
	if line == nil {
		return &asset.Balance{}, nil
	}
	atomic, err := parseDecimalToAtomic(line.Balance, w.token.Decimals)
	if err != nil {
		return nil, fmt.Errorf("error parsing trustline balance %q: %w", line.Balance, err)
	}
	return &asset.Balance{Available: atomic}, nil
}

// emitBalanceChange announces this token's current balance. The parent calls
// it whenever the shared account's balance changes, since the token wallet
// has no chain observer of its own. Balance also refreshes the trust-line
// cache, so this is what keeps TrustlineStatus current as well.
func (w *TokenWallet) emitBalanceChange() {
	if w.emit == nil {
		return
	}
	bal, err := w.Balance()
	if err != nil {
		w.parent.log.Warnf("Error emitting %s balance change: %v", w.token.Name, err)
		return
	}
	w.emit.BalanceChange(bal)
}

// lookupTrustline returns addr's trust line for this token's (currency,
// issuer) pair, or nil when the account has none or does not exist.
func (w *TokenWallet) lookupTrustline(ctx context.Context, addr string) (*accountLine, error) {
	res, err := w.parent.rpc.accountLines(ctx, addr, w.issuerAddr)
	if err != nil {
		if isAccountNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("accountLines: %w", err)
	}
	for i := range res.Lines {
		line := &res.Lines[i]
		if line.Account == w.issuerAddr && line.Currency == w.token.Currency {
			return line, nil
		}
	}
	return nil, nil
}

// fullLimit is trustlineLimitAtomic in the token's conventional units, the
// limit the wallet sets and the bar a line must meet to count as ready.
func (w *TokenWallet) fullLimit() *big.Rat {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(w.token.Decimals)), nil)
	return new(big.Rat).SetFrac(new(big.Int).SetUint64(trustlineLimitAtomic), scale)
}

// trustlineReady reports whether line accepts deposits up to the wallet's
// full limit. A line at a lower limit is not ready: XRPL refuses a payment
// that would raise the holder's balance above the holder's limit, and a
// RippleState with a zero limit on this side persists for as long as a
// balance or the issuer's side keeps it alive. EstablishTrustline re-sets
// such a line to the full limit rather than treating it as done.
func (w *TokenWallet) trustlineReady(line *accountLine) bool {
	if line == nil {
		return false
	}
	limit, ok := new(big.Rat).SetString(line.Limit)
	return ok && limit.Cmp(w.fullLimit()) >= 0
}

// trustSetAddsReserve reports whether setting the wallet's limit on line
// adds to the account's owner count. A side of a trust line carries the
// owner reserve once it is non-default: a nonzero limit, a positive
// balance, a quality, or a freeze. With no line, or a line on which this
// side has neither a limit nor a balance, the TrustSet makes the side
// reserved and the owner reserve must be covered on top of the fee.
// Otherwise the reserve is already held and only the fee is due. A line
// whose fields cannot be parsed is assumed to need the reserve.
func trustSetAddsReserve(line *accountLine) bool {
	if line == nil {
		return true
	}
	limit, ok := new(big.Rat).SetString(line.Limit)
	balance, okBal := new(big.Rat).SetString(line.Balance)
	if !ok || !okBal {
		return true
	}
	return limit.Sign() == 0 && balance.Sign() <= 0
}

// lineCapacity is how much more of the token line can receive before its
// holder's limit is reached, in atomic units. A limit beyond what the token's
// atomic units can represent is treated as unbounded.
func (w *TokenWallet) lineCapacity(line *accountLine) (uint64, error) {
	limitRat, ok := new(big.Rat).SetString(line.Limit)
	if !ok {
		return 0, fmt.Errorf("invalid trust line limit %q", line.Limit)
	}
	balance, err := parseDecimalToAtomic(line.Balance, w.token.Decimals)
	if err != nil {
		return 0, fmt.Errorf("invalid trust line balance %q: %w", line.Balance, err)
	}
	limit, err := parseDecimalToAtomic(line.Limit, w.token.Decimals)
	if err != nil {
		if limitRat.Sign() < 0 {
			return 0, fmt.Errorf("invalid trust line limit %q", line.Limit)
		}
		limit = math.MaxUint64 // beyond uint64 at these decimals: unbounded
	}
	if balance >= limit {
		return 0, nil
	}
	return limit - balance, nil
}

// pendingTrustSet returns the ID of this token's unresolved TrustSet, if the
// tx database holds one. Pre-stored at submission (see EstablishTrustline) and
// resolved by the parent's checkPendingTxs, it is what makes the Pending
// status survive a restart.
func (w *TokenWallet) pendingTrustSet() string {
	if w.txDB == nil {
		return ""
	}
	pending, err := w.txDB.GetAssetPendingTxs(&w.assetID)
	if err != nil {
		w.parent.log.Errorf("Error getting pending %s txs: %v", w.token.Name, err)
		return ""
	}
	for _, tx := range pending {
		if tx.Type == txdb.ApproveToken && !tx.Confirmed {
			return tx.ID
		}
	}
	return ""
}

// updateTrustlineState derives the trust-line state from an account_lines
// observation (line is nil when the account holds no line for the token)
// plus local pending-tx and account-state knowledge, caches it, and
// announces it when it differs from the previous state. It is the single
// writer of the cache.
func (w *TokenWallet) updateTrustlineState(line *accountLine) {
	state := asset.TrustlineState{Reserve: w.parent.ownerReserveDrops()}
	switch {
	case w.trustlineReady(line):
		state.Status = asset.Approved
	case w.pendingTrustSet() != "":
		state.Status = asset.Pending
	default:
		state.Status = asset.NotApproved
		state.Shortfall = w.parent.trustlineShortfall(DefaultFeeDrops, trustSetAddsReserve(line))
	}
	w.trustlineMtx.Lock()
	changed := !w.trustlineKnown || w.trustline != state
	w.trustline, w.trustlineKnown = state, true
	w.trustlineMtx.Unlock()
	if changed && w.emit != nil {
		w.emit.Data("trustline", &state)
	}
}

// TrustlineStatus satisfies asset.TrustlineEstablisher. Until the first
// account_lines query lands it reports NotApproved, which is the safe side:
// the UI withholds the deposit address, and EstablishTrustline re-checks the
// ledger before submitting anything.
func (w *TokenWallet) TrustlineStatus() *asset.TrustlineState {
	w.trustlineMtx.Lock()
	defer w.trustlineMtx.Unlock()
	if !w.trustlineKnown {
		return &asset.TrustlineState{Status: asset.NotApproved, Reserve: w.parent.ownerReserveDrops()}
	}
	state := w.trustline
	return &state
}

// DepositAddress returns the wallet's classic XRP address. The address can
// only receive the token once a trustline to the issuer exists; callers gate
// on TrustlineStatus and offer EstablishTrustline when it is missing.
func (w *TokenWallet) DepositAddress() (string, error) {
	return w.parent.address, nil
}

func (w *TokenWallet) OwnsDepositAddress(address string) (bool, error) {
	return w.parent.OwnsDepositAddress(address)
}

func (w *TokenWallet) ValidateAddress(address string) bool {
	return w.parent.ValidateAddress(address)
}

func (w *TokenWallet) SyncStatus() (*asset.SyncStatus, error) {
	return w.parent.SyncStatus()
}

func (w *TokenWallet) StandardSendFee(feeRate uint64) uint64 {
	return w.parent.StandardSendFee(feeRate)
}

// FeeRate satisfies asset.Wallet. Token transaction fees are paid in the
// parent asset, so the rate is the parent chain's.
func (w *TokenWallet) FeeRate() uint64 {
	return w.parent.FeeRate()
}

// EstimateSendTxFee estimates the native XRP fee for sending value token
// atoms to address; the fee is paid in XRP regardless of the token, same as
// Send. feeRate is ignored; XRP prices its own fee. An invalid address is
// reported via isValidAddress rather than an error. Unlike Send, this does
// not check the destination's trustline — that's a send-time policy check,
// not fee computation, and a missing trustline still surfaces at submit.
func (w *TokenWallet) EstimateSendTxFee(address string, value, _ uint64, _, maxWithdraw bool) (uint64, bool, error) {
	w.parent.opWG.Add(1)
	defer w.parent.opWG.Done()
	if !w.parent.running.Load() {
		return 0, false, fmt.Errorf("wallet not running")
	}

	bal, err := w.Balance()
	if err != nil {
		return 0, false, err
	}
	if !maxWithdraw && bal.Available < value {
		return 0, false, asset.ErrInsufficientBalance
	}

	fee := w.StandardSendFee(0)
	nativeBalance, err := w.parent.balance(true)
	if err != nil {
		return 0, false, fmt.Errorf("checking available balance: %w", err)
	}
	if fee > nativeBalance.Available {
		return 0, false, asset.ErrInsufficientBalance
	}
	return fee, w.ValidateAddress(address), nil
}

// trustlineLimitAtomic is the TrustSet limit used when establishing a
// trustline. XRPL trust limits only cap how much of the currency the
// account can hold; they don't lock or reserve funds, so there's no
// downside to setting the highest value IOU precision allows (see
// normalizeIOUValue) rather than asking the caller to pick one.
const trustlineLimitAtomic = 999_999_999_999_999

// EstablishTrustline submits a TrustSet transaction authorizing this
// account to hold the token, up to trustlineLimitAtomic units, and returns
// the transaction ID. The line must exist before the account can receive the
// token via Send from another wallet.
//
// The call is idempotent. A line already at the full limit returns an empty
// ID and no error, and an unresolved earlier TrustSet returns that
// transaction's ID, so a repeated request (a restore, a double click) never
// burns a second fee. A line at a lower limit, including zero, is re-set to
// the full limit.
// A nil error means the transaction was accepted for inclusion, not that the
// trustline is guaranteed to exist: a tec/ter result (e.g.
// tecINSUF_RESERVE_LINE) is tracked as a pending tx and reconciled
// asynchronously by the parent wallet's checkPendingTxs loop, same as Send.
func (w *TokenWallet) EstablishTrustline() (string, error) {
	w.parent.opWG.Add(1)
	defer w.parent.opWG.Done()
	if !w.parent.running.Load() {
		return "", fmt.Errorf("wallet not running")
	}
	w.parent.keyMtx.RLock()
	if w.parent.privKey == nil {
		w.parent.keyMtx.RUnlock()
		return "", fmt.Errorf("wallet is locked")
	}
	w.parent.keyMtx.RUnlock()

	// Serialize against native and token sends so a concurrent send cannot
	// drain the fee balance this TrustSet authorizes against, and against a
	// concurrent EstablishTrustline so the idempotency check below holds.
	w.parent.sendMtx.Lock()
	defer w.parent.sendMtx.Unlock()

	line, err := w.lookupTrustline(w.parent.ctx, w.parent.address)
	if err != nil {
		return "", fmt.Errorf("error checking for an existing trust line: %w", err)
	}
	if w.trustlineReady(line) {
		w.updateTrustlineState(line)
		return "", nil
	}
	if pendingID := w.pendingTrustSet(); pendingID != "" {
		w.updateTrustlineState(line)
		return pendingID, nil
	}

	feeInfo, err := w.parent.rpc.fee(w.parent.ctx)
	if err != nil {
		return "", fmt.Errorf("error querying fee: %w", err)
	}
	fee := feeInfo.feeDrops()
	if fee == 0 {
		fee = DefaultFeeDrops
	}
	lastLedgerSeq := uint32(feeInfo.LedgerCurrentIndex + lastLedgerWindow)

	// balance(true) also refreshes the parent's cached account state, which
	// trustlineShortfall reads below.
	nativeBalance, err := w.parent.balance(true)
	if err != nil {
		return "", fmt.Errorf("checking available balance: %w", err)
	}
	// A TrustSet that makes this side of the line reserved (see
	// trustSetAddsReserve) increments OwnerCount and so the owner reserve
	// the account must hold — on top of the fee — or the tx lands as
	// tecINSUF_RESERVE_LINE, burning the fee and the sequence for nothing.
	// An account that does not exist yet cannot submit at all, so its
	// shortfall includes the base reserve.
	if shortfall := w.parent.trustlineShortfall(fee, trustSetAddsReserve(line)); shortfall > 0 {
		w.setTrustlineShortfall(shortfall)
		return "", fmt.Errorf("%w: establishing the %s trust line needs %s more XRP (available %s XRP)",
			asset.ErrInsufficientBalance, w.token.Name, formatDrops(shortfall), formatDrops(nativeBalance.Available))
	}

	sequence, rollbackSeq, err := w.parent.reserveSequence()
	if err != nil {
		return "", fmt.Errorf("error reserving sequence: %w", err)
	}

	srcAccountID, err := addressToAccountID(w.parent.address)
	if err != nil {
		rollbackSeq()
		return "", fmt.Errorf("error decoding source address: %w", err)
	}

	t := &xrpTrustSet{
		account:       [20]byte(srcAccountID),
		currency:      w.currency,
		issuer:        w.issuer,
		limitAtomic:   trustlineLimitAtomic,
		limitDecimals: w.token.Decimals,
		flags:         tfSetNoRipple,
		fee:           fee,
		sequence:      sequence,
		lastLedgerSeq: lastLedgerSeq,
		signingPubKey: w.parent.pubKey.SerializeCompressed(),
	}

	w.parent.keyMtx.RLock()
	if w.parent.privKey == nil {
		w.parent.keyMtx.RUnlock()
		rollbackSeq()
		return "", fmt.Errorf("wallet is locked")
	}
	sig, err := signTrustSet(t, w.parent.privKey)
	w.parent.keyMtx.RUnlock()
	if err != nil {
		rollbackSeq()
		return "", fmt.Errorf("error signing transaction: %w", err)
	}
	t.txnSig = sig

	rawTxBytes, err := encodeTrustSet(t, false)
	if err != nil {
		rollbackSeq()
		return "", fmt.Errorf("error encoding transaction: %w", err)
	}
	txBlob := strings.ToUpper(hex.EncodeToString(rawTxBytes))
	txHash := computeTxHash(rawTxBytes)
	txID := strings.ToUpper(hex.EncodeToString(txHash[:]))

	// tec/ter results still consume the fee and sequence and are included in
	// a validated ledger, but do not guarantee the TrustSet took effect (e.g.
	// tecINSUF_RESERVE_LINE for a reserve-starved account). Pre-store as
	// pending, like Send, so the parent wallet's checkPendingTxs loop
	// reconciles the real outcome once the tx is validated — surfacing a
	// tec/ter rejection instead of silently reporting success.
	wtx := &txdb.WalletTransaction{
		Type:      txdb.ApproveToken,
		ID:        txID,
		Amount:    trustlineLimitAtomic,
		Fees:      fee,
		NetworkID: BipID,
		AssetID:   w.assetID,
	}
	setLastLedgerSequence(wtx, lastLedgerSeq)
	if w.txDB != nil {
		if err := w.txDB.StoreTx(wtx); err != nil {
			w.parent.log.Errorf("Error pre-storing trustline tx: %v", err)
		}
	}

	sr, err := w.parent.rpc.submit(w.parent.ctx, txBlob)
	if err != nil {
		// Network error: the node may have received the signed blob. Do NOT
		// roll back the sequence — reusing it would cause tefPAST_SEQ if the
		// node did process the tx. The pre-stored entry will be resolved by
		// checkPendingTxs, and reports Pending until then.
		w.parent.log.Warnf("TrustSet submit network error (outcome unknown): %v", err)
		w.updateTrustlineState(line)
		return "", fmt.Errorf("error submitting transaction: %w", err)
	}
	class := ""
	if len(sr.EngineResult) >= 3 {
		class = sr.EngineResult[:3]
	}
	switch class {
	case "tes", "tec", "ter":
		if class != "tes" {
			w.parent.log.Warnf("TrustSet submit result %s (%s): tracking as pending", sr.EngineResult, sr.EngineResultMessage)
		}
		if w.emit != nil {
			w.emit.TransactionNote(wtx, true)
		}
		w.updateTrustlineState(line) // Pending, via the pre-stored tx
		return txID, nil
	default:
		// Deterministic rejection: sequence was not consumed. This is
		// terminal, so mark Confirmed (to leave the pending set) and
		// Rejected.
		rollbackSeq()
		wtx.Confirmed = true
		wtx.Rejected = true
		if w.txDB != nil {
			if err := w.txDB.StoreTx(wtx); err != nil {
				w.parent.log.Errorf("Error updating rejected trustline tx %s: %v", txID, err)
			}
		}
		w.updateTrustlineState(line)
		return "", fmt.Errorf("trustline transaction rejected: %s — %s", sr.EngineResult, sr.EngineResultMessage)
	}
}

// setTrustlineShortfall records the exact shortfall a failed
// EstablishTrustline computed from the live fee, replacing the estimate the
// balance refresh derived from DefaultFeeDrops.
func (w *TokenWallet) setTrustlineShortfall(shortfall uint64) {
	state := asset.TrustlineState{
		Status:    asset.NotApproved,
		Reserve:   w.parent.ownerReserveDrops(),
		Shortfall: shortfall,
	}
	w.trustlineMtx.Lock()
	changed := !w.trustlineKnown || w.trustline != state
	w.trustline, w.trustlineKnown = state, true
	w.trustlineMtx.Unlock()
	if changed && w.emit != nil {
		w.emit.Data("trustline", &state)
	}
}

// formatDrops renders drops as a decimal XRP string for error messages.
func formatDrops(drops uint64) string {
	return strconv.FormatFloat(float64(drops)/1e6, 'f', -1, 64)
}

func (w *TokenWallet) Send(address string, value, _ uint64) (asset.Coin, error) {
	w.parent.opWG.Add(1)
	defer w.parent.opWG.Done()
	if !w.parent.running.Load() {
		return nil, fmt.Errorf("wallet not running")
	}
	if !w.ValidateAddress(address) {
		return nil, fmt.Errorf("invalid XRP address %q", address)
	}
	if value == 0 {
		return nil, fmt.Errorf("cannot send zero value")
	}
	w.parent.keyMtx.RLock()
	if w.parent.privKey == nil {
		w.parent.keyMtx.RUnlock()
		return nil, fmt.Errorf("wallet is locked")
	}
	w.parent.keyMtx.RUnlock()

	// Reject up front when the destination requires a DestinationTag: this
	// wallet has no SendTagged (unlike the native wallet), so a payment here
	// would always be rejected on-chain with tecDST_TAG_NEEDED after already
	// paying the network fee.
	destInfo, err := w.parent.rpc.accountInfo(w.parent.ctx, address)
	if err != nil && !isAccountNotFound(err) {
		return nil, fmt.Errorf("error checking destination account info: %w", err)
	}
	if err == nil && destInfo.AccountData.Flags&lsfRequireDestTag != 0 {
		return nil, fmt.Errorf("destination %q requires a destination tag, which token sends do not support", address)
	}

	// The destination must already have a trustline for this currency and
	// issuer with room for the payment under its limit; XRPL rejects an
	// issued-currency Payment to an account without one (tecNO_LINE /
	// tecPATH_DRY) and one that would raise the holder's balance above its
	// limit.
	destLine, err := w.lookupTrustline(w.parent.ctx, address)
	if err != nil {
		return nil, fmt.Errorf("error checking destination trustlines: %w", err)
	}
	if destLine == nil {
		return nil, fmt.Errorf("destination %q has no %s trustline with issuer %s; recipient must establish one first", address, w.token.Name, w.issuerAddr)
	}
	capacity, err := w.lineCapacity(destLine)
	if err != nil {
		return nil, fmt.Errorf("error reading destination %s trustline: %w", w.token.Name, err)
	}
	if capacity < value {
		return nil, fmt.Errorf("destination %q can accept at most %s more %s under its trustline limit; recipient must raise the limit first",
			address, formatAtomic(capacity, w.token.Decimals), w.token.Name)
	}

	// Serialize against the parent wallet's native sends: a concurrent native
	// Send drains the fee balance this token send authorizes against, and two
	// concurrent token sends must not both authorize against the same
	// pre-send trustline snapshot. Held through submit, same as native Send.
	w.parent.sendMtx.Lock()
	defer w.parent.sendMtx.Unlock()

	balance, err := w.Balance()
	if err != nil {
		return nil, err
	}
	if balance.Available < value {
		return nil, asset.ErrInsufficientBalance
	}

	feeInfo, err := w.parent.rpc.fee(w.parent.ctx)
	if err != nil {
		return nil, fmt.Errorf("error querying fee: %w", err)
	}
	fee := feeInfo.feeDrops()
	if fee == 0 {
		fee = DefaultFeeDrops
	}
	lastLedgerSeq := uint32(feeInfo.LedgerCurrentIndex + lastLedgerWindow)

	// Token payments are still funded in native XRP, so the parent account
	// must have enough available balance to cover the fee.
	nativeBalance, err := w.parent.balance(true)
	if err != nil {
		return nil, fmt.Errorf("checking available balance: %w", err)
	}
	if fee > nativeBalance.Available {
		return nil, asset.ErrInsufficientBalance
	}

	sequence, rollbackSeq, err := w.parent.reserveSequence()
	if err != nil {
		return nil, fmt.Errorf("error reserving sequence: %w", err)
	}

	destAccountID, err := addressToAccountID(address)
	if err != nil {
		rollbackSeq()
		return nil, fmt.Errorf("error decoding destination address: %w", err)
	}
	srcAccountID, err := addressToAccountID(w.parent.address)
	if err != nil {
		rollbackSeq()
		return nil, fmt.Errorf("error decoding source address: %w", err)
	}

	p := &xrpIOUPayment{
		account:        [20]byte(srcAccountID),
		destination:    [20]byte(destAccountID),
		currency:       w.currency,
		issuer:         w.issuer,
		amountAtomic:   value,
		amountDecimals: w.token.Decimals,
		fee:            fee,
		sequence:       sequence,
		lastLedgerSeq:  lastLedgerSeq,
		signingPubKey:  w.parent.pubKey.SerializeCompressed(),
	}

	w.parent.keyMtx.RLock()
	if w.parent.privKey == nil {
		w.parent.keyMtx.RUnlock()
		rollbackSeq()
		return nil, fmt.Errorf("wallet is locked")
	}
	sig, err := signIOUPayment(p, w.parent.privKey)
	w.parent.keyMtx.RUnlock()
	if err != nil {
		rollbackSeq()
		return nil, fmt.Errorf("error signing transaction: %w", err)
	}
	p.txnSig = sig

	rawTxBytes, err := encodeIOUPayment(p, false)
	if err != nil {
		rollbackSeq()
		return nil, fmt.Errorf("error encoding transaction: %w", err)
	}
	txBlob := strings.ToUpper(hex.EncodeToString(rawTxBytes))
	txHash := computeTxHash(rawTxBytes)
	txID := strings.ToUpper(hex.EncodeToString(txHash[:]))

	recipient := address
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        txID,
		Amount:    value,
		Fees:      fee,
		NetworkID: BipID,
		AssetID:   w.assetID,
		Recipient: &recipient,
	}
	setLastLedgerSequence(wtx, lastLedgerSeq)
	if w.txDB != nil {
		if err := w.txDB.StoreTx(wtx); err != nil {
			w.parent.log.Errorf("Error pre-storing token send tx: %v", err)
		}
	}

	sr, err := w.parent.rpc.submit(w.parent.ctx, txBlob)
	if err != nil {
		w.parent.log.Warnf("Submit network error (tx %s outcome unknown): %v", txID, err)
		return nil, fmt.Errorf("error submitting transaction: %w", err)
	}
	class := ""
	if len(sr.EngineResult) >= 3 {
		class = sr.EngineResult[:3]
	}
	switch class {
	case "tes", "tec", "ter":
	default:
		rollbackSeq()
		wtx.Confirmed = true
		wtx.Rejected = true
		if w.txDB != nil {
			if err := w.txDB.StoreTx(wtx); err != nil {
				w.parent.log.Errorf("Error updating rejected token tx %s: %v", txID, err)
			}
		}
		return nil, fmt.Errorf("transaction rejected: %s — %s", sr.EngineResult, sr.EngineResultMessage)
	}

	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
	}

	return &coin{txHash: txHash, value: value}, nil
}

func (w *TokenWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if w.txDB == nil || req == nil {
		return &txdb.TxHistoryResponse{}, nil
	}
	r := *req
	if r.N == 0 {
		r.N = 50
	}
	return w.txDB.GetAssetTxs(&w.assetID, &r)
}

func (w *TokenWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	txID = strings.ToUpper(txID) // normalize hash case before DB lookup, as the native path does
	if w.txDB != nil {
		wt, err := w.txDB.GetTx(txID)
		if err != nil {
			return nil, err
		}
		if wt != nil {
			return wt, nil
		}
	}
	tr, err := w.parent.rpc.tx(ctx, txID)
	if err != nil {
		if isTxNotFound(err) {
			return nil, asset.CoinNotFoundError
		}
		return nil, fmt.Errorf("tx: %w", err)
	}
	wt := w.walletTransactionFromTx(tr)
	if wt == nil {
		return nil, asset.CoinNotFoundError
	}
	return wt, nil
}

func (w *TokenWallet) PendingTransactions(_ context.Context) []*txdb.WalletTransaction {
	if w.txDB == nil {
		return nil
	}
	pending, err := w.txDB.GetAssetPendingTxs(&w.assetID)
	if err != nil {
		w.parent.log.Errorf("Error getting pending token txs: %v", err)
		return nil
	}
	return pending
}

// tokenSpec identifies one issued currency well enough to parse a Payment for
// it, without needing an open TokenWallet. The native scanner uses it to record
// incoming token transfers, which arrive at the same account it already walks.
type tokenSpec struct {
	assetID    uint32
	issuerAddr string
	currency   string
	decimals   int
}

// spec returns this wallet's tokenSpec.
func (w *TokenWallet) spec() tokenSpec {
	return tokenSpec{
		assetID:    w.assetID,
		issuerAddr: w.issuerAddr,
		currency:   w.token.Currency,
		decimals:   w.token.Decimals,
	}
}

// networkTokenSpecs returns a spec for every registered token that has an
// issuer on net. Tokens without one are not usable on that network.
func networkTokenSpecs(net util.Network) []tokenSpec {
	specs := make([]tokenSpec, 0, len(Tokens))
	for assetID, token := range Tokens {
		issuer, ok := token.NetIssuers[net]
		if !ok {
			continue
		}
		specs = append(specs, tokenSpec{
			assetID:    assetID,
			issuerAddr: issuer,
			currency:   token.Currency,
			decimals:   token.Decimals,
		})
	}
	return specs
}

// walletTransactionFromTx extracts a WalletTransaction from a Payment
// transaction result carrying this token's issued-currency amount.
func (w *TokenWallet) walletTransactionFromTx(tr *txResult) *txdb.WalletTransaction {
	if tr == nil {
		return nil
	}
	return tokenTransactionFromTx(tr, w.parent.address, tr.Meta, tr.Validated, tr.LedgerIndex, w.spec())
}

// tokenTransactionFromTx extracts a WalletTransaction from a Payment carrying
// spec's issued currency. meta, validated and ledgerIndex are passed separately
// because the WebSocket feed delivers them alongside the transaction rather
// than inside it, matching walletTransactionFromTx's signature.
//
// It parses failed inbound payments too, so a caller asking about a specific
// hash can be told the payment to it failed. Those must not reach history —
// see failedInbound — but that is the scanner's rule, not the parser's.
func tokenTransactionFromTx(tr *txResult, walletAddr string, meta txMeta, validated bool, ledgerIndex uint64, spec tokenSpec) *txdb.WalletTransaction {
	if tr == nil {
		return nil
	}
	if tr.TransactionType == "TrustSet" {
		return tokenTrustSetFromTx(tr, walletAddr, meta, validated, ledgerIndex, spec)
	}
	if tr.TransactionType != "Payment" {
		return nil
	}
	// For incoming payments, credit what meta.delivered_amount reports was
	// actually delivered, not the sender-stated Amount/DeliverMax: a
	// tfPartialPayment transaction validates tesSUCCESS while delivering
	// far less than Amount (see deliveredIssuedAmount). If delivered_amount
	// can't be determined for a successful incoming payment, the actually
	// credited amount is unknown, so don't record it rather than trusting
	// the sender-stated amount, which is exactly what a spoofed
	// tfPartialPayment forges.
	var ia *issuedAmount
	if tr.Destination == walletAddr && meta.TransactionResult == "tesSUCCESS" {
		ia = meta.deliveredIssuedAmount()
		if ia == nil {
			return nil
		}
	} else {
		ia = tr.issuedAmount()
	}
	if ia == nil || ia.Issuer != spec.issuerAddr || ia.Currency != spec.currency {
		return nil
	}
	atomic, err := parseDecimalToAtomic(ia.Value, spec.decimals)
	if err != nil {
		return nil
	}
	var (
		txType    txdb.TransactionType
		recipient *string
	)
	switch {
	case tr.Account == walletAddr && tr.Destination == walletAddr:
		txType = txdb.SelfSend
		r := walletAddr
		recipient = &r
	case tr.Account == walletAddr:
		txType = txdb.Send
		r := tr.Destination
		recipient = &r
	case tr.Destination == walletAddr:
		txType = txdb.Receive
		r := walletAddr
		recipient = &r
	default:
		return nil
	}
	assetID := spec.assetID
	// The fee for an issued-currency Payment is still XRP drops. Report it as
	// the native parser does: processObservedTransaction copies Fees onto the
	// stored record, so leaving it zero wipes the fee a token Send recorded.
	feeParsed, _ := strconv.ParseUint(tr.Fee, 10, 64)
	return &txdb.WalletTransaction{
		Type:        txType,
		ID:          strings.ToUpper(tr.Hash),
		Amount:      atomic,
		Fees:        feeParsed,
		NetworkID:   BipID,
		AssetID:     assetID,
		BlockNumber: ledgerIndex,
		Timestamp:   tr.closeTime(),
		Recipient:   recipient,
		Rejected:    meta.TransactionResult != "tesSUCCESS",
		Confirmed:   validated,
	}
}

// tokenTrustSetFromTx extracts a WalletTransaction from a TrustSet the
// wallet account submitted for spec's currency and issuer: ApproveToken for
// a positive limit, RevokeTokenApproval for zero. Amount is the limit in
// atomic units, capped at trustlineLimitAtomic. The wallet's own TrustSets
// are pre-stored by EstablishTrustline; this parser records one submitted
// from another client of the same account, so the fee it burned and the
// reserve it changed have a history row, and its arrival is what cues the
// scanner and the websocket feed to refresh trust-line state.
func tokenTrustSetFromTx(tr *txResult, walletAddr string, meta txMeta, validated bool, ledgerIndex uint64, spec tokenSpec) *txdb.WalletTransaction {
	if tr == nil || tr.TransactionType != "TrustSet" || tr.Account != walletAddr {
		return nil
	}
	la := issuedAmountFrom(tr.LimitAmount)
	if la == nil || la.Issuer != spec.issuerAddr || la.Currency != spec.currency {
		return nil
	}
	limitRat, ok := new(big.Rat).SetString(la.Value)
	if !ok || limitRat.Sign() < 0 {
		return nil
	}
	limit, err := parseDecimalToAtomic(la.Value, spec.decimals)
	if err != nil {
		limit = trustlineLimitAtomic // beyond uint64 at these decimals
	}
	txType := txdb.ApproveToken
	if limit == 0 {
		txType = txdb.RevokeTokenApproval
	}
	feeParsed, _ := strconv.ParseUint(tr.Fee, 10, 64)
	return &txdb.WalletTransaction{
		Type:        txType,
		ID:          strings.ToUpper(tr.Hash),
		Amount:      limit,
		Fees:        feeParsed,
		NetworkID:   BipID,
		AssetID:     spec.assetID,
		BlockNumber: ledgerIndex,
		Timestamp:   tr.closeTime(),
		Rejected:    meta.TransactionResult != "tesSUCCESS",
		Confirmed:   validated,
	}
}

// formatAtomic renders an atomic token amount as a decimal string at the
// token's decimals, for messages.
func formatAtomic(v uint64, decimals int) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return new(big.Rat).SetFrac(new(big.Int).SetUint64(v), scale).FloatString(decimals)
}

// parseDecimalToAtomic converts an XRPL decimal string value (as returned by
// account_lines or a Payment Amount object) into atomic units at the given
// number of decimals, truncating any precision finer than that.
func parseDecimalToAtomic(s string, decimals int) (uint64, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("invalid decimal value %q", s)
	}
	if r.Sign() < 0 {
		return 0, fmt.Errorf("negative amount %q not supported", s)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(scale))
	i := new(big.Int).Quo(scaled.Num(), scaled.Denom())
	if !i.IsUint64() {
		return 0, fmt.Errorf("value %q overflows uint64 atomic units", s)
	}
	return i.Uint64(), nil
}
