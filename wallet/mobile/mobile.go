// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build mobile

// Package mobile is the gomobile-bound entry point for the mobile app. It boots
// the Go core and the appserver on loopback and hands the native shell a URL to
// load in a WebView. See ~/ocean/bossmobile/mobile.md for the architecture: the
// shell hosts the existing React SPA over a loopback HTTP/WS appserver rather
// than a JS bridge.
//
// Build the bound library with:
//
//	gomobile bind -target=android -tags mobile ./wallet/mobile
//
// The contract is intentionally platform-neutral (Start/Stop only) so the iOS
// shell is a reshell rather than a core change.
package mobile

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/app"
	"github.com/bisoncraft/bosswallet/wallet/appserver"
	"github.com/bisoncraft/bosswallet/wallet/core"

	// Register the assets compiled into the mobile build. The build tags on the
	// importall files select the set (e.g. add -tags xmr to include Monero).
	_ "github.com/bisoncraft/bosswallet/wallet/asset/importall"
)

// App is the bound application handle. The native shell creates one with
// NewApp, calls Start on launch, and Stop on teardown. It is safe for
// concurrent use; lifecycle calls are serialized.
type App struct {
	mtx      sync.Mutex
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	log      util.Logger
	closeLog func()
	core     *core.Core
	srv      *appserver.AppServer
	srvCM    *util.ConnectionMaster
	capToken string
}

// NewApp constructs an App. It does no work until Start is called.
func NewApp() *App {
	return &App{}
}

