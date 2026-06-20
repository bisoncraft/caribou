// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquidmock

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset/hyperliquid"
)

func TestSendAsset(t *testing.T) {
	priv, err := gethcrypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sender := strings.ToLower(gethcrypto.PubkeyToAddress(priv.PublicKey).Hex())
	const dest = "0x000000000000000000000000000000000000aa"

	srv := New(util.Testnet)
	srv.Fund(sender, 5_00000000) // 5 HYPE

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	action := sendAssetAction{
		Type:             "sendAsset",
		HyperliquidChain: "Testnet",
		SignatureChainID: "0x66eee",
		Destination:      dest,
		SourceDex:        "spot",
		DestinationDex:   "spot",
		Token:            "HYPE:" + hypeTokenID,
		Amount:           "1",
		Nonce:            1000,
	}

	hash := hyperliquid.SendAssetEIP712Hash(util.Testnet, action.Destination, action.SourceDex, action.DestinationDex, action.Token, action.Amount, action.FromSubAccount, action.Nonce)
	sig, err := gethcrypto.Sign(hash, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	req := exchangeRequest{
		Action: mustMarshal(t, action),
		Nonce:  action.Nonce,
		Signature: exchangeSignature{
			R: "0x" + hexEncode(sig[:32]),
			S: "0x" + hexEncode(sig[32:64]),
			V: sig[64] + 27,
		},
	}

	var resp exchangeResponse
	postJSON(t, httpSrv.URL+"/exchange", req, &resp)
	if resp.Status != "ok" {
		t.Fatalf("send rejected: %s", resp.Response)
	}

	if got := srv.Balance(sender); got != 4_00000000 {
		t.Errorf("sender balance = %d, want %d", got, 4_00000000)
	}
	if got := srv.Balance(dest); got != 1_00000000 {
		t.Errorf("dest balance = %d, want %d", got, 1_00000000)
	}

	// Bad signature is rejected and balances are unchanged.
	badReq := req
	badReq.Signature.V = 27
	badReq.Signature.R = "0x" + hexEncode(make([]byte, 32))
	var badResp exchangeResponse
	postJSON(t, httpSrv.URL+"/exchange", badReq, &badResp)
	if badResp.Status == "ok" {
		t.Fatalf("expected rejection for bad signature, got ok")
	}
	if got := srv.Balance(sender); got != 4_00000000 {
		t.Errorf("sender balance changed after rejected send: %d", got)
	}
}

// signedSendReq builds a signed sendAsset exchangeRequest for the given key.
func signedSendReq(t *testing.T, priv *ecdsa.PrivateKey, chain, dest, amount string, nonce uint64) exchangeRequest {
	t.Helper()
	action := sendAssetAction{
		Type:             "sendAsset",
		HyperliquidChain: chain,
		SignatureChainID: "0x66eee",
		Destination:      dest,
		SourceDex:        "spot",
		DestinationDex:   "spot",
		Token:            "HYPE:" + hypeTokenID,
		Amount:           amount,
		Nonce:            nonce,
	}
	hash := hyperliquid.SendAssetEIP712Hash(util.Testnet, action.Destination, action.SourceDex, action.DestinationDex, action.Token, action.Amount, action.FromSubAccount, action.Nonce)
	sig, err := gethcrypto.Sign(hash, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return exchangeRequest{
		Action: mustMarshal(t, action),
		Nonce:  action.Nonce,
		Signature: exchangeSignature{
			R: "0x" + hexEncode(sig[:32]),
			S: "0x" + hexEncode(sig[32:64]),
			V: sig[64] + 27,
		},
	}
}

func ledgerFor(t *testing.T, url, addr string) []ledgerUpdate {
	t.Helper()
	var updates []ledgerUpdate
	postJSON(t, url+"/info", map[string]any{
		"type": "userNonFundingLedgerUpdates", "user": addr, "startTime": 0,
	}, &updates)
	return updates
}

// TestSendAssetFeeAndNonce verifies the mock charges the configured transfer fee
// and stamps the emitted spotTransfer delta with the fee and originating nonce.
func TestSendAssetFeeAndNonce(t *testing.T) {
	priv, err := gethcrypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sender := strings.ToLower(gethcrypto.PubkeyToAddress(priv.PublicKey).Hex())
	const dest = "0x000000000000000000000000000000000000aa"

	srv := New(util.Testnet)
	srv.SetTransferFee(1_000000) // 0.01 HYPE
	srv.Fund(sender, 5_00000000)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	req := signedSendReq(t, priv, "Testnet", dest, "1", 1000)
	var resp exchangeResponse
	postJSON(t, httpSrv.URL+"/exchange", req, &resp)
	if resp.Status != "ok" {
		t.Fatalf("send rejected: %s", resp.Response)
	}

	// Sender debited amount + fee.
	if got := srv.Balance(sender); got != 3_99000000 {
		t.Errorf("sender balance = %d, want %d", got, 3_99000000)
	}

	updates := ledgerFor(t, httpSrv.URL, sender)
	var found bool
	for _, u := range updates {
		if u.Delta.Type != "spotTransfer" {
			continue
		}
		found = true
		if u.Delta.Fee != "0.01" {
			t.Errorf("delta fee = %q, want %q", u.Delta.Fee, "0.01")
		}
		if u.Delta.Nonce == nil || *u.Delta.Nonce != 1000 {
			t.Errorf("delta nonce = %v, want 1000", u.Delta.Nonce)
		}
	}
	if !found {
		t.Fatal("no spotTransfer delta in sender ledger")
	}
}

// TestSendAssetWrongChainRejected verifies the mock rejects an action whose
// hyperliquidChain does not match the server's network.
func TestSendAssetWrongChainRejected(t *testing.T) {
	priv, err := gethcrypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sender := strings.ToLower(gethcrypto.PubkeyToAddress(priv.PublicKey).Hex())

	srv := New(util.Testnet)
	srv.Fund(sender, 5_00000000)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	req := signedSendReq(t, priv, "Mainnet", "0x000000000000000000000000000000000000aa", "1", 1000)
	var resp exchangeResponse
	postJSON(t, httpSrv.URL+"/exchange", req, &resp)
	if resp.Status == "ok" {
		t.Fatal("expected rejection for wrong hyperliquidChain")
	}
	if got := srv.Balance(sender); got != 5_00000000 {
		t.Errorf("sender balance changed after rejected send: %d", got)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0xf]
	}
	return string(out)
}

func postJSON(t *testing.T, url string, req, resp any) {
	t.Helper()
	body := mustMarshal(t, req)
	httpResp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer httpResp.Body.Close()
	if err := json.NewDecoder(httpResp.Body).Decode(resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
