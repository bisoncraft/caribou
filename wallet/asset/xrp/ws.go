// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/gorilla/websocket"
)

const (
	wsWriteTimeout      = 5 * time.Second
	wsReconnectInterval = 2 * time.Second
	maxWSReconnectWait  = 30 * time.Second
	wsPingInterval      = 20 * time.Second
	wsPongWait          = 30 * time.Second
)

type wsEnvelope struct {
	Type string `json:"type"`
}

type wsLedgerClosed struct {
	Type        string `json:"type"`
	LedgerIndex uint64 `json:"ledger_index"`
	ReserveBase uint64 `json:"reserve_base"`
	ReserveInc  uint64 `json:"reserve_inc"`
}

type wsSubscribeResponse struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Result struct {
		LedgerIndex uint64 `json:"ledger_index"`
	} `json:"result"`
	Error        string `json:"error"`
	ErrorMessage string `json:"error_message"`
}

type wsTransactionEvent struct {
	Type         string   `json:"type"`
	Hash         string   `json:"hash"` // v2: top-level hash; absent in tx_json
	Validated    bool     `json:"validated"`
	LedgerIndex  uint64   `json:"ledger_index"`
	CloseTimeISO string   `json:"close_time_iso"`
	Meta         txMeta   `json:"meta"`
	TxJSON       txResult `json:"tx_json"`
}

func (w *xrpWallet) subscriptionLoop(ctx context.Context) {
	wsEndpoint := w.wsEndpoint()
	if wsEndpoint == "" {
		w.log.Errorf("No XRP websocket endpoint configured; live updates disabled")
		if w.peersChange != nil {
			w.peersChange(0, fmt.Errorf("no websocket endpoint configured"))
		}
		return
	}

	wait := wsReconnectInterval
	for {
		subscribed, err := w.runSubscription(ctx, wsEndpoint)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.log.Warnf("XRP websocket update loop disconnected: %v", err)
		}
		if w.peersChange != nil && w.peersUp.CompareAndSwap(true, false) {
			w.peersChange(0, err)
		}

		// After a successfully-subscribed connection drops, reconnect quickly.
		// After a failed or never-subscribed connection, apply exponential backoff.
		if subscribed {
			wait = wsReconnectInterval
		} else if wait < maxWSReconnectWait {
			wait *= 2
			if wait > maxWSReconnectWait {
				wait = maxWSReconnectWait
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w *xrpWallet) runSubscription(ctx context.Context, endpoint string) (subscribed bool, _ error) {
	// torproxy: NetDialContext is the proxy dialer when configured
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		NetDialContext:   dexnet.ProxyDialContext(w.torProxy),
	}
	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("error connecting websocket %q: %w", endpoint, err)
	}
	defer conn.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	subscribeReq := map[string]any{
		"id":          "caribou-xrp",
		"command":     "subscribe",
		"api_version": 2,
		"streams":     []string{"ledger"},
		"accounts":    []string{w.address},
	}
	if err := conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return false, err
	}
	if err := conn.WriteJSON(subscribeReq); err != nil {
		return false, fmt.Errorf("error sending websocket subscribe request: %w", err)
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return false, err
	}

	// Read the subscribe acknowledgement before entering the event loop.
	// A rejected subscribe is a fatal connection error; only signal peers-up
	// after the ack confirms the subscription is active.
	if err := conn.SetReadDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return false, err
	}
	_, ackMsg, err := conn.ReadMessage()
	if err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		return false, fmt.Errorf("error reading subscribe ack: %w", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return false, err
	}
	var ack wsSubscribeResponse
	if err := json.Unmarshal(ackMsg, &ack); err != nil {
		return false, fmt.Errorf("error decoding subscribe ack: %w", err)
	}
	if ack.Status != "" && ack.Status != "success" {
		return false, fmt.Errorf("subscribe rejected: %s: %s", ack.Error, ack.ErrorMessage)
	}

	// Seed the initial tip from the ack's ledger state if available.
	if ack.Result.LedgerIndex > 0 {
		select {
		case w.tipCh <- ack.Result.LedgerIndex:
		default:
		}
	}

	if w.peersChange != nil && w.peersUp.CompareAndSwap(false, true) {
		w.peersChange(1, nil)
	}
	subscribed = true

	// Pong handler refreshes the read deadline to detect wedged connections.
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	// Ping goroutine: sends periodic pings to detect silently wedged connections.
	pingTicker := time.NewTicker(wsPingInterval)
	defer pingTicker.Stop()
	go func() {
		for {
			select {
			case <-done:
				return
			case <-pingTicker.C:
				_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
				_ = conn.WriteMessage(websocket.PingMessage, nil)
				_ = conn.SetWriteDeadline(time.Time{})
			}
		}
	}()

	// Set the initial read deadline; refreshed by the pong handler.
	if err := conn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
		return subscribed, err
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return subscribed, nil
			}
			return subscribed, err
		}
		if err := w.handleWSMessage(msg); err != nil {
			w.log.Warnf("Ignoring malformed XRP websocket notification: %v", err)
		}
	}
}

