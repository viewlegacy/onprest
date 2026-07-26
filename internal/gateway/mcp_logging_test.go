package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func requestLogEntries(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	if strings.TrimSpace(logs.String()) == "" {
		return nil
	}
	all := logEntries(t, logs)
	requests := make([]map[string]any, 0)
	for _, entry := range all {
		if entry["event"] == "request" {
			requests = append(requests, entry)
		}
	}
	return requests
}

func serveMCPForLogTest(s *Server, apiKey, method, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	recorder := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(recorder, req)
	return recorder
}

func TestMCPPreToolCallBranchesEmitNoRequestEvent(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		body       string
		authorized bool
	}{
		{"method", http.MethodGet, ``, true},
		{"authentication", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, false},
		{"body limit", http.MethodPost, strings.Repeat("x", int(defaultMaxRequestBodyBytes)+1), true},
		{"JSON parse", http.MethodPost, `{`, true},
		{"trailing JSON", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}{}`, true},
		{"JSON-RPC invalid", http.MethodPost, `{"jsonrpc":"1.0","id":1,"method":"tools/call"}`, true},
		{"initialize", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, true},
		{"initialized notification", http.MethodPost, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, true},
		{"ping", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, true},
		{"tools list", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, true},
		{"tool notification", http.MethodPost, `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, true},
		{"unsupported", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s, logs, apiKey := testServer(t)
			if !test.authorized {
				apiKey = ""
			}
			serveMCPForLogTest(s, apiKey, test.method, test.body)
			if entries := requestLogEntries(t, logs); len(entries) != 0 {
				t.Fatalf("pre-tools/call branch emitted request event: %#v; all logs=%s", entries, logs.String())
			}
		})
	}
}

