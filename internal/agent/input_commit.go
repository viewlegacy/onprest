package agent

import (
	"errors"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
	"strconv"
	"strings"
)

func replaceInputFiles(params map[string]any, bindings []protocol.FileBinding, values map[string]contract.FileValue) (map[string]any, error) {
	seen := map[string]bool{}
	for _, b := range bindings {
		v, ok := values[b.FileID]
		if !ok || v.Pointer != b.Pointer || seen[b.FileID] {
			return nil, errors.New("invalid input binding")
		}
		seen[b.FileID] = true
		tokens := strings.Split(strings.TrimPrefix(b.Pointer, "/"), "/")
		if !strings.HasPrefix(b.Pointer, "/") {
			return nil, errors.New("invalid pointer")
		}
		var current any = params
		for i, raw := range tokens {
			token := ""
			for p := 0; p < len(raw); p++ {
				if raw[p] == '~' {
					p++
					if p >= len(raw) || raw[p] != '0' && raw[p] != '1' {
						return nil, errors.New("invalid pointer")
					}
					if raw[p] == '0' {
						token += "~"
					} else {
						token += "/"
					}
				} else {
					token += string(raw[p])
				}
			}
			last := i == len(tokens)-1
			switch node := current.(type) {
			case map[string]any:
				child, ok := node[token]
				if !ok {
					return nil, errors.New("missing placeholder")
				}
				if last {
					if child != nil {
						return nil, errors.New("invalid placeholder")
					}
					node[token] = v
				} else {
					current = child
				}
			case []any:
				index, e := strconv.ParseUint(token, 10, 63)
				if e != nil || strconv.FormatUint(index, 10) != token || index >= uint64(len(node)) {
					return nil, errors.New("invalid index")
				}
				if last {
					if node[index] != nil {
						return nil, errors.New("invalid placeholder")
					}
					node[index] = v
				} else {
					current = node[index]
				}
			default:
				return nil, errors.New("invalid placeholder path")
			}
		}
	}
	if len(seen) != len(values) {
		return nil, errors.New("unused input")
	}
	return params, nil
}
func schemaHasFile(s *contract.Schema) bool {
	if s == nil {
		return false
	}
	if s.File != nil {
		return true
	}
	if schemaHasFile(s.Items) {
		return true
	}
	for _, child := range s.Properties {
		if schemaHasFile(child) {
			return true
		}
	}
	return false
}
