package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testAgentPublicKey  = "TrMm87V3aET3MmGUzHf3_XKZRPEHe1bDM-POH1mrjr8"
	testAgentPrivateKey = "keEk2aSPeUHiCbhK-XxleMUFj3cwzcJCFUflKSs_CiZOsybztXdoRPcyYZTMd_f9cplE8Qd7VsMz484fWauOvw"
)

func TestClientIPDirectConnection(t *testing.T) {
	s := NewServer(Config{}, nil)
	r := &http.Request{
		RemoteAddr: "203.0.113.10:12345",
		Header:     http.Header{"X-Forwarded-For": []string{"198.51.100.20"}},
	}

	if got := s.clientIP(r); got != "203.0.113.10" {
		t.Fatalf("clientIP() = %q, want direct remote address", got)
	}
}

func TestClientIPTrustedProxyUsesForwardedFor(t *testing.T) {
	trusted, err := parseIPBlocks("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{TrustedProxies: trusted}, nil)
	r := &http.Request{
		RemoteAddr: "10.0.0.5:443",
		Header:     http.Header{"X-Forwarded-For": []string{"198.51.100.20"}},
	}

	if got := s.clientIP(r); got != "198.51.100.20" {
		t.Fatalf("clientIP() = %q, want forwarded client address", got)
	}
}

func TestClientIPTrustedProxySkipsSpoofedForwardedFor(t *testing.T) {
	trusted, err := parseIPBlocks("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{TrustedProxies: trusted}, nil)
	r := &http.Request{
		RemoteAddr: "10.0.0.5:443",
		Header:     http.Header{"X-Forwarded-For": []string{"192.0.2.99, 198.51.100.20"}},
	}

	if got := s.clientIP(r); got != "198.51.100.20" {
		t.Fatalf("clientIP() = %q, want nearest untrusted forwarded address", got)
	}
}

func TestClientIPUntrustedProxyIgnoresForwardedHeaders(t *testing.T) {
	trusted, err := parseIPBlocks("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{TrustedProxies: trusted}, nil)
	r := &http.Request{
		RemoteAddr: "203.0.113.10:12345",
		Header: http.Header{
			"X-Forwarded-For": []string{"198.51.100.20"},
			"X-Real-IP":       []string{"198.51.100.21"},
		},
	}

	if got := s.clientIP(r); got != "203.0.113.10" {
		t.Fatalf("clientIP() = %q, want untrusted remote address", got)
	}
}

func TestClientIPTrustedProxyUsesXRealIPWhenForwardedForMissing(t *testing.T) {
	trusted, err := parseIPBlocks("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{TrustedProxies: trusted}, nil)
	r := &http.Request{RemoteAddr: "10.0.0.5:443", Header: http.Header{}}
	r.Header.Set("X-Real-IP", "198.51.100.21")

	if got := s.clientIP(r); got != "198.51.100.21" {
		t.Fatalf("clientIP() = %q, want X-Real-IP client address", got)
	}
}

func TestIPAllowListAllowsAndDenies(t *testing.T) {
	allow, err := parseIPBlocks("203.0.113.0/24")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{IPAllowList: allow, RateLimit: RateLimitConfig{RequestsPerSecond: 100, Burst: 100}}, nil)
	allowedReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	allowedReq.RemoteAddr = "203.0.113.10:12345"
	if !s.ipAllowed(allowedReq) {
		t.Fatal("ipAllowed() = false, want true")
	}

	deniedReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	deniedReq.RemoteAddr = "198.51.100.10:12345"
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, deniedReq)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayIPDenied, "source ip is not allowed")
}

func TestRateLimitAllowsExceedsAndRefills(t *testing.T) {
	s := NewServer(Config{RateLimit: RateLimitConfig{RequestsPerSecond: 10, Burst: 2}}, nil)
	if !s.take("203.0.113.10") {
		t.Fatal("first request rejected")
	}
	if !s.take("203.0.113.10") {
		t.Fatal("burst request rejected")
	}
	if s.take("203.0.113.10") {
		t.Fatal("third request allowed, want rate limited")
	}
	s.rateMu.Lock()
	s.rate["203.0.113.10"].last = time.Now().Add(-time.Second)
	s.rateMu.Unlock()
	if !s.take("203.0.113.10") {
		t.Fatal("request after refill rejected")
	}
}

func TestRateLimitHTTPErrorMessage(t *testing.T) {
	s := NewServer(Config{RateLimit: RateLimitConfig{RequestsPerSecond: 1, Burst: 1}}, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d; body=%s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
	assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayRateLimited, "too many requests")
}

func TestListenAndServeContextShutsDownOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveStarted := make(chan struct{})
	shutdownCalled := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- serveWithShutdown(
			ctx,
			":test",
			func() error {
				close(serveStarted)
				<-shutdownCalled
				return http.ErrServerClosed
			},
			func(context.Context) error {
				close(shutdownCalled)
				return nil
			},
			func(string, map[string]any) {},
		)
	}()
	<-serveStarted
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServeContext() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ListenAndServeContext() did not return after context cancel")
	}
}

