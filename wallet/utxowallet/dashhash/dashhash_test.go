// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dashhash

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// mustHash parses a big-endian (display) hash string.
func mustHash(t *testing.T, s string) chainhash.Hash {
	t.Helper()
	h, err := chainhash.NewHashFromStr(s)
	if err != nil {
		t.Fatalf("bad hash %q: %v", s, err)
	}
	return *h
}

// TestGenesisHashX11 is the linchpin validation for the whole Dash integration:
// X11 of each network's serialized 80-byte genesis header must reproduce the
// known genesis block hash. If this passes, the X11 primitive and the
// "block identity == X11(header)" model are both correct. Values are from
// dashpay/dash src/chainparams.cpp. The merkle root is shared across networks
// (single identical coinbase tx).
func TestGenesisHashX11(t *testing.T) {
	merkle := mustHash(t, "e0028eb9648db56b1ac77cf090b99048a8007e2bb64b68f092c03c7f56a662c7")

	tests := []struct {
		name   string
		time   int64
		bits   uint32
		nonce  uint32
		expect string
	}{
		{"mainnet", 1390095618, 0x1e0ffff0, 28917698, "00000ffd590b1485b3caadc19b22e6379c733355108f107a430458cdf3407ab6"},
		{"testnet", 1390666206, 0x1e0ffff0, 3861367235, "00000bafbc94add76cb75e2ec92894837288a481e5c005f6563d91623bf8bc2c"},
		{"regtest", 1417713337, 0x207fffff, 1096447, "000008ca1832a4baf228eb1553c03d3a2c8e02399550dd6ea8d65cec3ef23d2e"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := wire.BlockHeader{
				Version:    1,
				PrevBlock:  chainhash.Hash{},
				MerkleRoot: merkle,
				Timestamp:  time.Unix(tt.time, 0),
				Bits:       tt.bits,
				Nonce:      tt.nonce,
			}
			var buf bytes.Buffer
			if err := hdr.Serialize(&buf); err != nil {
				t.Fatalf("serialize: %v", err)
			}
			if buf.Len() != 80 {
				t.Fatalf("header length = %d, want 80", buf.Len())
			}

			got := HashX11(buf.Bytes())
			want := mustHash(t, tt.expect)
			if got != want {
				t.Fatalf("X11 genesis hash mismatch:\n got  %s\n want %s", got, want)
			}

			// sha256d must NOT match — proves Dash identity is X11, not sha256d.
			if sha := hdr.BlockHash(); sha == want {
				t.Fatalf("sha256d unexpectedly equals genesis hash; X11 assumption wrong")
			}
		})
	}
}
