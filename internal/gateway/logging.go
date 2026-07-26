package gateway

import (
	"encoding/json"
	"time"
)

func (s *Server) accessLog(requestID, keyName, capability string, status int, code string, message string, start time.Time) {
	s.log("request", map[string]any{
		"request_id": requestID, "capability": capability, "api_key_name": keyName,
		"http_status": status, "error_code": emptyToNil(code), "error_message": emptyToNil(message), "duration_ms": time.Since(start).Milliseconds(),
	})
}

func (s *Server) log(event string, fields map[string]any) {
	if s.logOut == nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if fields == nil {
		fields = map[string]any{}
	}
	fields["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["event"] = event
	_ = json.NewEncoder(s.logOut).Encode(fields)
}

func emptyToNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