func TestAuthenticateAcceptsBearerAndXAPIKey(t *testing.T) {
	s, _, apiKey := testServer(t)
	for _, tc := range []struct {
		name   string
		header http.Header
	}{
		{name: "bearer", header: canonicalHeader("Authorization", "Bearer "+apiKey)},
		{name: "x-api-key", header: canonicalHeader("X-API-Key", apiKey)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
			req.Header = tc.header
			rec := httptest.NewRecorder()
			key, ok := s.authenticate(rec, req)
			if !ok {
				t.Fatalf("authenticate() failed: status=%d body=%s", rec.Code, rec.Body.String())
			}
			if key.Name != "dev" {
				t.Fatalf("key.Name = %q, want dev", key.Name)
			}
		})
	}
}

func TestAuthenticateRejectsMissingAndInvalidAPIKey(t *testing.T) {
	s, _, _ := testServer(t)
	for _, tc := range []struct {
		name   string
		header http.Header
	}{
		{name: "missing", header: http.Header{}},
		{name: "invalid", header: canonicalHeader("Authorization", "Bearer wrong")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
			req.Header = tc.header
			rec := httptest.NewRecorder()
			if _, ok := s.authenticate(rec, req); ok {
				t.Fatal("authenticate() ok = true, want false")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayAuthFailed, "invalid api key")
		})
	}
}

func TestAllowedCapabilities(t *testing.T) {
	if allowed(APIKey{Name: "none"}, "get_customer") {
		t.Fatal("empty capabilities allowed request")
	}
	if !allowed(APIKey{Name: "one", Capabilities: []string{"get_customer"}}, "get_customer") {
		t.Fatal("specific capability rejected")
	}
	if allowed(APIKey{Name: "one", Capabilities: []string{"get_customer"}}, "search_orders") {
		t.Fatal("unlisted capability allowed")
	}
	if !allowed(APIKey{Name: "all", Capabilities: []string{"*"}}, "search_orders") {
		t.Fatal("wildcard capability rejected")
	}
}

func TestCapabilityEndpointHTTPErrorCases(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{name: "content-type", method: http.MethodPost, path: "/api/v1/capabilities/get_customer", contentType: "text/plain", body: `{}`, wantStatus: http.StatusBadRequest, wantCode: errGatewayInvalidRequest, wantMessage: "Content-Type must be application/json"},
		{name: "invalid-json", method: http.MethodPost, path: "/api/v1/capabilities/get_customer", contentType: "application/json", body: `{`, wantStatus: http.StatusBadRequest, wantCode: errGatewayInvalidRequest, wantMessage: "invalid json body"},
		{name: "method", method: http.MethodGet, path: "/api/v1/capabilities/get_customer", contentType: "application/json", body: `{}`, wantStatus: http.StatusMethodNotAllowed, wantCode: errGatewayMethodNotAllowed, wantMessage: "use POST"},
		{name: "empty-name", method: http.MethodPost, path: "/api/v1/capabilities/", contentType: "application/json", body: `{}`, wantStatus: http.StatusNotFound, wantCode: errGatewayCapabilityNotFound, wantMessage: "capability not found"},
		{name: "slash-name", method: http.MethodPost, path: "/api/v1/capabilities/get/customer", contentType: "application/json", body: `{}`, wantStatus: http.StatusNotFound, wantCode: errGatewayCapabilityNotFound, wantMessage: "capability not found"},
		{name: "offline", method: http.MethodPost, path: "/api/v1/capabilities/get_customer", contentType: "application/json", body: `{"id":1}`, wantStatus: http.StatusServiceUnavailable, wantCode: errGatewayAgentOffline, wantMessage: "agent is not connected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, logs, apiKey := testServer(t)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()
			s.httpSrv.Handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			assertAPIErrorMessage(t, rec.Body.Bytes(), tc.wantCode, tc.wantMessage)
			entry := lastLogEntry(t, logs)
			if entry["event"] != "request" || entry["error_code"] != tc.wantCode {
				t.Fatalf("unexpected access log: %#v", entry)
			}
			if entry["error_message"] != tc.wantMessage {
				t.Fatalf("error_message = %#v, want %q", entry["error_message"], tc.wantMessage)
			}
		})
	}
}

