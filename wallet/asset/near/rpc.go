// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"golang.org/x/net/proxy"
)

const (
	// rpcRequestTimeout bounds a single request to a single endpoint. Kept
	// short so a dead provider delays failover by seconds, not the full
	// former 30-second client timeout.
	rpcRequestTimeout = 10 * time.Second
	// endpointCooldown is how long an endpoint sits out after a transport
	// or server failure before it is retried.
	endpointCooldown = time.Minute
)

// rpcEndpoint tracks the health of a single RPC provider URL.
type rpcEndpoint struct {
	url     string
	downTil time.Time
}

// rpcClient is a minimal NEAR JSON-RPC client with ranked-endpoint failover.
// Requests go to the most recently successful endpoint first; endpoints that
// fail at the transport or HTTP layer are placed in a cooldown and skipped
// while any alternative is available.
type rpcClient struct {
	httpClient *http.Client
	log        util.Logger

	mtx       sync.Mutex
	endpoints []*rpcEndpoint
	active    int
}

func newRPCClient(endpoints []string, torProxy string, log util.Logger) (*rpcClient, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no RPC endpoints provided")
	}

	transport := &http.Transport{
		MaxIdleConns:       2,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: true,
	}

	if torProxy != "" {
		dialer, err := proxy.SOCKS5("tcp", torProxy, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("error creating SOCKS5 dialer: %w", err)
		}
		transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	}

	eps := make([]*rpcEndpoint, len(endpoints))
	for i, u := range endpoints {
		eps[i] = &rpcEndpoint{url: u}
	}

	return &rpcClient{
		endpoints: eps,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   rpcRequestTimeout,
		},
		log: log,
	}, nil
}

