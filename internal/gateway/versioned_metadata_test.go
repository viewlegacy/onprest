package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
)

func TestVersionedMetadataRejectsPartialAndInconsistentCachesAndGatesCalls(t *testing.T) {
	valid := `{"data":{"openapi":"3.1.0","paths":{"/api/v1/capabilities/get_customer":{"post":{"x-onprest-capability":"get_customer"}}}},"response_kinds":{"get_customer":"select"}}`
	fixture := versionedTestResponse(agentRequest{Capability: "meta"}, agentResponse{Result: json.RawMessage(valid)})
	var base map[string]any
	if decodeJSONNumber(fixture.Result, &base) != nil {
		t.Fatal("invalid test metadata")
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing_data":         func(m map[string]any) { delete(m, "data") },
		"empty_data":           func(m map[string]any) { m["data"] = map[string]any{} },
		"missing_capabilities": func(m map[string]any) { delete(m, "capabilities") },
		"empty_capabilities":   func(m map[string]any) { m["capabilities"] = map[string]any{} },
		"missing_kinds":        func(m map[string]any) { delete(m, "response_kinds") },
		"empty_kinds":          func(m map[string]any) { m["response_kinds"] = map[string]any{} },
		"wrong_version":        func(m map[string]any) { m["data"].(map[string]any)["openapi"] = "2.0" },
		"missing_info":         func(m map[string]any) { delete(m["data"].(map[string]any), "info") },
		"missing_info_version": func(m map[string]any) { delete(m["data"].(map[string]any)["info"].(map[string]any), "version") },
		"missing_paths":        func(m map[string]any) { delete(m["data"].(map[string]any), "paths") },
		"malformed_paths":      func(m map[string]any) { m["data"].(map[string]any)["paths"] = []any{} },
		"mismatched_names":     func(m map[string]any) { m["response_kinds"] = map[string]any{"other": "select"} },
		"invalid_kind":         func(m map[string]any) { m["response_kinds"] = map[string]any{"get_customer": "unknown"} },
		"effect_kind_mismatch": func(m map[string]any) { m["response_kinds"] = map[string]any{"get_customer": "mutation"} },
		"missing_hash": func(m map[string]any) {
			delete(m["capabilities"].(map[string]any)["get_customer"].(map[string]any), "definition_hash")
		},
		"uppercase_hash": func(m map[string]any) {
			m["capabilities"].(map[string]any)["get_customer"].(map[string]any)["definition_hash"] = strings.Repeat("A", 64)
		},
		"missing_limits": func(m map[string]any) {
			delete(m["capabilities"].(map[string]any)["get_customer"].(map[string]any), "input_limits")
		},
		"unknown_limit": func(m map[string]any) {
			m["capabilities"].(map[string]any)["get_customer"].(map[string]any)["input_limits"].(map[string]any)["other"] = 1
		},
		"invalid_root_schema": func(m map[string]any) {
			m["capabilities"].(map[string]any)["get_customer"].(map[string]any)["input_schema"] = map[string]any{"type": "array"}
		},
		"unknown_public_path": func(m map[string]any) {
			m["data"].(map[string]any)["paths"] = map[string]any{"/api/v1/capabilities/other": map[string]any{"post": map[string]any{"x-onprest-capability": "other"}}}
		},
		"wrong_public_binding": func(m map[string]any) {
			m["data"].(map[string]any)["paths"] = map[string]any{"/api/v1/capabilities/get_customer": map[string]any{"post": map[string]any{"x-onprest-capability": "other"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := cloneMap(base)
			mutate(bad)
			metadata := bad
			businessCalls := 0
			s, _, _, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
				if req.Capability == "meta" {
					raw, _ := json.Marshal(metadata)
					return agentResponse{Type: "metadata", ID: req.ID, Result: raw}
				}
				businessCalls++
				if req.DefinitionHash != strings.Repeat("a", 64) {
					t.Error("business request omitted the prepared definition hash")
				}
				return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
			})
			defer cleanup()
			s.openapi, s.capabilities, s.responseKinds = nil, nil, nil
			s.fetchMeta()
			if s.openapi != nil || s.capabilities != nil || s.responseKinds != nil {
				t.Fatal("invalid metadata reached the cache")
			}
			if response := s.callAgent(context.Background(), "get_customer", nil); response.Status != http.StatusServiceUnavailable || businessCalls != 0 {
				t.Fatal("business request bypassed the metadata gate")
			}
			metadata = base
			s.fetchMeta()
			if s.openapi == nil || s.callAgent(context.Background(), "get_customer", nil).Status != http.StatusOK || businessCalls != 1 {
				t.Fatal("valid metadata did not restore the prepared hash path")
			}
			value := s.capabilities["get_customer"]
			value.DefinitionHash = ""
			s.capabilities["get_customer"] = value
			if s.callAgent(context.Background(), "get_customer", nil).Status != http.StatusServiceUnavailable || businessCalls != 1 {
				t.Fatal("empty hash bypassed the metadata gate")
			}
		})
	}
}

