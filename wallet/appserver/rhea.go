// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// HTTP handlers for RHEA lending. The engine lives in wallet/core/rhea.go;
// these handlers only marshal responses.

package appserver

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/core"
)

// apiRheaMarkets is the handler for the '/rhea/markets' API request. It
// returns the allowlisted lending markets read at one final block.
func (s *AppServer) apiRheaMarkets(w http.ResponseWriter, r *http.Request) {
	markets, err := s.core.RheaMarkets(r.Context())
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK      bool                  `json:"ok"`
		Markets *core.RheaMarketsView `json:"markets"`
	}{
		OK:      true,
		Markets: markets,
	})
}

// apiRheaAccount is the handler for the '/rhea/account' API request. It
// returns the logged-in identities' lending account, positions, and
// admission gate, read at one final block.
func (s *AppServer) apiRheaAccount(w http.ResponseWriter, r *http.Request) {
	account, err := s.core.RheaAccount(r.Context())
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK      bool                  `json:"ok"`
		Account *core.RheaAccountView `json:"account"`
	}{
		OK:      true,
		Account: account,
	})
}

// apiRheaOps is the handler for the '/rhea/ops' API request. It returns
// every lending operation, newest first.
func (s *AppServer) apiRheaOps(w http.ResponseWriter, _ *http.Request) {
	ops, err := s.core.RheaOps()
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK  bool           `json:"ok"`
		Ops []*core.RheaOp `json:"ops"`
	}{
		OK:  true,
		Ops: ops,
	})
}

// apiRheaSupplyPreview is the handler for the '/rhea/supply/preview' API
// request. It returns a dry-quote preview of supplying value atoms of a
// wallet asset to a lending market.
func (s *AppServer) apiRheaSupplyPreview(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		SourceAssetID *uint32 `json:"sourceAssetID"`
		TokenID       string  `json:"tokenID"`
		Value         uint64  `json:"value"`
	})
	if !readPost(w, r, form) {
		return
	}
	if form.SourceAssetID == nil || form.TokenID == "" {
		s.writeAPIError(w, errors.New("missing source asset or market"))
		return
	}
	preview, err := s.core.RheaSupplyPreview(r.Context(), *form.SourceAssetID, form.TokenID, form.Value)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK      bool                    `json:"ok"`
		Preview *core.RheaSupplyPreview `json:"preview"`
	}{
		OK:      true,
		Preview: preview,
	})
}

// apiRheaSupplyExecute is the handler for the '/rhea/supply/execute' API
// request. It echoes the bounds of the preview the user accepted: the
// minimum delivery, whether the supply creates the account, and the
// creation fee.
func (s *AppServer) apiRheaSupplyExecute(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		SourceAssetID  *uint32          `json:"sourceAssetID"`
		TokenID        string           `json:"tokenID"`
		Value          uint64           `json:"value"`
		MinDelivered   string           `json:"minDelivered"`
		Creating       bool             `json:"creating"`
		MaxCreationFee string           `json:"maxCreationFee"`
		Pass           encode.PassBytes `json:"pass"`
	})
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	if form.SourceAssetID == nil || form.TokenID == "" {
		s.writeAPIError(w, errors.New("missing source asset or market"))
		return
	}
	if form.MinDelivered == "" {
		s.writeAPIError(w, errors.New("missing previewed minimum delivery"))
		return
	}
	if form.Creating && form.MaxCreationFee == "" {
		s.writeAPIError(w, errors.New("missing previewed creation fee"))
		return
	}
	requirePW, err := s.core.SendRequiresPassword()
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error reading send-requires-password setting: %w", err))
		return
	}
	if requirePW && len(form.Pass) == 0 {
		s.writeAPIError(w, fmt.Errorf("password required to supply"))
		return
	}
	pass, err := s.resolvePass(form.Pass, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)

	op, err := s.core.RheaSupplyExecute(pass, *form.SourceAssetID, form.TokenID, form.Value, form.MinDelivered, form.Creating, form.MaxCreationFee)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("supply error: %w", err))
		return
	}
	writeJSON(w, struct {
		OK bool         `json:"ok"`
		Op *core.RheaOp `json:"op"`
	}{
		OK: true,
		Op: op,
	})
}

// apiRheaWithdrawPreview is the handler for the '/rhea/withdraw/preview' API
// request. It previews withdrawing amount (market-token base units, or
// empty for the most available) to a wallet asset.
func (s *AppServer) apiRheaWithdrawPreview(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		TokenID     string  `json:"tokenID"`
		DestAssetID *uint32 `json:"destAssetID"`
		Amount      string  `json:"amount"`
	})
	if !readPost(w, r, form) {
		return
	}
	if form.DestAssetID == nil || form.TokenID == "" {
		s.writeAPIError(w, errors.New("missing market or destination asset"))
		return
	}
	preview, err := s.core.RheaWithdrawPreview(r.Context(), form.TokenID, *form.DestAssetID, form.Amount)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK      bool                      `json:"ok"`
		Preview *core.RheaWithdrawPreview `json:"preview"`
	}{
		OK:      true,
		Preview: preview,
	})
}

