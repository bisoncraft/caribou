// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// selfSignedDER generates a throwaway self-signed certificate and returns its
// DER bytes.
func selfSignedDER(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31-1, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return der
}

func TestServerTLSConfigPinned(t *testing.T) {
	der := selfSignedDER(t)
	other := selfSignedDER(t)

	cfg := serverTLSConfig(&ServerAddr{Proto: "ssl", Addr: "host:50002", Cert: der}, "host")
	if !cfg.InsecureSkipVerify {
		t.Fatal("pinned mode should disable default chain verification")
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("pinned mode should set VerifyPeerCertificate")
	}
	if cfg.ServerName != "host" {
		t.Fatalf("ServerName = %q, want host", cfg.ServerName)
	}
	if err := cfg.VerifyPeerCertificate([][]byte{der}, nil); err != nil {
		t.Fatalf("matching pinned cert should verify: %v", err)
	}
	if err := cfg.VerifyPeerCertificate([][]byte{other}, nil); err == nil {
		t.Fatal("non-matching cert should be rejected")
	}
	if err := cfg.VerifyPeerCertificate(nil, nil); err == nil {
		t.Fatal("missing cert should be rejected")
	}
}

func TestServerTLSConfigSkip(t *testing.T) {
	cfg := serverTLSConfig(&ServerAddr{Proto: "ssl", Addr: "host:993", SkipVerify: true}, "host")
	if !cfg.InsecureSkipVerify {
		t.Fatal("skip mode should set InsecureSkipVerify")
	}
	if cfg.VerifyPeerCertificate != nil {
		t.Fatal("skip mode should not set VerifyPeerCertificate")
	}
}

func TestServerTLSConfigCAVerify(t *testing.T) {
	cfg := serverTLSConfig(&ServerAddr{Proto: "ssl", Addr: "host:700"}, "host")
	if cfg.InsecureSkipVerify {
		t.Fatal("default mode should verify against system roots")
	}
	if cfg.VerifyPeerCertificate != nil {
		t.Fatal("default mode should not set VerifyPeerCertificate")
	}
	if cfg.RootCAs != nil {
		t.Fatal("default mode should leave RootCAs nil (system default)")
	}
	if cfg.ServerName != "host" {
		t.Fatalf("ServerName = %q, want host", cfg.ServerName)
	}
}

func TestMustParseCertPEM(t *testing.T) {
	der := selfSignedDER(t)
	good := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if got := MustParseCertPEM(good); !bytes.Equal(got, der) {
		t.Fatal("MustParseCertPEM should return the leaf DER")
	}

	bad := []string{
		"",
		"not a pem block",
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),                  // wrong block type
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a cert")})), // unparseable
	}
	for _, in := range bad {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("MustParseCertPEM(%q) should panic", in)
				}
			}()
			MustParseCertPEM(in)
		}()
	}
}
