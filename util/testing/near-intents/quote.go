// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"time"

	"github.com/bisoncraft/caribou/wallet/intents"
)

// keylessSurchargeBps mirrors the service's observed keyless surcharge:
// an appFees entry paying the operator's own fee account, injected on
// every keyless quote whether or not the request configures fees.
const keylessSurchargeBps = 10

// operatorFeeAccount is the production 1Click operator's fee-collection
// account inside intents.near, mirrored verbatim so the rewritten appFees
// echo is display-identical to mainnet.
const operatorFeeAccount = "5880ad2b362620fadf759cbceb1cd5737ce8c6ed7fb8e9942881e6731f9247dd"

// rewriteAppFees models the service's observed keyless fee mechanics: each
// configured fee of N bps is split — N/2 stays with its recipient, the
// remainder goes to the operator — and the keyless surcharge lands on the
// operator's entry, appended last. The sum of the returned entries is the
// user's total appFees haircut, so pricing charges configured + surcharge.
func rewriteAppFees(configured []intents.AppFee) []intents.AppFee {
	operatorBps := keylessSurchargeBps
	rewritten := make([]intents.AppFee, 0, len(configured)+1)
	for _, f := range configured {
		half := f.Fee / 2
		rewritten = append(rewritten, intents.AppFee{Recipient: f.Recipient, Fee: half})
		operatorBps += f.Fee - half
	}
	return append(rewritten, intents.AppFee{Recipient: operatorFeeAccount, Fee: operatorBps})
}

// replaceAppFees swaps the appFees member of a raw quote-request body,
// leaving every other member's value byte-intact (compacted).
func replaceAppFees(body []byte, fees []intents.AppFee) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(fees)
	if err != nil {
		return nil, err
	}
	m["appFees"] = raw
	return json.Marshal(m)
}

// maxQuoteBodySize bounds quote request bodies.
const maxQuoteBodySize = 1 << 20

