//go:build harness

package nearintentsmock

// These tests require running harnesses for the chains involved:
//   util/testing/btc/harness.sh
//   util/testing/eth/harness.sh
//   util/testing/near/harness.sh
// Run with: go test -v -tags harness -run TestHarness ./util/testing/near-intents/
//
// Swaps move real simnet funds: deposits are sent from the harness wallets
// (btc alpha, NEAR test.near) and payouts land on harness-reachable
// addresses.

import (
	"context"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/intents"
)

func newHarnessRig(t *testing.T, chains ...string) *testRig {
	t.Helper()
	s, err := New(&Config{
		Chains:          chains,
		ProcessingDelay: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.Run(ctx)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	c, err := intents.NewClient(&intents.Config{
		BaseURL:    srv.URL,
		SigningKey: s.PubKey(),
		AppFees:    []intents.AppFee{{Recipient: "0xfeeaccount", Fee: 20}},
	}, testLogger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return &testRig{server: s, client: c}
}

func waitStatusLive(t *testing.T, rig *testRig, addr, want string, timeout time.Duration) *intents.StatusResponse {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		st, err := rig.client.Status(context.Background(), addr)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Status == want {
			return st
		}
		if intents.TerminalStatus(st.Status) {
			t.Fatalf("swap terminal at %s, want %s", st.Status, want)
		}
		last = st.Status
		time.Sleep(time.Second)
	}
	t.Fatalf("swap never reached %s (last %s)", want, last)
	return nil
}

func harnessCtlDir(t *testing.T, chain string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, "dextest", chain, "harness-ctl")
}

// TestHarnessBTCToETH swaps sats for wei across the live btc and eth
// harnesses.
func TestHarnessBTCToETH(t *testing.T) {
	rig := newHarnessRig(t, "btc", "eth")
	ctx := context.Background()

	ethDrv := rig.server.drivers["eth"]
	recipient, err := ethDrv.NewDepositAddress()
	if err != nil {
		t.Fatalf("recipient address: %v", err)
	}

	req := &intents.QuoteRequest{
		SwapType:          intents.SwapTypeExactInput,
		SlippageTolerance: 100,
		OriginAsset:       "nep141:btc.omft.near",
		DepositType:       intents.LocationOriginChain,
		DestinationAsset:  "nep141:eth.omft.near",
		Amount:            "10000000", // 0.1 BTC
		RefundTo:          "bcrt1qrefundnotused",
		RefundType:        intents.LocationOriginChain,
		Recipient:         recipient,
		RecipientType:     intents.LocationDestinationChain,
		Deadline:          testDeadline(time.Hour),
	}
	resp, err := rig.client.Quote(ctx, req)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	depositAddr := resp.Quote.DepositAddress
	t.Logf("deposit address %s, quoted %s wei out", depositAddr, resp.Quote.AmountOut)

	// Send the deposit the way a wallet would, then submit the txid.
	txid, err := runScript(harnessCtlDir(t, "btc"), "alpha", "sendtoaddress", depositAddr, "0.1")
	if err != nil {
		t.Fatalf("deposit send: %v", err)
	}
	if err := rig.client.SubmitDeposit(ctx, depositAddr, txid); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}

	st := waitStatusLive(t, rig, depositAddr, intents.StatusSuccess, time.Minute)
	if len(st.SwapDetails.DestinationChainTxs) == 0 {
		t.Fatal("no destination tx hash")
	}
	if st.SwapDetails.OriginChainTxs[0].Hash != txid {
		t.Fatalf("origin tx = %v, want %s", st.SwapDetails.OriginChainTxs, txid)
	}

	wantOut, _ := new(big.Int).SetString(resp.Quote.AmountOut, 10)
	// geth dev mode mines on its own schedule; allow time for the payout
	// to land.
	var got *big.Int
	for range 30 {
		if got, _, err = ethDrv.CheckDeposit(recipient); err != nil {
			t.Fatalf("CheckDeposit: %v", err)
		}
		if got.Sign() > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	// sendtoaddress parses a 6-decimal conventional amount, so the
	// delivered value may differ from the quote by less than 1e12 wei.
	diff := new(big.Int).Abs(new(big.Int).Sub(got, wantOut))
	if diff.Cmp(big.NewInt(1e12)) > 0 {
		t.Fatalf("recipient balance = %s, want ~%s", got, wantOut)
	}
	t.Logf("received %s wei at %s", got, recipient)
}

// TestHarnessNEARToBTC swaps yoctoNEAR for sats across the live near and
// btc harnesses.
func TestHarnessNEARToBTC(t *testing.T) {
	rig := newHarnessRig(t, "btc", "near")
	ctx := context.Background()

	btcDrv := rig.server.drivers["btc"]
	recipient, err := btcDrv.NewDepositAddress()
	if err != nil {
		t.Fatalf("recipient address: %v", err)
	}

	req := &intents.QuoteRequest{
		SwapType:          intents.SwapTypeExactInput,
		SlippageTolerance: 100,
		OriginAsset:       "nep141:wrap.near",
		DepositType:       intents.LocationOriginChain,
		DestinationAsset:  "nep141:btc.omft.near",
		Amount:            "100" + "000000000000000000000000", // 100 NEAR
		RefundTo:          "refundnotused.near",
		RefundType:        intents.LocationOriginChain,
		Recipient:         recipient,
		RecipientType:     intents.LocationDestinationChain,
		Deadline:          testDeadline(time.Hour),
	}
	resp, err := rig.client.Quote(ctx, req)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	depositAddr := resp.Quote.DepositAddress
	t.Logf("deposit address %s, quoted %s sats out", depositAddr, resp.Quote.AmountOut)

	home, _ := os.UserHomeDir()
	out, err := runScript(filepath.Join(home, "dextest", "near"), "send", depositAddr, "100")
	if err != nil {
		t.Fatalf("deposit send: %v", err)
	}
	t.Logf("near send: %s", out)
	if err := rig.client.SubmitDeposit(ctx, depositAddr, "neardeposittx"); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}

	st := waitStatusLive(t, rig, depositAddr, intents.StatusSuccess, time.Minute)
	if len(st.SwapDetails.DestinationChainTxs) == 0 {
		t.Fatal("no destination tx hash")
	}

	wantOut, _ := new(big.Int).SetString(resp.Quote.AmountOut, 10)
	got, _, err := btcDrv.CheckDeposit(recipient)
	if err != nil {
		t.Fatalf("CheckDeposit: %v", err)
	}
	// sendtoaddress takes an 8-decimal amount; exact delivery expected.
	if got.Cmp(wantOut) != 0 {
		t.Fatalf("recipient balance = %s, want %s", got, wantOut)
	}
	t.Logf("received %s sats at %s", got, recipient)
}

// TestHarnessRates verifies the live-rate fetch against a running caribou
// app server, if one is up on the simnet default address.
func TestHarnessRates(t *testing.T) {
	rates, err := fetchFiatRates(context.Background(), "http://127.0.0.3:5758")
	if err != nil {
		t.Skipf("no caribou app server on simnet default address: %v", err)
	}
	t.Logf("fetched %d rates: %v", len(rates), rates)
}
