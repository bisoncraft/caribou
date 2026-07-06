//go:build !live

package appserver

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	pi "github.com/bisoncraft/bosswallet/util/politeia"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/core"
	"github.com/bisoncraft/bosswallet/wallet/db"
	"github.com/bisoncraft/bosswallet/wallet/mnemonic"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
)

var (
	errTest        = fmt.Errorf("expected dummy error")
	tLogger        util.Logger
	tCtx           context.Context
	testAppVersion = "1.1.0-rc1+release.local"
)

type tCoin struct {
	id       []byte
	confs    uint32
	confsErr error
}

func (c *tCoin) ID() util.Bytes {
	return c.id
}

func (c *tCoin) String() string {
	return hex.EncodeToString(c.id)
}

func (c *tCoin) TxID() string {
	return hex.EncodeToString(c.id)
}

func (c *tCoin) Value() uint64 {
	return 0
}

func (c *tCoin) Confirmations(context.Context) (uint32, error) {
	return c.confs, c.confsErr
}

type TCore struct {
	clientCore           // This is here so we don't have to implement core methods we're not testing
	balanceErr           error
	loginErr             error
	logoutErr            error
	logoutCalls          int
	initErr              error
	isInited             bool
	createWalletErr      error
	openWalletErr        error
	closeWalletErr       error
	rescanWalletErr      error
	sendErr              error
	sendRequiresPW       bool
	sendRequiresPWErr    error
	setSendRequiresPWErr error
	notHas               bool
	notRunning           bool
	notOpen              bool
	rateSourceErr        error
	estFee               uint64
	estFeeErr            error
	validAddr            bool
	walletDisabled       bool
	walletStatusErr      error
	notes                []*db.Notification
	notesErr             error
}

func (c *TCore) Network() util.Network { return util.Mainnet }
func (c *TCore) ToggleRateSourceStatus(src string, disable bool) error {
	return c.rateSourceErr
}
func (c *TCore) FiatRateSources() map[string]bool {
	return nil
}

func (c *TCore) InitializeClient(pw []byte, seed *string) (string, error) {
	var mnemonicSeed string
	if seed == nil {
		_, mnemonicSeed = mnemonic.New()
	}
	return mnemonicSeed, c.initErr
}
func (c *TCore) Login(pw []byte) error                                    { return c.loginErr }
func (c *TCore) IsInitialized() bool                                      { return c.isInited }
func (c *TCore) AssetBalance(assetID uint32) (*core.WalletBalance, error) { return nil, c.balanceErr }
func (c *TCore) WalletFeeState(assetID uint32) (*core.FeeState, error)    { return &core.FeeState{}, nil }
func (c *TCore) WalletState(assetID uint32) *core.WalletState {
	if c.notHas {
		return nil
	}
	return &core.WalletState{
		Symbol:  unbip(assetID),
		AssetID: assetID,
		Open:    !c.notOpen,
		Running: !c.notRunning,
	}
}
func (c *TCore) CreateWallet(appPW, walletPW []byte, form *core.WalletForm) error {
	return c.createWalletErr
}
func (c *TCore) RescanWallet(assetID uint32, force bool) error    { return c.rescanWalletErr }
func (c *TCore) OpenWallet(assetID uint32, pw []byte) error       { return c.openWalletErr }
func (c *TCore) CloseWallet(assetID uint32) error                 { return c.closeWalletErr }
func (c *TCore) ConnectWallet(assetID uint32) error               { return nil }
func (c *TCore) Wallets() []*core.WalletState                     { return nil }
func (c *TCore) WalletSettings(uint32) (map[string]string, error) { return nil, nil }
func (c *TCore) ReconfigureWallet(aPW, nPW []byte, form *core.WalletForm) error {
	return nil
}
func (c *TCore) ToggleWalletStatus(assetID uint32, disable bool) error {
	if c.walletStatusErr != nil {
		return c.walletStatusErr
	}
	c.walletDisabled = disable
	return c.walletStatusErr
}
func (c *TCore) ChangeAppPass(appPW, newAppPW []byte) error                         { return nil }
func (c *TCore) ResetAppPass(newAppPW []byte, seed string) error                    { return nil }
func (c *TCore) SetWalletPassword(appPW []byte, assetID uint32, newPW []byte) error { return nil }
func (c *TCore) NewDepositAddress(assetID uint32) (string, error)                   { return "", nil }
func (c *TCore) AutoWalletConfig(assetID uint32, walletType string) (map[string]string, error) {
	return nil, nil
}
func (c *TCore) User() *core.User { return nil }
func (c *TCore) SupportedAssets() map[uint32]*core.SupportedAsset {
	return make(map[uint32]*core.SupportedAsset)
}
func (c *TCore) Send(pw []byte, assetID uint32, value uint64, address string, subtract bool) (asset.Coin, error) {
	return &tCoin{id: []byte{0xde, 0xc7, 0xed}}, c.sendErr
}
func (c *TCore) ValidateAddress(address string, assetID uint32) (bool, error) {
	return c.validAddr, nil
}
func (c *TCore) EstimateSendTxFee(addr string, assetID uint32, value uint64, subtract, maxWithdraw bool) (fee uint64, isValidAddress bool, err error) {
	return c.estFee, true, c.estFeeErr
}
func (c *TCore) NotificationFeed() *core.NoteFeed {
	return &core.NoteFeed{
		C: make(chan core.Notification, 1),
	}
}

