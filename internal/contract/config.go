package contract

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var capacityRE = regexp.MustCompile(`^([0-9]+)(B|KiB|MiB|GiB|KB|MB|GB)?$`)

// ByteSize accepts positive integer bytes or the documented binary units.
func ByteSize(v any) (int64, error) {
	if n, ok := v.(int64); ok {
		if n > 0 {
			return n, nil
		}
		return 0, errors.New("byte capacity must be positive")
	}
	s, ok := v.(string)
	if !ok {
		return 0, errors.New("byte capacity must be integer or unit string")
	}
	m := capacityRE.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New("invalid byte capacity")
	}
	n, e := strconv.ParseInt(m[1], 10, 64)
	if e != nil || n <= 0 {
		return 0, errors.New("byte capacity must be positive int64")
	}
	mul := int64(1)
	switch m[2] {
	case "KiB", "KB":
		mul = 1 << 10
	case "MiB", "MB":
		mul = 1 << 20
	case "GiB", "GB":
		mul = 1 << 30
	}
	if n > math.MaxInt64/mul {
		return 0, errors.New("byte capacity overflows int64")
	}
	return n * mul, nil
}
func duration(v any, max time.Duration) (string, error) {
	s, e := str(v, "duration")
	if e != nil {
		return "", e
	}
	d, e := time.ParseDuration(s)
	if e != nil || d <= 0 || max > 0 && d > max {
		return "", errors.New("invalid positive finite duration")
	}
	return d.String(), nil
}

