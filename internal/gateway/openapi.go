package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
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

func (s *Server) finalizeOpenAPI(doc map[string]any) map[string]any {
	applyOpenAPIGatewayMetadata(doc, s.gatewayPublicURL())
	return doc
}

func (s *Server) gatewayPublicURL() string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	return publicURLFromAddr(s.cfg.Addr)
}

func applyOpenAPIGatewayMetadata(doc map[string]any, serverURL string) {
	if doc == nil {
		return
	}
	doc["servers"] = []any{map[string]any{"url": serverURL}}
	components, _ := doc["components"].(map[string]any)
	if components == nil {
		components = map[string]any{}
		doc["components"] = components
	}
	components["securitySchemes"] = map[string]any{
		"bearerAuth": map[string]any{
			"type":   "http",
			"scheme": "bearer",
		},
		"apiKeyAuth": map[string]any{
			"type": "apiKey",
			"in":   "header",
			"name": "X-API-Key",
		},
	}
	doc["security"] = []any{
		map[string]any{"bearerAuth": []any{}},
		map[string]any{"apiKeyAuth": []any{}},
	}
}

func normalizePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("GATEWAY_PUBLIC_URL must be an absolute http or https URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("GATEWAY_PUBLIC_URL must use http or https")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("GATEWAY_PUBLIC_URL must not include query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func normalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("GATEWAY_CORS_ALLOWED_ORIGINS must contain absolute http or https origins")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("GATEWAY_CORS_ALLOWED_ORIGINS must use http or https")
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("GATEWAY_CORS_ALLOWED_ORIGINS values must not include path, query, or fragment")
	}
	return u.String(), nil
}

func publicURLFromAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if strings.HasPrefix(addr, ":") {
			host = ""
			port = strings.TrimPrefix(addr, ":")
		} else {
			host = addr
		}
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "localhost"
	}
	if port == "" {
		return fmt.Sprintf("http://%s", host)
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(host, port))
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
	doc, _, err := metaData(raw)
	return doc, err
}

func metaData(raw []byte) (map[string]any, map[string]responseKind, error) {
	var meta struct {
		Data          map[string]any    `json:"data"`
		ResponseKinds map[string]string `json:"response_kinds"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, nil, err
	}
	if meta.Data == nil {
		return nil, nil, errors.New("missing data")
	}
	kinds := make(map[string]responseKind, len(meta.ResponseKinds))
	for name, kind := range meta.ResponseKinds {
		switch responseKind(kind) {
		case responseKindSelect, responseKindMutation:
			kinds[name] = responseKind(kind)
		}
	}
	return meta.Data, kinds, nil
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
