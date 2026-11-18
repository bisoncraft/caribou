package appserver

import (
	"fmt"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
)

func TestAPIMasternodes(t *testing.T) {
	writer := new(TWriter)
	reader := new(TReader)
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	tCore.mns = []*asset.Masternode{{ProTxHash: "ab", Collateral: "cd:2", Shares: []*asset.MasternodeShare{}}}
	body := &struct {
		AssetID uint32 `json:"assetID"`
	}{AssetID: 5}
	ensureResponse(t, s.apiMasternodes,
		`{"ok":true,"masternodes":[{"proTxHash":"ab","collateral":"cd:2","registeredHeight":0,"shares":[],"staked":0,"earlyPeriodBlocks":0,"earlyPenalty":0,"dissolvedHeight":0}]}`,
		reader, writer, body, nil)
	if tCore.mnAssetID != 5 {
		t.Fatalf("asset id %d", tCore.mnAssetID)
	}
	tCore.mnErr = errTest
	ensureResponse(t, s.apiMasternodes,
		fmt.Sprintf(`{"ok":false,"msg":"%s"}`, errTest), reader, writer, body, nil)
}

func TestAPIMasternodeStatus(t *testing.T) {
	writer := new(TWriter)
	reader := new(TReader)
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	tCore.mnStatus = &asset.MasternodeStatus{State: "active", LastPaidHeight: -1}
	tCore.mnStatus.ProTxHash = "ab"
	body := &struct {
		AssetID   uint32 `json:"assetID"`
		ProTxHash string `json:"proTxHash"`
	}{AssetID: 5, ProTxHash: "ab"}
	ensureResponse(t, s.apiMasternodeStatus,
		`{"ok":true,"status":{"proTxHash":"ab","collateral":"","registeredHeight":0,"shares":null,"staked":0,"earlyPeriodBlocks":0,"earlyPenalty":0,"dissolvedHeight":0,"state":"active","knownKeys":null,"reportedKeysKnown":false,"lastPaidHeight":-1}}`,
		reader, writer, body, nil)
	if tCore.mnAssetID != 5 || tCore.mnHash != "ab" {
		t.Fatalf("passed %d %q", tCore.mnAssetID, tCore.mnHash)
	}
	tCore.mnErr = errTest
	ensureResponse(t, s.apiMasternodeStatus,
		fmt.Sprintf(`{"ok":false,"msg":"%s"}`, errTest), reader, writer, body, nil)
}
