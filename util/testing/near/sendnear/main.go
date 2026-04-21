// sendnear is a helper tool for the NEAR sandbox harness. It sends NEAR
// from test.near to a recipient using the sandbox validator key.
//
// Usage: sendnear <rpc_url> <recipient> <amount_in_NEAR>
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"

	"github.com/decred/base58"
)

// The sandbox validator key for test.near, baked into the Docker image.
const sandboxSecretKey = "ed25519:5Fdfi486ameiVMqz6B2Z4dH5yayzMzQrZmekYnqpw8wTDwvfeBRoyCoMCTUsotWijXPiiL2vEpLrsa2hwHCBhS29"

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <rpc_url> <recipient> <amount_in_NEAR>\n", os.Args[0])
		os.Exit(1)
	}

	rpcURL := os.Args[1]
	recipient := os.Args[2]
	amountNEAR, err := strconv.ParseFloat(os.Args[3], 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid amount: %v\n", err)
		os.Exit(1)
	}

	// Parse the secret key. Format: "ed25519:<base58 of 64 bytes = seed+pubkey>"
	keyData := base58.Decode(sandboxSecretKey[len("ed25519:"):])
	if len(keyData) != 64 {
		fmt.Fprintf(os.Stderr, "unexpected key length %d\n", len(keyData))
		os.Exit(1)
	}
	seed := keyData[:32]
	privKey := ed25519.NewKeyFromSeed(seed)
	pubKey := privKey.Public().(ed25519.PublicKey)
	signerID := "test.near"

	// Convert NEAR to yoctoNEAR. 1 NEAR = 1e24 yoctoNEAR.
	// Use big.Float for the multiplication.
	yoctoPerNEAR, _ := new(big.Float).SetString("1000000000000000000000000")
	amountFloat := new(big.Float).SetFloat64(amountNEAR)
	yoctoFloat := new(big.Float).Mul(amountFloat, yoctoPerNEAR)
	yoctoAmount, _ := yoctoFloat.Int(nil)

	// Query the access key for the nonce.
	pubKeyStr := "ed25519:" + base58.Encode(pubKey)
	nonce, err := getAccessKeyNonce(rpcURL, signerID, pubKeyStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting access key: %v\n", err)
		os.Exit(1)
	}

	// Get the latest block hash.
	blockHash, err := getBlockHash(rpcURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting block hash: %v\n", err)
		os.Exit(1)
	}

	// Build the transaction.
	var pubKey32 [32]byte
	copy(pubKey32[:], pubKey)
	var blockHash32 [32]byte
	copy(blockHash32[:], blockHash)

	txBytes := serializeTransaction(signerID, pubKey32, nonce+1, recipient, blockHash32, yoctoAmount)
	hash := sha256.Sum256(txBytes)
	sig := ed25519.Sign(privKey, hash[:])
	signedTx := serializeSignedTransaction(txBytes, sig)
	encoded := base64.StdEncoding.EncodeToString(signedTx)

	// Broadcast.
	result, err := broadcastTxCommit(rpcURL, encoded)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error broadcasting tx: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Sent %s NEAR to %s\n", os.Args[3], recipient)
	fmt.Printf("Result: %s\n", result)
}

// Borsh serialization for a NEAR Transfer transaction.
func serializeTransaction(signerID string, pubKey [32]byte, nonce uint64, receiverID string, blockHash [32]byte, amount *big.Int) []byte {
	var b []byte
	b = appendString(b, signerID)
	b = append(b, 0) // key type ed25519
	b = append(b, pubKey[:]...)
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, nonce)
	b = append(b, buf...)
	b = appendString(b, receiverID)
	b = append(b, blockHash[:]...)
	// actions: 1 Transfer
	b = appendU32LE(b, 1)
	b = append(b, 3) // Transfer variant
	b = append(b, marshalU128LE(amount)...)
	return b
}

func serializeSignedTransaction(txBytes, sig []byte) []byte {
	b := make([]byte, 0, len(txBytes)+1+64)
	b = append(b, txBytes...)
	b = append(b, 0) // ed25519 signature type
	b = append(b, sig...)
	return b
}

func appendString(b []byte, s string) []byte {
	b = appendU32LE(b, uint32(len(s)))
	return append(b, s...)
}

func appendU32LE(b []byte, v uint32) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, v)
	return append(b, buf...)
}

func marshalU128LE(v *big.Int) []byte {
	buf := make([]byte, 16)
	raw := v.Bytes() // big-endian
	for i, j := 0, len(raw)-1; i < j; i, j = i+1, j-1 {
		raw[i], raw[j] = raw[j], raw[i]
	}
	copy(buf, raw)
	return buf
}

// RPC helpers.

func rpcCall(rpcURL, method string, params interface{}) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "sendnear",
		"method":  method,
		"params":  params,
	})
	resp, err := http.Post(rpcURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("RPC error: %s", rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

func getAccessKeyNonce(rpcURL, accountID, pubKey string) (uint64, error) {
	result, err := rpcCall(rpcURL, "query", map[string]interface{}{
		"request_type": "view_access_key",
		"finality":     "final",
		"account_id":   accountID,
		"public_key":   pubKey,
	})
	if err != nil {
		return 0, err
	}
	var info struct {
		Nonce uint64 `json:"nonce"`
	}
	if err := json.Unmarshal(result, &info); err != nil {
		return 0, err
	}
	return info.Nonce, nil
}

func getBlockHash(rpcURL string) ([]byte, error) {
	result, err := rpcCall(rpcURL, "block", map[string]interface{}{
		"finality": "final",
	})
	if err != nil {
		return nil, err
	}
	var info struct {
		Header struct {
			Hash string `json:"hash"`
		} `json:"header"`
	}
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, err
	}
	h := base58.Decode(info.Header.Hash)
	if len(h) < 32 {
		return nil, fmt.Errorf("block hash too short: %d bytes", len(h))
	}
	return h[:32], nil
}

func broadcastTxCommit(rpcURL, signedTxBase64 string) (string, error) {
	result, err := rpcCall(rpcURL, "broadcast_tx_commit", []string{signedTxBase64})
	if err != nil {
		return "", err
	}
	return string(result), nil
}
