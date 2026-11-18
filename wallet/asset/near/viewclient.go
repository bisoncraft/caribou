// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bisoncraft/caribou/util"
)

// DefaultArchivalRPCEndpoints ranks archival RPC providers per network.
// Regular endpoints garbage-collect transactions after a few epochs
// (roughly two to three days); TxStatus falls back to these for older
// transactions. Both mainnet endpoints were verified on 2026-11-17 to serve
// EXPERIMENTAL_tx_status, block, and historical query requests for
// transactions the regular endpoints report as unknown.
var DefaultArchivalRPCEndpoints = map[util.Network][]string{
	util.Mainnet: {
		"https://archival-rpc.mainnet.fastnear.com",
		"https://archival-rpc.mainnet.near.org",
	},
}

// ViewClientConfig configures a ViewClient.
type ViewClientConfig struct {
	// Net selects the default endpoints.
	Net util.Network
	// Endpoints overrides DefaultRPCEndpoints[Net].
	Endpoints []string
	// ArchivalEndpoints overrides DefaultArchivalRPCEndpoints[Net]. With
	// none configured, TxStatus reports old transactions as unknown.
	ArchivalEndpoints []string
	// TorProxy is the address (host:port) of a SOCKS5 proxy to route all
	// requests through. Empty means direct connections.
	TorProxy string
}

// ViewClient is a read-only NEAR RPC client for consumers that need chain
// state without an activated NEAR wallet: contract views and account state
// at a named block, block headers, and transaction receipt trees. It shares
// the wallet's endpoint failover. Responses are unauthenticated; finality
// and block references describe what the answering endpoint reported.
type ViewClient struct {
	rpc      *rpcClient
	archival *rpcClient // nil when no archival endpoint is configured
}

