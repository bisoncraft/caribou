// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPublicServerLists(t *testing.T) {
	for name, list := range map[string][]string{"mainnet": publicMainnetServers, "testnet": publicTestnetServers} {
		if len(list) == 0 {
			t.Errorf("no public %s servers", name)
		}
		seen := make(map[string]bool)
		for _, address := range list {
			host, port, err := net.SplitHostPort(address)
			if err != nil || host == "" {
				t.Errorf("%s server %q: %v", name, address, err)
			}
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				t.Errorf("%s server %q: port %q", name, address, port)
			}
			if seen[address] {
				t.Errorf("%s server %q is listed twice", name, address)
			}
			seen[address] = true
		}
	}
}

func TestConfigEndpoints(t *testing.T) {
	// Without a server, the public ones of the network, with TLS.
	for _, tt := range []struct {
		network Network
		want    []string
	}{{Mainnet, publicMainnetServers}, {Testnet, publicTestnetServers}} {
		got, err := (&Config{Network: tt.network, Plaintext: true}).endpoints()
		if err != nil || len(got) != len(tt.want) {
			t.Fatalf("%s: %v, %v", tt.network, got, err)
		}
		for i, server := range got {
			if server.address != tt.want[i] || server.plaintext {
				t.Errorf("%s server %d: %+v", tt.network, i, server)
			}
		}
	}
	// A regtest network has none.
	if _, err := (&Config{Network: testNetwork}).endpoints(); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("regtest without a server: %v", err)
	}
	// A configured server is the only one.
	got, err := (&Config{Network: Mainnet, Server: "127.0.0.1:9067", Plaintext: true}).endpoints()
	if err != nil || len(got) != 1 || got[0] != (endpoint{address: "127.0.0.1:9067", plaintext: true}) {
		t.Errorf("configured server: %v, %v", got, err)
	}
	if _, err := (&Config{Network: Mainnet, Server: "no-port"}).endpoints(); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("malformed server: %v", err)
	}
}

func TestServerList(t *testing.T) {
	a, b, c := endpoint{address: "a:1"}, endpoint{address: "b:1"}, endpoint{address: "c:1"}
	l := newServerList([]endpoint{a, b, c})
	if l.len() != 3 || l.get() != a {
		t.Fatalf("new list: %d, %+v", l.len(), l.get())
	}
	if got := l.rotate(a); got != b || l.get() != b {
		t.Errorf("after a failed: %+v", got)
	}
	// A second report of the same failure does not move the list again.
	if got := l.rotate(a); got != b {
		t.Errorf("after a failed again: %+v", got)
	}
	if got := l.rotate(b); got != c {
		t.Errorf("after b failed: %+v", got)
	}
	// The list wraps around.
	if got := l.rotate(c); got != a {
		t.Errorf("after c failed: %+v", got)
	}
}

// serverEnv is an env whose wallet has several servers, all with the same
// chain. servers[0] is e.srv.
func serverEnv(t *testing.T, chainHeight uint32, n int) (e *env, servers []*fakeLwd) {
	t.Helper()
	return serverEnvFor(t, fakeKind, chainHeight, n)
}

// serverEnvFor is serverEnv with the given kind of backend.
func serverEnvFor(t *testing.T, kind backendKind, chainHeight uint32, n int) (e *env, servers []*fakeLwd) {
	t.Helper()
	e = newEnv(t, kind, chainHeight, nil)
	servers = []*fakeLwd{e.srv}
	endpoints := []endpoint{{address: e.srv.addr, plaintext: true}}
	for range n - 1 {
		srv := newFakeLwd(t, chainHeight)
		srv.branch = e.srv.branch
		servers = append(servers, srv)
		endpoints = append(endpoints, endpoint{address: srv.addr, plaintext: true})
	}
	e.w.servers = newServerList(endpoints)
	return e, servers
}

// calls returns the number of calls a server has received.
func calls(srv *fakeLwd) int {
	n := 0
	srv.set(func(s *fakeLwd) {
		for _, count := range s.calls {
			n += count
		}
	})
	return n
}

var errServerDown = status.Error(codes.Unavailable, "down")

// down makes every call to a server fail the way an unreachable server does,
// and up undoes it.
func down(srv *fakeLwd) {
	srv.set(func(s *fakeLwd) {
		for _, method := range []string{
			"GetLightdInfo", "GetLatestBlock", "GetBlock", "GetBlockRange", "GetTreeState", "GetSubtreeRoots",
			"GetAddressUtxosStream", "GetTransaction", "GetTaddressTxids",
		} {
			s.fail[method] = errServerDown
		}
	})
}

func up(srv *fakeLwd) {
	srv.set(func(s *fakeLwd) { clear(s.fail) })
}

