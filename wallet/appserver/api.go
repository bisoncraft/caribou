// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

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

	"github.com/bisoncraft/caribou/util/config"
	"github.com/bisoncraft/caribou/util/encode"
	pi "github.com/bisoncraft/caribou/util/politeia/pitypes"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/core"
	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/mnemonic"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

var zero = encode.ClearBytes

// apiValidateAddress is the handlers for the '/validateaddress' API request.
func (s *AppServer) apiValidateAddress(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiEstimateSendTxFee(w http.ResponseWriter, r *http.Request) {
	form := new(sendTxFeeForm)
	if !readPost(w, r, form) {
		return
	}
	if form.AssetID == nil {
		s.writeAPIError(w, errors.New("missing asset ID"))
		return
	}
	est, err := s.core.EstimateSendTxFee(form.Addr, *form.AssetID, form.Value, form.Subtract, form.MaxWithdraw)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	resp := struct {
		OK           bool   `json:"ok"`
		TxFee        uint64 `json:"txfee"`
		FeeRate      uint64 `json:"feeRate"`
		ValidAddress bool   `json:"validaddress"`
	}{
		OK:           true,
		TxFee:        est.Fee,
		FeeRate:      est.FeeRate,
		ValidAddress: est.ValidAddress,
	}
	writeJSON(w, resp)
}

// apiGetWalletPeers is the handler for the '/getwalletpeers' API request.
func (s *AppServer) apiGetWalletPeers(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiAddWalletPeer(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiRemoveWalletPeer(w http.ResponseWriter, r *http.Request) {
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

// apiNewWallet is the handler for the '/newwallet' API request.
func (s *AppServer) apiNewWallet(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiRecoverWallet(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiRescanWallet(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AssetID uint32 `json:"assetID"`
		Force   bool   `json:"force"`
	}
	if !readPost(w, r, &form) {
		return
	}
	status := s.core.WalletState(form.AssetID)
	if status == nil {
		s.writeAPIError(w, fmt.Errorf("no wallet for %d -> %s", form.AssetID, unbip(form.AssetID)))
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
func (s *AppServer) apiOpenWallet(w http.ResponseWriter, r *http.Request) {
	form := new(openWalletForm)
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	status := s.core.WalletState(form.AssetID)
	if status == nil {
		s.writeAPIError(w, fmt.Errorf("no wallet for %d -> %s", form.AssetID, unbip(form.AssetID)))
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
func (s *AppServer) apiNewDepositAddress(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiAddressUsed(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiConnectWallet(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiExportSeed(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiRestoreWalletInfo(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiCloseWallet(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiInit(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiIsInitialized(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, &struct {
		OK          bool `json:"ok"`
		Initialized bool `json:"initialized"`
	}{
		OK:          true,
		Initialized: s.core.IsInitialized(),
	})
}

// apiValidateSeed is the handler for the '/validateseed' API request.
// It validates the submitted mnemonic using the mnemonic package's decoder
// (15-word wordlist check + embedded checksum) without initializing or
// mutating any wallet state. This endpoint is unauthenticated because it
// is used during the init wizard, before the client is initialized.
func (s *AppServer) apiValidateSeed(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		Seed string `json:"seed"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	if _, _, err := mnemonic.DecodeMnemonic(form.Seed); err != nil {
		s.writeAPIError(w, fmt.Errorf("invalid seed: %w", err))
		return
	}
	writeJSON(w, simpleAck())
}

func (s *AppServer) apiLocale(w http.ResponseWriter, r *http.Request) {
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
	for translationID, defaultTranslation := range localesMap["en-US"] {
		t, found := m[translationID]
		if !found {
			t = defaultTranslation
		}
		resp[translationID] = t.T
	}

	writeJSON(w, resp)
}

func (s *AppServer) apiSetLocale(w http.ResponseWriter, r *http.Request) {
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
	s.lang.Store(&actualLang)

	writeJSON(w, simpleAck())
}

// apiBuildInfo is the handler for the '/buildinfo' API request.
func (s *AppServer) apiBuildInfo(w http.ResponseWriter, r *http.Request) {
	resp := buildInfoResponse{
		OK:       true,
		Version:  s.appVersion,
		Revision: commitHash,
	}
	writeJSON(w, resp)
}

// errBadCredentials marks a login failure caused by rejected credentials (a
// wrong password). Environmental failures (e.g. the companion app is not
// paired, or caching the password failed after a valid login) are not tagged
// with it, so only genuine credential rejections feed the brute-force throttle.
var errBadCredentials = errors.New("bad credentials")

// badCredentials tags err as a credential rejection (detected via
// errors.Is(err, errBadCredentials)) while keeping a single-error unwrap chain,
// so writeAPIError still resolves the user-facing message to err's root cause.
type badCredentials struct{ err error }

func (e badCredentials) Error() string        { return e.err.Error() }
func (e badCredentials) Unwrap() error        { return e.err }
func (e badCredentials) Is(target error) bool { return target == errBadCredentials }

// apiLogin handles the 'login' API request.
func (s *AppServer) apiLogin(w http.ResponseWriter, r *http.Request) {
	if d := s.loginThrottle(); d > 0 {
		s.writeAPIError(w, fmt.Errorf("too many failed login attempts, try again in %s", d.Round(time.Second)))
		return
	}

	login := new(loginForm)
	defer login.Pass.Clear()
	if !readPost(w, r, login) {
		return
	}

	err := s.actuallyLogin(w, r, login)
	if err != nil {
		// Only a rejected password counts toward the throttle; environmental
		// errors must not lock out the (single) legitimate client.
		if errors.Is(err, errBadCredentials) {
			s.noteLoginFailure()
		}
		s.writeAPIError(w, err)
		return
	}
	s.noteLoginSuccess()

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

func (s *AppServer) apiNotes(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiLogout(w http.ResponseWriter, r *http.Request) {
	// Lock locks core and invalidates all known auth tokens and cached passwords,
	// forcing any other sessions to login again. It deauths even when Logout
	// errors, so the session these cookies name is gone either way: clear them
	// regardless, and before the response is written, so a retry starts from a
	// clean login instead of riding stale cookies into an auth failure.
	logoutErr := s.Lock()
	clearCookie(authCK, w)
	clearCookie(pwKeyCK, w)

	if logoutErr != nil {
		s.writeAPIError(w, fmt.Errorf("logout error: %w", logoutErr))
		return
	}

	response := struct {
		OK bool `json:"ok"`
	}{
		OK: true,
	}
	writeJSON(w, response)
}

// apiUnpairCompanionApp removes the companion app pairing.
func (s *AppServer) apiUnpairCompanionApp(w http.ResponseWriter, r *http.Request) {
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
	// Drop the notification sockets with the pairing. /ws is authorized at
	// upgrade and never re-checked, so unpairing would otherwise refuse every
	// later onion request while an already-open link kept streaming balances,
	// wallet state, deposit addresses and sends to the remote peer this call is
	// meant to cut off. See deauth.
	s.wsServer.DisconnectAll()
	writeJSON(w, simpleAck())
}

// apiGenerateAPIToken mints a local API token for same-machine automation
// clients. Only the hash is persisted; the raw token is returned once and
// cannot be recovered later. Any previously issued token is replaced.
func (s *AppServer) apiGenerateAPIToken(w http.ResponseWriter, r *http.Request) {
	// A long-lived local credential may only be minted from the same machine.
	if !s.isLocalRequest(r) {
		s.writeAPIError(w, errors.New("API tokens can only be generated from a local request"))
		return
	}
	token := hex.EncodeToString(encode.RandomBytes(32))
	tokenHash := hashToken(token)
	if err := s.core.SetAPIToken(tokenHash); err != nil {
		s.writeAPIError(w, fmt.Errorf("error storing API token: %w", err))
		return
	}
	s.authMtx.Lock()
	s.apiTokenHash = tokenHash
	s.authMtx.Unlock()

	writeJSON(w, &struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}{
		OK:    true,
		Token: token,
	})
}

// apiRevokeAPIToken clears the local API token, immediately invalidating any
// automation client using it.
func (s *AppServer) apiRevokeAPIToken(w http.ResponseWriter, r *http.Request) {
	// A local credential may only be revoked from the same machine, mirroring
	// the restriction on minting it.
	if !s.isLocalRequest(r) {
		s.writeAPIError(w, errors.New("API tokens can only be revoked from a local request"))
		return
	}
	if err := s.core.SetAPIToken(""); err != nil {
		s.writeAPIError(w, fmt.Errorf("error clearing API token: %w", err))
		return
	}
	s.authMtx.Lock()
	s.apiTokenHash = ""
	s.authMtx.Unlock()
	// Drop the notification sockets with the token, or "immediately" above is
	// false for the one credential-bearing link that is never re-checked: an
	// automation client can set the API token header on a /ws upgrade, and
	// isAuthed authorizes that upgrade on the token alone. See deauth.
	s.wsServer.DisconnectAll()
	writeJSON(w, simpleAck())
}

// apiGetBalance handles the 'balance' API request.
func (s *AppServer) apiGetBalance(w http.ResponseWriter, r *http.Request) {
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

// apiWalletFeeState returns an on-demand fee-rate estimate for a wallet. The
// estimate is computed when requested; it is not cached or pushed to the front
// end, so callers should request it only when a fee figure is needed.
func (s *AppServer) apiWalletFeeState(w http.ResponseWriter, r *http.Request) {
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
	feeState, err := s.core.WalletFeeState(*form.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("fee state error: %w", err))
		return
	}
	resp := &struct {
		OK       bool           `json:"ok"`
		FeeState *core.FeeState `json:"feeState"`
	}{
		OK:       true,
		FeeState: feeState,
	}
	writeJSON(w, resp)
}

// apiParseConfig parses an INI config file into a map[string]string.
func (s *AppServer) apiParseConfig(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiWalletSettings(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiToggleWalletStatus(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiDefaultWalletCfg(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiChangeAppPass(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AppPW    encode.PassBytes `json:"appPW"`
		NewAppPW encode.PassBytes `json:"newAppPW"`
	}{}
	defer form.AppPW.Clear()
	defer form.NewAppPW.Clear()
	if !readPost(w, r, form) {
		return
	}

	// Serialize against Lock/Unlock. Changing the password tears down every
	// session (deauth) and mints a fresh one; without this a concurrent biometric
	// Unlock could mint a grant against a token this deauth is about to drop,
	// leaving that unlock stranded on the login gate.
	s.sessionMtx.Lock()
	defer s.sessionMtx.Unlock()

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
	authToken, err := s.authorize()
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("authorize: %w", err))
		return
	}
	setCookie(authCK, authToken, w)
	if passwordIsCached {
		key, err := s.cacheAppPassword(form.NewAppPW, authToken)
		if err != nil {
			log.Errorf("unable to cache password: %v", err)
			clearCookie(pwKeyCK, w)
		} else {
			setCookie(pwKeyCK, hex.EncodeToString(key), w)
			zero(key)
		}
	}

	writeJSON(w, simpleAck())
}

// apiResetAppPassword resets the application password.
func (s *AppServer) apiResetAppPassword(w http.ResponseWriter, r *http.Request) {
	form := new(struct {
		NewPass encode.PassBytes `json:"newPass"`
		Seed    string           `json:"seed"`
	})
	defer form.NewPass.Clear()
	if !readPost(w, r, form) {
		return
	}

	// Serialize against Lock/Unlock, as apiChangeAppPass does. deauth drops
	// pending grants, and a concurrent biometric Unlock must not mint one
	// against a token this is about to remove.
	s.sessionMtx.Lock()
	defer s.sessionMtx.Unlock()

	err := s.core.ResetAppPass(form.NewPass, form.Seed)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}

	// Every auth token and cached password was minted under the old password,
	// so a session left live here would stay authenticated but fail every
	// password-bearing operation. Unlike apiChangeAppPass no fresh token is
	// issued: the reset flow ends at the login screen, so leaving the caller
	// logged out is the intended outcome. With no live session this is a no-op.
	s.deauth()

	writeJSON(w, simpleAck())
}

// apiReconfig sets new configuration details for the wallet.
func (s *AppServer) apiReconfig(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiSend(w http.ResponseWriter, r *http.Request) {
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
	// When the send-requires-password setting is enabled, the user must
	// supply their app password explicitly; otherwise fall back to the
	// session-cached password.
	requirePW, err := s.core.SendRequiresPassword()
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error reading send-requires-password setting: %w", err))
		return
	}
	if requirePW && len(form.Pass) == 0 {
		s.writeAPIError(w, fmt.Errorf("password required to send"))
		return
	}
	pass, err := s.resolvePass(form.Pass, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)
	coin, err := s.core.Send(pass, form.AssetID, form.Value, form.Address, form.Subtract, form.FeeRate, form.DestTag)
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

// apiEstablishTrustline handles the '/establishtrustline' API request,
// submitting the opt-in transaction that lets a token wallet's account
// receive its token. It commits native funds, so it honors the
// send-requires-password setting exactly as '/send' does. The response
// carries the transaction ID, empty when the line already existed.
func (s *AppServer) apiEstablishTrustline(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID uint32           `json:"assetID"`
		Pass    encode.PassBytes `json:"pass"`
	}{}
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	if s.core.WalletState(form.AssetID) == nil {
		s.writeAPIError(w, fmt.Errorf("no wallet found for %s", unbip(form.AssetID)))
		return
	}
	requirePW, err := s.core.SendRequiresPassword()
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error reading send-requires-password setting: %w", err))
		return
	}
	if requirePW && len(form.Pass) == 0 {
		s.writeAPIError(w, fmt.Errorf("password required to establish a trust line"))
		return
	}
	pass, err := s.resolvePass(form.Pass, r)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("password error: %w", err))
		return
	}
	defer zero(pass)
	txID, err := s.core.EstablishTrustline(pass, form.AssetID)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error establishing %s trust line: %w", unbip(form.AssetID), err))
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

// apiSetSendRequirePassword handles the 'setsendrequirespassword' API request,
// toggling whether sending funds requires the app password.
func (s *AppServer) apiSetSendRequirePassword(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		Require bool `json:"require"`
	}{}
	if !readPost(w, r, form) {
		return
	}
	if err := s.core.SetSendRequiresPassword(form.Require); err != nil {
		s.writeAPIError(w, fmt.Errorf("error setting send-requires-password: %w", err))
		return
	}
	writeJSON(w, simpleAck())
}

// apiActuallyLogin logs the user in. login form private data is expected to be
// cleared by the caller.
func (s *AppServer) actuallyLogin(w http.ResponseWriter, r *http.Request, login *loginForm) error {
	// Serialize against Lock/Unlock. Both mint a session (auth token + cached
	// password); without this a concurrent biometric Unlock could roll back and
	// deauth() — wiping the token this login just created — leaving an unrelated,
	// successful session silently stranded on the login gate.
	s.sessionMtx.Lock()
	defer s.sessionMtx.Unlock()

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
		// A missing or unreadable cached password is environmental, not a
		// rejected credential, so it must not feed the brute-force throttle.
		return fmt.Errorf("password error: %w", err)
	}
	err = s.core.Login(pass)
	if err != nil {
		return badCredentials{fmt.Errorf("login error: %w", err)}
	}

	if !s.isAuthed(r) {
		authToken, err := s.authorize()
		if err != nil {
			return fmt.Errorf("authorize: %w", err)
		}
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
func (s *AppServer) apiUser(w http.ResponseWriter, r *http.Request) {
	var u *core.User
	if s.isAuthed(r) {
		u = s.core.User()
	}

	s.authMtx.RLock()
	paired := s.companionToken != "" && s.companionTokenClaimed
	s.authMtx.RUnlock()

	// appSettings carries user-configurable settings surfaced on the
	// /settings page. They are only meaningful once authed; the zero value
	// is a harmless default otherwise.
	var settings appSettings
	if u != nil {
		var err error
		settings.SendRequiresPassword, err = s.core.SendRequiresPassword()
		if err != nil {
			log.Errorf("error reading send-requires-password setting: %v", err)
		}
	}

	response := struct {
		User               *core.User  `json:"user"`
		Lang               string      `json:"lang"`
		Langs              []string    `json:"langs"`
		Inited             bool        `json:"inited"`
		OK                 bool        `json:"ok"`
		OnionUrl           string      `json:"onionUrl"`
		CompanionAppPaired bool        `json:"companionAppPaired"`
		AppSettings        appSettings `json:"appSettings"`
	}{
		User:               u,
		Lang:               *s.lang.Load(),
		Langs:              s.langs,
		Inited:             s.core.IsInitialized(),
		OK:                 true,
		OnionUrl:           s.onion,
		CompanionAppPaired: paired,
		AppSettings:        settings,
	}
	writeJSON(w, response)
}

// apiFiatRates serves the cached USD exchange rates, keyed by asset ID.
// It is deliberately unauthenticated: the rates are public market data,
// and local tooling (the simnet Intents mock among it) prices against a
// running wallet without holding credentials.
func (s *AppServer) apiFiatRates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, &struct {
		OK        bool               `json:"ok"`
		FiatRates map[uint32]float64 `json:"fiatRates"`
	}{
		OK:        true,
		FiatRates: s.core.FiatRates(),
	})
}

// apiToggleRateSource handles the /toggleratesource API request.
func (s *AppServer) apiToggleRateSource(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiStakeStatus(w http.ResponseWriter, r *http.Request) {
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

func (s *AppServer) apiSetVSP(w http.ResponseWriter, r *http.Request) {
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

func (s *AppServer) apiPurchaseTickets(w http.ResponseWriter, r *http.Request) {
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

func (s *AppServer) apiSetVotingPreferences(w http.ResponseWriter, r *http.Request) {
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

func (s *AppServer) apiListVSPs(w http.ResponseWriter, r *http.Request) {
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

func (s *AppServer) apiTicketPage(w http.ResponseWriter, r *http.Request) {
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

func (s *AppServer) apiMixingStats(w http.ResponseWriter, r *http.Request) {
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

func (s *AppServer) apiConfigureMixer(w http.ResponseWriter, r *http.Request) {
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

// apiWalletTx is the handler for the '/wallettx' API request. It returns a
// single transaction from the wallet's history, whether the wallet made it
// or received funds in it.
func (s *AppServer) apiWalletTx(w http.ResponseWriter, r *http.Request) {
	var form struct {
		AssetID uint32 `json:"assetID"`
		TxID    string `json:"txID"`
	}
	if !readPost(w, r, &form) {
		return
	}
	if form.TxID == "" {
		s.writeAPIError(w, fmt.Errorf("missing txID"))
		return
	}
	tx, err := s.core.WalletTransaction(form.AssetID, form.TxID)
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	writeJSON(w, struct {
		OK bool                    `json:"ok"`
		Tx *txdb.WalletTransaction `json:"tx"`
	}{
		OK: true,
		Tx: tx,
	})
}

func (s *AppServer) apiTxHistory(w http.ResponseWriter, r *http.Request) {
	var form struct {
		txdb.TxHistoryRequest
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
		OK            bool                      `json:"ok"`
		Txs           []*txdb.WalletTransaction `json:"txs"`
		MoreAvailable bool                      `json:"moreAvailable"`
	}{
		OK:            true,
		Txs:           resp.Txs,
		MoreAvailable: resp.MoreAvailable,
	})
}

func (s *AppServer) apiTxHistoryByTicker(w http.ResponseWriter, r *http.Request) {
	var form struct {
		txdb.TxHistoryRequest
		Ticker string `json:"ticker"`
	}
	if !readPost(w, r, &form) {
		return
	}

	resp, err := s.core.TxHistoryByTicker(form.Ticker, &form.TxHistoryRequest)
	if err != nil {
		s.writeAPIError(w, fmt.Errorf("error getting ticker transaction history: %w", err))
		return
	}
	writeJSON(w, &struct {
		OK            bool                      `json:"ok"`
		Txs           []*txdb.WalletTransaction `json:"txs"`
		MoreAvailable bool                      `json:"moreAvailable"`
	}{
		OK:            true,
		Txs:           resp.Txs,
		MoreAvailable: resp.MoreAvailable,
	})
}

func (s *AppServer) apiTakeAction(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiExportAppLogs(w http.ResponseWriter, r *http.Request) {
	timeString := time.Now().Format("2006-01-02T15_04_05")
	zipAttachment := fmt.Sprintf("attachment; filename=bwlog_%s.zip", timeString)

	w.Header().Set("Content-Disposition", zipAttachment)
	w.Header().Set("Content-Type", "application/octet-stream; type=zip")
	w.WriteHeader(http.StatusOK)

	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()

	lf, err := os.Open(s.mainLogFilePath)
	if err != nil {
		log.Errorf("error opening caribou log file: %v", err)
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
		log.Errorf("error copying caribou log to zip writer: %v", err)
		return
	}
}

func (s *AppServer) apiWalletLogFilePath(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) writeAPIError(w http.ResponseWriter, err error) {
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
		// HttpOnly keeps the session cookies out of document.cookie; the
		// sessionkey cookie decrypts the cached app password. Secure is
		// omitted intentionally, matching the capability cookie: the loopback
		// server is HTTP-only, and a Secure cookie would never be sent.
		HttpOnly: true,
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
		HttpOnly: true,
	})
}

// resolvePass returns the appPW if it has a value, but if not, it attempts
// to retrieve the cached password using the information in cookies.
func (s *AppServer) resolvePass(appPW []byte, r *http.Request) ([]byte, error) {
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

// checkSendRequiresPassword enforces the send-requires-password setting for
// endpoints that move or authorize value but, unlike '/send', do not
// otherwise take a password to unlock a wallet (they rely on the wallet
// already being unlocked). When the setting is enabled, the caller must
// supply the correct app password explicitly; emptyPassMsg is returned when
// none was supplied.
func (s *AppServer) checkSendRequiresPassword(pass encode.PassBytes, emptyPassMsg string) error {
	requirePW, err := s.core.SendRequiresPassword()
	if err != nil {
		return fmt.Errorf("error reading send-requires-password setting: %w", err)
	}
	if !requirePW {
		return nil
	}
	if len(pass) == 0 {
		return errors.New(emptyPassMsg)
	}
	if err := s.core.VerifyPassword(pass); err != nil {
		return fmt.Errorf("password error: %w", err)
	}
	return nil
}

func (s *AppServer) apiAllBridgePaths(w http.ResponseWriter, r *http.Request) {
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
func (s *AppServer) apiBridgeFeesAndLimits(w http.ResponseWriter, r *http.Request) {
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

// apiBridge is the handler for the '/bridge' API request. It moves funds off
// the source chain, so it honors the send-requires-password setting exactly
// as '/send' does.
func (s *AppServer) apiBridge(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		FromAssetID uint32           `json:"fromAssetID"`
		ToAssetID   uint32           `json:"toAssetID"`
		Amount      uint64           `json:"amount"`
		BridgeName  string           `json:"bridgeName"`
		Pass        encode.PassBytes `json:"pass"`
	}{}
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	if err := s.checkSendRequiresPassword(form.Pass, "password required to bridge"); err != nil {
		s.writeAPIError(w, err)
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
func (s *AppServer) apiBridgeApprovalStatus(w http.ResponseWriter, r *http.Request) {
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

// apiApproveBridgeContract is the handler for the '/approvebridgecontract'
// API request. It grants the bridge contract a token spending allowance, so
// it honors the send-requires-password setting exactly as '/send' does.
func (s *AppServer) apiApproveBridgeContract(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID    uint32           `json:"assetID"`
		BridgeName string           `json:"bridgeName"`
		Pass       encode.PassBytes `json:"pass"`
	}{}
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	if err := s.checkSendRequiresPassword(form.Pass, "password required to approve the bridge contract"); err != nil {
		s.writeAPIError(w, err)
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

// apiUnapproveBridgeContract is the handler for the
// '/unapprovebridgecontract' API request. It revokes the bridge contract's
// token spending allowance, so it honors the send-requires-password setting
// exactly as '/send' does.
func (s *AppServer) apiUnapproveBridgeContract(w http.ResponseWriter, r *http.Request) {
	form := &struct {
		AssetID    uint32           `json:"assetID"`
		BridgeName string           `json:"bridgeName"`
		Pass       encode.PassBytes `json:"pass"`
	}{}
	defer form.Pass.Clear()
	if !readPost(w, r, form) {
		return
	}
	if err := s.checkSendRequiresPassword(form.Pass, "password required to unapprove the bridge contract"); err != nil {
		s.writeAPIError(w, err)
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
func (s *AppServer) apiPendingBridges(w http.ResponseWriter, r *http.Request) {
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
		OK      bool                      `json:"ok"`
		Bridges []*txdb.WalletTransaction `json:"bridges"`
	}{
		OK:      true,
		Bridges: bridges,
	})
}

// apiBridgeHistory is the handler for the '/bridgehistory' API request.
func (s *AppServer) apiBridgeHistory(w http.ResponseWriter, r *http.Request) {
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
		OK      bool                      `json:"ok"`
		Bridges []*txdb.WalletTransaction `json:"bridges"`
	}{
		OK:      true,
		Bridges: bridges,
	})
}

// apiCastVote is the handler for the '/castvote' API request.
func (s *AppServer) apiCastVote(w http.ResponseWriter, r *http.Request) {
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
