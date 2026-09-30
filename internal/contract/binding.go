package contract

import (
	"errors"
	"strconv"
	"strings"
)

// ResolvePointer checks declared positions and computes existence guarantees.
// Array indexes are concrete, canonical decimal indexes, never wildcards.
func (s *Schema) ResolvePointer(pointer string) (*Schema, bool, error) {
	if pointer == "" {
		return s, true, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false, errors.New("binding must be an RFC6901 JSON Pointer")
	}
	cur := s
	guaranteed := true
	for _, raw := range strings.Split(pointer[1:], "/") {
		token, e := unescapePointer(raw)
		if e != nil {
			return nil, false, e
		}
		if contains(cur.Types, "null") {
			guaranteed = false
		}
		if contains(cur.Types, "object") {
			child, ok := cur.Properties[token]
			if !ok {
				return nil, false, errors.New("binding must reference a declared property")
			}
			guaranteed = guaranteed && (contains(cur.Required, token) || child.DefaultSet)
			cur = child
		} else if contains(cur.Types, "array") {
			if token == "" || len(token) > 1 && token[0] == '0' {
				return nil, false, errors.New("invalid binding array index")
			}
			for _, r := range token {
				if r < '0' || r > '9' {
					return nil, false, errors.New("invalid binding array index")
				}
			}
			i, e := strconv.ParseInt(token, 10, 64)
			if e != nil || cur.Items == nil {
				return nil, false, errors.New("binding index has no declared item schema")
			}
			min, _ := cur.Constraints["minItems"].(int64)
			guaranteed = guaranteed && min > i
			cur = cur.Items
		} else {
			return nil, false, errors.New("binding traverses a scalar or unconstrained schema")
		}
	}
	return cur, guaranteed, nil
}
func unescapePointer(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '~' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) || s[i] != '0' && s[i] != '1' {
			return "", errors.New("invalid JSON Pointer escape")
		}
		if s[i] == '0' {
			b.WriteByte('~')
		} else {
			b.WriteByte('/')
		}
	}
	return b.String(), nil
}
func requirePointer(input *Schema, v any, kind string, required, nonnull bool) (string, *Schema, error) {
	p, e := str(v, "input binding")
	if e != nil {
		return "", nil, e
	}
	s, g, e := input.ResolvePointer(p)
	if e != nil {
		return "", nil, e
	}
	if required && !g {
		return "", nil, errors.New("binding position must be guaranteed present")
	}
	if nonnull && contains(s.Types, "null") {
		return "", nil, errors.New("binding must not allow null")
	}
	if kind == "file" {
		if s.File == nil {
			return "", nil, errors.New("binding must reference one file")
		}
	} else if kind == "files" {
		if s.File == nil && !(contains(s.Types, "array") && s.Items != nil && s.Items.File != nil) {
			return "", nil, errors.New("binding must reference file or file array")
		}
		if required && s.File == nil {
			n, _ := s.Constraints["minItems"].(int64)
			if n < 1 {
				return "", nil, errors.New("required file array must have minItems>=1")
			}
		}
	} else if kind != "" && !contains(s.Types, kind) {
		return "", nil, errors.New("binding target type does not match")
	}
	return p, s, nil
}
func binding(input *Schema, m rawMap, mode string) (map[string]any, error) {
	if e := known(m, "input value"); e != nil {
		return nil, e
	}
	if m.has("input") == m.has("value") {
		return nil, errors.New("binding requires exactly one input or value")
	}
	if m.has("input") {
		p, s, e := requirePointer(input, m.get("input"), "", false, false)
		if e != nil {
			return nil, e
		}
		if e := bindingType(s, mode); e != nil {
			return nil, e
		}
		return map[string]any{"input": p}, nil
	}
	v := plain(m.get("value"))
	if e := bindingValue(v, mode); e != nil {
		return nil, e
	}
	if _, e := CanonicalJSON(v); e != nil {
		return nil, e
	}
	return map[string]any{"value": v}, nil
}
func bindingType(s *Schema, mode string) error {
	if s.HasFiles() {
		return errors.New("JSON/text binding cannot contain file")
	}
	if mode == "json" {
		return nil
	}
	types := without(s.Types, "null")
	if len(types) == 0 {
		return errors.New("text binding requires a scalar schema")
	}
	for _, t := range types {
		if t == "array" && mode == "many" {
			if s.Items == nil {
				return errors.New("array text binding requires typed items")
			}
			if e := bindingType(s.Items, "scalar"); e != nil {
				return e
			}
			if contains(s.Items.Types, "null") {
				return errors.New("text array items must be nonnull")
			}
			continue
		}
		if !contains([]string{"string", "integer", "number", "boolean"}, t) {
			return errors.New("text binding requires scalar type")
		}
	}
	return nil
}
func bindingValue(v any, mode string) error {
	if mode == "json" {
		return nil
	}
	if a, ok := v.([]any); ok && mode == "many" {
		for _, v := range a {
			if e := bindingValue(v, "scalar"); e != nil {
				return e
			}
		}
		return nil
	}
	switch v.(type) {
	case string, bool, int64, float64:
		return nil
	}
	return errors.New("fixed text binding must be nonnull scalar")
}

// Output pointers may enter retained, unconstrained JSON. Only statically known
// exclusions or scalar traversal are rejected here; value existence is P02.
func (s *Schema) resolveOutputPointer(pointer string) (*Schema, error) {
	if pointer == "" {
		return s, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("output binding must be RFC6901 Pointer")
	}
	cur := s
	for _, raw := range strings.Split(pointer[1:], "/") {
		key, e := unescapePointer(raw)
		if e != nil {
			return nil, e
		}
		if len(cur.Types) == 0 {
			cur = &Schema{Nullable: true}
			continue
		}
		if contains(cur.Types, "object") {
			child, ok := cur.Properties[key]
			if !ok {
				if !cur.AdditionalAllowed {
					return nil, errors.New("CSV from references excluded output property")
				}
				child = cur.Additional
				if child == nil {
					child = &Schema{Nullable: true}
				}
			}
			cur = child
			continue
		}
		if contains(cur.Types, "array") {
			if key == "" || len(key) > 1 && key[0] == '0' {
				return nil, errors.New("invalid output array index")
			}
			for _, r := range key {
				if r < '0' || r > '9' {
					return nil, errors.New("invalid output array index")
				}
			}
			if _, e := strconv.ParseInt(key, 10, 64); e != nil {
				return nil, errors.New("invalid output array index")
			}
			cur = cur.Items
			if cur == nil {
				cur = &Schema{Nullable: true}
			}
			continue
		}
		return nil, errors.New("CSV from traverses known scalar")
	}
	return cur, nil
}
