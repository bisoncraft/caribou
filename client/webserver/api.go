// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package webserver

import (
	"archive/zip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/db"
	"decred.org/dcrdex/dex/config"
	"decred.org/dcrdex/dex/encode"
	pi "decred.org/dcrdex/dex/politeia"
)

var zero = encode.ClearBytes

// apiValidateAddress is the handlers for the '/validateaddress' API request.
func (s *WebServer) apiValidateAddress(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		Addr    string  `json:"addr"`
		AssetID *uint32 `json:"assetID"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	if form.AssetID == nil {
		s.writeAPIError(w, errors.New("missing asset ID"))
		return
	}
	valid, err := s.core.ValidateAddress(form.Addr, *form.AssetID)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	resp := struct {
		OK bool `json:"ok"`
	}{
		OK: valid,
	}
	writeJSON(w, resp)
}

// apiEstimateSendTxFee is the handler for the '/txfee' API request.
func (s *WebServer) apiEstimateSendTxFee(w http.ResponseWriter, r *http.Request) {
	form := new(sendTxFeeForm)
	if !readPost(w, r, form) {
		return
	}
	if form.AssetID == nil {
		s.writeAPIError(w, errors.New("missing asset ID"))
		return
	}
	txFee, validAddress, err := s.core.EstimateSendTxFee(form.Addr, *form.AssetID, form.Value, form.Subtract, form.MaxWithdraw)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	resp := struct {
		OK           bool   `json:"ok"`
		TxFee        uint64 `json:"txfee"`
		ValidAddress bool   `json:"validaddress"`
	}{
		OK:           true,
		TxFee:        txFee,
		ValidAddress: validAddress,
	}
	writeJSON(w, resp)
}

// apiGetWalletPeers is the handler for the '/getwalletpeers' API request.
func (s *WebServer) apiGetWalletPeers(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AssetID uint32 `json:"assetID"`
	}
	if !readPost(w, r, &form) {
		return
	}
	peers, err := s.core.WalletPeers(form.AssetID)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	resp := struct {
		OK    bool                `json:"ok"`
		Peers []*asset.WalletPeer `json:"peers"`
	}{
		OK:    true,
		Peers: peers,
	}
	writeJSON(w, resp)
}

// apiAddWalletPeer is the handler for the '/addwalletpeer' API request.
func (s *WebServer) apiAddWalletPeer(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AssetID uint32 `json:"assetID"`
		Address string `json:"addr"`
	}
	if !readPost(w, r, &form) {
		return
	}
	err := s.core.AddWalletPeer(form.AssetID, form.Address)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, simpleAck())
}

// apiRemoveWalletPeer is the handler for the '/removewalletpeer' API request.
func (s *WebServer) apiRemoveWalletPeer(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AssetID uint32 `json:"assetID"`
		Address string `json:"addr"`
	}
	if !readPost(w, r, &form) {
		return
	}
	err := s.core.RemoveWalletPeer(form.AssetID, form.Address)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, simpleAck())
}

func (s *WebServer) apiApproveTokenFee(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AssetID  uint32 `json:"assetID"`
		Version  uint32 `json:"version"`
		Approval bool   `json:"approval"`
	}
	if !readPost(w, r, &form) {
		return
	}

	txFee, err := s.core.ApproveTokenFee(form.AssetID, form.Version, form.Approval)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}

	resp := struct {
		OK    bool   `json:"ok"`
		TxFee uint64 `json:"txFee"`
	}{
		OK:    true,
		TxFee: txFee,
	}
	writeJSON(w, resp)
}

func (s *WebServer) apiApproveToken(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AssetID  uint32           `json:"assetID"`
		Password encode.PassBytes `json:"pass"`
	}
	if !readPost(w, r, &form) {
		return
	}
	pass, err := s.resolvePass(form.Password, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)

	txID, err := s.core.ApproveToken(pass, form.AssetID, func() {})
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	resp := struct {
		OK   bool   `json:"ok"`
		TxID string `json:"txID"`
	}{
		OK:   true,
		TxID: txID,
	}
	writeJSON(w, resp)
}

func (s *WebServer) apiUnapproveToken(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AssetID  uint32           `json:"assetID"`
		Version  uint32           `json:"version"`
		Password encode.PassBytes `json:"pass"`
	}
	if !readPost(w, r, &form) {
		return
	}
	pass, err := s.resolvePass(form.Password, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)

	txID, err := s.core.UnapproveToken(pass, form.AssetID, form.Version)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	resp := struct {
		OK   bool   `json:"ok"`
		TxID string `json:"txID"`
	}{
		OK:   true,
		TxID: txID,
	}
	writeJSON(w, resp)
}

// apiNewWallet is the handler for the '/newwallet' API request.
func (s *WebServer) apiNewWallet(w http.ResponseWriter, r *http.Request) {
	form := new(newWalletForm)
	defer form.AppPW.Clear()
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	has := s.core.WalletState(form.AssetID) != nil
	if has {
		s.writeAPIError(w, fmt.Errorf("already have a wallet for %s", unbip(form.AssetID)))
		return
	}
	pass, err := s.resolvePass(form.AppPW, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)
	// Wallet does not exist yet. Try to create it.
	err = s.core.CreateWallet(pass, form.Pass, &core.WalletForm{
		AssetID: form.AssetID,
		Type:    form.WalletType,
		Config:  form.Config,
	})
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error creating %s wallet: %w", unbip(form.AssetID), err))
		return
	}

	writeJSON(w, simpleAck())
}

// apiRecoverWallet is the handler for the '/recoverwallet' API request. Commands
// a recovery of the specified wallet.
func (s *WebServer) apiRecoverWallet(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AppPW   encode.PassBytes `json:"appPW"`
		AssetID uint32           `json:"assetID"`
		Force   bool             `json:"force"`
	}
	if !readPost(w, r, &form) {
		return
	}
	appPW, err := s.resolvePass(form.AppPW, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	status := s.core.WalletState(form.AssetID)
	if status == nil {
		s.writeAPIError(w, fmt.Errorf("no wallet for %d -> %s", form.AssetID, unbip(form.AssetID)))
		return
	}
	err = s.core.RecoverWallet(form.AssetID, appPW, form.Force)
	if err != nil {
		// NOTE: client may check for code activeOrdersErr to prompt for
		// an override of the safety check.
		s.writeAPIError(w, fmt.Errorf("error recovering %s wallet: %w", unbip(form.AssetID), err))
		return
	}

	writeJSON(w, simpleAck())
}

// apiRescanWallet is the handler for the '/rescanwallet' API request. Commands
// a rescan of the specified wallet.
func (s *WebServer) apiRescanWallet(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AssetID uint32 `json:"assetID"`
		Force   bool   `json:"force"`
	}
	if !readPost(w, r, &form) {
		return
	}
	status := s.core.WalletState(form.AssetID)
	if status == nil {
		s.writeAPIError(w, fmt.Errorf("No wallet for %d -> %s", form.AssetID, unbip(form.AssetID)))
		return
	}
	err := s.core.RescanWallet(form.AssetID, form.Force)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error rescanning %s wallet: %w", unbip(form.AssetID), err))
		return
	}

	writeJSON(w, simpleAck())
}

// apiOpenWallet is the handler for the '/openwallet' API request. Unlocks the
// specified wallet.
func (s *WebServer) apiOpenWallet(w http.ResponseWriter, r *http.Request) {
	form := new(openWalletForm)
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	status := s.core.WalletState(form.AssetID)
	if status == nil {
		s.writeAPIError(w, fmt.Errorf("No wallet for %d -> %s", form.AssetID, unbip(form.AssetID)))
		return
	}
	pass, err := s.resolvePass(form.Pass, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)
	err = s.core.OpenWallet(form.AssetID, pass)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error unlocking %s wallet: %w", unbip(form.AssetID), err))
		return
	}

	writeJSON(w, simpleAck())
}

// apiNewDepositAddress gets a new deposit address from a wallet.
func (s *WebServer) apiNewDepositAddress(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID *uint32 `json:"assetID"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	if form.AssetID == nil {
		s.writeAPIError(w, errors.New("missing asset ID"))
		return
	}
	assetID := *form.AssetID

	addr, err := s.core.NewDepositAddress(assetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error connecting to %s wallet: %w", unbip(assetID), err))
		return
	}

	writeJSON(w, &struct {
		OK      bool   `json:"ok"`
		Address string `json:"address"`
	}{
		OK:      true,
		Address: addr,
	})
}

