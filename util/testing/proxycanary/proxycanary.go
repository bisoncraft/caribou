// Package proxycanary provides loopback listeners for tests that prove an
// outbound connection honors --torproxy.
package proxycanary

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Listener accepts loopback TCP connections, counts them, and hangs up. It
// stands in for a SOCKS5 proxy, or for a server a proxied client must never
// reach directly.
type Listener struct {
	// Addr is the host:port to configure as the proxy or the server.
	Addr     string
	accepted atomic.Int32
	closed   atomic.Int32
}

// New starts a Listener that is closed when the test ends.
func New(t testing.TB) *Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &Listener{Addr: l.Addr().String()}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			c.accepted.Add(1)
			conn.Close()
		}
	}()
	t.Cleanup(func() { l.Close() })
	return c
}

// Accepted is the number of connections received so far.
func (c *Listener) Accepted() int { return int(c.accepted.Load()) }

// WaitAccepted waits up to d for at least one connection and reports whether
// one arrived.
func (c *Listener) WaitAccepted(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.Accepted() > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.Accepted() > 0
}

// NewStalling starts a Listener that accepts connections and then says
// nothing, like a proxy that never answers the SOCKS handshake. The
// connections are held open until the test ends.
func NewStalling(t testing.TB) *Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &Listener{Addr: l.Addr().String()}
	var mtx sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			mtx.Lock()
			held = append(held, conn)
			mtx.Unlock()
			c.accepted.Add(1)
			// Count the peer hanging up, which is how a client that has
			// given up on the handshake shows itself.
			go func() {
				io.Copy(io.Discard, conn)
				c.closed.Add(1)
			}()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		mtx.Lock()
		defer mtx.Unlock()
		for _, conn := range held {
			conn.Close()
		}
	})
	return c
}

// WaitClosed waits up to d for the peer to hang up on a connection that a
// stalling Listener accepted, and reports whether one did.
func (c *Listener) WaitClosed(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.closed.Load() > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.closed.Load() > 0
}

// AssertViaProxy fails the test unless the proxy received a connection and
// the direct listener received none.
func AssertViaProxy(t testing.TB, proxy, direct *Listener) {
	t.Helper()
	if !proxy.WaitAccepted(2 * time.Second) {
		t.Fatal("never connected to the configured proxy")
	}
	if n := direct.Accepted(); n != 0 {
		t.Fatalf("connected directly %d time(s) despite the proxy", n)
	}
}
