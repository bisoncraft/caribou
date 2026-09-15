// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package app

import (
	"io"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/btcsuite/btclog"
	"github.com/decred/slog"
)

// TestBtclogAdapterLevels pins the assumption btclogAdapter rests on: btclog
// and slog number their levels identically, so a cast converts one to the
// other.
func TestBtclogAdapterLevels(t *testing.T) {
	for lvl := slog.LevelTrace; lvl <= slog.LevelOff; lvl++ {
		if got, want := btclog.Level(lvl).String(), lvl.String(); got != want {
			t.Fatalf("level %d: btclog %q, slog %q", lvl, got, want)
		}
	}

	a := btclogAdapter{util.NewLogger("T", slog.LevelInfo, io.Discard)}
	a.SetLevel(btclog.LevelWarn)
	if a.Level() != btclog.LevelWarn {
		t.Fatalf("adapter level %s, want %s", a.Level(), btclog.LevelWarn)
	}
	if a.Logger.Level() != slog.LevelWarn {
		t.Fatalf("underlying level %s, want %s", a.Logger.Level(), slog.LevelWarn)
	}
}