// apiAddressUsed checks whether an address has been used.
func (s *WebServer) apiAddressUsed(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID *uint32 `json:"assetID"`
		Addr    string  `json:"addr"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	if form.AssetID == nil {
		s.writeAPIError(w, errors.New("missing asset ID"))
		return
	}
	assetID := *form.AssetID

	used, err := s.core.AddressUsed(assetID, form.Addr)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}

	writeJSON(w, &struct {
		OK   bool `json:"ok"`
		Used bool `json:"used"`
	}{
		OK:   true,
		Used: used,
	})
}

// apiConnectWallet is the handler for the '/connectwallet' API request.
// Connects to a specified wallet, but does not unlock it.
func (s *WebServer) apiConnectWallet(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID uint32 `json:"assetID"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	err := s.core.ConnectWallet(form.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error connecting to %s wallet: %w", unbip(form.AssetID), err))
		return
	}

	writeJSON(w, simpleAck())
}

// apiExportSeed is the handler for the '/exportseed' API request.
func (s *WebServer) apiExportSeed(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		Pass encode.PassBytes `json:"pass"`
	}{}
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	r.Close = true
	seed, err := s.core.ExportSeed(form.Pass)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error exporting seed: %w", err))
		return
	}
	writeJSON(w, &struct {
		OK   bool   `json:"ok"`
		Seed string `json:"seed"`
	}{
		OK:   true,
		Seed: seed,
	})
}

