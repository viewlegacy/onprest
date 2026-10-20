package contract

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

func compileSources(m rawMap) (Sources, error) {
	s := Sources{map[string]map[string]any{}, map[string]map[string]any{}, map[string]map[string]any{}}
	if e := known(m, "databases folders http"); e != nil {
		return s, e
	}
	for _, group := range []string{"databases", "folders", "http"} {
		g, e := m.object(group)
		if e != nil {
			return s, e
		}
		for _, name := range g.keys {
			if name == "" {
				return s, errors.New("source name must not be empty")
			}
			r, e := object(g.get(name), "source")
			if e != nil {
				return s, e
			}
			var out map[string]any
			switch group {
			case "databases":
				out, e = compileDatabase(r)
			case "folders":
				out, e = compileFolder(r)
			case "http":
				out, e = compileHTTPSource(r)
			}
			if e != nil {
				return s, e
			}
			switch group {
			case "databases":
				s.Databases[name] = out
			case "folders":
				s.Folders[name] = out
			case "http":
				s.HTTP[name] = out
			}
		}
	}
	return s, nil
}
func compileDatabase(m rawMap) (map[string]any, error) {
	if e := known(m, "driver host port name user password password_file tls"); e != nil {
		return nil, e
	}
	out := map[string]any{}
	for _, k := range []string{"driver", "host", "name", "user"} {
		v, e := requiredString(m, k)
		if e != nil {
			return nil, e
		}
		out[k] = v
	}
	driver := out["driver"].(string)
	if !contains([]string{"postgres", "mysql", "sqlserver", "oracle"}, driver) {
		return nil, errors.New("unsupported DB driver")
	}
	port, e := intDefault(m, "port", 0, 1)
	if e != nil || port == 0 || port > 65535 {
		return nil, errors.New("database port must be 1..65535")
	}
	out["port"] = port
	if m.has("password") && m.has("password_file") {
		return nil, errors.New("password and password_file are exclusive")
	}
	for _, k := range []string{"password", "password_file"} {
		if m.has(k) {
			s, e := str(m.get(k), k)
			if e != nil || k == "password_file" && (s == "" || strings.ContainsRune(s, 0)) {
				return nil, errors.New("invalid password configuration")
			}
			out[k] = s
		}
	}
	tls, e := m.object("tls")
	if e != nil {
		return nil, e
	}
	if e := known(tls, "mode ca_file cert_file key_file server_name"); e != nil {
		return nil, e
	}
	mode, e := stringDefault(tls, "mode", "disable")
	if e != nil || !contains([]string{"disable", "require", "verify-ca", "verify-full"}, mode) || mode == "verify-ca" && driver != "postgres" {
		return nil, errors.New("unsupported DB TLS mode")
	}
	t := map[string]any{"mode": mode}
	for _, k := range []string{"ca_file", "cert_file", "key_file", "server_name"} {
		if tls.has(k) {
			v, e := requiredString(tls, k)
			if e != nil {
				return nil, e
			}
			t[k] = v
		}
	}
	if tls.has("cert_file") != tls.has("key_file") {
		return nil, errors.New("DB TLS certificate and key must be paired")
	}
	if (tls.has("cert_file") || tls.has("key_file")) && driver != "postgres" && driver != "mysql" {
		return nil, errors.New("driver does not support client certificate")
	}
	if mode == "disable" && len(t) > 1 {
		return nil, errors.New("TLS fields cannot be used with disabled TLS")
	}
	if tls.has("server_name") && mode != "verify-full" {
		return nil, errors.New("server_name requires verify-full")
	}
	if tls.has("ca_file") && mode != "verify-full" && !(driver == "postgres" && mode == "verify-ca") {
		return nil, errors.New("CA verification requires verification mode")
	}
	out["tls"] = t
	return out, nil
}
func compileFolder(m rawMap) (map[string]any, error) {
	if e := known(m, "root access"); e != nil {
		return nil, e
	}
	r, e := requiredString(m, "root")
	if e != nil || strings.ContainsRune(r, 0) {
		return nil, errors.New("invalid folder root")
	}
	a, e := requiredString(m, "access")
	if e != nil || !contains([]string{"read", "read_write"}, a) {
		return nil, errors.New("folder access must be read or read_write")
	}
	return map[string]any{"root": r, "access": a}, nil
}
func httpURL(s string, origin bool) (*url.URL, error) {
	u, e := url.Parse(s)
	if e != nil || !contains([]string{"http", "https"}, u.Scheme) || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || origin && u.Path != "" && u.Path != "/" {
		return nil, errors.New("invalid HTTP origin/base URL")
	}
	if u.Port() != "" {
		if _, _, e := net.SplitHostPort(u.Host); e != nil {
			return nil, errors.New("invalid HTTP port")
		}
	}
	return u, nil
}
func methods(v any) ([]string, error) {
	a, e := stringsList(v, "methods", true)
	if e != nil || len(a) == 0 {
		return nil, errors.New("methods must be a nonempty list")
	}
	for _, s := range a {
		if !regexp.MustCompile(`^[A-Z]+$`).MatchString(s) {
			return nil, errors.New("invalid HTTP method")
		}
	}
	sort.Strings(a)
	return a, nil
}
func cidrs(v any) ([]string, error) {
	a, e := stringsList(v, "allowed_cidrs", true)
	if e != nil {
		return nil, e
	}
	out := []string{}
	for _, s := range a {
		_, n, e := net.ParseCIDR(s)
		if e != nil {
			return nil, errors.New("invalid allowed CIDR")
		}
		out = append(out, n.String())
	}
	return sortedUnique(out), nil
}
func compileHTTPSource(m rawMap) (map[string]any, error) {
	if e := known(m, "base_url allowed_methods allowed_cidrs credential credential_file headers"); e != nil {
		return nil, e
	}
	base, e := requiredString(m, "base_url")
	if e != nil {
		return nil, e
	}
	if _, e := httpURL(base, false); e != nil {
		return nil, e
	}
	ms, e := methods(m.get("allowed_methods"))
	if e != nil {
		return nil, e
	}
	out := map[string]any{"base_url": base, "allowed_methods": ms}
	if m.has("allowed_cidrs") {
		a, e := cidrs(m.get("allowed_cidrs"))
		if e != nil {
			return nil, e
		}
		out["allowed_cidrs"] = a
	}
	if m.has("credential") && m.has("credential_file") {
		return nil, errors.New("credential and credential_file are exclusive")
	}
	for _, k := range []string{"credential", "credential_file"} {
		if m.has(k) {
			v, e := requiredString(m, k)
			if e != nil || strings.ContainsAny(v, "\r\n\x00") || k == "credential" && strings.HasPrefix(v, "Bearer ") {
				return nil, errors.New("invalid credential configuration")
			}
			out[k] = v
		}
	}
	headers, e := m.object("headers")
	if e != nil {
		return nil, e
	}
	h, e := compileFixedHeaders(headers)
	if e != nil {
		return nil, e
	}
	if _, ok := h["authorization"]; ok && (m.has("credential") || m.has("credential_file")) {
		return nil, errors.New("credential and Authorization header are exclusive")
	}
	out["headers"] = h
	return out, nil
}

var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func validHeader(k string) bool {
	return headerName.MatchString(k) && !contains([]string{"host", "authorization", "connection", "proxy-authorization", "proxy-authenticate", "keep-alive", "upgrade", "te", "trailer"}, strings.ToLower(k))
}
func compileFixedHeaders(m rawMap) (map[string]any, error) {
	out := map[string]any{}
	for _, k := range m.keys {
		low := strings.ToLower(k)
		if !validHeader(k) && strings.ToLower(k) != "authorization" {
			return nil, errors.New("forbidden or invalid HTTP header")
		}
		if _, ok := out[low]; ok {
			return nil, errors.New("case-insensitive duplicate HTTP header")
		}
		v, e := str(m.get(k), "header")
		if e != nil || strings.ContainsAny(v, "\r\n\x00") {
			return nil, errors.New("invalid HTTP header value")
		}
		out[low] = v
	}
	return out, nil
}
