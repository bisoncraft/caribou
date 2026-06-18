// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/gorilla/websocket"
)

const (
	wsPingInterval  = 30 * time.Second
	wsReadDeadline  = 70 * time.Second // must exceed ping interval
	wsWriteDeadline = 10 * time.Second
	wsReconnectBase = 1 * time.Second
	wsReconnectMax  = 30 * time.Second
	wsStableAfter   = 5 * time.Second // reset backoff after this long
)

// wsSubscribeMsg is sent to subscribe to a feed.
type wsSubscribeMsg struct {
	Method       string `json:"method"`
	Subscription struct {
		Type string `json:"type"`
		User string `json:"user"`
	} `json:"subscription"`
}

// wsIncomingMsg is the envelope for all incoming WebSocket messages.
type wsIncomingMsg struct {
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

// wsLedgerData is the payload for userNonFundingLedgerUpdates events.
// IsSnapshot is true for the initial burst sent on subscription.
type wsLedgerData struct {
	IsSnapshot bool           `json:"isSnapshot"`
	Updates    []ledgerUpdate `json:"updates"`
}

// wsClient manages a persistent WebSocket connection that subscribes to
// userNonFundingLedgerUpdates for a single user address. It reconnects
// automatically on disconnect using exponential backoff.
//
// Goroutine ownership: run owns the write side; an internal goroutine owns
// the read side. Gorilla WebSocket allows exactly one concurrent reader and
// one concurrent writer.
type wsClient struct {
	url     string
	user    string
	log     util.Logger
	netDial func(ctx context.Context, network, addr string) (net.Conn, error) // nil = default

	// onEvent is called for every ledger update, including the initial snapshot.
	onEvent func(u ledgerUpdate)
	// onLive is called whenever the connection proves liveness (data or pong).
	onLive func()
	// onReconnect is called after each successful dial, before the message loop.
	// Typically used to fetch missed events via REST before resuming real-time.
	onReconnect func(ctx context.Context) error
}

// run connects, subscribes, and reads messages until ctx is done. It
// reconnects with exponential backoff on any disconnect or error.
func (c *wsClient) run(ctx context.Context) {
	backoff := wsReconnectBase
	for {
		start := time.Now()
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) >= wsStableAfter {
			backoff = wsReconnectBase
		}
		if err != nil {
			c.log.Warnf("WS disconnected (%v) — reconnecting in %s", err, backoff)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < wsReconnectMax {
			backoff *= 2
		}
	}
}

// runOnce manages one WebSocket connection from dial to close.
func (c *wsClient) runOnce(ctx context.Context) error {
	d := &websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}
	if c.netDial != nil {
		d.NetDialContext = c.netDial
	}

	conn, _, err := d.DialContext(ctx, c.url, http.Header{})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// Catch up on missed events before entering the live message loop.
	if c.onReconnect != nil {
		if err := c.onReconnect(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("catchup: %w", err)
		}
	}

	conn.SetPongHandler(func(_ string) error {
		conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
		if c.onLive != nil {
			c.onLive()
		}
		return nil
	})
	conn.SetReadDeadline(time.Now().Add(wsReadDeadline))

	// Subscribe to ledger events for this address.
	var sub wsSubscribeMsg
	sub.Method = "subscribe"
	sub.Subscription.Type = "userNonFundingLedgerUpdates"
	sub.Subscription.User = c.user
	conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
	if err := conn.WriteJSON(sub); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// Read loop runs on its own goroutine; write loop (pings) runs here.
	// readErr has capacity 1: the goroutine sends once on exit and never
	// blocks, even after runOnce has already returned and stopped draining.
	readErr := make(chan error, 1)
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
			// Any successfully received message proves the connection is
			// alive — call onLive before parsing so pings, subscription
			// confirmations, and empty data frames all refresh tipTime.
			if c.onLive != nil {
				c.onLive()
			}
			c.handleMessage(raw)
		}
	}()

	pingTicker := time.NewTicker(wsPingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return fmt.Errorf("ping: %w", err)
			}

		case err := <-readErr:
			return err

		case <-ctx.Done():
			conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return nil
		}
	}
}

func (c *wsClient) handleMessage(raw []byte) {
	var msg wsIncomingMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		c.log.Tracef("WS parse: %v", err)
		return
	}
	if msg.Channel != "userNonFundingLedgerUpdates" {
		return
	}

	var data wsLedgerData
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		c.log.Tracef("WS parse ledger data: %v", err)
		return
	}
	for _, u := range data.Updates {
		if c.onEvent != nil {
			c.onEvent(u)
		}
	}
}
