// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/hex"
	"fmt"
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
	netVersions := make(map[util.Network][]uint32, len(allowedNets))
	for net, issuer := range token.NetIssuers {
		if !slices.Contains(allowedNets, net) {
			continue
		}
		netAddrs[net] = issuer
		netVersions[net] = []uint32{version}
	}
	asset.RegisterToken(tokenID, &util.Token{
		NetworkID: BipID,
		Name:      token.Name,
		UnitInfo:  token.UnitInfo,
	}, &asset.WalletDefinition{
		Type:        walletTypeToken,
		Tab:         "XRP token",
		Description: desc,
	}, netAddrs, netVersions)
}

// CreateTokenWallet just checks that the token exists; XRPL issued
// currencies additionally require the account to establish a trustline with
// the issuer before it can receive funds (see EstablishTrustline).
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
		parent:     w,
		assetID:    cfg.AssetID,
		token:      token,
		issuerAddr: issuerAddr,
		currency:   currency,
		issuer:     [20]byte(issuerAccountID),
		emit:       cfg.Emit,
		txDB:       cfg.NetworkTxDB,
		wi: asset.WalletInfo{
			Name:              token.Name,
			SupportedVersions: []uint32{version},
			UnitInfo:          token.UnitInfo,
			BlockchainClass:   asset.BlockchainClassAccount,
			Network:           w.Info().Network,
		},
	}, nil
}

// TokenWallet is an XRPL issued-currency wallet sharing its parent Wallet's
// keys, address, and RPC connection. The deposit address is the same as the
// native XRP address; the recipient must have an established trustline with
// the issuer before receiving funds.
type TokenWallet struct {
	parent     *xrpWallet
	assetID    uint32
	token      *Token
	issuerAddr string
	currency   [20]byte
	issuer     [20]byte
	emit       *asset.WalletEmitter
	txDB       txdb.NetworkTxDB
	wi         asset.WalletInfo
}

var _ asset.Wallet = (*TokenWallet)(nil)

func (w *TokenWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	if w.parent.ctx == nil || w.parent.ctx.Err() != nil {
		return nil, fmt.Errorf("parent wallet not connected")
	}
	// The parent's scanner and checkPendingTxs are what observe this token's
	// transactions — they walk the one account both wallets share — so they
	// need this wallet's emitter to announce them under the right asset ID.
	w.parent.registerTokenEmitter(w.assetID, w.emit)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		w.parent.unregisterTokenEmitter(w.assetID)
	}()
	return &wg, nil
}

func (w *TokenWallet) Info() *asset.WalletInfo {
	wi := w.wi
	return &wi
}

// Balance queries the trustline balance for this token's (currency, issuer)
// pair. A missing trustline (the account has never opted in to hold this
// currency) is reported as a zero balance, not an error.
func (w *TokenWallet) Balance() (*asset.Balance, error) {
	res, err := w.parent.rpc.accountLines(w.parent.ctx, w.parent.address, w.issuerAddr)
	if err != nil {
		if isAccountNotFound(err) {
			return &asset.Balance{}, nil
		}
		return nil, fmt.Errorf("accountLines: %w", err)
	}
	for _, line := range res.Lines {
		if line.Account != w.issuerAddr || line.Currency != w.token.Currency {
			continue
		}
		atomic, err := parseDecimalToAtomic(line.Balance, w.token.Decimals)
		if err != nil {
			return nil, fmt.Errorf("error parsing trustline balance %q: %w", line.Balance, err)
		}
		return &asset.Balance{Available: atomic}, nil
	}
	return &asset.Balance{}, nil
}

// DepositAddress returns the wallet's classic XRP address. Callers must
// ensure a trustline to the issuer exists (see EstablishTrustline) before
// this address can receive the token.
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

// trustlineLimitAtomic is the TrustSet limit used when establishing a
// trustline. XRPL trust limits only cap how much of the currency the
// account can hold; they don't lock or reserve funds, so there's no
// downside to setting the highest value IOU precision allows (see
// normalizeIOUValue) rather than asking the caller to pick one.
const trustlineLimitAtomic = 999_999_999_999_999