func (w *xrpWallet) handleWSMessage(msg []byte) error {
	var env wsEnvelope
	if err := json.Unmarshal(msg, &env); err != nil {
		return fmt.Errorf("error decoding websocket envelope: %w", err)
	}

	switch env.Type {
	case "response":
		var resp wsSubscribeResponse
		if err := json.Unmarshal(msg, &resp); err != nil {
			return fmt.Errorf("error decoding websocket response: %w", err)
		}
		if resp.Status != "" && resp.Status != "success" {
			return fmt.Errorf("websocket subscribe failed: %s: %s", resp.Error, resp.ErrorMessage)
		}
		return nil

	case "ledgerClosed":
		var ledger wsLedgerClosed
		if err := json.Unmarshal(msg, &ledger); err != nil {
			return fmt.Errorf("error decoding ledgerClosed message: %w", err)
		}
		if ledger.ReserveBase > 0 && ledger.ReserveInc > 0 {
			w.reserveMtx.Lock()
			w.baseReserve = ledger.ReserveBase
			w.ownerReserve = ledger.ReserveInc
			w.reserveMtx.Unlock()
		}
		// Send to the tip worker for async processing; drop if channel is full
		// (worker is busy — it will coalesce with the next tip).
		select {
		case w.tipCh <- ledger.LedgerIndex:
		default:
		}
		return nil

	case "transaction":
		var evt wsTransactionEvent
		if err := json.Unmarshal(msg, &evt); err != nil {
			return fmt.Errorf("error decoding transaction message: %w", err)
		}
		if !evt.Validated {
			return nil
		}
		// API v2 places the hash at the top level; copy it into TxJSON so
		// walletTransactionFromTx can read it regardless of API version.
		if evt.Hash != "" && evt.TxJSON.Hash == "" {
			evt.TxJSON.Hash = evt.Hash
		}
		evt.TxJSON.Validated = true
		evt.TxJSON.LedgerIndex = evt.LedgerIndex
		if evt.CloseTimeISO != "" {
			evt.TxJSON.CloseTimeISO = evt.CloseTimeISO
		}
		w.processObservedTransaction(&evt.TxJSON, evt.Meta, evt.LedgerIndex)
		return nil
	}

	return nil
}

func (w *xrpWallet) processObservedTransaction(tr *txResult, meta txMeta, ledgerIndex uint64) {
	wtx := walletTransactionFromTx(tr, w.address, meta, true, ledgerIndex)
	if wtx == nil {
		wtx = w.tokenTransaction(tr, meta, true, ledgerIndex)
	}
	if wtx == nil {
		// A TrustSet this wallet does not record (the issuer's side of a
		// line, or an unregistered currency) still changed a trust line on
		// this account; the balance refresh re-derives the tokens' state.
		if tr.TransactionType == "TrustSet" {
			w.emitBalanceChange()
		}
		return
	}

	emitTx := wtx
	emitNew := true

	if w.txDB != nil {
		existing, err := w.txDB.GetTx(wtx.ID)
		if err != nil {
			w.log.Errorf("getTx %s: %v", wtx.ID, err)
			return
		}

		if existing != nil {
			if existing.BlockNumber == ledgerIndex && existing.Confirmed && existing.Rejected == wtx.Rejected {
				return
			}

			existing.Type = wtx.Type
			existing.Amount = wtx.Amount
			existing.Fees = wtx.Fees
			existing.BlockNumber = ledgerIndex
			existing.Confirmed = true
			existing.Rejected = wtx.Rejected
			existing.Recipient = wtx.Recipient
			if ct := tr.closeTime(); ct > 0 {
				existing.Timestamp = ct
			}
			if err := w.txDB.StoreTx(existing); err != nil {
				w.log.Errorf("Error updating observed tx %s: %v", wtx.ID, err)
				return
			}
			emitTx = existing
			emitNew = false
		} else {
			if err := w.storeNewTx(wtx, tr); err != nil {
				w.log.Errorf("Error storing observed tx %s: %v", wtx.ID, err)
				return
			}
		}
	}

	w.emitTxNote(emitTx, emitNew)
	w.emitBalanceChange()
}

func deriveWSEndpoint(rpcEndpoint string) (string, error) {
	u, err := url.Parse(rpcEndpoint)
	if err != nil {
		return "", err
	}

	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported endpoint scheme %q", u.Scheme)
	}

	// rippled commonly exposes JSON-RPC on 51234 and WebSocket on 51233.
	if u.Port() == "51234" {
		u.Host = net.JoinHostPort(u.Hostname(), "51233")
	}

	if u.Path == "" {
		u.Path = "/"
	}

	return u.String(), nil
}
