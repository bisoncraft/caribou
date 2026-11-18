// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
)

// testCert is a self-signed certificate for key, valid over [from, until].
type testCert struct {
	tls  tls.Certificate
	leaf *x509.Certificate
}

func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

var certSerial atomic.Int64

func newTestCert(t *testing.T, key *ecdsa.PrivateKey, from, until time.Time) testCert {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(certSerial.Add(1)),
		Subject:      pkix.Name{CommonName: "electrum test"},
		NotBefore:    from,
		NotAfter:     until,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return testCert{tls: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf: leaf}
}

// presenter is a fake TLS server's certificate source: whatever is current.
type presenter struct {
	mtx  sync.Mutex
	cert testCert
}

func (p *presenter) set(c testCert) {
	p.mtx.Lock()
	p.cert = c
	p.mtx.Unlock()
}

func (p *presenter) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	c := p.cert.tls
	return &c, nil
}

// enableLoopbackTOFU applies trust on first use to loopback fakes for the
// test's lifetime.
func enableLoopbackTOFU(t *testing.T) {
	t.Helper()
	old := tofuEndpoint
	tofuEndpoint = func(_ string, mainnet bool) bool { return mainnet }
	t.Cleanup(func() { tofuEndpoint = old })
}

// pinnedNetwork builds an unstarted mainnet network whose only server is sv,
// with its data under dataDir and the pool clock reading *clock.
func pinnedNetwork(t *testing.T, params *netparams.ChainParams, sv *ServerAddr,
	dataDir string, clock *time.Time) *Network {

	t.Helper()
	params.Name = "mainnet"
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	n, err := NewNetwork(&Config{Chain: chainCfg, TrustedServer: sv, DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	n.ctx, n.cancel = context.WithCancel(t.Context())
	t.Cleanup(n.cancel)
	n.pool.now = func() time.Time { return *clock }
	return n
}

// dialOnce dials the network's server through dialServer, closing any node.
func dialOnce(t *testing.T, n *Network, sv *ServerAddr) error {
	t.Helper()
	node, err := n.dialServer(&ServerAddr{Proto: sv.Proto, Addr: normalizeAddr(sv.Addr),
		SkipVerify: sv.SkipVerify, Cert: sv.Cert})
	if node != nil {
		node.Close()
	}
	return err
}

// storedPin returns the pin for addr in the pool database on disk.
func storedPin(t *testing.T, dataDir, addr string) certPin {
	t.Helper()
	p, err := newServerPool(filepath.Join(dataDir, "electrum", "btc"), nil)
	if err != nil {
		t.Fatalf("reload pool: %v", err)
	}
	e := p.entries[normalizeAddr(addr)]
	if e == nil {
		return certPin{}
	}
	return e.pin()
}

func (n *Network) pinFor(addr string) certPin {
	n.pool.mtx.Lock()
	defer n.pool.mtx.Unlock()
	if e := n.pool.entries[normalizeAddr(addr)]; e != nil {
		return e.pin()
	}
	return certPin{}
}

// TestTOFURotation drives a fake TLS server through the life of a learned
// pin: first use pins the key; the same key renewed early is continuity and
// extends the pin; an older certificate with that key never shortens it; a
// new key is refused before the pinned expiry and pinned after it.
func TestTOFURotation(t *testing.T) {
	enableLoopbackTOFU(t)
	resetServerHealth()
	now := time.Now()
	clock := now
	keyA, keyB := newTestKey(t), newTestKey(t)
	a1 := newTestCert(t, keyA, now.Add(-time.Hour), now.Add(24*time.Hour))
	a2 := newTestCert(t, keyA, now.Add(-time.Hour), now.Add(48*time.Hour))
	b1 := newTestCert(t, keyB, now.Add(-time.Hour), now.Add(96*time.Hour))
	pres := &presenter{cert: a1}
	f, params := newFakeTLSServer(t, 3, pres.get)
	dataDir := t.TempDir()
	sv := f.addr()
	n := pinnedNetwork(t, params, sv, dataDir, &clock)

	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if got := n.pinFor(sv.Addr); got.spki != spkiHash(a1.leaf) || !got.notAfter.Equal(a1.leaf.NotAfter) {
		t.Fatal("first use did not pin the presented key")
	}
	if got := storedPin(t, dataDir, sv.Addr); got.spki != spkiHash(a1.leaf) {
		t.Fatal("first-use pin not persisted")
	}

	pres.set(a2) // the same key, renewed early
	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("same key renewed early: %v", err)
	}
	if got := n.pinFor(sv.Addr); !got.notAfter.Equal(a2.leaf.NotAfter) {
		t.Fatalf("pin expiry %s, want the renewal's %s", got.notAfter, a2.leaf.NotAfter)
	}

	pres.set(a1) // an older certificate with the same key
	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("older certificate with the pinned key: %v", err)
	}
	if got := n.pinFor(sv.Addr); !got.notAfter.Equal(a2.leaf.NotAfter) {
		t.Fatal("an older certificate shortened the pin")
	}

	pres.set(b1) // a new key before the pinned expiry
	if err := dialOnce(t, n, sv); !errors.Is(err, errCertPinMismatch) {
		t.Fatalf("new key before expiry: got %v, want errCertPinMismatch", err)
	}
	if got := n.pinFor(sv.Addr); got.spki != spkiHash(a1.leaf) {
		t.Fatal("refused key changed the pin")
	}

	clock = a2.leaf.NotAfter.Add(time.Hour) // past the pinned expiry
	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("new key after expiry: %v", err)
	}
	if got := storedPin(t, dataDir, sv.Addr); got.spki != spkiHash(b1.leaf) {
		t.Fatal("new key not pinned after the old expiry")
	}

	// A certificate offered for a new pin must be currently valid.
	clock = b1.leaf.NotAfter.Add(time.Hour)
	expired := newTestCert(t, newTestKey(t), now.Add(-2*time.Hour), now.Add(-time.Hour))
	pres.set(expired)
	if err := dialOnce(t, n, sv); !errors.Is(err, errCertNotCurrent) {
		t.Fatalf("expired certificate offered for enrollment: got %v, want errCertNotCurrent", err)
	}
}

