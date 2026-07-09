// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/bisoncraft/caribou/util"
	"golang.org/x/net/proxy"
)

const (
	httpTimeout  = 30 * time.Second
	infoPath     = "/info"
	exchangePath = "/exchange"
)

// rpcClient is a minimal Hyperliquid REST API client.
type rpcClient struct {
	baseURL    string
	httpClient *http.Client
	log        util.Logger
}

func newRPCClient(baseURL, torProxy string, log util.Logger) (*rpcClient, error) {
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
		baseURL: baseURL,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   httpTimeout,
		},
		log: log,
	}, nil
}

func (c *rpcClient) post(ctx context.Context, path string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, respBody)
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}

func (c *rpcClient) infoRequest(ctx context.Context, payload, result any) error {
	return c.post(ctx, infoPath, payload, result)
}

// --- balance ---

type spotBalance struct {
	Coin  string `json:"coin"`
	Total string `json:"total"`
	Hold  string `json:"hold"`
}

type spotClearinghouseState struct {
	Balances []spotBalance `json:"balances"`
}

// spotBalance returns the available and held HYPE balance in microHYPE atoms.
func (c *rpcClient) spotBalance(ctx context.Context, user string) (available, held uint64, err error) {
	var state spotClearinghouseState
	if err = c.infoRequest(ctx, map[string]any{
		"type": "spotClearinghouseState",
		"user": user,
	}, &state); err != nil {
		return 0, 0, fmt.Errorf("spotClearinghouseState: %w", err)
	}

	for _, b := range state.Balances {
		if b.Coin != "HYPE" {
			continue
		}
		total, err := parseHYPE(b.Total)
		if err != nil {
			return 0, 0, fmt.Errorf("parsing total balance: %w", err)
		}
		holdAmt, err := parseHYPE(b.Hold)
		if err != nil {
			return 0, 0, fmt.Errorf("parsing held balance: %w", err)
		}
		if total < holdAmt {
			holdAmt = total
		}
		return total - holdAmt, holdAmt, nil
	}
	// No HYPE entry means zero balance.
	return 0, 0, nil
}

// --- spot meta ---

type spotToken struct {
	Name       string `json:"name"`
	Index      int    `json:"index"`
	TokenID    string `json:"tokenId"`
	SzDecimals int    `json:"szDecimals"`
}

type spotMetaResponse struct {
	Tokens []spotToken `json:"tokens"`
}

// spotMeta fetches spot token metadata. Used on connect to resolve the HYPE
// token identifier needed for spot send actions.
func (c *rpcClient) spotMeta(ctx context.Context) (*spotMetaResponse, error) {
	var meta spotMetaResponse
	if err := c.infoRequest(ctx, map[string]any{"type": "spotMeta"}, &meta); err != nil {
		return nil, fmt.Errorf("spotMeta: %w", err)
	}
	return &meta, nil
}

// hypeTokenIDFromMeta returns the HYPE token identifier string ("HYPE:<tokenId>")
// from spot metadata, or an error if HYPE is not found.
func hypeTokenIDFromMeta(meta *spotMetaResponse) (string, error) {
	for _, t := range meta.Tokens {
		if t.Name == "HYPE" {
			return "HYPE:" + t.TokenID, nil
		}
	}
	return "", fmt.Errorf("HYPE token not found in spot meta")
}

// --- ledger updates ---

// ledgerDelta describes a balance change event.
type ledgerDelta struct {
	Type        string `json:"type"`
	Token       string `json:"token"` // coin name e.g. "HYPE" (field is "token" in API)
	Amount      string `json:"amount"`
	User        string `json:"user"`
	Destination string `json:"destination"`
}

// ledgerUpdate is one entry from userNonFundingLedgerUpdates.
type ledgerUpdate struct {
	Time  uint64      `json:"time"`
	Hash  string      `json:"hash"`
	Delta ledgerDelta `json:"delta"`
}

// ledgerUpdates returns ledger update events for user since startTimeMS.
func (c *rpcClient) ledgerUpdates(ctx context.Context, user string, startTimeMS int64) ([]ledgerUpdate, error) {
	var updates []ledgerUpdate
	if err := c.infoRequest(ctx, map[string]any{
		"type":      "userNonFundingLedgerUpdates",
		"user":      user,
		"startTime": startTimeMS,
	}, &updates); err != nil {
		return nil, fmt.Errorf("ledgerUpdates: %w", err)
	}
	return updates, nil
}

// --- send asset ---

// sendAssetAction is the generalized spot transfer action. It routes between
// DEX spot balances and works in all account modes (manual, unified, portfolio).
type sendAssetAction struct {
	Type             string `json:"type"`
	HyperliquidChain string `json:"hyperliquidChain"`
	SignatureChainID string `json:"signatureChainId"`
	Destination      string `json:"destination"`
	SourceDex        string `json:"sourceDex"`
	DestinationDex   string `json:"destinationDex"`
	Token            string `json:"token"`
	Amount           string `json:"amount"`
	FromSubAccount   string `json:"fromSubAccount"`
	Nonce            uint64 `json:"nonce"`
}

type exchangeSignature struct {
	R string `json:"r"`
	S string `json:"s"`
	V uint8  `json:"v"`
}

type exchangeRequest struct {
	Action    any               `json:"action"`
	Nonce     uint64            `json:"nonce"`
	Signature exchangeSignature `json:"signature"`
}

type exchangeResponse struct {
	Status   string          `json:"status"`
	Response json.RawMessage `json:"response"`
}

// exchangeRejection is returned when the server explicitly rejects a request
// (status != "ok"). It is distinct from transport or HTTP errors so callers
// can decide whether to retain or discard a pending transaction record.
type exchangeRejection struct{ msg string }

func (e *exchangeRejection) Error() string { return e.msg }

// sendAsset broadcasts a signed spot-to-spot asset transfer. sig is the
// 65-byte compact signature [R(32) || S(32) || V(1)] with V = 27 or 28.
func (c *rpcClient) sendAsset(ctx context.Context, action sendAssetAction, sig [65]byte, nonce uint64) error {
	r := "0x" + fmt.Sprintf("%064x", sig[:32])
	s := "0x" + fmt.Sprintf("%064x", sig[32:64])
	v := sig[64]

	req := exchangeRequest{
		Action: action,
		Nonce:  nonce,
		Signature: exchangeSignature{
			R: r,
			S: s,
			V: v,
		},
	}

	var resp exchangeResponse
	if err := c.post(ctx, exchangePath, req, &resp); err != nil {
		return err
	}
	if resp.Status != "ok" {
		return &exchangeRejection{msg: fmt.Sprintf("exchange error: %s", resp.Response)}
	}
	// Check for per-action errors embedded in an otherwise-ok response.
	var inner struct {
		Data struct {
			Statuses []json.RawMessage `json:"statuses"`
		} `json:"data"`
	}
	if json.Unmarshal(resp.Response, &inner) == nil {
		for _, s := range inner.Data.Statuses {
			var errStatus struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(s, &errStatus) == nil && errStatus.Error != "" {
				return &exchangeRejection{msg: fmt.Sprintf("action error: %s", errStatus.Error)}
			}
		}
	}
	return nil
}
