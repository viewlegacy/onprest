// Package hostservices provides private, request-owned HTTP transport. It does
// not register executors, interpret response bodies, or expose a Wasm ABI.
package hostservices

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

type authorization struct {
	mode          string
	origin        string
	methods       []string
	cidrs         []netip.Prefix
	explicitCIDRs bool
	grants        []grant
	headers       http.Header
}
type grant struct {
	origin, path  string
	methods       []string
	cidrs         []netip.Prefix
	explicitCIDRs bool
}

// FixedSource uses the same immutable prepared source and credentials as the
// definition hash. The caller supplies the URL/body after HTTP executor input binding.
func FixedSource(snapshot *providers.Snapshot, options Options) (*Service, error) {
	if snapshot == nil || snapshot.Definition().Executor.Type != "http" {
		return nil, invalid(nil)
	}
	d := snapshot.Definition()
	limits := d.Executor.Settings["limits"].(map[string]any)
	options.MaxRequestBytes = min(options.MaxRequestBytes, limits["max_http_request_bytes"].(int64))
	options.MaxResponseBytes = min(options.MaxResponseBytes, limits["max_http_response_bytes"].(int64))
	src, ok := snapshot.Source(providers.SourceKey{Group: "http", Name: d.Executor.Settings["source"].(string)})
	if !ok {
		return nil, invalid(nil)
	}
	u, err := parseURL(src["base_url"].(string))
	if err != nil {
		return nil, err
	}
	a := authorization{mode: "fixed", origin: origin(u), methods: append([]string(nil), src["allowed_methods"].([]string)...), headers: make(http.Header)}
	a.cidrs, a.explicitCIDRs, err = prefixes(src)
	if err != nil {
		return nil, err
	}
	for k, v := range src["headers"].(map[string]any) {
		a.headers.Set(k, v.(string))
	}
	if token, ok := src["credential"].(string); ok {
		a.headers.Set("Authorization", "Bearer "+token)
	}
	return newService(options, a)
}

// WasmOFF grants OS-user networking without requiring a fixed source. TLS
// verification, finite I/O, deadlines and single-hop sends still apply.
func WasmOFF(options Options) (*Service, error) {
	return newService(options, authorization{mode: "off"})
}

// WasmON accepts only compiler-normalized HTTP grants. No fixed-source
// credentials are inherited by a Wasm execution.
func WasmON(permissions []any, options Options) (*Service, error) {
	a := authorization{mode: "on"}
	for _, raw := range permissions {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, invalid(nil)
		}
		address, ok := m["origin"].(string)
		if !ok {
			return nil, invalid(nil)
		}
		u, err := parseURL(address)
		if err != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" {
			return nil, invalid(err)
		}
		p, ok := m["path_prefix"].(string)
		if !ok || !strings.HasPrefix(p, "/") {
			return nil, invalid(nil)
		}
		methods, ok := m["methods"].([]string)
		if !ok || len(methods) == 0 {
			return nil, invalid(nil)
		}
		g := grant{origin: origin(u), path: p, methods: append([]string(nil), methods...)}
		g.cidrs, g.explicitCIDRs, err = prefixes(m)
		if err != nil {
			return nil, err
		}
		a.grants = append(a.grants, g)
	}
	return newService(options, a)
}

