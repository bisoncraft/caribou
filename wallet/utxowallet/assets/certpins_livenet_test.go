// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

//go:build livenet

// Live check of the shipped certificate pins against the servers they pin.
// It dials real mainnet infrastructure, so it is opt-in:
//
//	go test -tags livenet -run TestShippedCertPins -v ./wallet/utxowallet/assets/

package assets

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"
)

// TestShippedCertPins fails when a pinned server presents a certificate other
// than its shipped one, or when a shipped certificate expires within a year.
// A new certificate for the same key still authenticates, but the constant
// should be refreshed to carry the later expiry; a new key is refused by
// clients while their effective pin for the old key is active.
func TestShippedCertPins(t *testing.T) {
	const warnWithin = 365 * 24 * time.Hour
	for name, sv := range shippedPins() {
		t.Run(name, func(t *testing.T) {
			shipped, err := x509.ParseCertificate(sv.Cert)
			if err != nil {
				t.Fatalf("shipped pin does not parse: %v", err)
			}
			if left := time.Until(shipped.NotAfter); left < warnWithin {
				t.Errorf("shipped pin expires %s, within a year; re-fetch the certificate",
					shipped.NotAfter.UTC().Format(time.DateOnly))
			}
			host, _, err := net.SplitHostPort(sv.Addr)
			if err != nil {
				t.Fatalf("address %q: %v", sv.Addr, err)
			}
			dialer := &net.Dialer{Timeout: 20 * time.Second}
			conn, err := tls.DialWithDialer(dialer, "tcp", sv.Addr, &tls.Config{
				ServerName:         host,
				InsecureSkipVerify: true, // the comparison below is the check
			})
			if err != nil {
				t.Fatalf("handshake failed; the pin is unverified: %v", err)
			}
			leaf := conn.ConnectionState().PeerCertificates[0]
			conn.Close()
			if bytes.Equal(leaf.Raw, sv.Cert) {
				return
			}
			if sha256.Sum256(leaf.RawSubjectPublicKeyInfo) == sha256.Sum256(shipped.RawSubjectPublicKeyInfo) {
				t.Errorf("server presents a renewed certificate for the pinned key (valid until %s); "+
					"refresh the shipped constant", leaf.NotAfter.UTC().Format(time.DateOnly))
			} else {
				t.Errorf("server presents a different key (certificate valid until %s); "+
					"clients refuse it while their pin for the shipped key is active (the "+
					"shipped pin until %s, longer where a renewal for that key was learned)",
					leaf.NotAfter.UTC().Format(time.DateOnly), shipped.NotAfter.UTC().Format(time.DateOnly))
			}
		})
	}
}
