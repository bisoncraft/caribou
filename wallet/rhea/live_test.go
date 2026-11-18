//go:build live

// Read-only checks against NEAR mainnet, the RHEA relayer API, and 1Click.
// Nothing is signed or submitted. Run with:
//
//	go test -tags live ./wallet/rhea -run Live -v
//
// The checks hold the protocol layer to the deployment as it is now: the
// pinned templates and MCA code, the view decoders, the receipt decoders on
// the captured transactions read back from an archival endpoint, the
// relayer configuration and history formats, and the 1Click quote shapes
// for supplies with customRecipientMsg.

package rhea

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/near"
	"github.com/bisoncraft/caribou/wallet/intents"
)

// liveIdentity is the identity bound to liveMCA.
const (
	liveIdentity Identity = "242c9fc3162321d326892cfe044fc0d9ffc3e9b0"
	liveMCA               = "rhea001569.multica.near"
)

type liveViewer struct {
	*near.ViewClient
}

func (v liveViewer) CallFunction(ctx context.Context, contract, method string, args []byte, blockHash string) (*ViewResult, error) {
	res, err := v.ViewClient.CallFunction(ctx, contract, method, args, blockHash)
	if err != nil {
		return nil, err
	}
	return &ViewResult{Result: res.Result, BlockHeight: res.BlockHeight, BlockHash: res.BlockHash}, nil
}

