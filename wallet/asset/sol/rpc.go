// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

type rpcI interface {
	getSlot(ctx context.Context) (uint64, error)
	getBlockHeight(ctx context.Context) (uint64, error)
	getBalance(ctx context.Context, addr string) (uint64, error)
	getMinimumBalanceForRentExemption(ctx context.Context, size uint64) (uint64, error)
	getFeeForMessage(ctx context.Context, msg []byte) (uint64, error)
	getLatestBlockhash(ctx context.Context) (string, uint64, error)
	sendTransaction(ctx context.Context, txB []byte) (string, error)
	getTransaction(ctx context.Context, sig string) (*rpcTransaction, error)
	getSignaturesForAddress(ctx context.Context, addr string, before, until *string, limit int) ([]signatureInfo, error)
	getSignatureStatuses(ctx context.Context, sigs []string) ([]*signatureStatus, error)
}

type rpcClient struct {
	httpURL string
	wsURL   string
}

func newRPCClient(httpURL, wsURL string) *rpcClient {
	return &rpcClient{httpURL: httpURL, wsURL: wsURL}
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
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
		Type string `json:"type"`
		Info struct {
			Source      string `json:"source"`
			Destination string `json:"destination"`
			Lamports    uint64 `json:"lamports"`
		} `json:"info"`
	} `json:"parsed"`
}

func (c *rpcClient) getTransaction(ctx context.Context, sig string) (*rpcTransaction, error) {
	var tx *rpcTransaction
	err := c.call(ctx, "getTransaction", []any{
		sig,
		map[string]any{
			"commitment":                     "confirmed",
			"encoding":                       "jsonParsed",
			"maxSupportedTransactionVersion": 0,
		},
	}, &tx)
	return tx, err
}
