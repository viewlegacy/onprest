package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/viewlegacy/onprest/internal/valueformat"
	"math"
	"mime"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var extensionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func compileFile(m rawMap) (*FileConstraints, error) {
	f := &FileConstraints{AllowEmpty: true, Extensions: []string{}, MIMETypes: []string{}}
	var e error
	if m.has("extensions") {
		a, e := list(m.get("extensions"), "extensions")
		if e != nil {
			return nil, e
		}
		seen := map[string]bool{}
		for _, v := range a {
			s, e := str(v, "extensions")
			if e != nil || !extensionRE.MatchString(s) {
				return nil, errors.New("invalid file extension")
			}
			s = strings.ToLower(s)
			if !seen[s] {
				f.Extensions = append(f.Extensions, s)
				seen[s] = true
			}
		}
		sort.Strings(f.Extensions)
	}
	if m.has("mime_types") {
		a, e := stringsList(m.get("mime_types"), "mime_types", true)
		if e != nil {
			return nil, e
		}
		for _, s := range a {
			if !validMIME(s) {
				return nil, errors.New("invalid lowercase MIME type")
			}
		}
		sort.Strings(a)
		f.MIMETypes = a
	}
	if m.has("max_bytes") {
		f.MaxBytes, e = ByteSize(m.get("max_bytes"))
		if e != nil {
			return nil, e
		}
	}
	f.AllowEmpty, e = boolDefault(m, "allow_empty", true)
	if e != nil {
		return nil, e
	}
	return f, nil
}
func validMIME(s string) bool {
	m, p, e := mime.ParseMediaType(s)
	return e == nil && m == s && len(p) == 0 && s == strings.ToLower(s) && strings.Contains(s, "/") && !strings.Contains(s, "*")
}
func ValidateBasename(s string) error {
	if !utf8.ValidString(s) || len(s) < 1 || len(s) > 255 || s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
		return errors.New("invalid file basename")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("invalid file basename")
		}
	}
	return nil
}
func (f *FileConstraints) Validate(v FileValue) error {
	if e := ValidateBasename(v.Name); e != nil {
		return e
	}
	if !validMIME(v.MIMEType) {
		return errors.New("invalid file MIME type")
	}
	if v.SizeBytes < 0 || f.MaxBytes <= 0 || v.SizeBytes > f.MaxBytes {
		return errors.New("file byte limit exceeded")
	}
	if v.SizeBytes == 0 && !f.AllowEmpty {
		return errors.New("empty file is forbidden")
	}
	if !digestRE.MatchString(v.SHA256) {
		return errors.New("invalid file digest")
	}
	if len(f.MIMETypes) > 0 && !contains(f.MIMETypes, v.MIMEType) {
		return errors.New("file MIME type is forbidden")
	}
	if len(f.Extensions) > 0 {
		i := strings.LastIndexByte(v.Name, '.')
		if i <= 0 || i == len(v.Name)-1 || !contains(f.Extensions, asciiLower(v.Name[i+1:])) {
			return errors.New("file extension is forbidden")
		}
	}
	return nil
}
func asciiLower(s string) string {
	b := []byte(s)
	for i, v := range b {
		if v >= 'A' && v <= 'Z' {
			b[i] = v + 32
		}
	}
	return string(b)
}

// ResolveFileLimits applies policy to every file leaf before binding lint.
func (s *Schema) resolveFileLimits(max int64) error {
	if s.File != nil {
		if max <= 0 {
			return errors.New("file policy is missing")
		}
		if s.File.MaxBytes == 0 {
			s.File.MaxBytes = max
		} else if s.File.MaxBytes > max {
			return errors.New("file max_bytes exceeds policy")
		}
		s.normalized["max_bytes"] = s.File.MaxBytes
	}
	for k, c := range s.Properties {
		if e := c.resolveFileLimits(max); e != nil {
			return e
		}
		if s.PropertiesSet {
			s.normalized["properties"].(map[string]any)[k] = c.Normalized()
		}
	}
	if s.Items != nil {
		if e := s.Items.resolveFileLimits(max); e != nil {
			return e
		}
		s.normalized["items"] = s.Items.Normalized()
	}
	return nil
}