func (c *TCore) AckNotes(ids []util.Bytes) {}

func (c *TCore) Logout() error { c.logoutCalls++; return c.logoutErr }

func (c *TCore) ExportSeed(pw []byte) (string, error) {
	return "seed words here", nil
}
func (c *TCore) WalletLogFilePath(uint32) (string, error) {
	return "", nil
}
func (c *TCore) RecoverWallet(uint32, []byte, bool) error {
	return nil
}
func (c *TCore) WalletRestorationInfo(pw []byte, assetID uint32) ([]*asset.WalletRestoration, error) {
	return nil, nil
}
func (c *TCore) WalletPeers(assetID uint32) ([]*asset.WalletPeer, error) {
	return nil, nil
}
func (c *TCore) AddWalletPeer(assetID uint32, address string) error {
	return nil
}
func (c *TCore) RemoveWalletPeer(assetID uint32, address string) error {
	return nil
}
func (c *TCore) Notifications(n int) (notes, pokes []*db.Notification, _ error) {
	return c.notes, []*db.Notification{}, c.notesErr
}
func (c *TCore) StakeStatus(assetID uint32) (*asset.TicketStakingStatus, error) {
	return nil, nil
}

func (c *TCore) SetVSP(assetID uint32, addr string) error {
	return nil
}

func (c *TCore) PurchaseTickets(assetID uint32, appPW []byte, n int) error {
	return nil
}

func (c *TCore) SetVotingPreferences(assetID uint32, choices, tSpendPolicy, treasuryPolicy map[string]string) error {
	return nil
}

func (c *TCore) ListVSPs(assetID uint32) ([]*asset.VotingServiceProvider, error) {
	return nil, nil
}

func (c *TCore) TicketPage(assetID uint32, scanStart int32, n, skipN int) ([]*asset.Ticket, error) {
	return nil, nil
}

func (c *TCore) TxHistory(assetID uint32, req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	return nil, nil
}

func (c *TCore) TxHistoryByTicker(ticker string, req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	return nil, nil
}

func (c *TCore) FundsMixingStats(assetID uint32) (*asset.FundsMixingStats, error) {
	return nil, nil
}

func (c *TCore) ConfigureFundsMixer(appPW []byte, assetID uint32, enabled bool) error {
	return nil
}

func (*TCore) SetLanguage(string) error              { return nil }
func (*TCore) Language() string                      { return "en-US" }
func (*TCore) SetCompanionToken(token string) error  { return nil }
func (*TCore) CompanionToken() (string, error)       { return "", nil }
func (*TCore) SetAPIToken(token string) error        { return nil }
func (*TCore) APIToken() (string, error)             { return "", nil }
func (c *TCore) SetSendRequiresPassword(bool) error  { return c.setSendRequiresPWErr }
func (c *TCore) SendRequiresPassword() (bool, error) { return c.sendRequiresPW, c.sendRequiresPWErr }

