// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package dash

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/dash/masternode"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
)

// infoChain answers protx.info with a fixed reply, at a tip that matches it.
type infoChain struct {
	chainStub
	tip   int32
	info  *electrum.ProTxInfo
	err   error
	calls int
}

func (c *infoChain) Tip() int32 { return c.tip }
func (c *infoChain) ProTxInfo(context.Context, chainhash.Hash) (*electrum.ProTxInfo, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	cp := *c.info
	return &cp, nil
}
func (c *infoChain) HeaderByHeight(int32) *wire.BlockHeader { return nil }

func loadProTxInfo(t *testing.T, name string) *electrum.ProTxInfo {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("masternode", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var info electrum.ProTxInfo
	if err := json.Unmarshal(b, &info); err != nil {
		t.Fatal(err)
	}
	return &info
}

// ownerWallet is a wallet whose Manager holds the registration vector, with the
// chain answering for it.
func ownerWallet(t *testing.T, chain *infoChain) (*dashWallet, string, int64) {
	t.Helper()
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, amount := stakedManager(t, chain, seqKeys{})
	dw.mn.Store(mgr)
	return dw, chain.info.ProTxHash, amount
}

func newInfoChain(t *testing.T, name string) *infoChain {
	return &infoChain{tip: 910, info: loadProTxInfo(t, name)}
}

// Server data never becomes known keys: the known keys are the registration's
// whatever the server says, and the reported keys are marked as known only when
// both the operator key and the voting address match.
func TestMasternodeStatusKeys(t *testing.T) {
	const otherAddr = "ySkrz68ugbp6ny9JmMToJE9E2AeqnV4xnd"
	tests := []struct {
		name      string
		mutate    func(*electrum.ProTxInfo)
		wantKnown bool
		wantState string
	}{
		{"unchanged", func(*electrum.ProTxInfo) {}, true, "active"},
		{"voting address changed", func(i *electrum.ProTxInfo) { i.State.VotingAddress = otherAddr }, false, "active"},
		{"operator key changed", func(i *electrum.ProTxInfo) { i.State.PubKeyOperator = "00" + i.State.PubKeyOperator[2:] }, false, "active"},
		{"banned", func(i *electrum.ProTxInfo) { i.State.PoSeBanHeight = 900 }, true, "banned"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chain := newInfoChain(t, "protx_info_shared.json")
			dw, hash, _ := ownerWallet(t, chain)
			registered, err := dw.MasternodeStatus(t.Context(), hash)
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(chain.info)
			st, err := dw.MasternodeStatus(t.Context(), hash)
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tt.wantState {
				t.Fatalf("state %q", st.State)
			}
			if st.ReportedKeysKnown != tt.wantKnown {
				t.Fatalf("reported keys known: %v", st.ReportedKeysKnown)
			}
			if len(st.KnownKeys) != 1 || *st.KnownKeys[0] != *registered.KnownKeys[0] {
				t.Fatalf("known keys followed the server: %+v", st.KnownKeys)
			}
			if st.ReportedKeys == nil || st.ReportedKeys.VotingAddress != chain.info.State.VotingAddress ||
				st.ReportedKeys.OperatorPubKey != chain.info.State.PubKeyOperator {
				t.Fatalf("reported keys %+v", st.ReportedKeys)
			}
			if st.LastPaidHeight != chain.info.State.LastPaidHeight {
				t.Fatalf("last paid %d", st.LastPaidHeight)
			}
		})
	}
}

