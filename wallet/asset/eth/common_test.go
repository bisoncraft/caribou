// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"bytes"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestDecodeCoinID(t *testing.T) {
	t.Parallel()
	t.Run("tx hash", func(t *testing.T) {
		t.Parallel()
		raw := bytes.Repeat([]byte{0xab}, common.HashLength)
		cid, err := DecodeCoinID(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(cid.TxHash[:], raw) {
			t.Errorf("TxHash = %x, want %x", cid.TxHash, raw)
		}
	})

	t.Run("wrong length", func(t *testing.T) {
		t.Parallel()
		for _, n := range []int{0, 1, common.HashLength - 1, common.HashLength + 1, common.HashLength * 2} {
			if _, err := DecodeCoinID(make([]byte, n)); err == nil {
				t.Errorf("expected error for coin ID of length %d", n)
			}
		}
	})
}

func TestCheckAPIModules(t *testing.T) {
	t.Parallel()

	// A fresh rpc.Server auto-registers the "rpc" module (server.go's
	// MetadataApi), which is enough to serve rpc_modules without any
	// blockchain-specific namespace.
	newClient := func(t *testing.T) *rpc.Client {
		t.Helper()
		server := rpc.NewServer()
		t.Cleanup(server.Stop)
		return rpc.DialInProc(server)
	}

	t.Run("required module present", func(t *testing.T) {
		t.Parallel()
		c := newClient(t)
		if err := CheckAPIModules(c, "test-endpoint", util.Disabled, []string{"rpc"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("required module missing", func(t *testing.T) {
		t.Parallel()
		c := newClient(t)
		if err := CheckAPIModules(c, "test-endpoint", util.Disabled, []string{"eth"}); err == nil {
			t.Fatal("expected error for missing eth module")
		}
	})
}