// Compile decodes, normalizes, and lints all definitions without reading secret
// files, opening a source, executing a module, or connecting to the Gateway.
func Compile(data []byte) (*Config, error) {
	m, e := decodeYAML(data)
	if e != nil {
		return nil, e
	}
	if !m.has("schema_version") {
		return nil, errors.New("schema_version: 1 is required; replace legacy database/sql/params/result with sources/executor/input/output")
	}
	v, ok := m.get("schema_version").(int64)
	if !ok || v != 1 {
		return nil, errors.New("unsupported schema_version; use schema_version: 1")
	}
	if m.has("database") {
		return nil, errors.New("legacy database is unsupported; use sources.databases")
	}
	if e := known(m, "schema_version service gateway runtime logging defaults sources capabilities"); e != nil {
		return nil, e
	}
	c := &Config{SchemaVersion: 1, Capabilities: map[string]*Capability{}}
	service, e := m.object("service")
	if e != nil {
		return nil, e
	}
	if e := known(service, "title version description"); e != nil {
		return nil, e
	}
	c.Service = map[string]any{}
	for k, d := range map[string]string{"title": "Onprest Agent", "version": "0.1.0", "description": ""} {
		s, e := stringDefault(service, k, d)
		if e != nil {
			return nil, e
		}
		c.Service[k] = s
	}
	gateway, e := m.object("gateway")
	if e != nil {
		return nil, e
	}
	c.Gateway, e = compileGateway(gateway)
	if e != nil {
		return nil, e
	}
	runtime, e := m.object("runtime")
	if e != nil {
		return nil, e
	}
	c.Runtime, e = compileRuntime(runtime)
	if e != nil {
		return nil, e
	}
	logging, e := m.object("logging")
	if e != nil {
		return nil, e
	}
	c.Logging, e = compileLogging(logging)
	if e != nil {
		return nil, e
	}
	sources, e := m.object("sources")
	if e != nil {
		return nil, e
	}
	c.Sources, e = compileSources(sources)
	if e != nil {
		return nil, e
	}
	defaults, e := m.object("defaults")
	if e != nil {
		return nil, e
	}
	defs, e := compilePolicyValues(defaults)
	if e != nil {
		return nil, fmt.Errorf("defaults: %w", e)
	}
	caps, e := m.object("capabilities")
	if e != nil {
		return nil, e
	}
	if len(caps.keys) == 0 {
		return nil, errors.New("at least one capability is required")
	}
	for _, name := range caps.keys {
		if !regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`).MatchString(name) || strings.HasPrefix(name, "onprest_") {
			return nil, errors.New("invalid or reserved capability name")
		}
		raw, e := object(caps.get(name), "capability")
		if e != nil {
			return nil, e
		}
		cap, e := compileCapability(name, raw, defs, c.Sources)
		if e != nil {
			return nil, fmt.Errorf("capability %s: %w", safe(name), e)
		}
		c.Capabilities[name] = cap
	}
	return c, nil
}
func compileGateway(m rawMap) (map[string]any, error) {
	if e := known(m, "url agent_private_key agent_private_key_file allow_insecure_non_loopback_ws"); e != nil {
		return nil, e
	}
	s, e := requiredString(m, "url")
	if e != nil {
		return nil, e
	}
	u, e := url.Parse(s)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/ws/agent" || !contains([]string{"ws", "wss"}, u.Scheme) {
		return nil, errors.New("invalid gateway URL")
	}
	allow, e := boolDefault(m, "allow_insecure_non_loopback_ws", false)
	if e != nil {
		return nil, e
	}
	if u.Scheme == "ws" && !allow {
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("non-loopback ws requires explicit insecure development permission")
		}
	}
	if m.has("agent_private_key") == m.has("agent_private_key_file") {
		return nil, errors.New("specify exactly one agent_private_key or agent_private_key_file")
	}
	key := "agent_private_key"
	if m.has("agent_private_key_file") {
		key = "agent_private_key_file"
	}
	val, e := requiredString(m, key)
	if e != nil {
		return nil, e
	}
	if key == "agent_private_key" {
		b, e := base64.RawURLEncoding.DecodeString(val)
		if e != nil || len(b) != 64 {
			return nil, errors.New("agent_private_key must encode an Ed25519 private key")
		}
	}
	return map[string]any{"url": s, key: val, "allow_insecure_non_loopback_ws": allow}, nil
}
func compileRuntime(m rawMap) (map[string]any, error) {
	if e := known(m, "max_concurrent_requests identity_dir spool_dir spool_total_bytes transfer_timeout input_limits control_limits"); e != nil {
		return nil, e
	}
	out := map[string]any{}
	n, e := intDefault(m, "max_concurrent_requests", 16, 1)
	if e != nil {
		return nil, e
	}
	out["max_concurrent_requests"] = n
	for k, d := range map[string]string{"identity_dir": "./identity", "spool_dir": "./spool"} {
		s, e := stringDefault(m, k, d)
		if e != nil || s == "" || strings.ContainsRune(s, 0) {
			return nil, errors.New("invalid runtime directory")
		}
		out[k] = s
	}
	b := int64(1 << 30)
	if m.has("spool_total_bytes") {
		b, e = ByteSize(m.get("spool_total_bytes"))
		if e != nil {
			return nil, e
		}
	}
	out["spool_total_bytes"] = b
	t := "1m0s"
	if m.has("transfer_timeout") {
		t, e = duration(m.get("transfer_timeout"), 300*time.Second)
		if e != nil {
			return nil, e
		}
	}
	out["transfer_timeout"] = t
	limits, e := m.object("input_limits")
	if e != nil {
		return nil, e
	}
	if e := known(limits, "max_pending_requests max_multipart_parts max_recent_requests"); e != nil {
		return nil, e
	}
	il := map[string]any{}
	for k, d := range map[string]int64{"max_pending_requests": 16, "max_multipart_parts": 128, "max_recent_requests": 4096} {
		n, e := intDefault(limits, k, d, 1)
		if e != nil {
			return nil, e
		}
		il[k] = n
	}
	out["input_limits"] = il
	ctrl, e := m.object("control_limits")
	if e != nil {
		return nil, e
	}
	if e := known(ctrl, "read write cancel transfer bytes_per_second burst_bytes max_concurrent_transfers"); e != nil {
		return nil, e
	}
	co := map[string]any{}
	for k, d := range map[string][3]int64{"read": {60, 120, 8}, "write": {10, 20, 2}, "cancel": {20, 40, 2}, "transfer": {32, 64, 4}} {
		v, e := ctrl.object(k)
		if e != nil {
			return nil, e
		}
		if e := known(v, "requests per burst max_concurrent"); e != nil {
			return nil, e
		}
		vm := map[string]any{}
		for key, def := range map[string]int64{"requests": d[0], "burst": d[1], "max_concurrent": d[2]} {
			n, e := intDefault(v, key, def, 1)
			if e != nil {
				return nil, e
			}
			vm[key] = n
		}
		p := "1s"
		if v.has("per") {
			p, e = duration(v.get("per"), 0)
			if e != nil {
				return nil, e
			}
		}
		pd, _ := time.ParseDuration(p)
		if vm["burst"].(int64) > math.MaxInt64/int64(pd) {
			return nil, errors.New("control rate arithmetic overflows")
		}
		vm["per"] = p
		co[k] = vm
	}
	for k, d := range map[string]int64{"bytes_per_second": 8 << 20, "burst_bytes": 16 << 20} {
		n := d
		if ctrl.has(k) {
			n, e = ByteSize(ctrl.get(k))
			if e != nil {
				return nil, e
			}
		}
		co[k] = n
	}
	n, e = intDefault(ctrl, "max_concurrent_transfers", 4, 1)
	if e != nil {
		return nil, e
	}
	co["max_concurrent_transfers"] = n
	out["control_limits"] = co
	return out, nil
}
func compileLogging(m rawMap) (map[string]any, error) {
	if e := known(m, "level max_size max_files"); e != nil {
		return nil, e
	}
	level, e := stringDefault(m, "level", "info")
	if e != nil || !contains([]string{"debug", "info", "warn", "error"}, level) {
		return nil, errors.New("invalid log level")
	}
	size := int64(10 << 20)
	if m.has("max_size") {
		size, e = ByteSize(m.get("max_size"))
		if e != nil {
			return nil, e
		}
	}
	n, e := intDefault(m, "max_files", 3, 1)
	if e != nil {
		return nil, e
	}
	return map[string]any{"level": level, "max_size": size, "max_files": n}, nil
}

var policyDefaults = map[string]any{"timeout": "5s", "expose_in_openapi": true, "max_input_bytes": int64(1 << 20), "max_json_depth": int64(32), "max_json_items": int64(100000), "readonly": true, "max_rows": int64(100), "max_output_bytes": int64(1 << 20), "max_input_file_bytes": int64(32 << 20), "max_total_input_file_bytes": int64(64 << 20), "max_input_files": int64(16), "max_output_file_bytes": int64(32 << 20), "max_total_output_file_bytes": int64(64 << 20), "max_output_files": int64(16), "file_ttl": "15m0s", "mcp_inline_total_bytes": int64(1 << 20)}

const policyKeys = "timeout expose_in_openapi rate_limit max_input_bytes max_json_depth max_json_items readonly max_rows max_affected_rows max_output_bytes max_input_file_bytes max_total_input_file_bytes max_input_files max_output_file_bytes max_total_output_file_bytes max_output_files file_ttl mcp_inline_total_bytes"

func compilePolicyValues(m rawMap) (map[string]any, error) {
	if m.has("max_bytes") {
		return nil, errors.New("policy/defaults max_bytes is obsolete; use max_output_bytes")
	}
	if e := known(m, policyKeys); e != nil {
		return nil, e
	}
	out := map[string]any{}
	for _, k := range m.keys {
		v := m.get(k)
		var nv any
		var e error
		switch {
		case k == "timeout":
			nv, e = duration(v, 300*time.Second)
		case k == "file_ttl":
			nv, e = duration(v, 0)
		case k == "readonly" || k == "expose_in_openapi":
			b, ok := v.(bool)
			if !ok {
				e = errors.New("must be boolean")
			} else {
				nv = b
			}
		case k == "rate_limit":
			var r rawMap
			r, e = object(v, k)
			if e == nil {
				nv, e = compileRate(r)
			}
		case strings.Contains(k, "bytes"):
			nv, e = ByteSize(v)
		default:
			n, ok := v.(int64)
			if !ok || n <= 0 {
				e = errors.New("must be positive int64 count")
			} else {
				nv = n
			}
		}
		if e != nil {
			return nil, fmt.Errorf("%s: %w", safe(k), e)
		}
		out[k] = nv
	}
	return out, nil
}
func compileRate(m rawMap) (map[string]any, error) {
	if e := known(m, "requests per burst"); e != nil {
		return nil, e
	}
	for _, k := range []string{"requests", "per", "burst"} {
		if !m.has(k) {
			return nil, errors.New("rate_limit requires requests/per/burst")
		}
	}
	r, e := intDefault(m, "requests", 0, 1)
	if e != nil {
		return nil, e
	}
	b, e := intDefault(m, "burst", 0, 1)
	if e != nil {
		return nil, e
	}
	p, e := duration(m.get("per"), 0)
	if e != nil {
		return nil, e
	}
	pd, _ := time.ParseDuration(p)
	if b > math.MaxInt64/int64(pd) {
		return nil, errors.New("rate_limit arithmetic overflows")
	}
	return map[string]any{"requests": r, "per": p, "burst": b}, nil
}
func resolvePolicy(c *Capability, direct, defaults map[string]any) (map[string]any, error) {
	allowed := map[string]bool{}
	for _, k := range strings.Fields("timeout expose_in_openapi rate_limit max_input_bytes max_json_depth max_json_items") {
		allowed[k] = true
	}
	if c.Executor.Type == "db" {
		allowed["readonly"] = true
		if c.Executor.Operation == "select" {
			allowed["max_rows"] = true
		} else {
			allowed["max_affected_rows"] = true
		}
	}
	if c.Executor.Type == "csv" {
		allowed["max_rows"] = true
	}
	if c.Input.HasFiles() {
		for _, k := range strings.Fields("max_input_file_bytes max_total_input_file_bytes max_input_files") {
			allowed[k] = true
		}
	}
	if c.Output.Data || c.Output.Files != nil && c.Output.Files["format"] == "csv" {
		allowed["max_output_bytes"] = true
	}
	if c.Output.Files != nil {
		for _, k := range strings.Fields("max_output_file_bytes max_total_output_file_bytes max_output_files file_ttl") {
			allowed[k] = true
		}
		if c.Output.Files["delivery"].(map[string]any)["mcp"] == "inline" {
			allowed["mcp_inline_total_bytes"] = true
		}
	}
	out := map[string]any{}
	for k := range direct {
		if !allowed[k] {
			return nil, fmt.Errorf("policy %s does not apply to this capability", safe(k))
		}
	}
	for k := range allowed {
		if v, ok := direct[k]; ok {
			out[k] = cloneValue(v)
		} else if v, ok := defaults[k]; ok {
			out[k] = cloneValue(v)
		} else if v, ok := policyDefaults[k]; ok {
			out[k] = cloneValue(v)
		}
	}
	for _, pair := range [][2]string{{"max_input_file_bytes", "max_total_input_file_bytes"}, {"max_output_file_bytes", "max_total_output_file_bytes"}} {
		a, aok := out[pair[0]].(int64)
		b, bok := out[pair[1]].(int64)
		if aok && bok && a > b {
			return nil, errors.New("single file capacity exceeds total capacity")
		}
	}
	if c.Executor.Type == "db" && c.Executor.Operation != "select" && out["readonly"] != false {
		return nil, errors.New("DB DML requires readonly:false")
	}
	return out, nil
}
func sortedUnique(a []string) []string {
	sort.Strings(a)
	out := []string{}
	for _, s := range a {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
