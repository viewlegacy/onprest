package gateway

import (
	"encoding/json"
	"errors"
)

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	var out map[string]any
	b, _ := json.Marshal(in)
	_ = json.Unmarshal(b, &out)
	return out
}

func filterOpenAPI(doc map[string]any, caps []string) {
	if hasAll(caps) {
		return
	}
	paths, _ := doc["paths"].(map[string]any)
	for path, v := range paths {
		methods, _ := v.(map[string]any)
		keepPath := false
		for method, opv := range methods {
			op, _ := opv.(map[string]any)
			cap, _ := op["x-onprest-capability"].(string)
			if cap == "" || !capAllowed(caps, cap) {
				delete(methods, method)
				continue
			}
			keepPath = true
		}
		if !keepPath {
			delete(paths, path)
		}
	}
}

func toolsFromOpenAPI(doc map[string]any) []map[string]any {
	paths, _ := doc["paths"].(map[string]any)
	tools := []map[string]any{}
	for _, v := range paths {
		methods, _ := v.(map[string]any)
		for _, opv := range methods {
			op, _ := opv.(map[string]any)
			name, _ := op["x-onprest-capability"].(string)
			if name == "" {
				continue
			}
			tool := map[string]any{"name": name}
			if desc, _ := op["description"].(string); desc != "" {
				tool["description"] = desc
			} else if summary, _ := op["summary"].(string); summary != "" {
				tool["description"] = summary
			}
			if rb, _ := op["requestBody"].(map[string]any); rb != nil {
				if content, _ := rb["content"].(map[string]any); content != nil {
					if app, _ := content["application/json"].(map[string]any); app != nil {
						if schema, ok := app["schema"]; ok {
							tool["inputSchema"] = mcpInputSchema(schema)
						}
					}
				}
			}
			tools = append(tools, tool)
		}
	}
	return tools
}

func mcpInputSchema(schema any) any {
	root, _ := schema.(map[string]any)
	props, _ := root["properties"].(map[string]any)
	params, ok := props["params"]
	if ok {
		return params
	}
	return schema
}

func openAPIPathCount(doc map[string]any) int {
	paths, _ := doc["paths"].(map[string]any)
	return len(paths)
}

func openAPIFromMeta(raw []byte) (map[string]any, error) {
	var meta struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	if meta.Data == nil {
		return nil, errors.New("missing data")
	}
	return meta.Data, nil
}

func hasAll(caps []string) bool {
	for _, c := range caps {
		if c == "*" {
			return true
		}
	}
	return false
}

func capAllowed(caps []string, cap string) bool {
	for _, c := range caps {
		if c == cap {
			return true
		}
	}
	return false
}