func (*TCore) TakeAction(assetID uint32, actionID string, actionB json.RawMessage) error { return nil }

func (*TCore) ExtensionModeConfig() *core.ExtensionModeConfig {
	return nil
}

func (*TCore) ProposalsAll(offset, rowsCount int, searchPhrase string, filterByVoteStatus ...int) ([]*pi.Proposal, int, error) {
	return nil, 0, nil
}

func (*TCore) Proposal(assetID uint32, token string) (*pi.Proposal, error) {
	return nil, nil
}

func (*TCore) ProposalsInProgress() ([]*pi.MiniProposal, error) {
	return nil, nil
}

func (*TCore) CastVote(assetID uint32, pw []byte, token, bit string) error {
	return nil
}

func (*TCore) PoliteiaDetails() (string, bool, int64) {
	return "", false, 0
}

type TWriter struct {
	b []byte
}

func (*TWriter) Header() http.Header {
	return http.Header{}
}

func (w *TWriter) Write(b []byte) (int, error) {
	w.b = b
	return len(b), nil
}

func (w *TWriter) WriteHeader(int) {}

type TReader struct {
	msg []byte
	err error
}

func (r *TReader) Read(p []byte) (n int, err error) {
	if r.err != nil {
		return 0, r.err
	}
	if len(r.msg) == 0 {
		return 0, io.EOF
	}
	copy(p, r.msg)
	if len(p) < len(r.msg) {
		r.msg = r.msg[:len(p)]
		return len(p), nil
	}
	l := len(r.msg)
	r.msg = nil
	return l, io.EOF
}

func (r *TReader) Close() error { return nil }

func newTServer(t *testing.T, start bool) (*AppServer, *TCore, func()) {
	t.Helper()
	c := &TCore{}
	var shutdown func()
	ctx, killCtx := context.WithCancel(tCtx)
	s, err := New(&Config{
		Core:       c,
		Addr:       "127.0.0.1:0",
		Logger:     tLogger,
		AppVersion: testAppVersion,
	})
	if err != nil {
		t.Fatalf("error creating server: %v", err)
	}

	if start {
		cm := util.NewConnectionMaster(s)
		err := cm.Connect(ctx)
		if err != nil {
			t.Fatalf("Error starting AppServer: %v", err)
		}
		shutdown = func() {
			killCtx()
			cm.Disconnect()
		}
	} else {
		shutdown = killCtx
	}
	return s, c, shutdown
}

func ensureResponse(t *testing.T, f func(w http.ResponseWriter, r *http.Request), want string, reader *TReader, writer *TWriter, body any, cookies map[string]string) {
	t.Helper()
	var err error
	reader.msg, err = json.Marshal(body)
	if err != nil {
		t.Fatalf("error marshalling request body: %v", err)
	}
	req, err := http.NewRequest("GET", "/", reader)
	if err != nil {
		t.Fatalf("error creating request: %v", err)
	}
	for name, value := range cookies {
		cookie := http.Cookie{
			Name:  name,
			Value: value,
		}
		req.AddCookie(&cookie)
	}
	f(writer, req)
	if len(writer.b) == 0 {
		t.Fatalf("no response")
	}
	// Drop the line feed.
	errMsg := string(writer.b[:len(writer.b)-1])
	if errMsg != want {
		t.Fatalf("wrong response. expected %s, got %s", want, errMsg)
	}
	writer.b = nil
}