// Start boots the core and appserver and returns the loopback URL the WebView
// should load. dataDir is the platform's per-app storage path (Android
// filesDir, iOS app-support dir); net is "mainnet", "testnet", or "simnet"
// (empty means mainnet). The appserver binds 127.0.0.1 on a random high port
// with plaintext HTTP: loopback traffic never leaves the device, so there is no
// TLS/self-signed-cert story, and the existing token auth is retained.
//
// Android loopback is shared by every app on the device, so the returned URL
// carries a per-run capability token (?k=...) that the appserver requires on
// every request. Only this WebView gets the token, so other local apps cannot
// reach the server even though they can open the port.
func (a *App) Start(dataDir, net string) (string, error) {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	if a.cancel != nil {
		// Already running. The native shell shares one App across Activity
		// instances, so a recreate can call Start while the core is still up;
		// hand back the live URL so the WebView reattaches instead of erroring.
		return a.bootstrapURL(), nil
	}
	if dataDir == "" {
		return "", errors.New("no data directory")
	}

	// Mint a per-run capability token. It is embedded in the bootstrap URL and
	// required on every request, so only this WebView can reach the loopback
	// server, not other apps on the device that can also open 127.0.0.1.
	capToken, err := util.RandomHex(32)
	if err != nil {
		return "", fmt.Errorf("generate capability token: %w", err)
	}

	cfg := app.DefaultConfig
	cfg.AppData = dataDir
	switch net {
	case "", "mainnet":
	case "testnet":
		cfg.Testnet = true
	case "simnet":
		cfg.Simnet = true
	default:
		return "", fmt.Errorf("unknown network %q", net)
	}
	// Loopback, random port, plaintext HTTP. A loopback IP with no WebTLS keeps
	// app.Config.Web from generating a cert pair (see config.go), so this stays
	// plaintext as the WebView requires.
	cfg.WebAddr = "127.0.0.1:0"

	if err := app.ResolveConfig(dataDir, &cfg); err != nil {
		return "", fmt.Errorf("resolve config: %w", err)
	}

	logMaker, closeLog := app.InitLogging(cfg.LogPath, cfg.DebugLevel, true, false)
	a.log = logMaker.Logger("MOBILE")
	a.log.Infof("Boss Wallet mobile starting for network %q", cfg.Net)

	clientCore, err := core.New(cfg.Core(logMaker.Logger("CORE")))
	if err != nil {
		closeLog()
		return "", fmt.Errorf("create core: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// coreDone is closed when Core.Run returns. Core.Run closes Ready() on both
	// successful init and init failure, so Ready() alone cannot tell the two
	// apart. On success Run blocks (serving) until ctx is cancelled, so coreDone
	// stays open; on failure Run returns immediately after closing Ready(), so
	// coreDone closes. That makes coreDone an unambiguous "core gave up" signal,
	// unlike racing ctx.Err() against the cancel() below.
	coreDone := make(chan struct{})
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		clientCore.Run(ctx)
		cancel() // in case Run returns before the context is cancelled
		close(coreDone)
	}()
	<-clientCore.Ready()

	// Fast path: if init already failed, coreDone is (or is about to be) closed.
	// This may not yet observe a just-failed core, so it is only a best-effort
	// early-out; the authoritative check is after the appserver binds, by which
	// point the run goroutine has certainly run.
	select {
	case <-coreDone:
		a.wg.Wait()
		closeLog()
		return "", errors.New("core stopped before startup completed; see log")
	default:
	}

	webCfg := cfg.Web(clientCore, logMaker.Logger("WEB"), false)
	webCfg.CapabilityToken = capToken
	srv, err := appserver.New(webCfg)
	if err != nil {
		cancel()
		a.wg.Wait()
		closeLog()
		return "", fmt.Errorf("create appserver: %w", err)
	}

	// Connect populates the bound address (random port) before it returns.
	srvCM := util.NewConnectionMaster(srv)
	if err := srvCM.Connect(ctx); err != nil {
		cancel()
		a.wg.Wait()
		closeLog()
		return "", fmt.Errorf("start appserver: %w", err)
	}

	// Authoritative check: if the core fell over during startup, its goroutine
	// closed coreDone while the appserver was binding/serving (ample time to be
	// scheduled). Surface that as a startup failure rather than returning a dead
	// URL.
	select {
	case <-coreDone:
		cancel()
		srvCM.Disconnect()
		a.wg.Wait()
		closeLog()
		return "", errors.New("core stopped before startup completed; see log")
	default:
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		srvCM.Wait()
	}()

	a.cancel = cancel
	a.closeLog = closeLog
	a.core = clientCore
	a.srv = srv
	a.srvCM = srvCM
	a.capToken = capToken

	// Log the address, never the token.
	a.log.Infof("Boss Wallet mobile listening at http://%s", srv.Addr())
	return a.bootstrapURL(), nil
}

// Lock locks wallets and tears down the session, delegating to the appserver's
// Lock (Core.Logout + deauth). This is the expensive key-eviction path: the
// native shell calls it only on the escalation tier (an explicit "lock now" or a
// long idle window), NOT on every background. The cheap UI gate is a native
// biometric overlay that keeps keys resident and needs no core call. After Lock,
// the WebView's session cookies are dead; a later Unlock restores them.
func (a *App) Lock() error {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	// Nothing bound means nothing is unlocked. An eviction alarm that fires after
	// the core was already torn down (or before it finished starting) has no work
	// to do, so report success rather than surfacing a spurious "not running"
	// error toast for an already-locked wallet. Lock is meant to be idempotent.
	if a.srv == nil {
		return nil
	}
	return a.srv.Lock()
}

// Unlock re-establishes the session after Lock, using a password the shell just
// released from the OS keystore after a successful biometric/PIN check. It logs
// core back in (returning only once wallets are re-unlocked, so the shell knows
// when to drop the overlay) and returns a fresh bootstrap URL carrying a
// one-time session grant. The shell loads that URL in the WebView, which swaps
// the grant for its session cookies. The password is never held in JS or passed
// through the WebView.
func (a *App) Unlock(pw string) (string, error) {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	if a.srv == nil {
		return "", errors.New("not running")
	}

	// gomobile hands pw over as an immutable Go string we cannot wipe; the []byte
	// copy is the one secret-bearing buffer we control, so zero it before return.
	pwb := []byte(pw)
	defer func() {
		for i := range pwb {
			pwb[i] = 0
		}
	}()

	grant, err := a.srv.Unlock(pwb)
	if err != nil {
		return "", err
	}
	return a.bootstrapURL() + "&g=" + grant, nil
}

// bootstrapURL is the loopback URL the WebView loads. It carries the capability
// token as a query param; the appserver swaps it for a cookie on first load and
// strips it from the URL. a.srv and a.capToken must be set.
func (a *App) bootstrapURL() string {
	return "http://" + a.srv.Addr() + "/?k=" + a.capToken
}

// Stop locks wallets and tears down the appserver and core, waiting for clean
// shutdown. The web server is stopped first so no in-flight HTTP/WS request can
// call into the core mid-teardown; cancelling the core context then drives its
// ordered shutdown, which stops wallets before closing the txDB/storage engine
// (the ordering required by the txdb review). Stop is idempotent.
func (a *App) Stop() error {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	if a.cancel == nil {
		return nil
	}

	// Shut the web server down and wait for it before touching the core, so no
	// in-flight HTTP/WS request can call into the core mid-teardown.
	a.srvCM.Disconnect()

	// With the server down, lock wallets (zero cached secrets) before
	// cancelling the core, mirroring the desktop shutdown path.
	if err := a.core.Logout(); err != nil {
		a.log.Errorf("Logout error during shutdown: %v", err)
	}

	a.cancel()
	a.wg.Wait()
	if a.closeLog != nil {
		a.closeLog()
	}

	a.cancel = nil
	a.closeLog = nil
	a.core = nil
	a.srv = nil
	a.srvCM = nil
	a.capToken = ""
	return nil
}
