// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package webserver

import (
	"context"
	"crypto/elliptic"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/meshwallet/util"
	"github.com/decred/dcrd/certgen"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/time/rate"
)

const (
	// Defaults for configurable HTTP rate limit values.
	DefaultIPRatePerSec      = 1.0
	DefaultIPBurstSize       = 5
	DefaultMaxIPRateLimiters = 10000
	DefaultGlobalHTTPRate    = 100.0
	DefaultGlobalHTTPBurst   = 1000

	// serverTimeoutSeconds is the read/write timeout for the HTTP server.
	serverTimeoutSeconds = 10
)

// ipRateLimiter tracks a per-IP HTTP request rate.
type ipRateLimiter struct {
	*rate.Limiter
	lastHit time.Time
}

// getIPLimiter returns the ipRateLimiter for the given IP, creating one if
// needed. If the per-IP map has reached maxIPRateLimiters, the shared overflow
// limiter is returned instead.
func (s *Server) getIPLimiter(ip util.IPKey) *ipRateLimiter {
	s.rateLimiterMtx.Lock()
	defer s.rateLimiterMtx.Unlock()
	limiter := s.ipHTTPRateLimiter[ip]
	if limiter != nil {
		limiter.lastHit = time.Now()
		return limiter
	}
	if len(s.ipHTTPRateLimiter) >= s.maxIPRateLimiters {
		return s.overflowLimiter
	}
	limiter = &ipRateLimiter{
		Limiter: rate.NewLimiter(rate.Limit(s.ipMaxRatePerSec), s.ipMaxBurstSize),
		lastHit: time.Now(),
	}
	s.ipHTTPRateLimiter[ip] = limiter
	return limiter
}

// RPCConfig is the server configuration.
type RPCConfig struct {
	// ListenAddrs are the addresses on which the server will listen.
	ListenAddrs []string
	// RPCKey and RPCCert are the TLS keypair file paths. If they do not exist,
	// a self-signed keypair will be generated at these paths.
	RPCKey  string
	RPCCert string
	// NoTLS disables TLS, serving plain HTTP instead.
	NoTLS bool
	// AltDNSNames specifies additional subject alternative names for an
	// auto-generated TLS certificate.
	AltDNSNames []string
	// IPRatePerSec is the per-IP HTTP request rate in requests/second. Zero
	// means use the default (1).
	IPRatePerSec float64
	// IPBurstSize is the per-IP HTTP burst size. Zero means use the default (5).
	IPBurstSize int
	// MaxIPRateLimiters is the maximum number of entries in the per-IP rate
	// limiter map. Zero means use the default (10000).
	MaxIPRateLimiters int
	// GlobalHTTPRate is the global HTTP request rate in requests/second. Zero
	// means use the default (100).
	GlobalHTTPRate float64
	// GlobalHTTPBurst is the global HTTP burst size. Zero means use the
	// default (1000).
	GlobalHTTPBurst int
}

// Server is an HTTP server with per-IP and global rate limiting.
type Server struct {
	mux       *chi.Mux
	listeners []net.Listener

	// Per-IP HTTP rate limiting state.
	ipMaxRatePerSec   float64
	ipMaxBurstSize    int
	maxIPRateLimiters int
	overflowLimiter   *ipRateLimiter
	ipHTTPRateLimiter map[util.IPKey]*ipRateLimiter
	rateLimiterMtx    sync.Mutex

	// globalHTTPRateLimiter limits the overall HTTP request rate.
	globalHTTPRateLimiter *rate.Limiter
}