// apiRestoreWalletInfo is the handler for the '/restorewalletinfo' API
// request.
func (s *WebServer) apiRestoreWalletInfo(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID uint32
		Pass    encode.PassBytes
	}{}
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}

	info, err := s.core.WalletRestorationInfo(form.Pass, form.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error updating cert: %w", err))
		return
	}

	resp := struct {
		OK              bool                       `json:"ok"`
		RestorationInfo []*asset.WalletRestoration `json:"restorationinfo,omitempty"`
	}{
		OK:              true,
		RestorationInfo: info,
	}
	writeJSON(w, resp)
}

// apiCloseWallet is the handler for the '/closewallet' API request.
func (s *WebServer) apiCloseWallet(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID uint32 `json:"assetID"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	err := s.core.CloseWallet(form.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error locking %s wallet: %w", unbip(form.AssetID), err))
		return
	}

	writeJSON(w, simpleAck())
}

// apiInit is the handler for the '/init' API request.
func (s *WebServer) apiInit(w http.ResponseWriter, r *http.Request) {
	var init struct {
		Pass encode.PassBytes `json:"pass"`
		Seed string           `json:"seed,omitempty"`
	}
	defer init.Pass.Clear()
	if !readPost(w, r, &init) {
		return
	}
	var seed *string
	if len(init.Seed) > 0 {
		seed = &init.Seed
	}
	mnemonicSeed, err := s.core.InitializeClient(init.Pass, seed)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("initialization error: %w", err))
		return
	}
	err = s.actuallyLogin(w, r, &loginForm{Pass: init.Pass})
	if err != nil {
		s.writeAPIError(w, err)
		return
	}

	writeJSON(w, struct {
		OK           bool   `json:"ok"`
		MnemonicSeed string `json:"mnemonic"`
	}{
		OK:           true,
		MnemonicSeed: mnemonicSeed,
	})
}

// apiIsInitialized is the handler for the '/isinitialized' request.
func (s *WebServer) apiIsInitialized(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, &struct {
		OK          bool `json:"ok"`
		Initialized bool `json:"initialized"`
	}{
		OK:          true,
		Initialized: s.core.IsInitialized(),
	})
}

func (s *WebServer) apiLocale(w http.ResponseWriter, r *http.Request) {
	var lang string
	if !readPost(w, r, &lang) {
		return
	}
	m, found := localesMap[lang]
	if !found {
		s.writeAPIError(w, fmt.Errorf("no locale for language %q", lang))
		return
	}
	resp := make(map[string]string)
	for translationID, defaultTranslation := range enUS {
		t, found := m[translationID]
		if !found {
			t = defaultTranslation
		}
		resp[translationID] = t.T
	}

	writeJSON(w, resp)
}

func (s *WebServer) apiSetLocale(w http.ResponseWriter, r *http.Request) {
	var lang string
	if !readPost(w, r, &lang) {
		return
	}
	if err := s.core.SetLanguage(lang); err != nil {
		s.writeAPIError(w, err)
		return
	}

	// Get actual language after SetLanguage (in case of fallback)
	actualLang := s.core.Language()
	s.lang.Store(actualLang)
	if err := s.buildTemplates(actualLang); err != nil {
		s.writeAPIError(w, err)
		return
	}

	writeJSON(w, simpleAck())
}

// apiBuildInfo is the handler for the '/buildinfo' API request.
func (s *WebServer) apiBuildInfo(w http.ResponseWriter, r *http.Request) {
	resp := buildInfoResponse{
		OK:       true,
		Version:  s.appVersion,
		Revision: commitHash,
	}
	writeJSON(w, resp)
}

// apiLogin handles the 'login' API request.
func (s *WebServer) apiLogin(w http.ResponseWriter, r *http.Request) {
	login := new(loginForm)
	defer login.Pass.Clear()
	if !readPost(w, r, login) {
		return
	}

	err := s.actuallyLogin(w, r, login)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}

	notes, pokes, err := s.core.Notifications(100)
	if err != nil {
		log.Errorf("failed to get notifications: %v", err)
	}

	writeJSON(w, &struct {
		OK    bool               `json:"ok"`
		Notes []*db.Notification `json:"notes"`
		Pokes []*db.Notification `json:"pokes"`
	}{
		OK:    true,
		Notes: notes,
		Pokes: pokes,
	})
}

func (s *WebServer) apiNotes(w http.ResponseWriter, r *http.Request) {
	notes, pokes, err := s.core.Notifications(100)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("failed to get notifications: %w", err))
		return
	}

	writeJSON(w, &struct {
		OK    bool               `json:"ok"`
		Notes []*db.Notification `json:"notes"`
		Pokes []*db.Notification `json:"pokes"`
	}{
		OK:    true,
		Notes: notes,
		Pokes: pokes,
	})
}

// apiLogout handles the 'logout' API request.
func (s *WebServer) apiLogout(w http.ResponseWriter, r *http.Request) {
	err := s.core.Logout()
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("logout error: %w", err))
		return
	}

	// With Core locked up, invalidate all known auth tokens and cached passwords
	// to force any other sessions to login again.
	s.deauth()

	clearCookie(authCK, w)
	clearCookie(pwKeyCK, w)

	response := struct {
		OK bool `json:"ok"`
	}{
		OK: true,
	}
	writeJSON(w, response)
}

// apiUnpairCompanionApp removes the companion app pairing.
func (s *WebServer) apiUnpairCompanionApp(w http.ResponseWriter, r *http.Request) {
	if err := s.core.SetCompanionToken(""); err != nil {
		s.writeAPIError(w, fmt.Errorf("error clearing companion token: %w", err))
		return
	}
	s.authMtx.Lock()
	if s.companionToken != "" {
		delete(s.authTokens, s.companionToken)
		s.companionToken = ""
		s.companionTokenHash = ""
		s.companionTokenClaimed = false
		s.companionTokenExpiry = time.Time{}
		if s.companionExpiryTimer != nil {
			s.companionExpiryTimer.Stop()
			s.companionExpiryTimer = nil
		}
	}
	s.authMtx.Unlock()
	writeJSON(w, simpleAck())
}

// apiGetBalance handles the 'balance' API request.
func (s *WebServer) apiGetBalance(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID uint32 `json:"assetID"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	bal, err := s.core.AssetBalance(form.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("balance error: %w", err))
		return
	}
	resp := &struct {
		OK      bool                `json:"ok"`
		Balance *core.WalletBalance `json:"balance"`
	}{
		OK:      true,
		Balance: bal,
	}
	writeJSON(w, resp)

}

