// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"net"
	"sync"
)

// The public lightwalletd servers that a wallet uses when Config.Server is
// empty, in the order it tries them. It starts with the first and moves to
// the next when the one it uses fails.
//
// A server is listed when it is open to the public, is among the servers
// that the Zodl wallets are built with, and passes TestPublicServers, which
// makes every call the wallet makes:
//
//	go test -tags live -run TestPublicServers -v ./wallet/asset/zec/zeclib/
//
// The order matters for privacy. Every server a wallet uses learns the
// wallet's transparent addresses, and servers of one operator come before
// those of the next, so that a wallet reaches a second operator only when
// every server of the first has failed.
//
// Verified on 2026-11-13. Other servers that passed then and are not listed,
// because nothing is known here about who runs them:
// zec-node.cakewallet.com:443, lwd.zcashexplorer.app:9067, lwd.z0n.jp:443,
// zec.alexxiy.top:9067, zcash.mysideoftheweb.com:9067,
// lwd.blakyniica.xyz:443, lwd.bunny-zec.xyz:443 and zec.0xrpc.io:443. New
// candidates are in the server list of the Zodl Android wallet
// (LightWalletEndpointProvider.kt) and on https://hosh.zec.rocks/zec.
var (
	publicMainnetServers = []string{
		"zec.rocks:443",
		"na.zec.rocks:443",
		"eu.zec.rocks:443",
		"sa.zec.rocks:443",
		"ap.zec.rocks:443",
		"us.zec.stardust.rest:443",
		"eu.zec.stardust.rest:443",
	}
	publicTestnetServers = []string{
		"testnet.zec.rocks:443",
	}
)

// publicServers returns the public servers known for a network. There are
// none for a regtest network.
func publicServers(network Network) []string {
	switch network.Type {
	case NetworkMainnet:
		return publicMainnetServers
	case NetworkTestnet:
		return publicTestnetServers
	default:
		return nil
	}
}

// endpoint is a lightwalletd server to connect to.
type endpoint struct {
	// address is the host:port of the server.
	address string
	// plaintext disables TLS.
	plaintext bool
}

// endpoints returns the servers the configuration selects: Config.Server
// alone if it is set, and the public servers of the network otherwise.
func (cfg *Config) endpoints() ([]endpoint, error) {
	if cfg.Server != "" {
		if _, _, err := net.SplitHostPort(cfg.Server); err != nil {
			return nil, wrapf(ErrInvalidArgument, "lightwalletd server %q: %v", cfg.Server, err)
		}
		return []endpoint{{address: cfg.Server, plaintext: cfg.Plaintext}}, nil
	}
	public := publicServers(cfg.Network)
	if len(public) == 0 {
		return nil, wrapf(ErrInvalidArgument, "no lightwalletd server is configured and none is known for %s", cfg.Network)
	}
	servers := make([]endpoint, len(public))
	for i, address := range public {
		servers[i] = endpoint{address: address}
	}
	return servers, nil
}

// serverList is the servers a wallet can use and the one it uses now. The
// wallet stays with a server until that server fails, and with the one it
// moved to after that, for as long as the Wallet lives.
type serverList struct {
	mtx     sync.Mutex
	servers []endpoint
	current int
}

func newServerList(servers []endpoint) *serverList {
	return &serverList{servers: servers}
}

// len returns the number of servers.
func (l *serverList) len() int {
	l.mtx.Lock()
	defer l.mtx.Unlock()
	return len(l.servers)
}

// get returns the server in use.
func (l *serverList) get() endpoint {
	l.mtx.Lock()
	defer l.mtx.Unlock()
	return l.servers[l.current]
}

// rotate moves on from a server that failed and returns the server in use
// after that. It moves only if failed is still the one in use: two callers
// that saw the same server fail move on once.
func (l *serverList) rotate(failed endpoint) endpoint {
	l.mtx.Lock()
	defer l.mtx.Unlock()
	if l.servers[l.current] == failed {
		l.current = (l.current + 1) % len(l.servers)
	}
	return l.servers[l.current]
}
