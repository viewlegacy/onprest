package gateway

import (
	"encoding/json"
	"strings"

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
)

// The older HTTP/security fixtures describe business rows/counts. This helper
// constructs the current typed Agent frame for those fixtures; production has
// no decoder for the earlier frame or API shape. Wire rejection cases below
// use explicit Response values and bypass this fixture constructor.
func versionedTestResponse(req agentRequest, resp agentResponse) agentResponse {
	if resp.Error != nil {
		resp.Type = "error"
		if resp.Error.Outcome == "" {
			resp.Error.Outcome = "unknown"
		}
		return resp
	}
	if resp.Type != "" {
		return resp
	}
	if req.Capability == "meta" {
		var m map[string]any
		if decodeJSONNumber(resp.Result, &m) != nil {
			return resp
		}
		kinds := map[string]string{}
		if raw, ok := m["response_kinds"].(map[string]any); ok {
			for k, v := range raw {
				if s, ok := v.(string); ok {
					kinds[k] = s
				}
			}
		}
		if doc, ok := m["data"].(map[string]any); ok {
			if doc["info"] == nil {
				doc["info"] = map[string]any{"title": "fixture", "version": "1.0.0"}
			}
			if info, ok := doc["info"].(map[string]any); ok {
				if info["title"] == nil || info["title"] == "" {
					info["title"] = "fixture"
				}
				if info["version"] == nil || info["version"] == "" {
					info["version"] = "1.0.0"
				}
			}
			if paths, ok := doc["paths"].(map[string]any); ok {
				for p, value := range paths {
					name := strings.TrimPrefix(p, "/api/v1/capabilities/")
					if operations, ok := value.(map[string]any); ok {
						if len(operations) == 0 {
							operations["post"] = map[string]any{}
						}
						if operation, ok := operations["post"].(map[string]any); ok {
							if operation["x-onprest-capability"] == nil {
								operation["x-onprest-capability"] = name
							}
						}
					}
					if kinds[name] == "" {
						kinds[name] = "select"
					}
				}
			}
		}
		caps := map[string]protocol.CapabilityMetadata{}
		for name, kind := range kinds {
			effect := "none"
			if kind == "mutation" {
				effect = "possible"
			}
			caps[name] = protocol.CapabilityMetadata{DefinitionHash: strings.Repeat("a", 64), ExecutorKind: "db", ExecutionTimeout: "5s", EffectClass: effect, InputSchema: map[string]any{"type": "object", "additionalProperties": true}, InputLimits: map[string]int64{"max_input_bytes": 1 << 20, "max_json_depth": 32, "max_json_items": 100000}}
		}
		// Empty OpenAPI fixtures still have one prepared hidden DB capability.
		if len(caps) == 0 {
			caps["fixture"] = protocol.CapabilityMetadata{DefinitionHash: strings.Repeat("a", 64), ExecutorKind: "db", ExecutionTimeout: "5s", EffectClass: "none", InputSchema: map[string]any{"type": "object", "additionalProperties": true}, InputLimits: map[string]int64{"max_input_bytes": 1 << 20, "max_json_depth": 32, "max_json_items": 100000}}
			kinds["fixture"] = "select"
		}
		m["capabilities"] = caps
		m["response_kinds"] = kinds
		resp.Result = protocol.MustJSON(m)
		resp.Type = "metadata"
		return resp
	}
	var m map[string]json.RawMessage
	if decodeJSONNumber(resp.Result, &m) != nil {
		return resp
	}
	var count int64
	err := json.Unmarshal(m["count"], &count)
	metrics := &protocol.Metrics{ExecutorKind: "db"}
	data := json.RawMessage(resp.Result)
	if rows, ok := m["rows"]; ok {
		_ = rows
		if err == nil {
			metrics.ReturnedRows = &count
		}
	} else if err == nil {
		metrics.AffectedRows = &count
	}
	canonical, e := contract.DecodeJSON(data, contract.Limits{MaxBytes: 16 << 20, MaxDepth: 32, MaxItems: 100000})
	if e == nil {
		b, _ := contract.CanonicalJSON(canonical)
		metrics.ResponseBytes = int64(len(b))
	}
	resp.Type = "result"
	resp.Result = protocol.MustJSON(map[string]any{"data": data, "files": []any{}})
	resp.Metrics = metrics
	return resp
}

