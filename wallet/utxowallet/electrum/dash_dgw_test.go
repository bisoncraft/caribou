// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/wire"
)

// readHeaderFile parses testdata/name: hex-encoded serialized headers, one
// per line. Blank lines and lines starting with # are skipped.
func readHeaderFile(t *testing.T, name string) []*wire.BlockHeader {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var hdrs []*wire.BlockHeader
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		raw, err := hex.DecodeString(line)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var hdr wire.BlockHeader
		if err := hdr.Deserialize(bytes.NewReader(raw)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		hdrs = append(hdrs, &hdr)
	}
	return hdrs
}

// dashMainnetStore returns a header store for params anchored at cp and
// holding the checkpoint header alone. Below it the store has nothing but
// the checkpoint's difficulty entries.
func dashMainnetStore(t *testing.T, params *netparams.ChainParams, cp netparams.Checkpoint) *Headers {
	t.Helper()
	h := mustNewHeaders(t, &ChainConfig{
		Chain:       bisonwire.ChainDASH,
		Params:      params,
		HeaderSize:  80,
		StartHeight: cp.Height,
	})
	if _, err := h.AppendBatch(cp.Height, serializeBatch(t, []*wire.BlockHeader{cp.Header})); err != nil {
		t.Fatalf("anchor at %d: %v", cp.Height, err)
	}
	return h
}

// realDashHeaders returns the 23 mainnet headers above cp from testdata,
// checked to link to it.
func realDashHeaders(t *testing.T, params *netparams.ChainParams, cp netparams.Checkpoint) []*wire.BlockHeader {
	t.Helper()
	hdrs := readHeaderFile(t, fmt.Sprintf("dash-mainnet-%d-%d.hex", cp.Height+1, cp.Height+23))
	if len(hdrs) != params.DifficultyLookback {
		t.Fatalf("%d headers above checkpoint %d, want %d", len(hdrs), cp.Height, params.DifficultyLookback)
	}
	prev := *cp.Hash
	for i, hdr := range hdrs {
		if hdr.PrevBlock != prev {
			t.Fatalf("header %d does not link to its parent", cp.Height+1+int32(i))
		}
		prev = params.HeaderHash(hdr)
	}
	return hdrs
}

// TestDashMainnetDGWFromCheckpoint checks that a checkpoint's difficulty
// entries are enough to validate the blocks above it. A store anchored at each
// Dash mainnet checkpoint accepts the 23 real headers above it, each of whose
// Dark Gravity Wave inputs reach below the anchor. With altered bits, the
// contextual validator refuses each of them for its consensus bits, and
// AppendBatch refuses each as a *RuleError, possibly at its proof-of-work
// check before the bits rule runs. TestDashMainnetForgedPowLimitHeader covers
// AppendBatch refusing a header for its bits. One entry fewer leaves the first
// of them without its inputs.
func TestDashMainnetDGWFromCheckpoint(t *testing.T) {
	params := assets.DashParams["mainnet"]
	for _, cp := range params.Checkpoints {
		t.Run(strconv.Itoa(int(cp.Height)), func(t *testing.T) {
			hdrs := realDashHeaders(t, params, cp)
			h := dashMainnetStore(t, params, cp)
			for i, hdr := range hdrs {
				height := cp.Height + 1 + int32(i)
				for _, bits := range []uint32{hdr.Bits + 1, hdr.Bits - 1, params.PowLimitBits} {
					bad := *hdr
					bad.Bits = bits
					// The contextual checks alone, without the PoW hash
					// check that runs above the newest checkpoint.
					h.mu.RLock()
					_, err := h.validateBranchLocked(height-1, []*wire.BlockHeader{&bad}, time.Now())
					h.mu.RUnlock()
					if at, ok := isRuleError(err); !ok || at != height ||
						!strings.Contains(err.Error(), "consensus requires") {
						t.Fatalf("header %d with bits 0x%08x: got %v, want a bits *RuleError", height, bits, err)
					}
					if _, err := h.AppendBatch(height, serializeBatch(t, []*wire.BlockHeader{&bad})); err == nil {
						t.Fatalf("header %d with bits 0x%08x appended", height, bits)
					} else if _, ok := isRuleError(err); !ok {
						t.Fatalf("header %d with bits 0x%08x: %v is not a *RuleError", height, bits, err)
					}
				}
				if _, err := h.AppendBatch(height, serializeBatch(t, []*wire.BlockHeader{hdr})); err != nil {
					t.Fatalf("real header %d: %v", height, err)
				}
			}
			if tip := h.Tip(); tip != cp.Height+23 {
				t.Fatalf("tip %d, want %d", tip, cp.Height+23)
			}

			short := cp
			short.Difficulty = cp.Difficulty[1:]
			shortParams := &netparams.ChainParams{
				Chain:              params.Chain,
				Name:               params.Name,
				GenesisHash:        params.GenesisHash,
				PowLimit:           params.PowLimit,
				PowLimitBits:       params.PowLimitBits,
				Checkpoints:        []netparams.Checkpoint{short},
				CheckPoW:           params.CheckPoW,
				HeaderHashFunc:     params.HeaderHashFunc,
				NextBits:           params.NextBits,
				DifficultyLookback: params.DifficultyLookback - 1,
			}
			hs := dashMainnetStore(t, shortParams, short)
			_, err := hs.AppendBatch(cp.Height+1, serializeBatch(t, hdrs[:1]))
			if err == nil || !strings.Contains(err.Error(), "inputs unavailable") {
				t.Fatalf("first header above a 22-entry checkpoint: got %v, want missing inputs", err)
			}
		})
	}
}

// TestDashMainnetForgedPowLimitHeader checks that a header mined to the
// PowLimit target on top of the real chain is refused for its bits. Its proof
// of work is valid, so the bits rule is the check that stops it.
func TestDashMainnetForgedPowLimitHeader(t *testing.T) {
	params := assets.DashParams["mainnet"]
	cp := params.Checkpoints[len(params.Checkpoints)-1]
	hdrs := realDashHeaders(t, params, cp)
	h := dashMainnetStore(t, params, cp)
	if _, err := h.AppendBatch(cp.Height+1, serializeBatch(t, hdrs)); err != nil {
		t.Fatalf("real headers: %v", err)
	}
	forged := readHeaderFile(t, "dash-mainnet-2540024-forged.hex")[0]
	if forged.PrevBlock != params.HeaderHash(hdrs[len(hdrs)-1]) {
		t.Fatal("forged header does not extend the real tip")
	}
	if forged.Bits != params.PowLimitBits {
		t.Fatalf("forged header has bits 0x%08x, want PowLimit bits 0x%08x", forged.Bits, params.PowLimitBits)
	}
	if err := h.checkPoW(forged); err != nil {
		t.Fatalf("forged header fails proof of work: %v", err)
	}
	height := cp.Height + 24
	_, err := h.AppendBatch(height, serializeBatch(t, []*wire.BlockHeader{forged}))
	if at, ok := isRuleError(err); !ok || at != height || !strings.Contains(err.Error(), "consensus requires") {
		t.Fatalf("forged header: got %v, want a bits *RuleError at %d", err, height)
	}
	if tip := h.Tip(); tip != height-1 {
		t.Fatalf("tip %d after the refused header, want %d", tip, height-1)
	}
}
