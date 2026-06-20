// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
)

// TestShutdownDrainsInflightOps hammers the public DB/RPC methods from several
// goroutines while shutdown() runs concurrently. The ordered drain (running flag
// + opWG, registered under lifecycleMtx) must guarantee no call touches the txDB
// after it is closed. Run under -race, a use-after-close (the gap atomic.Pointer
// alone leaves) trips the detector or panics in badger.
func TestShutdownDrainsInflightOps(t *testing.T) {
	log := util.StdOutLogger("HLRACE", util.LevelError)
	db, err := newTxDB(t.TempDir(), log)
	if err != nil {
		t.Fatalf("newTxDB: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		// Balance posts spotClearinghouseState; an empty balance set suffices.
		_ = json.NewEncoder(rw).Encode(spotClearinghouseState{Balances: []spotBalance{}})
	}))
	defer srv.Close()
	rpc, err := newRPCClient(srv.URL, "", log)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}

	w := &HyperWallet{
		log:     log,
		net:     util.Testnet,
		address: "0xabcdef1234567890abcdef1234567890abcdef12",
		rpc:     rpc,
	}
	w.txDB.Store(db)
	w.running = true

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = w.TxHistory(&txdb.TxHistoryRequest{N: 5})
				_ = w.PendingTransactions(context.Background())
				_, _ = w.Balance()
				_, _ = w.WalletTransaction(context.Background(), "0x0000000000000001")
			}
		}()
	}

	time.Sleep(20 * time.Millisecond)
	w.shutdown() // sets running=false, drains opWG, then closes the DB
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()

	// After shutdown the DB is closed and public calls fail closed.
	if _, err := w.Balance(); err == nil {
		t.Fatal("Balance should error after shutdown")
	}
}