func TestCapabilityEndpointUsesDirectParamsBody(t *testing.T) {
	agentPayload := `{"rows":[{"id":7,"name":"Ada"}],"count":1}`
	s, logs, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		if _, nested := req.Params["params"]; nested {
			return agentResponse{ID: req.ID, Error: &wireError{Code: errAgentValidationFailed, Message: "nested params are not expected"}}
		}
		if req.Params["id"] != float64(7) {
			return agentResponse{ID: req.ID, Error: &wireError{Code: errAgentValidationFailed, Message: "missing direct id"}}
		}
		return agentResponse{ID: req.ID, Result: json.RawMessage(agentPayload)}
	})
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/get_customer", strings.NewReader(`{"id":7}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != agentPayload {
		t.Fatalf("body = %s, want exact agent payload %s", rec.Body.String(), agentPayload)
	}
	entry := lastLogEntry(t, logs)
	if entry["http_status"] != float64(http.StatusOK) || entry["error_code"] != nil {
		t.Fatalf("unexpected success access log: %#v", entry)
	}
	if entry["error_message"] != nil {
		t.Fatalf("success access log error_message = %#v, want nil", entry["error_message"])
	}
}

func TestAgentErrorCodeAndMessagePassThroughForRESTAndMCPHTTP(t *testing.T) {
	tests := []struct {
		code       string
		message    string
		wantStatus int
	}{
		{errGatewayCapabilityNotFound, "capability is not defined", http.StatusNotFound},
		{errAgentValidationFailed, "id: required param missing", http.StatusBadGateway},
		{errAgentQueryFailed, "database query failed", http.StatusBadGateway},
		{errAgentQueryTimeout, "query exceeded policy.timeout", http.StatusBadGateway},
		{errAgentDBUnreachable, "database is unreachable", http.StatusBadGateway},
		{errAgentInternal, "agent internal error", http.StatusBadGateway},
		{"AGENT_NEW_PUBLIC_ERROR", "future public message", http.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run("REST "+tc.code, func(t *testing.T) {
			s, logs, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
				return agentResponse{ID: req.ID, Error: &wireError{Code: tc.code, Message: tc.message}}
			})
			defer cleanup()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/get_customer", strings.NewReader(`{"id":7}`))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.httpSrv.Handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			assertAPIErrorMessage(t, rec.Body.Bytes(), tc.code, tc.message)
			entry := lastLogEntry(t, logs)
			if entry["error_code"] != tc.code || entry["error_message"] != tc.message {
				t.Fatalf("access log = %#v, want code/message pass-through", entry)
			}
		})
		t.Run("MCP "+tc.code, func(t *testing.T) {
			if tc.code == errGatewayCapabilityNotFound {
				t.Skip("tools/call maps capability-not-found to JSON-RPC INVALID_PARAMS")
			}
			s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
				return agentResponse{ID: req.ID, Error: &wireError{Code: tc.code, Message: tc.message}}
			})
			defer cleanup()
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{"id":7}}}`))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			rec := httptest.NewRecorder()
			s.httpSrv.Handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			assertAPIErrorMessage(t, rec.Body.Bytes(), tc.code, tc.message)
		})
	}
}

func TestAgentErrorMessagePassesThroughCapabilityResponseAndLogs(t *testing.T) {
	message := "id: required param missing"
	s, logs, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		return agentResponse{ID: req.ID, Error: &wireError{Code: errAgentValidationFailed, Message: message}}
	})
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/get_customer", strings.NewReader(`{"id":7}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	assertAPIErrorMessage(t, rec.Body.Bytes(), errAgentValidationFailed, message)
	if !strings.Contains(logs.String(), message) {
		t.Fatalf("agent message missing from logs: %s", logs.String())
	}
}

func TestCapabilityEndpointReturnsGatewayTimeoutWhenAgentDoesNotRespond(t *testing.T) {
	s, _, apiKey := testServer(t)
	s.cfg.AgentTimeout = time.Millisecond
	s.agent = &agentConn{conn: silentAgentConn{}, pending: map[string]chan agentResponse{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/get_customer", strings.NewReader(`{"id":7}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
	}
	assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayTimeout, "agent response timed out")
}

func TestMCPRequiresPost(t *testing.T) {
	s, _, apiKey := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()

	s.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayMethodNotAllowed, "use POST")
}

