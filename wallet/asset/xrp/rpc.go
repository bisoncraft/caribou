// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/dexnet"
)

const (
	// accountTxPageLimit is the number of transactions to request per page.
	// rippled defaults to 200 and that is also the max for unauthenticated tier.
	accountTxPageLimit = 200

	// rpcBodySizeLimit is the maximum response body size accepted from rippled.
	// account_tx pages can be large on accounts with dense history.
	rpcBodySizeLimit = 4 << 20 // 4 MiB
)

// rpcClient is a minimal rippled JSON-RPC client backed by dexnet.
type rpcClient struct {
	endpoint string
	log      util.Logger
}

func newRPCClient(endpoint string, log util.Logger) *rpcClient {
	return &rpcClient{endpoint: endpoint, log: log}
}

// rippled JSON-RPC wraps params in a single-element array.
type rpcRequest struct {
	Method string        `json:"method"`
	Params []interface{} `json:"params"`
}

// rippled puts all result data (including errors) in "result".
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
}

// rpcResult is the common wrapper present in every rippled response.
type rpcResult struct {
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	ErrorCode    int    `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type rpcError struct {
	code    int
	errType string
	msg     string
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rippled RPC error %d (%s): %s", e.code, e.errType, e.msg)
}

func (c *rpcClient) call(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	req := rpcRequest{
		Method: method,
		Params: []interface{}{params},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("error marshaling RPC request: %w", err)
	}

	var resp rpcResponse
	if err := dexnet.Post(ctx, c.endpoint, &resp, body,
		dexnet.WithSizeLimit(rpcBodySizeLimit),
		dexnet.WithRequestHeader("Content-Type", "application/json"),
	); err != nil {
		return nil, fmt.Errorf("error posting RPC request: %w", err)
	}

	if len(resp.Result) == 0 {
		return nil, fmt.Errorf("empty result from rippled for method %q", method)
	}

	// Check for application-level errors inside "result" (rippled returns HTTP 200
	// even for error responses, encoding the error in result.status).
	var base rpcResult
	if err := json.Unmarshal(resp.Result, &base); err != nil {
		return nil, fmt.Errorf("error decoding result status: %w", err)
	}
	if base.Status == "error" {
		return nil, &rpcError{
			code:    base.ErrorCode,
			errType: base.Error,
			msg:     base.ErrorMessage,
		}
	}

	return resp.Result, nil
}

// accountData holds the fields we need from account_info.
type accountData struct {
	Account    string `json:"Account"`
	Balance    string `json:"Balance"` // drops as decimal string
	Sequence   uint32 `json:"Sequence"`
	OwnerCount uint32 `json:"OwnerCount"`
	Flags      uint32 `json:"Flags"`
}

type accountQueueData struct {
	TxnCount        uint32 `json:"txn_count"`
	LowestSequence  uint32 `json:"lowest_sequence"`
	HighestSequence uint32 `json:"highest_sequence"`
}

type accountInfoResult struct {
	AccountData accountData       `json:"account_data"`
	LedgerIndex uint64            `json:"ledger_index"`
	QueueData   *accountQueueData `json:"queue_data,omitempty"`
}

func (c *rpcClient) accountInfo(ctx context.Context, address string) (*accountInfoResult, error) {
	result, err := c.call(ctx, "account_info", map[string]interface{}{
		"account":      address,
		"ledger_index": "validated",
	})
	if err != nil {
		return nil, err
	}
	var info accountInfoResult
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("error decoding account_info: %w", err)
	}
	return &info, nil
}

func (c *rpcClient) accountInfoCurrent(ctx context.Context, address string) (*accountInfoResult, error) {
	result, err := c.call(ctx, "account_info", map[string]interface{}{
		"account":      address,
		"ledger_index": "current",
		"queue":        true,
	})
	if err != nil {
		return nil, err
	}
	var info accountInfoResult
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("error decoding account_info: %w", err)
	}
	return &info, nil
}

// feeResult holds fee and current ledger info from the fee method.
type feeResult struct {
	Drops struct {
		BaseFee       string `json:"base_fee"`
		MinimumFee    string `json:"minimum_fee"`
		OpenLedgerFee string `json:"open_ledger_fee"`
	} `json:"drops"`
	LedgerCurrentIndex uint64 `json:"ledger_current_index"`
}

func (c *rpcClient) fee(ctx context.Context) (*feeResult, error) {
	result, err := c.call(ctx, "fee", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var fr feeResult
	if err := json.Unmarshal(result, &fr); err != nil {
		return nil, fmt.Errorf("error decoding fee result: %w", err)
	}
	return &fr, nil
}

// feeDrops returns the open ledger fee in drops, or 0 if unparseable.
// Callers must treat 0 as a signal to use DefaultFeeDrops and log a warning.
func (fr *feeResult) feeDrops() uint64 {
	v, err := strconv.ParseUint(fr.Drops.OpenLedgerFee, 10, 64)
	if err != nil || v == 0 {
		return 0
	}
	return v
}

// submitResult holds the fields we need from the submit response.
type submitResult struct {
	EngineResult        string    `json:"engine_result"`
	EngineResultMessage string    `json:"engine_result_message"`
	TxJSON              rpcTxJSON `json:"tx_json"`
}

type rpcTxJSON struct {
	Hash string `json:"hash"`
}

func (c *rpcClient) submit(ctx context.Context, txBlob string) (*submitResult, error) {
	result, err := c.call(ctx, "submit", map[string]interface{}{
		"tx_blob": txBlob,
	})
	if err != nil {
		return nil, err
	}
	var sr submitResult
	if err := json.Unmarshal(result, &sr); err != nil {
		return nil, fmt.Errorf("error decoding submit result: %w", err)
	}
	return &sr, nil
}

// txMeta holds transaction metadata returned alongside tx and account_tx
// responses. The delivered_amount field reflects what was actually credited to
// the destination — it can differ from Amount when the tfPartialPayment flag
// is set on a Payment transaction.
type txMeta struct {
	DeliveredAmount   interface{} `json:"delivered_amount"`
	TransactionResult string      `json:"TransactionResult"` // e.g. "tesSUCCESS", "tecNO_DST"
}

// deliveredAmountDrops returns the actual delivered XRP in drops. Falls back
// to 0 for IOU amounts or when the field is absent.
func (m *txMeta) deliveredAmountDrops() uint64 {
	s, ok := m.DeliveredAmount.(string)
	if !ok {
		return 0
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// deliveredIssuedAmount returns the actual delivered issued-currency amount,
// or nil for XRP amounts, pre-2014 transactions where the field is the
// literal string "unavailable", or when the field is absent. Callers must
// use this instead of Amount/DeliverMax to credit an incoming IOU payment:
// a Payment with the tfPartialPayment flag set can validate tesSUCCESS while
// delivering far less than Amount states.
func (m *txMeta) deliveredIssuedAmount() *issuedAmount {
	return issuedAmountFrom(m.DeliveredAmount)
}

// txResult holds fields from a transaction lookup.
type txResult struct {
	Account         string      `json:"Account"`
	Destination     string      `json:"Destination"`
	TransactionType string      `json:"TransactionType"`
	Amount          interface{} `json:"Amount"`     // string (XRP drops) or object (IOU); API v1
	DeliverMax      interface{} `json:"DeliverMax"` // API v2 rename of Amount; same semantics
	Fee             string      `json:"Fee"`
	Hash            string      `json:"hash"`
	LedgerIndex     uint64      `json:"ledger_index"`
	Validated       bool        `json:"validated"`
	Meta            txMeta      `json:"meta"`
	CloseTimeISO    string      `json:"close_time_iso,omitempty"` // present in account_tx entries
}

// amountDrops returns the XRP amount in drops. Returns 0 for IOU amounts.
// Checks Amount first (API v1), then DeliverMax (API v2 rename).
func (r *txResult) amountDrops() uint64 {
	for _, v := range []interface{}{r.Amount, r.DeliverMax} {
		s, ok := v.(string)
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(s, 10, 64)
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// closeTime parses CloseTimeISO and returns the Unix timestamp, or 0 if
// absent or unparseable.
func (r *txResult) closeTime() uint64 {
	if r.CloseTimeISO == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, r.CloseTimeISO)
	if err != nil {
		return 0
	}
	return uint64(t.Unix())
}

func (c *rpcClient) tx(ctx context.Context, txHash string) (*txResult, error) {
	result, err := c.call(ctx, "tx", map[string]interface{}{
		"transaction": txHash,
		"binary":      false,
	})
	if err != nil {
		return nil, err
	}
	var tr txResult
	if err := json.Unmarshal(result, &tr); err != nil {
		return nil, fmt.Errorf("error decoding tx result: %w", err)
	}
	return &tr, nil
}

// serverInfoResult holds the fields we need from server_info.
type serverInfoResult struct {
	Info struct {
		ServerState     string `json:"server_state"`
		ValidatedLedger struct {
			Seq uint64  `json:"seq"`
			Age float64 `json:"age"` // seconds since this ledger closed
		} `json:"validated_ledger"`
	} `json:"info"`
}

func (c *rpcClient) serverInfo(ctx context.Context) (*serverInfoResult, error) {
	result, err := c.call(ctx, "server_info", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var si serverInfoResult
	if err := json.Unmarshal(result, &si); err != nil {
		return nil, fmt.Errorf("error decoding server_info: %w", err)
	}
	return &si, nil
}

// accountTxEntry is one entry in the account_tx transactions array.
type accountTxEntry struct {
	Tx          txResult `json:"tx"`
	Meta        txMeta   `json:"meta"`
	Validated   bool     `json:"validated"`
	LedgerIndex uint64   `json:"ledger_index"`
}

type accountTxResult struct {
	Transactions []accountTxEntry `json:"transactions"`
	Marker       json.RawMessage  `json:"marker"` // pagination marker, nil when exhausted
}

func (c *rpcClient) accountTx(ctx context.Context, address string, minLedger, maxLedger uint64, marker json.RawMessage) (*accountTxResult, error) {
	params := map[string]interface{}{
		"account":          address,
		"ledger_index_min": minLedger,
		"ledger_index_max": maxLedger,
		"limit":            accountTxPageLimit,
		"forward":          true, // oldest first
	}
	if marker != nil {
		params["marker"] = marker
	}
	result, err := c.call(ctx, "account_tx", params)
	if err != nil {
		return nil, err
	}
	var atr accountTxResult
	if err := json.Unmarshal(result, &atr); err != nil {
		return nil, fmt.Errorf("error decoding account_tx: %w", err)
	}
	return &atr, nil
}

// serverStateResult holds reserve values from server_state.
type serverStateResult struct {
	State struct {
		ValidatedLedger struct {
			ReserveBase uint64 `json:"reserve_base"` // drops
			ReserveInc  uint64 `json:"reserve_inc"`  // drops per owned object
		} `json:"validated_ledger"`
	} `json:"state"`
}

func (c *rpcClient) serverState(ctx context.Context) (*serverStateResult, error) {
	result, err := c.call(ctx, "server_state", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var ss serverStateResult
	if err := json.Unmarshal(result, &ss); err != nil {
		return nil, fmt.Errorf("error decoding server_state: %w", err)
	}
	return &ss, nil
}

// accountLine is one trustline entry from account_lines.
type accountLine struct {
	Account   string `json:"account"` // the issuer
	Balance   string `json:"balance"` // signed decimal string; positive = we hold this much
	Currency  string `json:"currency"`
	Limit     string `json:"limit"`
	LimitPeer string `json:"limit_peer"`
}

type accountLinesResult struct {
	Lines []accountLine `json:"lines"`
}

// accountLines queries the trustlines for address, optionally filtered to a
// single peer (issuer) account.
func (c *rpcClient) accountLines(ctx context.Context, address, peer string) (*accountLinesResult, error) {
	params := map[string]interface{}{
		"account":      address,
		"ledger_index": "validated",
	}
	if peer != "" {
		params["peer"] = peer
	}
	result, err := c.call(ctx, "account_lines", params)
	if err != nil {
		return nil, err
	}
	var alr accountLinesResult
	if err := json.Unmarshal(result, &alr); err != nil {
		return nil, fmt.Errorf("error decoding account_lines: %w", err)
	}
	return &alr, nil
}

// issuedAmount is a parsed XRPL issued-currency Amount object.
type issuedAmount struct {
	Currency string
	Issuer   string
	Value    string
}

// issuedAmountFrom extracts an issued-currency amount from an interface{}
// decoded from an "Amount" or "DeliverMax" JSON field, or nil if v is a
// native XRP (string) amount or otherwise not a valid IOU object.
func issuedAmountFrom(v interface{}) *issuedAmount {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	currency, _ := m["currency"].(string)
	issuer, _ := m["issuer"].(string)
	value, _ := m["value"].(string)
	if currency == "" || issuer == "" || value == "" {
		return nil
	}
	return &issuedAmount{Currency: currency, Issuer: issuer, Value: value}
}

// issuedAmount returns the issued-currency amount from Amount or DeliverMax,
// or nil if this is a native XRP payment.
func (r *txResult) issuedAmount() *issuedAmount {
	if ia := issuedAmountFrom(r.Amount); ia != nil {
		return ia
	}
	return issuedAmountFrom(r.DeliverMax)
}

// isAccountNotFound returns true when the error indicates the account does
// not exist (i.e. has never received any XRP).
func isAccountNotFound(err error) bool {
	re, ok := err.(*rpcError)
	return ok && re.errType == "actNotFound"
}

// isTxNotFound returns true when the error indicates the transaction is not
// found in any validated ledger.
func isTxNotFound(err error) bool {
	re, ok := err.(*rpcError)
	return ok && re.errType == "txnNotFound"
}
