// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"encoding/json"

	"github.com/bisoncraft/caribou/wallet/txdb"
)

// nearTxData carries NEAR-specific data alongside a txdb.WalletTransaction,
// encoded in its RawTx field.
type nearTxData struct {
	// SentAtHeight is the block height of the tip when the tx was broadcast,
	// used to age out a pre-stored pending tx that never landed (mirroring
	// xrp's LastLedgerSequence expiry).
	SentAtHeight uint64 `json:"sentAtHeight"`
}

func nearData(wt *txdb.WalletTransaction) nearTxData {
	var d nearTxData
	if len(wt.RawTx) > 0 {
		_ = json.Unmarshal(wt.RawTx, &d)
	}
	return d
}

// setSentAtHeight encodes height into wt.RawTx.
func setSentAtHeight(wt *txdb.WalletTransaction, height uint64) {
	b, _ := json.Marshal(nearTxData{SentAtHeight: height})
	wt.RawTx = b
}