// NewServer constructs a Server. Call Run to start serving.
func NewServer(cfg *RPCConfig) (*Server, error) {
	var tlsConfig *tls.Config
	if !cfg.NoTLS {
		keyExists := util.FileExists(cfg.RPCKey)
		certExists := util.FileExists(cfg.RPCCert)
		if certExists != keyExists {
			return nil, fmt.Errorf("missing cert pair file")
		}
		if !keyExists && !certExists {
			if err := genCertPair(cfg.RPCCert, cfg.RPCKey, cfg.AltDNSNames); err != nil {
				return nil, err
			}
		}
		keypair, err := tls.LoadX509KeyPair(cfg.RPCCert, cfg.RPCKey)
		if err != nil {
			return nil, err
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{keypair},
			MinVersion:   tls.VersionTLS12,
		}
	}

	ipv4ListenAddrs, ipv6ListenAddrs, _, err := parseListeners(cfg.ListenAddrs)
	if err != nil {
		return nil, err
	}
	listeners := make([]net.Listener, 0, len(cfg.ListenAddrs))
	parseListener := func(network, addr string) error {
		var ln net.Listener
		if cfg.NoTLS {
			ln, err = net.Listen(network, addr)
		} else {
			ln, err = tls.Listen(network, addr, tlsConfig)
		}
		if err != nil {
			return fmt.Errorf("cannot listen on %s: %w", addr, err)
		}
		listeners = append(listeners, ln)
		return nil
	}
	for _, addr := range ipv4ListenAddrs {
		if err := parseListener("tcp4", addr); err != nil {
			return nil, err
		}
	}
	for _, addr := range ipv6ListenAddrs {
		if err := parseListener("tcp6", addr); err != nil {
			return nil, err
		}
	}
	if len(listeners) == 0 {
		return nil, fmt.Errorf("no valid listen address")
	}

	ipRate := cfg.IPRatePerSec
	if ipRate <= 0 {
		ipRate = DefaultIPRatePerSec
	}
	ipBurst := cfg.IPBurstSize
	if ipBurst <= 0 {
		ipBurst = DefaultIPBurstSize
	}
	maxIPLimiters := cfg.MaxIPRateLimiters
	if maxIPLimiters <= 0 {
		maxIPLimiters = DefaultMaxIPRateLimiters
	}
	globalRate := cfg.GlobalHTTPRate
	if globalRate <= 0 {
		globalRate = DefaultGlobalHTTPRate
	}
	globalBurst := cfg.GlobalHTTPBurst
	if globalBurst <= 0 {
		globalBurst = DefaultGlobalHTTPBurst
	}

	log.Infof("Web server config: ipRate=%.1f/s, ipBurst=%d, maxIPLimiters=%d, globalRate=%.1f/s, globalBurst=%d",
		ipRate, ipBurst, maxIPLimiters, globalRate, globalBurst)

	mux := chi.NewRouter()
	mux.Use(middleware.Recoverer)

	return &Server{
		mux:               mux,
		listeners:         listeners,
		ipMaxRatePerSec:   ipRate,
		ipMaxBurstSize:    ipBurst,
		maxIPRateLimiters: maxIPLimiters,
		overflowLimiter: &ipRateLimiter{
			Limiter: rate.NewLimiter(rate.Limit(ipRate), ipBurst),
		},
		ipHTTPRateLimiter:     make(map[util.IPKey]*ipRateLimiter),
		globalHTTPRateLimiter: rate.NewLimiter(rate.Limit(globalRate), globalBurst),
	}, nil
}

// Mux returns the chi.Mux so callers can register routes before calling Run.
func (s *Server) Mux() *chi.Mux {
	return s.mux
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	httpServer := &http.Server{
		Handler:      s.mux,
		ReadTimeout:  serverTimeoutSeconds * time.Second,
		WriteTimeout: serverTimeoutSeconds * time.Second,
	}

	var wg sync.WaitGroup
	for _, ln := range s.listeners {
		wg.Add(1)
		go func(ln net.Listener) {
			defer wg.Done()
			log.Infof("Server listening on %s", ln.Addr())
			if err := httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				log.Warnf("unexpected (http.Server).Serve error: %v", err)
			}
			log.Debugf("Listener done for %s", ln.Addr())
		}(ln)
	}

	// Periodically evict stale per-IP rate limiter entries.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.rateLimiterMtx.Lock()
				for ip, limiter := range s.ipHTTPRateLimiter {
					if time.Since(limiter.lastHit) > time.Minute {
						delete(s.ipHTTPRateLimiter, ip)
					}
				}
				s.rateLimiterMtx.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()

	<-ctx.Done()

	log.Infof("Server shutting down...")
	ctxTimeout, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctxTimeout); err != nil {
		log.Warnf("http.Server.Shutdown: %v", err)
	}
	wg.Wait()
	log.Infof("Server shutdown complete")
}

// genCertPair generates a TLS key/cert pair at the given paths.
func genCertPair(certFile, keyFile string, altDNSNames []string) error {
	log.Infof("Generating TLS certificates...")
	org := "meshwallet autogenerated cert"
	validUntil := time.Now().Add(10 * 365 * 24 * time.Hour)
	cert, key, err := certgen.NewTLSCertPair(elliptic.P521(), org, validUntil, altDNSNames)
	if err != nil {
		return err
	}
	if err = os.WriteFile(certFile, cert, 0644); err != nil {
		return err
	}
	if err = os.WriteFile(keyFile, key, 0600); err != nil {
		os.Remove(certFile)
		return err
	}
	log.Infof("Done generating TLS certificates")
	return nil
}

// parseListeners splits listen addresses into IPv4 and IPv6 slices.
func parseListeners(addrs []string) ([]string, []string, bool, error) {
	ipv4ListenAddrs := make([]string, 0, len(addrs))
	ipv6ListenAddrs := make([]string, 0, len(addrs))
	haveWildcard := false

	for _, addr := range addrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, nil, false, err
		}
		if host == "" {
			ipv4ListenAddrs = append(ipv4ListenAddrs, addr)
			ipv6ListenAddrs = append(ipv6ListenAddrs, addr)
			haveWildcard = true
			continue
		}
		zoneIndex := strings.LastIndex(host, "%")
		if zoneIndex > 0 {
			host = host[:zoneIndex]
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return nil, nil, false, fmt.Errorf("'%s' is not a valid IP address", host)
		}
		if ip.To4() == nil {
			ipv6ListenAddrs = append(ipv6ListenAddrs, addr)
		} else {
			ipv4ListenAddrs = append(ipv4ListenAddrs, addr)
		}
	}
	return ipv4ListenAddrs, ipv6ListenAddrs, haveWildcard, nil
}