func TestMCPRejectsMissingID(t *testing.T) {
	s, _, apiKey := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()

	s.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	assertMCPErrorMessage(t, rec.Body.Bytes(), -32600, errJSONRPCInvalidRequest, "invalid json rpc request")
}

func TestMCPInitializePingParseAndMethodErrors(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantRPC    int
		wantApp    string
		wantMsg    string
		wantResult bool
	}{
		{name: "initialize", body: `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, wantResult: true},
		{name: "ping", body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`, wantResult: true},
		{name: "parse", body: `{`, wantRPC: -32700, wantApp: errJSONRPCParseError, wantMsg: "invalid json rpc"},
		{name: "method", body: `{"jsonrpc":"2.0","id":1,"method":"unknown"}`, wantRPC: -32601, wantApp: errJSONRPCMethodNotFound, wantMsg: "unsupported MCP method"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, apiKey := testServer(t)
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			rec := httptest.NewRecorder()
			s.httpSrv.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if tc.wantResult {
				if got["result"] == nil {
					t.Fatalf("result missing: %#v", got)
				}
				return
			}
			errObj := got["error"].(map[string]any)
			if errObj["code"] != float64(tc.wantRPC) {
				t.Fatalf("rpc code = %#v, want %d; body=%s", errObj["code"], tc.wantRPC, rec.Body.String())
			}
			if errObj["message"] != tc.wantMsg {
				t.Fatalf("rpc message = %#v, want %q; body=%s", errObj["message"], tc.wantMsg, rec.Body.String())
			}
			data := errObj["data"].(map[string]any)
			if data["code"] != tc.wantApp {
				t.Fatalf("app code = %#v, want %s; body=%s", data["code"], tc.wantApp, rec.Body.String())
			}
		})
	}
}

func TestMCPToolsListOfflineAndFiltered(t *testing.T) {
	s, _, apiKey := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayAgentOffline, "agent metadata is not cached yet")

	s.openapi = testOpenAPIDoc()
	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec = httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Result.Tools) != 1 || body.Result.Tools[0].Name != "get_customer" {
		t.Fatalf("tools = %#v, want only get_customer", body.Result.Tools)
	}
}

func TestMCPToolsCallUsesAgentResultAsContentAndStructuredContent(t *testing.T) {
	agentPayload := `{"rows":[{"id":7,"name":"Ada"}],"count":1}`
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		return agentResponse{ID: req.ID, Result: json.RawMessage(agentPayload)}
	})
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{"id":7}}}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Result.Content) != 1 || body.Result.Content[0].Text != agentPayload {
		t.Fatalf("content = %#v, want exact agent payload %s", body.Result.Content, agentPayload)
	}
	if body.Result.StructuredContent["count"] != float64(1) {
		t.Fatalf("structuredContent = %#v", body.Result.StructuredContent)
	}
}

func TestMCPToolsCallInvalidDeniedAndUnknown(t *testing.T) {
	t.Run("invalid params", func(t *testing.T) {
		s, _, apiKey := testServer(t)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		rec := httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(rec, req)
		assertMCPErrorMessage(t, rec.Body.Bytes(), -32602, errJSONRPCInvalidParams, "invalid tools/call params")
	})
	t.Run("denied", func(t *testing.T) {
		s, _, apiKey := testServer(t)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_orders","arguments":{}}}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		rec := httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
		}
		assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayCapabilityDenied, "capability not allowed")
	})
	t.Run("unknown", func(t *testing.T) {
		s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
			return agentResponse{ID: req.ID, Error: &wireError{Code: errGatewayCapabilityNotFound, Message: "capability is not defined"}}
		})
		defer cleanup()
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		rec := httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}
		assertMCPErrorMessage(t, rec.Body.Bytes(), -32602, errJSONRPCInvalidParams, "tool is not defined")
	})
}

func TestOpenAPIEndpointFiltersCapabilities(t *testing.T) {
	s, _, apiKey := testServer(t)
	s.openapi = s.finalizeOpenAPI(testOpenAPIDoc())
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	paths := body["paths"].(map[string]any)
	if _, ok := paths["/api/v1/capabilities/get_customer"]; !ok {
		t.Fatalf("allowed path missing: %#v", paths)
	}
	if _, ok := paths["/api/v1/capabilities/search_orders"]; ok {
		t.Fatalf("denied path present: %#v", paths)
	}
	assertOpenAPIGatewayMetadata(t, body, "http://localhost:8080")
}

