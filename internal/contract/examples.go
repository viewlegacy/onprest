package contract

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
)

// normalizeExampleFiles is a bounded static check of explicitly configured
// public samples. It is not a REST/MCP receiver and creates no artifacts.
func normalizeExampleFiles(s *Schema, v any, pointer string, remaining *int64) (any, error) {
	if v == nil {
		return nil, nil
	}
	if s.File != nil {
		m, ok := v.(map[string]any)
		if !ok || len(m) != 3 {
			return nil, errors.New("file example requires name/mime_type/data")
		}
		name, ok := m["name"].(string)
		if !ok {
			return nil, errors.New("invalid file example name")
		}
		mt, ok := m["mime_type"].(string)
		if !ok {
			return nil, errors.New("invalid file example MIME")
		}
		data, ok := m["data"].(string)
		if !ok || strings.ContainsAny(data, "\r\n\t ") {
			return nil, errors.New("invalid example Base64")
		}
		size := base64.StdEncoding.DecodedLen(len(data))
		if len(data) > 0 {
			if strings.HasSuffix(data, "==") {
				size -= 2
			} else if strings.HasSuffix(data, "=") {
				size--
			}
		}
		if size < 0 || int64(size) > s.File.MaxBytes || int64(size) > *remaining {
			return nil, errors.New("file example exceeds byte limit")
		}
		b, e := base64.StdEncoding.Strict().DecodeString(data)
		if e != nil || base64.StdEncoding.EncodeToString(b) != data {
			return nil, errors.New("file example Base64 must be canonical padded RFC4648")
		}
		*remaining -= int64(len(b))
		digest := sha256.Sum256(b)
		return FileValue{Pointer: pointer, ID: "example:" + pointer, RequestID: "configuration-example", Name: name, MIMEType: mt, SizeBytes: int64(len(b)), SHA256: hex.EncodeToString(digest[:])}, nil
	}
	switch x := v.(type) {
	case map[string]any:
		o := map[string]any{}
		for k, v := range x {
			child := s.Properties[k]
			if child == nil {
				child = s.Additional
			}
			if child == nil {
				o[k] = v
				continue
			}
			nv, e := normalizeExampleFiles(child, v, pointer+"/"+escapePointer(k), remaining)
			if e != nil {
				return nil, e
			}
			o[k] = nv
		}
		return o, nil
	case []any:
		if s.Items == nil {
			return v, nil
		}
		a := make([]any, len(x))
		for i, v := range x {
			nv, e := normalizeExampleFiles(s.Items, v, fmt.Sprintf("%s/%d", pointer, i), remaining)
			if e != nil {
				return nil, e
			}
			a[i] = nv
		}
		return a, nil
	}
	return v, nil
}
func publicExample(v, raw any) any {
	switch x := v.(type) {
	case FileValue:
		return cloneValue(raw)
	case map[string]any:
		o := map[string]any{}
		r, _ := raw.(map[string]any)
		for k, v := range x {
			o[k] = publicExample(v, r[k])
		}
		return o
	case []any:
		o := make([]any, len(x))
		r, _ := raw.([]any)
		for i, v := range x {
			var original any
			if i < len(r) {
				original = r[i]
			}
			o[i] = publicExample(v, original)
		}
		return o
	default:
		return cloneValue(v)
	}
}

// Inline configured file samples have one additional object level and six
// metadata items compared with the logical FileValue. Their Base64 wire bytes
// are bounded before decoding; the common validator then enforces exact logical
// limits after replacing samples with owned metadata.
func exampleWireLimits(l Limits) Limits {
	add := func(a, b int64) int64 {
		if b > math.MaxInt64-a {
			return math.MaxInt64
		}
		return a + b
	}
	multiply := func(a, b int64) int64 {
		if a > math.MaxInt64/b {
			return math.MaxInt64
		}
		return a * b
	}
	if l.MaxFiles > 0 {
		// Padded Base64 needs at most four bytes per three raw bytes; per-file
		// metadata is covered by the logical JSON budget plus field punctuation.
		encoded := multiply(add(l.MaxTotalFileBytes/3, 1), 4)
		l.MaxBytes = add(l.MaxBytes, add(encoded, multiply(l.MaxFiles, 64)))
		l.MaxItems = add(l.MaxItems, multiply(l.MaxFiles, 6))
		l.MaxDepth = add(l.MaxDepth, 1)
	}
	return l
}
