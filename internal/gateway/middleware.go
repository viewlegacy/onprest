package gateway

import (
	"math"
	"net"
	"net/http"
	"strings"
	"time"
)

const maxRateLimitBuckets = 10000

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				if r.URL.Path == "/mcp" {
					s.mcpHTTPRejected(http.StatusInternalServerError, errGatewayInternal, "unexpected gateway error")
				} else {
					s.accessLog(newID(), "", capabilityFromPath(r.URL.Path), http.StatusInternalServerError, errGatewayInternal, "unexpected gateway error", start)
				}
				writeJSON(w, http.StatusInternalServerError, apiError(errGatewayInternal, "unexpected gateway error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		corsAllowed := s.applyCORS(w, r)
		if r.Method == http.MethodOptions {
			if !corsAllowed && r.Header.Get("Origin") != "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if len(s.cfg.IPAllowList) > 0 && !s.ipAllowed(r) {
			if r.URL.Path == "/mcp" {
				s.mcpHTTPRejected(http.StatusForbidden, errGatewayIPDenied, "source ip is not allowed")
			} else {
				s.accessLog(newID(), "", capabilityFromPath(r.URL.Path), http.StatusForbidden, errGatewayIPDenied, "source ip is not allowed", start)
			}
			writeJSON(w, http.StatusForbidden, apiError(errGatewayIPDenied, "source ip is not allowed"))
			return
		}
		if s.cfg.RateLimit.RequestsPerSecond > 0 && !s.take(s.clientIP(r)) {
			if r.URL.Path == "/mcp" {
				s.mcpHTTPRejected(http.StatusTooManyRequests, errGatewayRateLimited, "too many requests")
			} else {
				s.accessLog(newID(), "", capabilityFromPath(r.URL.Path), http.StatusTooManyRequests, errGatewayRateLimited, "too many requests", start)
			}
			writeJSON(w, http.StatusTooManyRequests, apiError(errGatewayRateLimited, "too many requests"))
			return
		}
		if r.URL.Path != "/ws/agent" && r.Body != nil && r.Body != http.NoBody {
			controller := http.NewResponseController(w)
			_ = controller.SetReadDeadline(time.Now().Add(s.cfg.BodyReadTimeout))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	if !s.corsOriginAllowed(origin) {
		return false
	}
	headers := w.Header()
	headers.Set("Access-Control-Allow-Origin", origin)
	headers.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	headers.Set("Access-Control-Allow-Headers", "Authorization, X-API-Key, Content-Type")
	headers.Set("Access-Control-Max-Age", "600")
	headers.Add("Vary", "Origin")
	headers.Add("Vary", "Access-Control-Request-Method")
	headers.Add("Vary", "Access-Control-Request-Headers")
	return true
}

func (s *Server) corsOriginAllowed(origin string) bool {
	for _, allowed := range s.cfg.CORSAllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
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
	if s.rateLastCleanup.IsZero() || now.Sub(s.rateLastCleanup) >= time.Minute || len(s.rate) >= maxRateLimitBuckets {
		s.cleanupRateBuckets(now)
		s.rateLastCleanup = now
	}
	b := s.rate[ip]
	if b == nil {
		if len(s.rate) >= maxRateLimitBuckets {
			s.evictOldestRateBucket()
		}
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

func (s *Server) cleanupRateBuckets(now time.Time) {
	idleTTL := 10 * time.Minute
	if s.cfg.RateLimit.RequestsPerSecond > 0 {
		refill := time.Duration(math.Ceil(float64(s.cfg.RateLimit.Burst)/s.cfg.RateLimit.RequestsPerSecond)) * time.Second
		if refill*2 > idleTTL {
			idleTTL = refill * 2
		}
	}
	cutoff := now.Add(-idleTTL)
	for ip, b := range s.rate {
		if b.last.Before(cutoff) {
			delete(s.rate, ip)
		}
	}
}

func (s *Server) evictOldestRateBucket() {
	var oldestIP string
	var oldest time.Time
	for ip, b := range s.rate {
		if oldest.IsZero() || b.last.Before(oldest) {
			oldestIP, oldest = ip, b.last
		}
	}
	if oldestIP != "" {
		delete(s.rate, oldestIP)
	}
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
