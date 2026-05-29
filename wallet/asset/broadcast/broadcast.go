// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package broadcast

import (
	"strings"
)

// IsAlreadyBroadcastErr checks if the error indicates the transaction is
// already in the mempool or blockchain.
func IsAlreadyBroadcastErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range []string{
		"txn-already-in-mempool",     // Bitcoin Core, btcd (mempool)
		"txn-already-known",          // Bitcoin Core (confirmed tx resubmission)
		"already in block chain",     // btcd
		"transaction already exists", // dcrd
		"tx already exists",          // Bitcoin Core
		"already have transaction",   // zcashd
		"already in the mempool",     // Electrum
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
