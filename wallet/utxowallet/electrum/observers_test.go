// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// mineTestHeader produces a header whose hash beats its own claimed target —
// trivial at regtest-grade bits, which is exactly the attack auditDissent's
// callers must defeat via the cumulative-work comparison.
func mineTestHeader(t *testing.T, prev chainhash.Hash, bits uint32, seed uint32) *wire.BlockHeader {
	t.Helper()
	target := blockchain.CompactToBig(bits)
	for nonce := seed; ; nonce++ {
		hdr := &wire.BlockHeader{Version: 4, PrevBlock: prev, Bits: bits, Nonce: nonce}
		hash := hdr.BlockHash()
		if blockchain.HashToBig(&hash).Cmp(target) <= 0 {
			return hdr
		}
	}
}

func TestAuditDissent(t *testing.T) {
	const easyBits = 0x207fffff   // regtest-grade: nearly free to "mine"
	const harderBits = 0x1f00ffff // 128x more work per header

	pow := func(hdr *wire.BlockHeader) error {
		hash := hdr.BlockHash()
		if blockchain.HashToBig(&hash).Cmp(blockchain.CompactToBig(hdr.Bits)) > 0 {
			return errors.New("pow")
		}
		return nil
	}

	mkChain := func(n int, bits uint32) []*wire.BlockHeader {
		var out []*wire.BlockHeader
		prev := chainhash.Hash{}
		for i := 0; i < n; i++ {
			hdr := mineTestHeader(t, prev, bits, uint32(i)*1000)
			out = append(out, hdr)
			prev = hdr.BlockHash()
		}
		return out
	}

	linked := mkChain(4, easyBits)
	tip := linked[len(linked)-1].BlockHash()

	// A linked, PoW-valid batch audits clean and reports its work.
	work, err := auditDissent(linked, tip, pow)
	if err != nil {
		t.Fatalf("linked batch rejected: %v", err)
	}

	// Self-attested trivial difficulty MUST lose the work comparison the
	// caller performs: 4 easy headers carry less work than one harder one.
	if harder := blockchain.CalcWork(harderBits); work.Cmp(harder) >= 0 {
		t.Fatalf("4 easy headers (%s) should not outweigh one harder header (%s)",
			work, harder)
	}

	// Broken linkage is rejected.
	broken := mkChain(4, easyBits)
	broken[2].PrevBlock = chainhash.Hash{0xde, 0xad}
	if _, err := auditDissent(broken, broken[3].BlockHash(), pow); err == nil {
		t.Fatal("unlinked batch accepted")
	}

	// A batch not ending at the advertised tip is rejected — the observer
	// must be audited on the chain it actually advertised.
	if _, err := auditDissent(linked, chainhash.Hash{1}, pow); err == nil {
		t.Fatal("wrong-tip batch accepted")
	}

	// Empty batches are rejected.
	if _, err := auditDissent(nil, tip, pow); err == nil {
		t.Fatal("empty batch accepted")
	}
}
