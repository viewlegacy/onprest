package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError(errGatewayMethodNotAllowed, "use POST"))
		return
	}
	key, authStatus := s.authenticate(w, r)
	if authStatus != 0 {
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBodyBytes))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		writeMCPError(w, nil, -32700, errJSONRPCParseError, "invalid json rpc")
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeMCPError(w, nil, -32700, errJSONRPCParseError, "invalid json rpc")
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeMCPError(w, req.ID, -32600, errJSONRPCInvalidRequest, "invalid json rpc request")
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		writeMCP(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"serverInfo":      map[string]any{"name": "onprest-gateway", "version": "1.0.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}, nil)
	case "ping":
		writeMCP(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		s.agentMu.RLock()
		doc := cloneMap(s.openapi)
		s.agentMu.RUnlock()
		if doc == nil {
			writeMCPError(w, req.ID, -32000, errGatewayAgentOffline, "agent metadata is not cached yet")
			return
		}
		filterOpenAPI(doc, key.Capabilities)
		writeMCP(w, req.ID, map[string]any{"tools": toolsFromOpenAPI(doc)}, nil)
	case "tools/call":
		s.handleMCPToolCall(w, r, req.ID, req.Params, key, newID(), time.Now())
	default:
		writeMCPError(w, req.ID, -32601, errJSONRPCMethodNotFound, "unsupported MCP method")
	}
}

func (s *Server) handleMCPToolCall(w http.ResponseWriter, r *http.Request, id any, rawParams json.RawMessage, key APIKey, requestID string, start time.Time) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := decodeJSONNumber(rawParams, &params); err != nil || params.Name == "" {
		s.accessLogProtocol("mcp", requestID, key.Name, "", http.StatusOK, errJSONRPCInvalidParams, "invalid tools/call params", start)
		writeMCPError(w, id, -32602, errJSONRPCInvalidParams, "invalid tools/call params")
		return
	}
	if !allowed(key, params.Name) {
		s.accessLogProtocol("mcp", requestID, key.Name, params.Name, http.StatusForbidden, errGatewayCapabilityDenied, "capability not allowed", start)
		writeJSON(w, http.StatusForbidden, apiError(errGatewayCapabilityDenied, "capability not allowed"))
		return
	}
	result := s.callAgent(r.Context(), params.Name, params.Arguments)
	if !result.OK() {
		if result.Code == errGatewayCapabilityNotFound {
			s.accessLogProtocol("mcp", requestID, key.Name, params.Name, http.StatusOK, errJSONRPCInvalidParams, "tool is not defined", start)
			writeMCPError(w, id, -32602, errJSONRPCInvalidParams, "tool is not defined")
			return
		}
		s.accessLogProtocol("mcp", requestID, key.Name, params.Name, http.StatusOK, result.Code, result.Message, start)
		writeMCP(w, id, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": result.Message}},
			"structuredContent": map[string]any{"error": map[string]any{
				"code": result.Code, "message": result.Message,
			}},
			"isError": true,
		}, nil)
		return
	}
	var structured any
	_ = decodeJSONNumber(result.Payload, &structured)
	s.accessLogProtocol("mcp", requestID, key.Name, params.Name, http.StatusOK, "", "", start)
	writeMCP(w, id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(result.Payload)}},
		"structuredContent": structured,
	}, nil)
}

func decodeJSONNumber(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return ensureJSONEOF(dec)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return io.ErrUnexpectedEOF
	}
	return err
}
