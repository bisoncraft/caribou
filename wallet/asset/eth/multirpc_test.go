// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestProviderString(t *testing.T) {
	t.Parallel()
	p := &provider{host: "infura.io"}
	if p.String() != "infura.io" {
		t.Errorf("String() = %q, want infura.io", p.String())
	}
}

func TestProviderTipCache(t *testing.T) {
	t.Parallel()
	hdr := &types.Header{Number: big.NewInt(42)}

	t.Run("fresh tip is returned", func(t *testing.T) {
		t.Parallel()
		p := &provider{host: "h"}
		p.setTip(hdr, util.Disabled)
		if got := p.cachedTip(); got != hdr {
			t.Errorf("cachedTip = %v, want the set header", got)
		}
	})

	t.Run("stale header is dropped", func(t *testing.T) {
		t.Parallel()
		p := &provider{host: "h"}
		p.tip.header = hdr
		p.tip.headerStamp = time.Now().Add(-11 * time.Second) // > 10s default staleness
		if got := p.cachedTip(); got != nil {
			t.Errorf("cachedTip = %v, want nil for a stale header", got)
		}

		// With a websocket feed seen, the staleness window stretches to a
		// minute, so an 11s-old header is still fresh.
		p.tip.wsHeaderSeen.Store(true)
		if got := p.cachedTip(); got != hdr {
			t.Errorf("cachedTip = %v, want header (ws staleness window)", got)
		}
	})

	t.Run("recently failed provider returns nil", func(t *testing.T) {
		t.Parallel()
		p := &provider{host: "h"}
		p.tip.header = hdr
		p.tip.headerStamp = time.Now()
		p.setFailed()
		if got := p.cachedTip(); got != nil {
			t.Errorf("cachedTip = %v, want nil while quarantined", got)
		}
	})
}

func TestProviderFailed(t *testing.T) {
	t.Parallel()
	t.Run("fresh provider is not failed", func(t *testing.T) {
		t.Parallel()
		if (&provider{}).failed() {
			t.Error("a fresh provider should not be failed")
		}
	})

	t.Run("recently failed", func(t *testing.T) {
		t.Parallel()
		p := &provider{}
		p.setFailed()
		if !p.failed() {
			t.Error("expected failed after setFailed")
		}
	})

	t.Run("bricked by fail count", func(t *testing.T) {
		t.Parallel()
		p := &provider{}
		p.tip.failCount = brickedFailCount + 1 // old failStamp, but too many fails
		if !p.failed() {
			t.Error("expected failed when failCount exceeds brickedFailCount")
		}
	})

	t.Run("setTip clears failure", func(t *testing.T) {
		t.Parallel()
		p := &provider{host: "h"}
		p.setFailed()
		p.setTip(&types.Header{Number: big.NewInt(1)}, util.Disabled)
		if p.failed() {
			t.Error("setTip should clear the failed state")
		}
	})
}

func TestFailedProviders(t *testing.T) {
	t.Parallel()
	t.Run("reports the missing endpoints by domain", func(t *testing.T) {
		t.Parallel()
		succeeded := []*provider{{endpointAddr: "https://a.infura.io/k"}}
		tried := []string{"https://a.infura.io/k", "https://b.alchemy.com/k2"}
		got := failedProviders(succeeded, tried)
		if got != "alchemy.com" {
			t.Errorf("failedProviders = %q, want alchemy.com", got)
		}
	})

	t.Run("none failed is empty", func(t *testing.T) {
		t.Parallel()
		succeeded := []*provider{{endpointAddr: "https://a.infura.io/k"}}
		tried := []string{"https://a.infura.io/k"}
		if got := failedProviders(succeeded, tried); got != "" {
			t.Errorf("failedProviders = %q, want empty", got)
		}
	})
}

func TestErrorFilter(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom sentinel")

	t.Run("substring match", func(t *testing.T) {
		t.Parallel()
		if !errorFilter(errors.New("rpc: known transaction abc"), "known transaction") {
			t.Error("expected substring match")
		}
	})

	t.Run("errors.Is match", func(t *testing.T) {
		t.Parallel()
		wrapped := fmt.Errorf("wrapped: %w", sentinel)
		if !errorFilter(wrapped, sentinel) {
			t.Error("expected errors.Is match for wrapped sentinel")
		}
	})

	t.Run("error matcher by message", func(t *testing.T) {
		t.Parallel()
		// Distinct error instance, but its message is contained in err.
		if !errorFilter(errors.New("xxx already known yyy"), errors.New("already known")) {
			t.Error("expected match on error matcher's message")
		}
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		if errorFilter(errors.New("totally unrelated"), "nope", errors.New("also no")) {
			t.Error("did not expect a match")
		}
	})
}

func TestAcceptabilityFilters(t *testing.T) {
	t.Parallel()
	t.Run("allRPCErrorsAreFails", func(t *testing.T) {
		t.Parallel()
		discard, propagate, fail := allRPCErrorsAreFails(errors.New("x"))
		if discard || propagate || !fail {
			t.Errorf("got (%v, %v, %v), want (false, false, true)", discard, propagate, fail)
		}
	})

	t.Run("allowAlreadyKnownFilter discards known txs", func(t *testing.T) {
		t.Parallel()
		discard, propagate, fail := allowAlreadyKnownFilter(errors.New("rpc: known transaction 0xabc"))
		if !discard || propagate || fail {
			t.Errorf("got (%v, %v, %v), want (true, false, false)", discard, propagate, fail)
		}
	})

	t.Run("allowAlreadyKnownFilter keeps other errors", func(t *testing.T) {
		t.Parallel()
		discard, _, _ := allowAlreadyKnownFilter(errors.New("insufficient funds"))
		if discard {
			t.Error("unrelated error should not be discarded")
		}
	})
}

func TestRPCTransactionUnmarshalJSON(t *testing.T) {
	t.Parallel()
	// Build and sign a real tx so its JSON satisfies types.Transaction's
	// unmarshaler, then augment the object with the block fields the relay
	// returns so txExtraDetail parsing is exercised too.
	priv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	to := common.HexToAddress("0xdac17f958d2ee523a2206206994597c13d831ec7")
	signer := types.LatestSignerForChainID(big.NewInt(1))
	tx := types.MustSignNewTx(priv, signer, &types.LegacyTx{
		Nonce:    7,
		GasPrice: big.NewInt(1_000_000_000),
		Gas:      21000,
		To:       &to,
		Value:    big.NewInt(12345),
	})

	txJSON, err := tx.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(txJSON, &obj); err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(priv.PublicKey)
	obj["blockNumber"] = json.RawMessage(`"0x10"`)
	obj["blockHash"] = json.RawMessage(`"0x95118897eba948a62df72765e20186081f2554d90a146403ba93da690b322e06"`)
	obj["from"] = json.RawMessage(`"` + from.Hex() + `"`)
	augmented, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}

	var rt rpcTransaction
	if err := json.Unmarshal(augmented, &rt); err != nil {
		t.Fatal(err)
	}
	if rt.tx == nil || rt.tx.Hash() != tx.Hash() {
		t.Errorf("decoded tx hash = %v, want %s", rt.tx, tx.Hash())
	}
	if rt.BlockNumber == nil || *rt.BlockNumber != "0x10" {
		t.Errorf("blockNumber = %v, want 0x10", rt.BlockNumber)
	}
	if rt.From == nil || *rt.From != from {
		t.Errorf("from = %v, want %s", rt.From, from)
	}
}