// TestRotatesPastAnUnavailableServer checks that a wallet whose first server
// cannot be reached creates its account and syncs with the second, and does
// not touch the third.
func TestRotatesPastAnUnavailableServer(t *testing.T) {
	e, servers := serverEnv(t, 150, 3)
	down(servers[0])

	e.createAccount(100)
	if got := e.w.servers.get().address; got != servers[1].addr {
		t.Fatalf("after CreateAccount the wallet uses %s, want the second server %s", got, servers[1].addr)
	}
	// The wallet stays with the server that worked.
	before := calls(servers[0])
	e.connect()
	e.waitSynced(150)
	if after := calls(servers[0]); after != before {
		t.Errorf("the first server was called %d more times after it had failed", after-before)
	}
	if n := calls(servers[2]); n != 0 {
		t.Errorf("the third server was called %d times", n)
	}
}

// TestRotatesWhenTheServerGoesAway checks that a wallet moves on when the
// server it is synced with stops answering, one server at a time, and comes
// back to the first after the last.
func TestRotatesWhenTheServerGoesAway(t *testing.T) {
	e, servers := serverEnv(t, 150, 3)
	e.createAccount(100)
	e.connect()
	e.waitSynced(150)
	if n := calls(servers[1]) + calls(servers[2]); n != 0 {
		t.Fatalf("%d calls to servers the wallet had no need of", n)
	}

	height := uint32(150)
	for round, next := range []int{1, 2, 0} {
		failing := (next + 2) % 3
		for _, srv := range servers {
			srv.mine(2)
		}
		height += 2
		down(servers[failing])
		e.statuses.reset()
		e.waitSynced(height)
		if got := e.w.servers.get().address; got != servers[next].addr {
			t.Fatalf("round %d: the wallet uses %s, want server %d", round, got, next)
		}
		up(servers[failing])
	}
}

// TestATransientFailureDoesNotRotate checks that one failed call to a server
// that has been working does not make the wallet turn to another server,
// which would then learn its transparent addresses.
func TestATransientFailureDoesNotRotate(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	e.createAccount(100)
	e.connect()
	e.waitSynced(150)

	e.statuses.reset()
	servers[0].set(func(s *fakeLwd) { s.failOnce["GetLatestBlock"] = errServerDown })
	e.statuses.wait(t, "the failed attempt", func(s SyncStatus) bool { return s.Err != nil })
	servers[0].mine(1)
	e.waitSynced(151)
	if got := e.w.servers.get().address; got != servers[0].addr {
		t.Errorf("the wallet uses %s after one failed call", got)
	}
	if n := calls(servers[1]); n != 0 {
		t.Errorf("the second server was called %d times", n)
	}
}

// TestALostConnectionDuringTheFirstSyncDoesNotRotate checks that a server
// which has given the wallet blocks counts as working before the wallet has
// synced with it once. A restore is one long sync attempt, and a connection
// lost in the course of it must not send the wallet, and its transparent
// addresses, to the next server.
func TestALostConnectionDuringTheFirstSyncDoesNotRotate(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	e.w.cfg.BatchSize = 10
	e.createAccount(100)
	// The third batch fails, after two that were scanned.
	servers[0].set(func(s *fakeLwd) { s.failNth["GetBlockRange"] = s.calls["GetBlockRange"] + 3 })

	e.connect()
	e.statuses.wait(t, "the failed attempt", func(s SyncStatus) bool { return s.Err != nil })
	e.waitSynced(150)
	if got := e.w.servers.get().address; got != servers[0].addr {
		t.Errorf("the wallet uses %s after one lost connection", got)
	}
	if n := calls(servers[1]); n != 0 {
		t.Errorf("the second server was called %d times", n)
	}
}

// staysWithServer checks that a wallet whose first server serves blocks and
// fails some other request on every attempt stays with that server, and
// scans a new block all the same.
func staysWithServer(t *testing.T, e *env, servers []*fakeLwd) {
	t.Helper()
	e.connect()
	e.statuses.wait(t, "the blocks to be scanned", func(s SyncStatus) bool {
		return s.FullyScanned == 150 && s.Err != nil
	})
	attempts := servers[0].callCount("GetLatestBlock")
	eventually(t, "five more sync attempts", func() bool {
		return servers[0].callCount("GetLatestBlock") >= attempts+5
	})
	if got := e.w.servers.get().address; got != servers[0].addr {
		t.Errorf("the wallet uses %s", got)
	}
	if n := calls(servers[1]); n != 0 {
		t.Errorf("the second server was called %d times", n)
	}

	servers[0].mine(1)
	e.statuses.wait(t, "the new block to be scanned", func(s SyncStatus) bool { return s.FullyScanned == 151 })
	if n := calls(servers[1]); n != 0 {
		t.Errorf("the second server was called %d times after the new block", n)
	}
}

// TestFailingAddressHistoryDoesNotRotate checks that a wallet stays with a
// server that serves blocks and fails every request for the history of an
// address. The next server would be told the addresses for nothing. The
// wallet asks for such a history in the address walk and, with the wallet
// database, for its ephemeral addresses when it answers the requests for
// transaction data.
func TestFailingAddressHistoryDoesNotRotate(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e, servers := serverEnvFor(t, kind, 150, 2)
		e.createAccount(100)
		servers[0].set(func(s *fakeLwd) { s.fail["GetTaddressTxids"] = errServerDown })
		staysWithServer(t, e, servers)
		if n := servers[0].callCount("GetTaddressTxids"); n == 0 {
			t.Error("no address history was asked for")
		}
	})
}