// rpcRequestID is the JSON-RPC request id sent with every call. NEAR
// echoes it back unused; it is a fixed client identifier.
const rpcRequestID = "caribou"

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      string      `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Name    string          `json:"name"`
	Cause   *rpcErrorCause  `json:"cause"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type rpcErrorCause struct {
	Name string `json:"name"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("NEAR RPC error %d: %s: %s", e.Code, e.Message, string(e.Data))
}

// jsonRPCMethodNotFound is the standard JSON-RPC code for an unsupported
// method. Some public providers proxy only a subset of the NEAR API, so this
// is treated as an endpoint problem, not an authoritative response.
const jsonRPCMethodNotFound = -32601

// rpcResponseSizeLimit caps how much of an RPC response body is read. The
// largest legitimate response is a chunk, well under 1 MiB in practice;
// the cap keeps a broken or hostile public endpoint from ballooning memory
// while leaving generous headroom.
const rpcResponseSizeLimit = 1 << 22 // 4 MiB

// doRequest posts the request body to a single endpoint. endpointBad reports
// whether a failure should be attributed to the endpoint (failover candidate)
// rather than being an authoritative server-side response.
func (c *rpcClient) doRequest(ctx context.Context, url string, body []byte) (_ json.RawMessage, endpointBad bool, _ error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, true, fmt.Errorf("error creating RPC request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("error posting RPC request: %w", err)
	}
	defer resp.Body.Close()

	// Read one byte past the cap so truncation is distinguishable from a
	// response that is exactly the cap.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, rpcResponseSizeLimit+1))
	if err != nil {
		return nil, true, fmt.Errorf("error reading response: %w", err)
	}
	if len(respBody) > rpcResponseSizeLimit {
		return nil, true, fmt.Errorf("response exceeds the %d-byte limit", rpcResponseSizeLimit)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, true, fmt.Errorf("HTTP status %d: %s", resp.StatusCode, string(respBody))
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, true, fmt.Errorf("error decoding response: %w", err)
	}

	if rpcResp.Error != nil {
		// A JSON-RPC error is an authoritative answer (e.g. UNKNOWN_ACCOUNT),
		// except when the provider simply doesn't serve the method.
		return nil, rpcResp.Error.Code == jsonRPCMethodNotFound, rpcResp.Error
	}

	return rpcResp.Result, false, nil
}

func (c *rpcClient) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      rpcRequestID,
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("error marshaling request: %w", err)
	}

	// Build the try order: endpoints not in cooldown first (starting from the
	// active one), then cooling endpoints as a fallback. Each endpoint is
	// tried at most once, so a mix of one cooling and one transiently-failing
	// endpoint still reaches the cooling one — which may have recovered, since
	// the cooldown is not a proof of unavailability.
	c.mtx.Lock()
	start := c.active
	n := len(c.endpoints)
	now := time.Now()
	order := make([]int, 0, n)
	var cooling []int
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if c.endpoints[idx].downTil.After(now) {
			cooling = append(cooling, idx)
		} else {
			order = append(order, idx)
		}
	}
	order = append(order, cooling...)
	c.mtx.Unlock()

	var lastErr error
	for _, idx := range order {
		c.mtx.Lock()
		ep := c.endpoints[idx]
		c.mtx.Unlock()

		result, endpointBad, err := c.doRequest(ctx, ep.url, body)
		if err == nil {
			c.mtx.Lock()
			ep.downTil = time.Time{}
			c.active = idx
			c.mtx.Unlock()
			return result, nil
		}
		// A cancelled or expired context is the caller's verdict, not the
		// endpoint's: bail without failing over or poisoning cooldowns.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !endpointBad {
			return nil, err
		}
		lastErr = err
		c.mtx.Lock()
		ep.downTil = time.Now().Add(endpointCooldown)
		c.mtx.Unlock()
		c.log.Warnf("NEAR RPC endpoint %s failed (%s): %v", ep.url, method, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no NEAR RPC endpoints available")
	}
	return nil, fmt.Errorf("all NEAR RPC endpoints failed: %w", lastErr)
}

// accountInfo holds the response from a view_account query.
type accountInfo struct {
	Amount      string `json:"amount"` // yoctoNEAR as decimal string
	Locked      string `json:"locked"` // staked yoctoNEAR
	CodeHash    string `json:"code_hash"`
	StorageUsed uint64 `json:"storage_usage"`
	BlockHeight uint64 `json:"block_height"`
	BlockHash   string `json:"block_hash"`
}

func (c *rpcClient) viewAccount(ctx context.Context, accountID string) (*accountInfo, error) {
	result, err := c.call(ctx, "query", map[string]interface{}{
		"request_type": "view_account",
		"finality":     "final",
		"account_id":   accountID,
	})
	if err != nil {
		return nil, err
	}
	var info accountInfo
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("error decoding account info: %w", err)
	}
	return &info, nil
}

func (c *rpcClient) viewAccountAt(ctx context.Context, accountID string, blockHeight uint64) (*accountInfo, error) {
	result, err := c.call(ctx, "query", map[string]interface{}{
		"request_type": "view_account",
		"block_id":     blockHeight,
		"account_id":   accountID,
	})
	if err != nil {
		return nil, err
	}
	var info accountInfo
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("error decoding account info: %w", err)
	}
	return &info, nil
}

// accessKeyInfo holds the response from a view_access_key query.
type accessKeyInfo struct {
	Nonce       uint64 `json:"nonce"`
	BlockHeight uint64 `json:"block_height"`
	BlockHash   string `json:"block_hash"`
}

// viewAccessKey queries the access key's current nonce. finality is "final"
// or "optimistic"; optimistic sees recently executed transactions sooner and
// is used when refreshing a stale nonce.
func (c *rpcClient) viewAccessKey(ctx context.Context, accountID, pubKeyBase58, finality string) (*accessKeyInfo, error) {
	result, err := c.call(ctx, "query", map[string]interface{}{
		"request_type": "view_access_key",
		"finality":     finality,
		"account_id":   accountID,
		"public_key":   pubKeyBase58,
	})
	if err != nil {
		return nil, err
	}
	var info accessKeyInfo
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("error decoding access key info: %w", err)
	}
	return &info, nil
}

// callFunction invokes a read-only ("view") contract method and returns its
// raw JSON-encoded return value.
func (c *rpcClient) callFunction(ctx context.Context, contractID, methodName string, args interface{}) (json.RawMessage, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("error marshaling args: %w", err)
	}
	result, err := c.call(ctx, "query", map[string]interface{}{
		"request_type": "call_function",
		"finality":     "final",
		"account_id":   contractID,
		"method_name":  methodName,
		"args_base64":  base64.StdEncoding.EncodeToString(argsJSON),
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		// Result is the UTF-8-encoded return value, transmitted as a JSON
		// array of byte values rather than a base64 string.
		Result []byte `json:"result"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("error decoding call_function result: %w", err)
	}
	return json.RawMessage(resp.Result), nil
}

// blockInfo holds selected fields from a block response.
type blockInfo struct {
	Header blockHeader `json:"header"`
	Chunks []chunkRef  `json:"chunks"`
}

type blockHeader struct {
	Height    uint64 `json:"height"`
	Hash      string `json:"hash"`
	Timestamp uint64 `json:"timestamp"` // nanoseconds
}

