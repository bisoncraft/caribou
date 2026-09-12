//go:build !harness

package btc

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
)

func TestCloneAndSPVParams(t *testing.T) {
	params := ReadCloneParams(&CloneParams{
		Name: "clone", PubKeyHashAddrID: 1, ScriptHashAddrID: 2, Bech32HRPSegwit: "cl",
		CoinbaseMaturity: 3, Net: uint32(wire.TestNet3), GenesisHash: chaincfg.TestNet3Params.GenesisHash,
		HDPrivateKeyID: [4]byte{1, 2, 3, 4}, HDPublicKeyID: [4]byte{5, 6, 7, 8},
	})
	if params.Name != "clone" || params.Net != wire.TestNet3 || params.Bech32HRPSegwit != "cl" || params.GenesisHash != chaincfg.TestNet3Params.GenesisHash {
		t.Fatalf("clone params = %+v", params)
	}
	spv := btcSPVChainConfig(&chaincfg.MainNetParams)
	if spv.RecoveryWindow != neutrinoRecoveryWindow {
		t.Fatalf("SPV recovery window = %d", spv.RecoveryWindow)
	}
}
