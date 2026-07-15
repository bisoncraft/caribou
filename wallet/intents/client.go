// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package intents is a client for the NEAR Intents 1Click API
// (https://1click.chaindefuser.com), used for cross-chain swaps. The
// package operates keyless: no API key is sent, at the cost of a platform
// surcharge priced into quotes. All quote responses are
// signature-verified before use. See docs/near-intents-plan.md for the
// integration plan and trust model.
package intents

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
)

// DefaultBaseURL is the production 1Click service.
const DefaultBaseURL = "https://1click.chaindefuser.com"

// requestTimeout bounds a single API request. Quote requests block while
// the service collects solver quotes (quoteWaitingTimeMs, ~3s max), so
// this stays comfortably above that.
const requestTimeout = 30 * time.Second

// ErrSwapNotFound is returned by Status for a deposit address the service
// does not recognize. During post-restore reconstruction this classifies
// an outgoing transaction's counterparty as not-a-swap.
var ErrSwapNotFound = errors.New("no swap for deposit address")

// Config configures a Client.
type Config struct {
	// BaseURL overrides DefaultBaseURL (e.g. for tests).
	BaseURL string
	// SigningKey overrides the pinned quote-signing key, as base58 with
	// optional "ed25519:" prefix. Configuration, not a constant, because
	// the service can rotate it independently of releases.
	SigningKey string
	// AppFees are applied to every quote request that does not carry its
	// own. See AppFee.
	AppFees []AppFee
	// Proxy is the address (host:port) of a SOCKS5 proxy (e.g. Tor) to
	// route all requests through. Empty means direct connections. Swap
	// traffic (quotes, deposit addresses, status) is sensitive, so a
	// privacy-configured wallet must supply this.
	Proxy string
}

// Client is a 1Click API client.
type Client struct {
	baseURL    string
	httpClient *http.Client
	signingKey ed25519.PublicKey
	appFees    []AppFee
	log        util.Logger
}

// NewClient creates a Client. cfg zero value gives the production service
// with the pinned signing key and no application fee.
func NewClient(cfg *Config, log util.Logger) (*Client, error) {
	if cfg == nil {
		cfg = &Config{}
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	keyStr := cfg.SigningKey
	if keyStr == "" {
		keyStr = OneClickSigningKey
	}
	key := decodeEd25519Base58(keyStr)
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad signing key %q", keyStr)
	}
	httpClient := &http.Client{Timeout: requestTimeout}
	if cfg.Proxy != "" {
		httpClient.Transport = dexnet.ProxyTransport(cfg.Proxy)
	}
	return &Client{
		baseURL:    baseURL,
		httpClient: httpClient,
		signingKey: ed25519.PublicKey(key),
		appFees:    cfg.AppFees,
		log:        log,
	}, nil
}

// do performs one API request. A non-2xx response decodes into the
// service's error envelope.
func (c *Client) do(ctx context.Context, method, path string, body, thing any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("error marshaling request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error requesting %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := new(apiError)
		if json.Unmarshal(respBody, apiErr) == nil && apiErr.StatusCode != 0 {
			return apiErr
		}
		return fmt.Errorf("HTTP status %d from %s: %s", resp.StatusCode, path, string(respBody))
	}

	if thing == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, thing); err != nil {
		return fmt.Errorf("error decoding %s response: %w", path, err)
	}
	return nil
}

// Tokens fetches the raw supported-asset catalog.
func (c *Client) Tokens(ctx context.Context) ([]*Token, error) {
	var tokens []*Token
	if err := c.do(ctx, http.MethodGet, "/v0/tokens", nil, &tokens); err != nil {
		return nil, err
	}
	return tokens, nil
}

// Catalog fetches the supported-asset catalog and indexes it against the
// wallet's chains.
func (c *Client) Catalog(ctx context.Context) (*Catalog, error) {
	tokens, err := c.Tokens(ctx)
	if err != nil {
		return nil, err
	}
	return newCatalog(tokens), nil
}

// Quote requests a quote. The client's application fees are attached when
// the request carries none. The response signature is verified and the
// echoed request is cross-checked against the submitted one — a mismatch
// in either means the response (and any deposit address in it) must not
// be used. Committed (non-dry) quotes create a real deposit address and
// are screened by the service's compliance providers; use dry quotes for
// price previews.
func (c *Client) Quote(ctx context.Context, req *QuoteRequest) (*QuoteResponse, error) {
	if len(req.AppFees) == 0 && len(c.appFees) > 0 {
		reqCopy := *req
		reqCopy.AppFees = c.appFees
		req = &reqCopy
	}

	resp := new(QuoteResponse)
	if err := c.do(ctx, http.MethodPost, "/v0/quote", req, resp); err != nil {
		return nil, err
	}

	if err := VerifyQuoteSignature(resp, c.signingKey); err != nil {
		return nil, fmt.Errorf("rejecting quote: %w", err)
	}

	// The signature proves the service signed what it received; it cannot
	// prove the service received what was sent. Cross-check the fields a
	// request-tamperer would target. Asset IDs are excluded: the service
	// normalizes them across its ID schemes.
	echo := &resp.Request
	if echo.Recipient != req.Recipient || echo.RefundTo != req.RefundTo ||
		echo.Amount != req.Amount || echo.SwapType != req.SwapType ||
		echo.Dry != req.Dry {
		return nil, fmt.Errorf("rejecting quote: echoed request differs from submitted request")
	}

	return resp, nil
}

// Status fetches the state of the swap identified by depositAddress,
// returning ErrSwapNotFound if the service doesn't know it.
func (c *Client) Status(ctx context.Context, depositAddress string) (*StatusResponse, error) {
	path := "/v0/status?depositAddress=" + url.QueryEscape(depositAddress)
	status := new(StatusResponse)
	if err := c.do(ctx, http.MethodGet, path, nil, status); err != nil {
		apiErr := new(apiError)
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, ErrSwapNotFound
		}
		return nil, err
	}
	return status, nil
}

// SubmitDeposit notifies the service of the deposit transaction hash to
// speed up detection. Optional; the service also detects deposits by
// watching the address.
func (c *Client) SubmitDeposit(ctx context.Context, depositAddress, txHash string) error {
	body := map[string]string{
		"depositAddress": depositAddress,
		"txHash":         txHash,
	}
	return c.do(ctx, http.MethodPost, "/v0/deposit/submit", body, nil)
}