func TestOpenAPIEndpointIncludesGatewayMetadataFromCachedMeta(t *testing.T) {
	raw := []byte(`{"data":{"openapi":"3.1.0","info":{"title":"IT"},"paths":{"/api/v1/capabilities/get_customer":{"post":{"x-onprest-capability":"get_customer"}}}}}`)
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		return agentResponse{ID: req.ID, Result: json.RawMessage(raw)}
	})
	defer cleanup()
	s.cfg.PublicURL = "https://tenant.example.com"

	s.fetchMeta()

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	assertOpenAPIGatewayMetadata(t, body, "https://tenant.example.com")
}

func TestOpenAPIEndpointWildcardReturnsAllCapabilities(t *testing.T) {
	apiKey := "wildcard"
	hash, err := hashSecret(apiKey)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	s := NewServer(Config{APIKeys: []APIKey{{Name: "admin", KeyHash: hash, Capabilities: []string{"*"}}}, AgentTimeout: 200 * time.Millisecond}, logs)
	s.openapi = s.finalizeOpenAPI(testOpenAPIDoc())
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	paths := body["paths"].(map[string]any)
	if len(paths) != 2 {
		t.Fatalf("len(paths) = %d, want 2: %#v", len(paths), paths)
	}
}

func TestCORSPreflightAllowsConfiguredOrigin(t *testing.T) {
	s, _, _ := testServer(t)
	s.cfg.CORSAllowedOrigins = []string{"http://localhost:8090"}

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/capabilities/get_customer", nil)
	req.Header.Set("Origin", "http://localhost:8090")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8090" {
		t.Fatalf("Access-Control-Allow-Origin = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Authorization, X-API-Key, Content-Type" {
		t.Fatalf("Access-Control-Allow-Headers = %q", got)
	}
}

func TestCORSPreflightRejectsUnconfiguredOrigin(t *testing.T) {
	s, _, _ := testServer(t)
	s.cfg.CORSAllowedOrigins = []string{"https://dashboard.example.com"}

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/capabilities/get_customer", nil)
	req.Header.Set("Origin", "http://localhost:8090")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestCORSHeadersOnAllowedOpenAPIRequest(t *testing.T) {
	s, _, apiKey := testServer(t)
	s.cfg.CORSAllowedOrigins = []string{"http://localhost:8090"}
	s.openapi = s.finalizeOpenAPI(testOpenAPIDoc())

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Origin", "http://localhost:8090")
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8090" {
		t.Fatalf("Access-Control-Allow-Origin = %q", got)
	}
}

func TestCapabilityErrorsAreAccessLoggedWithoutSensitiveData(t *testing.T) {
	s, logs, apiKey := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/get_customer", strings.NewReader(`{"password":"secret"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()

	s.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	entry := lastLogEntry(t, logs)
	if entry["event"] != "request" {
		t.Fatalf("event = %#v, want request", entry["event"])
	}
	if entry["capability"] != "get_customer" || entry["api_key_name"] != "dev" {
		t.Fatalf("unexpected access log: %#v", entry)
	}
	if entry["http_status"] != float64(http.StatusBadRequest) || entry["error_code"] != errGatewayInvalidRequest {
		t.Fatalf("unexpected access log status/code: %#v", entry)
	}
	if entry["error_message"] != "Content-Type must be application/json" {
		t.Fatalf("error_message = %#v, want Content-Type must be application/json", entry["error_message"])
	}
	if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "password") {
		t.Fatalf("access log leaked request params: %s", logs.String())
	}
}

func TestCapabilityAuthFailureIsAccessLogged(t *testing.T) {
	s, logs, _ := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/get_customer", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	entry := lastLogEntry(t, logs)
	if entry["http_status"] != float64(http.StatusUnauthorized) || entry["error_code"] != errGatewayAuthFailed {
		t.Fatalf("unexpected access log: %#v", entry)
	}
	if entry["error_message"] != "invalid api key" {
		t.Fatalf("error_message = %#v, want invalid api key", entry["error_message"])
	}
	if entry["api_key_name"] != "" {
		t.Fatalf("api_key_name = %#v, want empty", entry["api_key_name"])
	}
}

func TestOpenAPIFetchFailureLogDoesNotIncludeAgentDetail(t *testing.T) {
	s, logs, _ := testServer(t)

	s.fetchMeta()

	entry := lastLogEntry(t, logs)
	if entry["event"] != "openapi_fetch_failed" || entry["code"] != errGatewayAgentOffline {
		t.Fatalf("unexpected log entry: %#v", entry)
	}
	if _, ok := entry["detail"]; ok {
		t.Fatalf("openapi failure log included detail: %#v", entry)
	}
	if strings.Contains(logs.String(), "agent is not connected") {
		t.Fatalf("openapi failure log leaked agent detail: %s", logs.String())
	}
}