// apiParseConfig parses an INI config file into a map[string]string.
func (s *WebServer) apiParseConfig(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		ConfigText string `json:"configtext"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	configMap, err := config.Parse([]byte(form.ConfigText))
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("parse error: %w", err))
		return
	}
	resp := &struct {
		OK  bool              `json:"ok"`
		Map map[string]string `json:"map"`
	}{
		OK:  true,
		Map: configMap,
	}
	writeJSON(w, resp)
}

// apiWalletSettings fetches the currently stored wallet configuration settings.
func (s *WebServer) apiWalletSettings(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID uint32 `json:"assetID"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	settings, err := s.core.WalletSettings(form.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error setting wallet settings: %w", err))
		return
	}
	writeJSON(w, &struct {
		OK  bool              `json:"ok"`
		Map map[string]string `json:"map"`
	}{
		OK:  true,
		Map: settings,
	})
}

// apiToggleWalletStatus updates the wallet's status.
func (s *WebServer) apiToggleWalletStatus(w http.ResponseWriter, r *http.Request) {
	form := new(walletStatusForm)
	if !readPost(w, r, form) {
		return
	}
	err := s.core.ToggleWalletStatus(form.AssetID, form.Disable)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error setting wallet settings: %w", err))
		return
	}
	response := struct {
		OK bool `json:"ok"`
	}{
		OK: true,
	}
	writeJSON(w, response)
}