// TestTOFUConcurrentEnrollment: two dials that both start unpinned and
// handshake with different keys commit as a compare-and-set. Exactly one key
// is pinned; the other dial is refused and its node never handed out.
func TestTOFUConcurrentEnrollment(t *testing.T) {
	enableLoopbackTOFU(t)
	resetServerHealth()
	now := time.Now()
	clock := now
	certs := []testCert{
		newTestCert(t, newTestKey(t), now.Add(-time.Hour), now.Add(24*time.Hour)),
		newTestCert(t, newTestKey(t), now.Add(-time.Hour), now.Add(24*time.Hour)),
	}
	// Each handshake waits until both have begun, so both dials snapshot
	// the unpinned state before either commits.
	var arrived atomic.Int32
	both := make(chan struct{})
	getCert := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		i := arrived.Add(1)
		if i == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(5 * time.Second):
		}
		c := certs[(i-1)%2].tls
		return &c, nil
	}
	f, params := newFakeTLSServer(t, 3, getCert)
	sv := f.addr()
	n := pinnedNetwork(t, params, sv, t.TempDir(), &clock)

	type result struct {
		node *Node
		err  error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			node, err := n.dialServer(&ServerAddr{Proto: "ssl", Addr: normalizeAddr(sv.Addr), SkipVerify: true})
			results <- result{node, err}
		}()
	}
	var won *Node
	var lost error
	for range 2 {
		r := <-results
		switch {
		case r.err == nil && won == nil:
			won = r.node
		case r.err != nil && lost == nil:
			lost = r.err
		default:
			t.Fatalf("want one success and one refusal; got node %v err %v", r.node, r.err)
		}
	}
	defer won.Close()
	if !errors.Is(lost, errCertPinMismatch) {
		t.Fatalf("losing dial: got %v, want errCertPinMismatch", lost)
	}
	if got := n.pinFor(sv.Addr); got.spki != spkiHash(won.peerLeaf()) {
		t.Fatal("pinned key is not the winning dial's")
	}
}

