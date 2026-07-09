// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquidmock

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/hyperliquid"
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