// handleQuote serves POST /v0/quote. The received body is echoed as the
// envelope's quoteRequest member with its appFees array rewritten per the
// service's keyless mechanics (see rewriteAppFees) — every field the
// client's echo cross-check covers is echoed verbatim; appFees sits
// outside both the cross-check and the signed payload. The envelope is
// signed with the dev key. A committed (non-dry) quote allocates a real
// deposit address on the origin chain's harness and starts the swap's
// state machine.
func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxQuoteBodySize))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "error reading body: %v", err)
		return
	}
	var req intents.QuoteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "bad request body: %v", err)
		return
	}

	var exactOutput bool
	switch req.SwapType {
	case intents.SwapTypeExactInput:
	case intents.SwapTypeExactOutput:
		exactOutput = true
	default:
		writeError(w, http.StatusBadRequest, "Bad Request", "unsupported swapType %q", req.SwapType)
		return
	}
	origin := s.assets[req.OriginAsset]
	if origin == nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "unknown origin asset %q", req.OriginAsset)
		return
	}
	dest := s.assets[req.DestinationAsset]
	if dest == nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "unknown destination asset %q", req.DestinationAsset)
		return
	}
	if origin == dest {
		writeError(w, http.StatusBadRequest, "Bad Request", "origin and destination are the same asset")
		return
	}
	amount, ok := new(big.Int).SetString(req.Amount, 10)
	if !ok || amount.Sign() <= 0 {
		writeError(w, http.StatusBadRequest, "Bad Request", "bad amount %q", req.Amount)
		return
	}
	if req.Recipient == "" || req.RefundTo == "" {
		writeError(w, http.StatusBadRequest, "Bad Request", "recipient and refundTo are required")
		return
	}
	deadline, err := time.Parse(time.RFC3339, req.Deadline)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "bad deadline %q: %v", req.Deadline, err)
		return
	}
	slippageBps := req.SlippageTolerance
	if slippageBps < 0 || slippageBps >= 10_000 {
		writeError(w, http.StatusBadRequest, "Bad Request", "bad slippageTolerance %d", slippageBps)
		return
	}

	appFees := rewriteAppFees(req.AppFees)
	var feeBps int64
	for _, f := range appFees {
		feeBps += int64(f.Fee)
	}
	body, err = replaceAppFees(body, appFees)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error", "error rewriting appFees: %v", err)
		return
	}

	priceO, priceD := s.price(origin.intentsID), s.price(dest.intentsID)
	bps := big.NewInt(10_000)
	var amountIn, amountOut, minAmountOut *big.Int
	if exactOutput {
		// The requested amount is the exact destination output; the input
		// required to fund it inverts the exact-input math, rounded up so
		// the priced deposit always covers the promised output:
		// amountIn = ⌈amountOut · priceD · 10^originDec · 10^4 /
		//            (priceO · 10^destDec · (10^4 − feeBps))⌉.
		amountOut = amount
		num := new(big.Int).Mul(amountOut, big.NewInt(priceD))
		num.Mul(num, tenPow(origin.decimals))
		num.Mul(num, bps)
		den := new(big.Int).Mul(big.NewInt(priceO), tenPow(dest.decimals))
		den.Mul(den, big.NewInt(10_000-feeBps))
		amountIn = new(big.Int).Add(num, new(big.Int).Sub(den, big.NewInt(1)))
		amountIn.Quo(amountIn, den)
		// The output is fixed; slippage has nothing to erode.
		minAmountOut = amountOut
	} else {
		// grossOut = amountIn · priceO · 10^destDec / (priceD · 10^originDec),
		// multiplying before dividing to keep integer precision.
		amountIn = amount
		grossOut := new(big.Int).Mul(amountIn, big.NewInt(priceO))
		grossOut.Mul(grossOut, tenPow(dest.decimals))
		grossOut.Quo(grossOut, new(big.Int).Mul(big.NewInt(priceD), tenPow(origin.decimals)))

		amountOut = new(big.Int).Mul(grossOut, big.NewInt(10_000-feeBps))
		amountOut.Quo(amountOut, bps)
		minAmountOut = new(big.Int).Mul(amountOut, big.NewInt(10_000-int64(slippageBps)))
		minAmountOut.Quo(minAmountOut, bps)
	}
	refundFee := new(big.Int).Quo(amountIn, big.NewInt(1000))
	withdrawFee := new(big.Int).Quo(amountOut, big.NewInt(1000))

	if amountOut.Sign() <= 0 || amountIn.Sign() <= 0 {
		writeError(w, http.StatusBadRequest, "Bad Request", "amount too small to quote")
		return
	}

	quote := map[string]any{
		"amountIn":           amountIn.String(),
		"amountInFormatted":  formatUnits(amountIn, origin.decimals, origin.decimals),
		"amountInUsd":        usdString(amountIn, origin.decimals, priceO),
		"minAmountIn":        amountIn.String(),
		"amountOut":          amountOut.String(),
		"amountOutFormatted": formatUnits(amountOut, dest.decimals, dest.decimals),
		"amountOutUsd":       usdString(amountOut, dest.decimals, priceD),
		"minAmountOut":       minAmountOut.String(),
		"refundFee":          refundFee.String(),
		"withdrawFee":        withdrawFee.String(),
		"timeEstimate":       10,
		"deadline":           req.Deadline,
		"timeWhenInactive":   req.Deadline,
	}

	var sw *swapState
	if !req.Dry {
		originDrv := s.drivers[origin.chain.slug]
		depositAddress, err := originDrv.NewDepositAddress()
		if err != nil {
			s.log.Printf("NewDepositAddress on %s failed: %v", origin.chain.slug, err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error",
				"error generating deposit address: %v", err)
			return
		}
		quote["depositAddress"] = depositAddress
		sw = &swapState{
			server:         s,
			correlationID:  newCorrelationID(),
			depositAddress: depositAddress,
			origin:         origin,
			dest:           dest,
			originDrv:      originDrv,
			destDrv:        s.drivers[dest.chain.slug],
			amountIn:       amountIn,
			amountOut:      amountOut,
			refundFee:      refundFee,
			recipient:      req.Recipient,
			refundTo:       req.RefundTo,
			deadline:       deadline,
			status:         intents.StatusPendingDeposit,
			deposited:      new(big.Int),
			createdAt:      time.Now(),
			updatedAt:      time.Now(),
			poke:           make(chan struct{}, 1),
		}
	}

	rawQuote, err := json.Marshal(quote)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error", "error encoding quote: %v", err)
		return
	}
	ts := time.Now().UTC().Format(timestampFormat)
	sig, err := intents.SignQuote(s.priv, body, rawQuote, ts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error", "error signing quote: %v", err)
		return
	}
	correlationID := newCorrelationID()
	if sw != nil {
		correlationID = sw.correlationID
	}
	envelope, err := json.Marshal(map[string]any{
		"timestamp":     ts,
		"signature":     sig,
		"correlationId": correlationID,
		"quoteRequest":  json.RawMessage(body),
		"quote":         json.RawMessage(rawQuote),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error", "error encoding envelope: %v", err)
		return
	}

	if sw != nil {
		sw.envelope = envelope
		s.swapMtx.Lock()
		s.swaps[sw.depositAddress] = sw
		s.swapMtx.Unlock()
		go sw.run(s.ctx)
		s.log.Printf("committed quote %s: %s %s (%s) -> %s %s (%s), deposit address %s",
			sw.correlationID, formatUnits(amountIn, origin.decimals, origin.decimals), origin.symbol, origin.chain.slug,
			formatUnits(amountOut, dest.decimals, dest.decimals), dest.symbol, dest.chain.slug, sw.depositAddress)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(envelope)
}
