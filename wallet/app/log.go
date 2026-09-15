// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/utxowallet"
	"github.com/btcsuite/btclog"
	"github.com/decred/slog"
	"github.com/jrick/logrotate/rotator"
)

const (
	maxLogRolls = 16
)

var (
	// logRotator is one of the logging outputs. It should be closed on
	// application shutdown.
	logRotator         *rotator.Rotator
	defaultLogLevelMap = map[string]slog.Level{asset.InternalNodeLoggerName: slog.LevelError}
)

// logWriter implements an io.Writer that outputs to a rotating log file.
type logWriter struct {
	*rotator.Rotator
	stdout bool
}

// Write writes the data in p to the log file.
func (w logWriter) Write(p []byte) (n int, err error) {
	if w.stdout {
		if _, err := os.Stdout.Write(p); err != nil {
			fmt.Fprintf(os.Stderr, "error writing to stdout: %v\n", err)
		}
	}
	return w.Rotator.Write(p)
}

// InitLogging initializes the logging rotater to write logs to logFile and
// create roll files in the same directory. initLogging must be called before
// the package-global log rotator variables are used.
func InitLogging(logFilename, lvl string, stdout bool, utc bool) (lm *util.LoggerMaker, closeFn func()) {
	logDirectory := filepath.Dir(logFilename)
	err := os.MkdirAll(logDirectory, 0700)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create log directory: %v\n", err)
		os.Exit(1)
	}
	logRotator, err = rotator.New(logFilename, 32*1024, false, maxLogRolls)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create file rotator: %v\n", err)
		os.Exit(1)
	}
	if !stdout {
		fmt.Println("Logging to", logFilename)
	}
	lm, err = util.NewLoggerMaker(&logWriter{logRotator, stdout}, lvl, utc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create custom logger: %v\n", err)
		os.Exit(1)
	}
	lm.SetLevelsFromMap(defaultLogLevelMap)

	// Route the shared utxowallet subsystems (which use btclog, not slog)
	// through the app's own backend, so their output lands in the single app
	// log file (and stdout) with the same timestamp zone and format as every
	// other subsystem. A subsystem named in the configured levels (e.g.
	// NTRNO=trace) takes that level; the rest keep the default utxowallet
	// assigns them.
	utxowallet.UseLogger(func(name string, lvl btclog.Level) btclog.Logger {
		if configured, ok := lm.Levels[name]; ok {
			lvl = btclog.Level(configured)
		}
		return btclogAdapter{lm.NewLogger(name, slog.Level(lvl))}
	})

	return lm, func() {
		_ = logRotator.Close()
	}
}

// btclogAdapter lets a util.Logger stand in for a btclog.Logger. The two share
// every logging method and number their levels identically, trace (0) through
// off (6), so only the Level type needs converting. Writing through the slog
// backend is what makes the utxowallet subsystems honor the UTC flag: a btclog
// backend stamps local time unconditionally.
type btclogAdapter struct {
	util.Logger
}

var _ btclog.Logger = btclogAdapter{}

// Level returns the current logging level.
func (a btclogAdapter) Level() btclog.Level {
	return btclog.Level(a.Logger.Level())
}

// SetLevel changes the logging level.
func (a btclogAdapter) SetLevel(lvl btclog.Level) {
	a.Logger.SetLevel(slog.Level(lvl))
}