func TestAPITokenAuth(t *testing.T) {
	s, _, shutdown := newTServer(t, false)
	defer shutdown()

	const token = "f00dcafe"
	s.apiTokenHash = hashToken(token)

	newReq := func(remoteAddr, headerToken string) *http.Request {
		req, err := http.NewRequest("POST", "/api/balance", nil)
		if err != nil {
			t.Fatalf("error creating request: %v", err)
		}
		req.RemoteAddr = remoteAddr
		if headerToken != "" {
			req.Header.Set(apiTokenHeader, headerToken)
		}
		return req
	}

	tests := []struct {
		name       string
		remoteAddr string
		headerTok  string
		storedHash string
		want       bool
	}{
		{"valid token from loopback", "127.0.0.1:5555", token, s.apiTokenHash, true},
		{"valid token from loopback ipv6", "[::1]:5555", token, s.apiTokenHash, true},
		{"valid token from non-loopback", "10.0.0.5:5555", token, s.apiTokenHash, false},
		{"wrong token from loopback", "127.0.0.1:5555", "deadbeef", s.apiTokenHash, false},
		{"no header", "127.0.0.1:5555", "", s.apiTokenHash, false},
		{"no token configured", "127.0.0.1:5555", token, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.apiTokenHash = tt.storedHash
			req := newReq(tt.remoteAddr, tt.headerTok)
			if got := s.apiTokenAuthorized(req); got != tt.want {
				t.Fatalf("apiTokenAuthorized = %v, want %v", got, tt.want)
			}
			// isAuthed must agree: a valid local token authorizes with no
			// cookie session present.
			if got := s.isAuthed(req); got != tt.want {
				t.Fatalf("isAuthed = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAPITokenEndpoints exercises the generate/revoke handlers at the HTTP
// level and the full generate -> use -> revoke flow.
func TestAPITokenEndpoints(t *testing.T) {
	s, _, shutdown := newTServer(t, false)
	defer shutdown()

	// call invokes a handler with the given remote address, optional
	// X-API-Token header, and JSON body, returning the response bytes.
	call := func(h http.HandlerFunc, remoteAddr, headerTok, body string) string {
		req, err := http.NewRequest("POST", "/", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.RemoteAddr = remoteAddr
		if headerTok != "" {
			req.Header.Set(apiTokenHeader, headerTok)
		}
		w := new(TWriter)
		h(w, req)
		return string(w.b)
	}

	// Generation is rejected from a non-loopback peer and stores nothing.
	if resp := call(s.apiGenerateAPIToken, "10.0.0.5:5555", "", "{}"); !strings.Contains(resp, `"ok":false`) {
		t.Fatalf("non-loopback generation should be rejected, got %s", resp)
	}
	if s.apiTokenHash != "" {
		t.Fatalf("token hash must not be set after a rejected generation")
	}

	// Generation succeeds from loopback and returns a usable token whose
	// hash is what gets stored.
	var gen struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	resp := call(s.apiGenerateAPIToken, "127.0.0.1:5555", "", "{}")
	if err := json.Unmarshal([]byte(resp), &gen); err != nil {
		t.Fatalf("unmarshal generate response %q: %v", resp, err)
	}
	if !gen.OK || gen.Token == "" {
		t.Fatalf("expected ok response with a token, got %s", resp)
	}
	if s.apiTokenHash != hashToken(gen.Token) {
		t.Fatalf("stored hash does not match the generated token")
	}

	// The fresh token authorizes a request from loopback.
	useReq, _ := http.NewRequest("POST", "/", nil)
	useReq.RemoteAddr = "127.0.0.1:5555"
	useReq.Header.Set(apiTokenHeader, gen.Token)
	if !s.isAuthed(useReq) {
		t.Fatalf("freshly generated token should authorize")
	}

	// Logout does not revoke the API token: it survives deauth so in-flight
	// automation clients keep working until /revokeapitoken is called.
	s.deauth()
	if s.apiTokenHash == "" {
		t.Fatalf("API token hash must survive logout")
	}
	if !s.isAuthed(useReq) {
		t.Fatalf("API token should still authorize after logout")
	}

	// Revoke is rejected from a non-loopback peer and leaves the token intact.
	if resp := call(s.apiRevokeAPIToken, "10.0.0.5:5555", "", "{}"); !strings.Contains(resp, `"ok":false`) {
		t.Fatalf("non-loopback revoke should be rejected, got %s", resp)
	}
	if s.apiTokenHash == "" {
		t.Fatalf("token hash must survive a rejected revoke")
	}

	// Revoke succeeds from loopback and clears the token.
	if resp := call(s.apiRevokeAPIToken, "127.0.0.1:5555", "", "{}"); !strings.Contains(resp, `"ok":true`) {
		t.Fatalf("revoke should succeed, got %s", resp)
	}
	if s.apiTokenHash != "" {
		t.Fatalf("token hash should be cleared after revoke")
	}

	// The revoked token no longer authorizes.
	if s.isAuthed(useReq) {
		t.Fatalf("revoked token should not authorize")
	}
}

func TestMain(m *testing.M) {
	tLogger = util.StdOutLogger("TEST", util.LevelTrace)
	var shutdown func()
	tCtx, shutdown = context.WithCancel(context.Background())
	doIt := func() int {
		// Not counted as coverage, must test Archiver constructor explicitly.
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

func TestNew_siteError(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot get current directory: %v", err)
	}

	// Change to a directory with no "site" or "../../appserver/site" folder.
	dir := t.TempDir()
	defer os.Chdir(cwd) // leave the temp dir before trying to delete it

	if err = os.Chdir(dir); err != nil {
		t.Fatalf("Cannot cd to %q", dir)
	}

	c := &TCore{}
	_, err = New(&Config{
		Core:    c,
		Addr:    "127.0.0.1:0",
		Logger:  tLogger,
		NoEmbed: true, // this tests locating on-disk files, not the embedded ones
	})
	if err == nil || !strings.HasPrefix(err.Error(), "no HTML template files found") {
		t.Errorf("Should have failed to start with no site folder.")
	}
}

func TestConnectStart(t *testing.T) {
	_, _, shutdown := newTServer(t, true)
	defer shutdown()
}

func TestConnectBindError(t *testing.T) {
	s0, _, shutdown := newTServer(t, true)
	defer shutdown()

	tAddr := s0.addr
	s, err := New(&Config{
		Core: &TCore{},
		Addr: tAddr,
	})
	if err != nil {
		t.Fatalf("error creating server: %v", err)
	}

	cm := util.NewConnectionMaster(s)
	err = cm.Connect(tCtx)
	if err == nil {
		shutdown() // shutdown both servers with shared context
		cm.Disconnect()
		t.Fatalf("should have failed to bind")
	}
}

func TestAPILogin(t *testing.T) {
	writer := new(TWriter)
	var body any
	reader := new(TReader)
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	ensure := func(want string) {
		t.Helper()
		ensureResponse(t, s.apiLogin, want, reader, writer, body, nil)
	}

	goodBody := &loginForm{
		Pass: encode.PassBytes("def"),
	}
	body = goodBody
	ensure(`{"ok":true,"notes":null,"pokes":[]}`)

	tCore.notes = []*db.Notification{{
		TopicID: core.TopicWalletUnlockError,
	}}
	ensure(`{"ok":true,"notes":[{"type":"","topic":"WalletUnlockError","subject":"","details":"","severity":0,"stamp":0,"acked":false,"id":""}],"pokes":[]}`)

	tCore.notes = nil
	tCore.notesErr = errors.New("")
	ensure(`{"ok":true,"notes":null,"pokes":[]}`)

	// Login error
	tCore.loginErr = errTest
	ensure(fmt.Sprintf(`{"ok":false,"msg":"%s"}`, errTest))
	tCore.loginErr = nil
}

func TestSend(t *testing.T) {
	writer := new(TWriter)
	var body any
	reader := new(TReader)
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	isOK := func() bool {
		reader.msg, _ = json.Marshal(body)
		req, err := http.NewRequest("GET", "/", reader)
		if err != nil {
			t.Fatalf("error creating request: %v", err)
		}
		s.apiSend(writer, req)
		if len(writer.b) == 0 {
			t.Fatalf("no response")
		}
		resp := &standardResponse{}
		err = json.Unmarshal(writer.b, resp)
		if err != nil {
			t.Fatalf("json unmarshal error: %v", err)
		}
		return resp.OK
	}

	body = &sendForm{
		Pass: encode.PassBytes("dummyAppPass"),
	}

	// initial success
	if !isOK() {
		t.Fatalf("not ok: %s", string(writer.b))
	}

	// no wallet
	tCore.notHas = true
	if isOK() {
		t.Fatalf("no error for missing wallet")
	}
	tCore.notHas = false

	// Send/Withdraw error
	tCore.sendErr = errTest
	if isOK() {
		t.Fatalf("no error for Send/Withdraw error")
	}
	tCore.sendErr = nil

	// re-success
	if !isOK() {
		t.Fatalf("not ok afterwards: %s", string(writer.b))
	}

	// send-requires-password enabled, but no password supplied -> rejected.
	tCore.sendRequiresPW = true
	body = &sendForm{Pass: nil}
	if isOK() {
		t.Fatalf("no error when password required but none supplied")
	}

	// send-requires-password enabled, explicit password supplied -> ok.
	body = &sendForm{Pass: encode.PassBytes("dummyAppPass")}
	if !isOK() {
		t.Fatalf("not ok with explicit pass when password required: %s", string(writer.b))
	}

	// An error reading the setting is surfaced.
	tCore.sendRequiresPWErr = errTest
	if isOK() {
		t.Fatalf("no error for SendRequiresPassword error")
	}
	tCore.sendRequiresPWErr = nil
	tCore.sendRequiresPW = false
}

func TestAPISetSendRequirePassword(t *testing.T) {
	writer := new(TWriter)
	reader := new(TReader)
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	body := &struct {
		Require bool `json:"require"`
	}{Require: true}

	// success
	ensureResponse(t, s.apiSetSendRequirePassword, `{"ok":true}`, reader, writer, body, nil)

	// core error is surfaced
	tCore.setSendRequiresPWErr = errTest
	ensureResponse(t, s.apiSetSendRequirePassword,
		fmt.Sprintf(`{"ok":false,"msg":"%s"}`, errTest), reader, writer, body, nil)
	tCore.setSendRequiresPWErr = nil
}

func TestAPIInit(t *testing.T) {
	writer := new(TWriter)
	var body any
	reader := new(TReader)
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	ensure := func(f func(http.ResponseWriter, *http.Request), want string) {
		t.Helper()
		ensureResponse(t, f, want, reader, writer, body, nil)
	}

	body = struct{}{}

	// Success but uninitialized
	ensure(s.apiIsInitialized, `{"ok":true,"initialized":false}`)

	// Now initialized
	tCore.isInited = true
	ensure(s.apiIsInitialized, `{"ok":true,"initialized":true}`)

	// Initialization error
	tCore.initErr = errTest
	ensure(s.apiInit, fmt.Sprintf(`{"ok":false,"msg":"%s"}`, errTest))
	tCore.initErr = nil
}

// TODO: TestAPIGetDEXInfo

func TestAPINewWallet(t *testing.T) {
	writer := new(TWriter)
	var body any
	reader := new(TReader)
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	ensure := func(want string) {
		ensureResponse(t, s.apiNewWallet, want, reader, writer, body, nil)
	}

	body = &newWalletForm{
		Pass:  encode.PassBytes("abc"),
		AppPW: encode.PassBytes("dummyAppPass"),
	}
	tCore.notHas = true
	ensure(`{"ok":true}`)

	tCore.notHas = false
	ensure(`{"ok":false,"msg":"already have a wallet for btc"}`)
	tCore.notHas = true

	tCore.createWalletErr = errTest
	ensure(fmt.Sprintf(`{"ok":false,"msg":"%s"}`, errTest))
	tCore.createWalletErr = nil

	tCore.notHas = false
}

func TestAPILogout(t *testing.T) {
	writer := new(TWriter)
	reader := new(TReader)
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	ensure := func(want string) {
		ensureResponse(t, s.apiLogout, want, reader, writer, nil, nil)
	}
	ensure(`{"ok":true}`)

	// Logout error
	tCore.logoutErr = errTest
	ensure(fmt.Sprintf(`{"ok":false,"msg":"%s"}`, errTest))
	tCore.logoutErr = nil
}

func TestApiGetBalance(t *testing.T) {
	writer := new(TWriter)
	reader := new(TReader)
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	ensure := func(want string) {
		ensureResponse(t, s.apiGetBalance, want, reader, writer, struct{}{}, nil)
	}
	ensure(`{"ok":true,"balance":null}`)

	// Logout error
	tCore.balanceErr = errTest
	ensure(fmt.Sprintf(`{"ok":false,"msg":"%s"}`, errTest))
	tCore.balanceErr = nil
}

func TestPasswordCache(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	password := encode.PassBytes("def")
	authToken1, err := s.authorize()
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	authToken2, err := s.authorize()
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	key1, err := s.cacheAppPassword(password, authToken1)
	if err != nil {
		t.Fatalf("error caching password: %v", err)
	}

	key2, err := s.cacheAppPassword(password, authToken2)
	if err != nil {
		t.Fatalf("error caching password: %v", err)
	}

	retrievedPW, err := s.getCachedPassword(key1, authToken1)
	if err != nil {
		t.Fatalf("error getting password: %v", err)
	}
	if !bytes.Equal(password, retrievedPW) {
		t.Fatalf("retrieved PW not same: %v - %v", password, retrievedPW)
	}

	retrievedPW, err = s.getCachedPassword(key2, authToken2)
	if err != nil {
		t.Fatalf("error getting password: %v", err)
	}
	if !bytes.Equal(password, retrievedPW) {
		t.Fatalf("retrieved PW not same: %v - %v", password, retrievedPW)
	}

	// test new wallet request first without the cookies populated, then with
	writer := new(TWriter)
	reader := new(TReader)
	body := &newWalletForm{
		Pass: encode.PassBytes(""),
	}
	want := `{"ok":false,"msg":"app pass cannot be empty"}`
	tCore.notHas = true
	ensureResponse(t, s.apiNewWallet, want, reader, writer, body, nil)

	want = `{"ok":true}`
	ensureResponse(t, s.apiNewWallet, want, reader, writer, body, map[string]string{
		authCK:  authToken1,
		pwKeyCK: hex.EncodeToString(key1),
	})

	s.apiLogout(writer, nil)

	if len(s.cachedPasswords) != 0 {
		t.Fatal("logout should clear all cached passwords")
	}
}

func TestAPI_ToggleRatesource(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	writer := new(TWriter)
	reader := new(TReader)

	type rateSourceForm struct {
		Disable bool   `json:"disable"`
		Source  string `json:"source"`
	}

	// Test enabling fiat rate sources.
	enableTests := []struct {
		name, source, want string
		wantErr            error
	}{{
		name:    "Invalid rate source",
		source:  "binance",
		wantErr: errors.New("cannot enable unknown fiat rate source"),
		want:    `{"ok":false,"msg":"cannot enable unknown fiat rate source"}`,
	}, {
		name:   "ok valid source",
		source: "dcrdata",
		want:   `{"ok":true}`,
	}, {
		name:   "ok already initialized",
		source: "dcrdata",
		want:   `{"ok":true}`,
	}}

	for _, test := range enableTests {
		body := &rateSourceForm{
			Disable: false,
			Source:  test.source,
		}
		tCore.rateSourceErr = test.wantErr
		ensureResponse(t, s.apiToggleRateSource, test.want, reader, writer, body, nil)
	}

	// Test disabling fiat rate sources.
	disableTests := []struct {
		name, source, want string
		wantErr            error
	}{{
		name:    "Invalid rate source",
		source:  "Messari",
		wantErr: errors.New("cannot disable unknown fiat rate source"),
		want:    `{"ok":false,"msg":"cannot disable unknown fiat rate source"}`,
	}, {
		name:   "ok valid source",
		source: "Coinpaprika",
		want:   `{"ok":true}`,
	}, {
		name:   "ok already disabled/not initialized",
		source: "Coinpaprika",
		want:   `{"ok":true}`,
	}}

	for _, test := range disableTests {
		body := &rateSourceForm{
			Disable: true,
			Source:  test.source,
		}
		tCore.rateSourceErr = test.wantErr
		ensureResponse(t, s.apiToggleRateSource, test.want, reader, writer, body, nil)
	}
}

func TestAPIValidateAddress(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	writer := new(TWriter)
	reader := new(TReader)
	testID := uint32(42)

	body := &struct {
		Addr    string  `json:"addr"`
		AssetID *uint32 `json:"assetID"`
	}{
		Addr:    "addr",
		AssetID: &testID,
	}

	want := `{"ok":true}`
	tCore.validAddr = true
	ensureResponse(t, s.apiValidateAddress, want, reader, writer, body, nil)

	want = `{"ok":false}`
	tCore.validAddr = false
	ensureResponse(t, s.apiValidateAddress, want, reader, writer, body, nil)
}

func TestAPIEstimateSendTxFee(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()

	writer := new(TWriter)
	reader := new(TReader)
	testID := uint32(42)

	body := &sendTxFeeForm{
		Addr:     "addr",
		Value:    1e8,
		Subtract: false,
		AssetID:  &testID,
	}

	want := `{"ok":true,"txfee":10000,"validaddress":true}`
	tCore.estFee = 10000
	ensureResponse(t, s.apiEstimateSendTxFee, want, reader, writer, body, nil)

	want = fmt.Sprintf(`{"ok":false,"msg":"%s"}`, errTest)
	tCore.estFeeErr = errTest
	ensureResponse(t, s.apiEstimateSendTxFee, want, reader, writer, body, nil)
}

func TestAPIToggleWalletStatus(t *testing.T) {
	s, tCore, shutdown := newTServer(t, false)
	defer shutdown()
	writer := new(TWriter)
	reader := new(TReader)

	var body *walletStatusForm
	ensure := func(want string) {
		ensureResponse(t, s.apiToggleWalletStatus, want, reader, writer, body, nil)
	}

	body = &walletStatusForm{
		Disable: true,
		AssetID: 12,
	}

	ensure(`{"ok":true}`)
	if !tCore.walletDisabled {
		t.Fatal("Expected wallet to be disabled")
	}

	tCore.walletStatusErr = errors.New("wallet not found")
	ensure(`{"ok":false,"msg":"wallet not found"}`)

	tCore.walletDisabled = false
	body.Disable = false
	tCore.walletStatusErr = nil
	ensure(`{"ok":true}`)
	if tCore.walletDisabled {
		t.Fatal("Expected wallet to be enabled")
	}
}

func TestAPITrade(t *testing.T) {
	t.Skip("removed: DEX trading functionality")
}

func TestAPITradeAsync(t *testing.T) {
	t.Skip("removed: DEX trading functionality")
}

func Test_prepareAddr(t *testing.T) {
	tests := []struct {
		name       string
		addr       net.Addr
		allowInCSP bool
		want       string
	}{{
		name: "OK: IPv4",
		addr: &net.TCPAddr{
			IP:   net.IPv4(127, 0, 0, 1),
			Port: 7232,
		},
		want:       "127.0.0.1:7232",
		allowInCSP: true,
	}, {
		name: "IPv6 loopback",
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 7232,
		},
		want: "[::1]:7232",
	}, {
		name: "OK: IPv6 unspecified",
		addr: &net.TCPAddr{
			IP:   net.IPv6unspecified,
			Port: 7232,
		},
		want:       "127.0.0.1:7232",
		allowInCSP: true,
	}, {
		name: "OK: zero IPv4",
		addr: &net.TCPAddr{
			IP:   net.IPv4zero,
			Port: 7232,
		},
		want:       "127.0.0.1:7232",
		allowInCSP: true,
	}, {
		name: "others",
		addr: &net.UDPAddr{
			IP:   []byte{},
			Port: 7232,
		},
		want: ":7232",
	}}

	for _, test := range tests {
		gotAddr, allowInCSP := prepareAddr(test.addr)
		if gotAddr != test.want {
			t.Fatalf("%s: address: got %s, want %s", test.name, gotAddr, test.want)
		}
		if allowInCSP != test.allowInCSP {
			t.Fatalf("%s: allow in CSP: got %v, want %v", test.name, allowInCSP, test.allowInCSP)
		}
	}
}

func TestAPIBuildInfo(t *testing.T) {
	s, _, shutdown := newTServer(t, false)
	defer shutdown()

	writer := new(TWriter)
	reader := new(TReader)

	expectedBody := &buildInfoResponse{
		OK:       true,
		Version:  s.appVersion,
		Revision: commitHash, // this is not set in the test, so it will be empty
	}

	body, err := json.Marshal(expectedBody)
	if err != nil {
		t.Fatalf("error marshalling expected body: %v", err)
	}

	ensureResponse(t, s.apiBuildInfo, string(body), reader, writer, nil, nil)
}
