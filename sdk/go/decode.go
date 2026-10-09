package onprest

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
)

// InputFile is a request-owned read-only file. Its opaque identity cannot be
// serialized into result Data; return a reader in Result.Files instead.
type InputFile struct {
	e                      *execution
	id, name, mime, digest string
	size                   int64
}

func (f InputFile) Name() string     { return f.name }
func (f InputFile) MIMEType() string { return f.mime }
func (f InputFile) Size() int64      { return f.size }
func (f InputFile) SHA256() string   { return f.digest }
func (f InputFile) MarshalJSON() ([]byte, error) {
	return nil, errors.New("InputFile cannot be serialized as result data")
}
func (f InputFile) Open(ctx context.Context) (io.ReadCloser, error) {
	if f.e == nil || f.id == "" {
		return nil, errors.New("invalid InputFile")
	}
	if err := f.e.check(ctx); err != nil {
		return nil, err
	}
	v, err := f.e.businessCall(ctx, "input.open", map[string]any{"file_id": f.id})
	if err != nil {
		return nil, err
	}
	h := integer(v["body_handle"])
	if h <= 0 {
		return nil, errors.New("invalid input body handle")
	}
	return f.e.own(ctx, h, "open", "", false), nil
}

var fileType = reflect.TypeFor[InputFile]()
var rawType = reflect.TypeFor[json.RawMessage]()
var numberType = reflect.TypeFor[json.Number]()
var jsonDecoderType = reflect.TypeFor[json.Unmarshaler]()
var textDecoderType = reflect.TypeFor[encoding.TextUnmarshaler]()

func nilReader(v io.Reader) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return r.IsNil()
	}
	return false
}
func decode[P any](raw []byte, e *execution) (P, error) {
	var out P
	t := reflect.TypeFor[P]()
	if t.Kind() != reflect.Struct && (t.Kind() != reflect.Map || t.Key().Kind() != reflect.String) {
		return out, fmt.Errorf("input root: expected struct or string-keyed map, got %v", t)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return out, errors.New("invalid normalized input JSON")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return out, errors.New("normalized input must contain one JSON object")
	}
	if _, ok := v.(map[string]any); !ok {
		return out, errors.New("normalized input root must be object")
	}
	err := decodeValue(reflect.ValueOf(&out).Elem(), v, "", e)
	return out, err
}
func mismatch(path string, t reflect.Type, reason string) error {
	return fmt.Errorf("input %s: expected %v: %s", path, t, reason)
}
func descriptor(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, exists := m["__onprest_input_file_v1"]
	return exists
}
func containsFile(v any) bool {
	if descriptor(v) {
		return true
	}
	switch x := v.(type) {
	case map[string]any:
		for _, v := range x {
			if containsFile(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if containsFile(v) {
				return true
			}
		}
	}
	return false
}
func numericRange(t reflect.Type, v any) error {
	n, ok := v.(json.Number)
	if !ok {
		return errors.New("number required")
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		r, ok := new(big.Rat).SetString(n.String())
		if !ok || !r.IsInt() {
			return errors.New("integer value required")
		}
		i := r.Num()
		bits := t.Bits()
		upper := new(big.Int).Lsh(big.NewInt(1), uint(bits))
		lower := big.NewInt(0)
		if t.Kind() >= reflect.Int && t.Kind() <= reflect.Int64 {
			upper.Rsh(upper, 1)
			lower.Neg(new(big.Int).Set(upper))
		}
		upper.Sub(upper, big.NewInt(1))
		if i.Cmp(lower) < 0 || i.Cmp(upper) > 0 {
			return errors.New("integer outside destination range")
		}
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(n.String(), t.Bits())
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return errors.New("number outside finite destination range")
		}
	}
	return nil
}
func decodeValue(dst reflect.Value, v any, path string, e *execution) error {
	t := dst.Type()
	if t == rawType {
		if containsFile(v) {
			return mismatch(path, t, "file descriptor cannot enter RawMessage")
		}
		b, err := json.Marshal(v)
		if err == nil {
			dst.SetBytes(b)
		}
		return err
	}
	if v == nil {
		switch dst.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
			dst.SetZero()
			return nil
		}
		return mismatch(path, t, "null cannot enter a non-nil Go type")
	}
	if dst.Kind() == reflect.Pointer {
		dst.Set(reflect.New(t.Elem()))
		return decodeValue(dst.Elem(), v, path, e)
	}
	if t == fileType {
		m, ok := v.(map[string]any)
		if !ok || len(m) != 1 {
			return mismatch(path, t, "internal file required")
		}
		meta, ok := m["__onprest_input_file_v1"].(map[string]any)
		if !ok || len(meta) != 5 {
			return mismatch(path, t, "invalid internal file")
		}
		id, _ := meta["file_id"].(string)
		name, _ := meta["name"].(string)
		mime, _ := meta["mime_type"].(string)
		digest, _ := meta["sha256"].(string)
		size, err := strconv.ParseInt(fmt.Sprint(meta["size_bytes"]), 10, 64)
		if id == "" || !validName(name) || !validMIME(mime) || len(digest) != 64 || err != nil || size < 0 {
			return mismatch(path, t, "invalid internal file metadata")
		}
		for _, c := range digest {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return mismatch(path, t, "invalid digest")
			}
		}
		dst.Set(reflect.ValueOf(InputFile{e: e, id: id, name: name, mime: mime, digest: digest, size: size}))
		return nil
	}
	if descriptor(v) {
		return mismatch(path, t, "file must be received as InputFile")
	}
	if t == numberType {
		n, ok := v.(json.Number)
		if !ok {
			return mismatch(path, t, "number required")
		}
		dst.SetString(n.String())
		return nil
	}
	if dst.CanAddr() && (dst.Addr().Type().Implements(jsonDecoderType) || dst.Addr().Type().Implements(textDecoderType)) {
		if containsFile(v) {
			return mismatch(path, t, "file cannot enter a custom decoder")
		}
		switch dst.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
			if err := numericRange(t, v); err != nil {
				return mismatch(path, t, err.Error())
			}
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(b, dst.Addr().Interface()); err != nil {
			return mismatch(path, t, "custom decoder rejected value")
		}
		return nil
	}
	switch dst.Kind() {
	case reflect.Interface:
		if t.NumMethod() != 0 || containsFile(v) {
			return mismatch(path, t, "unsupported interface or file descriptor")
		}
		dst.Set(reflect.ValueOf(v))
		return nil
	case reflect.String:
		x, ok := v.(string)
		if !ok {
			return mismatch(path, t, "string required")
		}
		dst.SetString(x)
	case reflect.Bool:
		x, ok := v.(bool)
		if !ok {
			return mismatch(path, t, "boolean required")
		}
		dst.SetBool(x)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		if err := numericRange(t, v); err != nil {
			return mismatch(path, t, err.Error())
		}
		n := v.(json.Number)
		if dst.Kind() == reflect.Float32 || dst.Kind() == reflect.Float64 {
			f, _ := strconv.ParseFloat(n.String(), t.Bits())
			dst.SetFloat(f)
		} else {
			r, _ := new(big.Rat).SetString(n.String())
			if dst.Kind() >= reflect.Int && dst.Kind() <= reflect.Int64 {
				dst.SetInt(r.Num().Int64())
			} else {
				dst.SetUint(r.Num().Uint64())
			}
		}
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return mismatch(path, t, "object required")
		}
		fields, err := structFields(t)
		if err != nil {
			return mismatch(path, t, err.Error())
		}
		for k, v := range obj {
			f, ok := fields[k]
			if !ok {
				return mismatch(path+"/"+escape(k), t, "no corresponding exported field")
			}
			if f.stringOption {
				return mismatch(path+"/"+escape(k), t, "json string conversion is unsupported")
			}
			field := dst
			for _, i := range f.index {
				if field.Kind() == reflect.Pointer {
					if field.IsNil() {
						if !field.CanSet() {
							return mismatch(path, t, "embedded pointer cannot be set")
						}
						field.Set(reflect.New(field.Type().Elem()))
					}
					field = field.Elem()
				}
				field = field.Field(i)
			}
			if !field.CanSet() {
				return mismatch(path, t, "field cannot be set")
			}
			if err := decodeValue(field, v, path+"/"+escape(k), e); err != nil {
				return err
			}
		}
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok || t.Key().Kind() != reflect.String {
			return mismatch(path, t, "string-keyed object required")
		}
		dst.Set(reflect.MakeMapWithSize(t, len(obj)))
		for k, v := range obj {
			x := reflect.New(t.Elem()).Elem()
			if err := decodeValue(x, v, path+"/"+escape(k), e); err != nil {
				return err
			}
			key := reflect.New(t.Key()).Elem()
			key.SetString(k)
			dst.SetMapIndex(key, x)
		}
	case reflect.Slice:
		a, ok := v.([]any)
		if !ok {
			return mismatch(path, t, "array required")
		}
		dst.Set(reflect.MakeSlice(t, len(a), len(a)))
		for i, v := range a {
			if err := decodeValue(dst.Index(i), v, path+"/"+strconv.Itoa(i), e); err != nil {
				return err
			}
		}
	default:
		return mismatch(path, t, "unsupported Go field type")
	}
	return nil
}
func escape(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }

