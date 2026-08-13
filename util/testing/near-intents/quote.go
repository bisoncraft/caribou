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

// protocolFeeBps is a fake protocol fee applied on top of any requested
// appFees, so quoted output is visibly below the raw price conversion.
const protocolFeeBps = 10

// maxQuoteBodySize bounds quote request bodies.
const maxQuoteBodySize = 1 << 20

// handleQuote serves POST /v0/quote. The received body is echoed verbatim
// as the envelope's quoteRequest member — the client's echo cross-check
// compares it byte-wise against what it sent — and the envelope is signed
// with the dev key. A committed (non-dry) quote allocates a real deposit
// address on the origin chain's harness and starts the swap's state
// machine.
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

	if req.SwapType != intents.SwapTypeExactInput {
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
	amountIn, ok := new(big.Int).SetString(req.Amount, 10)
	if !ok || amountIn.Sign() <= 0 {
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

	feeBps := int64(protocolFeeBps)
	for _, f := range req.AppFees {
		feeBps += int64(f.Fee)
	}

	// grossOut = amountIn · priceO · 10^destDec / (priceD · 10^originDec),
	// multiplying before dividing to keep integer precision.
	priceO, priceD := s.price(origin.intentsID), s.price(dest.intentsID)
	grossOut := new(big.Int).Mul(amountIn, big.NewInt(priceO))
	grossOut.Mul(grossOut, tenPow(dest.decimals))
	grossOut.Quo(grossOut, new(big.Int).Mul(big.NewInt(priceD), tenPow(origin.decimals)))

	bps := big.NewInt(10_000)
	amountOut := new(big.Int).Mul(grossOut, big.NewInt(10_000-feeBps))
	amountOut.Quo(amountOut, bps)
	minAmountOut := new(big.Int).Mul(amountOut, big.NewInt(10_000-int64(slippageBps)))
	minAmountOut.Quo(minAmountOut, bps)
	refundFee := new(big.Int).Quo(amountIn, big.NewInt(1000))
	withdrawFee := new(big.Int).Quo(amountOut, big.NewInt(1000))

	if amountOut.Sign() <= 0 {
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