// apiDefaultWalletCfg attempts to load configuration settings from the
// asset's default path on the server.
func (s *WebServer) apiDefaultWalletCfg(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID uint32 `json:"assetID"`
		Type    string `json:"type"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	cfg, err := s.core.AutoWalletConfig(form.AssetID, form.Type)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error getting wallet config: %w", err))
		return
	}
	writeJSON(w, struct {
		OK     bool              `json:"ok"`
		Config map[string]string `json:"config"`
	}{
		OK:     true,
		Config: cfg,
	})
}

// apiChangeAppPass updates the application password.
func (s *WebServer) apiChangeAppPass(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AppPW    encode.PassBytes `json:"appPW"`
		NewAppPW encode.PassBytes `json:"newAppPW"`
	}{}
	defer form.AppPW.Clear()
	defer form.NewAppPW.Clear()
	if !readPost(w, r, form) {
		return
	}

	// Update application password.
	err := s.core.ChangeAppPass(form.AppPW, form.NewAppPW)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("change app pass error: %w", err))
		return
	}

	passwordIsCached := s.isPasswordCached(r)
	// Since the user changed the password, we clear all of the auth tokens
	// and cached passwords. However, we assign a new auth token and cache
	// the new password (if it was previously cached) for this session.
	s.deauth()
	authToken := s.authorize()
	setCookie(authCK, authToken, w)
	if passwordIsCached {
		key, err := s.cacheAppPassword(form.NewAppPW, authToken)
		if err != nil {
			log.Errorf("unable to cache password: %w", err)
			clearCookie(pwKeyCK, w)
		} else {
			setCookie(pwKeyCK, hex.EncodeToString(key), w)
			zero(key)
		}
	}

	writeJSON(w, simpleAck())
}

// apiResetAppPassword resets the application password.
func (s *WebServer) apiResetAppPassword(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		NewPass encode.PassBytes `json:"newPass"`
		Seed    string           `json:"seed"`
	})
	defer form.NewPass.Clear()
	if !readPost(w, r, form) {
		return
	}

	err := s.core.ResetAppPass(form.NewPass, form.Seed)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}

	writeJSON(w, simpleAck())
}

// apiReconfig sets new configuration details for the wallet.
func (s *WebServer) apiReconfig(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID    uint32            `json:"assetID"`
		WalletType string            `json:"walletType"`
		Config     map[string]string `json:"config"`
		// newWalletPW json field should be omitted in case caller isn't interested
		// in setting new password, passing null JSON value will cause an unmarshal
		// error.
		NewWalletPW encode.PassBytes `json:"newWalletPW"`
		AppPW       encode.PassBytes `json:"appPW"`
	}{}
	defer form.NewWalletPW.Clear()
	defer form.AppPW.Clear()
	if !readPost(w, r, form) {
		return
	}
	pass, err := s.resolvePass(form.AppPW, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)
	// Update wallet settings.
	err = s.core.ReconfigureWallet(pass, form.NewWalletPW, &core.WalletForm{
		AssetID: form.AssetID,
		Config:  form.Config,
		Type:    form.WalletType,
	})
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("reconfig error: %w", err))
		return
	}

	writeJSON(w, simpleAck())
}

// apiSend handles the 'send' API request.
func (s *WebServer) apiSend(w http.ResponseWriter, r *http.Request) {
	form := new(sendForm)
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	state := s.core.WalletState(form.AssetID)
	if state == nil {
		s.writeAPIError(w, fmt.Errorf("no wallet found for %s", unbip(form.AssetID)))
		return
	}
	if len(form.Pass) == 0 {
		s.writeAPIError(w, fmt.Errorf("empty password"))
		return
	}
	coin, err := s.core.Send(form.Pass, form.AssetID, form.Value, form.Address, form.Subtract)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("send/withdraw error: %w", err))
		return
	}
	resp := struct {
		OK   bool   `json:"ok"`
		Coin string `json:"coin"`
	}{
		OK:   true,
		Coin: coin.String(),
	}
	writeJSON(w, resp)
}

// apiActuallyLogin logs the user in. login form private data is expected to be
// cleared by the caller.
func (s *WebServer) actuallyLogin(w http.ResponseWriter, r *http.Request, login *loginForm) error {
	// Only allow login from the onion address when a companion app is
	// actively paired. This prevents an unpaired companion app from
	// re-authenticating with saved credentials.
	if s.isOnionRequest(r) {
		s.authMtx.RLock()
		paired := s.companionToken != "" && s.companionTokenClaimed
		s.authMtx.RUnlock()
		if !paired {
			return errors.New("companion app is not paired")
		}
	}
	pass, err := s.resolvePass(login.Pass, r)
	defer zero(pass)
	if err != nil {
		return fmt.Errorf("password error: %w", err)
	}
	err = s.core.Login(pass)
	if err != nil {
		return fmt.Errorf("login error: %w", err)
	}

	if !s.isAuthed(r) {
		authToken := s.authorize()
		setCookie(authCK, authToken, w)
		key, err := s.cacheAppPassword(pass, authToken)
		if err != nil {
			return fmt.Errorf("login error: %w", err)

		}
		setCookie(pwKeyCK, hex.EncodeToString(key), w)
		zero(key)
	}

	return nil
}

// apiUser handles the 'user' API request.
func (s *WebServer) apiUser(w http.ResponseWriter, r *http.Request) {
	var u *core.User
	if s.isAuthed(r) {
		u = s.core.User()
	}

	s.authMtx.RLock()
	paired := s.companionToken != "" && s.companionTokenClaimed
	s.authMtx.RUnlock()

	response := struct {
		User               *core.User `json:"user"`
		Lang               string     `json:"lang"`
		Langs              []string   `json:"langs"`
		Inited             bool       `json:"inited"`
		OK                 bool       `json:"ok"`
		OnionUrl           string     `json:"onionUrl"`
		CompanionAppPaired bool       `json:"companionAppPaired"`
	}{
		User:               u,
		Lang:               s.lang.Load().(string),
		Langs:              s.langs,
		Inited:             s.core.IsInitialized(),
		OK:                 true,
		OnionUrl:           s.onion,
		CompanionAppPaired: paired,
	}
	writeJSON(w, response)
}

// apiToggleRateSource handles the /toggleratesource API request.
func (s *WebServer) apiToggleRateSource(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		Disable bool   `json:"disable"`
		Source  string `json:"source"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	err := s.core.ToggleRateSourceStatus(form.Source, form.Disable)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error disabling/enabling rate source: %w", err))
		return
	}
	writeJSON(w, simpleAck())
}