// apiRheaWithdrawExecute is the handler for the '/rhea/withdraw/execute' API
// request. It echoes the bounds of the preview the user accepted. Signing
// for the lending account always needs the app password, which may come
// from the request or the cached session password.
func (s *AppServer) apiRheaWithdrawExecute(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		TokenID     string           `json:"tokenID"`
		DestAssetID *uint32          `json:"destAssetID"`
		Amount      string           `json:"amount"`
		MinAtomsOut uint64           `json:"minAtomsOut"`
		MaxFee      string           `json:"maxFee"`
		Pass        encode.PassBytes `json:"pass"`
	})
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	if form.DestAssetID == nil || form.TokenID == "" {
		s.writeAPIError(w, errors.New("missing market or destination asset"))
		return
	}
	if form.MinAtomsOut == 0 || form.MaxFee == "" {
		s.writeAPIError(w, errors.New("missing previewed minimum output or fee"))
		return
	}
	requirePW, err := s.core.SendRequiresPassword()
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error reading send-requires-password setting: %w", err))
		return
	}
	if requirePW && len(form.Pass) == 0 {
		s.writeAPIError(w, fmt.Errorf("password required to withdraw"))
		return
	}
	pass, err := s.resolvePass(form.Pass, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)

	op, err := s.core.RheaWithdrawExecute(pass, form.TokenID, *form.DestAssetID, form.Amount, form.MinAtomsOut, form.MaxFee)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("withdraw error: %w", err))
		return
	}
	writeJSON(w, struct {
		OK bool         `json:"ok"`
		Op *core.RheaOp `json:"op"`
	}{
		OK: true,
		Op: op,
	})
}

// apiRheaSweepPreview is the handler for the '/rhea/sweep/preview' API
// request. It previews moving amount (market-token base units, or empty for
// the whole plain balance) of a token in the lending account back into
// supply when destAssetID is absent, otherwise to that wallet asset.
func (s *AppServer) apiRheaSweepPreview(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		TokenID     string  `json:"tokenID"`
		DestAssetID *uint32 `json:"destAssetID"`
		Amount      string  `json:"amount"`
	})
	if !readPost(w, r, form) {
		return
	}
	if form.TokenID == "" {
		s.writeAPIError(w, errors.New("missing market"))
		return
	}
	preview, err := s.core.RheaSweepPreview(r.Context(), form.TokenID, form.DestAssetID, form.Amount)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK      bool                   `json:"ok"`
		Preview *core.RheaSweepPreview `json:"preview"`
	}{
		OK:      true,
		Preview: preview,
	})
}

// apiRheaSweepExecute is the handler for the '/rhea/sweep/execute' API
// request. It echoes the bounds of the preview the user accepted: the fee
// always, and the minimum output for a sweep to a wallet. Signing for the
// lending account always needs the app password, which may come from the
// request or the cached session password.
func (s *AppServer) apiRheaSweepExecute(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		TokenID     string           `json:"tokenID"`
		DestAssetID *uint32          `json:"destAssetID"`
		Amount      string           `json:"amount"`
		MinAtomsOut uint64           `json:"minAtomsOut"`
		MaxFee      string           `json:"maxFee"`
		Pass        encode.PassBytes `json:"pass"`
	})
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	if form.TokenID == "" {
		s.writeAPIError(w, errors.New("missing market"))
		return
	}
	if form.MaxFee == "" || (form.DestAssetID != nil && form.MinAtomsOut == 0) {
		s.writeAPIError(w, errors.New("missing previewed minimum output or fee"))
		return
	}
	requirePW, err := s.core.SendRequiresPassword()
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error reading send-requires-password setting: %w", err))
		return
	}
	if requirePW && len(form.Pass) == 0 {
		s.writeAPIError(w, fmt.Errorf("password required to sweep"))
		return
	}
	pass, err := s.resolvePass(form.Pass, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)

	op, err := s.core.RheaSweepExecute(pass, form.TokenID, form.DestAssetID, form.Amount, form.MinAtomsOut, form.MaxFee)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("sweep error: %w", err))
		return
	}
	writeJSON(w, struct {
		OK bool         `json:"ok"`
		Op *core.RheaOp `json:"op"`
	}{
		OK: true,
		Op: op,
	})
}
