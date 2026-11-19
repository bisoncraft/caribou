package appserver

import (
	"fmt"
	"net/http"

	"github.com/bisoncraft/caribou/util/encode"
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

func (s *AppServer) apiCreateMasternodeOffer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID uint32           `json:"assetID"`
		Amount  uint64           `json:"amount"`
		AppPW   encode.PassBytes `json:"appPW"`
	}
	if !readPost(w, r, &req) {
		return
	}
	appPW, err := s.resolvePass(req.AppPW, r)
	defer zero(appPW)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	offer, err := s.core.CreateMasternodeOffer(r.Context(), req.AssetID, appPW, req.Amount)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error creating a masternode offer for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, &struct {
		OK    bool                   `json:"ok"`
		Offer *asset.MasternodeOffer `json:"offer"`
	}{OK: true, Offer: offer})
}

func (s *AppServer) apiMasternodeOffers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID uint32 `json:"assetID"`
	}
	if !readPost(w, r, &req) {
		return
	}
	offers, err := s.core.MasternodeOffers(r.Context(), req.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error listing masternode offers for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, &struct {
		OK     bool                     `json:"ok"`
		Offers []*asset.MasternodeOffer `json:"offers"`
	}{OK: true, Offers: offers})
}

func (s *AppServer) apiCancelMasternodeOffer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID uint32 `json:"assetID"`
		OfferID string `json:"offerID"`
	}
	if !readPost(w, r, &req) {
		return
	}
	if err := s.core.CancelMasternodeOffer(r.Context(), req.AssetID, req.OfferID); err != nil {
		s.writeAPIError(w, fmt.Errorf("error cancelling a masternode offer for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, simpleAck())
}

func (s *AppServer) apiReviewMasternodeReg(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID uint32 `json:"assetID"`
		OfferID string `json:"offerID"`
		Blob    string `json:"blob"`
	}
	if !readPost(w, r, &req) {
		return
	}
	review, err := s.core.ReviewMasternodeRegistration(r.Context(), req.AssetID, req.OfferID, req.Blob)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error reviewing a masternode registration for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, &struct {
		OK     bool                       `json:"ok"`
		Review *asset.MasternodeRegReview `json:"review"`
	}{OK: true, Review: review})
}

func (s *AppServer) apiSignMasternodeReg(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID  uint32           `json:"assetID"`
		OfferID  string           `json:"offerID"`
		Blob     string           `json:"blob"`
		ReviewID string           `json:"reviewID"`
		AppPW    encode.PassBytes `json:"appPW"`
	}
	if !readPost(w, r, &req) {
		return
	}
	appPW, err := s.resolvePass(req.AppPW, r)
	defer zero(appPW)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	signed, err := s.core.SignMasternodeRegistration(r.Context(), req.AssetID, appPW, req.OfferID, req.Blob, req.ReviewID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error signing a masternode registration for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, &struct {
		OK     bool                       `json:"ok"`
		Signed *asset.MasternodeRegSigned `json:"signed"`
	}{OK: true, Signed: signed})
}

func (s *AppServer) apiBroadcastMasternodeReg(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID uint32 `json:"assetID"`
		OfferID string `json:"offerID"`
		Blob    string `json:"blob"`
	}
	if !readPost(w, r, &req) {
		return
	}
	txid, err := s.core.BroadcastMasternodeRegistration(r.Context(), req.AssetID, req.OfferID, req.Blob)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error broadcasting a masternode registration for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, &struct {
		OK   bool   `json:"ok"`
		TxID string `json:"txID"`
	}{OK: true, TxID: txid})
}
