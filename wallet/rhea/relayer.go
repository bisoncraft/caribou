// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package rhea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
)

// relayerRequestTimeout bounds a single relayer API request.
const relayerRequestTimeout = 30 * time.Second

// relayerResponseLimit caps how much of a relayer response is read. The
// largest legitimate response is a history page, a few KiB per record.
const relayerResponseLimit = 1 << 22 // 4 MiB

// RelayerClient is a client for RHEA's relayer API. The relayer is a
// centralized service used only to submit signed businesses and to read
// their status and history; every conclusion drawn from its answers is
// verified on chain.
type RelayerClient struct {
	baseURL    string
	httpClient *http.Client
	log        util.Logger
}

// NewRelayerClient creates a relayer client. proxy is the address
// (host:port) of a SOCKS5 proxy to route requests through, or empty.
func NewRelayerClient(baseURL, proxy string, log util.Logger) *RelayerClient {
	// torproxy: the proxy transport is installed below when configured
	httpClient := &http.Client{Timeout: relayerRequestTimeout}
	if proxy != "" {
		httpClient.Transport = dexnet.ProxyTransport(proxy)
	}
	return &RelayerClient{baseURL: baseURL, httpClient: httpClient, log: log}
}

func (c *RelayerClient) do(ctx context.Context, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("error encoding request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	}
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error requesting %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, relayerResponseLimit+1))
	if err != nil {
		return fmt.Errorf("error reading %s response: %w", path, err)
	}
	c.log.Debugf("%s %s: HTTP %d, %d bytes in %s", method, path, resp.StatusCode, len(respBody), time.Since(start))
	if len(respBody) > relayerResponseLimit {
		return fmt.Errorf("%s response exceeds the %d-byte limit", path, relayerResponseLimit)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP status %d from %s: %s", resp.StatusCode, path, string(respBody))
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("error decoding %s response: %w", path, err)
	}
	return nil
}

// LendingConfig is the relayer's published configuration.
type LendingConfig struct {
	// CollectFeeAccountID receives the relayer fee.
	CollectFeeAccountID string
	// GasFee is the per-business relayer fee by token ID ("near" for
	// native NEAR), in base units.
	GasFee map[string]*big.Int
	// MCAVersion is the MCA version the relayer advertises.
	MCAVersion string
}

// Config fetches the relayer configuration.
func (c *RelayerClient) Config(ctx context.Context) (*LendingConfig, error) {
	var entries []*struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := c.do(ctx, http.MethodGet, "/get_multichain_lending_config", nil, &entries); err != nil {
		return nil, err
	}
	cfg := &LendingConfig{GasFee: make(map[string]*big.Int)}
	seen := make(map[string]bool)
	for i, e := range entries {
		if e == nil {
			return nil, fmt.Errorf("relayer config entry %d is null", i)
		}
		if seen[e.Key] {
			return nil, fmt.Errorf("duplicate relayer config key %q", e.Key)
		}
		seen[e.Key] = true
		switch e.Key {
		case "COLLECT_FEE_ACCOUNT_ID":
			cfg.CollectFeeAccountID = e.Value
		case "MCA_VERSION":
			cfg.MCAVersion = e.Value
		case "GAS_FEE":
			var fees map[string]string
			if err := json.Unmarshal([]byte(e.Value), &fees); err != nil {
				return nil, fmt.Errorf("error decoding GAS_FEE: %w", err)
			}
			for token, s := range fees {
				v, err := parseAmount(s)
				if err != nil {
					return nil, fmt.Errorf("GAS_FEE for %s: %w", token, err)
				}
				cfg.GasFee[token] = v
			}
		}
	}
	return cfg, nil
}

// RelayerError is a non-success reply from the relayer. It is not evidence
// that the request was not forwarded to the chain.
type RelayerError struct {
	Code int
	Msg  string
}

func (e *RelayerError) Error() string {
	return fmt.Sprintf("relayer error %d: %s", e.Code, e.Msg)
}

// Submit sends a signed business to the relayer for execution on mcaID and
// returns the relayer's batch ID. An error, including a RelayerError,
// leaves it unknown whether the business reached the chain.
func (c *RelayerClient) Submit(ctx context.Context, mcaID string, req *SignedRequest) (string, error) {
	reqJSON, err := req.MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("error encoding signed request: %w", err)
	}
	body := struct {
		MCAID           string   `json:"mca_id"`
		Wallet          string   `json:"wallet"`
		Request         []string `json:"request"`
		PageDisplayData string   `json:"page_display_data"`
	}{
		MCAID:   mcaID,
		Wallet:  req.SignerWallet.String(),
		Request: []string{string(reqJSON)},
	}
	var resp struct {
		Code *int   `json:"code"`
		Data string `json:"data"`
		Msg  string `json:"msg"`
	}
	if err := c.do(ctx, http.MethodPost, "/multichain_lending_requests", body, &resp); err != nil {
		return "", err
	}
	if resp.Code == nil {
		return "", errors.New("relayer reply lacks a code")
	}
	if *resp.Code != 0 {
		return "", &RelayerError{Code: *resp.Code, Msg: resp.Msg}
	}
	if resp.Data == "" {
		return "", errors.New("relayer reply lacks a batch ID")
	}
	return resp.Data, nil
}

