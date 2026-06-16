// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package hyperliquidmock implements a minimal in-memory reproduction of the
// Hyperliquid REST and WebSocket APIs that the bosswallet HYPE wallet relies
// on (spotClearinghouseState, spotMeta, userNonFundingLedgerUpdates, and the
// sendAsset exchange action). It exists so wallet behavior — including
// EIP-712 signature verification — can be exercised end-to-end without a
// funded Hyperliquid testnet account.
//
// It is test-only scaffolding: the real Hyperliquid API surface is much
// larger, and this server implements only the subset listed above.
package hyperliquidmock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/gorilla/websocket"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset/hyperliquid"
)

// hypeTokenID is the spot token identifier the mock advertises for HYPE via
// spotMeta. The wallet only cares that the "HYPE" name resolves to some
// "HYPE:<id>" string.
const hypeTokenID = "0x0000000000000000000000000000000000000000000000000000000000000000"

// mockFaucetAddress is the synthetic sender address used for ledger entries
// created by Fund/-test/fund deposits.
const mockFaucetAddress = "0x000000000000000000000000000000000000ad"

// --- wire types (mirroring the real Hyperliquid API shapes) ---

type spotBalance struct {
	Coin  string `json:"coin"`
	Total string `json:"total"`
	Hold  string `json:"hold"`
}

type spotClearinghouseState struct {
	Balances []spotBalance `json:"balances"`
}

type spotToken struct {
	Name       string `json:"name"`
	Index      int    `json:"index"`
	TokenID    string `json:"tokenId"`
	SzDecimals int    `json:"szDecimals"`
}

type spotMetaResponse struct {
	Tokens []spotToken `json:"tokens"`
}

type ledgerDelta struct {
	Type        string `json:"type"`
	Token       string `json:"token"`
	Amount      string `json:"amount"`
	User        string `json:"user"`
	Destination string `json:"destination"`
}

type ledgerUpdate struct {
	Time  uint64      `json:"time"`
	Hash  string      `json:"hash"`
	Delta ledgerDelta `json:"delta"`
}

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
	Action    json.RawMessage   `json:"action"`
	Nonce     uint64            `json:"nonce"`
	Signature exchangeSignature `json:"signature"`
}

type exchangeResponse struct {
	Status   string `json:"status"`
	Response string `json:"response"`
}

// --- account state ---

type account struct {
	totalAtoms uint64
	ledger     []ledgerUpdate
}

// Server is an in-memory mock of the Hyperliquid REST/WS API. The zero value
// is not usable; construct with New.
type Server struct {
	net util.Network

	mu       sync.Mutex
	accounts map[string]*account // key: lowercase 0x address
	subs     map[string][]chan ledgerUpdate
}

// New creates a mock server for the given network. The network determines
// which EIP-712 domain (chain ID and "hyperliquidChain" name) signatures are
// verified against, and must match the wallet-under-test's configured
// network.
func New(net util.Network) *Server {
	return &Server{
		net:      net,
		accounts: make(map[string]*account),
		subs:     make(map[string][]chan ledgerUpdate),
	}
}

// Fund credits address with amountAtoms of HYPE (in atoms, i.e. 1e-8 HYPE).
// It is a test-only helper with no equivalent in the real API — there is no
// faucet here, so tests seed balances directly. It records a "deposit" ledger
// entry and pushes it to any active WS subscription for address so the wallet
// picks up the new balance immediately rather than waiting for the next ping.
func (s *Server) Fund(address string, amountAtoms uint64) {
	addr := strings.ToLower(address)
	s.mu.Lock()
	defer s.mu.Unlock()
	acct := s.account(addr)
	acct.totalAtoms += amountAtoms

	if amountAtoms == 0 {
		return
	}

	update := ledgerUpdate{
		Time: uint64(time.Now().UnixMilli()),
		Hash: fmt.Sprintf("0x%x", time.Now().UnixNano()),
		Delta: ledgerDelta{
			Type:        "deposit",
			Token:       "HYPE",
			Amount:      atomsToHYPE(amountAtoms),
			User:        mockFaucetAddress,
			Destination: addr,
		},
	}
	acct.ledger = append(acct.ledger, update)

	for _, ch := range s.subs[addr] {
		select {
		case ch <- update:
		default:
		}
	}
}

// Balance returns address's current total HYPE balance in atoms.
func (s *Server) Balance(address string) uint64 {
	addr := strings.ToLower(address)
	s.mu.Lock()
	defer s.mu.Unlock()
	if acct, ok := s.accounts[addr]; ok {
		return acct.totalAtoms
	}
	return 0
}

// account returns the account for addr, creating it if necessary. Callers
// must hold s.mu.
func (s *Server) account(addr string) *account {
	acct, ok := s.accounts[addr]
	if !ok {
		acct = &account{}
		s.accounts[addr] = acct
	}
	return acct
}

