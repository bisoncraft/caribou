//go:build !live

package appserver

import (
	"fmt"
	"net/http"
	"slices"
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

// Each offer route passes what it was given to core, and an error comes back as
// the message.
func TestAPIMasternodeOffers(t *testing.T) {
	writer := new(TWriter)
	reader := new(TReader)
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	tCore.mnOffer = &asset.MasternodeOffer{ID: "o1", State: "open", Inputs: []string{}}
	tCore.mnOffers = []*asset.MasternodeOffer{tCore.mnOffer}
	tCore.mnReview = &asset.MasternodeRegReview{ReviewID: "r1", SignedShares: []int{}}
	tCore.mnSigned = &asset.MasternodeRegSigned{Blob: "b2", Complete: true}
	tCore.mnTxID = "tx1"
	tests := []struct {
		name string
		f    func(http.ResponseWriter, *http.Request)
		body any
		want string
		args []string
	}{
		{"create", s.apiCreateMasternodeOffer, map[string]any{"assetID": 5, "amount": 7, "appPW": "pw"},
			`{"ok":true,"offer":{"id":"o1","state":"open","createdAt":0,"expiresAt":0,"amount":0,"maxFee":0,"ownerAddress":"","refundAddress":"","rewardAddress":"","changeAddress":"","inputs":[],"inputsValue":0}}`,
			[]string{"pw", "7"}},
		{"list", s.apiMasternodeOffers, map[string]any{"assetID": 5},
			`{"ok":true,"offers":[{"id":"o1","state":"open","createdAt":0,"expiresAt":0,"amount":0,"maxFee":0,"ownerAddress":"","refundAddress":"","rewardAddress":"","changeAddress":"","inputs":[],"inputsValue":0}]}`, nil},
		{"cancel", s.apiCancelMasternodeOffer, map[string]any{"assetID": 5, "offerID": "o1"}, `{"ok":true}`, []string{"o1"}},
		{"review", s.apiReviewMasternodeReg, map[string]any{"assetID": 5, "offerID": "o1", "blob": "bb"},
			`{"ok":true,"review":{"reviewID":"r1","offerID":"","shareIndex":0,"terms":{"shares":null,"keys":{"operatorPubKey":"","votingAddress":""},"operatorReward":0,"earlyPeriodBlocks":0,"earlyPenalty":0,"netInfo":null},"collateralIndex":0,"feePortion":0,"otherOutputs":null,"signedShares":[]}}`,
			[]string{"o1", "bb"}},
		{"sign", s.apiSignMasternodeReg, map[string]any{"assetID": 5, "offerID": "o1", "blob": "bb", "reviewID": "r1", "appPW": "pw"},
			`{"ok":true,"signed":{"blob":"b2","complete":true}}`, []string{"pw", "o1", "bb", "r1"}},
		{"broadcast", s.apiBroadcastMasternodeReg, map[string]any{"assetID": 5, "offerID": "o1", "blob": "bb"},
			`{"ok":true,"txID":"tx1"}`, []string{"o1", "bb"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tCore.mnErr, tCore.mnArgs, tCore.mnAssetID = nil, nil, 0
			ensureResponse(t, tt.f, tt.want, reader, writer, tt.body, nil)
			if tCore.mnAssetID != 5 || !slices.Equal(tCore.mnArgs, tt.args) {
				t.Fatalf("core got %d %q, want %q", tCore.mnAssetID, tCore.mnArgs, tt.args)
			}
			tCore.mnErr = errTest
			ensureResponse(t, tt.f, fmt.Sprintf(`{"ok":false,"msg":"%s"}`, errTest), reader, writer, tt.body, nil)
		})
	}
}
