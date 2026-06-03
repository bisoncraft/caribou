// sendhype is a harness helper that sends HYPE from a pre-funded testnet
// account to a recipient using the Hyperliquid testnet exchange API.
//
// Usage: sendhype <api_url> <recipient_0x_addr> <amount_hype>
//
// The funder's private key is hardcoded. The account must hold testnet HYPE,
// obtainable from the Hyperliquid testnet faucet.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// funderPrivKeyHex is the hex-encoded private key for the testnet funder
// account. Replace with a key whose address holds testnet HYPE.
// SECURITY: this is a testnet-only key with no real value.
const funderPrivKeyHex = "0000000000000000000000000000000000000000000000000000000000000001"

// defaultFunderKey is the placeholder value; running with it is a no-op.
const defaultFunderKey = "0000000000000000000000000000000000000000000000000000000000000001"

// eip712ChainID is Hyperliquid's chain ID used in the EIP-712 domain.
const eip712ChainID = uint64(998)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <api_url> <recipient> <amount_hype>\n", os.Args[0])
		os.Exit(1)
	}
	apiURL := os.Args[1]
	recipient := os.Args[2]
	amtHYPE := os.Args[3]

	if funderPrivKeyHex == defaultFunderKey {
		fmt.Fprintln(os.Stderr, "sendhype: funderPrivKeyHex is still the placeholder — replace it with a funded testnet account key before running the harness")
		os.Exit(1)
	}

	privKey, err := gethcrypto.HexToECDSA(funderPrivKeyHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading private key: %v\n", err)
		os.Exit(1)
	}

	sender := strings.ToLower(gethcrypto.PubkeyToAddress(privKey.PublicKey).Hex())
	fmt.Printf("Funder address: %s\n", sender)

	// Fetch HYPE token ID from spot meta.
	tokenID, err := fetchHypeTokenID(apiURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetching spot meta: %v\n", err)
		os.Exit(1)
	}

	nonce := uint64(time.Now().UnixMilli())
	if err := sendHYPE(apiURL, privKey, recipient, tokenID, amtHYPE, nonce); err != nil {
		fmt.Fprintf(os.Stderr, "sending HYPE: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Sent %s HYPE to %s\n", amtHYPE, recipient)
}

func fetchHypeTokenID(apiURL string) (string, error) {
	var meta struct {
		Tokens []struct {
			Name    string `json:"name"`
			TokenID string `json:"tokenId"`
		} `json:"tokens"`
	}
	if err := infoRequest(apiURL, map[string]any{"type": "spotMeta"}, &meta); err != nil {
		return "", err
	}
	for _, t := range meta.Tokens {
		if t.Name == "HYPE" {
			return "HYPE:" + t.TokenID, nil
		}
	}
	return "", fmt.Errorf("HYPE not found in spot meta")
}

func sendHYPE(apiURL string, privKey *ecdsa.PrivateKey, dest, tokenID, amount string, nonce uint64) error {
	hash := spotSendEIP712Hash("Testnet", dest, tokenID, amount, nonce)
	sig, err := gethcrypto.Sign(hash, privKey)
	if err != nil {
		return fmt.Errorf("signing: %w", err)
	}
	sig[64] += 27 // Ethereum legacy V

	r := "0x" + fmt.Sprintf("%064x", sig[:32])
	s := "0x" + fmt.Sprintf("%064x", sig[32:64])
	v := sig[64]

	req := map[string]any{
		"action": map[string]any{
			"type":             "spotSend",
			"hyperliquidChain": "Testnet",
			"signatureChainId": "0x" + strconv.FormatUint(eip712ChainID, 16),
			"destination":      dest,
			"token":            tokenID,
			"amount":           amount,
			"time":             nonce,
		},
		"nonce": nonce,
		"signature": map[string]any{
			"r": r,
			"s": s,
			"v": v,
		},
	}

	var resp struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if err := postJSON(apiURL+"/exchange", req, &resp); err != nil {
		return err
	}
	if resp.Status != "ok" {
		return fmt.Errorf("exchange error: %s", resp.Response)
	}
	return nil
}

// --- EIP-712 (minimal, matching the wallet implementation) ---

func spotSendEIP712Hash(chainName, dest, token, amount string, nonce uint64) []byte {
	domainHash := eip712DomainHash()
	msgHash := spotSendMessageHash(chainName, dest, token, amount, nonce)
	return gethcrypto.Keccak256(append([]byte{0x19, 0x01}, append(domainHash, msgHash...)...))
}

func eip712DomainHash() []byte {
	typeHash := gethcrypto.Keccak256([]byte(
		"EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)",
	))
	chainIDBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(chainIDBytes[24:], eip712ChainID)

	enc := make([]byte, 0, 5*32)
	enc = append(enc, typeHash...)
	enc = append(enc, gethcrypto.Keccak256([]byte("HyperliquidSignTransaction"))...)
	enc = append(enc, gethcrypto.Keccak256([]byte("1"))...)
	enc = append(enc, chainIDBytes...)
	enc = append(enc, make([]byte, 32)...) // zero address
	return gethcrypto.Keccak256(enc)
}

func spotSendMessageHash(chainName, dest, token, amount string, nonce uint64) []byte {
	typeHash := gethcrypto.Keccak256([]byte(
		"SpotSend(string hyperliquidChain,string destination,string token,string amount,uint64 time)",
	))
	nonceBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(nonceBytes[24:], nonce)

	enc := make([]byte, 0, 6*32)
	enc = append(enc, typeHash...)
	enc = append(enc, gethcrypto.Keccak256([]byte(chainName))...)
	enc = append(enc, gethcrypto.Keccak256([]byte(dest))...)
	enc = append(enc, gethcrypto.Keccak256([]byte(token))...)
	enc = append(enc, gethcrypto.Keccak256([]byte(amount))...)
	enc = append(enc, nonceBytes...)
	return gethcrypto.Keccak256(enc)
}

// --- HTTP helpers ---

func infoRequest(apiURL string, payload, result any) error {
	return postJSON(apiURL+"/info", payload, result)
}

func postJSON(url string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, respBody)
	}
	return json.Unmarshal(respBody, result)
}
