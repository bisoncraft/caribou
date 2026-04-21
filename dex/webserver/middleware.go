// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package webserver

import (
	"fmt"
	"net/http"

	"github.com/bisoncraft/meshwallet/dex"
)

// LimitRate is rate-limiting middleware for HTTP endpoints.
func (s *Server) LimitRate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, err := s.meterIP(dex.NewIPKey(r.RemoteAddr))
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// meterIP applies the per-IP rate limiter and the global HTTP rate limiter.
// The per-IP limiter is checked first so that a single abusive source is
// rejected without consuming global rate limit tokens.
func (s *Server) meterIP(ip dex.IPKey) (int, error) {
	ipLimiter := s.getIPLimiter(ip)
	if !ipLimiter.Allow() {
		return http.StatusTooManyRequests, fmt.Errorf("too many requests")
	}
	if !s.globalHTTPRateLimiter.Allow() {
		return http.StatusTooManyRequests, fmt.Errorf("too many global requests")
	}
	return 0, nil
}