// Batch statuses.
const (
	BatchPending    = 0
	BatchProcessing = 1
	BatchComplete   = 2
)

// RequestResult is the relayer's account of a request's execution. An
// empty TxHash with an error message means the relayer reports not
// broadcasting; that is not proof the business was never executed.
type RequestResult struct {
	TxHash      string `json:"tx_hash"`
	TxErrMsg    string `json:"tx_err_msg"`
	OtherErrMsg string `json:"other_err_msg"`
}

// BatchRequest is one request of a relayer batch.
type BatchRequest struct {
	MCAID       string
	Wallet      string
	Request     string
	RequestHash string
	BatchID     string
	BatchStatus int
	Sequence    int
	// Result is nil until the relayer records one.
	Result *RequestResult
}

// BatchStatus fetches the requests of a relayer batch.
func (c *RelayerClient) BatchStatus(ctx context.Context, batchID string) ([]*BatchRequest, error) {
	var recs []*struct {
		MCAID         string  `json:"mca_id"`
		Wallet        string  `json:"wallet"`
		Request       string  `json:"request"`
		RequestHash   string  `json:"request_hash"`
		BatchID       string  `json:"batch_id"`
		BatchStatus   int     `json:"batch_status"`
		Sequence      int     `json:"sequence"`
		RequestResult *string `json:"request_result"`
	}
	path := "/get_multichain_lending_data?batch_id=" + url.QueryEscape(batchID)
	if err := c.do(ctx, http.MethodGet, path, nil, &recs); err != nil {
		return nil, err
	}
	out := make([]*BatchRequest, 0, len(recs))
	for i, r := range recs {
		if r == nil {
			return nil, fmt.Errorf("batch %s request %d is null", batchID, i)
		}
		if r.BatchID != batchID {
			return nil, fmt.Errorf("batch %s reply carries batch %q", batchID, r.BatchID)
		}
		br := &BatchRequest{
			MCAID: r.MCAID, Wallet: r.Wallet, Request: r.Request, RequestHash: r.RequestHash,
			BatchID: r.BatchID, BatchStatus: r.BatchStatus, Sequence: r.Sequence,
		}
		if r.RequestResult != nil && *r.RequestResult != "" {
			br.Result = new(RequestResult)
			if err := json.Unmarshal([]byte(*r.RequestResult), br.Result); err != nil {
				return nil, fmt.Errorf("error decoding request result: %w", err)
			}
		}
		out = append(out, br)
	}
	return out, nil
}

// History record types.
const (
	// HistoryTypeRelayer is a relayer-submitted business; RequestHash is
	// the NEAR exec transaction hash.
	HistoryTypeRelayer = 1
	// HistoryTypeSupply is a supply through 1Click; RequestHash is the
	// origin-chain transaction.
	HistoryTypeSupply = 2
)

// HistoryRecord is one entry of an MCA's relayer history.
type HistoryRecord struct {
	ID          int64   `json:"id"`
	Type        int     `json:"type"`
	MCAID       string  `json:"mca_id"`
	Wallet      string  `json:"wallet"`
	RequestHash string  `json:"request_hash"`
	BatchID     *string `json:"batch_id"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	CompletedAt *string `json:"completed_at"`
	DestTxHash  *string `json:"dest_tx_hash"`
	DestChain   *string `json:"dest_chain"`
}

// HistoryPage is a page of an MCA's relayer history, newest first.
type HistoryPage struct {
	Records    []*HistoryRecord `json:"record_list"`
	PageNumber int              `json:"page_number"`
	PageSize   int              `json:"page_size"`
	TotalPage  int              `json:"total_page"`
	TotalSize  int              `json:"total_size"`
}

// History fetches a page (1-based) of an MCA's relayer history. The
// history is the relayer's own record: it lists what passed through the
// relayer, may be incomplete, and every entry must be verified on chain
// before it is relied on.
func (c *RelayerClient) History(ctx context.Context, mcaID string, page, size int) (*HistoryPage, error) {
	q := url.Values{}
	q.Set("mca_id", mcaID)
	q.Set("page_number", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(size))
	var p HistoryPage
	if err := c.do(ctx, http.MethodGet, "/get_multichain_lending_history?"+q.Encode(), nil, &p); err != nil {
		return nil, err
	}
	if p.Records == nil {
		return nil, errors.New("history page lacks a record list")
	}
	for i, r := range p.Records {
		if r == nil {
			return nil, fmt.Errorf("history record %d is null", i)
		}
		if r.MCAID != mcaID {
			return nil, fmt.Errorf("history for %s carries a record for %q", mcaID, r.MCAID)
		}
	}
	return &p, nil
}
