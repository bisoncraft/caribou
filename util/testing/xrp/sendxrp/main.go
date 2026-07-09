// sendxrp funds an XRP address from the standalone-mode genesis account.
//
// Usage: sendxrp <rpc_url> <destination> <drops>
//
// The genesis key is the well-known standalone-mode master key shipped with
// every rippled release; it is public information and carries no secrets.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	xrp "github.com/bisoncraft/caribou/wallet/asset/xrp"
	btcec "github.com/btcsuite/btcd/btcec/v2"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

const (
	// genesisKeyHex is the secp256k1 private key for the rippled standalone
	// genesis account rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh. This is the
	// publicly documented master key embedded in rippled's own test suite.
	genesisKeyHex = "1ACAAEDECE405B2A958212629E16F2EB46B153EEE94CDD350FDEFF52795525B7"
	genesisAddr   = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "usage: sendxrp <rpc_url> <destination> <drops>\n")
		os.Exit(1)
	}
	rpcURL := os.Args[1]
	dest := os.Args[2]
	drops, err := strconv.ParseUint(os.Args[3], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid drops %q: %v\n", os.Args[3], err)
		os.Exit(1)
	}

	keyBytes, err := hex.DecodeString(genesisKeyHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad genesis key constant: %v\n", err)
		os.Exit(1)
	}
	privKey, pubKey := btcec.PrivKeyFromBytes(keyBytes)

	acctInfo, err := accountInfo(rpcURL, genesisAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "account_info error: %v\n", err)
		os.Exit(1)
	}
	sequence := acctInfo.AccountData.Sequence

	feeInfo, err := feeResult(rpcURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fee error: %v\n", err)
		os.Exit(1)
	}
	fee := feeInfo.Drops.OpenLedgerFee
	lastLedgerSeq := uint32(feeInfo.LedgerCurrentIndex + 20)

	txBlob, err := xrp.BuildSignedPaymentBlob(privKey, pubKey, genesisAddr, dest, drops, fee, sequence, lastLedgerSeq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build tx error: %v\n", err)
		os.Exit(1)
	}

	sr, err := submit(rpcURL, txBlob)
	if err != nil {
		fmt.Fprintf(os.Stderr, "submit error: %v\n", err)
		os.Exit(1)
	}
	if len(sr.EngineResult) >= 3 && sr.EngineResult[:3] != "tes" {
		fmt.Fprintf(os.Stderr, "submit rejected: %s — %s\n", sr.EngineResult, sr.EngineResultMessage)
		os.Exit(1)
	}

	if err := ledgerAccept(rpcURL); err != nil {
		fmt.Fprintf(os.Stderr, "ledger_accept error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("funded %s with %d drops (tx %s)\n", dest, drops, sr.TxJSON.Hash)
}

// ---- minimal RPC helpers ----

func rpcCall(rpcURL, method string, params map[string]interface{}) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"method": method,
		"params": []interface{}{params},
	})
	resp, err := httpClient.Post(rpcURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var rpcResp struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(b, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	var status struct {
		Status  string `json:"status"`
		Error   string `json:"error"`
		Message string `json:"error_message"`
	}
	_ = json.Unmarshal(rpcResp.Result, &status)
	if status.Status == "error" {
		return nil, fmt.Errorf("%s: %s", status.Error, status.Message)
	}
	return rpcResp.Result, nil
}

type accountData struct {
	Sequence uint32 `json:"Sequence"`
}
type accountInfoResult struct {
	AccountData accountData `json:"account_data"`
}

func accountInfo(rpcURL, address string) (*accountInfoResult, error) {
	raw, err := rpcCall(rpcURL, "account_info", map[string]interface{}{
		"account":      address,
		"ledger_index": "current",
	})
	if err != nil {
		return nil, err
	}
	var r accountInfoResult
	return &r, json.Unmarshal(raw, &r)
}

type feeDrops struct {
	OpenLedgerFee uint64 `json:"open_ledger_fee,string"`
}
type feeResponse struct {
	Drops              feeDrops `json:"drops"`
	LedgerCurrentIndex uint64   `json:"ledger_current_index"`
}

func feeResult(rpcURL string) (*feeResponse, error) {
	raw, err := rpcCall(rpcURL, "fee", nil)
	if err != nil {
		return nil, err
	}
	var r feeResponse
	return &r, json.Unmarshal(raw, &r)
}

type txJSON struct {
	Hash string `json:"hash"`
}
type submitResult struct {
	EngineResult        string `json:"engine_result"`
	EngineResultMessage string `json:"engine_result_message"`
	TxJSON              txJSON `json:"tx_json"`
}

func submit(rpcURL, txBlob string) (*submitResult, error) {
	raw, err := rpcCall(rpcURL, "submit", map[string]interface{}{
		"tx_blob": txBlob,
	})
	if err != nil {
		return nil, err
	}
	var r submitResult
	return &r, json.Unmarshal(raw, &r)
}

func ledgerAccept(rpcURL string) error {
	_, err := rpcCall(rpcURL, "ledger_accept", nil)
	return err
}