func liveClient(t *testing.T) *near.ViewClient {
	t.Helper()
	c, err := near.NewViewClient(&near.ViewClientConfig{Net: util.Mainnet}, util.StdOutLogger(t.Name(), util.LevelInfo))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLiveViews(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	c := liveClient(t)
	r := NewReader(liveViewer{c}, Mainnet)
	block, err := c.FinalBlock(ctx)
	if err != nil {
		t.Fatalf("FinalBlock: %v", err)
	}
	B := block.Hash
	t.Logf("final block %d %s at %s", block.Height, B, block.Timestamp)

	if _, err := r.CheckSupplyTemplates(ctx, B); err != nil {
		t.Fatalf("supply templates no longer match the pinned bytes: %v", err)
	}
	if h, _, err := r.LatestMCACodeHash(ctx, B); err != nil {
		t.Fatalf("LatestMCACodeHash: %v", err)
	} else {
		t.Logf("latest MCA code hash %s", h)
	}
	mca, _, err := r.MCAByWallet(ctx, liveIdentity, B)
	if err != nil || mca != liveMCA {
		t.Fatalf("MCAByWallet: %q, %v", mca, err)
	}
	if n, _, err := r.Nonce(ctx, mca, B); err != nil || n == 0 {
		t.Fatalf("Nonce: %d, %v", n, err)
	}
	if _, _, err := r.MCAInitBalance(ctx, B); err != nil {
		t.Fatalf("MCAInitBalance: %v", err)
	}
	if _, _, err := r.AccountPositions(ctx, liveMCA, B); err != nil {
		t.Fatalf("AccountPositions: %v", err)
	}
	for _, m := range Mainnet.Markets {
		a, _, err := r.Asset(ctx, m.TokenID, B)
		if err != nil {
			t.Fatalf("%s Asset: %v", m.Symbol, err)
		}
		md, _, err := r.FTMetadata(ctx, m.TokenID, B)
		if err != nil {
			t.Fatalf("%s FTMetadata: %v", m.Symbol, err)
		}
		nv, _, err := r.NearValue(ctx, m.TokenID, B)
		if err != nil || nv.Sign() <= 0 {
			t.Fatalf("%s NearValue: %v, %v", m.Symbol, nv, err)
		}
		fee, _, err := r.CreateMCAFee(ctx, m.TokenID, B)
		if err != nil {
			t.Fatalf("%s CreateMCAFee: %v", m.Symbol, err)
		}
		t.Logf("%s: decimals %d, extra decimals %d, can deposit %t, can withdraw %t, near value %s, creation fee %s",
			m.Symbol, md.Decimals, a.Config.ExtraDecimals, a.Config.CanDeposit, a.Config.CanWithdraw, nv, fee)
	}
}

// TestLiveReceipts reads the captured transactions back from mainnet and
// checks that the decoders draw the same evidence from the live responses
// as from the fixtures.
func TestLiveReceipts(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	c := liveClient(t)
	for _, name := range []string{
		"tx-create-supply-1aqcDa8Y.json",
		"tx-supply-existing-Ey4MNC5d.json",
		"tx-exec-4SDDqzRj.json",
		"tx-exec-gaspayment-4175G6iY.json",
	} {
		fixture := loadTree(t, name)
		// As the engine reads them: whatever the endpoint knows, with the
		// archival fallback for transactions regular endpoints dropped.
		raw, err := c.TxStatus(ctx, fixture.Hash, fixture.SignerID, near.TxWaitNone)
		if err != nil {
			t.Fatalf("%s: TxStatus: %v", name, err)
		}
		live, err := DecodeTxTree(raw)
		if err != nil {
			t.Fatalf("%s: DecodeTxTree: %v", name, err)
		}
		if live.Hash != fixture.Hash {
			t.Fatalf("%s: live tree describes %s", name, live.Hash)
		}
		if err := live.Complete(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := live.EvidenceAvailable(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		same := func(what string, get func(*TxTree) (any, error)) {
			want, err := get(fixture)
			if err != nil {
				t.Fatalf("%s %s (fixture): %v", name, what, err)
			}
			got, err := get(live)
			if err != nil {
				t.Fatalf("%s %s (live): %v", name, what, err)
			}
			if !reflect.DeepEqual(got, want) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(want)
				t.Fatalf("%s %s differ:\nlive    %s\nfixture %s", name, what, g, w)
			}
		}
		same("transfers", func(tr *TxTree) (any, error) { return tr.FTTransfers() })
		same("Burrow events", func(tr *TxTree) (any, error) { return tr.BurrowEvents(Mainnet.Burrow) })
		same("resolved transfers", func(tr *TxTree) (any, error) { return tr.ResolvedTransfers() })
		same("exec calls", func(tr *TxTree) (any, error) { return tr.ExecCalls(Mainnet) })
		same("verifier withdrawals", func(tr *TxTree) (any, error) { return tr.VerifierWithdrawals("intents.near") })
	}
}

func TestLiveRelayer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	rc := NewRelayerClient(Mainnet.RelayerAPI, "", util.StdOutLogger(t.Name(), util.LevelInfo))
	cfg, err := rc.Config(ctx)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.CollectFeeAccountID != Mainnet.Relayer {
		t.Fatalf("fees collected by %q", cfg.CollectFeeAccountID)
	}
	if cfg.GasFee[GasTokenNEAR] == nil {
		t.Fatalf("no native NEAR fee in %v", cfg.GasFee)
	}
	for _, m := range Mainnet.Markets {
		if cfg.GasFee[m.TokenID] == nil {
			t.Errorf("no relayer fee for %s", m.Symbol)
		}
	}
	t.Logf("relayer MCA version %s, fees %v", cfg.MCAVersion, cfg.GasFee)
	page, err := rc.History(ctx, liveMCA, 1, 5)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(page.Records) == 0 {
		t.Fatalf("empty relayer history")
	}
}

// TestLiveSupplyQuote requests dry 1Click quotes shaped as supplies are:
// to an existing MCA with the supply message, and to the account manager
// with a register-and-supply message for an identity without an MCA. It
// checks that the service accepts and echoes each message.
func TestLiveSupplyQuote(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	client, err := intents.NewClient(&intents.Config{}, util.StdOutLogger(t.Name(), util.LevelInfo))
	if err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	k, err := DeriveKey(DerivationV1, seed)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Zero()
	sig, err := k.SignCreation()
	if err != nil {
		t.Fatal(err)
	}
	createMsg, err := RegisterAndSupplyMsg(k.Identity(), sig)
	if err != nil {
		t.Fatal(err)
	}
	usdt := Mainnet.Market("usdt.tether-token.near")
	for _, q := range []struct {
		name, recipient, msg string
	}{
		{"existing", liveMCA, SupplyMsg(liveIdentity)},
		{"creating", Mainnet.AccountManager, createMsg},
	} {
		resp, err := client.Quote(ctx, &intents.QuoteRequest{
			Dry:                true,
			SwapType:           intents.SwapTypeExactInput,
			SlippageTolerance:  100,
			OriginAsset:        "nep141:wrap.near",
			DepositType:        intents.LocationOriginChain,
			DestinationAsset:   usdt.OneClickAssetID,
			Amount:             "1000000000000000000000000", // 1 NEAR
			RefundTo:           liveMCA,
			RefundType:         intents.LocationOriginChain,
			Recipient:          q.recipient,
			RecipientType:      intents.LocationDestinationChain,
			Deadline:           time.Now().Add(10 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"),
			CustomRecipientMsg: q.msg,
			NoAppFee:           true,
		})
		if err != nil {
			t.Fatalf("%s: Quote: %v", q.name, err)
		}
		if resp.Request.CustomRecipientMsg != q.msg {
			t.Fatalf("%s: recipient message echoed as %q", q.name, resp.Request.CustomRecipientMsg)
		}
		if resp.Request.Recipient != q.recipient || resp.Quote.AmountOut == "" {
			t.Fatalf("%s: quote %+v", q.name, resp)
		}
		t.Logf("%s: 1 NEAR quotes %s USDT base units to %s (min %s)", q.name, resp.Quote.AmountOut, resp.Request.Recipient, resp.Quote.MinAmountOut)
	}
}
