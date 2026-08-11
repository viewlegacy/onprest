package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestExtractCountContract(t *testing.T) {
	tests := []struct {
		raw     string
		want    int64
		invalid bool
	}{
		{`{"count":0}`, 0, false},
		{`{"count":9223372036854775807}`, 9223372036854775807, false},
		{`{"count":-1}`, 0, true},
		{`{"count":1.5}`, 0, true},
		{`{"count":9223372036854775808}`, 0, true},
		{`{"rows":[]}`, 0, true},
	}
	for _, tc := range tests {
		count, invalid := extractCount(json.RawMessage(tc.raw))
		if invalid != tc.invalid {
			t.Fatalf("extractCount(%s) invalid=%t want=%t", tc.raw, invalid, tc.invalid)
		}
		if !invalid && (count == nil || *count != tc.want) {
			t.Fatalf("extractCount(%s)=%v want=%d", tc.raw, count, tc.want)
		}
	}
}

func TestCallAgentResponseKindContract(t *testing.T) {
	tests := []struct {
		name       string
		kind       responseKind
		payload    string
		wantStatus int
		wantCode   string
		wantCount  *int64
	}{
		{name: "known select", kind: responseKindSelect, payload: `{"rows":[],"count":2}`, wantStatus: http.StatusOK, wantCount: int64Ptr(2)},
		{name: "known mutation", kind: responseKindMutation, payload: `{"count":0}`, wantStatus: http.StatusOK, wantCount: int64Ptr(0)},
		{name: "unknown valid", kind: responseKindUnknown, payload: `{"count":3}`, wantStatus: http.StatusOK, wantCount: int64Ptr(3)},
		{name: "select missing", kind: responseKindSelect, payload: `{"rows":[]}`, wantStatus: http.StatusBadGateway, wantCode: errAgentInternal},
		{name: "mutation missing", kind: responseKindMutation, payload: `{}`, wantStatus: http.StatusBadGateway, wantCode: errAgentTransactionOutcomeUnknown},
		{name: "unknown missing", kind: responseKindUnknown, payload: `{}`, wantStatus: http.StatusBadGateway, wantCode: errAgentTransactionOutcomeUnknown},
		{name: "mutation negative", kind: responseKindMutation, payload: `{"count":-1}`, wantStatus: http.StatusBadGateway, wantCode: errAgentTransactionOutcomeUnknown},
		{name: "mutation fraction", kind: responseKindMutation, payload: `{"count":1.5}`, wantStatus: http.StatusBadGateway, wantCode: errAgentTransactionOutcomeUnknown},
		{name: "mutation overflow", kind: responseKindMutation, payload: `{"count":9223372036854775808}`, wantStatus: http.StatusBadGateway, wantCode: errAgentTransactionOutcomeUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
				return agentResponse{ID: req.ID, Result: json.RawMessage(tc.payload)}
			})
			defer cleanup()
			if tc.kind != responseKindUnknown {
				s.responseKinds = map[string]responseKind{"cap": tc.kind}
			}
			result := s.callAgent(context.Background(), "cap", nil)
			if result.Status != tc.wantStatus || result.Code != tc.wantCode {
				t.Fatalf("result=%+v want status=%d code=%q", result, tc.wantStatus, tc.wantCode)
			}
			if tc.wantCount == nil {
				if result.Count != nil {
					t.Fatalf("count=%v want nil", result.Count)
				}
			} else if result.Count == nil || *result.Count != *tc.wantCount {
				t.Fatalf("count=%v want=%d", result.Count, *tc.wantCount)
			}
		})
	}
}

func int64Ptr(value int64) *int64 { return &value }

func TestMetaResponseKindsArePrivateAndParsed(t *testing.T) {
	doc, kinds, err := metaData([]byte(`{"data":{"openapi":"3.1.0","paths":{}},"response_kinds":{"read":"select","write":"mutation","bad":"other"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.1.0" || kinds["read"] != responseKindSelect || kinds["write"] != responseKindMutation {
		t.Fatalf("doc=%v kinds=%v", doc, kinds)
	}
	if _, ok := kinds["bad"]; ok {
		t.Fatalf("unsupported kind retained: %v", kinds)
	}
}

func TestRESTAndMCPMutationPayloadPassThroughAndCountLog(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    responseKind
		payload string
		count   float64
	}{
		{name: "select", kind: responseKindSelect, payload: `{"rows":[{"id":1}],"count":1}`, count: 1},
		{name: "mutation zero", kind: responseKindMutation, payload: `{"count":0}`, count: 0},
	} {
		t.Run(tc.name+" REST", func(t *testing.T) {
			s, logs, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
				return agentResponse{ID: req.ID, Result: json.RawMessage(tc.payload)}
			})
			defer cleanup()
			s.responseKinds = map[string]responseKind{"get_customer": tc.kind}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/get_customer", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.httpSrv.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != tc.payload {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if count := lastLogEntry(t, logs)["count"]; count != tc.count {
				t.Fatalf("log count=%#v want=%v", count, tc.count)
			}
		})
		t.Run(tc.name+" MCP", func(t *testing.T) {
			s, logs, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
				return agentResponse{ID: req.ID, Result: json.RawMessage(tc.payload)}
			})
			defer cleanup()
			s.responseKinds = map[string]responseKind{"get_customer": tc.kind}
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			rec := httptest.NewRecorder()
			s.httpSrv.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":`+strconv.Quote(tc.payload)) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if count := lastLogEntry(t, logs)["count"]; count != tc.count {
				t.Fatalf("log count=%#v want=%v", count, tc.count)
			}
		})
	}
}

func TestMissingCountClassificationThroughRESTAndMCPHandlers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     responseKind
		wantCode string
	}{
		{name: "known select", kind: responseKindSelect, wantCode: errAgentInternal},
		{name: "known mutation", kind: responseKindMutation, wantCode: errAgentTransactionOutcomeUnknown},
		{name: "unknown", kind: responseKindUnknown, wantCode: errAgentTransactionOutcomeUnknown},
	} {
		for _, protocolName := range []string{"REST", "MCP"} {
			t.Run(tc.name+" "+protocolName, func(t *testing.T) {
				s, logs, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
					return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[]}`)}
				})
				defer cleanup()
				if tc.kind != responseKindUnknown {
					s.responseKinds = map[string]responseKind{"get_customer": tc.kind}
				}
				if protocolName == "REST" {
					req := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/get_customer", strings.NewReader(`{}`))
					req.Header.Set("Authorization", "Bearer "+apiKey)
					req.Header.Set("Content-Type", "application/json")
					rec := httptest.NewRecorder()
					s.httpSrv.Handler.ServeHTTP(rec, req)
					if rec.Code != http.StatusBadGateway {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					assertAPIErrorMessage(t, rec.Body.Bytes(), tc.wantCode, map[string]string{errAgentInternal: "agent internal error", errAgentTransactionOutcomeUnknown: "transaction outcome is unknown"}[tc.wantCode])
				} else {
					req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`))
					req.Header.Set("Authorization", "Bearer "+apiKey)
					rec := httptest.NewRecorder()
					s.httpSrv.Handler.ServeHTTP(rec, req)
					assertMCPToolError(t, rec.Body.Bytes(), tc.wantCode, map[string]string{errAgentInternal: "agent internal error", errAgentTransactionOutcomeUnknown: "transaction outcome is unknown"}[tc.wantCode])
				}
				entry := lastLogEntry(t, logs)
				if entry["count"] != nil || entry["error_code"] != tc.wantCode {
					t.Fatalf("log=%#v", entry)
				}
			})
		}
	}
}
