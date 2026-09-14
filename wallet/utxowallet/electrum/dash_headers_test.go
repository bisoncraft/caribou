package electrum

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// dashTestConfig returns a ChainConfig for the Dash regtest params, whose
// HeaderHash hook is X11.
func dashTestConfig() *ChainConfig {
	return &ChainConfig{
		Chain:       bisonwire.ChainDASH,
		Params:      assets.DashParams["simnet"],
		HeaderSize:  80,
		StartHeight: 0,
	}
}

// mineDash bumps hdr.Nonce until its X11 hash satisfies its target, so the
// header passes the per-header PoW check (the regtest target rejects ~half of
// random hashes).
func mineDash(cfg *ChainConfig, hdr *wire.BlockHeader) {
	target := blockchain.CompactToBig(hdr.Bits)
	for {
		hh := cfg.Params.HeaderHash(hdr)
		if blockchain.HashToBig(&hh).Cmp(target) <= 0 {
			return
		}
		hdr.Nonce++
	}
}

// buildDashChain returns n mined headers linked by the chain's X11 header hash
// (params.HeaderHash), starting from a zero prev-block, plus their concatenated
// serialization.
func buildDashChain(t *testing.T, cfg *ChainConfig, n int) ([]*wire.BlockHeader, []byte) {
	t.Helper()
	var (
		hdrs []*wire.BlockHeader
		raw  bytes.Buffer
		prev chainhash.Hash // zero for the first header
	)
	for i := range n {
		hdr := &wire.BlockHeader{
			Version:    1,
			PrevBlock:  prev,
			MerkleRoot: chainhash.Hash{byte(i + 1)},
			Timestamp:  time.Unix(1417713337+int64(i), 0),
			Bits:       0x207fffff,
		}
		mineDash(cfg, hdr)
		if err := hdr.Serialize(&raw); err != nil {
			t.Fatal(err)
		}
		hdrs = append(hdrs, hdr)
		prev = cfg.Params.HeaderHash(hdr) // X11 link to the next header
	}
	return hdrs, raw.Bytes()
}

// TestHeadersDashX11Linkage proves the electrum header store links and indexes
// Dash headers by their X11 identity hash, not sha256d.
func TestHeadersDashX11Linkage(t *testing.T) {
	cfg := dashTestConfig()
	h, err := NewHeaders(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("NewHeaders: %v", err)
	}

	hdrs, raw := buildDashChain(t, cfg, 4)
	n, err := h.AppendBatch(0, raw, 0)
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if n != 4 {
		t.Fatalf("appended %d, want 4", n)
	}
	if h.Tip() != 3 {
		t.Fatalf("tip = %d, want 3", h.Tip())
	}

	// TipHash and ByHash must be keyed by the X11 hash.
	wantTip := cfg.Params.HeaderHash(hdrs[3])
	if h.TipHash() != wantTip {
		t.Fatalf("TipHash = %s, want %s", h.TipHash(), wantTip)
	}
	x11h1 := cfg.Params.HeaderHash(hdrs[1])
	height, got, ok := h.ByHash(&x11h1)
	if !ok || height != 1 || got == nil {
		t.Fatalf("ByHash(X11(h1)) = (%d,%v,%v), want (1,hdr,true)", height, got, ok)
	}
	// The sha256d hash must NOT be indexed.
	sha := hdrs[1].BlockHash()
	if _, _, ok := h.ByHash(&sha); ok {
		t.Fatal("sha256d hash unexpectedly indexed for Dash")
	}

	if err := h.VerifyAll(); err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}
}

// TestHeadersDashRejectsSha256dLinkage confirms a batch whose second header is
// linked by sha256d (the wrong hash for Dash) fails to connect.
func TestHeadersDashRejectsSha256dLinkage(t *testing.T) {
	cfg := dashTestConfig()
	h, err := NewHeaders(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("NewHeaders: %v", err)
	}

	h0 := &wire.BlockHeader{
		Version:    1,
		MerkleRoot: chainhash.Hash{0x01},
		Timestamp:  time.Unix(1417713337, 0),
		Bits:       0x207fffff,
	}
	mineDash(cfg, h0)
	// h1 links to h0 by sha256d instead of X11 — wrong for Dash.
	h1 := &wire.BlockHeader{
		Version:    1,
		PrevBlock:  h0.BlockHash(), // sha256d, not X11
		MerkleRoot: chainhash.Hash{0x02},
		Timestamp:  time.Unix(1417713338, 0),
		Bits:       0x207fffff,
	}
	mineDash(cfg, h1)

	var raw bytes.Buffer
	if err := h0.Serialize(&raw); err != nil {
		t.Fatal(err)
	}
	if err := h1.Serialize(&raw); err != nil {
		t.Fatal(err)
	}

	// The break is inside the batch (h0 itself is genesis), which is a
	// server fault rather than a reorg signal: refused outright, nothing
	// appended.
	if _, err := h.AppendBatch(0, raw.Bytes(), 0); err == nil {
		t.Fatal("sha256d-linked batch accepted")
	} else if errors.Is(err, errHeaderDoesNotConnect) {
		t.Fatalf("interior link break reported as a reorg signal: %v", err)
	}
	if h.Tip() != -1 {
		t.Fatalf("tip = %d after a refused batch, want -1", h.Tip())
	}
}
