// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"fmt"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
)

// RestorationInfo describes how to restore the wallet in other software from
// the seed core gives it: the 24-word BIP 39 phrase of the seed. The phrase
// needs nothing from the database. The birthday is the account's while the
// database is open; while it is closed (a disabled wallet, a failed open, a
// damaged database) it is the activation of NU6.3, a lower bound for any
// account, and the instructions say so.
func (w *zecWallet) RestorationInfo(seed []byte) ([]*asset.WalletRestoration, error) {
	if len(seed) != entropyLen {
		return nil, fmt.Errorf("the seed is %d bytes, want %d", len(seed), entropyLen)
	}
	birthday, exact := w.netCfg.network.IronwoodActivation(), false
	if w.net == util.Simnet {
		// The harness serves no tree state for height 0, below the first
		// birthday a scan can start from.
		birthday = simnetBirthday
	}
	if _, account, release, err := w.backend(); err == nil {
		birthday, exact = account.Birthday, true
		release()
	}
	phrase, err := mnemonicFor(seed)
	if err != nil {
		return nil, err
	}
	needs := fmt.Sprintf("The wallet must support the Ironwood pool (NU6.3 or later) and "+
		"restore ZIP 32 account 0 of a 24-word BIP 39 phrase with an empty passphrase. "+
		"Its scan must start no later than the first payment to this wallet: a birthday "+
		"height of %d or lower. A lower birthday is safe and only makes the scan longer.", birthday)
	if !exact {
		needs += fmt.Sprintf(" The wallet's own birthday could not be read; %d is the activation height of NU6.3, "+
			"below which no Zcash account of this wallet begins.", birthday)
	}
	return []*asset.WalletRestoration{{
		Target:   "zcash-devtool",
		Seed:     phrase,
		SeedName: "Seed phrase (24 words)",
		Instructions: fmt.Sprintf("Create a wallet with \"zcash-devtool wallet init\", entering the "+
			"phrase when asked, with \"--birthday %d\". %s", birthday, needs),
	}, {
		Target:       "Other Zcash wallets",
		Seed:         phrase,
		SeedName:     "Seed phrase (24 words)",
		Instructions: "Restore from the seed phrase. " + needs,
	}}, nil
}