// apiDeleteArchiveRecords handles the '/deletearchivedrecords' API request.
func (s *WebServer) apiStakeStatus(w http.ResponseWriter, r *http.Request) {
	var assetID uint32
	if !readPost(w, r, &assetID) {
		return
	}
	status, err := s.core.StakeStatus(assetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error fetching stake status for asset ID %d: %w", assetID, err))
		return
	}

	proposalsInProgress, err := s.core.ProposalsInProgress()
	if err != nil {
		log.Errorf("error fetching proposals in progress: %v", err)
	}

	writeJSON(w, &struct {
		OK            bool                       `json:"ok"`
		Status        *asset.TicketStakingStatus `json:"status"`
		ProposalsMeta *proposalsMeta             `json:"proposalsMeta"`
	}{
		OK:     true,
		Status: status,
		ProposalsMeta: &proposalsMeta{
			ProposalsInProgress: proposalsInProgress,
		},
	})
}

func (s *WebServer) apiSetVSP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID uint32 `json:"assetID"`
		URL     string `json:"url"`
	}
	if !readPost(w, r, &req) {
		return
	}
	if err := s.core.SetVSP(req.AssetID, req.URL); err != nil {
		s.writeAPIError(w, fmt.Errorf("error settings vsp to %q for asset ID %d: %w", req.URL, req.AssetID, err))
		return
	}
	writeJSON(w, simpleAck())
}

func (s *WebServer) apiPurchaseTickets(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID uint32           `json:"assetID"`
		N       int              `json:"n"`
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
	if err = s.core.PurchaseTickets(req.AssetID, appPW, req.N); err != nil {
		s.writeAPIError(w, fmt.Errorf("error purchasing tickets for asset ID %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, simpleAck())
}

func (s *WebServer) apiSetVotingPreferences(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID        uint32            `json:"assetID"`
		Choices        map[string]string `json:"choices"`
		TSpendPolicy   map[string]string `json:"tSpendPolicy"`
		TreasuryPolicy map[string]string `json:"treasuryPolicy"`
	}
	if !readPost(w, r, &req) {
		return
	}
	if err := s.core.SetVotingPreferences(req.AssetID, req.Choices, req.TSpendPolicy, req.TreasuryPolicy); err != nil {
		s.writeAPIError(w, fmt.Errorf("error setting voting preferences for asset ID %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, simpleAck())
}

func (s *WebServer) apiListVSPs(w http.ResponseWriter, r *http.Request) {
	var assetID uint32
	if !readPost(w, r, &assetID) {
		return
	}
	vsps, err := s.core.ListVSPs(assetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error listing VSPs for asset ID %d: %w", assetID, err))
		return
	}
	writeJSON(w, &struct {
		OK   bool                           `json:"ok"`
		VSPs []*asset.VotingServiceProvider `json:"vsps"`
	}{
		OK:   true,
		VSPs: vsps,
	})
}

func (s *WebServer) apiTicketPage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID   uint32 `json:"assetID"`
		ScanStart int32  `json:"scanStart"`
		N         int    `json:"n"`
		SkipN     int    `json:"skipN"`
	}
	if !readPost(w, r, &req) {
		return
	}
	tickets, err := s.core.TicketPage(req.AssetID, req.ScanStart, req.N, req.SkipN)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error retrieving ticket page for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, &struct {
		OK      bool            `json:"ok"`
		Tickets []*asset.Ticket `json:"tickets"`
	}{
		OK:      true,
		Tickets: tickets,
	})
}

func (s *WebServer) apiMixingStats(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID uint32 `json:"assetID"`
	}
	if !readPost(w, r, &req) {
		return
	}
	stats, err := s.core.FundsMixingStats(req.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error reteiving mixing stats for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, &struct {
		OK    bool                    `json:"ok"`
		Stats *asset.FundsMixingStats `json:"stats"`
	}{
		OK:    true,
		Stats: stats,
	})
}

func (s *WebServer) apiConfigureMixer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID uint32 `json:"assetID"`
		Enabled bool   `json:"enabled"`
	}
	if !readPost(w, r, &req) {
		return
	}
	pass, err := s.resolvePass(nil, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)
	if err := s.core.ConfigureFundsMixer(pass, req.AssetID, req.Enabled); err != nil {
		s.writeAPIError(w, fmt.Errorf("error configuring mixing for %d: %w", req.AssetID, err))
		return
	}
	writeJSON(w, simpleAck())
}

func (s *WebServer) apiTxHistory(w http.ResponseWriter, r *http.Request) {
	var form struct {
		asset.TxHistoryRequest
		AssetID uint32 `json:"assetID"`
	}
	if !readPost(w, r, &form) {
		return
	}

	resp, err := s.core.TxHistory(form.AssetID, &form.TxHistoryRequest)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error getting transaction history: %w", err))
		return
	}
	writeJSON(w, &struct {
		OK            bool                       `json:"ok"`
		Txs           []*asset.WalletTransaction `json:"txs"`
		MoreAvailable bool                       `json:"moreAvailable"`
	}{
		OK:            true,
		Txs:           resp.Txs,
		MoreAvailable: resp.MoreAvailable,
	})
}

