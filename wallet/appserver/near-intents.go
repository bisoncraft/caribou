// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// HTTP handlers for NEAR Intents cross-chain swaps. The engine and all
// swap logic live in wallet/core/near-intents.go; these handlers only
// marshal requests and responses.

package appserver

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/core"
)

// apiIntentsAssets is the handler for the '/intents/assets' API request.
// It returns the assets available for cross-chain swaps.
func (s *AppServer) apiIntentsAssets(w http.ResponseWriter, r *http.Request) {
	assets, err := s.core.IntentsAssets(r.Context())
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK     bool                 `json:"ok"`
		Assets []*core.IntentsAsset `json:"assets"`
	}{
		OK:     true,
		Assets: assets,
	})
}

// apiIntentsPreview is the handler for the '/intents/preview' API request.
// It returns a dry-quote price preview; no deposit address is created and
// no compliance screening runs. value is denominated in the origin asset's
// atoms for an exact-input preview, the destination asset's for
// exact-output.
func (s *AppServer) apiIntentsPreview(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		OriginAssetID *uint32 `json:"originAssetID"`
		DestAssetID   *uint32 `json:"destAssetID"`
		Value         uint64  `json:"value"`
		SlippageBps   int     `json:"slippageBps"`
		ExactOutput   bool    `json:"exactOutput"`
	})
	if !readPost(w, r, form) {
		return
	}
	if form.OriginAssetID == nil || form.DestAssetID == nil {
		s.writeAPIError(w, errors.New("missing origin or destination asset ID"))
		return
	}
	preview, err := s.core.IntentsPreview(r.Context(), *form.OriginAssetID, *form.DestAssetID, form.Value, form.SlippageBps, form.ExactOutput)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK      bool                 `json:"ok"`
		Preview *core.IntentsPreview `json:"preview"`
	}{
		OK:      true,
		Preview: preview,
	})
}

// apiIntentsExecute is the handler for the '/intents/execute' API request.
// It obtains a committed quote and funds it from the origin wallet. A
// committed quote creates a real deposit address and is screened by the
// service. Each direction echoes its displayed preview bound, and a
// committed quote falling outside it is rejected before any funds move:
// minAtomsOut (the preview's minimum output) for exact-input, maxAtomsIn
// (the preview's computed input) for exact-output.
func (s *AppServer) apiIntentsExecute(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		OriginAssetID *uint32          `json:"originAssetID"`
		DestAssetID   *uint32          `json:"destAssetID"`
		Value         uint64           `json:"value"`
		SlippageBps   int              `json:"slippageBps"`
		ExactOutput   bool             `json:"exactOutput"`
		MinAtomsOut   uint64           `json:"minAtomsOut"`
		MaxAtomsIn    uint64           `json:"maxAtomsIn"`
		Pass          encode.PassBytes `json:"pass"`
	})
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	if form.OriginAssetID == nil || form.DestAssetID == nil {
		s.writeAPIError(w, errors.New("missing origin or destination asset ID"))
		return
	}
	if form.ExactOutput {
		if form.MaxAtomsIn == 0 {
			s.writeAPIError(w, errors.New("missing previewed maximum input"))
			return
		}
	} else if form.MinAtomsOut == 0 {
		s.writeAPIError(w, errors.New("missing previewed minimum output"))
		return
	}
	requirePW, err := s.core.SendRequiresPassword()
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error reading send-requires-password setting: %w", err))
		return
	}
	if requirePW && len(form.Pass) == 0 {
		s.writeAPIError(w, fmt.Errorf("password required to execute a swap"))
		return
	}
	pass, err := s.resolvePass(form.Pass, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)

	swap, err := s.core.IntentsExecute(pass, *form.OriginAssetID, *form.DestAssetID, form.Value, form.SlippageBps, form.ExactOutput, form.MinAtomsOut, form.MaxAtomsIn)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("swap error: %w", err))
		return
	}
	writeJSON(w, struct {
		OK   bool              `json:"ok"`
		Swap *core.IntentsSwap `json:"swap"`
	}{
		OK:   true,
		Swap: swap,
	})
}

// apiIntentsSwaps is the handler for the '/intents/swaps' API request. It
// returns all persisted swap records, newest first.
func (s *AppServer) apiIntentsSwaps(w http.ResponseWriter, r *http.Request) {
	swaps, err := s.core.IntentsSwaps()
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK    bool                `json:"ok"`
		Swaps []*core.IntentsSwap `json:"swaps"`
	}{
		OK:    true,
		Swaps: swaps,
	})
}

// apiIntentsRefresh is the handler for the '/intents/refresh' API request.
// It polls the service for one swap immediately.
func (s *AppServer) apiIntentsRefresh(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		DepositAddress string `json:"depositAddress"`
	})
	if !readPost(w, r, form) {
		return
	}
	if form.DepositAddress == "" {
		s.writeAPIError(w, errors.New("missing deposit address"))
		return
	}
	swap, err := s.core.IntentsRefresh(r.Context(), form.DepositAddress)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK   bool              `json:"ok"`
		Swap *core.IntentsSwap `json:"swap"`
	}{
		OK:   true,
		Swap: swap,
	})
}

// apiIntentsReconstruct is the handler for the '/intents/reconstruct' API
// request. It rebuilds swap records lost with local state by probing the
// service with recipients of recent outgoing transactions.
func (s *AppServer) apiIntentsReconstruct(w http.ResponseWriter, r *http.Request) {
	found, err := s.core.IntentsReconstruct(r.Context())
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK    bool `json:"ok"`
		Found int  `json:"found"`
	}{
		OK:    true,
		Found: found,
	})
}
