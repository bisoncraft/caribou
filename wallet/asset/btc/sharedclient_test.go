//go:build !harness

package btc

import (
	"context"
	"testing"

	"github.com/bisoncraft/caribou/util/feeratefetcher"
	"github.com/bisoncraft/caribou/util/testing/sharedclient"
)

// TestFeeSourcesUseSharedClient: every fee source, which the BTC, LTC and
// DASH wallets all build from these constructors, must fetch through the
// shared client so --torproxy applies to the fee APIs.
func TestFeeSourcesUseSharedClient(t *testing.T) {
	sources := map[string]*feeratefetcher.SourceConfig{
		"mempool.space":  MempoolSpaceSource("m", "https://mempool.example", 1),
		"blockstream":    BlockstreamSource("b", "https://blockstream.example/api", 1),
		"blockcypher":    BlockcypherSource("btc", 1),
		"bitcore":        BitcoreSource("BTC", 1),
		"blockchair":     BlockchairSource("bitcoin", 1),
		"bitcoiner.live": BitcoinerLiveSource(1),
	}
	for name, src := range sources {
		reqs := sharedclient.Capture(t)
		src.F(context.Background())
		sharedclient.AssertReached(t, reqs, name+" fee source")
	}
}