func seedVersionedTestMetadata(s *Server) {
	s.openapi = map[string]any{"openapi": "3.1.0", "paths": map[string]any{}}
	s.capabilities = map[string]protocol.CapabilityMetadata{}
	for _, name := range []string{"get_customer", "slow", "fast", "first", "second", "capability", "mutate"} {
		s.capabilities[name] = protocol.CapabilityMetadata{DefinitionHash: strings.Repeat("a", 64), ExecutorKind: "db", ExecutionTimeout: s.cfg.AgentTimeout.String(), EffectClass: "none", InputSchema: map[string]any{"type": "object", "additionalProperties": true}, InputLimits: map[string]int64{"max_input_bytes": 1 << 20, "max_json_depth": 32, "max_json_items": 100000}}
	}
	setVersionedTestKind(s, "mutate", responseKindMutation)
}

func setVersionedTestKind(s *Server, name string, kind responseKind) {
	value, ok := s.capabilities[name]
	if !ok {
		value = s.capabilities["get_customer"]
	}
	value.EffectClass = "none"
	if kind == responseKindMutation {
		value.EffectClass = "possible"
	}
	s.capabilities[name] = value
}

// Expected envelopes retain the old fixture's business assertions while also
// checking the current request identity and data/files or error outcome shape.
func expectedTestEnvelope(t interface {
	Helper()
	Fatalf(string, ...any)
}, text string, business map[string]any) map[string]any {
	t.Helper()
	var actual map[string]any
	if decodeJSONNumber([]byte(text), &actual) != nil {
		t.Fatalf("invalid envelope: %s", text)
	}
	id, ok := actual["request_id"].(string)
	if !ok || len(id) != 26 {
		t.Fatalf("invalid request identity: %s", text)
	}
	if e, ok := business["error"].(map[string]any); ok {
		v := map[string]any{}
		for k, x := range e {
			v[k] = x
		}
		v["outcome"] = "unknown"
		return map[string]any{"request_id": id, "error": v}
	}
	data := any(business)
	if rows, ok := business["rows"]; ok {
		_ = rows
	}
	return map[string]any{"request_id": id, "result": map[string]any{"data": data, "files": []any{}}}
}
func testEnvelopeErrorMessage(t interface {
	Helper()
	Fatalf(string, ...any)
}, text string) string {
	t.Helper()
	var e contract.ErrorEnvelope
	if json.Unmarshal([]byte(text), &e) != nil || len(e.RequestID) != 26 || e.Error.Validate() != nil {
		t.Fatalf("invalid error envelope: %s", text)
	}
	return e.Error.Message
}
func equivalentTestBusiness(t interface {
	Helper()
	Fatalf(string, ...any)
}, text, want string) bool {
	t.Helper()
	var actual any
	var business map[string]any
	if decodeJSONNumber([]byte(text), &actual) != nil || decodeJSONNumber([]byte(want), &business) != nil {
		return false
	}
	expected := expectedTestEnvelope(t, text, business)
	a, _ := json.Marshal(actual)
	b, _ := json.Marshal(expected)
	return string(a) == string(b)
}

func assertMCPToolErrorLegacy(t interface {
	Helper()
	Fatalf(string, ...any)
}, body []byte, want string) {
	t.Helper()
	var rpc struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &rpc) != nil || !rpc.Result.IsError || len(rpc.Result.Content) != 1 {
		t.Fatalf("invalid tool error: %s", body)
	}
	var failure contract.ErrorEnvelope
	if json.Unmarshal([]byte(rpc.Result.Content[0].Text), &failure) != nil || len(failure.RequestID) != 26 || failure.Error.Code != want || failure.Error.Validate() != nil {
		t.Fatalf("wrong tool error: %s", body)
	}
}
