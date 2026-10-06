package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/viewlegacy/onprest/internal/contract"
	"io"
	"net/http"
	"strings"
	"time"
)

func (s *Server) handleCapability(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := authRequestID(r)
	r = r.WithContext(context.WithValue(r.Context(), callIdentityKey{}, callIdentity{ID: reqID}))
	name := capabilityFromPath(r.URL.Path)
	key, authStatus := s.authenticate(w, r)
	if authStatus != 0 {
		code, message := errGatewayAuthFailed, "invalid api key"
		if authStatus == http.StatusTooManyRequests {
			code, message = errGatewayRateLimited, "authentication is busy; retry later"
		}
		s.accessLog(reqID, "", name, authStatus, code, message, start)
		return
	}
	if name == "" || strings.Contains(name, "/") {
		s.accessLog(reqID, key.Name, name, http.StatusNotFound, errGatewayCapabilityNotFound, "capability not found", start)
		writeJSON(w, http.StatusNotFound, commonAPIError(reqID, errGatewayCapabilityNotFound, "capability not found", "not_started"))
		return
	}
	if !allowed(key, name) {
		s.accessLog(reqID, key.Name, name, http.StatusForbidden, errGatewayCapabilityDenied, "capability not allowed", start)
		writeJSON(w, http.StatusForbidden, commonAPIError(reqID, errGatewayCapabilityDenied, "capability not allowed", "not_started"))
		return
	}
	if r.Method != http.MethodPost {
		s.accessLog(reqID, key.Name, name, http.StatusMethodNotAllowed, errGatewayMethodNotAllowed, "use POST", start)
		writeJSON(w, http.StatusMethodNotAllowed, commonAPIError(reqID, errGatewayMethodNotAllowed, "use POST", "not_started"))
		return
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		s.accessLog(reqID, key.Name, name, http.StatusUnsupportedMediaType, errGatewayInvalidRequest, "Content-Type must be application/json", start)
		writeJSON(w, http.StatusUnsupportedMediaType, commonAPIError(reqID, errGatewayInvalidRequest, "Content-Type must be application/json", "not_started"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBodyBytes))
	if err != nil {
		status, code := 400, "INVALID_REQUEST"
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status, code = 413, "LIMIT_EXCEEDED"
		}
		s.accessLog(reqID, key.Name, name, status, code, "invalid request body", start)
		writeJSON(w, status, commonAPIError(reqID, code, "invalid request body", "not_started"))
		return
	}
	value, err := contract.DecodeJSON(body, contract.Limits{MaxBytes: s.cfg.MaxRequestBodyBytes, MaxDepth: 32, MaxItems: 100000})
	params, ok := value.(map[string]any)
	if err != nil || !ok || params == nil {
		s.accessLog(reqID, key.Name, name, 400, "INVALID_REQUEST", "invalid json body", start)
		writeJSON(w, 400, commonAPIError(reqID, "INVALID_REQUEST", "invalid json body", "not_started"))
		return
	}
	fingerprint := sha256.Sum256([]byte(key.KeyHash))
	ctx := context.WithValue(r.Context(), callIdentityKey{}, callIdentity{ID: reqID, Binding: hex.EncodeToString(fingerprint[:])})
	result := s.callAgent(ctx, name, params)
	if !result.OK() {
		s.accessExecutionLog("rest", reqID, key.Name, name, result, start)
		writeJSON(w, result.Status, resultError(reqID, result))
		return
	}
	s.agentMu.RLock()
	meta := s.capabilities[name]
	s.agentMu.RUnlock()
	if meta.Output != nil && meta.Output.REST == "direct" {
		envelope, e := decodeSuccess(result)
		if e != nil || len(envelope.Result.Files) != 1 || envelope.Result.Data != nil {
			result = invalidAgentResult(reqID)
			s.accessExecutionLog("rest", reqID, key.Name, name, result, start)
			writeJSON(w, result.Status, resultError(reqID, result))
			return
		}
		v, e := s.fileLookup(key, envelope.Result.Files[0].ID, false)
		if e != nil {
			result = fileTransferError(e)
			s.accessExecutionLog("rest", reqID, key.Name, name, result, start)
			writeJSON(w, result.Status, resultError(reqID, result))
			return
		}
		logged := deliveryFailure("OUTPUT_INVALID", "file transfer could not be completed", "unknown")
		defer func() { s.accessExecutionLog("rest", reqID, key.Name, name, logged, start) }()
		if failure := s.streamFile(w, r, v); failure != nil {
			logged = *failure
		} else {
			logged = result
		}
		return
	}
	s.accessExecutionLog("rest", reqID, key.Name, name, result, start)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(result.Payload)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	key, authStatus := s.authenticate(w, r)
	if authStatus != 0 {
		return
	}
	s.agentMu.RLock()
	doc := cloneMap(s.openapi)
	s.agentMu.RUnlock()
	if doc == nil {
		writeJSON(w, http.StatusServiceUnavailable, requestAPIError(r, errGatewayAgentOffline, "agent metadata is not cached yet"))
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