func TestOpenAPIMetaWrapperAndControlLog(t *testing.T) {
	raw := []byte(`{"data":{"openapi":"3.1.0","paths":{"/api/v1/capabilities/get_customer":{}}}}`)
	doc, err := openAPIFromMeta(raw)
	if err != nil {
		t.Fatal(err)
	}
	if openAPIPathCount(doc) != 1 {
		t.Fatalf("openAPIPathCount = %d, want 1", openAPIPathCount(doc))
	}

	s, logs, _, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		return agentResponse{ID: req.ID, Result: json.RawMessage(raw)}
	})
	defer cleanup()
	s.fetchMeta()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.agentMu.RLock()
		cached := s.openapi != nil
		s.agentMu.RUnlock()
		if cached {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	entry := lastLogEntry(t, logs)
	if entry["event"] != "openapi_cached" || entry["paths"] != float64(1) {
		t.Fatalf("unexpected control log: %#v", entry)
	}
	if _, ok := entry["openapi"]; ok {
		t.Fatalf("openapi_cached included full OpenAPI document: %#v", entry)
	}
	if strings.Contains(logs.String(), "openapi_snapshot") {
		t.Fatalf("unexpected OpenAPI snapshot log when disabled: %s", logs.String())
	}
}

func TestOpenAPISnapshotLogWhenEnabled(t *testing.T) {
	raw := []byte(`{"data":{"openapi":"3.1.0","info":{"title":"IT"},"paths":{"/api/v1/capabilities/get_customer":{"post":{"x-onprest-capability":"get_customer"}}}}}`)
	s, logs, _, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		return agentResponse{ID: req.ID, Result: json.RawMessage(raw)}
	})
	defer cleanup()
	s.cfg.EmitOpenAPISnapshot = true
	s.cfg.PublicURL = "https://snapshot.example.com"

	s.fetchMeta()

	entries := logEntries(t, logs)
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want exactly 2: %s", len(entries), logs.String())
	}
	cached := entries[0]
	if cached["event"] != "openapi_cached" || cached["paths"] != float64(1) {
		t.Fatalf("unexpected cached log before snapshot: %#v", cached)
	}
	if _, ok := cached["openapi"]; ok {
		t.Fatalf("openapi_cached included full OpenAPI document: %#v", cached)
	}
	snapshot := entries[1]
	if snapshot["event"] != "openapi_snapshot" || snapshot["paths"] != float64(1) {
		t.Fatalf("unexpected snapshot log: %#v", snapshot)
	}
	openAPI, ok := snapshot["openapi"].(map[string]any)
	if !ok {
		t.Fatalf("openapi field missing from snapshot log: %#v", snapshot)
	}
	if openAPI["openapi"] != "3.1.0" {
		t.Fatalf("snapshot OpenAPI version = %#v, want 3.1.0", openAPI["openapi"])
	}
	info, ok := openAPI["info"].(map[string]any)
	if !ok || info["title"] != "IT" {
		t.Fatalf("snapshot OpenAPI info = %#v, want title IT", openAPI["info"])
	}
	paths, ok := openAPI["paths"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot OpenAPI paths missing: %#v", openAPI)
	}
	if _, ok := paths["/api/v1/capabilities/get_customer"]; !ok {
		t.Fatalf("snapshot OpenAPI full document missing capability path: %#v", paths)
	}
	assertOpenAPIGatewayMetadata(t, openAPI, "https://snapshot.example.com")
}

