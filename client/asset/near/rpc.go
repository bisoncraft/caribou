// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"time"

	"decred.org/dcrdex/dex"
	dexnear "decred.org/dcrdex/dex/networks/near"
	"golang.org/x/net/proxy"
)

// rpcClient is a minimal NEAR JSON-RPC client.
type rpcClient struct {
	endpoint   string
	httpClient *http.Client
	log        dex.Logger
}

func newRPCClient(endpoint, torProxy string, log dex.Logger) (*rpcClient, error) {
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

	return &rpcClient{
		endpoint: endpoint,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		log: log,
	}, nil
}

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
	Data    string          `json:"data"`
}

type rpcErrorCause struct {
	Name string `json:"name"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("NEAR RPC error %d: %s: %s", e.Code, e.Message, e.Data)
}

func (c *rpcClient) call(method string, params interface{}) (json.RawMessage, error) {
	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      "meshwallet",
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("error marshaling request: %w", err)
	}

	resp, err := c.httpClient.Post(c.endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("error posting RPC request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %w", err)
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("error decoding response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}

	return rpcResp.Result, nil
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

func (c *rpcClient) viewAccount(accountID string) (*accountInfo, error) {
	result, err := c.call("query", map[string]interface{}{
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

func (c *rpcClient) viewAccountAt(accountID string, blockHeight uint64) (*accountInfo, error) {
	result, err := c.call("query", map[string]interface{}{
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

func (c *rpcClient) viewAccessKey(accountID string, pubKeyBase58 string) (*accessKeyInfo, error) {
	result, err := c.call("query", map[string]interface{}{
		"request_type": "view_access_key",
		"finality":     "final",
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

// blockInfo holds selected fields from a block response.
type blockInfo struct {
	Header blockHeader  `json:"header"`
	Chunks []chunkRef   `json:"chunks"`
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

func (c *rpcClient) getBlock(finality string) (*blockInfo, error) {
	result, err := c.call("block", map[string]interface{}{
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

func (c *rpcClient) getBlockByHash(blockHash string) (*blockInfo, error) {
	result, err := c.call("block", map[string]interface{}{
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

func (c *rpcClient) getBlockByHeight(height uint64) (*blockInfo, error) {
	result, err := c.call("block", map[string]interface{}{
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

func (c *rpcClient) getChunk(chunkHash string) (*chunkInfo, error) {
	result, err := c.call("chunk", map[string]interface{}{
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
	Status             txStatus       `json:"status"`
	Transaction        txTransaction  `json:"transaction"`
	TransactionOutcome txOutcomeBlock `json:"transaction_outcome"`
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
	BlockHash string `json:"block_hash"`
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
				total += dexnear.YoctoToDrops(yocto)
			}
		}
	}
	return total
}

func (c *rpcClient) broadcastTxAsync(signedTxBase64 string) (string, error) {
	result, err := c.call("broadcast_tx_async", []string{signedTxBase64})
	if err != nil {
		return "", err
	}
	var txHash string
	if err := json.Unmarshal(result, &txHash); err != nil {
		return "", fmt.Errorf("error decoding tx hash: %w", err)
	}
	return txHash, nil
}

func (c *rpcClient) txStatus(txHash, senderID string) (*txResult, error) {
	result, err := c.call("tx", map[string]interface{}{
		"tx_hash":  txHash,
		"sender_account_id": senderID,
		"wait_until": "EXECUTED_OPTIMISTIC",
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