// Handler returns an http.Handler implementing /info, /exchange, and /ws.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/info", s.handleInfo)
	mux.HandleFunc("/exchange", s.handleExchange)
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/test/fund", s.handleFund)
	return mux
}

// --- /test/fund ---
//
// handleFund is a test-only endpoint with no equivalent in the real
// Hyperliquid API. It exists because the mock has no faucet: callers credit
// an address with HYPE directly so they can exercise sends without a funded
// testnet account.

type fundRequest struct {
	Address     string `json:"address"`
	AmountAtoms uint64 `json:"amountAtoms"`
}

func (s *Server) handleFund(w http.ResponseWriter, r *http.Request) {
	var req fundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}
	if req.Address == "" {
		http.Error(w, "address is required", http.StatusBadRequest)
		return
	}
	s.Fund(req.Address, req.AmountAtoms)
	writeJSON(w, struct{}{})
}

// --- /info ---

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}

	switch req["type"] {
	case "spotClearinghouseState":
		s.respondSpotClearinghouseState(w, req)
	case "spotMeta":
		writeJSON(w, spotMetaResponse{Tokens: []spotToken{{
			Name:       "HYPE",
			Index:      0,
			TokenID:    hypeTokenID,
			SzDecimals: 8,
		}}})
	case "userNonFundingLedgerUpdates":
		s.respondLedgerUpdates(w, req)
	default:
		http.Error(w, fmt.Sprintf("unsupported info type %q", req["type"]), http.StatusBadRequest)
	}
}

func (s *Server) respondSpotClearinghouseState(w http.ResponseWriter, req map[string]any) {
	user, _ := req["user"].(string)
	addr := strings.ToLower(user)

	s.mu.Lock()
	var total uint64
	if acct, ok := s.accounts[addr]; ok {
		total = acct.totalAtoms
	}
	s.mu.Unlock()

	resp := spotClearinghouseState{}
	if total > 0 {
		resp.Balances = []spotBalance{{
			Coin:  "HYPE",
			Total: atomsToHYPE(total),
			Hold:  "0",
		}}
	}
	writeJSON(w, resp)
}

func (s *Server) respondLedgerUpdates(w http.ResponseWriter, req map[string]any) {
	user, _ := req["user"].(string)
	addr := strings.ToLower(user)

	var startTime int64
	switch v := req["startTime"].(type) {
	case float64:
		startTime = int64(v)
	}

	s.mu.Lock()
	acct := s.accounts[addr]
	var updates []ledgerUpdate
	if acct != nil {
		for _, u := range acct.ledger {
			if int64(u.Time) >= startTime {
				updates = append(updates, u)
			}
		}
	}
	s.mu.Unlock()

	if updates == nil {
		updates = []ledgerUpdate{}
	}
	writeJSON(w, updates)
}

// --- /exchange ---

func (s *Server) handleExchange(w http.ResponseWriter, r *http.Request) {
	var req exchangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}

	var typed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(req.Action, &typed); err != nil {
		http.Error(w, fmt.Sprintf("decode action: %v", err), http.StatusBadRequest)
		return
	}

	switch typed.Type {
	case "sendAsset":
		s.handleSendAsset(w, req)
	default:
		writeJSON(w, exchangeResponse{Status: "err", Response: fmt.Sprintf("unsupported action type %q", typed.Type)})
	}
}

func (s *Server) handleSendAsset(w http.ResponseWriter, req exchangeRequest) {
	var action sendAssetAction
	if err := json.Unmarshal(req.Action, &action); err != nil {
		http.Error(w, fmt.Sprintf("decode sendAsset action: %v", err), http.StatusBadRequest)
		return
	}

	sender, err := recoverSendAssetSigner(s.net, action, req.Signature)
	if err != nil {
		writeJSON(w, exchangeResponse{Status: "err", Response: fmt.Sprintf("bad signature: %v", err)})
		return
	}

	amount, err := parseHYPE(action.Amount)
	if err != nil {
		writeJSON(w, exchangeResponse{Status: "err", Response: fmt.Sprintf("bad amount: %v", err)})
		return
	}

	dest := strings.ToLower(action.Destination)

	s.mu.Lock()
	senderAcct := s.account(sender)
	if senderAcct.totalAtoms < amount {
		s.mu.Unlock()
		writeJSON(w, exchangeResponse{Status: "err", Response: "insufficient balance"})
		return
	}
	senderAcct.totalAtoms -= amount
	destAcct := s.account(dest)
	destAcct.totalAtoms += amount

	hash := fmt.Sprintf("0x%x", gethcrypto.Keccak256(req.Action, []byte(strconv.FormatUint(req.Nonce, 10))))
	update := ledgerUpdate{
		Time: req.Nonce,
		Hash: hash,
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      action.Amount,
			User:        sender,
			Destination: dest,
		},
	}
	senderAcct.ledger = append(senderAcct.ledger, update)
	if dest != sender {
		destAcct.ledger = append(destAcct.ledger, update)
	}

	subs := append([]chan ledgerUpdate{}, s.subs[sender]...)
	if dest != sender {
		subs = append(subs, s.subs[dest]...)
	}
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- update:
		default:
		}
	}

	writeJSON(w, exchangeResponse{Status: "ok", Response: hash})
}

