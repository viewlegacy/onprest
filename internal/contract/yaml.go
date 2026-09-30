package contract

import (
	"bytes"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type rawMap struct {
	keys   []string
	values map[string]any
}

func emptyMap() rawMap             { return rawMap{values: map[string]any{}} }
func (m rawMap) has(k string) bool { _, ok := m.values[k]; return ok }
func (m rawMap) get(k string) any  { return m.values[k] }
func (m rawMap) object(k string) (rawMap, error) {
	if !m.has(k) {
		return emptyMap(), nil
	}
	return object(m.get(k), k)
}
func object(v any, label string) (rawMap, error) {
	m, ok := v.(rawMap)
	if !ok {
		return rawMap{}, fmt.Errorf("%s must be a map", safe(label))
	}
	return m, nil
}
func safe(s string) string {
	if len(s) > 128 {
		return "<field>"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-[]", r)) {
			return "<field>"
		}
	}
	return s
}
func known(m rawMap, keys string) error {
	allowed := strings.Fields(keys)
	for _, k := range m.keys {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown field %s", safe(k))
		}
	}
	return nil
}
func str(v any, label string) (string, error) {
	s, ok := v.(string)
	if !ok || !utf8.ValidString(s) {
		return "", fmt.Errorf("%s must be a UTF-8 string", safe(label))
	}
	return s, nil
}
func requiredString(m rawMap, k string) (string, error) {
	s, e := str(m.get(k), k)
	if e != nil {
		return "", e
	}
	if s == "" {
		return "", fmt.Errorf("%s must not be empty", safe(k))
	}
	return s, nil
}
func stringDefault(m rawMap, k, d string) (string, error) {
	if !m.has(k) {
		return d, nil
	}
	return str(m.get(k), k)
}
func boolDefault(m rawMap, k string, d bool) (bool, error) {
	if !m.has(k) {
		return d, nil
	}
	b, ok := m.get(k).(bool)
	if !ok {
		return false, fmt.Errorf("%s must be boolean", safe(k))
	}
	return b, nil
}
func integerValue(v any) (int64, bool) { n, ok := v.(int64); return n, ok }
func intDefault(m rawMap, k string, d, min int64) (int64, error) {
	if !m.has(k) {
		return d, nil
	}
	n, ok := integerValue(m.get(k))
	if !ok || n < min {
		return 0, fmt.Errorf("%s must be an integer >= %d", safe(k), min)
	}
	return n, nil
}
func list(v any, label string) ([]any, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list", safe(label))
	}
	return l, nil
}
func stringsList(v any, label string, nonempty bool) ([]string, error) {
	l, e := list(v, label)
	if e != nil {
		return nil, e
	}
	out := []string{}
	seen := map[string]bool{}
	for _, v := range l {
		s, e := str(v, label)
		if e != nil || nonempty && s == "" {
			return nil, fmt.Errorf("%s contains an invalid string", safe(label))
		}
		if seen[s] {
			return nil, fmt.Errorf("%s contains duplicates", safe(label))
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}
func plain(v any) any {
	switch x := v.(type) {
	case rawMap:
		m := map[string]any{}
		for k, v := range x.values {
			m[k] = plain(v)
		}
		return m
	case []any:
		o := make([]any, len(x))
		for i, v := range x {
			o[i] = plain(v)
		}
		return o
	default:
		return v
	}
}
func decodeYAML(data []byte) (rawMap, error) {
	if !utf8.Valid(data) {
		return rawMap{}, errors.New("configuration must be valid UTF-8")
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if e := d.Decode(&node); e != nil {
		return rawMap{}, errors.New("invalid YAML configuration")
	}
	var extra yaml.Node
	if e := d.Decode(&extra); e != io.EOF {
		return rawMap{}, errors.New("configuration must contain one YAML document")
	}
	if len(node.Content) != 1 {
		return rawMap{}, errors.New("configuration must be a map")
	}
	v, e := decodeNode(node.Content[0], map[*yaml.Node]bool{}, 0)
	if e != nil {
		return rawMap{}, e
	}
	return object(v, "configuration")
}
func decodeNode(n *yaml.Node, stack map[*yaml.Node]bool, depth int) (any, error) {
	if n == nil || depth > 128 || stack[n] {
		return nil, errors.New("configuration nesting or alias cycle exceeds limit")
	}
	stack[n] = true
	defer delete(stack, n)
	if n.Kind == yaml.AliasNode {
		return decodeNode(n.Alias, stack, depth+1)
	}
	if n.Tag == "!!null" {
		return nil, nil
	}
	if n.Kind == yaml.MappingNode && n.Tag != "!!map" || n.Kind == yaml.SequenceNode && n.Tag != "!!seq" {
		return nil, errors.New("unsupported YAML collection tag")
	}
	switch n.Kind {
	case yaml.MappingNode:
		m := emptyMap()
		direct := map[string]bool{}
		// Merge sources are validated and expanded first, with sequence first-wins.
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Tag != "!!merge" {
				continue
			}
			if direct["<<"] {
				return nil, errors.New("duplicate YAML merge")
			}
			direct["<<"] = true
			var sources []*yaml.Node
			if v.Kind == yaml.SequenceNode {
				sources = v.Content
			} else {
				sources = []*yaml.Node{v}
			}
			for _, source := range sources {
				vv, e := decodeNode(source, stack, depth+1)
				if e != nil {
					return nil, e
				}
				sm, e := object(vv, "merge")
				if e != nil {
					return nil, e
				}
				for _, k := range sm.keys {
					if !m.has(k) {
						m.keys = append(m.keys, k)
						m.values[k] = sm.get(k)
					}
				}
			}
		}
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Tag == "!!merge" {
				continue
			}
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
				return nil, errors.New("configuration key must be a string")
			}
			if direct[k.Value] {
				return nil, fmt.Errorf("duplicate configuration key %s", safe(k.Value))
			}
			direct[k.Value] = true
			vv, e := decodeNode(v, stack, depth+1)
			if e != nil {
				return nil, e
			}
			if !m.has(k.Value) {
				m.keys = append(m.keys, k.Value)
			}
			m.values[k.Value] = vv
		}
		return m, nil
	case yaml.SequenceNode:
		l := []any{}
		for _, c := range n.Content {
			v, e := decodeNode(c, stack, depth+1)
			if e != nil {
				return nil, e
			}
			l = append(l, v)
		}
		return l, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str":
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool":
			var b bool
			if e := n.Decode(&b); e != nil {
				return nil, errors.New("invalid boolean")
			}
			return b, nil
		case "!!int":
			var i int64
			if e := n.Decode(&i); e != nil {
				return nil, errors.New("integer exceeds int64")
			}
			return i, nil
		case "!!float":
			f, e := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64)
			if e != nil || math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, errors.New("number must be finite")
			}
			if f == 0 {
				f = 0
			}
			return f, nil
		default:
			return nil, errors.New("unsupported YAML scalar tag")
		}
	default:
		return nil, errors.New("invalid YAML node")
	}
}
