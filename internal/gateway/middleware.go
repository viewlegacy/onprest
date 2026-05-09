package gateway

import (
	"net"
	"net/http"
	"strings"
	"time"
)

func (s *Server) withAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if len(s.cfg.IPAllowList) > 0 && !s.ipAllowed(r) {
			s.accessLog(newID(), "", capabilityFromPath(r.URL.Path), http.StatusForbidden, errGatewayIPDenied, start)
			writeJSON(w, http.StatusForbidden, apiError(errGatewayIPDenied, "source ip is not allowed"))
			return
		}
		if s.cfg.RateLimit.RequestsPerSecond > 0 && !s.take(s.clientIP(r)) {
			s.accessLog(newID(), "", capabilityFromPath(r.URL.Path), http.StatusTooManyRequests, errGatewayRateLimited, start)
			writeJSON(w, http.StatusTooManyRequests, apiError(errGatewayRateLimited, "too many requests"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ipAllowed(r *http.Request) bool {
	ip := net.ParseIP(s.clientIP(r))
	for _, block := range s.cfg.IPAllowList {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) take(ip string) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := time.Now()
	b := s.rate[ip]
	if b == nil {
		s.rate[ip] = &bucket{tokens: float64(s.cfg.RateLimit.Burst - 1), last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * s.cfg.RateLimit.RequestsPerSecond
	if max := float64(s.cfg.RateLimit.Burst); b.tokens > max {
		b.tokens = max
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) clientIP(r *http.Request) string {
	remote := net.ParseIP(remoteIP(r))
	if remote == nil || !ipInBlocks(remote, s.cfg.TrustedProxies) {
		return remoteIP(r)
	}
	if ip := forwardedForClientIP(r.Header.Get("X-Forwarded-For"), remote, s.cfg.TrustedProxies); ip != "" {
		return ip
	}
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
		return ip.String()
	}
	return remote.String()
}

func forwardedForClientIP(header string, remote net.IP, trusted []*net.IPNet) string {
	parts := strings.Split(header, ",")
	chain := make([]net.IP, 0, len(parts)+1)
	for _, part := range parts {
		ip := net.ParseIP(strings.TrimSpace(part))
		if ip != nil {
			chain = append(chain, ip)
		}
	}
	chain = append(chain, remote)
	for i := len(chain) - 1; i >= 0; i-- {
		if !ipInBlocks(chain[i], trusted) {
			return chain[i].String()
		}
	}
	return ""
}

func ipInBlocks(ip net.IP, blocks []*net.IPNet) bool {
	for _, block := range blocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}