// recoverSendAssetSigner verifies sig against the EIP-712 hash for action and
// returns the lowercase 0x address that produced it, using the same hashing
// logic as the production wallet (hyperliquid.SendAssetEIP712Hash).
func recoverSendAssetSigner(net util.Network, action sendAssetAction, sig exchangeSignature) (string, error) {
	hash := hyperliquid.SendAssetEIP712Hash(net, action.Destination, action.SourceDex, action.DestinationDex, action.Token, action.Amount, action.FromSubAccount, action.Nonce)

	rBytes, err := hexBytes(sig.R)
	if err != nil {
		return "", fmt.Errorf("decode r: %w", err)
	}
	sBytes, err := hexBytes(sig.S)
	if err != nil {
		return "", fmt.Errorf("decode s: %w", err)
	}
	if len(rBytes) != 32 || len(sBytes) != 32 {
		return "", fmt.Errorf("invalid r/s length")
	}
	if sig.V != 27 && sig.V != 28 {
		return "", fmt.Errorf("invalid v: %d", sig.V)
	}

	sigBytes := make([]byte, 65)
	copy(sigBytes[:32], rBytes)
	copy(sigBytes[32:64], sBytes)
	sigBytes[64] = sig.V - 27

	pub, err := gethcrypto.SigToPub(hash, sigBytes)
	if err != nil {
		return "", fmt.Errorf("recover pubkey: %w", err)
	}
	return strings.ToLower(gethcrypto.PubkeyToAddress(*pub).Hex()), nil
}

func hexBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 {
		s = "0" + s
	}
	b := make([]byte, len(s)/2)
	for i := range b {
		v, err := strconv.ParseUint(s[i*2:i*2+2], 16, 8)
		if err != nil {
			return nil, err
		}
		b[i] = byte(v)
	}
	return b, nil
}

// --- /ws ---

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

type wsSubscribeMsg struct {
	Method       string `json:"method"`
	Subscription struct {
		Type string `json:"type"`
		User string `json:"user"`
	} `json:"subscription"`
}

type wsIncomingMsg struct {
	Channel string `json:"channel"`
	Data    any    `json:"data"`
}

type wsLedgerData struct {
	IsSnapshot bool           `json:"isSnapshot"`
	Updates    []ledgerUpdate `json:"updates"`
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	var sub wsSubscribeMsg
	if err := conn.ReadJSON(&sub); err != nil {
		return
	}
	if sub.Method != "subscribe" || sub.Subscription.Type != "userNonFundingLedgerUpdates" {
		return
	}
	addr := strings.ToLower(sub.Subscription.User)

	ch := make(chan ledgerUpdate, 16)
	s.mu.Lock()
	acct := s.account(addr)
	snapshot := append([]ledgerUpdate{}, acct.ledger...)
	s.subs[addr] = append(s.subs[addr], ch)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		subs := s.subs[addr]
		for i, c := range subs {
			if c == ch {
				s.subs[addr] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()

	if err := conn.WriteJSON(wsIncomingMsg{
		Channel: "userNonFundingLedgerUpdates",
		Data:    wsLedgerData{IsSnapshot: true, Updates: snapshot},
	}); err != nil {
		return
	}

	// Drain client reads (pings etc.) so the connection stays alive, and
	// detect disconnects.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case update := <-ch:
			if err := conn.WriteJSON(wsIncomingMsg{
				Channel: "userNonFundingLedgerUpdates",
				Data:    wsLedgerData{IsSnapshot: false, Updates: []ledgerUpdate{update}},
			}); err != nil {
				return
			}
		case <-closed:
			return
		}
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// atomsToHYPE converts atoms to a decimal HYPE string, matching the format the
// real API returns.
func atomsToHYPE(atoms uint64) string {
	whole := atoms / 1e8
	frac := atoms % 1e8
	if frac == 0 {
		return strconv.FormatUint(whole, 10)
	}
	fracStr := fmt.Sprintf("%08d", frac)
	fracStr = strings.TrimRight(fracStr, "0")
	return fmt.Sprintf("%d.%s", whole, fracStr)
}

// parseHYPE parses a decimal HYPE string into atoms.
func parseHYPE(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid HYPE amount %q: %w", s, err)
	}
	atoms := whole * 1e8
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) > 8 {
			frac = frac[:8]
		}
		for len(frac) < 8 {
			frac += "0"
		}
		fracVal, err := strconv.ParseUint(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid HYPE decimal in %q: %w", s, err)
		}
		atoms += fracVal
	}
	return atoms, nil
}
