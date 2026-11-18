// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Certificate pinning for electrum servers whose certificates cannot be
// verified against a public CA.
//
// A server is authenticated one of three ways, in order of precedence:
//
//  1. A shipped pin (ServerAddr.Cert) that has not expired: the server's key
//     must be the shipped certificate's key.
//  2. CA verification, for ssl servers that set neither Cert nor SkipVerify.
//  3. A learned pin (trust on first use), for SkipVerify servers on mainnet
//     and for shipped pins past their expiry: the first certificate a server
//     presents is pinned by its key once the server passes the genesis
//     check, and later connections must present the same key.
//
// Pins compare the SHA-256 of the leaf's SubjectPublicKeyInfo, so a server
// that renews its certificate with the same key keeps its identity. A
// connection that presents the pinned key and completes the handshake proves
// possession of the key; its certificate's expiry extends the pin's (never
// shortens it). A different key is accepted only once local time is past the
// pinned expiry, and then is pinned in turn. A learned pin authenticates
// continuity with the first connection, not an operator: a man in the middle
// at first contact, or at the first contact after a pin expires, is pinned
// like the real server.
//
// Testnet SkipVerify servers stay unauthenticated (their coins carry no
// value), and loopback endpoints are always exempt, since a local test
// harness regenerates its certificate on every rebuild.

// errCertPinMismatch is returned (wrapped) when a server presents a key other
// than the one pinned for it, or loses a race to pin a different key.
var errCertPinMismatch = errors.New("electrum: server certificate key does not match its pin")

// errCertNotCurrent is returned (wrapped) when a certificate offered for a
// new pin is expired or not yet valid. Certificate validation is otherwise
// off for these servers, so the check is explicit.
var errCertNotCurrent = errors.New("electrum: server certificate is not currently valid")

// certPin is a learned TLS identity: the SHA-256 of a leaf certificate's
// SubjectPublicKeyInfo, the latest NotAfter seen on a certificate carrying
// that key, and when the key was first pinned.
type certPin struct {
	spki     [sha256.Size]byte
	notAfter time.Time
	pinnedAt time.Time
}

func (c certPin) isZero() bool { return c.pinnedAt.IsZero() }

// activeAt reports whether the pin binds the server at now.
func (c certPin) activeAt(now time.Time) bool {
	return !c.isZero() && !now.After(c.notAfter)
}

func (c certPin) equal(o certPin) bool {
	return c.spki == o.spki && c.notAfter.Equal(o.notAfter) && c.pinnedAt.Equal(o.pinnedAt)
}

// spkiHash returns the pin value for cert.
func spkiHash(cert *x509.Certificate) [sha256.Size]byte {
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo)
}

// normalizeAddr returns the canonical spelling of a host:port endpoint, so
// one server cannot hold two identities (two pins, two reputations) under
// different spellings: the host lowercased with any trailing dot removed, IP
// literals in canonical form (IPv4-mapped IPv6 unmapped), and the port in
// decimal without leading zeros. An unparseable endpoint is returned
// lowercased.
func normalizeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.ToLower(addr)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	}
	if n, err := strconv.ParseUint(port, 10, 16); err == nil {
		port = strconv.FormatUint(n, 10)
	}
	return net.JoinHostPort(host, port)
}

// isLoopbackLiteral reports whether a normalized endpoint names the local
// host by its literal spelling: an IP literal in 127.0.0.0/8 or ::1, or
// "localhost". Nothing is resolved, so a server cannot make itself look
// local.
func isLoopbackLiteral(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// tofuEndpoint reports whether trust on first use applies to a server at the
// normalized endpoint addr: mainnet only, and never a loopback literal. A
// variable so tests can enable it for loopback fakes.
var tofuEndpoint = func(addr string, mainnet bool) bool {
	return mainnet && !isLoopbackLiteral(addr)
}

// pinMode is how one dial authenticates a server's certificate.
type pinMode int

const (
	// pinNone runs no pin check: tcp, CA verification, or an exempt
	// SkipVerify server.
	pinNone pinMode = iota
	// pinShipped requires the key of the server's shipped certificate.
	pinShipped
	// pinLearned requires the key of the server's active learned pin.
	pinLearned
	// pinEnroll accepts any currently valid certificate and pins its key
	// once the server passes the genesis check.
	pinEnroll
)

// tlsPolicy is the certificate check for one dial, with the learned pin
// state the dial started from, which recordPin compares against before
// committing anything.
type tlsPolicy struct {
	mode         pinMode
	want         [sha256.Size]byte
	wantNotAfter time.Time
	snapshot     certPin
	now          time.Time
	// learnable records whether learned pins apply to the endpoint, so
	// recordPin knows to record a shipped key's continuity.
	learnable bool
}

// shippedPolicy is the check for a shipped pin with no learned state to fall
// back on (a dial outside the server pool): the shipped key, whatever its
// expiry.
func shippedPolicy(addr *ServerAddr) *tlsPolicy {
	if len(addr.Cert) == 0 {
		return &tlsPolicy{mode: pinNone}
	}
	cert, err := x509.ParseCertificate(addr.Cert)
	if err != nil {
		// MustParseCertPEM vets shipped constants at init; a pin that
		// still does not parse matches nothing.
		return &tlsPolicy{mode: pinShipped}
	}
	return &tlsPolicy{mode: pinShipped, want: spkiHash(cert), wantNotAfter: cert.NotAfter}
}

// verify returns the tls.Config.VerifyConnection hook enforcing the policy.
// VerifyConnection, unlike VerifyPeerCertificate, also runs on resumed
// sessions.
func (p *tlsPolicy) verify(addr string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("electrum: %s presented no certificate", addr)
		}
		leaf := cs.PeerCertificates[0]
		got := spkiHash(leaf)
		switch p.mode {
		case pinShipped, pinLearned:
			if got != p.want {
				return fmt.Errorf("%w: %s presented key %x, pinned key %x (pinned until %s)",
					errCertPinMismatch, addr, got, p.want, p.wantNotAfter.UTC().Format(time.RFC3339))
			}
		case pinEnroll:
			if p.now.Before(leaf.NotBefore) || p.now.After(leaf.NotAfter) {
				return fmt.Errorf("%w: %s certificate is valid from %s to %s, local time %s",
					errCertNotCurrent, addr, leaf.NotBefore.UTC().Format(time.RFC3339),
					leaf.NotAfter.UTC().Format(time.RFC3339), p.now.UTC().Format(time.RFC3339))
			}
		}
		return nil
	}
}

// encodePinHex returns the stored form of a pin value.
func encodePinHex(spki [sha256.Size]byte) string { return hex.EncodeToString(spki[:]) }

// decodePinHex parses the stored form of a pin value.
func decodePinHex(s string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != sha256.Size {
		return out, fmt.Errorf("%d bytes, want %d", len(b), sha256.Size)
	}
	copy(out[:], b)
	return out, nil
}

// atomicWriteFile replaces path with data so that a crash leaves either the
// old file or the new one, never a truncated mix: the data goes to a temporary
// file in the same directory, which is synced and then renamed over path, and
// the directory is synced so the rename survives a crash.
func atomicWriteFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// Directory sync is unsupported on some platforms; the rename has
	// already happened, so a failure here only weakens crash durability.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
