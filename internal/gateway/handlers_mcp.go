package gateway

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError(errGatewayMethodNotAllowed, "use POST"))
		return
	}
	key, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		writeMCPError(w, nil, -32700, errJSONRPCParseError, "invalid json rpc")
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" || req.ID == nil {
		writeMCPError(w, req.ID, -32600, errJSONRPCInvalidRequest, "invalid json rpc request")
		return
	}
	switch req.Method {
	case "initialize":
		writeMCP(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"serverInfo":      map[string]any{"name": "onprest-gateway", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}, nil)
	case "ping":
		writeMCP(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		s.agentMu.RLock()
		doc := cloneMap(s.openapi)
		s.agentMu.RUnlock()
		if doc == nil {
			writeJSON(w, http.StatusServiceUnavailable, apiError(errGatewayAgentOffline, "agent metadata is not cached yet"))
			return
		}
		filterOpenAPI(doc, key.Capabilities)
		writeMCP(w, req.ID, map[string]any{"tools": toolsFromOpenAPI(doc)}, nil)
	case "tools/call":
		s.handleMCPToolCall(w, r, req.ID, req.Params, key)
	default:
		writeMCPError(w, req.ID, -32601, errJSONRPCMethodNotFound, "unsupported MCP method")
	}
}

func (s *Server) handleMCPToolCall(w http.ResponseWriter, r *http.Request, id any, rawParams json.RawMessage, key APIKey) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(rawParams, &params); err != nil || params.Name == "" {
		writeMCPError(w, id, -32602, errJSONRPCInvalidParams, "invalid tools/call params")
		return
	}
	if !allowed(key, params.Name) {
		writeJSON(w, http.StatusForbidden, apiError(errGatewayCapabilityDenied, "capability not allowed"))
		return
	}
	result := s.callAgent(r.Context(), params.Name, params.Arguments)
	if !result.OK() {
		if result.Code == errGatewayCapabilityNotFound {
			writeMCPError(w, id, -32602, errJSONRPCInvalidParams, "tool is not defined")
			return
		}
		writeJSON(w, result.Status, apiError(result.Code, result.Message))
		return
	}
	var structured any
	_ = json.Unmarshal(result.Payload, &structured)
	writeMCP(w, id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(result.Payload)}},
		"structuredContent": structured,
	}, nil)
}
