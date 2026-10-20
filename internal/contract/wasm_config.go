package contract

import (
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"
)

func byteLimits(m rawMap, bytes, counts map[string]int64) (map[string]any, error) {
	keys := []string{}
	for k := range bytes {
		keys = append(keys, k)
	}
	for k := range counts {
		keys = append(keys, k)
	}
	if e := known(m, strings.Join(keys, " ")); e != nil {
		return nil, e
	}
	out := map[string]any{}
	for k, d := range bytes {
		v := d
		var e error
		if m.has(k) {
			v, e = ByteSize(m.get(k))
			if e != nil {
				return nil, e
			}
		}
		out[k] = v
	}
	for k, d := range counts {
		v, e := intDefault(m, k, d, 1)
		if e != nil {
			return nil, e
		}
		out[k] = v
	}
	return out, nil
}
func compileWasmExecutor(m rawMap) (map[string]any, error) {
	if e := known(m, "type module sha256 limits sandbox effects"); e != nil {
		return nil, e
	}
	module, e := requiredString(m, "module")
	if e != nil || strings.ContainsRune(module, 0) {
		return nil, errors.New("invalid Wasm module path")
	}
	out := map[string]any{"type": "wasm", "module": module}
	if m.has("sha256") {
		v, e := requiredString(m, "sha256")
		if e != nil || !digestRE.MatchString(v) {
			return nil, errors.New("expected module sha256 must be 64 lowercase hexadecimal characters")
		}
		out["sha256"] = v
	}
	effects, e := stringDefault(m, "effects", "possible")
	if e != nil || !contains([]string{"none", "possible"}, effects) {
		return nil, errors.New("invalid Wasm effects")
	}
	out["effects"] = effects
	limits, e := m.object("limits")
	if e != nil {
		return nil, e
	}
	l, e := byteLimits(limits, map[string]int64{"memory_bytes": 128 << 20, "max_module_bytes": 32 << 20, "max_file_read_bytes": 64 << 20, "max_file_write_bytes": 64 << 20, "max_total_file_read_bytes": 256 << 20, "max_total_file_write_bytes": 256 << 20, "max_http_request_bytes": 32 << 20, "max_http_response_bytes": 32 << 20}, map[string]int64{"max_host_calls": 8, "max_handles": 64, "max_file_operations": 65536})
	if e != nil {
		return nil, e
	}
	for _, pair := range [][2]string{{"max_file_read_bytes", "max_total_file_read_bytes"}, {"max_file_write_bytes", "max_total_file_write_bytes"}} {
		if l[pair[0]].(int64) > l[pair[1]].(int64) {
			return nil, errors.New("single Wasm file budget exceeds total")
		}
	}
	if l["memory_bytes"].(int64) > int64(math.MaxUint32)+1 {
		return nil, errors.New("Wasm memory exceeds address space")
	}
	out["limits"] = l
	sandbox, e := m.object("sandbox")
	if e != nil {
		return nil, e
	}
	if e := known(sandbox, "enabled permissions"); e != nil {
		return nil, e
	}
	enabled, e := boolDefault(sandbox, "enabled", false)
	if e != nil {
		return nil, e
	}
	sb := map[string]any{"enabled": enabled}
	if !enabled && sandbox.has("permissions") {
		return nil, errors.New("sandbox permissions require enabled:true")
	}
	if enabled {
		p, e := sandbox.object("permissions")
		if e != nil {
			return nil, e
		}
		permissions, e := compilePermissions(p)
		if e != nil {
			return nil, e
		}
		sb["permissions"] = permissions
	}
	out["sandbox"] = sb
	return out, nil
}
func compilePermissions(m rawMap) (map[string]any, error) {
	if e := known(m, "filesystem http environment"); e != nil {
		return nil, e
	}
	out := map[string]any{"filesystem": []any{}, "http": []any{}, "environment": []string{}}
	if m.has("filesystem") {
		a, e := list(m.get("filesystem"), "filesystem grants")
		if e != nil {
			return nil, e
		}
		fs := []any{}
		for _, v := range a {
			r, e := object(v, "filesystem grant")
			if e != nil {
				return nil, e
			}
			f, e := compileFolder(r)
			if e != nil {
				return nil, e
			}
			fs = append(fs, f)
		}
		sort.Slice(fs, func(i, j int) bool {
			a, _ := CanonicalJSON(fs[i])
			b, _ := CanonicalJSON(fs[j])
			return string(a) < string(b)
		})
		out["filesystem"] = fs
	}
	if m.has("http") {
		a, e := list(m.get("http"), "HTTP grants")
		if e != nil {
			return nil, e
		}
		grants := []any{}
		for _, v := range a {
			r, e := object(v, "HTTP grant")
			if e != nil {
				return nil, e
			}
			if e := known(r, "origin methods path_prefix allowed_cidrs"); e != nil {
				return nil, e
			}
			origin, e := requiredString(r, "origin")
			if e != nil {
				return nil, e
			}
			u, e := httpURL(origin, true)
			if e != nil {
				return nil, e
			}
			if strings.Contains(u.Host, "*") {
				return nil, errors.New("wildcard HTTP origin is forbidden")
			}
			ms, e := methods(r.get("methods"))
			if e != nil {
				return nil, e
			}
			prefix, e := requiredString(r, "path_prefix")
			if e != nil || !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "?#\x00") {
				return nil, errors.New("invalid HTTP path prefix")
			}
			g := map[string]any{"origin": u.Scheme + "://" + u.Host, "methods": ms, "path_prefix": prefix}
			if r.has("allowed_cidrs") {
				c, e := cidrs(r.get("allowed_cidrs"))
				if e != nil {
					return nil, e
				}
				g["allowed_cidrs"] = c
			}
			grants = append(grants, g)
		}
		sort.Slice(grants, func(i, j int) bool {
			a, _ := CanonicalJSON(grants[i])
			b, _ := CanonicalJSON(grants[j])
			return string(a) < string(b)
		})
		out["http"] = grants
	}
	if m.has("environment") {
		a, e := stringsList(m.get("environment"), "environment", true)
		if e != nil {
			return nil, e
		}
		for _, s := range a {
			if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(s) {
				return nil, errors.New("invalid environment name")
			}
		}
		sort.Strings(a)
		out["environment"] = a
	}
	return out, nil
}
