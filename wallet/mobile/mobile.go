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

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/app"
	"github.com/bisoncraft/caribou/wallet/appserver"
	"github.com/bisoncraft/caribou/wallet/core"

	// Register the assets compiled into the mobile build. The build tags on the
	// importall files select the set (e.g. add -tags xmr to include Monero).
	_ "github.com/bisoncraft/caribou/wallet/asset/importall"
)

// App is the bound application handle. The native shell creates one with
// NewApp, calls Start on launch, and Stop on teardown. It is safe for
// concurrent use; lifecycle calls are serialized.
type App struct {
	mtx sync.Mutex
	// lifecycleWG counts in-flight Pause/Resume calls, which run without a.mtx
	// held. Stop drains it before cancelling the core. Add happens in
	// acquireCore under a.mtx, which Stop also holds when it Waits, so the
	// counter can never rise from zero alongside that Wait.
	lifecycleWG sync.WaitGroup
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	log         util.Logger
	closeLog    func()
	core        *core.Core
	srv         *appserver.AppServer
	srvCM       *util.ConnectionMaster
	capToken    string
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
	a.log.Infof("Caribou mobile starting for network %q", cfg.Net)

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
	a.log.Infof("Caribou mobile listening at http://%s", srv.Addr())
	return a.bootstrapURL(), nil
}

// secretBytes copies pw into a mutable buffer and returns it alongside a wipe
// func that zeros the buffer. gomobile hands pw over as an immutable Go string
// we cannot wipe, so this []byte copy is the one secret-bearing buffer we
// control; callers defer wipe so the password does not linger after use.
func secretBytes(pw string) (pwb []byte, wipe func()) {
	pwb = []byte(pw)
	return pwb, func() {
		for i := range pwb {
			pwb[i] = 0
		}
	}
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

	pwb, wipe := secretBytes(pw)
	defer wipe()

	grant, err := a.srv.Unlock(pwb)
	if err != nil {
		return "", err
	}
	return a.bootstrapURL() + "&g=" + grant, nil
}

// Pause drops every wallet's network connection without logging out, so a
// backgrounded app stops polling and lets the radio sleep while keys and the
// session stay resident. The native shell calls it after a short background
// grace (not on a quick app switch). Unlike Lock it does not touch the
// appserver: the session, cached password, and WebView cookies stay valid, so
// Resume needs no password. Idempotent; named Pause because "suspend" is a
// Kotlin soft keyword (the core method is Suspend).
//
// a.mtx is held only to take the core reference, not across the call. Suspend
// and Resume are per-wallet network teardown and reconnect, the one unbounded
// pair on this type, and Stop takes a.mtx before it reaches the Logout that
// zeroes secrets and closes storage in order. Holding it here would queue a
// termination behind a reconnect. iOS makes that concrete: its willTerminate
// observer calls Stop from the main thread, so the block would land on the UI
// thread with the OS termination deadline running. Core does its own locking,
// and Logout bumps a generation that unwinds an in-flight Resume, so a Stop
// that lands mid-pause-tier is safe rather than merely tolerated.
func (a *App) Pause() error {
	c, release := a.acquireCore()
	defer release()

	// Nothing bound means nothing is connected; a pause timer that fires after
	// teardown (or before startup finishes) has no work to do. Report success,
	// matching Lock's idempotence.
	if c == nil {
		return nil
	}
	return c.Suspend()
}

// Resume reconnects the wallets Pause disconnected when the app returns to the
// foreground. It reconnects nothing once the session was locked out while
// backgrounded; the shell's Unlock reconnects instead. See Pause for why a.mtx
// is not held across the call.
func (a *App) Resume() error {
	c, release := a.acquireCore()
	defer release()

	if c == nil {
		return errors.New("not running")
	}
	return c.Resume()
}

// acquireCore takes the live core under a.mtx and registers the call as
// in-flight, then releases a.mtx so the caller can make a long core call
// without holding the lifecycle lock. Returns a nil core (with a no-op release)
// if nothing is bound. Every result must be released.
//
// The registration is what makes releasing a.mtx safe. Without it a Pause or
// Resume could still be inside connectAndUpdateWallet when Stop cancels the
// core, and startWalletSyncMonitor's Add would land on the very WaitGroup
// Core.Run is waiting on. Stop drains this instead, after a Logout that has
// already unwound the reconnect.
func (a *App) acquireCore() (*core.Core, func()) {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	if a.core == nil {
		return nil, func() {}
	}
	a.lifecycleWG.Add(1)
	return a.core, a.lifecycleWG.Done
}

// VerifyPassword reports whether pw is the correct app password, without
// changing session state. The native shell calls it during biometric opt-in to
// confirm the password before storing it in the Android Keystore. A nil error
// means the password is correct.
func (a *App) VerifyPassword(pw string) error {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	if a.core == nil {
		return errors.New("not running")
	}

	pwb, wipe := secretBytes(pw)
	defer wipe()
	return a.core.VerifyPassword(pwb)
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

	// Pause and Resume run without a.mtx, so one can be in flight right now.
	// The Logout above bumped Core's suspend generation, which unwinds an
	// in-flight Resume after at most one wallet, so this drains promptly. It has
	// to happen before the cancel: a reconnect still in flight would start a
	// sync monitor on the WaitGroup Core.Run is about to wait on.
	a.lifecycleWG.Wait()

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
