// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

// minConfirmations is the confirmations policy of balances and sends, in one
// place: zero selects the ZIP 315 policy of zeclib, 3 confirmations for
// outputs of the wallet's own transactions and 10 for everything else.
const minConfirmations = 0

// netConfig is how the wallet reaches the chain.
type netConfig struct {
	network zeclib.Network
	// server is empty for the built-in public servers.
	server    string
	plaintext bool
	// proxy is the SOCKS5 proxy the wallet's connections go through, empty
	// for none.
	proxy string
}

// configFor builds the network configuration from the wallet settings. Only
// a custom server on this machine (a loopback address or localhost) is
// dialed in plaintext; any other needs TLS. A custom server named by
// localhost or by a loopback, private, link-local or carrier-grade NAT IP
// address is not reached through the proxy, as dexnet.ProxyFor decides; a
// host name is. The built-in servers are always reached through the proxy
// when one is set.
func configFor(network util.Network, settings map[string]string, torProxy string) (*netConfig, error) {
	c := &netConfig{proxy: torProxy}
	switch network {
	case util.Mainnet:
		c.network = zeclib.Mainnet
	case util.Testnet:
		c.network = zeclib.Testnet
	case util.Simnet:
		c.network = zeclib.Regtest(zeclib.AllUpgradesAt(1))
		c.server = simnetServer
	default:
		return nil, fmt.Errorf("unknown network %v", network)
	}
	if s := strings.TrimSpace(settings[settingServer]); s != "" {
		c.server = s
	}
	if c.server != "" {
		host, _, err := net.SplitHostPort(c.server)
		if err != nil || host == "" {
			return nil, fmt.Errorf("the lightwalletd server %q is not host:port", c.server)
		}
		c.plaintext = isLoopbackHost(host)
		c.proxy = dexnet.ProxyFor(c.server, torProxy)
	}
	return c, nil
}

// isLoopbackHost reports whether host names this machine: localhost, or an
// address in 127.0.0.0/8 or ::1.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// zeclibConfig returns the zeclib configuration for the database at dbPath.
// The callbacks are left to the caller.
func (c *netConfig) zeclibConfig(dbPath string, log util.Logger) *zeclib.Config {
	cfg := &zeclib.Config{
		DBPath:           dbPath,
		Network:          c.network,
		Server:           c.server,
		Plaintext:        c.plaintext,
		Logger:           log,
		MinConfirmations: minConfirmations,
	}
	if c.proxy != "" {
		dial := dexnet.ProxyDialContext(c.proxy)
		cfg.Dialer = func(ctx context.Context, address string) (net.Conn, error) {
			return dial(ctx, "tcp", address)
		}
	}
	return cfg
}
