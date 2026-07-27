// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/near"
	"github.com/bisoncraft/caribou/wallet/intents"
)

// endpoints returns the NEAR RPC endpoints to try in order: the override
// alone when given, else the wallet's built-in mainnet providers.
func endpoints(override string) []string {
	if override != "" {
		return []string{override}
	}
	return near.DefaultRPCEndpoints[util.Mainnet]
}

// viewCall invokes a read-only method on the verifier contract, trying
// each endpoint in order, and returns the raw JSON result.
func viewCall(ctx context.Context, rpcOverride, method string, args []byte) (json.RawMessage, error) {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      "treasurysweep",
		"method":  "query",
		"params": map[string]any{
			"request_type": "call_function",
			"finality":     "final",
			"account_id":   verifierContract,
			"method_name":  method,
			"args_base64":  base64.StdEncoding.EncodeToString(args),
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 20 * time.Second}
	var lastErr error
	for _, url := range endpoints(rpcOverride) {
		result, err := postRPC(ctx, client, url, body)
		if err != nil {
			// A contract-side execution failure is deterministic — every
			// endpoint runs the same view against the same state — so it
			// is authoritative, not an endpoint problem to fail over.
			if errors.Is(err, errContractExecution) {
				return nil, err
			}
			lastErr = fmt.Errorf("%s: %w", url, err)
			continue
		}
		return result, nil
	}
	return nil, fmt.Errorf("all NEAR RPC endpoints failed, last error: %w", lastErr)
}

// errContractExecution marks a failure inside the contract's execution of
// a view call, as opposed to an endpoint or transport failure.
var errContractExecution = errors.New("contract execution failed")

func postRPC(ctx context.Context, client *http.Client, url string, body []byte) (json.RawMessage, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var envelope struct {
		Result struct {
			Result []byte `json:"result"`
			// Error reports a contract-side execution failure (e.g. a
			// panic inside the view call); it arrives inside result, not
			// as a JSON-RPC-level error.
			Error string `json:"error"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("error decoding response: %w", err)
	}
	if len(envelope.Error) > 0 {
		return nil, fmt.Errorf("RPC error: %s", envelope.Error)
	}
	if envelope.Result.Error != "" {
		return nil, fmt.Errorf("%w: %s", errContractExecution, envelope.Result.Error)
	}
	return json.RawMessage(envelope.Result.Result), nil
}

// currentSalt fetches the verifier's active 4-byte nonce salt, as 8 hex
// characters.
func currentSalt(rpcOverride string) (string, error) {
	result, err := viewCall(context.Background(), rpcOverride, "current_salt", []byte("{}"))
	if err != nil {
		return "", err
	}
	var salt string
	if err := json.Unmarshal(result, &salt); err != nil {
		return "", fmt.Errorf("error decoding current_salt result %q: %w", result, err)
	}
	return salt, nil
}

// balanceBatchSize bounds token IDs per mt_batch_balance_of view call.
const balanceBatchSize = 64

// cmdBalances lists the treasury's nonzero NEP-245 balances inside the
// verifier, resolving token IDs, symbols, and decimals from the live
// 1Click catalog.
func cmdBalances(args []string) error {
	fs := flag.NewFlagSet("balances", flag.ContinueOnError)
	treasury := fs.String("treasury", "", "treasury identity (0x...)")
	rpcURL := fs.String("rpc", "", "NEAR RPC endpoint override")
	if err := fs.Parse(args); err != nil {
		return err
	}
	account, err := parseIdentity(*treasury)
	if err != nil {
		return fmt.Errorf("--treasury: %w", err)
	}

	ctx := context.Background()
	client, err := intents.NewClient(nil, util.Disabled)
	if err != nil {
		return err
	}
	tokens, err := client.Tokens(ctx)
	if err != nil {
		return fmt.Errorf("error fetching 1Click catalog: %w", err)
	}

	// The catalog repeats asset IDs across chains; balances live under the
	// unique verifier token ID.
	type tokenInfo struct {
		symbol   string
		decimals int
		chain    string
	}
	byID := make(map[string]tokenInfo)
	ids := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if _, seen := byID[tok.AssetID]; seen {
			continue
		}
		byID[tok.AssetID] = tokenInfo{symbol: tok.Symbol, decimals: tok.Decimals, chain: tok.Blockchain}
		ids = append(ids, tok.AssetID)
	}

	type balance struct {
		id     string
		amount *big.Int
	}
	var balances []balance
	for start := 0; start < len(ids); start += balanceBatchSize {
		batch := ids[start:min(start+balanceBatchSize, len(ids))]
		argsB, err := json.Marshal(map[string]any{
			"account_id": account,
			"token_ids":  batch,
		})
		if err != nil {
			return err
		}
		result, err := viewCall(ctx, *rpcURL, "mt_batch_balance_of", argsB)
		if err != nil {
			return err
		}
		var amounts []string
		if err := json.Unmarshal(result, &amounts); err != nil {
			return fmt.Errorf("error decoding balances: %w", err)
		}
		if len(amounts) != len(batch) {
			return fmt.Errorf("got %d balances for %d tokens", len(amounts), len(batch))
		}
		for i, amt := range amounts {
			v, ok := new(big.Int).SetString(amt, 10)
			if !ok {
				return fmt.Errorf("unparseable balance %q for %s", amt, batch[i])
			}
			if v.Sign() > 0 {
				balances = append(balances, balance{id: batch[i], amount: v})
			}
		}
	}

	if len(balances) == 0 {
		fmt.Printf("No verifier balances for %s (%d catalog tokens checked).\n", account, len(ids))
		return nil
	}
	sort.Slice(balances, func(i, j int) bool { return balances[i].id < balances[j].id })
	fmt.Printf("Verifier balances for %s:\n", account)
	for _, b := range balances {
		info := byID[b.id]
		fmt.Printf("  %s %s (%s)  %s  base units: %s\n",
			formatBaseUnits(b.amount, info.decimals), info.symbol, info.chain, b.id, b.amount)
	}
	return nil
}

// formatBaseUnits renders an integer base-unit amount with a decimal
// point, trimming trailing zeros.
func formatBaseUnits(v *big.Int, decimals int) string {
	if decimals <= 0 {
		return v.String()
	}
	div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole, frac := new(big.Int).QuoRem(v, div, new(big.Int))
	s := strings.TrimRight(fmt.Sprintf("%s.%0*s", whole, decimals, frac), "0")
	return strings.TrimSuffix(s, ".")
}

// cmdSimulate dry-runs a signed withdrawal against the verifier's
// simulate_intents view method.
func cmdSimulate(args []string) error {
	fs := flag.NewFlagSet("simulate", flag.ContinueOnError)
	signedPath := fs.String("signed", "", "file holding the signed execute_intents argument from sign")
	rpcURL := fs.String("rpc", "", "NEAR RPC endpoint override")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *signedPath == "" {
		return fmt.Errorf("--signed is required")
	}
	raw, err := os.ReadFile(*signedPath)
	if err != nil {
		return err
	}
	var envelope signedEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("signed file is not a valid execute_intents argument: %w", err)
	}
	if len(envelope.Signed) == 0 {
		return fmt.Errorf("signed file holds no payloads")
	}

	result, err := viewCall(context.Background(), *rpcURL, "simulate_intents", raw)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, result, "", "  "); err != nil {
		fmt.Printf("%s\n", result)
		return nil
	}
	fmt.Printf("%s\n", pretty.Bytes())
	return nil
}
