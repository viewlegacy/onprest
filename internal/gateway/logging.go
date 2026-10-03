package gateway

import (
	"encoding/json"
	"time"
)

func (s *Server) accessLog(requestID, keyName, capability string, status int, code string, message string, start time.Time, count ...*int64) {
	s.accessLogProtocol("rest", requestID, keyName, capability, status, code, message, start, count...)
}

func (s *Server) accessLogProtocol(protocol, requestID, keyName, capability string, status int, code string, message string, start time.Time, counts ...*int64) {
	var count any
	if len(counts) > 0 && counts[0] != nil {
		count = *counts[0]
	}
	s.log("request", map[string]any{
		"protocol": protocol, "request_id": requestID, "capability": capability, "api_key_name": keyName,
		"http_status": status, "count": count, "error_code": emptyToNil(code), "error_message": emptyToNil(message), "duration_ms": time.Since(start).Milliseconds(),
	})
}

func (s *Server) mcpHTTPRejected(status int, code, message string) {
	s.log("mcp_http_rejected", map[string]any{
		"http_status": status, "error_code": code, "error_message": message,
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

func (s *Server) accessExecutionLog(protocol, requestID, keyName, capability string, result agentCallResult, start time.Time) {
	fields := map[string]any{"protocol": protocol, "request_id": requestID, "capability": capability, "api_key_name": keyName, "http_status": result.Status, "count": nil, "error_code": emptyToNil(result.Code), "error_message": emptyToNil(result.Message), "duration_ms": time.Since(start).Milliseconds()}
	if result.Count != nil {
		fields["count"] = *result.Count
	}
	if m := result.Metrics; m != nil {
		fields["executor_kind"] = m.ExecutorKind
		fields["returned_rows"] = m.ReturnedRows
		fields["affected_rows"] = m.AffectedRows
		fields["file_count"] = m.FileCount
		fields["response_bytes"] = m.ResponseBytes
		fields["input_file_count"] = m.InputFileCount
		fields["input_file_bytes"] = m.InputFileBytes
	}
	if result.Failure != nil {
		fields["outcome"] = result.Failure.Outcome
		fields["reason"] = emptyToNil(result.Failure.Reason)
	}
	s.log("request", fields)
}