type chunkRef struct {
	ChunkHash string `json:"chunk_hash"`
	ShardID   int    `json:"shard_id"`
}

// chunkInfo holds selected fields from a chunk response.
type chunkInfo struct {
	Transactions []chunkTransaction `json:"transactions"`
}

type chunkTransaction struct {
	Hash       string     `json:"hash"`
	SignerID   string     `json:"signer_id"`
	ReceiverID string     `json:"receiver_id"`
	Actions    []txAction `json:"actions"`
}

func (c *rpcClient) getBlock(ctx context.Context, finality string) (*blockInfo, error) {
	result, err := c.call(ctx, "block", map[string]interface{}{
		"finality": finality,
	})
	if err != nil {
		return nil, err
	}
	var info blockInfo
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("error decoding block info: %w", err)
	}
	return &info, nil
}

func (c *rpcClient) getBlockByHash(ctx context.Context, blockHash string) (*blockInfo, error) {
	result, err := c.call(ctx, "block", map[string]interface{}{
		"block_id": blockHash,
	})
	if err != nil {
		return nil, err
	}
	var info blockInfo
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("error decoding block info: %w", err)
	}
	return &info, nil
}

func (c *rpcClient) getBlockByHeight(ctx context.Context, height uint64) (*blockInfo, error) {
	result, err := c.call(ctx, "block", map[string]interface{}{
		"block_id": height,
	})
	if err != nil {
		return nil, err
	}
	var info blockInfo
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("error decoding block info: %w", err)
	}
	return &info, nil
}

func (c *rpcClient) getChunk(ctx context.Context, chunkHash string) (*chunkInfo, error) {
	result, err := c.call(ctx, "chunk", map[string]interface{}{
		"chunk_id": chunkHash,
	})
	if err != nil {
		return nil, err
	}
	var info chunkInfo
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("error decoding chunk info: %w", err)
	}
	return &info, nil
}

// txResult holds the response from a transaction query or broadcast.
type txResult struct {
	Status             txStatus         `json:"status"`
	Transaction        txTransaction    `json:"transaction"`
	TransactionOutcome txOutcomeBlock   `json:"transaction_outcome"`
	ReceiptsOutcome    []txOutcomeBlock `json:"receipts_outcome"`
}

type txTransaction struct {
	Hash       string     `json:"hash"`
	SignerID   string     `json:"signer_id"`
	ReceiverID string     `json:"receiver_id"`
	Actions    []txAction `json:"actions"`
}

// txAction represents a NEAR transaction action. Transfer actions are
// deserialized into the Deposit field; other action types are ignored.
type txAction struct {
	Transfer *txTransfer `json:"Transfer,omitempty"`
}

type txTransfer struct {
	Deposit string `json:"deposit"` // yoctoNEAR as decimal string
}

type txOutcomeBlock struct {
	BlockHash string    `json:"block_hash"`
	Outcome   txOutcome `json:"outcome"`
}

type txOutcome struct {
	TokensBurnt string `json:"tokens_burnt"` // yoctoNEAR as decimal string
}

type txStatus struct {
	SuccessValue *string         `json:"SuccessValue,omitempty"`
	Failure      json.RawMessage `json:"Failure,omitempty"`
}

func (s *txStatus) isSuccess() bool {
	return s.SuccessValue != nil && s.Failure == nil
}

// transferAmount returns the total transfer amount in the transaction
// actions, in drops.
func (r *txResult) transferAmount() uint64 {
	var total uint64
	for _, a := range r.Transaction.Actions {
		if a.Transfer != nil {
			yocto, ok := parseYoctoNEAR(a.Transfer.Deposit)
			if ok {
				total += YoctoToDrops(yocto)
			}
		}
	}
	return total
}

// feeDrops returns the total tokens burnt across the transaction and receipt
// outcomes, in drops, or 0 if no outcome data is present.
func (r *txResult) feeDrops() uint64 {
	total := new(big.Int)
	add := func(s string) {
		if s == "" {
			return
		}
		if yocto, ok := parseYoctoNEAR(s); ok {
			total.Add(total, yocto)
		}
	}
	add(r.TransactionOutcome.Outcome.TokensBurnt)
	for _, ro := range r.ReceiptsOutcome {
		add(ro.Outcome.TokensBurnt)
	}
	return YoctoToDrops(total)
}

