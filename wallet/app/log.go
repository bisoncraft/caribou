// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet"
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

	// Route the shared utxowallet subsystems (which use btclog, not slog) to the
	// same destination as the app loggers, so their output lands in the single
	// app log file (and stdout) rather than a parallel per-wallet file.
	utxowallet.UseLogger(btclog.NewBackend(&logWriter{logRotator, stdout}))

	return lm, func() {
		_ = logRotator.Close()
	}
}