func TestVersionedMetadataHiddenOnlyDocumentRemainsValid(t *testing.T) {
	s, _, _, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		return versionedTestResponse(req, agentResponse{ID: req.ID, Result: json.RawMessage(`{"data":{"openapi":"3.1.0","paths":{}},"response_kinds":{"get_customer":"select"}}`)})
	})
	defer cleanup()
	s.openapi, s.capabilities, s.responseKinds = nil, nil, nil
	s.fetchMeta()
	if s.openapi == nil || len(s.openapi["paths"].(map[string]any)) != 0 || len(s.capabilities) != 1 || len(s.responseKinds) != 1 {
		t.Fatal("hidden-only metadata was rejected or advertised a path")
	}
}

func TestVersionedMetadataWebSocketCacheGateBeforeHTTPDispatch(t *testing.T) {
	s, _, apiKey := testServer(t)
	server := httptest.NewServer(s.httpSrv.Handler)
	defer server.Close()
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/get_customer", strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer "+apiKey)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(response, request)
		return response
	}
	// Authenticate before a pending metadata request starts its 200ms budget.
	if response := call(); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline response: %s", response.Body)
	}
	conn := dialSignedTestAgent(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent", "metadata-gate")
	defer conn.Close()
	readRequest := func() agentRequest {
		t.Helper()
		message, err := conn.ReadText()
		var request agentRequest
		if err != nil || json.Unmarshal(message, &request) != nil {
			t.Fatalf("request: %v", err)
		}
		return request
	}
	first := readRequest()
	if first.Capability != "meta" {
		t.Fatal("expected initial metadata request")
	}
	if err := conn.WriteText(protocol.MustJSON(map[string]any{"type": "metadata", "id": first.ID, "metadata": map[string]any{"data": map[string]any{"openapi": "3.1.0", "paths": map[string]any{}}, "capabilities": map[string]any{}, "response_kinds": map[string]any{}}})); err != nil {
		t.Fatal(err)
	}
	if response := call(); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unprepared metadata reached HTTP dispatch: %s", response.Body)
	}
	second := readRequest()
	if second.Capability != "meta" {
		t.Fatal("invalid metadata admitted a business request")
	}
	valid := versionedTestResponse(second, agentResponse{ID: second.ID, Result: json.RawMessage(`{"data":{"openapi":"3.1.0","paths":{"/api/v1/capabilities/get_customer":{"post":{}}}}}`)})
	if err := conn.WriteText(protocol.MustJSON(valid)); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, time.Second, func() bool { s.agentMu.RLock(); defer s.agentMu.RUnlock(); return s.openapi != nil }, "valid metadata cache")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call() }()
	business := readRequest()
	if business.Capability != "get_customer" || business.DefinitionHash != strings.Repeat("a", 64) {
		t.Fatal("normal HTTP request omitted its prepared definition hash")
	}
	if err := conn.WriteText(protocol.MustJSON(versionedTestResponse(business, agentResponse{ID: business.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}))); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-done:
		if response.Code != http.StatusOK {
			t.Fatalf("prepared business request failed: %s", response.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("business request did not complete")
	}
}
