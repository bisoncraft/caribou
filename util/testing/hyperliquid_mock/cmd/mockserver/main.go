// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Command mockserver runs a standalone Hyperliquid API mock server for local
// wallet testing. It implements /info, /exchange, and /ws as described in
// util/testing/hyperliquid_mock.
package main

import (
	"flag"
	"log"
	"net/http"
	"strconv"

	"github.com/bisoncraft/caribou/util"
	hyperliquidmock "github.com/bisoncraft/caribou/util/testing/hyperliquid_mock"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	testnet := flag.Bool("testnet", true, "verify signatures against the testnet EIP-712 domain")
	fundAddr := flag.String("fund-addr", "", "0x address to pre-fund")
	fundHYPE := flag.Float64("fund-hype", 1000, "amount of HYPE to credit to -fund-addr")
	flag.Parse()

	net := util.Mainnet
	if *testnet {
		net = util.Testnet
	}

	srv := hyperliquidmock.New(net)

	if *fundAddr != "" {
		atoms, err := hypeToAtoms(*fundHYPE)
		if err != nil {
			log.Fatalf("invalid -fund-hype: %v", err)
		}
		srv.Fund(*fundAddr, atoms)
		log.Printf("funded %s with %.8f HYPE", *fundAddr, *fundHYPE)
	}

	log.Printf("Hyperliquid mock server listening on http://%s (info: /info, exchange: /exchange, ws: /ws)", *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func hypeToAtoms(hype float64) (uint64, error) {
	return strconv.ParseUint(strconv.FormatFloat(hype*1e8, 'f', 0, 64), 10, 64)
}
