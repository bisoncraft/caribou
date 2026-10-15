package fiatrates

import (
	"context"
	"testing"

	"github.com/bisoncraft/caribou/util/testing/sharedclient"
)

// TestFiatRateFetchUsesSharedClient: fiat-rate requests must go through the
// shared client so --torproxy applies to them.
func TestFiatRateFetchUsesSharedClient(t *testing.T) {
	reqs := sharedclient.Capture(t)
	var v struct{}
	getRates(context.Background(), "https://rates.example", &v)
	sharedclient.AssertReached(t, reqs, "fiat rate fetcher")
}
