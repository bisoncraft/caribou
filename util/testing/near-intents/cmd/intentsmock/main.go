// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Command intentsmock runs a standalone simnet mock of the NEAR Intents
// 1Click swap API, backed by the local chain harnesses, as described in
// util/testing/near-intents.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	nearintentsmock "github.com/bisoncraft/caribou/util/testing/near-intents"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:17781", "listen address")
	chains := flag.String("chains", "btc,near", "comma-separated chain slugs to enable (btc,ltc,bch,doge,dash,zec,eth,pol,sol,near)")
	dextest := flag.String("dextest", "", "harness data root (default ~/dextest)")
	rates := flag.String("rates", "http://127.0.0.3:5758", "caribou app server base URL for live USD rates ('' disables)")
	processingDelay := flag.Duration("processing-delay", 3*time.Second, "pause per intermediate swap status")
	pubkey := flag.Bool("pubkey", false, "print the quote-signing public key and exit")
	flag.Parse()

	if *pubkey {
		_, pub := nearintentsmock.DevSigningKey()
		fmt.Println(pub)
		return
	}

	var slugs []string
	for s := range strings.SplitSeq(*chains, ",") {
		if s = strings.TrimSpace(s); s != "" {
			slugs = append(slugs, s)
		}
	}

	srv, err := nearintentsmock.New(&nearintentsmock.Config{
		DextestDir:      *dextest,
		Chains:          slugs,
		ProcessingDelay: *processingDelay,
		RatesURL:        *rates,
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.Run(ctx)

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("1Click mock listening on http://%s, chains %s, signing key %s", *addr, strings.Join(slugs, ","), srv.PubKey())
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
