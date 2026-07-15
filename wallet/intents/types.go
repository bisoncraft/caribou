// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package intents

import (
	"encoding/json"
	"fmt"
)

// Swap type values for QuoteRequest.SwapType.
const (
	SwapTypeExactInput  = "EXACT_INPUT"
	SwapTypeExactOutput = "EXACT_OUTPUT"
	SwapTypeFlexInput   = "FLEX_INPUT"
	SwapTypeAnyInput    = "ANY_INPUT"
)

// Deposit, refund, and recipient location values. ORIGIN_CHAIN and
// DESTINATION_CHAIN address the asset's native chain; INTENTS addresses a
// balance held inside the intents.near verifier contract.
const (
	LocationOriginChain      = "ORIGIN_CHAIN"
	LocationDestinationChain = "DESTINATION_CHAIN"
	LocationIntents          = "INTENTS"
)

// Swap status values, in rough lifecycle order.
const (
	StatusPendingDeposit    = "PENDING_DEPOSIT"
	StatusKnownDepositTx    = "KNOWN_DEPOSIT_TX"
	StatusProcessing        = "PROCESSING"
	StatusSuccess           = "SUCCESS"
	StatusRefunded          = "REFUNDED"
	StatusFailed            = "FAILED"
	StatusIncompleteDeposit = "INCOMPLETE_DEPOSIT"
)

// TerminalStatus reports whether a swap status is final.
func TerminalStatus(status string) bool {
	switch status {
	case StatusSuccess, StatusRefunded, StatusFailed:
		return true
	}
	return false
}

// DefaultAppFeeBps is the application fee to configure on quote requests,
// in basis points. Under the service's observed keyless fee mechanics
// (see AppFee), the configured recipient receives half the configured
// fee, so this value is set to twice the intended net revenue: 20 bps
// configured nets 10 bps. The user's marginal cost equals the full
// configured value. Verify net receipt against this assumption whenever
// the service's fee handling changes.
const DefaultAppFeeBps = 20

// AppFee is an application fee collected in-quote. Fee is in basis points
// of the swap amount. The service rewrites the fee array on keyless
// quotes: a configured fee of N becomes N/2 to the configured recipient
// and N/2 plus the keyless surcharge to the protocol's account, with the
// rewritten array echoed in the response. Recipient is an account identity
// inside the intents.near verifier (NEAR named/implicit or 0x-style),
// credited in the traded token.
type AppFee struct {
	Recipient string `json:"recipient"`
	Fee       int    `json:"fee"`
}

// QuoteRequest is the body of POST /v0/quote. Amount is in the asset's
// base units as a decimal string. Deadline (RFC3339) is the refund
// activation time and must accommodate origin-chain confirmation; the
// service may impose its own minimum window.
type QuoteRequest struct {
	Dry                bool     `json:"dry"`
	SwapType           string   `json:"swapType"`
	SlippageTolerance  int      `json:"slippageTolerance"` // basis points
	OriginAsset        string   `json:"originAsset"`
	DepositType        string   `json:"depositType"`
	DestinationAsset   string   `json:"destinationAsset"`
	Amount             string   `json:"amount"`
	RefundTo           string   `json:"refundTo"`
	RefundType         string   `json:"refundType"`
	Recipient          string   `json:"recipient"`
	RecipientType      string   `json:"recipientType"`
	Deadline           string   `json:"deadline"`
	DepositMode        string   `json:"depositMode,omitempty"` // SIMPLE | MEMO
	QuoteWaitingTimeMs int      `json:"quoteWaitingTimeMs,omitempty"`
	Referral           string   `json:"referral,omitempty"`
	AppFees            []AppFee `json:"appFees,omitempty"`
}

// Quote is the priced offer within a quote response. All amounts are
// base-unit decimal strings. RefundFee is denominated in the origin asset,
// WithdrawFee in the destination asset. DepositAddress is only present on
// committed (non-dry) quotes; DepositMemo additionally on memo-mode chains,
// where the deposit MUST carry the memo to be attributed.
type Quote struct {
	AmountIn           string `json:"amountIn"`
	AmountInFormatted  string `json:"amountInFormatted"`
	AmountInUsd        string `json:"amountInUsd"`
	MinAmountIn        string `json:"minAmountIn"`
	AmountOut          string `json:"amountOut"`
	AmountOutFormatted string `json:"amountOutFormatted"`
	AmountOutUsd       string `json:"amountOutUsd"`
	MinAmountOut       string `json:"minAmountOut"`
	RefundFee          string `json:"refundFee"`
	WithdrawFee        string `json:"withdrawFee"`
	TimeEstimate       int64  `json:"timeEstimate"` // seconds
	Deadline           string `json:"deadline"`
	TimeWhenInactive   string `json:"timeWhenInactive"`
	DepositAddress     string `json:"depositAddress"`
	DepositMemo        string `json:"depositMemo"`
}