// NewViewClient creates a ViewClient.
func NewViewClient(cfg *ViewClientConfig, log util.Logger) (*ViewClient, error) {
	eps := cfg.Endpoints
	if len(eps) == 0 {
		eps = DefaultRPCEndpoints[cfg.Net]
	}
	if len(eps) == 0 {
		return nil, fmt.Errorf("no NEAR RPC endpoints for network %s", cfg.Net)
	}
	rpc, err := newRPCClient(eps, cfg.TorProxy, log)
	if err != nil {
		return nil, err
	}
	c := &ViewClient{rpc: rpc}
	archEps := cfg.ArchivalEndpoints
	if len(archEps) == 0 {
		archEps = DefaultArchivalRPCEndpoints[cfg.Net]
	}
	if len(archEps) > 0 {
		if c.archival, err = newRPCClient(archEps, cfg.TorProxy, log.SubLogger("ARCHIVAL")); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// blockParams adds the block selector to query params: the named block when
// blockHash is set, otherwise finality final.
func blockParams(params map[string]any, blockHash string) map[string]any {
	if blockHash != "" {
		params["block_id"] = blockHash
	} else {
		params["finality"] = "final"
	}
	return params
}

// CallResult is the result of a view call and the block it was evaluated
// at.
type CallResult struct {
	// Result is the method's raw return value (UTF-8 JSON for contracts
	// that return JSON). A method that returns nothing yields an empty
	// slice, distinct from the JSON value null.
	Result      []byte
	Logs        []string
	BlockHeight uint64
	BlockHash   string
}

// CallFunction runs a view method with JSON-encoded args at the block
// named by blockHash, or at the latest final block when blockHash is empty.
// A contract panic or a missing account is returned as an error, never as
// an empty result, and so is a result for a block other than the one named.
func (c *ViewClient) CallFunction(ctx context.Context, contractID, method string, args []byte, blockHash string) (*CallResult, error) {
	if len(args) == 0 {
		args = []byte("{}")
	}
	raw, err := c.rpc.call(ctx, "query", blockParams(map[string]any{
		"request_type": "call_function",
		"account_id":   contractID,
		"method_name":  method,
		"args_base64":  base64.StdEncoding.EncodeToString(args),
	}, blockHash))
	if err != nil {
		return nil, err
	}
	var resp struct {
		// Result is transmitted as a JSON array of byte values.
		Result      []byte   `json:"result"`
		Logs        []string `json:"logs"`
		BlockHeight uint64   `json:"block_height"`
		BlockHash   string   `json:"block_hash"`
		// Error is set by some nodes, instead of a JSON-RPC error, when
		// the contract call fails.
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("error decoding call_function result: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s.%s failed: %s", contractID, method, resp.Error)
	}
	if resp.BlockHash == "" {
		return nil, fmt.Errorf("%s.%s result carries no block reference", contractID, method)
	}
	if blockHash != "" && resp.BlockHash != blockHash {
		return nil, fmt.Errorf("%s.%s read at block %s, requested %s", contractID, method, resp.BlockHash, blockHash)
	}
	if resp.Result == nil {
		resp.Result = []byte{}
	}
	return &CallResult{
		Result:      resp.Result,
		Logs:        resp.Logs,
		BlockHeight: resp.BlockHeight,
		BlockHash:   resp.BlockHash,
	}, nil
}

// CallFunctionAll runs a view call on every configured regular endpoint at
// the block named by blockHash and returns each endpoint's result. It is
// for corroborating a negative before acting on it irreversibly: any
// endpoint that fails to answer, answers with an error, or answers for
// another block fails the call.
func (c *ViewClient) CallFunctionAll(ctx context.Context, contractID, method string, args []byte, blockHash string) ([]*CallResult, error) {
	if blockHash == "" {
		return nil, fmt.Errorf("CallFunctionAll needs a named block")
	}
	if len(args) == 0 {
		args = []byte("{}")
	}
	raws, err := c.rpc.callEach(ctx, "query", map[string]any{
		"request_type": "call_function",
		"account_id":   contractID,
		"method_name":  method,
		"args_base64":  base64.StdEncoding.EncodeToString(args),
		"block_id":     blockHash,
	})
	if err != nil {
		return nil, err
	}
	results := make([]*CallResult, 0, len(raws))
	for _, raw := range raws {
		var resp struct {
			Result      []byte   `json:"result"`
			Logs        []string `json:"logs"`
			BlockHeight uint64   `json:"block_height"`
			BlockHash   string   `json:"block_hash"`
			Error       string   `json:"error"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("error decoding call_function result: %w", err)
		}
		if resp.Error != "" {
			return nil, fmt.Errorf("%s.%s failed: %s", contractID, method, resp.Error)
		}
		if resp.BlockHash != blockHash {
			return nil, fmt.Errorf("%s.%s read at block %q, requested %s", contractID, method, resp.BlockHash, blockHash)
		}
		if resp.Result == nil {
			resp.Result = []byte{}
		}
		results = append(results, &CallResult{Result: resp.Result, Logs: resp.Logs, BlockHeight: resp.BlockHeight, BlockHash: resp.BlockHash})
	}
	return results, nil
}

// AccountView is an account's state at a block. GlobalContractHash is set
// for an account running NEP-591 global contract code by hash, in which
// case CodeHash is the all-zero hash "11111111111111111111111111111111".
type AccountView struct {
	Amount                  string `json:"amount"`
	Locked                  string `json:"locked"`
	CodeHash                string `json:"code_hash"`
	GlobalContractHash      string `json:"global_contract_hash"`
	GlobalContractAccountID string `json:"global_contract_account_id"`
	StorageUsage            uint64 `json:"storage_usage"`
	BlockHeight             uint64 `json:"block_height"`
	BlockHash               string `json:"block_hash"`
}

// ViewAccount reads an account at the block named by blockHash, or at the
// latest final block when blockHash is empty. A result for a block other
// than the one named is an error.
func (c *ViewClient) ViewAccount(ctx context.Context, accountID, blockHash string) (*AccountView, error) {
	raw, err := c.rpc.call(ctx, "query", blockParams(map[string]any{
		"request_type": "view_account",
		"account_id":   accountID,
	}, blockHash))
	if err != nil {
		return nil, err
	}
	var v AccountView
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("error decoding account view: %w", err)
	}
	if v.BlockHash == "" {
		return nil, fmt.Errorf("account view carries no block reference")
	}
	if blockHash != "" && v.BlockHash != blockHash {
		return nil, fmt.Errorf("account %s read at block %s, requested %s", accountID, v.BlockHash, blockHash)
	}
	return &v, nil
}

// BlockHeader is the subset of a block header consumers need to name a
// block and its time.
type BlockHeader struct {
	Height   uint64
	Hash     string
	PrevHash string
	// Timestamp is the header's timestamp. Block timestamps are produced by
	// validators and increase monotonically along the chain.
	Timestamp time.Time
}

type rpcBlockHeader struct {
	Height    uint64 `json:"height"`
	Hash      string `json:"hash"`
	PrevHash  string `json:"prev_hash"`
	Timestamp uint64 `json:"timestamp"` // nanoseconds
}

func (c *ViewClient) block(ctx context.Context, params map[string]any) (*BlockHeader, error) {
	raw, err := c.rpc.call(ctx, "block", params)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Header rpcBlockHeader `json:"header"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("error decoding block: %w", err)
	}
	h := resp.Header
	if h.Hash == "" || h.Timestamp == 0 {
		return nil, fmt.Errorf("block response lacks a hash or timestamp")
	}
	return &BlockHeader{
		Height:    h.Height,
		Hash:      h.Hash,
		PrevHash:  h.PrevHash,
		Timestamp: time.Unix(0, int64(h.Timestamp)),
	}, nil
}

// FinalBlock returns the header of the latest final block as reported by
// the answering endpoint.
func (c *ViewClient) FinalBlock(ctx context.Context) (*BlockHeader, error) {
	return c.block(ctx, map[string]any{"finality": "final"})
}

// BlockByHash returns the header of the block with the given hash. The
// returned hash is checked against the requested one.
func (c *ViewClient) BlockByHash(ctx context.Context, blockHash string) (*BlockHeader, error) {
	h, err := c.block(ctx, map[string]any{"block_id": blockHash})
	if err != nil {
		return nil, err
	}
	if h.Hash != blockHash {
		return nil, fmt.Errorf("requested block %s, endpoint returned %s", blockHash, h.Hash)
	}
	return h, nil
}

// Transaction wait levels for TxStatus, in increasing order of finality.
// TxWaitNone answers with whatever the endpoint currently knows; TxWaitFinal
// holds the request until the transaction and all of its receipts are in
// final blocks, or the endpoint's wait window elapses (a TIMEOUT_ERROR).
const (
	TxWaitNone  = "NONE"
	TxWaitFinal = "FINAL"
)

// TxStatus returns the raw EXPERIMENTAL_tx_status result for a transaction:
// the transaction, every receipt with its actions, every receipt outcome,
// and final_execution_status. senderID is the transaction's signer. A
// transaction the regular endpoints report as unknown is looked up on the
// archival endpoints, since regular endpoints drop old transactions; an
// unknown transaction on both satisfies IsUnknownTransaction.
func (c *ViewClient) TxStatus(ctx context.Context, txHash, senderID, waitUntil string) (json.RawMessage, error) {
	params := map[string]any{
		"tx_hash":           txHash,
		"sender_account_id": senderID,
		"wait_until":        waitUntil,
	}
	raw, err := c.rpc.call(ctx, "EXPERIMENTAL_tx_status", params)
	if err != nil && c.archival != nil && (isUnknownTransaction(err) || isGarbageCollectedBlock(err)) {
		raw, err = c.archival.call(ctx, "EXPERIMENTAL_tx_status", params)
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// IsUnknownTransaction reports whether err is an endpoint's statement that
// it does not know the transaction. A single endpoint's answer is not proof
// the transaction does not exist: it may not have seen it yet, or may have
// dropped it.
func IsUnknownTransaction(err error) bool {
	return isUnknownTransaction(err)
}

// IsTimeout reports whether err is an endpoint's statement that a
// transaction did not reach the requested wait level within its wait
// window.
func IsTimeout(err error) bool {
	return isTimeoutError(err)
}
