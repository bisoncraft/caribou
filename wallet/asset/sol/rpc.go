// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// rpcResponseSizeLimit bounds data accepted from an untrusted RPC endpoint.
// Solana transaction responses can carry metadata, so retain the same 4 MiB
// headroom used by the Near client rather than imposing a request-specific cap.
const rpcResponseSizeLimit = 4 << 20

// errRateLimited marks an RPC call rejected with HTTP 429, independent of
// whether the response body was a well-formed JSON-RPC error. See
// isRateLimitedRPCError.
var errRateLimited = errors.New("rate limited")

type rpcI interface {
	getSlot(ctx context.Context) (uint64, error)
	getBlockHeight(ctx context.Context) (uint64, error)
	getBalance(ctx context.Context, addr string) (uint64, error)
	getMinimumBalanceForRentExemption(ctx context.Context, size uint64) (uint64, error)
	getFeeForMessage(ctx context.Context, msg []byte) (uint64, error)
	getLatestBlockhash(ctx context.Context) (string, uint64, error)
	sendTransaction(ctx context.Context, txB []byte) (string, error)
	getTransaction(ctx context.Context, sig string) (*rpcTransaction, error)
	getTransactionAtCommitment(ctx context.Context, sig, commitment string) (*rpcTransaction, error)
	getSignaturesForAddress(ctx context.Context, addr string, before, until *string, limit int) ([]signatureInfo, error)
	getSignatureStatuses(ctx context.Context, sigs []string) ([]*signatureStatus, error)
	getTokenAccountBalance(ctx context.Context, addr string) (amount uint64, exists bool, err error)
}

type rpcClient struct {
	httpURL string
}

func newRPCClient(httpURL string) *rpcClient {
	return &rpcClient{httpURL: httpURL}
}

func (c *rpcClient) call(ctx context.Context, method string, params any, result any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		// Some endpoints (or a proxy/CDN in front of one) return a 429 with a
		// plain-text or HTML body rather than a JSON-RPC error object, which
		// would otherwise surface only as an opaque decode failure below and
		// never be recognized as rate-limited by isRateLimitedRPCError.
		return fmt.Errorf("%w: %s", errRateLimited, method)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, rpcResponseSizeLimit+1))
	if err != nil {
		return err
	}
	if len(b) > rpcResponseSizeLimit {
		return fmt.Errorf("%s response exceeds the %d-byte limit", method, rpcResponseSizeLimit)
	}
	var rr struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &rr); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if rr.Error != nil {
		return fmt.Errorf("%s: %s (%d)", method, rr.Error.Message, rr.Error.Code)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(rr.Result, result)
}

