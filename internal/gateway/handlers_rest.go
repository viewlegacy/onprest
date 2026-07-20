package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const maxGatewayRequestBodyBytes int64 = 1 << 20

func (s *Server) handleCapability(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newID()
	name := capabilityFromPath(r.URL.Path)
	key, ok := s.authenticate(w, r)
	if !ok {
		s.accessLog(reqID, "", name, http.StatusUnauthorized, errGatewayAuthFailed, "invalid api key", start)
		return
	}
	if name == "" || strings.Contains(name, "/") {
		s.accessLog(reqID, key.Name, name, http.StatusNotFound, errGatewayCapabilityNotFound, "capability not found", start)
		writeJSON(w, http.StatusNotFound, apiError(errGatewayCapabilityNotFound, "capability not found"))
		return
	}
	if !allowed(key, name) {
		s.accessLog(reqID, key.Name, name, http.StatusForbidden, errGatewayCapabilityDenied, "capability not allowed", start)
		writeJSON(w, http.StatusForbidden, apiError(errGatewayCapabilityDenied, "capability not allowed"))
		return
	}
	if r.Method != http.MethodPost {
		s.accessLog(reqID, key.Name, name, http.StatusMethodNotAllowed, errGatewayMethodNotAllowed, "use POST", start)
		writeJSON(w, http.StatusMethodNotAllowed, apiError(errGatewayMethodNotAllowed, "use POST"))
		return
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		s.accessLog(reqID, key.Name, name, http.StatusBadRequest, errGatewayInvalidRequest, "Content-Type must be application/json", start)
		writeJSON(w, http.StatusBadRequest, apiError(errGatewayInvalidRequest, "Content-Type must be application/json"))
		return
	}
	var params map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxGatewayRequestBodyBytes))
	dec.UseNumber()
	if err := dec.Decode(&params); err != nil {
		s.accessLog(reqID, key.Name, name, http.StatusBadRequest, errGatewayInvalidRequest, "invalid json body", start)
		writeJSON(w, http.StatusBadRequest, apiError(errGatewayInvalidRequest, "invalid json body"))
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		s.accessLog(reqID, key.Name, name, http.StatusBadRequest, errGatewayInvalidRequest, "invalid json body", start)
		writeJSON(w, http.StatusBadRequest, apiError(errGatewayInvalidRequest, "invalid json body"))
		return
	}
	result := s.callAgent(r.Context(), name, params)
	s.accessLog(reqID, key.Name, name, result.Status, result.Code, result.Message, start)
	if !result.OK() {
		writeJSON(w, result.Status, apiError(result.Code, result.Message))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(result.Payload)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	s.agentMu.RLock()
	doc := cloneMap(s.openapi)
	s.agentMu.RUnlock()
	if doc == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError(errGatewayAgentOffline, "agent metadata is not cached yet"))
		return
	}
	filterOpenAPI(doc, key.Capabilities)
	writeJSON(w, http.StatusOK, doc)
}

func isJSONContentType(header string) bool {
	if header == "" {
		return false
	}
	mediaType := strings.TrimSpace(strings.Split(header, ";")[0])
	return strings.EqualFold(mediaType, "application/json")
}

func capabilityFromPath(path string) string {
	if !strings.HasPrefix(path, "/api/v1/capabilities/") {
		return ""
	}
	return strings.TrimPrefix(path, "/api/v1/capabilities/")
}
