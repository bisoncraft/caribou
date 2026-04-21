// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/bisoncraft/meshwallet/wallet/app"
	"github.com/bisoncraft/meshwallet/wallet/asset"
	_ "github.com/bisoncraft/meshwallet/wallet/asset/importall"
	"github.com/bisoncraft/meshwallet/wallet/core"
	"github.com/bisoncraft/meshwallet/wallet/appserver"
	"github.com/bisoncraft/meshwallet/util"
)

// appName defines the application name.
const appName = "bisonw"

var (
	appCtx, cancel = context.WithCancel(context.Background())
	appserverReady = make(chan string, 1)
	log            util.Logger
)

func runCore(cfg *app.Config) error {
	defer cancel() // for the earliest returns

	asset.SetNetwork(cfg.Net)

	if cfg.CPUProfile != "" {
		var f *os.File
		f, err := os.Create(cfg.CPUProfile)
		if err != nil {
			return fmt.Errorf("error starting CPU profiler: %w", err)
		}
		err = pprof.StartCPUProfile(f)
		if err != nil {
			return fmt.Errorf("error starting CPU profiler: %w", err)
		}
		defer pprof.StopCPUProfile()
	}

	// Initialize logging.
	utc := !cfg.LocalLogs
	logMaker, closeLogger := app.InitLogging(cfg.LogPath, cfg.DebugLevel, true, utc)
	defer closeLogger()
	log = logMaker.Logger("BW")
	log.Infof("%s version %v (Go version %s)", appName, app.Version, runtime.Version())
	if utc {
		log.Infof("Logging with UTC time stamps. Current local time is %v",
			time.Now().Local().Format("15:04:05 MST"))
	}
	log.Infof("bisonw starting for network: %s", cfg.Net)
	log.Infof("Swap locktimes config: maker %s, taker %s",
		util.LockTimeMaker(cfg.Net), util.LockTimeTaker(cfg.Net))

	defer func() {
		if pv := recover(); pv != nil {
			log.Criticalf("Uh-oh! \n\nPanic:\n\n%v\n\nStack:\n\n%v\n\n",
				pv, string(debug.Stack()))
		}
	}()

	// Prepare the Core.
	clientCore, err := core.New(cfg.Core(logMaker.Logger("CORE")))
	if err != nil {
		return fmt.Errorf("error creating client core: %w", err)
	}

	// Catch interrupt signal (e.g. ctrl+c) to initiate a clean shutdown.
	killChan := make(chan os.Signal, 1)
	signal.Notify(killChan, os.Interrupt)
	go func() {
		for range killChan {
			if promptShutdown(clientCore) {
				log.Infof("Shutting down...")
				cancel()
				return
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		clientCore.Run(appCtx)
		cancel() // in the event that Run returns prematurely prior to context cancellation
	}()

	<-clientCore.Ready()

	defer func() {
		log.Info("Exiting bisonw main.")
		cancel()  // no-op with clean rpc/web server setup
		wg.Wait() // no-op with clean setup and shutdown
	}()

	if !cfg.NoWeb {
		webSrv, err := appserver.New(cfg.Web(clientCore, logMaker.Logger("WEB"), utc))
		if err != nil {
			return fmt.Errorf("failed creating web server: %w", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			cm := util.NewConnectionMaster(webSrv)
			err := cm.Connect(appCtx)
			if err != nil {
				log.Errorf("Error starting web server: %v", err)
				cancel()
				return
			}
			appserverReady <- webSrv.Addr()
			cm.Wait()
		}()
	} else {
		close(appserverReady)
	}

	// Wait for everything to stop.
	wg.Wait()

	return nil
}

// promptShutdown logs out of core and returns true to indicate it is safe to
// shut down.
func promptShutdown(clientCore *core.Core) bool {
	log.Infof("Attempting to logout...")
	if err := clientCore.Logout(); err != nil {
		log.Errorf("Logout error: %v", err)
	}
	return true
}
