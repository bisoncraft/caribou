//go:build !harness

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dash

import (
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	inWallet "github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
)

func init() {
	// Real Create calls in this package's non-harness tests use the fast
	// scrypt parameters; excluded under the harness tag so the simnet
	// integration tests (electrum_simnet_test.go, neutrino_simnet_test.go)
	// keep exercising production KDF cost.
	createWalletOpts = []inWallet.LoaderOption{inWallet.WithScryptOptions(&waddrmgr.FastScryptOptions)}
}