// sendTx broadcasts a signed transaction and waits for optimistic execution,
// returning the full execution outcome so that on-chain failures surface at
// send time. If the wait window elapses first, the error satisfies
// isTimeoutError and the transaction may still complete.
func (c *rpcClient) sendTx(ctx context.Context, signedTxBase64 string) (*txResult, error) {
	body, err := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      rpcRequestID,
		Method:  "send_tx",
		Params: map[string]interface{}{
			"signed_tx_base64": signedTxBase64,
			"wait_until":       "EXECUTED_OPTIMISTIC",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("error marshaling send_tx request: %w", err)
	}

	// A broadcast is sent to a single endpoint and never failed over. Once
	// the request has left the client, a transport failure (timeout, reset)
	// cannot be distinguished from an accepted-but-slow transaction, so
	// re-sending to another endpoint could double-broadcast. The active
	// endpoint is the last one to answer a query (e.g. the access-key lookup
	// that precedes every send), so it is the best single choice.
	c.mtx.Lock()
	ep := c.endpoints[c.active]
	c.mtx.Unlock()

	raw, endpointBad, err := c.doRequest(ctx, ep.url, body)
	if err != nil {
		// Transport-level failure, or the server's own TIMEOUT_ERROR: the node
		// may or may not have accepted the transaction. Report as ambiguous so
		// the caller records it pending rather than retrying.
		if endpointBad || isTimeoutError(err) {
			return nil, fmt.Errorf("%w: %v", errBroadcastAmbiguous, err)
		}
		// Any other authoritative JSON-RPC error (e.g. InvalidNonce,
		// NotEnoughBalance) is a definitive rejection.
		return nil, err
	}
	var res txResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("error decoding send_tx result: %w", err)
	}
	return &res, nil
}

// txStatus fetches whatever the node currently knows about a transaction.
// wait_until NONE answers immediately: an unknown hash is a definitive
// UNKNOWN_TRANSACTION error (see isUnknownTransaction) rather than a
// request held open for the node's wait window, and a transaction that is
// known but not yet executed returns with no final status, which callers
// treat as still pending.
func (c *rpcClient) txStatus(ctx context.Context, txHash, senderID string) (*txResult, error) {
	result, err := c.call(ctx, "tx", map[string]interface{}{
		"tx_hash":           txHash,
		"sender_account_id": senderID,
		"wait_until":        "NONE",
	})
	if err != nil {
		return nil, err
	}
	var res txResult
	if err := json.Unmarshal(result, &res); err != nil {
		return nil, fmt.Errorf("error decoding tx status: %w", err)
	}
	return &res, nil
}

// gasPrice returns the current network gas price in yoctoNEAR per unit of
// gas, from the latest block.
func (c *rpcClient) gasPrice(ctx context.Context) (*big.Int, error) {
	result, err := c.call(ctx, "gas_price", []interface{}{nil})
	if err != nil {
		return nil, err
	}
	var res struct {
		GasPrice string `json:"gas_price"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		return nil, fmt.Errorf("error decoding gas price: %w", err)
	}
	price, ok := parseYoctoNEAR(res.GasPrice)
	if !ok {
		return nil, fmt.Errorf("error parsing gas price %q", res.GasPrice)
	}
	return price, nil
}

// errBroadcastAmbiguous indicates a broadcast whose outcome is unknown: the
// signed transaction may or may not have reached the network. The caller
// must treat the transaction as possibly live — record it as pending and
// never construct a replacement transaction — because it may still execute.
var errBroadcastAmbiguous = errors.New("broadcast outcome unknown")

// isTimeoutError reports whether an error is the RPC server's indication
// that a transaction did not reach the requested execution level within the
// wait window. The transaction remains in flight and may still complete.
// errors.As is used so the classification survives error wrapping.
func isTimeoutError(err error) bool {
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		return false
	}
	if rpcErr.Name == "TIMEOUT_ERROR" || strings.Contains(rpcErr.Message, "Timeout") {
		return true
	}
	return rpcErr.Cause != nil && rpcErr.Cause.Name == "TIMEOUT_ERROR"
}

// isInvalidNonce reports whether an error is a transaction rejection for a
// stale or reused access-key nonce. errors.As is used so the classification
// survives error wrapping.
func isInvalidNonce(err error) bool {
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return bytes.Contains(rpcErr.Data, []byte("InvalidNonce"))
}

// parseYoctoNEAR parses a decimal string of yoctoNEAR into a *big.Int.
func parseYoctoNEAR(s string) (*big.Int, bool) {
	v := new(big.Int)
	_, ok := v.SetString(s, 10)
	return v, ok
}

// encodeSignedTx base64-encodes a signed transaction for RPC submission.
func encodeSignedTx(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
