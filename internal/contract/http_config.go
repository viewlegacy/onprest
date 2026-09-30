package contract

import (
	"errors"
	"net/url"
	"strings"
)

func compileHTTPExecutor(m rawMap, input *Schema, sources Sources) (map[string]any, error) {
	if e := known(m, "type source method path query headers body response limits effects"); e != nil {
		return nil, e
	}
	source, e := requiredString(m, "source")
	if e != nil {
		return nil, e
	}
	src, ok := sources.HTTP[source]
	if !ok {
		return nil, errors.New("unknown HTTP source")
	}
	method, e := requiredString(m, "method")
	if e != nil || !contains(src["allowed_methods"].([]string), method) {
		return nil, errors.New("HTTP method is not allowed by source")
	}
	p, e := requiredString(m, "path")
	if e != nil {
		return nil, e
	}
	u, e := url.Parse(p)
	if e != nil || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(p, "\x00\r\n") {
		return nil, errors.New("HTTP path must be fixed absolute path without query")
	}
	effects, e := stringDefault(m, "effects", "possible")
	if e != nil || !contains([]string{"none", "possible"}, effects) {
		return nil, errors.New("invalid HTTP effects")
	}
	out := map[string]any{"type": "http", "source": source, "method": method, "path": p, "effects": effects}
	query, e := m.object("query")
	if e != nil {
		return nil, e
	}
	q, e := compileBindingMap(query, input, "many", false)
	if e != nil {
		return nil, e
	}
	out["query"] = q
	headers, e := m.object("headers")
	if e != nil {
		return nil, e
	}
	h, e := compileBindingMap(headers, input, "scalar", true)
	if e != nil {
		return nil, e
	}
	sourceHeaders := src["headers"].(map[string]any)
	for k := range h {
		if _, ok := sourceHeaders[k]; ok {
			return nil, errors.New("duplicate source/executor HTTP header")
		}
	}
	out["headers"] = h
	if m.has("body") {
		body, e := object(m.get("body"), "HTTP body")
		if e != nil {
			return nil, e
		}
		b, e := compileHTTPBody(body, input)
		if e != nil {
			return nil, e
		}
		out["body"] = b
		for _, key := range []string{"content-type", "content-length", "transfer-encoding"} {
			if _, ok := h[key]; ok {
				return nil, errors.New("body header is managed by Agent")
			}
			if _, ok := sourceHeaders[key]; ok {
				return nil, errors.New("body header is managed by Agent")
			}
		}
	}
	response, e := object(m.get("response"), "HTTP response")
	if e != nil {
		return nil, e
	}
	resp, e := compileHTTPResponse(response)
	if e != nil {
		return nil, e
	}
	out["response"] = resp
	limits, e := m.object("limits")
	if e != nil {
		return nil, e
	}
	ls, e := byteLimits(limits, map[string]int64{"max_http_request_bytes": 32 << 20, "max_http_response_bytes": 32 << 20}, nil)
	if e != nil {
		return nil, e
	}
	out["limits"] = ls
	control := map[string]any{"path": p, "query": q, "headers": h}
	if m.has("body") {
		control["body"] = out["body"]
	}
	b, e := CanonicalJSON(control)
	if e != nil {
		return nil, e
	}
	if len(b) > 65536 {
		return nil, errors.New("HTTP binding control exceeds 64KiB")
	}
	return out, nil
}
func collectFixed(v any, out *[]any) {
	switch x := v.(type) {
	case map[string]any:
		if value, ok := x["value"]; ok {
			*out = append(*out, value)
		}
		for k, v := range x {
			if k != "value" {
				collectFixed(v, out)
			}
		}
	case []any:
		for _, v := range x {
			collectFixed(v, out)
		}
	}
}
func compileBindingMap(m rawMap, input *Schema, mode string, headers bool) (map[string]any, error) {
	out := map[string]any{}
	for _, name := range m.keys {
		k := name
		if name == "" || strings.ContainsAny(name, "\r\n\x00") {
			return nil, errors.New("invalid HTTP field name")
		}
		if headers {
			if !validHeader(name) {
				return nil, errors.New("invalid or forbidden HTTP header")
			}
			k = strings.ToLower(name)
			if _, ok := out[k]; ok {
				return nil, errors.New("case-insensitive duplicate header")
			}
		}
		r, e := object(m.get(name), "binding")
		if e != nil {
			return nil, e
		}
		b, e := binding(input, r, mode)
		if e != nil {
			return nil, e
		}
		if headers {
			if v, ok := b["value"].(string); ok && strings.ContainsAny(v, "\r\n\x00") {
				return nil, errors.New("invalid fixed header value")
			}
		}
		out[k] = b
	}
	return out, nil
}
func compileHTTPBody(m rawMap, input *Schema) (map[string]any, error) {
	kind, e := requiredString(m, "kind")
	if e != nil {
		return nil, e
	}
	keys := "kind"
	switch kind {
	case "json":
		keys += " fields input value"
	case "form":
		keys += " fields"
	case "multipart":
		keys += " parts"
	case "file":
		keys += " input content_type"
	default:
		return nil, errors.New("invalid HTTP body kind")
	}
	if e := known(m, keys); e != nil {
		return nil, e
	}
	out := map[string]any{"kind": kind}
	switch kind {
	case "json":
		n := 0
		for _, k := range []string{"fields", "input", "value"} {
			if m.has(k) {
				n++
			}
		}
		if n != 1 {
			return nil, errors.New("JSON body requires exactly one fields/input/value")
		}
		if m.has("fields") {
			f, e := object(m.get("fields"), "body fields")
			if e != nil {
				return nil, e
			}
			fields, e := compileBindingMap(f, input, "json", false)
			if e != nil {
				return nil, e
			}
			out["fields"] = fields
		} else if m.has("input") {
			p, s, e := requirePointer(input, m.get("input"), "", true, false)
			if e != nil {
				return nil, e
			}
			if s.HasFiles() {
				return nil, errors.New("JSON body cannot contain file")
			}
			out["input"] = p
		} else {
			v := plain(m.get("value"))
			if _, e := CanonicalJSON(v); e != nil {
				return nil, e
			}
			out["value"] = v
		}
	case "form":
		f, e := object(m.get("fields"), "form fields")
		if e != nil {
			return nil, e
		}
		fields, e := compileBindingMap(f, input, "many", false)
		if e != nil {
			return nil, e
		}
		out["fields"] = fields
	case "file":
		p, _, e := requirePointer(input, m.get("input"), "file", true, true)
		if e != nil {
			return nil, e
		}
		out["input"] = p
		if m.has("content_type") {
			ct, e := requiredString(m, "content_type")
			if e != nil || !validMIME(ct) {
				return nil, errors.New("invalid body content_type")
			}
			out["content_type"] = ct
		}
	case "multipart":
		parts, e := list(m.get("parts"), "parts")
		if e != nil {
			return nil, e
		}
		result := []any{}
		names := map[string]bool{}
		for _, v := range parts {
			r, e := object(v, "part")
			if e != nil {
				return nil, e
			}
			k, e := requiredString(r, "kind")
			if e != nil || !contains([]string{"text", "json", "file"}, k) {
				return nil, errors.New("invalid multipart part kind")
			}
			allowed := "name kind input value"
			if k == "file" {
				allowed = "name kind input content_type"
			}
			if e := known(r, allowed); e != nil {
				return nil, e
			}
			name, e := requiredString(r, "name")
			if e != nil || strings.ContainsAny(name, "\r\n\x00") {
				return nil, errors.New("invalid part name")
			}
			if names[name] {
				return nil, errors.New("duplicate multipart part name")
			}
			names[name] = true
			part := map[string]any{"name": name, "kind": k}
			if k == "file" {
				p, _, e := requirePointer(input, r.get("input"), "files", false, true)
				if e != nil {
					return nil, e
				}
				part["input"] = p
				if r.has("content_type") {
					ct, e := requiredString(r, "content_type")
					if e != nil || !validMIME(ct) {
						return nil, errors.New("invalid part content_type")
					}
					part["content_type"] = ct
				}
			} else {
				b := emptyMap()
				for _, key := range []string{"input", "value"} {
					if r.has(key) {
						b.keys = append(b.keys, key)
						b.values[key] = r.get(key)
					}
				}
				mode := "json"
				if k == "text" {
					mode = "scalar"
				}
				binding, e := binding(input, b, mode)
				if e != nil {
					return nil, e
				}
				for k, v := range binding {
					part[k] = v
				}
			}
			result = append(result, part)
		}
		out["parts"] = result
	}
	return out, nil
}
func compileHTTPResponse(m rawMap) (map[string]any, error) {
	if e := known(m, "kind success_statuses mime_type filename redirect"); e != nil {
		return nil, e
	}
	kind, e := requiredString(m, "kind")
	if e != nil || !contains([]string{"json", "file"}, kind) {
		return nil, errors.New("HTTP response kind must be json or file")
	}
	out := map[string]any{"kind": kind}
	statuses := []any{}
	if m.has("success_statuses") {
		a, e := list(m.get("success_statuses"), "success_statuses")
		if e != nil || len(a) == 0 {
			return nil, errors.New("success_statuses must be a nonempty set")
		}
		seen := map[int64]bool{}
		for _, v := range a {
			n, ok := v.(int64)
			if !ok || n < 100 || n > 599 || seen[n] {
				return nil, errors.New("invalid or duplicate HTTP success status")
			}
			seen[n] = true
		}
		for i := int64(100); i <= 599; i++ {
			if seen[i] {
				statuses = append(statuses, i)
			}
		}
	} else {
		for i := int64(200); i <= 299; i++ {
			statuses = append(statuses, i)
		}
	}
	out["success_statuses"] = statuses
	if kind == "file" {
		mt, e := stringDefault(m, "mime_type", "application/octet-stream")
		if e != nil || !validMIME(mt) {
			return nil, errors.New("invalid response MIME")
		}
		filename, e := stringDefault(m, "filename", "response.bin")
		if e != nil {
			return nil, e
		}
		if e := ValidateBasename(filename); e != nil {
			return nil, e
		}
		out["mime_type"] = mt
		out["filename"] = filename
	} else if m.has("mime_type") || m.has("filename") {
		return nil, errors.New("response file settings require kind:file")
	}
	r, e := m.object("redirect")
	if e != nil {
		return nil, e
	}
	if e := known(r, "enabled max_hops"); e != nil {
		return nil, e
	}
	enabled, e := boolDefault(r, "enabled", false)
	if e != nil {
		return nil, e
	}
	hops, e := intDefault(r, "max_hops", 3, 1)
	if e != nil {
		return nil, e
	}
	out["redirect"] = map[string]any{"enabled": enabled, "max_hops": hops}
	return out, nil
}
