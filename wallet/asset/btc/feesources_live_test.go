//go:build live

package btc

// Live-network checks of the external fee-rate sources. The same mainnet
// sources serve every network — on simnet/testnet a live mainnet rate is
// the intended fee estimate — so these tests double as verification that
// fee-dependent flows (sends, Intents deposits) have a working rate on
// simnet. Run with: go test -v -tags live -run TestLiveFeeSources ./wallet/asset/btc/

import (
	"context"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/feeratefetcher"
)

func TestLiveFeeSources(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	log := util.StdOutLogger("FEELIVE", util.LevelDebug)

	// Per-source: every configured source should answer.
	for _, src := range BitcoinFeeSources() {
		rate, _, err := src.F(ctx)
		if err != nil {
			t.Errorf("source %s error: %v", src.Name, err)
			continue
		}
		t.Logf("source %s: %d sats/vB", src.Name, rate)
		if rate == 0 {
			t.Errorf("source %s returned zero", src.Name)
		}
	}

	// Composite: the path NativeWallet.FeeRate takes.
	fetcher := feeratefetcher.New(BitcoinFeeSources(), log)
	rate := fetcher.Rate(ctx)
	t.Logf("composite rate: %d sats/vB", rate)
	if rate == 0 {
		t.Fatal("composite fetcher returned zero")
	}
}