// ValidateInput applies defaults only to omitted properties and rejects unknown
// values. File leaves must already be receiver-owned FileValues, never inline
// wire objects or host paths. The returned leaves preserve input array order.
func (c *Capability) ValidateInput(input map[string]any) (map[string]any, []FileLeaf, error) {
	return ValidateInput(c.Input, input, c.Limits())
}
func ValidateInput(s *Schema, input map[string]any, l Limits) (map[string]any, []FileLeaf, error) {
	if input == nil {
		input = map[string]any{}
	}
	if e := checkJSONLimits(input, l); e != nil {
		return nil, nil, e
	}
	leaves := []FileLeaf{}
	v, e := s.validate(input, "", true, &leaves, l.MaxFileBytes)
	if e != nil {
		return nil, nil, e
	}
	if l.MaxFiles > 0 && int64(len(leaves)) > l.MaxFiles {
		return nil, nil, errors.New("input file count limit exceeded")
	}
	total := int64(0)
	for _, f := range leaves {
		if l.MaxFileBytes > 0 && f.Value.SizeBytes > l.MaxFileBytes {
			return nil, nil, errors.New("input file byte limit exceeded")
		}
		if f.Value.SizeBytes > math.MaxInt64-total {
			return nil, nil, errors.New("input file bytes overflow")
		}
		total += f.Value.SizeBytes
	}
	if l.MaxTotalFileBytes > 0 && total > l.MaxTotalFileBytes {
		return nil, nil, errors.New("input total file byte limit exceeded")
	}
	if e := checkJSONLimits(v, l); e != nil {
		return nil, nil, e
	}
	return v.(map[string]any), leaves, nil
}