func TestAgentErrorStatusMapping(t *testing.T) {
	tests := []struct {
		code       string
		wantStatus int
		wantCode   string
	}{
		{errGatewayCapabilityNotFound, http.StatusNotFound, errGatewayCapabilityNotFound},
		{errAgentValidationFailed, http.StatusBadGateway, errAgentValidationFailed},
		{errAgentQueryFailed, http.StatusBadGateway, errAgentQueryFailed},
		{errAgentQueryTimeout, http.StatusBadGateway, errAgentQueryTimeout},
		{errAgentDBUnreachable, http.StatusBadGateway, errAgentDBUnreachable},
		{errAgentInternal, http.StatusBadGateway, errAgentInternal},
		{"UNKNOWN", http.StatusBadGateway, "UNKNOWN"},
	}
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			status, code := agentErrorStatus(tc.code)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("agentErrorStatus(%q) = (%d, %q), want (%d, %q)", tc.code, status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestAuthenticateAgentRejectsInvalidHeadersAndReplay(t *testing.T) {
	s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
	missing := httptest.NewRequest(http.MethodGet, "/ws/agent", nil)
	if s.authenticateAgent(missing) {
		t.Fatal("missing headers authenticated")
	}

	badSignature := signedAgentRequest(t, "/ws/agent", time.Now().UTC(), "nonce-bad")
	badSignature.Header.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)))
	if s.authenticateAgent(badSignature) {
		t.Fatal("bad signature authenticated")
	}

	old := signedAgentRequest(t, "/ws/agent", time.Now().Add(-6*time.Minute).UTC(), "nonce-old")
	if s.authenticateAgent(old) {
		t.Fatal("stale timestamp authenticated")
	}

	future := signedAgentRequest(t, "/ws/agent", time.Now().Add(2*time.Minute).UTC(), "nonce-future")
	if s.authenticateAgent(future) {
		t.Fatal("future timestamp authenticated")
	}

	replay := signedAgentRequest(t, "/ws/agent", time.Now().UTC(), "nonce-replay")
	if !s.authenticateAgent(replay) {
		t.Fatal("valid signed request rejected")
	}
	replayAgain := signedAgentRequest(t, "/ws/agent", time.Now().UTC(), "nonce-replay")
	if s.authenticateAgent(replayAgain) {
		t.Fatal("replayed nonce authenticated")
	}
}

func TestAgentWebSocketHTTPErrorMessages(t *testing.T) {
	t.Run("invalid signature", func(t *testing.T) {
		s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
		req := httptest.NewRequest(http.MethodGet, "/ws/agent", nil)
		rec := httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
		assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayAuthFailed, "invalid agent signature")
	})
	t.Run("already connected", func(t *testing.T) {
		s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
		s.agent = &agentConn{conn: silentAgentConn{}, pending: map[string]chan agentResponse{}}
		req := signedAgentRequest(t, "/ws/agent", time.Now().UTC(), "nonce-already-connected")
		rec := httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
		}
		assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayAgentAlreadyConn, "agent already connected")
	})
}

func testServer(t *testing.T) (*Server, *bytes.Buffer, string) {
	t.Helper()
	apiKey := "test-api-key"
	hash, err := hashSecret(apiKey)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	s := NewServer(Config{
		AgentPublicKey: testAgentPublicKey,
		APIKeys: []APIKey{{
			Name:         "dev",
			KeyHash:      hash,
			Capabilities: []string{"get_customer"},
		}},
		RateLimit:    RateLimitConfig{RequestsPerSecond: 1000, Burst: 1000},
		AgentTimeout: 200 * time.Millisecond,
	}, logs)
	return s, logs, apiKey
}

func testServerWithAgent(t *testing.T, handler func(agentRequest) agentResponse) (*Server, *bytes.Buffer, string, func()) {
	t.Helper()
	s, logs, apiKey := testServer(t)
	ac := &agentConn{pending: map[string]chan agentResponse{}}
	fake := &fakeAgentConn{s: s, handler: handler}
	ac.conn = fake
	s.agent = ac
	cleanup := func() {
		s.agentMu.Lock()
		s.agent = nil
		s.agentMu.Unlock()
	}
	return s, logs, apiKey, cleanup
}

type fakeAgentConn struct {
	s       *Server
	handler func(agentRequest) agentResponse
}

type silentAgentConn struct{}

func (silentAgentConn) ReadText() ([]byte, error) { return nil, io.EOF }
func (silentAgentConn) WriteText([]byte) error    { return nil }
func (silentAgentConn) Close() error              { return nil }

func (f *fakeAgentConn) ReadText() ([]byte, error) {
	return nil, io.EOF
}

func (f *fakeAgentConn) WriteText(msg []byte) error {
	var req agentRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		return err
	}
	resp := agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	if f.handler != nil {
		resp = f.handler(req)
	} else if req.Capability == "meta" {
		resp.Result = json.RawMessage(`{"data":{"openapi":"3.1.0","paths":{}}}`)
	}
	go func() {
		f.s.agentMu.RLock()
		ac := f.s.agent
		f.s.agentMu.RUnlock()
		if ac == nil {
			return
		}
		ac.mu.Lock()
		ch := ac.pending[resp.ID]
		delete(ac.pending, resp.ID)
		ac.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}()
	return nil
}

func (f *fakeAgentConn) Close() error {
	return nil
}