func (c *rpcClient) getLatestBlockhash(ctx context.Context) (string, uint64, error) {
	var resp struct {
		Value struct {
			Blockhash            string `json:"blockhash"`
			LastValidBlockHeight uint64 `json:"lastValidBlockHeight"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getLatestBlockhash", []any{map[string]any{"commitment": "confirmed"}}, &resp); err != nil {
		return "", 0, err
	}
	return resp.Value.Blockhash, resp.Value.LastValidBlockHeight, nil
}

func (c *rpcClient) getSlot(ctx context.Context) (uint64, error) {
	var slot uint64
	return slot, c.call(ctx, "getSlot", []any{map[string]any{"commitment": "confirmed"}}, &slot)
}

func (c *rpcClient) getBlockHeight(ctx context.Context) (uint64, error) {
	var height uint64
	return height, c.call(ctx, "getBlockHeight", []any{map[string]any{"commitment": "confirmed"}}, &height)
}

func (c *rpcClient) getBalance(ctx context.Context, addr string) (uint64, error) {
	var resp struct {
		Value uint64 `json:"value"`
	}
	if err := c.call(ctx, "getBalance", []any{addr, map[string]any{"commitment": "confirmed"}}, &resp); err != nil {
		return 0, err
	}
	return resp.Value, nil
}

// getTokenAccountBalance returns the SPL token balance of the account at
// addr. If the account does not exist (e.g. it has never received tokens),
// exists is false and amount is 0, which is not treated as an error.
func (c *rpcClient) getTokenAccountBalance(ctx context.Context, addr string) (amount uint64, exists bool, err error) {
	var resp struct {
		Value struct {
			Amount string `json:"amount"`
		} `json:"value"`
	}
	err = c.call(ctx, "getTokenAccountBalance", []any{addr, map[string]any{"commitment": "confirmed"}}, &resp)
	if err != nil {
		if strings.Contains(err.Error(), "could not find account") {
			return 0, false, nil
		}
		return 0, false, err
	}
	amount, err = strconv.ParseUint(resp.Value.Amount, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("getTokenAccountBalance: invalid amount %q: %w", resp.Value.Amount, err)
	}
	return amount, true, nil
}

func (c *rpcClient) getMinimumBalanceForRentExemption(ctx context.Context, size uint64) (uint64, error) {
	var reserve uint64
	err := c.call(ctx, "getMinimumBalanceForRentExemption", []any{size, map[string]any{"commitment": "confirmed"}}, &reserve)
	return reserve, err
}

func (c *rpcClient) getFeeForMessage(ctx context.Context, msg []byte) (uint64, error) {
	var resp struct {
		Value *uint64 `json:"value"`
	}
	if err := c.call(ctx, "getFeeForMessage", []any{
		base64.StdEncoding.EncodeToString(msg),
		map[string]any{"commitment": "confirmed"},
	}, &resp); err != nil {
		return 0, err
	}
	if resp.Value == nil {
		return 0, nil
	}
	return *resp.Value, nil
}

func (c *rpcClient) sendTransaction(ctx context.Context, txB []byte) (string, error) {
	var sig string
	err := c.call(ctx, "sendTransaction", []any{
		base64.StdEncoding.EncodeToString(txB),
		map[string]any{
			"encoding":            "base64",
			"preflightCommitment": "confirmed",
			"maxRetries":          3,
		},
	}, &sig)
	return sig, err
}

type signatureInfo struct {
	Signature          string `json:"signature"`
	Slot               uint64 `json:"slot"`
	BlockTime          *int64 `json:"blockTime"`
	ConfirmationStatus string `json:"confirmationStatus"`
}

func (c *rpcClient) getSignaturesForAddress(ctx context.Context, addr string, before, until *string, limit int) ([]signatureInfo, error) {
	cfg := map[string]any{
		"commitment": "confirmed",
		"limit":      limit,
	}
	if before != nil {
		cfg["before"] = *before
	}
	if until != nil {
		cfg["until"] = *until
	}
	var sigs []signatureInfo
	err := c.call(ctx, "getSignaturesForAddress", []any{addr, cfg}, &sigs)
	return sigs, err
}

type signatureStatus struct {
	Slot               uint64  `json:"slot"`
	Confirmations      *uint64 `json:"confirmations"`
	Err                any     `json:"err"`
	ConfirmationStatus string  `json:"confirmationStatus"`
}

func (c *rpcClient) getSignatureStatuses(ctx context.Context, sigs []string) ([]*signatureStatus, error) {
	var resp struct {
		Value []*signatureStatus `json:"value"`
	}
	err := c.call(ctx, "getSignatureStatuses", []any{sigs, map[string]any{"searchTransactionHistory": true}}, &resp)
	return resp.Value, err
}

type rpcTransaction struct {
	Slot        uint64              `json:"slot"`
	BlockTime   *int64              `json:"blockTime"`
	Meta        *rpcTransactionMeta `json:"meta"`
	Transaction struct {
		Signatures []string         `json:"signatures"`
		Message    rpcParsedMessage `json:"message"`
	} `json:"transaction"`
}

type rpcTransactionMeta struct {
	Err          any      `json:"err"`
	Fee          uint64   `json:"fee"`
	PreBalances  []uint64 `json:"preBalances"`
	PostBalances []uint64 `json:"postBalances"`
}

type rpcParsedMessage struct {
	AccountKeys  []rpcAccountKey  `json:"accountKeys"`
	Instructions []rpcInstruction `json:"instructions"`
}

type rpcAccountKey struct {
	Pubkey   string `json:"pubkey"`
	Signer   bool   `json:"signer"`
	Writable bool   `json:"writable"`
}

type rpcInstruction struct {
	Program string `json:"program"`
	Parsed  *struct {
		Type string             `json:"type"`
		Info rpcInstructionInfo `json:"info"`
	} `json:"parsed"`
}

// rpcInstructionInfo covers both native "system" transfer instructions and
// "spl-token" transfer/transferChecked instructions.
type rpcInstructionInfo struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Lamports    uint64 `json:"lamports"`
	Authority   string `json:"authority"`
	Mint        string `json:"mint"`
	// Amount is the atomic token amount for the legacy spl-token "transfer"
	// instruction, reported as a decimal string.
	Amount      string `json:"amount"`
	TokenAmount *struct {
		Amount   string `json:"amount"`
		Decimals uint8  `json:"decimals"`
	} `json:"tokenAmount"`
}

func (c *rpcClient) getTransaction(ctx context.Context, sig string) (*rpcTransaction, error) {
	return c.getTransactionAtCommitment(ctx, sig, "confirmed")
}

// getTransactionAtCommitment fetches sig at an explicit commitment. A hit at
// "finalized" is proof the transaction is rooted, which is the only evidence
// checkPendingTxs will accept for the finalized transition when
// getSignatureStatuses has stopped reporting a status for it.
func (c *rpcClient) getTransactionAtCommitment(ctx context.Context, sig, commitment string) (*rpcTransaction, error) {
	var tx *rpcTransaction
	err := c.call(ctx, "getTransaction", []any{
		sig,
		map[string]any{
			"commitment":                     commitment,
			"encoding":                       "jsonParsed",
			"maxSupportedTransactionVersion": 0,
		},
	}, &tx)
	return tx, err
}