func TestMCPMiddlewareRejectionsUseControlEventsNotRequestEvents(t *testing.T) {
	allow, err := parseIPBlocks("198.51.100.0/24")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := NewServer(Config{IPAllowList: allow}, &logs)
	recorder := serveMCPForLogTest(s, "", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("IP rejection status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if entries := requestLogEntries(t, &logs); len(entries) != 0 {
		t.Fatalf("IP rejection emitted request events: %#v", entries)
	}
	entries := logEntries(t, &logs)
	if len(entries) != 1 || entries[0]["event"] != "mcp_http_rejected" || entries[0]["error_code"] != errGatewayIPDenied {
		t.Fatalf("IP rejection control event=%#v", entries)
	}

	s, logsPtr, apiKey := testServer(t)
	s.cfg.RateLimit.RequestsPerSecond = 0.000001
	s.cfg.RateLimit.Burst = 1
	serveMCPForLogTest(s, apiKey, http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	rateLimited := serveMCPForLogTest(s, apiKey, http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)
	if rateLimited.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit status=%d body=%s", rateLimited.Code, rateLimited.Body.String())
	}
	if requests := requestLogEntries(t, logsPtr); len(requests) != 0 {
		t.Fatalf("rate limit emitted request event: %#v", requests)
	}
	if entries := logEntries(t, logsPtr); entries[len(entries)-1]["event"] != "mcp_http_rejected" || entries[len(entries)-1]["error_code"] != errGatewayRateLimited {
		t.Fatalf("rate limit control event=%#v", entries)
	}
}

func TestMCPToolsCallBranchesEmitExactlyOneCompleteRequestEvent(t *testing.T) {
	type testCase struct {
		name           string
		body           string
		server         func(*testing.T) (*Server, *bytes.Buffer, string, func())
		wantCapability string
		wantStatus     int
		wantCode       any
		wantMessage    any
	}
	basic := func(t *testing.T) (*Server, *bytes.Buffer, string, func()) {
		s, logs, key := testServer(t)
		return s, logs, key, func() {}
	}
	offline := basic
	success := func(t *testing.T) (*Server, *bytes.Buffer, string, func()) {
		return testServerWithAgent(t, func(req agentRequest) agentResponse {
			return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
		})
	}
	agentFailure := func(code, message string) func(*testing.T) (*Server, *bytes.Buffer, string, func()) {
		return func(t *testing.T) (*Server, *bytes.Buffer, string, func()) {
			return testServerWithAgent(t, func(req agentRequest) agentResponse {
				return agentResponse{ID: req.ID, Error: &wireError{Code: code, Message: message}}
			})
		}
	}
	toolError := agentFailure(errAgentQueryFailed, "database query failed")
	validationError := agentFailure(errAgentValidationFailed, "parameters failed validation")
	timeoutError := agentFailure(errAgentQueryTimeout, "query exceeded policy.timeout")
	dbUnreachableError := agentFailure(errAgentDBUnreachable, "database unavailable")
	internalError := agentFailure(errAgentInternal, "agent internal error")
	busyError := agentFailure(errAgentBusy, "agent concurrency limit reached")
	unknownAgentError := agentFailure("AGENT_FUTURE_ERROR", "future agent error")
	unknown := func(t *testing.T) (*Server, *bytes.Buffer, string, func()) {
		return testServerWithAgent(t, func(req agentRequest) agentResponse {
			return agentResponse{ID: req.ID, Error: &wireError{Code: errGatewayCapabilityNotFound, Message: "capability is not defined"}}
		})
	}
	cases := []testCase{
		{"invalid params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, basic, "", http.StatusOK, errJSONRPCInvalidParams, "invalid tools/call params"},
		{"capability denied", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_orders","arguments":{}}}`, basic, "search_orders", http.StatusForbidden, errGatewayCapabilityDenied, "capability not allowed"},
		{"agent offline", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, offline, "get_customer", http.StatusOK, errGatewayAgentOffline, "agent is not connected"},
		{"tool undefined", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, unknown, "get_customer", http.StatusOK, errJSONRPCInvalidParams, "tool is not defined"},
		{"validation error", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, validationError, "get_customer", http.StatusOK, errAgentValidationFailed, "parameters failed validation"},
		{"tool execution error", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, toolError, "get_customer", http.StatusOK, errAgentQueryFailed, "database query failed"},
		{"query timeout", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, timeoutError, "get_customer", http.StatusOK, errAgentQueryTimeout, "query exceeded policy.timeout"},
		{"database unreachable", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, dbUnreachableError, "get_customer", http.StatusOK, errAgentDBUnreachable, "database unavailable"},
		{"agent internal error", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, internalError, "get_customer", http.StatusOK, errAgentInternal, "agent internal error"},
		{"agent busy", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, busyError, "get_customer", http.StatusOK, errAgentBusy, "agent concurrency limit reached"},
		{"unknown agent error pass-through", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, unknownAgentError, "get_customer", http.StatusOK, "AGENT_FUTURE_ERROR", "future agent error"},
		{"success", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, success, "get_customer", http.StatusOK, nil, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s, logs, apiKey, cleanup := test.server(t)
			defer cleanup()
			serveMCPForLogTest(s, apiKey, http.MethodPost, test.body)
			entries := requestLogEntries(t, logs)
			if len(entries) != 1 {
				t.Fatalf("request event count=%d want=1 entries=%#v all=%s", len(entries), entries, logs.String())
			}
			entry := entries[0]
			assertAccessLogCoreFields(t, entry)
			if entry["protocol"] != "mcp" || entry["capability"] != test.wantCapability || entry["api_key_name"] != "dev" ||
				entry["http_status"] != float64(test.wantStatus) || entry["error_code"] != test.wantCode || entry["error_message"] != test.wantMessage {
				t.Fatalf("MCP request event fields=%#v", entry)
			}
			for _, forbidden := range []string{"arguments", "params", "authorization", "token", "secret", "path"} {
				if _, exists := entry[forbidden]; exists {
					t.Fatalf("MCP request event contains forbidden field %q: %#v", forbidden, entry)
				}
			}
		})
	}
}

func TestRESTRequestEventIncludesProtocol(t *testing.T) {
	s, logs, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"ok":true}`)}
	})
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/get_customer", strings.NewReader(`{"id":7}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(recorder, req)
	entry := lastLogEntry(t, logs)
	if entry["protocol"] != "rest" {
		t.Fatalf("REST protocol field=%#v", entry)
	}
}
