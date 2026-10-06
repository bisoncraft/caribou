//go:build !harness && !botlive

package core

import (
	"context"
	"testing"

	"github.com/bisoncraft/caribou/util/testing/sharedclient"
)

// TestExchangeRateFetchUsesSharedClient: exchange-rate requests must go
// through the shared client so --torproxy applies to them.
func TestExchangeRateFetchUsesSharedClient(t *testing.T) {
	reqs := sharedclient.Capture(t)
	var v struct{}
	getRates(context.Background(), "https://rates.example", &v)
	sharedclient.AssertReached(t, reqs, "exchange rate fetcher")
}
