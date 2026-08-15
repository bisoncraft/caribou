// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package nearintentsmock implements a minimal simnet stand-in for the NEAR
// Intents 1Click swap API (GET /v0/tokens, POST /v0/quote, GET /v0/status,
// POST /v0/deposit/submit) — the subset the caribou wallet's intents client
// uses. Committed quotes are backed by real fund movement: the mock watches
// the quoted deposit address on the origin chain's simnet harness and pays
// out on the destination chain's harness, so full swaps (including refunds)
// can be exercised end-to-end from the wallet UI on simnet.
//
// Quote responses are signed with a deterministic dev keypair whose public
// half is pinned in the wallet as intents.SimnetSigningKey. Swap prices
// come from a running caribou instance's own unauthenticated GET
// /api/fiatrates endpoint when available, with a static fallback table
// otherwise.
//
// A /test/ control API can force per-swap outcomes (refund, failure,
// stall) to exercise the corresponding UI states deterministically.
//
// It is test-only scaffolding: the real 1Click API surface is much larger,
// and this server implements only the subset listed above.
package nearintentsmock

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/wallet/intents"
)

// timestampFormat is the millisecond-precision UTC format the service uses
// in envelope timestamps and deadlines.
const timestampFormat = "2006-01-02T15:04:05.000Z"

// Config configures the mock server.
type Config struct {
	// DextestDir is the harness data root, typically ~/dextest.
	DextestDir string
	// Chains lists the service chain slugs to enable (keys of chainSpecs).
	// Both sides of every quote must be on enabled chains. The single
	// entry "auto" (or an empty list) probes every known chain and enables
	// those whose harness is present, logging what was skipped.
	Chains []string
	// Drivers overrides the harness-backed drivers, keyed by slug. When
	// set, Chains is ignored and the driver map defines the enabled set.
	// Used by tests.
	Drivers map[string]Driver
	// ProcessingDelay is the pause inserted before each intermediate
	// status transition, so the UI's in-flight states are observable.
	// Default 3s.
	ProcessingDelay time.Duration
	// LateWindow is how long past a quote's deadline a full deposit still
	// triggers the refund path rather than being ignored. Default 30m.
	LateWindow time.Duration
	// RatesURL is the base URL of a running caribou app server whose
	// unauthenticated GET /api/fiatrates supplies live USD rates. Empty
	// disables polling; the static fallback prices are used.
	RatesURL string
	// Logger receives operational logging. Defaults to the stdlib
	// default logger.
	Logger *log.Logger
}

// Server is the mock 1Click service. Construct with New and call Run
// before serving Handler.
type Server struct {
	cfg     Config
	priv    ed25519.PrivateKey
	pubKey  string
	drivers map[string]Driver
	assets  map[string]*mockAsset // key: Intents asset ID
	log     *log.Logger
	ctx     context.Context

	priceMtx sync.RWMutex
	prices   map[string]int64 // Intents asset ID -> microUSD per conventional unit

	swapMtx sync.RWMutex
	swaps   map[string]*swapState // key: deposit address
}

// New constructs a Server. Chains (or the Drivers override) must name at
// least two known chains for the mock to be able to quote a swap.
func New(cfg *Config) (*Server, error) {
	c := *cfg
	if c.ProcessingDelay == 0 {
		c.ProcessingDelay = 3 * time.Second
	}
	if c.LateWindow == 0 {
		c.LateWindow = 30 * time.Minute
	}
	if c.Logger == nil {
		c.Logger = log.Default()
	}
	if c.DextestDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("no home dir: %w", err)
		}
		c.DextestDir = filepath.Join(home, "dextest")
	}

	drivers := c.Drivers
	if drivers == nil {
		auto := len(c.Chains) == 0 || (len(c.Chains) == 1 && c.Chains[0] == "auto")
		slugs := c.Chains
		if auto {
			slugs = driverSlugs
		}
		drivers = make(map[string]Driver, len(slugs))
		for _, slug := range slugs {
			if _, known := chainSpecs[slug]; !known {
				return nil, fmt.Errorf("unknown chain slug %q", slug)
			}
			drv, err := newHarnessDriver(slug, c.DextestDir)
			if err == nil {
				if p, is := drv.(pinger); is {
					if pingErr := p.Ping(); pingErr != nil {
						err = fmt.Errorf("harness not responding: %w", pingErr)
					}
				}
			}
			if err != nil {
				if auto {
					c.Logger.Printf("chain %s not enabled: %v", slug, err)
					continue
				}
				return nil, fmt.Errorf("driver for %s: %w", slug, err)
			}
			drivers[slug] = drv
		}
	}
	for slug := range drivers {
		if _, known := chainSpecs[slug]; !known {
			return nil, fmt.Errorf("unknown chain slug %q", slug)
		}
	}
	if len(drivers) < 2 {
		return nil, fmt.Errorf("need at least two chains, got %d", len(drivers))
	}

	// The tradable asset set: every enabled chain's native, plus any test
	// tokens the chain's driver found deployed on its harness.
	assets := make(map[string]*mockAsset, len(drivers))
	for slug, drv := range drivers {
		spec := chainSpecs[slug]
		native := nativeAsset(spec)
		assets[native.intentsID] = native
		if td, is := drv.(TokenDriver); is {
			for _, tok := range td.Tokens() {
				a := tokenAsset(spec, tok)
				assets[a.intentsID] = a
			}
		}
	}

	priv, pub := DevSigningKey()
	prices := make(map[string]int64, len(assets))
	ids := make([]string, 0, len(assets))
	for id, a := range assets {
		prices[id] = a.fallbackMicroUSD
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := assets[id]
		kind := "native"
		if a.contract != "" {
			kind = "token " + a.contract
		}
		c.Logger.Printf("asset enabled: %s on %s (%s) = %s", a.symbol, a.chain.slug, kind, id)
	}

	return &Server{
		cfg:     c,
		priv:    priv,
		pubKey:  pub,
		drivers: drivers,
		assets:  assets,
		log:     c.Logger,
		prices:  prices,
		swaps:   make(map[string]*swapState),
	}, nil
}