// CanonicalInput omits opaque IDs, local paths, and transport representation.
func CanonicalInput(v any) ([]byte, error) { return CanonicalJSON(canonicalInput(v)) }
func canonicalInput(v any) any {
	switch x := v.(type) {
	case FileValue:
		return map[string]any{"name": x.Name, "mime_type": x.MIMEType, "size_bytes": x.SizeBytes, "sha256": x.SHA256}
	case map[string]any:
		m := map[string]any{}
		for k, v := range x {
			m[k] = canonicalInput(v)
		}
		return m
	case []any:
		a := make([]any, len(x))
		for i, v := range x {
			a[i] = canonicalInput(v)
		}
		return a
	default:
		return x
	}
}
func checkJSONLimits(v any, l Limits) error {
	if l.MaxBytes <= 0 || l.MaxDepth <= 0 || l.MaxItems <= 0 {
		return errors.New("input JSON limits must be positive")
	}
	count := int64(0)
	bytesUsed := int64(0)
	addBytes := func(n int64) error {
		if n < 0 || n > l.MaxBytes-bytesUsed {
			return errors.New("input JSON byte limit exceeded")
		}
		bytesUsed += n
		return nil
	}
	active := map[uintptr]bool{}
	var visit func(any, int64) error
	visit = func(v any, d int64) error {
		count++
		if d > l.MaxDepth || count > l.MaxItems {
			return errors.New("input JSON structural limit exceeded")
		}
		switch x := v.(type) {
		case nil:
			if e := addBytes(4); e != nil {
				return e
			}
		case bool:
			n := int64(5)
			if x {
				n = 4
			}
			if e := addBytes(n); e != nil {
				return e
			}
		case string:
			n, e := jsonStringBytes(x)
			if e != nil {
				return e
			}
			if e := addBytes(n); e != nil {
				return e
			}
		case int64:
			if e := addBytes(int64(len(strconv.FormatInt(x, 10)))); e != nil {
				return e
			}
		case int:
			if e := addBytes(int64(len(strconv.FormatInt(int64(x), 10)))); e != nil {
				return e
			}
		case float64, json.Number:
			b, e := CanonicalJSON(x)
			if e != nil {
				return e
			}
			if e := addBytes(int64(len(b))); e != nil {
				return e
			}
		case FileValue:
			if e := ValidateBasename(x.Name); e != nil {
				return e
			}
			n := int64(5 + len(strconv.FormatInt(x.SizeBytes, 10)))
			for _, key := range []string{"name", "mime_type", "size_bytes", "sha256"} {
				k, _ := jsonStringBytes(key)
				n += k + 1
			}
			for _, value := range []string{x.Name, x.MIMEType, x.SHA256} {
				k, e := jsonStringBytes(value)
				if e != nil {
					return e
				}
				if k > l.MaxBytes-n {
					return errors.New("input JSON byte limit exceeded")
				}
				n += k
			}
			if e := addBytes(n); e != nil {
				return e
			}
		case map[string]any:
			if e := addBytes(2 + int64(max(0, len(x)-1))); e != nil {
				return e
			}

			ptr := reflect.ValueOf(x).Pointer()
			if active[ptr] {
				return errors.New("cyclic input object")
			}
			active[ptr] = true
			defer delete(active, ptr)
			for k, v := range x {
				n, e := jsonStringBytes(k)
				if e != nil {
					return e
				}
				if e := addBytes(n + 1); e != nil {
					return e
				}
				count++
				if count > l.MaxItems {
					return errors.New("input JSON item limit exceeded")
				}
				if e := visit(v, d+1); e != nil {
					return e
				}
			}
		case []any:
			if e := addBytes(2 + int64(max(0, len(x)-1))); e != nil {
				return e
			}
			ptr := reflect.ValueOf(x).Pointer()
			if ptr != 0 && active[ptr] {
				return errors.New("cyclic input array")
			}
			if ptr != 0 {
				active[ptr] = true
				defer delete(active, ptr)
			}
			for _, v := range x {
				if e := visit(v, d+1); e != nil {
					return e
				}
			}
		default:
			return errors.New("unsupported input JSON value")
		}
		return nil
	}
	if e := visit(v, 1); e != nil {
		return e
	}
	return nil
}
func (s *Schema) validate(v any, pointer string, defaults bool, leaves *[]FileLeaf, fileLimits ...int64) (any, error) {
	fail := func(reason string) (any, error) { return nil, fmt.Errorf("%s: %s", safe(pointer), reason) }
	if v == nil {
		// Output nullable intentionally bypasses value constraints. Input null
		// still has to belong to its explicitly permitted enum/const domain.
		if s.Nullable {
			return nil, nil
		}
		if s.output {
			return fail("null is not allowed")
		}
		if len(s.Types) != 0 && !contains(s.Types, "null") {
			return fail("null is not allowed")
		}
		if e := s.validateEnumConst(nil); e != nil {
			return fail(e.Error())
		}
		return nil, nil
	}
	if s.File != nil {
		f, ok := v.(FileValue)
		if !ok {
			return fail("must be a receiver-owned file")
		}
		if f.ID == "" || f.RequestID == "" {
			return fail("file ownership is missing")
		}
		if f.Pointer != pointer {
			return fail("file input position does not match")
		}
		// CompileSchema has no capability policy to resolve omitted max_bytes.
		// Use a request-local constraint copy so concurrent users of the same
		// schema can apply different limits without mutating its definition.
		effective := *s.File
		if len(fileLimits) > 0 && fileLimits[0] > 0 {
			if effective.MaxBytes == 0 {
				effective.MaxBytes = fileLimits[0]
			} else {
				effective.MaxBytes = min(effective.MaxBytes, fileLimits[0])
			}
		}
		if e := effective.Validate(f); e != nil {
			return fail(e.Error())
		}
		if leaves != nil {
			leafSchema := *s
			leafSchema.File = &effective
			leafSchema.normalized = cloneMap(s.normalized)
			leafSchema.normalized["max_bytes"] = effective.MaxBytes
			*leaves = append(*leaves, FileLeaf{pointer, f, &leafSchema})
		}
		return f, nil
	}
	typ := ""
	switch v.(type) {
	case string:
		typ = "string"
	case bool:
		typ = "boolean"
	case map[string]any:
		typ = "object"
	case []any:
		typ = "array"
	case int64, int, json.Number, float64:
		typ = "number"
	default:
		return fail("unsupported JSON value")
	}
	if typ == "number" {
		if contains(s.Types, "integer") {
			if n, ok := exactInteger(v); ok {
				typ = "integer"
				v = n
			} else if !contains(s.Types, "number") {
				return fail("must be int64 integer")
			}
		}
		if typ == "number" {
			n, ok := finiteNumber(v)
			if !ok {
				return fail("must be finite number")
			}
			if len(s.Types) == 0 {
				switch raw := v.(type) {
				case int64:
					v = raw
				case int:
					v = int64(raw)
				case json.Number:
					if !strings.ContainsAny(string(raw), ".eE") {
						integer, err := raw.Int64()
						if err != nil {
							return fail("integer exceeds int64")
						}
						v = integer
					} else {
						v = n
					}
				default:
					v = n
				}
			} else {
				// Output validation must not round a provider's int64 through
				// float64 merely because the declaration also accepts numbers.
				if s.output {
					switch raw := v.(type) {
					case int64:
						v = raw
					case int:
						v = int64(raw)
					default:
						v = n
					}
				} else {
					v = n
				}
			}
		}
	}
	if len(s.Types) > 0 && !contains(s.Types, typ) && !(typ == "integer" && contains(s.Types, "number")) {
		return fail("type does not match schema")
	}
	switch x := v.(type) {
	case string:
		if !utf8.ValidString(x) {
			return fail("must be valid UTF-8")
		}
		n := int64(utf8.RuneCountInString(x))
		if lo, ok := s.Constraints["minLength"].(int64); ok && n < lo {
			return fail("below minLength")
		}
		if hi, ok := s.Constraints["maxLength"].(int64); ok && n > hi {
			return fail("above maxLength")
		}
		if s.Pattern != nil && !s.Pattern.MatchString(x) {
			return fail("does not match pattern")
		}
		if f, ok := s.Constraints["format"].(string); ok {
			if e := valueformat.Validate(f, x); e != nil {
				return fail(e.Error())
			}
		}
	case map[string]any:
		o := map[string]any{}
		for k := range x {
			if !utf8.ValidString(k) {
				return fail("invalid UTF-8 key")
			}
			if _, ok := s.Properties[k]; !ok && len(s.Types) > 0 && contains(s.Types, "object") && !s.AdditionalAllowed {
				return fail("unknown property")
			}
		}
		for _, k := range s.PropertyOrder {
			child := s.Properties[k]
			val, exists := x[k]
			if !exists && defaults && child.DefaultSet {
				val = cloneValue(child.Default)
				exists = true
			}
			if !exists {
				if contains(s.Required, k) {
					return fail("required property missing")
				}
				continue
			}
			nv, e := child.validate(val, pointer+"/"+escapePointer(k), defaults, leaves, fileLimits...)
			if e != nil {
				return nil, e
			}
			o[k] = nv
		}
		keys := []string{}
		for k := range x {
			if _, ok := s.Properties[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := s.Additional
			if child == nil {
				child = &Schema{Nullable: true, Constraints: map[string]any{}}
			}
			nv, e := child.validate(x[k], pointer+"/"+escapePointer(k), defaults, leaves, fileLimits...)
			if e != nil {
				return nil, e
			}
			o[k] = nv
		}
		v = o
	case []any:
		if lo, ok := s.Constraints["minItems"].(int64); ok && int64(len(x)) < lo {
			return fail("below minItems")
		}
		if hi, ok := s.Constraints["maxItems"].(int64); ok && int64(len(x)) > hi {
			return fail("above maxItems")
		}
		a := make([]any, len(x))
		child := s.Items
		if child == nil {
			child = &Schema{Nullable: true, Constraints: map[string]any{}}
		}
		for i, val := range x {
			nv, e := child.validate(val, fmt.Sprintf("%s/%d", pointer, i), defaults, leaves, fileLimits...)
			if e != nil {
				return nil, e
			}
			a[i] = nv
		}
		v = a
	}
	if typ == "integer" || typ == "number" {
		for _, k := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
			if bound, ok := s.Constraints[k]; ok {
				cmp := compareNumbers(v, bound)
				if k == "minimum" && cmp < 0 || k == "maximum" && cmp > 0 || k == "exclusiveMinimum" && cmp <= 0 || k == "exclusiveMaximum" && cmp >= 0 {
					return fail("outside numeric bounds")
				}
			}
		}
	}
	if e := s.validateEnumConst(v); e != nil {
		return fail(e.Error())
	}
	return v, nil
}
func (s *Schema) validateEnumConst(v any) error {
	if a, ok := s.Constraints["enum"].([]any); ok {
		found := false
		for _, candidate := range a {
			if equalJSON(v, candidate) {
				found = true
				break
			}
		}
		if !found {
			return errors.New("not in enum")
		}
	}
	if c, ok := s.Constraints["const"]; ok && !equalJSON(v, c) {
		return errors.New("does not equal const")
	}
	return nil
}

func exactInteger(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		return JSONNumberInt64(string(n))
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || math.Abs(n) > 1<<53 {
			return 0, false
		}
		return int64(n), true
	}
	return 0, false
}
func finiteNumber(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case int64:
		f = float64(n)
	case int:
		f = float64(n)
	case float64:
		f = n
	case json.Number:
		var e error
		f, e = n.Float64()
		if e != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}
func equalJSON(a, b any) bool {
	aa, e := CanonicalJSON(a)
	bb, f := CanonicalJSON(b)
	return e == nil && f == nil && string(aa) == string(bb)
}
func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// jsonStringBytes matches encoding/json with HTML escaping disabled, without
// allocating a second copy of an over-limit string.
func jsonStringBytes(s string) (int64, error) {
	if !utf8.ValidString(s) {
		return 0, errors.New("invalid UTF-8 string")
	}
	n := int64(2)
	for _, r := range s {
		switch r {
		case '\\', '"', '\b', '\f', '\n', '\r', '\t':
			n += 2
		case '\u2028', '\u2029':
			n += 6
		default:
			if r < 0x20 {
				n += 6
			} else {
				n += int64(utf8.RuneLen(r))
			}
		}
	}
	return n, nil
}