func (s *WebServer) apiTakeAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID  uint32          `json:"assetID"`
		ActionID string          `json:"actionID"`
		Action   json.RawMessage `json:"action"`
	}
	if !readPost(w, r, &req) {
		return
	}
	if err := s.core.TakeAction(req.AssetID, req.ActionID, req.Action); err != nil {
		s.writeAPIError(w, fmt.Errorf("error taking action: %w", err))
		return
	}
	writeJSON(w, simpleAck())
}

// apiExportAppLogs time stamps the application log, zips it and sends it back to
// the browser or webview as an attachment. Logfile names need to be distinct as
// webview will not overwite an existing file.
func (s *WebServer) apiExportAppLogs(w http.ResponseWriter, r *http.Request) {
	timeString := time.Now().Format("2006-01-02T15_04_05")
	zipAttachment := fmt.Sprintf("attachment; filename=bwlog_%s.zip", timeString)

	w.Header().Set("Content-Disposition", zipAttachment)
	w.Header().Set("Content-Type", "application/octet-stream; type=zip")
	w.WriteHeader(http.StatusOK)

	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()

	lf, err := os.Open(s.mainLogFilePath)
	if err != nil {
		log.Errorf("error opening bisonw log file: %v", err)
		return
	}
	defer lf.Close()

	logFile := fmt.Sprintf("bwlog_%s.log", timeString)
	iow, err := zipWriter.Create(logFile) // only 1 file in zip header
	if err != nil {
		log.Errorf("error creating an io.Writer: %v", err)
		return
	}

	if _, err := io.Copy(iow, lf); err != nil {
		log.Errorf("error copying bisonw log to zip writer: %v", err)
		return
	}
}

func (s *WebServer) apiWalletLogFilePath(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AssetID uint32 `json:"assetID"`
	}
	if !readPost(w, r, &form) {
		return
	}
	logFilePath, err := s.core.WalletLogFilePath(form.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error getting wallet log file path: %w", err))
		return
	}
	writeJSON(w, &struct {
		OK   bool   `json:"ok"`
		Path string `json:"path"`
	}{
		OK:   true,
		Path: logFilePath,
	})
}

// writeAPIError logs the formatted error and sends a standardResponse with the
// error message.
func (s *WebServer) writeAPIError(w http.ResponseWriter, err error) {
	var cErr *core.Error
	var code *int
	if errors.As(err, &cErr) {
		code = cErr.Code()
	}

	innerErr := core.UnwrapErr(err)
	resp := &standardResponse{
		OK:   false,
		Msg:  innerErr.Error(),
		Code: code,
	}
	log.Error(err.Error())
	writeJSON(w, resp)
}

// setCookie sets the value of a cookie in the http response.
func setCookie(name, value string, w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Path:     "/",
		Value:    value,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearCookie removes a cookie in the http response.
func clearCookie(name string, w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Path:     "/",
		Value:    "",
		Expires:  time.Unix(0, 0),
		SameSite: http.SameSiteStrictMode,
	})
}

// resolvePass returns the appPW if it has a value, but if not, it attempts
// to retrieve the cached password using the information in cookies.
func (s *WebServer) resolvePass(appPW []byte, r *http.Request) ([]byte, error) {
	if len(appPW) > 0 {
		return appPW, nil
	}
	cachedPass, err := s.getCachedPasswordUsingRequest(r)
	if err != nil {
		if errors.Is(err, errNoCachedPW) {
			return nil, fmt.Errorf("app pass cannot be empty")
		}
		return nil, fmt.Errorf("error retrieving cached pw: %w", err)
	}
	return cachedPass, nil
}

func (s *WebServer) apiAllBridgePaths(w http.ResponseWriter, r *http.Request) {
	paths, err := s.core.AllBridgePaths()
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error fetching bridge paths: %w", err))
		return
	}

	writeJSON(w, &struct {
		OK    bool                           `json:"ok"`
		Paths map[uint32]map[uint32][]string `json:"paths"`
	}{
		OK:    true,
		Paths: paths,
	})
}

// apiBridgeFeesAndLimits is the handler for the '/bridgefeesandlimits' API request.
func (s *WebServer) apiBridgeFeesAndLimits(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		FromAssetID uint32 `json:"fromAssetID"`
		ToAssetID   uint32 `json:"toAssetID"`
		BridgeName  string `json:"bridgeName"`
	}{}
	if !readPost(w, r, form) {
		return
	}

	result, err := s.core.BridgeFeesAndLimits(form.FromAssetID, form.ToAssetID, form.BridgeName)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("unable to get bridge fees and limits: %w", err))
		return
	}

	writeJSON(w, &struct {
		OK     bool                      `json:"ok"`
		Result *core.BridgeFeesAndLimits `json:"result"`
	}{
		OK:     true,
		Result: result,
	})
}