// A server that cannot answer leaves the state unknown, never dissolved, and
// the registered data is still returned. A cancelled request is an error.
func TestMasternodeStatusServerFailure(t *testing.T) {
	for name, tt := range map[string]struct {
		err  error
		tip  int32
		want string
	}{
		"unsupported": {err: electrum.ErrProTxUnsupported, tip: 910},
		"not found":   {err: electrum.ErrProTxNotFound, tip: 910},
		"stale":       {tip: 911},
	} {
		t.Run(name, func(t *testing.T) {
			chain := newInfoChain(t, "protx_info_shared.json")
			chain.err, chain.tip = tt.err, tt.tip
			dw, hash, amount := ownerWallet(t, chain)
			st, err := dw.MasternodeStatus(t.Context(), hash)
			if err != nil {
				t.Fatal(err)
			}
			if st.State != "unknown" || st.ReportError == "" || st.ReportedKeys != nil || st.ReportedKeysKnown {
				t.Fatalf("%+v", st)
			}
			if st.LastPaidHeight != -1 || len(st.KnownKeys) != 1 || int64(st.Staked) != amount {
				t.Fatalf("registered data missing: %+v", st)
			}
		})
	}
	// A reply that contradicts the registration is not taken, and not read as a
	// dissolution.
	chain := newInfoChain(t, "protx_info_shared.json")
	chain.info.CollateralIndex++
	dw, hash, _ := ownerWallet(t, chain)
	if st, err := dw.MasternodeStatus(t.Context(), hash); err != nil || st.State != "unknown" ||
		st.ReportError == "" || st.ReportedKeys != nil {
		t.Fatalf("%+v %v", st, err)
	}

	chain = newInfoChain(t, "protx_info_shared.json")
	dw, hash, _ = ownerWallet(t, chain)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	chain.err = ctx.Err()
	if _, err := dw.MasternodeStatus(ctx, hash); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled request returned %v", err)
	}
}

func TestMasternodeStatusInput(t *testing.T) {
	chain := newInfoChain(t, "protx_info_shared.json")
	dw, _, _ := ownerWallet(t, chain)
	if _, err := dw.MasternodeStatus(t.Context(), "zz"); err == nil {
		t.Fatal("a malformed hash")
	}
	if _, err := dw.MasternodeStatus(t.Context(), chainhash.Hash{1}.String()); !errors.Is(err, masternode.ErrUnknownMN) {
		t.Fatalf("an unknown masternode: %v", err)
	}
	if chain.calls != 0 {
		t.Fatalf("%d server calls for input that was refused", chain.calls)
	}
	// Without a Manager the methods return why.
	bare := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	why := errors.New("no protx")
	bare.mnErr.Store(&why)
	if _, err := bare.Masternodes(t.Context()); !errors.Is(err, why) {
		t.Fatalf("%v", err)
	}
	if _, err := bare.MasternodeStatus(t.Context(), chainhash.Hash{}.String()); !errors.Is(err, why) {
		t.Fatalf("%v", err)
	}
}

// Masternodes is the registered data and costs no server call.
func TestMasternodes(t *testing.T) {
	chain := newInfoChain(t, "protx_info_shared.json")
	dw, hash, amount := ownerWallet(t, chain)
	mns, err := dw.Masternodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(mns) != 1 || mns[0].ProTxHash != hash {
		t.Fatalf("%+v", mns)
	}
	mn := mns[0]
	if mn.Staked != uint64(amount) || mn.RegisteredHeight != 798 || mn.DissolvedHeight != 0 || len(mn.Shares) != 2 {
		t.Fatalf("%+v", mn)
	}
	if !mn.Shares[0].Own || mn.Shares[1].Own || mn.Shares[0].RefundScript == "" {
		t.Fatalf("shares %+v %+v", mn.Shares[0], mn.Shares[1])
	}
	if chain.calls != 0 {
		t.Fatalf("%d server calls", chain.calls)
	}
}

func TestMasternodeOwnerTrait(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	if !asset.DetermineWalletTraits(dw).IsMasternodeOwner() {
		t.Fatal("the shared wallet is not a masternode owner")
	}
}

// A dissolved masternode is reported from the chain, without asking the server.
func TestMasternodeStatusDissolved(t *testing.T) {
	chain := newInfoChain(t, "protx_info_shared.json")
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, _ := stakedManagerWith(t, chain, seqKeys{}, &masternode.Spend{Height: 850})
	dw.mn.Store(mgr)
	st, err := dw.MasternodeStatus(t.Context(), chain.info.ProTxHash)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "dissolved" || st.DissolvedHeight != 850 || st.ReportedKeys != nil || chain.calls != 0 {
		t.Fatalf("%+v, %d server calls", st, chain.calls)
	}
}