// TestPinPersistFailureAndRestart: a first-use pin that cannot be written
// refuses the connection and leaves nothing pinned, before and after a
// restart. A same-key renewal that cannot be written keeps the connection,
// holds the later expiry in memory (so no dial reopens first use on the stale
// one), and is written on a later connection; a restart in between sees the
// older expiry.
func TestPinPersistFailureAndRestart(t *testing.T) {
	enableLoopbackTOFU(t)
	resetServerHealth()
	now := time.Now()
	clock := now
	key := newTestKey(t)
	c1 := newTestCert(t, key, now.Add(-time.Hour), now.Add(24*time.Hour))
	c2 := newTestCert(t, key, now.Add(-time.Hour), now.Add(72*time.Hour))
	pres := &presenter{cert: c1}
	f, params := newFakeTLSServer(t, 3, pres.get)
	dataDir := t.TempDir()
	sv := f.addr()
	n := pinnedNetwork(t, params, sv, dataDir, &clock)
	failing := func(string, []byte) error { return errors.New("injected write failure") }
	setWriter := func(w func(string, []byte) error) {
		n.pool.mtx.Lock()
		n.pool.writeFile = w
		n.pool.mtx.Unlock()
	}

	setWriter(failing)
	if err := dialOnce(t, n, sv); err == nil || errors.Is(err, errCertPinMismatch) {
		t.Fatalf("first use with a failing write: got %v, want a persistence error", err)
	}
	if !n.pinFor(sv.Addr).isZero() {
		t.Fatal("unwritten first-use pin left in memory")
	}
	if !storedPin(t, dataDir, sv.Addr).isZero() {
		t.Fatal("unwritten first-use pin found after restart")
	}

	setWriter(atomicWriteFile)
	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("first use: %v", err)
	}
	setWriter(failing)
	pres.set(c2)
	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("same-key renewal with a failing write must keep the connection: %v", err)
	}
	if got := n.pinFor(sv.Addr); !got.notAfter.Equal(c2.leaf.NotAfter) {
		t.Fatal("renewed expiry not held in memory")
	}
	// Past the stored expiry but within the renewal, a new key is still
	// refused: the in-memory expiry governs.
	clock = c1.leaf.NotAfter.Add(time.Hour)
	pres.set(newTestCert(t, newTestKey(t), now, now.Add(96*time.Hour)))
	if err := dialOnce(t, n, sv); !errors.Is(err, errCertPinMismatch) {
		t.Fatalf("new key within the renewed expiry: got %v, want errCertPinMismatch", err)
	}
	if got := storedPin(t, dataDir, sv.Addr); !got.notAfter.Equal(c1.leaf.NotAfter) {
		t.Fatal("restart before the retry should see the older expiry")
	}

	setWriter(atomicWriteFile)
	pres.set(c1)
	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("retry connection: %v", err)
	}
	if got := storedPin(t, dataDir, sv.Addr); !got.notAfter.Equal(c2.leaf.NotAfter) {
		t.Fatal("renewed expiry not written on the next connection")
	}
}