// apiBridge is the handler for the '/bridge' API request.
func (s *WebServer) apiBridge(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		FromAssetID uint32 `json:"fromAssetID"`
		ToAssetID   uint32 `json:"toAssetID"`
		Amount      uint64 `json:"amount"`
		BridgeName  string `json:"bridgeName"`
	}{}
	if !readPost(w, r, form) {
		return
	}

	txID, err := s.core.Bridge(form.FromAssetID, form.ToAssetID, form.Amount, form.BridgeName)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("bridge error: %w", err))
		return
	}

	writeJSON(w, &struct {
		OK   bool   `json:"ok"`
		TxID string `json:"txID"`
	}{
		OK:   true,
		TxID: txID,
	})
}

// apiBridgeApprovalStatus is the handler for the '/bridgeapprovalstatus' API request.
func (s *WebServer) apiBridgeApprovalStatus(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID    uint32 `json:"assetID"`
		BridgeName string `json:"bridgeName"`
	}{}
	if !readPost(w, r, form) {
		return
	}

	status, err := s.core.BridgeContractApprovalStatus(form.AssetID, form.BridgeName)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error getting bridge approval status: %w", err))
		return
	}

	writeJSON(w, &struct {
		OK     bool                 `json:"ok"`
		Status asset.ApprovalStatus `json:"status"`
	}{
		OK:     true,
		Status: status,
	})
}

// apiApproveBridgeContract is the handler for the '/approvebridgecontract' API request.
func (s *WebServer) apiApproveBridgeContract(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID    uint32 `json:"assetID"`
		BridgeName string `json:"bridgeName"`
	}{}
	if !readPost(w, r, form) {
		return
	}

	txID, err := s.core.ApproveBridgeContract(form.AssetID, form.BridgeName)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error approving bridge contract: %w", err))
		return
	}

	writeJSON(w, &struct {
		OK   bool   `json:"ok"`
		TxID string `json:"txID"`
	}{
		OK:   true,
		TxID: txID,
	})
}

// apiUnapproveBridgeContract is the handler for the '/unapprovebridgecontract' API request.
func (s *WebServer) apiUnapproveBridgeContract(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID    uint32 `json:"assetID"`
		BridgeName string `json:"bridgeName"`
	}{}
	if !readPost(w, r, form) {
		return
	}

	txID, err := s.core.UnapproveBridgeContract(form.AssetID, form.BridgeName)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error unapproving bridge contract: %w", err))
		return
	}

	writeJSON(w, &struct {
		OK   bool   `json:"ok"`
		TxID string `json:"txID"`
	}{
		OK:   true,
		TxID: txID,
	})
}

// apiPendingBridges is the handler for the '/pendingbridges' API request.
func (s *WebServer) apiPendingBridges(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID uint32 `json:"assetID"`
	}{}
	if !readPost(w, r, form) {
		return
	}

	bridges, err := s.core.PendingBridges(form.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error getting pending bridges: %w", err))
		return
	}

	writeJSON(w, &struct {
		OK      bool                       `json:"ok"`
		Bridges []*asset.WalletTransaction `json:"bridges"`
	}{
		OK:      true,
		Bridges: bridges,
	})
}

// apiBridgeHistory is the handler for the '/bridgehistory' API request.
func (s *WebServer) apiBridgeHistory(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID uint32  `json:"assetID"`
		N       int     `json:"n"`
		RefID   *string `json:"refID"`
		Past    bool    `json:"past"`
	}{}
	if !readPost(w, r, form) {
		return
	}

	bridges, err := s.core.BridgeHistory(form.AssetID, form.N, form.RefID, form.Past)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error getting bridge history: %w", err))
		return
	}

	writeJSON(w, &struct {
		OK      bool                       `json:"ok"`
		Bridges []*asset.WalletTransaction `json:"bridges"`
	}{
		OK:      true,
		Bridges: bridges,
	})
}

// apiCastVote is the handler for the '/castvote' API request.
func (s *WebServer) apiCastVote(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		Token   string           `json:"token"`
		AssetID uint32           `json:"assetID"`
		AppPW   encode.PassBytes `json:"appPW"`
		Bit     string           `json:"bit"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	defer form.AppPW.Clear()
	appPW, err := s.resolvePass(form.AppPW, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}

	if form.Bit != pi.VoteBitYes && form.Bit != pi.VoteBitNo {
		s.writeAPIError(w, errors.New("invalid vote bit"))
		return
	}

	err = s.core.CastVote(form.AssetID, appPW, form.Token, form.Bit)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("castvote error: %w", err))
		return
	}

	writeJSON(w, simpleAck())
}
