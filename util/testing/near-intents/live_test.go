//go:build harness

package nearintentsmock

// These tests require running harnesses for the chains involved:
//   util/testing/btc/harness.sh
//   util/testing/eth/harness.sh
//   util/testing/near/harness.sh
//   util/testing/zec/harness.sh (the zec tests need only this one)
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
	"strings"
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

// TestHarnessBTCToUSDC swaps sats for harness-deployed test USDC on the
// live btc and eth harnesses, exercising the token payout path
// (harness-ctl/sendUSDC) and balanceOf-based verification.
func TestHarnessBTCToUSDC(t *testing.T) {
	rig := newHarnessRig(t, "btc", "eth")

	ethDrv, is := rig.server.drivers["eth"].(TokenDriver)
	if !is || len(ethDrv.Tokens()) == 0 {
		t.Fatal("eth harness has no deployed test tokens (re-run the eth harness)")
	}
	usdc := ethDrv.Tokens()[0]
	usdcID := "nep141:eth-" + strings.ToLower(usdc.contract) + ".omft.near"
	ctx := context.Background()

	recipient, err := rig.server.drivers["eth"].NewDepositAddress()
	if err != nil {
		t.Fatalf("recipient address: %v", err)
	}

	req := &intents.QuoteRequest{
		SwapType:          intents.SwapTypeExactInput,
		SlippageTolerance: 100,
		OriginAsset:       "nep141:btc.omft.near",
		DepositType:       intents.LocationOriginChain,
		DestinationAsset:  usdcID,
		Amount:            "1000000", // 0.01 BTC -> ~600 USDC at the $60k fallback
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
	t.Logf("deposit address %s, quoted %s USDC units out", depositAddr, resp.Quote.AmountOut)

	txid, err := runScript(harnessCtlDir(t, "btc"), "alpha", "sendtoaddress", depositAddr, "0.01")
	if err != nil {
		t.Fatalf("deposit send: %v", err)
	}
	if err := rig.client.SubmitDeposit(ctx, depositAddr, txid); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}

	waitStatusLive(t, rig, depositAddr, intents.StatusSuccess, time.Minute)

	wantOut, _ := new(big.Int).SetString(resp.Quote.AmountOut, 10)
	var got *big.Int
	for range 30 {
		if got, _, err = ethDrv.CheckTokenDeposit(usdc.contract, recipient); err != nil {
			t.Fatalf("CheckTokenDeposit: %v", err)
		}
		if got.Sign() > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if got == nil || got.Cmp(wantOut) != 0 {
		t.Fatalf("recipient USDC balance = %v, want %s", got, wantOut)
	}
	t.Logf("received %s USDC units at %s", got, recipient)
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

// TestHarnessZecDriver exercises the zec driver against the live zec harness
// alone: a deposit paid by the beta funder is seen while unmined and again
// once mined without being counted twice, and a payout from the alpha funder
// reaches a transparent address.
//
//	util/testing/zec/harness.sh
func TestHarnessZecDriver(t *testing.T) {
	ctlDir := harnessCtlDir(t, "zec")
	d, err := newZecDriver(filepath.Dir(filepath.Dir(ctlDir)))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	waitDeposit := func(addr string, want *big.Int) string {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			amt, txid, err := d.CheckDeposit(addr)
			if err != nil {
				t.Fatalf("CheckDeposit: %v", err)
			}
			if amt.Cmp(want) == 0 {
				return txid
			}
			if amt.Sign() != 0 {
				t.Fatalf("CheckDeposit = %s, want %s", amt, want)
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("deposit of %s to %s never seen", want, addr)
		return ""
	}

	// Deposit from the beta funder. The transaction is submitted to the beta
	// node and takes a few seconds to reach alpha's mempool.
	depositAddr, err := d.NewDepositAddress()
	if err != nil {
		t.Fatal(err)
	}
	out, err := runScript(ctlDir, "send-beta", depositAddr, "1.5")
	if err != nil {
		t.Fatalf("send-beta: %v", err)
	}
	lines := strings.Split(out, "\n")
	depositTxid := lines[len(lines)-1]
	deposit := big.NewInt(150_000_000)
	if txid := waitDeposit(depositAddr, deposit); txid != depositTxid {
		t.Fatalf("unmined deposit txid = %s, want %s", txid, depositTxid)
	}
	if err := d.Confirm(); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if txid := waitDeposit(depositAddr, deposit); txid != depositTxid {
		t.Fatalf("mined deposit txid = %s, want %s", txid, depositTxid)
	}

	// Payout from the alpha funder.
	payoutAddr, err := d.NewDepositAddress()
	if err != nil {
		t.Fatal(err)
	}
	payout := big.NewInt(25_000_000)
	payoutTxid, err := d.Send(payoutAddr, payout)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if txid := waitDeposit(payoutAddr, payout); txid != payoutTxid {
		t.Fatalf("payout txid = %s, want %s", txid, payoutTxid)
	}
	if err := d.Confirm(); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
}

// TestHarnessZECSwaps runs a swap in each direction between the live zec
// harness and an in-memory btc chain, so only the zec harness is needed.
//
//	util/testing/zec/harness.sh
func TestHarnessZECSwaps(t *testing.T) {
	ctlDir := harnessCtlDir(t, "zec")
	zec, err := newZecDriver(filepath.Dir(filepath.Dir(ctlDir)))
	if err != nil {
		t.Fatal(err)
	}
	btc := newFakeDriver("btc")
	s, err := New(&Config{
		Drivers:         map[string]Driver{"zec": zec, "btc": btc},
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
	rig := &testRig{server: s, client: c}

	quote := func(origin, dest, amount, recipient string) *intents.QuoteResponse {
		t.Helper()
		resp, err := c.Quote(ctx, &intents.QuoteRequest{
			SwapType:          intents.SwapTypeExactInput,
			SlippageTolerance: 100,
			OriginAsset:       origin,
			DepositType:       intents.LocationOriginChain,
			DestinationAsset:  dest,
			Amount:            amount,
			RefundTo:          "refundnotused",
			RefundType:        intents.LocationOriginChain,
			Recipient:         recipient,
			RecipientType:     intents.LocationDestinationChain,
			Deadline:          testDeadline(time.Hour),
		})
		if err != nil {
			t.Fatalf("Quote: %v", err)
		}
		return resp
	}
	const zecAsset, btcAsset = "nep141:zec.omft.near", "nep141:btc.omft.near"

	// ZEC in. The deposit is paid by the beta funder and no transaction ID
	// is submitted, so the origin hash has to come from the driver.
	resp := quote(zecAsset, btcAsset, "100000000", "btcrecipient") // 1 ZEC
	depositAddr := resp.Quote.DepositAddress
	out, err := runScript(ctlDir, "send-beta", depositAddr, "1")
	if err != nil {
		t.Fatalf("send-beta: %v", err)
	}
	lines := strings.Split(out, "\n")
	depositTxid := lines[len(lines)-1]
	st := waitStatusLive(t, rig, depositAddr, intents.StatusSuccess, time.Minute)
	if st.SwapDetails.OriginChainTxs[0].Hash != depositTxid {
		t.Fatalf("origin tx = %v, want %s", st.SwapDetails.OriginChainTxs, depositTxid)
	}
	wantOut, _ := new(big.Int).SetString(resp.Quote.AmountOut, 10)
	if got := btc.sentTo("btcrecipient"); got == nil || got.Cmp(wantOut) != 0 {
		t.Fatalf("btc payout = %v, want %s", got, wantOut)
	}

	// ZEC out, paid by the alpha funder to a transparent address.
	recipient, err := zec.NewDepositAddress()
	if err != nil {
		t.Fatal(err)
	}
	resp = quote(btcAsset, zecAsset, "100000", recipient) // 0.001 BTC
	depositAddr = resp.Quote.DepositAddress
	amountIn, _ := new(big.Int).SetString(resp.Quote.AmountIn, 10)
	btc.fund(depositAddr, amountIn)
	st = waitStatusLive(t, rig, depositAddr, intents.StatusSuccess, time.Minute)
	if len(st.SwapDetails.DestinationChainTxs) == 0 {
		t.Fatal("no destination tx hash")
	}
	wantOut, _ = new(big.Int).SetString(resp.Quote.AmountOut, 10)
	got, txid, err := zec.CheckDeposit(recipient)
	if err != nil {
		t.Fatalf("CheckDeposit: %v", err)
	}
	if got.Cmp(wantOut) != 0 {
		t.Fatalf("zec recipient received %s, want %s", got, wantOut)
	}
	if txid != st.SwapDetails.DestinationChainTxs[0].Hash {
		t.Fatalf("destination tx = %v, want %s", st.SwapDetails.DestinationChainTxs, txid)
	}
	t.Logf("received %s zats at %s", got, recipient)
}