// TestFailingTransactionDataRequestsDoNotRotate checks the same for the
// requests the wallet makes once the blocks are scanned.
func TestFailingTransactionDataRequestsDoNotRotate(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		request txRequest
	}{
		{"status", "GetTransaction", txRequest{kind: txRequestStatus, txid: bytes.Repeat([]byte{1}, 32)}},
		{"enhancement", "GetTransaction", txRequest{kind: txRequestEnhancement, txid: bytes.Repeat([]byte{2}, 32)}},
		{"address", "GetTaddressTxids", txRequest{kind: txRequestAddress, address: "taddr", start: 100}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, servers := serverEnv(t, 150, 2)
			e.createAccount(100)
			e.be.(*fakeBackend).set(func(b *fakeBackend) { b.requests = []txRequest{test.request} })
			// The server answers everything until the wallet is synced,
			// the address walk included, and fails one kind of request
			// from then on.
			e.connect()
			e.waitSynced(150)
			e.statuses.reset()
			servers[0].set(func(s *fakeLwd) { s.fail[test.method] = errServerDown })
			servers[0].mine(1)
			e.statuses.wait(t, "the failing request", func(s SyncStatus) bool {
				return s.FullyScanned == 151 && s.Err != nil
			})
			attempts := servers[0].callCount("GetLatestBlock")
			eventually(t, "five more sync attempts", func() bool {
				return servers[0].callCount("GetLatestBlock") >= attempts+5
			})
			if got := e.w.servers.get().address; got != servers[0].addr {
				t.Errorf("the wallet uses %s", got)
			}
			if n := calls(servers[1]); n != 0 {
				t.Errorf("the second server was called %d times", n)
			}
		})
	}
}

// TestRotatesPastAServerOnAnotherNetwork checks that a server on another
// chain is left at once.
func TestRotatesPastAServerOnAnotherNetwork(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	servers[0].set(func(s *fakeLwd) { s.chainName = "main" })

	e.createAccount(100)
	e.connect()
	e.waitSynced(150)
	if got := e.w.servers.get().address; got != servers[1].addr {
		t.Errorf("the wallet uses %s", got)
	}
	if n := servers[0].callCount("GetBlockRange"); n != 0 {
		t.Errorf("blocks were asked of the server on another network %d times", n)
	}
}

// TestAllServersUnavailable checks that a wallet with no server left keeps
// trying each of them and syncs when one is back.
func TestAllServersUnavailable(t *testing.T) {
	e, servers := serverEnv(t, 150, 3)
	e.createAccount(100)
	for _, srv := range servers {
		down(srv)
	}

	e.connect()
	e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool {
		return s.State == SyncDisconnected && s.Err != nil
	})
	eventually(t, "every server to be tried twice", func() bool {
		for _, srv := range servers {
			if calls(srv) < 2 {
				return false
			}
		}
		return true
	})

	e.statuses.reset()
	up(servers[2])
	e.waitSynced(150)
	if got := e.w.servers.get().address; got != servers[2].addr {
		t.Errorf("the wallet uses %s", got)
	}
}

// TestCreateAccountWithoutAServer checks what CreateAccount returns when no
// server can be reached, and when the configuration names none.
func TestCreateAccountWithoutAServer(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	for _, srv := range servers {
		down(srv)
	}
	_, err := e.w.CreateAccount(context.Background(), &AccountParams{Seed: testSeed, Birthday: 100})
	if !isConnectionError(err) {
		t.Fatalf("CreateAccount with every server down: %v", err)
	}
	for i, srv := range servers {
		if calls(srv) == 0 {
			t.Errorf("server %d was not tried", i)
		}
	}

	w := newWallet(&Config{Network: testNetwork}, newFakeBackend(), nil)
	if _, err := w.CreateAccount(context.Background(), &AccountParams{Seed: testSeed}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("CreateAccount without a server on regtest: %v", err)
	}
	if _, err := w.Connect(context.Background()); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Connect without a server on regtest: %v", err)
	}
}

// TestRotationWithAServerThatCannotBeDialed checks that the wallet passes
// over a server that it cannot prepare a connection for, which here brings
// it back to the one it had.
func TestRotationWithAServerThatCannotBeDialed(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	dial := e.w.dial
	e.w.dial = func(server endpoint) (chainSource, error) {
		if server.address == servers[1].addr {
			return nil, wrapf(ErrInvalidArgument, "no such server")
		}
		return dial(server)
	}
	e.createAccount(100)
	e.connect()
	e.waitSynced(150)

	down(servers[0])
	e.statuses.reset()
	e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
	// Two failed attempts send the wallet on from a server.
	before := calls(servers[0])
	eventually(t, "more failed attempts", func() bool { return calls(servers[0]) >= before+4 })
	servers[0].mine(1)
	up(servers[0])
	e.statuses.reset()
	e.waitSynced(151)
	if n := calls(servers[1]); n != 0 {
		t.Errorf("the second server was called %d times", n)
	}
}
