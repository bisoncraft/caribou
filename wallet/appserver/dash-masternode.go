package appserver

import (
	"fmt"
	"net/http"

	"github.com/bisoncraft/caribou/wallet/asset"
)

func (s *AppServer) apiMasternodes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID uint32 `json:"assetID"`
	}
	if !readPost(w, r, &req) {
		return
	}
	mns, err := s.core.Masternodes(r.Context(), req.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error listing masternodes for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, &struct {
		OK          bool                `json:"ok"`
		Masternodes []*asset.Masternode `json:"masternodes"`
	}{
		OK:          true,
		Masternodes: mns,
	})
}

func (s *AppServer) apiMasternodeStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID   uint32 `json:"assetID"`
		ProTxHash string `json:"proTxHash"`
	}
	if !readPost(w, r, &req) {
		return
	}
	st, err := s.core.MasternodeStatus(r.Context(), req.AssetID, req.ProTxHash)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error retrieving masternode status for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, &struct {
		OK     bool                    `json:"ok"`
		Status *asset.MasternodeStatus `json:"status"`
	}{
		OK:     true,
		Status: st,
	})
}