type goField struct {
	index                []int
	depth                int
	tagged, stringOption bool
}

func structFields(t reflect.Type) (map[string]goField, error) {
	all := map[string][]goField{}
	var walk func(reflect.Type, []int, map[reflect.Type]bool)
	walk = func(t reflect.Type, prefix []int, seen map[reflect.Type]bool) {
		if seen[t] {
			return
		}
		copySeen := map[reflect.Type]bool{}
		for k, v := range seen {
			copySeen[k] = v
		}
		copySeen[t] = true
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			parts := strings.Split(f.Tag.Get("json"), ",")
			name := parts[0]
			if name == "-" {
				continue
			}
			base := f.Type
			if base.Kind() == reflect.Pointer {
				base = base.Elem()
			}
			if !f.IsExported() && (!f.Anonymous || base.Kind() != reflect.Struct) {
				continue
			}
			index := append(append([]int(nil), prefix...), i)
			if f.Anonymous && name == "" && base.Kind() == reflect.Struct {
				walk(base, index, copySeen)
				continue
			}
			if !f.IsExported() {
				continue
			}
			tagged := name != ""
			if name == "" {
				name = f.Name
			}
			stringOpt := false
			for _, part := range parts[1:] {
				if part == "string" {
					stringOpt = true
				}
			}
			all[name] = append(all[name], goField{index, len(prefix), tagged, stringOpt})
		}
	}
	walk(t, nil, map[reflect.Type]bool{})
	out := map[string]goField{}
	for name, candidates := range all {
		best := candidates[0].depth
		for _, f := range candidates {
			if f.depth < best {
				best = f.depth
			}
		}
		var selected []goField
		tagged := false
		for _, f := range candidates {
			if f.depth == best && f.tagged {
				tagged = true
			}
		}
		for _, f := range candidates {
			if f.depth == best && (!tagged || f.tagged) {
				selected = append(selected, f)
			}
		}
		if len(selected) != 1 {
			return nil, fmt.Errorf("ambiguous JSON field %q", name)
		}
		out[name] = selected[0]
	}
	return out, nil
}
