// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package assets

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
)

// shippedPins returns every built-in electrum server with a shipped
// certificate pin, keyed by "chain.network host:port".
func shippedPins() map[string]*netparams.ServerAddr {
	pins := make(map[string]*netparams.ServerAddr)
	for chain, nets := range map[string]map[string]*netparams.ChainParams{
		"btc": BTCParams, "ltc": LTCParams, "dash": DashParams,
	} {
		for net, params := range nets {
			for _, sv := range params.ElectrumServers {
				if len(sv.Cert) > 0 {
					pins[chain+"."+net+" "+sv.Addr] = sv
				}
			}
		}
	}
	return pins
}

// TestShippedCertPinsUnexpired fails once a shipped certificate pin has
// expired. An expired pin no longer authenticates its server (the key is
// learned on first use instead), so it needs refreshing; the livenet
// TestShippedCertPins warns a year ahead.
func TestShippedCertPinsUnexpired(t *testing.T) {
	pins := shippedPins()
	if len(pins) == 0 {
		t.Fatal("no shipped certificate pins found")
	}
	now := time.Now()
	for name, sv := range pins {
		cert, err := x509.ParseCertificate(sv.Cert)
		if err != nil {
			t.Errorf("%s: shipped pin does not parse: %v", name, err)
			continue
		}
		if now.After(cert.NotAfter) {
			t.Errorf("%s: shipped pin expired %s; re-fetch the server's certificate",
				name, cert.NotAfter.UTC().Format(time.DateOnly))
		}
		if sv.SkipVerify {
			t.Errorf("%s: sets both Cert and SkipVerify", name)
		}
	}
}
