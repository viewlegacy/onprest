package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CanonicalJSON implements Onprest canonical JSON v1, not RFC 8785.
func CanonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if e := writeCanonical(&b, v); e != nil {
		return nil, e
	}
	return b.Bytes(), nil
}
func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		if !utf8.ValidString(x) {
			return errors.New("invalid UTF-8 string")
		}
		var s bytes.Buffer
		e := json.NewEncoder(&s)
		e.SetEscapeHTML(false)
		if err := e.Encode(x); err != nil {
			return err
		}
		b.Write(bytes.TrimSuffix(s.Bytes(), []byte{'\n'}))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		return writeCanonical(b, int64(x))
	case json.Number:
		if i, e := x.Int64(); e == nil {
			return writeCanonical(b, i)
		}
		if !strings.ContainsAny(string(x), ".eE") {
			return errors.New("integer exceeds int64")
		}
		f, e := x.Float64()
		if e != nil {
			return errors.New("invalid number")
		}
		return writeCanonical(b, f)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errors.New("number must be finite")
		}
		if x == 0 {
			x = 0
		}
		b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if e := writeCanonical(b, k); e != nil {
				return e
			}
			b.WriteByte(':')
			if e := writeCanonical(b, x[k]); e != nil {
				return e
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, v := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if e := writeCanonical(b, v); e != nil {
				return e
			}
		}
		b.WriteByte(']')
	case []string:
		l := make([]any, len(x))
		for i, v := range x {
			l[i] = v
		}
		return writeCanonical(b, l)
	default:
		return fmt.Errorf("unsupported canonical value %T", v)
	}
	return nil
}

// DecodeJSON rejects duplicate keys and invalid Unicode before encoding/json
// can replace them, and bounds bytes, nesting, and total values during decode.
func DecodeJSON(data []byte, limits Limits) (any, error) {
	if limits.MaxBytes <= 0 || limits.MaxDepth <= 0 || limits.MaxItems <= 0 {
		return nil, errors.New("JSON limits must be positive")
	}
	if int64(len(data)) > limits.MaxBytes {
		return nil, errors.New("JSON byte limit exceeded")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("JSON must be valid UTF-8")
	}
	if e := validateJSONUnicode(data); e != nil {
		return nil, e
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	count := int64(0)
	v, e := decodeJSONValue(d, limits, 1, &count)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, errors.New("JSON must contain one value")
	}
	return v, nil
}
func decodeJSONValue(d *json.Decoder, l Limits, depth int64, count *int64) (any, error) {
	*count++
	if depth > l.MaxDepth || *count > l.MaxItems {
		return nil, errors.New("JSON structural limit exceeded")
	}
	t, e := d.Token()
	if e != nil {
		return nil, errors.New("invalid JSON")
	}
	switch x := t.(type) {
	case json.Delim:
		switch x {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, errors.New("invalid JSON object")
				}
				key, ok := k.(string)
				if !ok {
					return nil, errors.New("invalid JSON key")
				}
				if _, ok = m[key]; ok {
					return nil, errors.New("duplicate JSON key")
				}
				*count++
				if *count > l.MaxItems {
					return nil, errors.New("JSON item limit exceeded")
				}
				v, e := decodeJSONValue(d, l, depth+1, count)
				if e != nil {
					return nil, e
				}
				m[key] = v
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return nil, errors.New("invalid JSON object")
			}
			return m, nil
		case '[':
			a := []any{}
			for d.More() {
				v, e := decodeJSONValue(d, l, depth+1, count)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return nil, errors.New("invalid JSON array")
			}
			return a, nil
		}
		return nil, errors.New("invalid JSON delimiter")
	case json.Number:
		if !strings.ContainsAny(string(x), ".eE") {
			i, e := x.Int64()
			if e != nil {
				return nil, errors.New("JSON integer exceeds int64")
			}
			return i, nil
		}
		f, e := x.Float64()
		if e != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, errors.New("JSON number must be finite")
		}
		if f == 0 {
			f = 0
		}
		return x, nil
	default:
		return t, nil
	}
}
func validateJSONUnicode(data []byte) error {
	in := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			in = !in
			continue
		}
		if !in || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return errors.New("invalid JSON escape")
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return errors.New("invalid Unicode escape")
		}
		n, e := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if e != nil {
			return errors.New("invalid Unicode escape")
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return errors.New("unpaired Unicode surrogate")
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return errors.New("unpaired Unicode surrogate")
			}
			low, e := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return errors.New("unpaired Unicode surrogate")
			}
			i += 6
		}
	}
	return nil
}
