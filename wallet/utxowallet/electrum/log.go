// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import "github.com/btcsuite/btclog"

// log is a logger that is initialized with no output filters. The package
// performs no logging until UseLogger is called.
var log btclog.Logger

func init() {
	DisableLog()
}

// DisableLog disables all library log output.
func DisableLog() {
	log = btclog.Disabled
}

// UseLogger uses a specified Logger to output package logging info.
func UseLogger(logger btclog.Logger) {
	log = logger
}
