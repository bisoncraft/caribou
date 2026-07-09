// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquidmock

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bisoncraft/caribou/util"
)

func TestWSSnapshotAndPush(t *testing.T) {
	const addr = "0x000000000000000000000000000000000000aa"

	srv := New(util.Testnet)
	srv.Fund(addr, 1_00000000)

	// Seed a ledger entry so the snapshot is non-empty.
	srv.mu.Lock()
	acct := srv.account(strings.ToLower(addr))
	acct.ledger = append(acct.ledger, ledgerUpdate{
		Time: 1,
		Hash: "0xseed",
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "1",
			User:        "0xfrom",
			Destination: addr,
		},
	})
	srv.mu.Unlock()

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sub := wsSubscribeMsg{Method: "subscribe"}
	sub.Subscription.Type = "userNonFundingLedgerUpdates"
	sub.Subscription.User = addr
	if err := conn.WriteJSON(sub); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var snapshot wsIncomingMsg
	if err := conn.ReadJSON(&snapshot); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if snapshot.Channel != "userNonFundingLedgerUpdates" {
		t.Fatalf("unexpected channel: %s", snapshot.Channel)
	}

	// Now push a live update by performing a send into addr from a fresh sender.
	// Use the server's internal push path directly via a second send to keep
	// the test focused on WS delivery rather than signing.
	srv.mu.Lock()
	update := ledgerUpdate{
		Time: 2,
		Hash: "0xlive",
		Delta: ledgerDelta{
			Type:        "spotTransfer",
			Token:       "HYPE",
			Amount:      "1",
			User:        "0xfrom2",
			Destination: addr,
		},
	}
	acct = srv.account(strings.ToLower(addr))
	acct.ledger = append(acct.ledger, update)
	for _, ch := range srv.subs[strings.ToLower(addr)] {
		ch <- update
	}
	srv.mu.Unlock()

	var live wsIncomingMsg
	if err := conn.ReadJSON(&live); err != nil {
		t.Fatalf("read live update: %v", err)
	}
	if live.Channel != "userNonFundingLedgerUpdates" {
		t.Fatalf("unexpected channel: %s", live.Channel)
	}
}
