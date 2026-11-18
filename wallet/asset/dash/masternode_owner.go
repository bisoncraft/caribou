// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package dash

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/dash/masternode"
	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
)

var _ asset.MasternodeOwner = (*dashWallet)(nil)

// Masternodes lists the masternodes the wallet owns shares of, as registered.
// It makes no server call.
func (dw *dashWallet) Masternodes(_ context.Context) ([]*asset.Masternode, error) {
	mgr, err := dw.manager()
	if err != nil {
		return nil, err
	}
	owned := mgr.List()
	out := make([]*asset.Masternode, len(owned))
	for i := range owned {
		out[i] = toAssetMN(&owned[i])
	}
	return out, nil
}

func toAssetMN(o *masternode.Owned) *asset.Masternode {
	own := make(map[int]bool, len(o.Shares))
	for _, i := range o.Shares {
		own[i] = true
	}
	mn := &asset.Masternode{
		ProTxHash:         o.MN.ProTxHash.String(),
		Collateral:        o.MN.Collateral.String(),
		RegisteredHeight:  o.MN.RegisteredHeight,
		EarlyPeriodBlocks: o.MN.EarlyPeriodBlocks,
		EarlyPenalty:      uint64(o.MN.EarlyPenalty),
		Shares:            make([]*asset.MasternodeShare, len(o.MN.Shares)),
	}
	for i, s := range o.MN.Shares {
		mn.Shares[i] = &asset.MasternodeShare{
			Amount:       uint64(s.Amount),
			Own:          own[i],
			RefundScript: hex.EncodeToString(s.RefundScript),
			RewardScript: hex.EncodeToString(s.RewardScript),
		}
		if own[i] {
			mn.Staked += uint64(s.Amount)
		}
	}
	if o.Dissolution != nil {
		mn.DissolvedHeight = o.Dissolution.Height
	}
	return mn
}

// MasternodeStatus is one masternode with what the server reports for it. A
// server that cannot answer, or answers something that does not match the
// registration, leaves the state unknown and says why in ReportError: what is
// known from the chain is still returned. Known keys come only from the
// registration, never from the server's reply.
func (dw *dashWallet) MasternodeStatus(ctx context.Context, proTxHash string) (*asset.MasternodeStatus, error) {
	mgr, err := dw.manager()
	if err != nil {
		return nil, err
	}
	hash, err := chainhash.NewHashFromStr(proTxHash)
	if err != nil {
		return nil, fmt.Errorf("invalid pro tx hash: %w", err)
	}
	var found *masternode.Owned
	owned := mgr.List()
	for i := range owned {
		if owned[i].MN.ProTxHash == *hash {
			found = &owned[i]
			break
		}
	}
	if found == nil {
		return nil, masternode.ErrUnknownMN
	}
	known := dw.keysOf(found.MN.PubKeyOperator, found.MN.KeyIDVoting)
	st := &asset.MasternodeStatus{
		Masternode:     *toAssetMN(found),
		State:          masternode.StateUnknown.String(),
		KnownKeys:      []*asset.MasternodeKeys{known},
		LastPaidHeight: -1,
	}
	if found.Dissolution != nil {
		st.State = masternode.StateDissolved.String()
		return st, nil
	}
	reported, info, err := mgr.Reported(ctx, *hash)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, masternode.ErrDissolved):
		// A refresh found the spend after the list was taken.
		st.State = masternode.StateDissolved.String()
	case err != nil:
		st.ReportError = err.Error()
	default:
		st.State = masternode.DeriveState(info, false).String()
		st.LastPaidHeight = info.State.LastPaidHeight
		st.ReportedKeys = dw.keysOf(reported.PubKeyOperator, reported.KeyIDVoting)
		// Compare the raw keys, both together. Only the registration's are
		// known so far.
		st.ReportedKeysKnown = found.MN.PubKeyOperator == reported.PubKeyOperator &&
			found.MN.KeyIDVoting == reported.KeyIDVoting
	}
	return st, nil
}

func (dw *dashWallet) keysOf(op [protx.OperatorPubKeySize]byte, voting [protx.KeyIDSize]byte) *asset.MasternodeKeys {
	k := &asset.MasternodeKeys{OperatorPubKey: hex.EncodeToString(op[:])}
	// A key id is 20 bytes, so the address cannot fail to encode.
	if a, err := btcutil.NewAddressPubKeyHash(voting[:], dw.net); err == nil {
		k.VotingAddress = a.EncodeAddress()
	}
	return k
}