// QuoteResponse is the signed envelope returned by POST /v0/quote. The
// service signs a canonical subset of the echoed request and quote (see
// VerifyQuoteSignature); the raw JSON of both is retained for that
// verification.
type QuoteResponse struct {
	Timestamp     string
	Signature     string
	CorrelationID string
	Quote         Quote
	Request       QuoteRequest // the request as echoed (asset IDs may be normalized)

	rawQuote   json.RawMessage
	rawRequest json.RawMessage
}

func (r *QuoteResponse) UnmarshalJSON(b []byte) error {
	var env struct {
		Timestamp     string          `json:"timestamp"`
		Signature     string          `json:"signature"`
		CorrelationID string          `json:"correlationId"`
		Quote         json.RawMessage `json:"quote"`
		QuoteRequest  json.RawMessage `json:"quoteRequest"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return err
	}
	r.Timestamp = env.Timestamp
	r.Signature = env.Signature
	r.CorrelationID = env.CorrelationID
	r.rawQuote = env.Quote
	r.rawRequest = env.QuoteRequest
	if err := json.Unmarshal(env.Quote, &r.Quote); err != nil {
		return fmt.Errorf("error decoding quote: %w", err)
	}
	if err := json.Unmarshal(env.QuoteRequest, &r.Request); err != nil {
		return fmt.Errorf("error decoding echoed quote request: %w", err)
	}
	return nil
}

// Envelope reconstructs the signed response envelope for persistence. A
// quote's signature only verifies against the raw bytes of the original
// /quote response (the status endpoint re-serializes and breaks
// verification), so callers that need an auditable record persist this
// envelope at quote time.
func (r *QuoteResponse) Envelope() (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"timestamp":     r.Timestamp,
		"signature":     r.Signature,
		"correlationId": r.CorrelationID,
		"quote":         r.rawQuote,
		"quoteRequest":  r.rawRequest,
	})
}

// TxHashRef is a transaction reference in swap details. The service has
// used both bare-string and object encodings; both are accepted.
type TxHashRef struct {
	Hash        string `json:"hash"`
	ExplorerURL string `json:"explorerUrl"`
}

func (t *TxHashRef) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &t.Hash)
	}
	type ref TxHashRef
	return json.Unmarshal(b, (*ref)(t))
}

// SwapDetails reports progress amounts and transaction references for a
// swap. Amount fields are null until known, so they decode to empty
// strings when absent.
type SwapDetails struct {
	DepositedAmount     string      `json:"depositedAmount"`
	AmountIn            string      `json:"amountIn"`
	AmountOut           string      `json:"amountOut"`
	Slippage            int         `json:"slippage"`
	RefundedAmount      string      `json:"refundedAmount"`
	RefundReason        string      `json:"refundReason"`
	RefundFee           string      `json:"refundFee"`
	WithdrawFee         string      `json:"withdrawFee"`
	IntentHashes        []string    `json:"intentHashes"`
	NearTxHashes        []string    `json:"nearTxHashes"`
	OriginChainTxs      []TxHashRef `json:"originChainTxHashes"`
	DestinationChainTxs []TxHashRef `json:"destinationChainTxHashes"`
}

// StatusResponse is the response of GET /v0/status for one deposit
// address (one swap).
type StatusResponse struct {
	Status        string         `json:"status"`
	UpdatedAt     string         `json:"updatedAt"`
	CorrelationID string         `json:"correlationId"`
	SwapDetails   SwapDetails    `json:"swapDetails"`
	QuoteResponse *QuoteResponse `json:"quoteResponse"`
}

// Token is one entry of the GET /v0/tokens catalog. AssetID is the
// Intents-side identifier (e.g. "nep141:btc.omft.near"); Blockchain is the
// service's chain slug. ContractAddress is empty for most chain-native
// assets. The catalog carries no min/max bounds; those surface at quote
// time via minAmountIn/minAmountOut.
type Token struct {
	AssetID         string  `json:"assetId"`
	Decimals        int     `json:"decimals"`
	Blockchain      string  `json:"blockchain"`
	Symbol          string  `json:"symbol"`
	Price           float64 `json:"price"`
	PriceUpdatedAt  string  `json:"priceUpdatedAt"`
	ContractAddress string  `json:"contractAddress"`
}

// apiError is the service's non-200 response envelope.
type apiError struct {
	Message    string `json:"message"`
	ErrorName  string `json:"error"`
	StatusCode int    `json:"statusCode"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("1Click API error %d (%s): %s", e.StatusCode, e.ErrorName, e.Message)
}
