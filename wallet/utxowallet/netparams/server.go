// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package netparams

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
)

// ServerAddr identifies an electrumx server endpoint.
type ServerAddr struct {
	// Proto is "ssl" or "tcp".
	Proto string
	// Addr is the host:port endpoint.
	Addr string
	// Onion is true if Addr is a Tor hidden service.
	Onion bool
	// Cert, when set, is the DER-encoded leaf certificate this ssl server is
	// pinned to: the certificate the server presents must byte-match it. Use it
	// for servers with self-signed certificates that cannot be validated against
	// a public CA. Mutually exclusive with SkipVerify.
	Cert []byte
	// SkipVerify disables TLS certificate verification entirely. Use it ONLY for
	// dev/testnet/simnet servers with throwaway self-signed certificates. When
	// neither Cert nor SkipVerify is set, an ssl server's certificate is
	// verified against the system CA roots.
	SkipVerify bool
}

// MustParseCertPEM decodes a single PEM-encoded X.509 certificate and returns
// its raw DER bytes, suitable for ServerAddr.Cert. It panics on malformed input,
// which is appropriate for the hard-coded pinned certificates decoded at package
// initialization — a bad constant should fail loudly at startup, not at connect.
func MustParseCertPEM(pemData string) []byte {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil || block.Type != "CERTIFICATE" {
		panic("netparams: pinned certificate is not valid PEM")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		panic(fmt.Sprintf("netparams: pinned certificate does not parse: %v", err))
	}
	return block.Bytes
}

// Network satisfies net.Addr.
func (a *ServerAddr) Network() string { return a.Proto }

// String satisfies net.Addr.
func (a *ServerAddr) String() string { return a.Addr }

// Equal reports whether two server addresses are the same.
func (a *ServerAddr) Equal(b *ServerAddr) bool {
	return a.Proto == b.Proto && a.Addr == b.Addr
}

// Validate sanity-checks a ServerAddr.
func (a *ServerAddr) Validate() error {
	switch a.Proto {
	case "ssl", "tcp":
	default:
		return fmt.Errorf("netparams: unknown proto %q (want ssl or tcp)", a.Proto)
	}
	if _, _, err := net.SplitHostPort(a.Addr); err != nil {
		return fmt.Errorf("netparams: invalid addr %q: %w", a.Addr, err)
	}
	return nil
}