// TestShippedPinUpgrade: an install whose stored entry for a server is a
// SkipVerify one with a learned pin picks up a newly shipped certificate. The
// unexpired shipped pin wins over the learned one; once it expires, with no
// connection having presented the shipped key, the still-valid learned pin
// wins (as a continuation, not a new enrollment); once both expire, a new key
// is pinned and survives a restart.
func TestShippedPinUpgrade(t *testing.T) {
	enableLoopbackTOFU(t)
	resetServerHealth()
	now := time.Now()
	clock := now
	learnedKey, shippedKey, laterKey := newTestKey(t), newTestKey(t), newTestKey(t)
	learned := newTestCert(t, learnedKey, now.Add(-time.Hour), now.Add(240*time.Hour))
	shipped := newTestCert(t, shippedKey, now.Add(-time.Hour), now.Add(48*time.Hour))
	pres := &presenter{cert: learned}
	f, params := newFakeTLSServer(t, 3, pres.get)
	dataDir := t.TempDir()
	poolDir := filepath.Join(dataDir, "electrum", "btc")
	if err := os.MkdirAll(poolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	addr := normalizeAddr(f.addr().Addr)

	// The stored database from before the release: a SkipVerify entry
	// with a learned pin.
	old, err := newServerPool(poolDir, []*ServerAddr{{Proto: "ssl", Addr: addr, SkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	old.mtx.Lock()
	old.entries[addr].setPin(certPin{spki: spkiHash(learned.leaf), notAfter: learned.leaf.NotAfter, pinnedAt: now})
	if err := old.saveLocked(); err != nil {
		t.Fatal(err)
	}
	old.mtx.Unlock()

	sv := &ServerAddr{Proto: "ssl", Addr: addr, Cert: shipped.tls.Certificate[0]}
	n := pinnedNetwork(t, params, sv, dataDir, &clock)
	n.pool.mtx.Lock()
	cfg := n.pool.entries[addr].Server
	n.pool.mtx.Unlock()
	if len(cfg.Cert) == 0 || cfg.SkipVerify {
		t.Fatal("stored SkipVerify configuration survived the shipped pin")
	}

	pres.set(learned)
	if err := dialOnce(t, n, sv); !errors.Is(err, errCertPinMismatch) {
		t.Fatalf("learned key under an unexpired shipped pin: got %v, want errCertPinMismatch", err)
	}

	clock = shipped.leaf.NotAfter.Add(time.Hour) // shipped expired, learned valid
	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("learned key after the shipped pin expired: %v", err)
	}
	if got := n.pinFor(addr); got.spki != spkiHash(learned.leaf) || !got.pinnedAt.Equal(now) {
		t.Fatal("learned key re-enrolled instead of continuing its pin")
	}
	pres.set(shipped)
	if err := dialOnce(t, n, sv); !errors.Is(err, errCertPinMismatch) {
		t.Fatalf("shipped key after it expired, with a valid learned pin: got %v, want errCertPinMismatch", err)
	}

	clock = learned.leaf.NotAfter.Add(time.Hour) // both expired
	later := newTestCert(t, laterKey, clock.Add(-time.Hour), clock.Add(240*time.Hour))
	pres.set(later)
	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("new key after both pins expired: %v", err)
	}
	restarted := pinnedNetwork(t, params, sv, dataDir, &clock)
	if got := restarted.pinFor(addr); got.spki != spkiHash(later.leaf) {
		t.Fatal("re-pin did not survive a restart")
	}
	if err := dialOnce(t, restarted, sv); err != nil {
		t.Fatalf("re-pinned key after restart: %v", err)
	}
}

// TestShippedKeyContinuity: a connection presenting the shipped key records it
// as the server's learned pin, replacing a stale pin for an older key, so a
// certificate renewed for the shipped key carries the server's identity past
// the shipped certificate's expiry and across a restart. Neither a new key nor
// the replaced old key is accepted until the renewal expires, and recording
// the renewal survives a failed write.
func TestShippedKeyContinuity(t *testing.T) {
	enableLoopbackTOFU(t)
	resetServerHealth()
	now := time.Now()
	clock := now
	oldKey, shippedKey := newTestKey(t), newTestKey(t)
	stale := newTestCert(t, oldKey, now.Add(-time.Hour), now.Add(240*time.Hour))
	shipped := newTestCert(t, shippedKey, now.Add(-time.Hour), now.Add(24*time.Hour))
	renewed := newTestCert(t, shippedKey, now.Add(-time.Hour), now.Add(72*time.Hour))
	pres := &presenter{cert: renewed}
	f, params := newFakeTLSServer(t, 3, pres.get)
	dataDir := t.TempDir()
	poolDir := filepath.Join(dataDir, "electrum", "btc")
	if err := os.MkdirAll(poolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	addr := normalizeAddr(f.addr().Addr)
	old, err := newServerPool(poolDir, []*ServerAddr{{Proto: "ssl", Addr: addr, SkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	old.mtx.Lock()
	old.entries[addr].setPin(certPin{spki: spkiHash(stale.leaf), notAfter: stale.leaf.NotAfter, pinnedAt: now})
	if err := old.saveLocked(); err != nil {
		t.Fatal(err)
	}
	old.mtx.Unlock()

	sv := &ServerAddr{Proto: "ssl", Addr: addr, Cert: shipped.tls.Certificate[0]}
	n := pinnedNetwork(t, params, sv, dataDir, &clock)
	// The renewal arrives while the shipped certificate is still valid,
	// with persistence failing: the connection stands and the pin is held
	// in memory.
	n.pool.mtx.Lock()
	n.pool.writeFile = func(string, []byte) error { return errors.New("injected write failure") }
	n.pool.mtx.Unlock()
	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("renewed shipped key with a failing write: %v", err)
	}
	if got := n.pinFor(addr); got.spki != spkiHash(renewed.leaf) || !got.notAfter.Equal(renewed.leaf.NotAfter) {
		t.Fatal("shipped key continuity not recorded")
	}
	n.pool.mtx.Lock()
	n.pool.writeFile = atomicWriteFile
	n.pool.mtx.Unlock()
	if err := dialOnce(t, n, sv); err != nil {
		t.Fatalf("renewed shipped key: %v", err)
	}

	clock = shipped.leaf.NotAfter.Add(time.Hour) // shipped expired, renewal valid
	restarted := pinnedNetwork(t, params, sv, dataDir, &clock)
	if got := restarted.pinFor(addr); got.spki != spkiHash(renewed.leaf) {
		t.Fatal("shipped key continuity did not survive a restart")
	}
	if err := dialOnce(t, restarted, sv); err != nil {
		t.Fatalf("renewed shipped key after the shipped expiry: %v", err)
	}
	pres.set(stale)
	if err := dialOnce(t, restarted, sv); !errors.Is(err, errCertPinMismatch) {
		t.Fatalf("replaced old key after the shipped expiry: got %v, want errCertPinMismatch", err)
	}
	pres.set(newTestCert(t, newTestKey(t), now, now.Add(240*time.Hour)))
	if err := dialOnce(t, restarted, sv); !errors.Is(err, errCertPinMismatch) {
		t.Fatalf("new key within the renewal: got %v, want errCertPinMismatch", err)
	}
}

// TestShippedCommitAtExpiry: a dial that began under a shipped pin and
// commits after the pin expired is judged by the state at commit time. A key
// another dial pinned meanwhile, or a still-active learned pin for another
// key, binds the server and the late dial is refused; a concurrent pin for
// the shipped key merges to the later expiry; an unchanged, inactive pin is
// replaced by the shipped key.
func TestShippedCommitAtExpiry(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second)
	shippedKey := newTestKey(t)
	shipped := newTestCert(t, shippedKey, start.Add(-time.Hour), start.Add(time.Minute))
	renewed := newTestCert(t, shippedKey, start.Add(-time.Hour), start.Add(24*time.Hour))
	other := newTestCert(t, newTestKey(t), start.Add(-time.Hour), start.Add(24*time.Hour))
	sv := &ServerAddr{Proto: "ssl", Addr: "a:1", Cert: shipped.tls.Certificate[0]}
	setup := func(learned certPin) (*serverPool, *time.Time, *tlsPolicy) {
		now := start
		p := mustServerPool(t, t.TempDir(), []*ServerAddr{sv})
		p.now = func() time.Time { return now }
		p.entries["a:1"].setPin(learned)
		slow := p.tlsPolicy(sv, true)
		if slow.mode != pinShipped {
			t.Fatalf("fixture: mode %d before the shipped expiry", slow.mode)
		}
		now = start.Add(2 * time.Minute) // the shipped pin expires mid-handshake
		return p, &now, slow
	}

	// Another dial enrolls a different key after the expiry; the late
	// shipped dial loses.
	p, _, slow := setup(certPin{})
	if err := p.recordPin("a:1", p.tlsPolicy(sv, true), other.leaf); err != nil {
		t.Fatalf("post-expiry enrollment: %v", err)
	}
	if err := p.recordPin("a:1", slow, shipped.leaf); !errors.Is(err, errCertPinMismatch) {
		t.Fatalf("late shipped commit over a concurrent pin: got %v, want errCertPinMismatch", err)
	}
	if got := p.entries["a:1"].pin(); got.spki != spkiHash(other.leaf) {
		t.Fatal("late shipped commit replaced the concurrent winner")
	}

	// The same, with the wall clock stepped back before the shipped expiry
	// by the time the late dial commits: the changed state still refuses
	// it.
	p, now, slow := setup(certPin{})
	if err := p.recordPin("a:1", p.tlsPolicy(sv, true), other.leaf); err != nil {
		t.Fatalf("post-expiry enrollment: %v", err)
	}
	*now = start
	if err := p.recordPin("a:1", slow, shipped.leaf); !errors.Is(err, errCertPinMismatch) {
		t.Fatalf("late shipped commit after a clock step: got %v, want errCertPinMismatch", err)
	}

	// An unchanged learned pin for another key, still active once the
	// shipped pin has expired, binds the server.
	learned := certPin{spki: spkiHash(other.leaf), notAfter: other.leaf.NotAfter, pinnedAt: start.Add(-time.Hour)}
	p, _, slow = setup(learned)
	if err := p.recordPin("a:1", slow, renewed.leaf); !errors.Is(err, errCertPinMismatch) {
		t.Fatalf("late shipped commit over an active learned pin: got %v, want errCertPinMismatch", err)
	}
	if got := p.entries["a:1"].pin(); !got.equal(learned) {
		t.Fatal("active learned pin changed")
	}

	// A concurrent dial pinned the shipped key with a later expiry.
	p, _, slow = setup(certPin{})
	if err := p.recordPin("a:1", p.tlsPolicy(sv, true), renewed.leaf); err != nil {
		t.Fatalf("post-expiry enrollment of the shipped key: %v", err)
	}
	if err := p.recordPin("a:1", slow, shipped.leaf); err != nil {
		t.Fatalf("late shipped commit of the same key: %v", err)
	}
	if got := p.entries["a:1"].pin(); !got.notAfter.Equal(renewed.leaf.NotAfter) {
		t.Fatal("same-key merge lost the later expiry")
	}

	// An unchanged pin no longer active is replaced by the shipped key.
	stale := certPin{spki: [32]byte{3}, notAfter: start.Add(time.Second), pinnedAt: start.Add(-time.Hour)}
	p, now, slow = setup(stale)
	if err := p.recordPin("a:1", slow, renewed.leaf); err != nil {
		t.Fatalf("late shipped commit over an expired pin: %v", err)
	}
	if got := p.entries["a:1"].pin(); got.spki != spkiHash(renewed.leaf) || !got.activeAt(*now) {
		t.Fatal("shipped key not recorded over an expired pin")
	}

	// Before the expiry, an active learned pin for another key is
	// replaced (the shipped key's continuity).
	p = mustServerPool(t, t.TempDir(), []*ServerAddr{sv})
	p.now = func() time.Time { return start }
	p.entries["a:1"].setPin(learned)
	if err := p.recordPin("a:1", p.tlsPolicy(sv, true), shipped.leaf); err != nil {
		t.Fatalf("shipped commit before the expiry: %v", err)
	}
	if got := p.entries["a:1"].pin(); got.spki != spkiHash(shipped.leaf) {
		t.Fatal("shipped key did not replace the learned pin before the expiry")
	}
}

// TestTLSPolicyPrecedence pins the check each server configuration gets.
func TestTLSPolicyPrecedence(t *testing.T) {
	now := time.Now()
	key := newTestKey(t)
	shippedLive := newTestCert(t, key, now.Add(-time.Hour), now.Add(time.Hour))
	shippedDead := newTestCert(t, key, now.Add(-2*time.Hour), now.Add(-time.Hour))
	active := certPin{spki: [32]byte{1}, notAfter: now.Add(time.Hour), pinnedAt: now.Add(-time.Hour)}
	expired := certPin{spki: [32]byte{2}, notAfter: now.Add(-time.Minute), pinnedAt: now.Add(-time.Hour)}
	for _, tc := range []struct {
		name      string
		sv        ServerAddr
		pin       certPin
		learnable bool
		want      pinMode
	}{
		{"tcp", ServerAddr{Proto: "tcp", Addr: "a:1"}, certPin{}, true, pinNone},
		{"CA", ServerAddr{Proto: "ssl", Addr: "a:1"}, active, true, pinNone},
		{"skip, testnet or loopback", ServerAddr{Proto: "ssl", Addr: "a:1", SkipVerify: true}, active, false, pinNone},
		{"skip, active pin", ServerAddr{Proto: "ssl", Addr: "a:1", SkipVerify: true}, active, true, pinLearned},
		{"skip, expired pin", ServerAddr{Proto: "ssl", Addr: "a:1", SkipVerify: true}, expired, true, pinEnroll},
		{"skip, first use", ServerAddr{Proto: "ssl", Addr: "a:1", SkipVerify: true}, certPin{}, true, pinEnroll},
		{"shipped, unexpired", ServerAddr{Proto: "ssl", Addr: "a:1", Cert: shippedLive.tls.Certificate[0]}, active, true, pinShipped},
		{"shipped, expired, active pin", ServerAddr{Proto: "ssl", Addr: "a:1", Cert: shippedDead.tls.Certificate[0]}, active, true, pinLearned},
		{"shipped, expired, no pin", ServerAddr{Proto: "ssl", Addr: "a:1", Cert: shippedDead.tls.Certificate[0]}, certPin{}, true, pinEnroll},
		{"shipped, expired, not learnable", ServerAddr{Proto: "ssl", Addr: "a:1", Cert: shippedDead.tls.Certificate[0]}, active, false, pinShipped},
	} {
		p := mustServerPool(t, t.TempDir(), []*ServerAddr{&tc.sv})
		p.now = func() time.Time { return now }
		p.entries["a:1"].setPin(tc.pin)
		if got := p.tlsPolicy(&tc.sv, tc.learnable).mode; got != tc.want {
			t.Errorf("%s: mode %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestRecordPinCompareAndSet exercises recordPin's compare-and-set directly.
func TestRecordPinCompareAndSet(t *testing.T) {
	now := time.Now()
	a := newTestCert(t, newTestKey(t), now.Add(-time.Hour), now.Add(time.Hour))
	b := newTestCert(t, newTestKey(t), now.Add(-time.Hour), now.Add(time.Hour))
	p := mustServerPool(t, t.TempDir(), []*ServerAddr{{Proto: "ssl", Addr: "a:1", SkipVerify: true}})
	p.now = func() time.Time { return now }
	snap := &tlsPolicy{mode: pinEnroll, now: now} // both dials saw nothing pinned

	if err := p.recordPin("a:1", snap, a.leaf); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if err := p.recordPin("a:1", snap, b.leaf); !errors.Is(err, errCertPinMismatch) {
		t.Fatalf("racing commit with another key: got %v, want errCertPinMismatch", err)
	}
	saves := p.saveCount
	if err := p.recordPin("a:1", snap, a.leaf); err != nil {
		t.Fatalf("racing commit with the winning key: %v", err)
	}
	if p.saveCount != saves {
		t.Fatal("a racing commit with the winning key and no later expiry wrote the database")
	}

	// Racing commits of one key with different expiries keep the later
	// one, whichever lands first, and a new key stays refused until it.
	key := newTestKey(t)
	short := newTestCert(t, key, now.Add(-time.Hour), now.Add(time.Hour))
	long := newTestCert(t, key, now.Add(-time.Hour), now.Add(24*time.Hour))
	q := mustServerPool(t, t.TempDir(), []*ServerAddr{{Proto: "ssl", Addr: "a:1", SkipVerify: true}})
	q.now = func() time.Time { return now }
	if err := q.recordPin("a:1", snap, short.leaf); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if err := q.recordPin("a:1", snap, long.leaf); err != nil {
		t.Fatalf("racing same-key commit: %v", err)
	}
	if got := q.entries["a:1"].pin(); !got.notAfter.Equal(long.leaf.NotAfter) {
		t.Fatal("racing same-key commit lost its later expiry")
	}
	q.now = func() time.Time { return short.leaf.NotAfter.Add(time.Hour) }
	if mode := q.tlsPolicy(&ServerAddr{Proto: "ssl", Addr: "a:1", SkipVerify: true}, true).mode; mode != pinLearned {
		t.Fatalf("after the shorter expiry the pin must still bind: mode %d", mode)
	}
}

// TestServerPoolLoadErrors: a database that does not parse is moved aside
// and the pool starts fresh; one that cannot be read is an error and is left
// in place.
func TestServerPoolLoadErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, serverDBName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := newServerPool(dir, []*ServerAddr{{Proto: "ssl", Addr: "ship:1"}})
	if err != nil {
		t.Fatalf("corrupt database: %v", err)
	}
	if len(p.entries) != 1 {
		t.Fatalf("%d entries after a corrupt load, want the shipped one", len(p.entries))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt database left in place")
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("%d set-aside files, want 1", len(matches))
	}
	if raw, _ := os.ReadFile(matches[0]); string(raw) != "{not json" {
		t.Fatal("set-aside file does not hold the corrupt content")
	}

	unreadable := t.TempDir()
	if err := os.Mkdir(filepath.Join(unreadable, serverDBName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := newServerPool(unreadable, nil); err == nil {
		t.Fatal("an unreadable database loaded as a fresh pool")
	}
	if fi, err := os.Stat(filepath.Join(unreadable, serverDBName)); err != nil || !fi.IsDir() {
		t.Fatal("an unreadable database was moved or replaced")
	}
}

// TestServerPoolRecordsAndMerge: a record that does not parse, or whose pin
// is malformed, loses only itself or its pin; records whose addresses collide
// after normalization merge, the earlier pin winning.
func TestServerPoolRecordsAndMerge(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	good := certPin{spki: [32]byte{7}, notAfter: now.Add(time.Hour), pinnedAt: now}
	early := certPin{spki: [32]byte{8}, notAfter: now.Add(time.Hour), pinnedAt: now.Add(-time.Hour)}
	entry := func(addr string, pin certPin, successes, failures int, quarantined time.Time) json.RawMessage {
		e := &poolEntry{Server: &ServerAddr{Proto: "ssl", Addr: addr, SkipVerify: true}, FirstSeen: now,
			LastSeen: now, Successes: successes, Failures: failures, QuarantinedUntil: quarantined}
		e.setPin(pin)
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	badPin := json.RawMessage(strings.Replace(string(entry("bad.example:1", good, 4, 0, time.Time{})),
		encodePinHex(good.spki), "zz", 1))
	records := []json.RawMessage{
		entry("good.example:1", good, 1, 0, time.Time{}),
		badPin,
		json.RawMessage(`{"server": 5}`),
		entry("Merge.Example.:050002", good, 0, 2, time.Time{}),
		entry("merge.example:50002", early, 3, 0, now.Add(time.Hour)),
		entry("Same.Example:1", certPin{spki: [32]byte{9}, notAfter: now.Add(2 * time.Hour), pinnedAt: now}, 0, 0, time.Time{}),
		entry("same.example.:1", certPin{spki: [32]byte{9}, notAfter: now.Add(time.Hour), pinnedAt: now.Add(-time.Hour)}, 0, 0, time.Time{}),
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, serverDBName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p := mustServerPool(t, dir, nil)
	if e := p.entries["good.example:1"]; e == nil || e.pin().spki != good.spki {
		t.Fatal("unaffected pin lost")
	}
	if e := p.entries["bad.example:1"]; e == nil || !e.pin().isZero() || e.Successes != 4 {
		t.Fatal("malformed pin must be dropped and the record kept")
	}
	merged := p.entries["merge.example:50002"]
	if merged == nil || len(p.entries) != 4 {
		t.Fatalf("entries %v, want four with the spellings merged", len(p.entries))
	}
	if same := p.entries["same.example:1"].pin(); !same.notAfter.Equal(now.Add(2*time.Hour)) ||
		!same.pinnedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("same-key merge %+v, want the later expiry and the earlier pin time", same)
	}
	if merged.pin().spki != early.spki || merged.Successes != 3 || merged.Failures != 2 ||
		!merged.QuarantinedUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("merged entry %+v", merged)
	}
}

// TestNormalizeAndEndpointClass pins address normalization and the literal
// loopback and mainnet classification.
func TestNormalizeAndEndpointClass(t *testing.T) {
	for in, want := range map[string]string{
		"Electrum.Example.COM.:050002": "electrum.example.com:50002",
		"[::FFFF:127.0.0.1]:80":        "127.0.0.1:80",
		"[0:0:0:0:0:0:0:1]:50002":      "[::1]:50002",
		"host:1":                       "host:1",
	} {
		if got := normalizeAddr(in); got != want {
			t.Errorf("normalizeAddr(%q) = %q, want %q", in, got, want)
		}
	}
	p := mustServerPool(t, t.TempDir(), []*ServerAddr{
		{Proto: "ssl", Addr: "Ship.Example:1"}, {Proto: "ssl", Addr: "ship.example.:01"},
	})
	if len(p.shipped) != 1 || p.shipped[0].Addr != "ship.example:1" {
		t.Errorf("shipped spellings not merged: %v", p.shipped)
	}
	for addr, applies := range map[string]bool{
		"127.0.0.1:1":        false,
		"127.9.9.9:1":        false,
		"localhost:1":        false,
		"[::1]:1":            false,
		"127.0.0.1.nip.io:1": true,
		"example.com:1":      true,
		"10.0.0.1:1":         true,
	} {
		if got := tofuEndpoint(addr, true); got != applies {
			t.Errorf("mainnet %s: %v, want %v", addr, got, applies)
		}
		if tofuEndpoint(addr, false) {
			t.Errorf("testnet %s: trust on first use applies", addr)
		}
	}
}

// TestObserverCandidatesPreferAuthenticated: within a health partition,
// servers a dial authenticates come first.
func TestObserverCandidatesPreferAuthenticated(t *testing.T) {
	resetServerHealth()
	now := time.Now()
	unpinned := &ServerAddr{Proto: "ssl", Addr: "unpinned:1", SkipVerify: true}
	expiredCert := newTestCert(t, newTestKey(t), now.Add(-2*time.Hour), now.Add(-time.Hour))
	expiredShipped := &ServerAddr{Proto: "ssl", Addr: "expired:1", Cert: expiredCert.tls.Certificate[0]}
	ca := &ServerAddr{Proto: "ssl", Addr: "ca:1"}
	pinned := &ServerAddr{Proto: "ssl", Addr: "pinned:1", SkipVerify: true}
	p := mustServerPool(t, t.TempDir(), []*ServerAddr{expiredShipped, unpinned, ca, pinned})
	p.now = func() time.Time { return now }
	p.entries["pinned:1"].setPin(certPin{spki: [32]byte{1}, notAfter: now.Add(time.Hour), pinnedAt: now})
	var got []string
	for _, sv := range p.observerCandidates() {
		got = append(got, sv.Addr)
	}
	// An expired shipped pin on a learnable endpoint would accept a
	// first-use certificate, so it ranks with the unauthenticated.
	if strings.Join(got, ",") != "ca:1,pinned:1,expired:1,unpinned:1" {
		t.Fatalf("observer order %v", got)
	}
}

// TestAtomicWriteFile: the file is replaced whole and no temporary file is
// left behind.
func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.json")
	for _, content := range []string{"first", "second"} {
		if err := atomicWriteFile(path, []byte(content)); err != nil {
			t.Fatalf("atomicWriteFile: %v", err)
		}
		if raw, _ := os.ReadFile(path); string(raw) != content {
			t.Fatalf("content %q, want %q", raw, content)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("%d files in the directory, want 1", len(entries))
	}
}