func prefixes(m map[string]any) ([]netip.Prefix, bool, error) {
	v, exists := m["allowed_cidrs"]
	if !exists {
		return nil, false, nil
	}
	a, ok := v.([]string)
	if !ok {
		return nil, false, invalid(nil)
	}
	out := make([]netip.Prefix, 0, len(a))
	for _, s := range a {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, false, invalid(err)
		}
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		out = append(out, p.Masked())
	}
	return out, true, nil
}
func parseURL(address string) (*url.URL, error) {
	u, err := url.Parse(address)
	if err != nil || !utf8.ValidString(address) || strings.ContainsAny(address, "\x00\r\n") || u == nil || u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || strings.Contains(u.Hostname(), "%") {
		return nil, invalid(err)
	}
	if u.Port() != "" {
		if _, err := net.LookupPort("tcp", u.Port()); err != nil {
			return nil, invalid(err)
		}
	}
	return u, nil
}
func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}
func contains(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func pathAllowed(path, prefix string) bool {
	// Decode before segment comparison. Reject aliases that could be interpreted
	// differently by an upstream router rather than normalizing through a grant.
	if strings.Contains(path, "\\") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	prefix = strings.TrimSuffix(prefix, "/")
	return prefix == "" || path == prefix || strings.HasPrefix(path, prefix+"/")
}
func ipAllowed(ip netip.Addr, cidrs []netip.Prefix, explicit bool, off bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() {
		return false
	}
	if explicit {
		for _, p := range cidrs {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	if off {
		return true
	}
	return !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}
func (a authorization) allowed(method string, u *url.URL, ip netip.Addr) bool {
	switch a.mode {
	case "off":
		return ipAllowed(ip, nil, false, true)
	case "fixed":
		return origin(u) == a.origin && contains(a.methods, method) && ipAllowed(ip, a.cidrs, a.explicitCIDRs, false)
	case "on":
		for _, g := range a.grants {
			if origin(u) == g.origin && contains(g.methods, method) && pathAllowed(u.Path, g.path) && ipAllowed(ip, g.cidrs, g.explicitCIDRs, false) {
				return true
			}
		}
	}
	return false
}
func (s *Service) destination(ctx context.Context, method string, u *url.URL) (netip.Addr, error) {
	if !validToken(method) {
		return netip.Addr{}, invalid(nil)
	}
	// Refuse an ungranted URL before DNS, which is itself network activity.
	allowedURL := s.auth.mode == "off"
	if s.auth.mode == "fixed" {
		allowedURL = origin(u) == s.auth.origin && contains(s.auth.methods, method)
	}
	if s.auth.mode == "on" {
		for _, g := range s.auth.grants {
			if origin(u) == g.origin && contains(g.methods, method) && pathAllowed(u.Path, g.path) {
				allowedURL = true
				break
			}
		}
	}
	if !allowedURL {
		return netip.Addr{}, denied(nil)
	}
	// Literal addresses and resolved DNS answers are checked identically. The
	// dialer receives only the chosen literal, so it cannot resolve a second time.
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		ips = []netip.Addr{ip}
	} else {
		values, err := s.lookup(ctx, u.Hostname())
		if err != nil {
			return netip.Addr{}, transportError(ctx, err)
		}
		ips = values
	}
	for _, ip := range ips {
		if s.auth.allowed(method, u, ip) {
			return ip.Unmap(), nil
		}
	}
	return netip.Addr{}, denied(nil)
}

// RedirectTarget validates an explicitly requested next hop. Do never follows
// redirects; the fixed executor must open a new request for each accepted hop.
func (s *Service) RedirectTarget(ctx context.Context, method, current, location string, hop int, enabled bool, maxHops int) (string, error) {
	if ctx == nil {
		return "", invalid(nil)
	}
	if s.auth.mode != "fixed" || !enabled || hop < 1 || maxHops < 1 || hop > maxHops {
		return "", denied(nil)
	}
	u, err := parseURL(current)
	if err != nil {
		return "", err
	}
	if origin(u) != s.auth.origin {
		return "", denied(nil)
	}
	ref, err := url.Parse(location)
	if err != nil {
		return "", invalid(err)
	}
	target, err := parseURL(u.ResolveReference(ref).String())
	if err != nil {
		return "", err
	}
	if origin(target) != s.auth.origin {
		return "", denied(nil)
	}
	if _, err = s.destination(ctx, method, target); err != nil {
		return "", err
	}
	return target.String(), nil
}

func failure(code string, cause error) error { return &providers.Failure{Code: code, Cause: cause} }
func invalid(cause error) error              { return failure("VALIDATION_FAILED", cause) }
func denied(cause error) error               { return failure("PERMISSION_DENIED", cause) }
func transportError(ctx context.Context, err error) error {
	var prior *providers.Failure
	if errors.As(err, &prior) {
		return prior
	}
	if ctx.Err() != nil {
		return failure("EXECUTION_TIMEOUT", ctx.Err())
	}
	var limit *contract.OutputError
	if errors.As(err, &limit) && limit.Code == "LIMIT_EXCEEDED" {
		return failure("LIMIT_EXCEEDED", err)
	}
	return failure("DEPENDENCY_UNAVAILABLE", err)
}