// EstablishTrustline submits a TrustSet transaction authorizing this
// account to hold the token, up to trustlineLimitAtomic units. This must
// succeed before the account can receive the token via Send from another
// wallet. A nil return means the transaction was accepted for inclusion, not
// that the trustline is guaranteed to exist: a tec/ter result (e.g.
// tecINSUF_RESERVE_LINE) is tracked as a pending tx and reconciled
// asynchronously by the parent wallet's checkPendingTxs loop, same as Send.
func (w *TokenWallet) EstablishTrustline() error {
	w.parent.keyMtx.RLock()
	if w.parent.privKey == nil {
		w.parent.keyMtx.RUnlock()
		return fmt.Errorf("wallet is locked")
	}
	w.parent.keyMtx.RUnlock()

	feeInfo, err := w.parent.rpc.fee(w.parent.ctx)
	if err != nil {
		return fmt.Errorf("error querying fee: %w", err)
	}
	fee := feeInfo.feeDrops()
	if fee == 0 {
		fee = DefaultFeeDrops
	}
	lastLedgerSeq := uint32(feeInfo.LedgerCurrentIndex + lastLedgerWindow)

	sequence, rollbackSeq, err := w.parent.reserveSequence()
	if err != nil {
		return fmt.Errorf("error reserving sequence: %w", err)
	}

	srcAccountID, err := addressToAccountID(w.parent.address)
	if err != nil {
		rollbackSeq()
		return fmt.Errorf("error decoding source address: %w", err)
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
		return fmt.Errorf("wallet is locked")
	}
	sig, err := signTrustSet(t, w.parent.privKey)
	w.parent.keyMtx.RUnlock()
	if err != nil {
		rollbackSeq()
		return fmt.Errorf("error signing transaction: %w", err)
	}
	t.txnSig = sig

	rawTxBytes, err := encodeTrustSet(t, false)
	if err != nil {
		rollbackSeq()
		return fmt.Errorf("error encoding transaction: %w", err)
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
		Type:    txdb.ApproveToken,
		ID:      txID,
		Amount:  trustlineLimitAtomic,
		Fees:    fee,
		TokenID: &w.assetID,
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
		// checkPendingTxs.
		w.parent.log.Warnf("TrustSet submit network error (outcome unknown): %v", err)
		return fmt.Errorf("error submitting transaction: %w", err)
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
		return nil
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
		return fmt.Errorf("trustline transaction rejected: %s — %s", sr.EngineResult, sr.EngineResultMessage)
	}
}

func (w *TokenWallet) Send(address string, value, _ uint64) (asset.Coin, error) {
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

	// The destination must already have a trustline for this currency and
	// issuer; XRPL rejects an issued-currency Payment to an account without
	// one (tecNO_LINE / tecPATH_DRY).
	destLines, err := w.parent.rpc.accountLines(w.parent.ctx, address, w.issuerAddr)
	if err != nil && !isAccountNotFound(err) {
		return nil, fmt.Errorf("error checking destination trustlines: %w", err)
	}
	hasTrustline := false
	if destLines != nil {
		for _, line := range destLines.Lines {
			if line.Account == w.issuerAddr && line.Currency == w.token.Currency {
				hasTrustline = true
				break
			}
		}
	}
	if !hasTrustline {
		return nil, fmt.Errorf("destination %q has no %s trustline with issuer %s; recipient must establish one first", address, w.token.Name, w.issuerAddr)
	}

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
		Recipient: &recipient,
		TokenID:   &w.assetID,
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
	if tr == nil || tr.TransactionType != "Payment" {
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
		BlockNumber: ledgerIndex,
		Timestamp:   tr.closeTime(),
		Recipient:   recipient,
		Rejected:    meta.TransactionResult != "tesSUCCESS",
		Confirmed:   validated,
		TokenID:     &assetID,
	}
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
