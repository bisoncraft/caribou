package assets

import "testing"

// TestDashParams validates that the Dash netparams are internally consistent:
// for every network the genesis merkle root equals the coinbase txid, the
// X11 HeaderHash of the genesis header reproduces the declared GenesisHash, and
// the genesis header satisfies its own proof-of-work target via CheckPoW.
func TestDashParams(t *testing.T) {
	for net, p := range DashParams {
		t.Run(net, func(t *testing.T) {
			hdr := &p.GenesisBlock.Header

			if mr := p.GenesisBlock.Transactions[0].TxHash(); mr != hdr.MerkleRoot {
				t.Fatalf("merkle root %s != coinbase txid %s", hdr.MerkleRoot, mr)
			}

			if got := p.HeaderHash(hdr); got != *p.GenesisHash {
				t.Fatalf("X11 header hash %s != GenesisHash %s", got, p.GenesisHash)
			}

			if p.HeaderHashFunc == nil {
				t.Fatal("HeaderHashFunc not set")
			}
			if p.CheckPoW == nil {
				t.Fatal("CheckPoW not set")
			}
			if err := p.CheckPoW(hdr); err != nil {
				t.Fatalf("genesis CheckPoW: %v", err)
			}

			// The claimed target is bounded by PowLimit: a header whose Bits
			// encode an easier-than-limit target must fail CheckPoW even
			// though any hash trivially satisfies the inflated target.
			// 0x21008000 encodes 2^255, above every network's limit.
			easy := *hdr
			easy.Bits = 0x21008000
			if err := p.CheckPoW(&easy); err == nil {
				t.Fatal("CheckPoW accepted a target above PowLimit")
			}

			// Sanity: no SegWit on Dash, and the real P2P magic must be set.
			if p.Bech32HRPSegwit != "" {
				t.Fatalf("unexpected bech32 HRP %q", p.Bech32HRPSegwit)
			}
			if p.Net == 0 {
				t.Fatal("network magic not set")
			}
		})
	}
}
