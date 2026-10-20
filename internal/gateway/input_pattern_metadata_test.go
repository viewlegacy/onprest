package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/viewlegacy/onprest/internal/contract"
)

func TestInputPatternMetadataStrictRecompileAndDispatch(t *testing.T) {
	source, e := contract.CompileSchema([]byte(`properties: {customer: {type: string, pattern: '^[A-Za-z0-9 ]{1,64}$'}}
required: [customer]`), true)
	if e != nil {
		t.Fatal(e)
	}
	fixture := versionedTestResponse(agentRequest{Capability: "meta"}, agentResponse{Result: json.RawMessage(`{"data":{"openapi":"3.1.0","paths":{"/api/v1/capabilities/get_customer":{"post":{"x-onprest-capability":"get_customer"}}}},"response_kinds":{"get_customer":"select"}}`)})
	var metadata map[string]any
	if decodeJSONNumber(fixture.Result, &metadata) != nil {
		t.Fatal("fixture")
	}
	cap := metadata["capabilities"].(map[string]any)["get_customer"].(map[string]any)
	cap["input_schema"] = source.InputMetadata()
	calls := 0
	server, _, _, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		if req.Capability == "meta" {
			raw, _ := json.Marshal(metadata)
			return agentResponse{Type: "metadata", ID: req.ID, Result: raw}
		}
		calls++
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	})
	defer cleanup()
	refresh := func() bool {
		server.openapi, server.capabilities, server.responseKinds = nil, nil, nil
		server.fetchMeta()
		return server.openapi != nil
	}
	if !refresh() {
		t.Fatal("string metadata rejected")
	}
	result := server.callInput(context.Background(), "get_customer", map[string]any{"customer": "Alice"})
	if result.Status != http.StatusOK || calls != 1 {
		t.Fatal(result, calls)
	}
	for _, invalid := range []string{"=SUM(A1)", "Alice!", strings.Repeat("a", 65)} {
		result = server.callInput(context.Background(), "get_customer", map[string]any{"customer": invalid})
		if result.Code != "VALIDATION_FAILED" || calls != 1 {
			t.Fatal("invalid input dispatched", result, calls)
		}
	}
	for name, pattern := range map[string]any{"ast": source.Normalized()["properties"].(map[string]any)["customer"].(map[string]any)["pattern"], "nonstring": 42, "flag": "(?i)a", "lookaround": "(?=a)", "unicode_property": `\p{L}`} {
		t.Run(name, func(t *testing.T) {
			bad := source.InputMetadata()
			bad["properties"].(map[string]any)["customer"].(map[string]any)["pattern"] = pattern
			cap["input_schema"] = bad
			if refresh() {
				t.Fatal("invalid pattern metadata admitted")
			}
			if server.callInput(context.Background(), "get_customer", map[string]any{"customer": "Alice"}).Status != 503 || calls != 1 {
				t.Fatal("invalid metadata dispatched")
			}
		})
	}
	bad := source.InputMetadata()
	bad["properties"].(map[string]any)["customer"].(map[string]any)["remote"] = true
	cap["input_schema"] = bad
	if refresh() {
		t.Fatal("unknown schema field admitted")
	}
	cap["input_schema"] = source.InputMetadata()
	if !refresh() {
		t.Fatal("valid metadata did not recover")
	}
}