func assertAPIError(t *testing.T, body []byte, want string) {
	t.Helper()
	var got struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Code != want {
		t.Fatalf("error code = %q, want %q; body=%s", got.Error.Code, want, string(body))
	}
	if got.Error.Message == "" {
		t.Fatalf("error message is empty; body=%s", string(body))
	}
}

func assertAPIErrorMessage(t *testing.T, body []byte, wantCode, wantMessage string) {
	t.Helper()
	var got struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Code != wantCode || got.Error.Message != wantMessage {
		t.Fatalf("error = (%q, %q), want (%q, %q); body=%s", got.Error.Code, got.Error.Message, wantCode, wantMessage, string(body))
	}
}

func assertMCPError(t *testing.T, body []byte, wantRPC int, wantApp string) {
	t.Helper()
	assertMCPErrorMessage(t, body, wantRPC, wantApp, "")
}

func assertMCPErrorMessage(t *testing.T, body []byte, wantRPC int, wantApp, wantMessage string) {
	t.Helper()
	var got struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Code != wantRPC || got.Error.Data.Code != wantApp {
		t.Fatalf("MCP error = (%d, %q), want (%d, %q); body=%s", got.Error.Code, got.Error.Data.Code, wantRPC, wantApp, string(body))
	}
	if wantMessage != "" && got.Error.Message != wantMessage {
		t.Fatalf("MCP error message = %q, want %q; body=%s", got.Error.Message, wantMessage, string(body))
	}
}

func canonicalHeader(key, value string) http.Header {
	h := http.Header{}
	h.Set(key, value)
	return h
}

func lastLogEntry(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	entries := logEntries(t, logs)
	return entries[len(entries)-1]
}

func logEntries(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("no log entries")
	}
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parse log entry: %v; logs=%s", err, logs.String())
		}
		entries = append(entries, entry)
	}
	return entries
}

func signedAgentRequest(t *testing.T, path string, ts time.Time, nonce string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header = signedAgentHeaders(t, path, ts, nonce)
	return req
}

func signedAgentHeaders(t *testing.T, path string, ts time.Time, nonce string) http.Header {
	t.Helper()
	privateKeyBytes, err := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := ts.Format(time.RFC3339)
	signature := ed25519.Sign(ed25519.PrivateKey(privateKeyBytes), agentAuthMessage(path, timestamp, nonce))
	headers := http.Header{}
	headers.Set("X-Agent-Timestamp", timestamp)
	headers.Set("X-Agent-Nonce", nonce)
	headers.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(signature))
	return headers
}

func testOpenAPIDoc() map[string]any {
	return map[string]any{
		"openapi": "3.1.0",
		"paths": map[string]any{
			"/api/v1/capabilities/get_customer": map[string]any{
				"post": map[string]any{
					"x-onprest-capability": "get_customer",
					"description":          "Get customer",
					"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"id": map[string]any{"type": "integer"}},
					}}}},
				},
			},
			"/api/v1/capabilities/search_orders": map[string]any{
				"post": map[string]any{
					"x-onprest-capability": "search_orders",
					"description":          "Search orders",
				},
			},
		},
	}
}

func assertOpenAPIGatewayMetadata(t *testing.T, doc map[string]any, wantServerURL string) {
	t.Helper()
	servers, ok := doc["servers"].([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("servers = %#v, want one server", doc["servers"])
	}
	server, ok := servers[0].(map[string]any)
	if !ok || server["url"] != wantServerURL {
		t.Fatalf("servers[0] = %#v, want url %q", servers[0], wantServerURL)
	}

	security, ok := doc["security"].([]any)
	if !ok || len(security) != 2 {
		t.Fatalf("security = %#v, want bearer and X-API-Key alternatives", doc["security"])
	}
	if _, ok := security[0].(map[string]any)["bearerAuth"]; !ok {
		t.Fatalf("security missing bearerAuth alternative: %#v", security)
	}
	if _, ok := security[1].(map[string]any)["apiKeyAuth"]; !ok {
		t.Fatalf("security missing apiKeyAuth alternative: %#v", security)
	}

	components, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatalf("components = %#v", doc["components"])
	}
	schemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatalf("securitySchemes = %#v", components["securitySchemes"])
	}
	bearer, ok := schemes["bearerAuth"].(map[string]any)
	if !ok || bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Fatalf("bearerAuth = %#v", schemes["bearerAuth"])
	}
	apiKey, ok := schemes["apiKeyAuth"].(map[string]any)
	if !ok || apiKey["type"] != "apiKey" || apiKey["in"] != "header" || apiKey["name"] != "X-API-Key" {
		t.Fatalf("apiKeyAuth = %#v", schemes["apiKeyAuth"])
	}
}
