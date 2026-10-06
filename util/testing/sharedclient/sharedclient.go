// Package sharedclient lets tests prove that a caller reaches the network
// through dexnet.Client, the one client --torproxy is applied to.
package sharedclient

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/bisoncraft/caribou/util/dexnet"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Capture replaces dexnet.Client with one that counts every request and
// fails it, and restores the original when the test ends. It swaps a global,
// so a test that uses it must not run in parallel.
func Capture(t testing.TB) *atomic.Int32 {
	t.Helper()
	old := dexnet.Client
	t.Cleanup(func() { dexnet.Client = old })
	reqs := new(atomic.Int32)
	dexnet.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		reqs.Add(1)
		return nil, errors.New("blocked by test transport")
	})}
	return reqs
}

// AssertReached fails the test unless a request went through the captured
// client. A caller that built its own client would leave a configured proxy
// unapplied, so what it connects to would see the user's address.
func AssertReached(t testing.TB, reqs *atomic.Int32, who string) {
	t.Helper()
	if reqs.Load() == 0 {
		t.Fatalf("%s bypassed dexnet.Client; a configured proxy would not apply", who)
	}
}
