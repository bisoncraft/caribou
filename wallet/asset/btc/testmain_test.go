//go:build !harness

package btc

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

func randBytes(l int) []byte {
	b := make([]byte, l)
	crand.Read(b)
	return b
}

var (
	tLogger util.Logger
	tCtx    context.Context
	tTxID   = "308e9a3675fc3ea3862b7863eeead08c621dcc37ff59de597dd3cdab41450ad9"
	tTxHash *chainhash.Hash
	tP2PKH  []byte
	tP2WPKH []byte
)

func TestMain(m *testing.M) {
	tLogger = util.StdOutLogger("TEST", util.LevelCritical)
	var shutdown func()
	tCtx, shutdown = context.WithCancel(context.Background())
	tTxHash, _ = chainhash.NewHashFromStr(tTxID)
	tP2PKH, _ = hex.DecodeString("76a9148fc02268f208a61767504fe0b48d228641ba81e388ac")
	tP2WPKH, _ = hex.DecodeString("0014148fc02268f208a61767504fe0b48d228641ba81")
	// tP2SH, _ = hex.DecodeString("76a91412a9abf5c32392f38bd8a1f57d81b1aeecc5699588ac")
	doIt := func() int {
		// Not counted as coverage, must test Archiver constructor explicitly.
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}