// PubKey returns the "ed25519:"-prefixed base58 public key quotes are
// signed with.
func (s *Server) PubKey() string { return s.pubKey }

// Run stores the root context governing swap goroutines and starts the
// rates poller. It returns immediately; cancel ctx to stop all background
// work. Must be called before Handler is served.
func (s *Server) Run(ctx context.Context) {
	s.ctx = ctx
	if s.cfg.RatesURL != "" {
		go s.pollRates(ctx)
	}
}

// Handler returns the HTTP handler serving the mock API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/tokens", s.handleTokens)
	mux.HandleFunc("POST /v0/quote", s.handleQuote)
	mux.HandleFunc("GET /v0/status", s.handleStatus)
	mux.HandleFunc("POST /v0/deposit/submit", s.handleSubmitDeposit)
	mux.HandleFunc("POST /test/force", s.handleForce)
	mux.HandleFunc("GET /test/swaps", s.handleTestSwaps)
	return mux
}

// price returns the current microUSD price for an enabled asset's Intents
// ID.
func (s *Server) price(intentsID string) int64 {
	s.priceMtx.RLock()
	defer s.priceMtx.RUnlock()
	return s.prices[intentsID]
}

// writeError writes the service's non-200 envelope.
func writeError(w http.ResponseWriter, statusCode int, errorName, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]any{
		"message":    fmt.Sprintf(format, args...),
		"error":      errorName,
		"statusCode": statusCode,
	})
}

func writeJSON(w http.ResponseWriter, thing any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(thing); err != nil {
		log.Printf("error encoding response: %v", err)
	}
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	ids := make([]string, 0, len(s.assets))
	for id := range s.assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := time.Now().UTC().Format(timestampFormat)
	tokens := make([]*intents.Token, 0, len(ids))
	for _, id := range ids {
		a := s.assets[id]
		tokens = append(tokens, &intents.Token{
			AssetID:         a.intentsID,
			Decimals:        a.decimals,
			Blockchain:      a.chain.slug,
			Symbol:          a.symbol,
			Price:           float64(s.price(id)) / 1e6,
			PriceUpdatedAt:  now,
			ContractAddress: a.contract,
		})
	}
	writeJSON(w, tokens)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("depositAddress")
	s.swapMtx.RLock()
	sw := s.swaps[addr]
	s.swapMtx.RUnlock()
	if sw == nil {
		writeError(w, http.StatusNotFound, "Not Found", "Swap not found for deposit address %s", addr)
		return
	}
	writeJSON(w, sw.statusResponse())
}

func (s *Server) handleSubmitDeposit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DepositAddress string `json:"depositAddress"`
		TxHash         string `json:"txHash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "bad request body: %v", err)
		return
	}
	s.swapMtx.RLock()
	sw := s.swaps[req.DepositAddress]
	s.swapMtx.RUnlock()
	if sw == nil {
		writeError(w, http.StatusNotFound, "Not Found", "Swap not found for deposit address %s", req.DepositAddress)
		return
	}
	sw.setDepositHint(req.TxHash)
	writeJSON(w, struct{}{})
}

func (s *Server) handleForce(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DepositAddress string `json:"depositAddress"`
		Outcome        string `json:"outcome"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "bad request body: %v", err)
		return
	}
	switch req.Outcome {
	case forceRefund, forceFail, forceStall:
	case forceResume:
		req.Outcome = ""
	default:
		writeError(w, http.StatusBadRequest, "Bad Request",
			"outcome must be one of REFUND, FAIL, STALL, RESUME")
		return
	}
	s.swapMtx.RLock()
	sw := s.swaps[req.DepositAddress]
	s.swapMtx.RUnlock()
	if sw == nil {
		writeError(w, http.StatusNotFound, "Not Found", "Swap not found for deposit address %s", req.DepositAddress)
		return
	}
	sw.setForced(req.Outcome)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleTestSwaps(w http.ResponseWriter, r *http.Request) {
	s.swapMtx.RLock()
	swaps := make([]*swapState, 0, len(s.swaps))
	for _, sw := range s.swaps {
		swaps = append(swaps, sw)
	}
	s.swapMtx.RUnlock()
	sort.Slice(swaps, func(i, j int) bool { return swaps[i].createdAt.Before(swaps[j].createdAt) })
	out := make([]any, 0, len(swaps))
	for _, sw := range swaps {
		out = append(out, sw.debugSummary())
	}
	writeJSON(w, out)
}

// newCorrelationID returns a random hex identifier.
func newCorrelationID() string {
	var b [8]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b[:])
}
