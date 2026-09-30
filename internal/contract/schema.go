package contract

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

const schemaKeys = "type description properties items required additionalProperties enum const minimum maximum exclusiveMinimum exclusiveMaximum minLength maxLength pattern format minItems maxItems"

func CompileSchema(data []byte, input bool) (*Schema, error) {
	m, e := decodeYAML(data)
	if e != nil {
		return nil, e
	}
	return compileSchema(m, input, input, false, 0)
}
func compileSchema(m rawMap, input, root, passthrough bool, depth int) (*Schema, error) {
	if depth > 128 {
		return nil, errors.New("schema nesting exceeds limit")
	}
	keys := schemaKeys
	if input {
		keys += " default"
	} else {
		keys += " nullable"
	}
	if typ, ok := m.get("type").(string); ok && typ == "file" {
		keys = "type description extensions max_bytes mime_types allow_empty"
	}
	if e := known(m, keys); e != nil {
		return nil, e
	}
	s := &Schema{Properties: map[string]*Schema{}, Required: []string{}, Nullable: !input, Constraints: map[string]any{}, normalized: map[string]any{}}
	if m.has("description") {
		v, e := str(m.get("description"), "description")
		if e != nil {
			return nil, e
		}
		s.normalized["description"] = v
	}
	if m.has("type") {
		switch v := m.get("type").(type) {
		case string:
			s.Types = []string{v}
		case []any:
			t, e := stringsList(v, "type", true)
			if e != nil || len(t) == 0 {
				return nil, errors.New("type must be a nonempty type union")
			}
			s.Types = t
		default:
			return nil, errors.New("type must be a string or union")
		}
	}
	for _, t := range s.Types {
		if !contains([]string{"string", "integer", "number", "boolean", "object", "array", "null", "file"}, t) {
			return nil, errors.New("unsupported schema type")
		}
	}
	if contains(s.Types, "file") {
		if !input || len(s.Types) != 1 || root {
			return nil, errors.New("file is an input leaf type only")
		}
		f, e := compileFile(m)
		if e != nil {
			return nil, e
		}
		s.File = f
		s.normalized["type"] = "file"
		s.normalized["extensions"] = f.Extensions
		s.normalized["mime_types"] = f.MIMETypes
		s.normalized["allow_empty"] = f.AllowEmpty
		if f.MaxBytes > 0 {
			s.normalized["max_bytes"] = f.MaxBytes
		}
		return s, nil
	}
	if root {
		if m.has("type") && (len(s.Types) != 1 || s.Types[0] != "object") {
			return nil, errors.New("input root must be object")
		}
		if m.has("items") {
			return nil, errors.New("input root cannot be array")
		}
		s.Types = []string{"object"}
	}
	if m.has("properties") && m.has("items") {
		return nil, errors.New("properties and items are exclusive")
	}
	inferred := ""
	if m.has("properties") {
		inferred = "object"
	}
	if m.has("items") {
		inferred = "array"
	}
	if inferred != "" {
		if len(s.Types) == 0 {
			s.Types = []string{inferred}
		} else {
			if !contains(s.Types, inferred) || len(s.Types) > 2 || len(s.Types) == 2 && !contains(s.Types, "null") {
				return nil, errors.New("structural type contradicts explicit type")
			}
		}
	}
	if !input {
		v, e := boolDefault(m, "nullable", true)
		if e != nil {
			return nil, e
		}
		if !v && contains(s.Types, "null") {
			return nil, errors.New("nullable false contradicts null type")
		}
		s.Nullable = v
		if contains(s.Types, "null") && len(s.Types) > 1 {
			s.Types = without(s.Types, "null")
		}
		s.normalized["nullable"] = v
	}
	sort.Strings(s.Types)
	if len(s.Types) == 1 {
		s.normalized["type"] = s.Types[0]
	} else if len(s.Types) > 1 {
		s.normalized["type"] = append([]string{}, s.Types...)
	}
	structural := func(t string) bool { return contains(s.Types, t) }
	if m.has("properties") {
		p, e := object(m.get("properties"), "properties")
		if e != nil {
			return nil, e
		}
		s.PropertiesSet = true
		s.PropertyOrder = append([]string{}, p.keys...)
		np := map[string]any{}
		for _, k := range p.keys {
			sm, e := object(p.get(k), "property")
			if e != nil {
				return nil, e
			}
			child, e := compileSchema(sm, input, false, passthrough, depth+1)
			if e != nil {
				return nil, fmt.Errorf("property %s: %w", safe(k), e)
			}
			s.Properties[k] = child
			np[k] = child.Normalized()
		}
		s.normalized["properties"] = np
	}
	if m.has("items") {
		im, e := object(m.get("items"), "items")
		if e != nil {
			return nil, e
		}
		s.Items, e = compileSchema(im, input, false, passthrough, depth+1)
		if e != nil {
			return nil, e
		}
		s.normalized["items"] = s.Items.Normalized()
	}
	if m.has("required") || m.has("additionalProperties") {
		if !structural("object") {
			return nil, errors.New("object constraints require object type")
		}
	}
	if structural("object") {
		if m.has("required") {
			r, e := stringsList(m.get("required"), "required", true)
			if e != nil {
				return nil, e
			}
			for _, k := range r {
				if _, ok := s.Properties[k]; !ok {
					return nil, errors.New("required must name a declared property")
				}
			}
			sort.Strings(r)
			s.Required = r
		}
		s.normalized["required"] = s.Required
		s.AdditionalAllowed = !input && (!s.PropertiesSet || passthrough)
		if m.has("additionalProperties") {
			switch v := m.get("additionalProperties").(type) {
			case bool:
				s.AdditionalAllowed = v
			case rawMap:
				child, e := compileSchema(v, input, false, passthrough, depth+1)
				if e != nil {
					return nil, e
				}
				if input && child.HasFiles() {
					return nil, errors.New("dynamic additionalProperties cannot contain file")
				}
				s.Additional = child
				s.AdditionalAllowed = true
			default:
				return nil, errors.New("additionalProperties must be boolean or schema")
			}
		}
		if s.Additional != nil {
			s.normalized["additionalProperties"] = s.Additional.Normalized()
		} else {
			s.normalized["additionalProperties"] = s.AdditionalAllowed
		}
	}
	for _, k := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
		if !m.has(k) {
			continue
		}
		if !structural("number") && !structural("integer") {
			return nil, errors.New("numeric constraints require a numeric type")
		}
		v := m.get(k)
		switch v.(type) {
		case int64, float64:
		default:
			return nil, errors.New("numeric bound must be finite number")
		}
		s.Constraints[k] = v
		s.normalized[k] = v
	}
	for _, k := range []string{"minLength", "maxLength", "minItems", "maxItems"} {
		if !m.has(k) {
			continue
		}
		t := "string"
		if strings.HasSuffix(k, "Items") {
			t = "array"
		}
		if !structural(t) {
			return nil, fmt.Errorf("%s requires %s type", k, t)
		}
		v, e := intDefault(m, k, 0, 0)
		if e != nil {
			return nil, e
		}
		s.Constraints[k] = v
		s.normalized[k] = v
	}
	for _, pair := range [][2]string{{"minLength", "maxLength"}, {"minItems", "maxItems"}} {
		lo, lok := s.Constraints[pair[0]].(int64)
		hi, hok := s.Constraints[pair[1]].(int64)
		if lok && hok && lo > hi {
			return nil, errors.New("minimum exceeds maximum")
		}
	}
	// Numeric bounds are compared without rounding integer endpoints.
	for _, lo := range []string{"minimum", "exclusiveMinimum"} {
		for _, hi := range []string{"maximum", "exclusiveMaximum"} {
			a, aok := s.Constraints[lo]
			b, bok := s.Constraints[hi]
			if aok && bok {
				cmp := compareNumbers(a, b)
				if cmp > 0 || cmp == 0 && (lo == "exclusiveMinimum" || hi == "exclusiveMaximum") {
					return nil, errors.New("numeric bounds have no intersection")
				}
			}
		}
	}
	for _, k := range []string{"pattern", "format"} {
		if !m.has(k) {
			continue
		}
		if !structural("string") {
			return nil, fmt.Errorf("%s requires string type", k)
		}
		v, e := str(m.get(k), k)
		if e != nil {
			return nil, e
		}
		if k == "pattern" {
			p, e := CompilePattern(v)
			if e != nil {
				return nil, e
			}
			s.Pattern = p
			s.normalized[k] = p.AST.normalized()
		} else {
			if !contains([]string{"", "email", "uuid", "date", "date-time", "uri"}, v) {
				return nil, errors.New("unsupported format")
			}
			s.Constraints[k] = v
			s.normalized[k] = v
		}
	}
	if len(s.Types) == 0 && (m.has("enum") || m.has("const")) {
		return nil, errors.New("constraints without structure require an explicit type")
	}
	if m.has("enum") {
		a, e := list(m.get("enum"), "enum")
		if e != nil || len(a) == 0 {
			return nil, errors.New("enum must be a nonempty list")
		}
		a = plain(a).([]any)
		seen := map[string]bool{}
		for index, v := range a {
			normalized, err := s.validate(v, "", false, nil)
			if err != nil {
				e := err
				return nil, fmt.Errorf("enum does not satisfy schema: %w", e)
			}
			a[index] = normalized
			b, e := CanonicalJSON(normalized)
			if e != nil {
				return nil, e
			}
			if seen[string(b)] {
				return nil, errors.New("enum values must be unique")
			}
			seen[string(b)] = true
		}
		sort.Slice(a, func(i, j int) bool {
			a, _ := CanonicalJSON(a[i])
			b, _ := CanonicalJSON(a[j])
			return string(a) < string(b)
		})
		s.Constraints["enum"] = a
		s.normalized["enum"] = a
	}
	if m.has("const") {
		v := plain(m.get("const"))
		normalized, e := s.validate(v, "", false, nil)
		if e != nil {
			return nil, e
		}
		v = normalized
		s.Constraints["const"] = v
		s.normalized["const"] = v
	}
	if input && m.has("default") {
		if s.HasFiles() {
			return nil, errors.New("default cannot synthesize request-owned files")
		}
		v := plain(m.get("default"))
		norm, e := s.validate(v, "", true, nil)
		if e != nil {
			return nil, fmt.Errorf("default does not satisfy schema: %w", e)
		}
		s.Default = norm
		s.DefaultSet = true
		s.normalized["default"] = norm
	}
	return s, nil
}
func contains(a []string, v string) bool {
	for _, s := range a {
		if s == v {
			return true
		}
	}
	return false
}
func without(a []string, v string) []string {
	o := []string{}
	for _, s := range a {
		if s != v {
			o = append(o, s)
		}
	}
	return o
}
func (s *Schema) HasFiles() bool {
	if s.File != nil {
		return true
	}
	if s.Items != nil && s.Items.HasFiles() {
		return true
	}
	if s.Additional != nil && s.Additional.HasFiles() {
		return true
	}
	for _, c := range s.Properties {
		if c.HasFiles() {
			return true
		}
	}
	return false
}
func (s *Schema) validateStatic(v any) error { _, e := s.validate(v, "", false, nil); return e }
func compareNumbers(a, b any) int            { return numberRat(a).Cmp(numberRat(b)) }
func numberRat(v any) *big.Rat {
	if i, ok := v.(int64); ok {
		return new(big.Rat).SetInt64(i)
	}
	return new(big.Rat).SetFloat64(v.(float64))
}
