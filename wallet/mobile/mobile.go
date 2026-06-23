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
func (a *App) Start(dataDir, net string) (string, error) {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	if a.cancel != nil {
		// Already running. The native shell shares one App across Activity
		// instances, so a recreate can call Start while the core is still up;
		// hand back the live URL so the WebView reattaches instead of erroring.
		return "http://" + a.srv.Addr(), nil
	}
	if dataDir == "" {
		return "", errors.New("no data directory")
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

	srv, err := appserver.New(cfg.Web(clientCore, logMaker.Logger("WEB"), false))
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

	url := "http://" + srv.Addr()
	a.log.Infof("Boss Wallet mobile listening at %s", url)
	return url, nil
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
	return nil
}
