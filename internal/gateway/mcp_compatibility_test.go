package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/viewlegacy/onprest/internal/buildinfo"
)

func newMCPCompatibilityRequest(method, body string) *http.Request {
	req := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func mcpInitializePayload(id any, protocolVersion string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"onprest-test-client","version":"test"}}}`, mustJSONString(id), protocolVersion)
}

func mustJSONString(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func serveMCPCompatibility(s *Server, apiKey string, req *http.Request) *httptest.ResponseRecorder {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	recorder := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(recorder, req)
	return recorder
}

func decodeMCPCompatibilityResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode MCP response: %v; body=%s", err, rec.Body.String())
	}
	return body
}

func assertMCPErrorIDNull(t *testing.T, raw []byte) {
	t.Helper()
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode MCP error id: %v; body=%s", err, raw)
	}
	if !bytes.Equal(bytes.TrimSpace(envelope.ID), []byte("null")) {
		t.Fatalf("MCP error id=%s, want explicit null; body=%s", envelope.ID, raw)
	}
}

type mcpTestToolCallEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   json.RawMessage `json:"error"`
	Result  struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	} `json:"result"`
}

func decodeMCPJSONUseNumber(t *testing.T, raw []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode MCP JSON: %v; raw=%s", err, raw)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("MCP JSON has trailing data: err=%v raw=%s", err, raw)
	}
}

func decodeMCPToolCallEnvelope(t *testing.T, body []byte) mcpTestToolCallEnvelope {
	t.Helper()
	var response mcpTestToolCallEnvelope
	decodeMCPJSONUseNumber(t, body, &response)
	if response.JSONRPC != "2.0" || (len(response.Error) != 0 && string(response.Error) != "null") {
		t.Fatalf("MCP tool call envelope=%#v; body=%s", response, body)
	}
	if len(response.Result.Content) != 1 || response.Result.Content[0].Type != "text" || response.Result.Content[0].Text == "" {
		t.Fatalf("MCP tool call result=%#v, want one non-empty text item; body=%s", response.Result, body)
	}
	return response
}

func requireMCPStructuredObject(t *testing.T, raw json.RawMessage) {
	t.Helper()
	if len(bytes.TrimSpace(raw)) == 0 {
		t.Fatal("MCP structuredContent is absent")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		t.Fatalf("MCP structuredContent=%s, want JSON object (err=%v)", raw, err)
	}
}

func mcpCompatibilityTool(t *testing.T, result map[string]any, name string) map[string]any {
	t.Helper()
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools=%#v", result["tools"])
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if ok && tool["name"] == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found in %#v", name, tools)
	return nil
}

func TestMCPInitializeNegotiatesSupportedAndUnknownVersions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested string
		want      string
	}{
		{name: "legacy", requested: mcpProtocolVersion20250326, want: mcpProtocolVersion20250326},
		{name: "middle", requested: mcpProtocolVersion20250618, want: mcpProtocolVersion20250618},
		{name: "latest", requested: mcpProtocolVersion20251125, want: mcpProtocolVersion20251125},
		{name: "unknown", requested: "2099-01-01", want: mcpProtocolVersion20251125},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, apiKey := testServer(t)
			req := newMCPCompatibilityRequest(http.MethodPost, mcpInitializePayload("init", tc.requested))
			// Initialization negotiates from the body and deliberately ignores the
			// subsequent-request header contract.
			req.Header.Set(mcpProtocolHeader, "2099-01-01")
			rec := serveMCPCompatibility(s, apiKey, req)
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("status=%d content-type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
			}
			body := decodeMCPCompatibilityResponse(t, rec)
			result := body["result"].(map[string]any)
			if result["protocolVersion"] != tc.want {
				t.Fatalf("protocolVersion=%#v want %q", result["protocolVersion"], tc.want)
			}
			serverInfo := result["serverInfo"].(map[string]any)
			if serverInfo["version"] != buildinfo.Current() {
				t.Fatalf("serverInfo.version=%#v want %q", serverInfo["version"], buildinfo.Current())
			}
		})
	}
}

func TestMCPInitializeRequiresStringProtocolVersion(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":null,"capabilities":{},"clientInfo":{"name":"client","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"client","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"client","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":1,"version":"1"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":null,"clientInfo":{"name":"client","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{ "name":"client"}}}`,
	} {
		s, _, apiKey := testServer(t)
		rec := serveMCPCompatibility(s, apiKey, newMCPCompatibilityRequest(http.MethodPost, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertMCPErrorMessage(t, rec.Body.Bytes(), -32602, errJSONRPCInvalidParams, "invalid initialize params")
	}
}

func TestMCPInitializeStructuralValidationDoesNotCallAgent(t *testing.T) {
	var calls atomic.Int32
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		calls.Add(1)
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	})
	defer cleanup()
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"client"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"client","version":null}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":[],"clientInfo":{"name":"client","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{"roots":{"listChanged":"yes"}},"clientInfo":{"name":"client","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{"roots":{"listChanged":null}},"clientInfo":{"name":"client","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{"experimental":{"x":null}},"clientInfo":{"name":"client","version":"1"}}}`,
	} {
		rec := serveMCPCompatibility(s, apiKey, newMCPCompatibilityRequest(http.MethodPost, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertMCPErrorMessage(t, rec.Body.Bytes(), -32602, errJSONRPCInvalidParams, "invalid initialize params")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("agent calls=%d want 0 for invalid initialize", got)
	}
}

func TestMCPInitializePreservesOpenCapabilityExtensions(t *testing.T) {
	for _, tc := range []struct {
		name, version, capabilities string
	}{
		{name: "legacy extension", version: mcpProtocolVersion20250326, capabilities: `{"future":null,"elicitation":null}`},
		{name: "middle known capability", version: mcpProtocolVersion20250618, capabilities: `{"future":null,"elicitation":{}}`},
		{name: "latest task capability shape", version: mcpProtocolVersion20251125, capabilities: `{"future":null,"tasks":{},"elicitation":{"form":{}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, apiKey := testServer(t)
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":%s,"clientInfo":{"name":"client","version":"1"}}}`, tc.version, tc.capabilities)
			rec := serveMCPCompatibility(s, apiKey, newMCPCompatibilityRequest(http.MethodPost, body))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if _, ok := decodeMCPCompatibilityResponse(t, rec)["result"]; !ok {
				t.Fatalf("initialize response=%s", rec.Body.String())
			}
		})
	}
}

func TestMCPInitializeClientInfoOptionalFieldsMatchVersionedSchemas(t *testing.T) {
	type infoCase struct {
		name   string
		fields string
		valid  bool
	}
	casesByVersion := map[string][]infoCase{
		mcpProtocolVersion20250326: {
			{name: "all later fields are opaque", fields: `,"title":null,"description":null,"websiteUrl":null,"icons":null,"future":{"nested":[null]}`, valid: true},
			{name: "unknown scalar is opaque", fields: `,"future":false`, valid: true},
		},
		mcpProtocolVersion20250618: {
			{name: "title type and later fields opaque", fields: `,"title":"Display","description":null,"websiteUrl":{},"icons":null,"future":[]`, valid: true},
			{name: "title null", fields: `,"title":null`, valid: false},
			{name: "title array", fields: `,"title":[]`, valid: false},
		},
		mcpProtocolVersion20251125: {
			{name: "all defined optional fields", fields: `,"title":"Display","description":"Client","websiteUrl":"https://example.com/client","icons":[{"src":"https://example.com/icon.png","mimeType":"image/png","sizes":["48x48"],"theme":"light"}],"future":null`, valid: true},
			{name: "unknown field opaque", fields: `,"future":{"nested":false}`, valid: true},
			{name: "title null", fields: `,"title":null`, valid: false},
			{name: "description null", fields: `,"description":null`, valid: false},
			{name: "websiteUrl null", fields: `,"websiteUrl":null`, valid: false},
			{name: "icons null", fields: `,"icons":null`, valid: false},
			{name: "icons object", fields: `,"icons":{}`, valid: false},
		},
	}
	versions := []struct {
		name, header string
	}{
		{name: mcpProtocolVersion20250326},
		{name: mcpProtocolVersion20250618, header: mcpProtocolVersion20250618},
		{name: mcpProtocolVersion20251125, header: mcpProtocolVersion20251125},
	}
	var calls atomic.Int32
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		calls.Add(1)
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	})
	defer cleanup()
	for _, version := range versions {
		version := version
		t.Run(version.name, func(t *testing.T) {
			for _, tc := range casesByVersion[version.name] {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					body := fmt.Sprintf(`{"jsonrpc":"2.0","id":"client-info","method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"client","version":"1"%s}}}`, version.name, tc.fields)
					req := newMCPCompatibilityRequest(http.MethodPost, body)
					if version.header != "" {
						req.Header.Set(mcpProtocolHeader, version.header)
					}
					rec := serveMCPCompatibility(s, apiKey, req)
					if rec.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					if tc.valid {
						body := decodeMCPCompatibilityResponse(t, rec)
						if _, present := body["error"]; present {
							t.Fatalf("valid clientInfo response has error=%#v", body["error"])
						}
						result, ok := body["result"].(map[string]any)
						if !ok || result["protocolVersion"] != version.name {
							t.Fatalf("valid clientInfo result=%#v want protocolVersion %q", body["result"], version.name)
						}
					} else {
						assertMCPErrorMessage(t, rec.Body.Bytes(), -32602, errJSONRPCInvalidParams, "invalid initialize params")
					}
				})
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("agent calls=%d want 0 for initialize clientInfo validation", got)
	}
}

func TestMCPMethodParamsMatchVersionedSchemas(t *testing.T) {
	var calls atomic.Int32
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		calls.Add(1)
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	})
	defer cleanup()
	s.openapi = mcpCompatibilityOpenAPIDoc()

	versions := []struct {
		name, header string
	}{
		{name: mcpProtocolVersion20250326},
		{name: mcpProtocolVersion20250618, header: mcpProtocolVersion20250618},
		{name: mcpProtocolVersion20251125, header: mcpProtocolVersion20251125},
	}
	type paramsCase struct {
		name    string
		raw     string
		valid   bool
		message string
	}
	cases := []struct {
		method string
		params []paramsCase
	}{
		{method: "initialize", params: []paramsCase{
			{name: "complete", raw: `{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"client","version":"1"}}`, valid: true},
			{name: "optional meta and extension", raw: `{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"client","version":"1"},"_meta":{},"extension":true}`, valid: true},
			{name: "meta progress token string", raw: `{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"client","version":"1"},"_meta":{"progressToken":"p"}}`, valid: true},
			{name: "meta progress token number", raw: `{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"client","version":"1"},"_meta":{"progressToken":7}}`, valid: true},
			{name: "omitted", message: "invalid initialize params"},
			{name: "null", raw: `null`, message: "invalid initialize params"},
			{name: "array", raw: `[]`, message: "invalid initialize params"},
			{name: "scalar", raw: `"params"`, message: "invalid initialize params"},
			{name: "meta null", raw: `{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"client","version":"1"},"_meta":null}`, message: "invalid initialize params"},
			{name: "meta progress token null", raw: `{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"client","version":"1"},"_meta":{"progressToken":null}}`, message: "invalid initialize params"},
			{name: "meta progress token array", raw: `{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"client","version":"1"},"_meta":{"progressToken":[]}}`, message: "invalid initialize params"},
			{name: "meta progress token object", raw: `{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"client","version":"1"},"_meta":{"progressToken":{}}}`, message: "invalid initialize params"},
		}},
		{method: "ping", params: []paramsCase{
			{name: "omitted", valid: true},
			{name: "empty object", raw: `{}`, valid: true},
			{name: "meta and extension", raw: `{"_meta":{},"extension":{"value":1}}`, valid: true},
			{name: "meta progress token", raw: `{"_meta":{"progressToken":"p"}}`, valid: true},
			{name: "meta numeric progress token", raw: `{"_meta":{"progressToken":7}}`, valid: true},
			{name: "null", raw: `null`, message: "invalid ping params"},
			{name: "array", raw: `[]`, message: "invalid ping params"},
			{name: "scalar", raw: `1`, message: "invalid ping params"},
			{name: "meta null", raw: `{"_meta":null}`, message: "invalid ping params"},
			{name: "meta progress token null", raw: `{"_meta":{"progressToken":null}}`, message: "invalid ping params"},
			{name: "meta progress token array", raw: `{"_meta":{"progressToken":[]}}`, message: "invalid ping params"},
			{name: "meta progress token object", raw: `{"_meta":{"progressToken":{}}}`, message: "invalid ping params"},
		}},
		{method: "tools/list", params: []paramsCase{
			{name: "omitted", valid: true},
			{name: "empty object", raw: `{}`, valid: true},
			{name: "cursor and extension", raw: `{"cursor":"opaque","extension":true}`, valid: true},
			{name: "meta", raw: `{"_meta":{}}`, valid: true},
			{name: "meta progress token string", raw: `{"_meta":{"progressToken":"p"}}`, valid: true},
			{name: "meta progress token number", raw: `{"_meta":{"progressToken":7}}`, valid: true},
			{name: "null", raw: `null`, message: "invalid tools/list params"},
			{name: "array", raw: `[]`, message: "invalid tools/list params"},
			{name: "scalar", raw: `1`, message: "invalid tools/list params"},
			{name: "cursor null", raw: `{"cursor":null}`, message: "invalid tools/list params"},
			{name: "cursor number", raw: `{"cursor":1}`, message: "invalid tools/list params"},
			{name: "meta null", raw: `{"_meta":null}`, message: "invalid tools/list params"},
			{name: "meta progress token null", raw: `{"_meta":{"progressToken":null}}`, message: "invalid tools/list params"},
			{name: "meta progress token array", raw: `{"_meta":{"progressToken":[]}}`, message: "invalid tools/list params"},
			{name: "meta progress token object", raw: `{"_meta":{"progressToken":{}}}`, message: "invalid tools/list params"},
		}},
		{method: "tools/call", params: []paramsCase{
			{name: "name only", raw: `{"name":"get_customer"}`, valid: true},
			{name: "arguments and extension", raw: `{"name":"get_customer","arguments":{},"_meta":{},"extension":true}`, valid: true},
			{name: "meta progress token string", raw: `{"name":"get_customer","_meta":{"progressToken":"p"}}`, valid: true},
			{name: "meta progress token number", raw: `{"name":"get_customer","_meta":{"progressToken":7}}`, valid: true},
			{name: "omitted", message: "invalid tools/call params"},
			{name: "null", raw: `null`, message: "invalid tools/call params"},
			{name: "array", raw: `[]`, message: "invalid tools/call params"},
			{name: "scalar", raw: `"params"`, message: "invalid tools/call params"},
			{name: "meta null", raw: `{"name":"get_customer","_meta":null}`, message: "invalid tools/call params"},
			{name: "meta progress token null", raw: `{"name":"get_customer","_meta":{"progressToken":null}}`, message: "invalid tools/call params"},
			{name: "meta progress token array", raw: `{"name":"get_customer","_meta":{"progressToken":[]}}`, message: "invalid tools/call params"},
			{name: "meta progress token object", raw: `{"name":"get_customer","_meta":{"progressToken":{}}}`, message: "invalid tools/call params"},
			{name: "arguments null", raw: `{"name":"get_customer","arguments":null}`, message: "invalid tools/call params"},
		}},
	}
	for _, version := range versions {
		version := version
		t.Run(version.name, func(t *testing.T) {
			for _, methodCase := range cases {
				methodCase := methodCase
				params := append([]paramsCase(nil), methodCase.params...)
				if methodCase.method == "tools/call" && version.name != mcpProtocolVersion20251125 {
					params = append(params, paramsCase{name: "unknown task extension", raw: `{"name":"get_customer","task":{"id":"extension"}}`, valid: true})
				}
				if methodCase.method == "tools/call" && version.name == mcpProtocolVersion20251125 {
					params = append(params,
						paramsCase{name: "task object unsupported", raw: `{"name":"get_customer","task":{"id":"unsupported"}}`, message: "invalid tools/call params"},
						paramsCase{name: "task null unsupported", raw: `{"name":"get_customer","task":null}`, message: "invalid tools/call params"},
					)
				}
				for _, tc := range params {
					tc := tc
					t.Run(methodCase.method+"/"+tc.name, func(t *testing.T) {
						raw := tc.raw
						if methodCase.method == "initialize" && strings.Contains(raw, "%s") {
							raw = fmt.Sprintf(raw, version.name)
						}
						paramsPart := ""
						if raw != "" {
							paramsPart = `,"params":` + raw
						}
						body := fmt.Sprintf(`{"jsonrpc":"2.0","id":"params-test","method":%q%s}`, methodCase.method, paramsPart)
						req := newMCPCompatibilityRequest(http.MethodPost, body)
						if version.header != "" {
							req.Header.Set(mcpProtocolHeader, version.header)
						}
						before := calls.Load()
						rec := serveMCPCompatibility(s, apiKey, req)
						if rec.Code != http.StatusOK {
							t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
						}
						if tc.valid {
							assertMCPMethodSuccess(t, rec, methodCase.method, version.name)
							if methodCase.method == "tools/call" && calls.Load() != before+1 {
								t.Fatalf("agent calls=%d want %d for valid params", calls.Load(), before+1)
							}
							return
						}
						assertMCPErrorMessage(t, rec.Body.Bytes(), -32602, errJSONRPCInvalidParams, tc.message)
						if calls.Load() != before {
							t.Fatalf("agent calls=%d changed from %d for invalid %s params", calls.Load(), before, methodCase.method)
						}
					})
				}
			}
		})
	}
}

func assertMCPMethodSuccess(t *testing.T, rec *httptest.ResponseRecorder, method, protocolVersion string) {
	t.Helper()
	body := decodeMCPCompatibilityResponse(t, rec)
	if body["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc=%#v want %q; body=%s", body["jsonrpc"], "2.0", rec.Body.String())
	}
	if _, present := body["error"]; present {
		t.Fatalf("successful %s response contains error=%#v; body=%s", method, body["error"], rec.Body.String())
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("successful %s response result=%#v; body=%s", method, body["result"], rec.Body.String())
	}
	switch method {
	case "initialize":
		if got, ok := result["protocolVersion"].(string); !ok || got != protocolVersion {
			t.Fatalf("initialize protocolVersion=%#v want %q; result=%#v", result["protocolVersion"], protocolVersion, result)
		}
	case "ping":
		if len(result) != 0 {
			t.Fatalf("ping result=%#v want empty object", result)
		}
	case "tools/list":
		if _, ok := result["tools"].([]any); !ok {
			t.Fatalf("tools/list result.tools=%#v want array; result=%#v", result["tools"], result)
		}
		mcpCompatibilityTool(t, result, "get_customer")
	case "tools/call":
		content, ok := result["content"].([]any)
		if !ok || len(content) != 1 {
			t.Fatalf("tools/call result.content=%#v want one item; result=%#v", result["content"], result)
		}
		item, ok := content[0].(map[string]any)
		if !ok || item["type"] != "text" || item["text"] != `{"rows":[],"count":0}` {
			t.Fatalf("tools/call content=%#v want successful text result", content[0])
		}
	}
}

func TestMCPMethodPresenceAndEmptyStringSemantics(t *testing.T) {
	cases := []struct {
		name        string
		methodField string
		withID      bool
		wantStatus  int
		wantRPCCode int
		wantAppCode string
		wantMessage string
	}{
		{name: "method missing with id", withID: true, wantStatus: http.StatusOK, wantRPCCode: -32600, wantAppCode: errJSONRPCInvalidRequest, wantMessage: "invalid json rpc request"},
		{name: "method missing without id", wantStatus: http.StatusOK, wantRPCCode: -32600, wantAppCode: errJSONRPCInvalidRequest, wantMessage: "invalid json rpc request"},
		{name: "method number with id", methodField: `,"method":1`, withID: true, wantStatus: http.StatusOK, wantRPCCode: -32600, wantAppCode: errJSONRPCInvalidRequest, wantMessage: "invalid json rpc request"},
		{name: "method number without id", methodField: `,"method":1`, wantStatus: http.StatusOK, wantRPCCode: -32600, wantAppCode: errJSONRPCInvalidRequest, wantMessage: "invalid json rpc request"},
		{name: "method null with id", methodField: `,"method":null`, withID: true, wantStatus: http.StatusOK, wantRPCCode: -32600, wantAppCode: errJSONRPCInvalidRequest, wantMessage: "invalid json rpc request"},
		{name: "method object without id", methodField: `,"method":{}`, wantStatus: http.StatusOK, wantRPCCode: -32600, wantAppCode: errJSONRPCInvalidRequest, wantMessage: "invalid json rpc request"},
		{name: "empty method with id", methodField: `,"method":""`, withID: true, wantStatus: http.StatusOK, wantRPCCode: -32601, wantAppCode: errJSONRPCMethodNotFound, wantMessage: "unsupported MCP method"},
		{name: "empty method without id", methodField: `,"method":""`, wantStatus: http.StatusBadRequest, wantAppCode: errGatewayInvalidRequest, wantMessage: "invalid json rpc notification"},
	}
	versions := []struct {
		name, header string
	}{
		{name: mcpProtocolVersion20250326},
		{name: mcpProtocolVersion20250618, header: mcpProtocolVersion20250618},
		{name: mcpProtocolVersion20251125, header: mcpProtocolVersion20251125},
	}
	for _, version := range versions {
		version := version
		t.Run(version.name, func(t *testing.T) {
			for _, tc := range cases {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					s, logs, apiKey := testServer(t)
					id := ""
					if tc.withID {
						id = `,"id":"method-shape"`
					}
					body := fmt.Sprintf(`{"jsonrpc":"2.0"%s%s}`, id, tc.methodField)
					req := newMCPCompatibilityRequest(http.MethodPost, body)
					if version.header != "" {
						req.Header.Set(mcpProtocolHeader, version.header)
					}
					rec := serveMCPCompatibility(s, apiKey, req)
					if rec.Code != tc.wantStatus {
						t.Fatalf("status=%d want %d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
					}
					if tc.wantRPCCode != 0 {
						assertMCPErrorMessage(t, rec.Body.Bytes(), tc.wantRPCCode, tc.wantAppCode, tc.wantMessage)
					} else {
						assertAPIErrorMessage(t, rec.Body.Bytes(), tc.wantAppCode, tc.wantMessage)
					}
					if entries := requestLogEntries(t, logs); len(entries) != 0 {
						t.Fatalf("invalid/unknown method emitted request events: %#v", entries)
					}
				})
			}
		})
	}
}

func TestMCPToolsCallNameTypeAndEmptyStringSemantics(t *testing.T) {
	cases := []struct {
		name        string
		params      string
		emptyName   bool
		wantRPCCode int
		wantMessage string
	}{
		{name: "name missing", params: `{"arguments":{}}`, wantRPCCode: -32602, wantMessage: "invalid tools/call params"},
		{name: "name null", params: `{"name":null,"arguments":{}}`, wantRPCCode: -32602, wantMessage: "invalid tools/call params"},
		{name: "name number", params: `{"name":1,"arguments":{}}`, wantRPCCode: -32602, wantMessage: "invalid tools/call params"},
		{name: "name array", params: `{"name":[],"arguments":{}}`, wantRPCCode: -32602, wantMessage: "invalid tools/call params"},
		{name: "name empty string", params: `{"name":"","arguments":{}}`, emptyName: true, wantRPCCode: -32602, wantMessage: "tool is not defined"},
	}
	versions := []struct {
		name, header string
	}{
		{name: mcpProtocolVersion20250326},
		{name: mcpProtocolVersion20250618, header: mcpProtocolVersion20250618},
		{name: mcpProtocolVersion20251125, header: mcpProtocolVersion20251125},
	}
	for _, version := range versions {
		version := version
		t.Run(version.name, func(t *testing.T) {
			for _, withID := range []bool{true, false} {
				withID := withID
				for _, tc := range cases {
					tc := tc
					t.Run(fmt.Sprintf("%s/id-%t", tc.name, withID), func(t *testing.T) {
						var calls atomic.Int32
						var agentCapability string
						s, logs, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
							calls.Add(1)
							agentCapability = req.Capability
							return agentResponse{ID: req.ID, Error: &wireError{Code: errGatewayCapabilityNotFound, Message: "capability is not defined"}}
						})
						defer cleanup()
						// The empty tool name must pass authorization so that the
						// agent's not-found response remains distinguishable from
						// malformed tools/call params.
						s.cfg.APIKeys[0].Capabilities = capabilities{"*"}
						id := ""
						if withID {
							id = `,"id":1`
						}
						body := fmt.Sprintf(`{"jsonrpc":"2.0"%s,"method":"tools/call","params":%s}`, id, tc.params)
						req := newMCPCompatibilityRequest(http.MethodPost, body)
						if version.header != "" {
							req.Header.Set(mcpProtocolHeader, version.header)
						}
						rec := serveMCPCompatibility(s, apiKey, req)
						if withID {
							if rec.Code != http.StatusOK {
								t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
							}
							assertMCPErrorMessage(t, rec.Body.Bytes(), tc.wantRPCCode, errJSONRPCInvalidParams, tc.wantMessage)
							entries := requestLogEntries(t, logs)
							wantLogMessage := "invalid tools/call params"
							if tc.emptyName {
								if got := calls.Load(); got != 1 || agentCapability != "" {
									t.Fatalf("empty name agent calls=%d capability=%q want 1/empty", got, agentCapability)
								}
								wantLogMessage = "tool is not defined"
							} else if calls.Load() != 0 {
								t.Fatalf("malformed name reached agent: calls=%d", calls.Load())
							}
							if len(entries) != 1 || entries[0]["capability"] != "" || entries[0]["error_code"] != errJSONRPCInvalidParams || entries[0]["error_message"] != wantLogMessage {
								t.Fatalf("tools/call name request log=%#v want message=%q", entries, wantLogMessage)
							}
						} else {
							if rec.Code != http.StatusBadRequest {
								t.Fatalf("idless tools/call status=%d body=%s", rec.Code, rec.Body.String())
							}
							assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayInvalidRequest, "invalid json rpc notification")
							if calls.Load() != 0 || len(requestLogEntries(t, logs)) != 0 {
								t.Fatalf("idless tools/call reached agent/log: calls=%d logs=%#v", calls.Load(), requestLogEntries(t, logs))
							}
						}
					})
				}
			}
		})
	}
}

func TestMCPInitializeReportsInjectedBuildVersion(t *testing.T) {
	original := buildinfo.Version
	buildinfo.Version = "1.2.4"
	t.Cleanup(func() { buildinfo.Version = original })

	s, _, apiKey := testServer(t)
	req := newMCPCompatibilityRequest(http.MethodPost, mcpInitializePayload(1, mcpProtocolVersion20251125))
	rec := serveMCPCompatibility(s, apiKey, req)
	result, ok := decodeMCPCompatibilityResponse(t, rec)["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result=%#v", decodeMCPCompatibilityResponse(t, rec)["result"])
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok || serverInfo["version"] != "1.2.4" {
		t.Fatalf("serverInfo=%#v, want version 1.2.4", result["serverInfo"])
	}
}

func TestMCPProtocolHeaderAndVersionedToolSchemas(t *testing.T) {
	s, _, apiKey := testServer(t)
	s.openapi = mcpCompatibilityOpenAPIDoc()
	for _, tc := range []struct {
		name             string
		header           string
		wantOutputSchema bool
		wantStatus       int
	}{
		{name: "headerless legacy", wantStatus: http.StatusOK},
		{name: "2025-06-18", header: mcpProtocolVersion20250618, wantOutputSchema: true, wantStatus: http.StatusOK},
		{name: "2025-11-25", header: mcpProtocolVersion20251125, wantOutputSchema: true, wantStatus: http.StatusOK},
		{name: "unsupported", header: "2099-01-01", wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			if tc.header != "" {
				req.Header.Set(mcpProtocolHeader, tc.header)
			}
			rec := serveMCPCompatibility(s, apiKey, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !tc.wantOutputSchema {
				if tc.header == "" {
					result := decodeMCPCompatibilityResponse(t, rec)["result"].(map[string]any)
					tool := mcpCompatibilityTool(t, result, "get_customer")
					if _, ok := tool["outputSchema"]; ok {
						t.Fatalf("legacy tool unexpectedly has outputSchema: %#v", tool)
					}
				}
				return
			}
			result := decodeMCPCompatibilityResponse(t, rec)["result"].(map[string]any)
			tool := mcpCompatibilityTool(t, result, "get_customer")
			output, ok := tool["outputSchema"].(map[string]any)
			if !ok || output["type"] != "object" {
				t.Fatalf("outputSchema=%#v, want object schema", tool["outputSchema"])
			}
		})
	}
}

func TestMCPProtocolHeaderPresenceAndMultiplicity(t *testing.T) {
	s, _, apiKey := testServer(t)
	s.openapi = mcpCompatibilityOpenAPIDoc()
	for _, tc := range []struct {
		name             string
		header           http.Header
		wantStatus       int
		wantOutputSchema bool
	}{
		{name: "absent uses legacy", wantStatus: http.StatusOK},
		{name: "explicit legacy", header: http.Header{mcpProtocolHeader: {mcpProtocolVersion20250326}}, wantStatus: http.StatusOK},
		{name: "middle", header: http.Header{mcpProtocolHeader: {mcpProtocolVersion20250618}}, wantStatus: http.StatusOK, wantOutputSchema: true},
		{name: "latest", header: http.Header{mcpProtocolHeader: {mcpProtocolVersion20251125}}, wantStatus: http.StatusOK, wantOutputSchema: true},
		{name: "case insensitive key", header: http.Header{"mcp-protocol-version": {mcpProtocolVersion20251125}}, wantStatus: http.StatusOK, wantOutputSchema: true},
		{name: "unknown", header: http.Header{mcpProtocolHeader: {"2099-01-01"}}, wantStatus: http.StatusBadRequest},
		{name: "present without value", header: http.Header{mcpProtocolHeader: nil}, wantStatus: http.StatusBadRequest},
		{name: "explicit empty", header: http.Header{mcpProtocolHeader: {""}}, wantStatus: http.StatusBadRequest},
		{name: "explicit whitespace", header: http.Header{mcpProtocolHeader: {" \t"}}, wantStatus: http.StatusBadRequest},
		{name: "duplicate same value", header: http.Header{mcpProtocolHeader: {mcpProtocolVersion20251125, mcpProtocolVersion20251125}}, wantStatus: http.StatusBadRequest},
		{name: "comma joined", header: http.Header{mcpProtocolHeader: {mcpProtocolVersion20251125 + ", " + mcpProtocolVersion20251125}}, wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			for name, values := range tc.header {
				req.Header[name] = append([]string(nil), values...)
			}
			rec := serveMCPCompatibility(s, apiKey, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			result, ok := decodeMCPCompatibilityResponse(t, rec)["result"].(map[string]any)
			if !ok {
				t.Fatalf("result=%#v", decodeMCPCompatibilityResponse(t, rec)["result"])
			}
			tool := mcpCompatibilityTool(t, result, "get_customer")
			_, gotOutputSchema := tool["outputSchema"]
			if gotOutputSchema != tc.wantOutputSchema {
				t.Fatalf("outputSchema present=%t want %t; tool=%#v", gotOutputSchema, tc.wantOutputSchema, tool)
			}
		})
	}
}

func TestMCPStructuredContentIsVersionGated(t *testing.T) {
	for _, tc := range []struct {
		name       string
		header     string
		wantStruct bool
	}{
		{name: "legacy", wantStruct: false},
		{name: "middle", header: mcpProtocolVersion20250618, wantStruct: true},
		{name: "latest", header: mcpProtocolVersion20251125, wantStruct: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
				return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[{"id":1,"name":"Ada"}],"count":1}`)}
			})
			defer cleanup()
			req := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`)
			if tc.header != "" {
				req.Header.Set(mcpProtocolHeader, tc.header)
			}
			rec := serveMCPCompatibility(s, apiKey, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			response := decodeMCPToolCallEnvelope(t, rec.Body.Bytes())
			if response.Result.IsError || string(response.ID) != `1` {
				t.Fatalf("MCP tools/call response=%#v, want id 1 successful result", response)
			}
			var textValue any
			decodeMCPJSONUseNumber(t, []byte(response.Result.Content[0].Text), &textValue)
			want := map[string]any{"rows": []any{map[string]any{"id": json.Number("1"), "name": "Ada"}}, "count": json.Number("1")}
			if !reflect.DeepEqual(textValue, want) {
				t.Fatalf("text value=%#v want %#v", textValue, want)
			}
			if tc.wantStruct {
				requireMCPStructuredObject(t, response.Result.StructuredContent)
				var structuredValue any
				decodeMCPJSONUseNumber(t, response.Result.StructuredContent, &structuredValue)
				if !reflect.DeepEqual(structuredValue, want) {
					t.Fatalf("structuredContent=%#v want %#v", structuredValue, want)
				}
			} else if len(response.Result.StructuredContent) != 0 {
				t.Fatalf("legacy structuredContent must be omitted, got %s", response.Result.StructuredContent)
			}
		})
	}
}

func TestMCPExecutionErrorStructuredContentIsVersionGated(t *testing.T) {
	for _, tc := range []struct {
		name       string
		header     string
		wantStruct bool
	}{
		{name: "legacy", wantStruct: false},
		{name: "middle", header: mcpProtocolVersion20250618, wantStruct: true},
		{name: "latest", header: mcpProtocolVersion20251125, wantStruct: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
				return agentResponse{ID: req.ID, Error: &wireError{Code: errAgentQueryFailed, Message: "query failed"}}
			})
			defer cleanup()
			req := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`)
			if tc.header != "" {
				req.Header.Set(mcpProtocolHeader, tc.header)
			}
			rec := serveMCPCompatibility(s, apiKey, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			response := decodeMCPToolCallEnvelope(t, rec.Body.Bytes())
			if !response.Result.IsError || string(response.ID) != `1` {
				t.Fatalf("MCP tools/call response=%#v, want id 1 error result", response)
			}
			if got := response.Result.Content[0].Text; got != "query failed" {
				t.Fatalf("error text=%q want %q", got, "query failed")
			}
			if tc.wantStruct {
				requireMCPStructuredObject(t, response.Result.StructuredContent)
				var structuredValue any
				decodeMCPJSONUseNumber(t, response.Result.StructuredContent, &structuredValue)
				want := map[string]any{"error": map[string]any{"code": errAgentQueryFailed, "message": "query failed"}}
				if !reflect.DeepEqual(structuredValue, want) {
					t.Fatalf("structuredContent=%#v want %#v", structuredValue, want)
				}
			} else if len(response.Result.StructuredContent) != 0 {
				t.Fatalf("legacy structuredContent must be omitted, got %s", response.Result.StructuredContent)
			}
		})
	}
}

func TestMCPDMLPayloadIsVersionGated(t *testing.T) {
	for _, tc := range []struct {
		name       string
		header     string
		wantStruct bool
	}{
		{name: "legacy"},
		{name: "middle", header: mcpProtocolVersion20250618, wantStruct: true},
		{name: "latest", header: mcpProtocolVersion20251125, wantStruct: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
				return agentResponse{ID: req.ID, Result: json.RawMessage(`{"count":2}`)}
			})
			defer cleanup()
			req := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`)
			if tc.header != "" {
				req.Header.Set(mcpProtocolHeader, tc.header)
			}
			rec := serveMCPCompatibility(s, apiKey, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			response := decodeMCPToolCallEnvelope(t, rec.Body.Bytes())
			if response.Result.IsError || string(response.ID) != `1` {
				t.Fatalf("MCP tools/call response=%#v, want id 1 successful result", response)
			}
			var textValue any
			decodeMCPJSONUseNumber(t, []byte(response.Result.Content[0].Text), &textValue)
			want := map[string]any{"count": json.Number("2")}
			if !reflect.DeepEqual(textValue, want) {
				t.Fatalf("text value=%#v want %#v", textValue, want)
			}
			if tc.wantStruct {
				requireMCPStructuredObject(t, response.Result.StructuredContent)
				var structuredValue any
				decodeMCPJSONUseNumber(t, response.Result.StructuredContent, &structuredValue)
				if !reflect.DeepEqual(structuredValue, want) {
					t.Fatalf("structuredContent=%#v want %#v", structuredValue, want)
				}
			} else if len(response.Result.StructuredContent) != 0 {
				t.Fatalf("legacy structuredContent must be omitted, got %s", response.Result.StructuredContent)
			}
		})
	}
}

func TestMCPExecutionErrorClassificationIsVersionGated(t *testing.T) {
	errorCases := []struct {
		name    string
		code    string
		message string
	}{
		{name: "validation", code: errAgentValidationFailed, message: "parameters failed validation"},
		{name: "query", code: errAgentQueryFailed, message: "database query failed"},
		{name: "timeout", code: errAgentQueryTimeout, message: "query exceeded policy.timeout"},
		{name: "database unreachable", code: errAgentDBUnreachable, message: "database unavailable"},
		{name: "internal", code: errAgentInternal, message: "agent internal error"},
		{name: "busy", code: errAgentBusy, message: "agent concurrency limit reached"},
		{name: "constraint", code: errAgentConstraintViolation, message: "database constraint violation"},
		{name: "transaction outcome unknown", code: errAgentTransactionOutcomeUnknown, message: "transaction outcome is unknown"},
	}
	for _, version := range []struct {
		name       string
		header     string
		wantStruct bool
	}{
		{name: "legacy"},
		{name: "middle", header: mcpProtocolVersion20250618, wantStruct: true},
		{name: "latest", header: mcpProtocolVersion20251125, wantStruct: true},
	} {
		version := version
		for _, errorCase := range errorCases {
			errorCase := errorCase
			t.Run(version.name+"/"+errorCase.name, func(t *testing.T) {
				s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
					return agentResponse{ID: req.ID, Error: &wireError{Code: errorCase.code, Message: errorCase.message}}
				})
				defer cleanup()
				req := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`)
				if version.header != "" {
					req.Header.Set(mcpProtocolHeader, version.header)
				}
				rec := serveMCPCompatibility(s, apiKey, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				response := decodeMCPToolCallEnvelope(t, rec.Body.Bytes())
				if !response.Result.IsError || string(response.ID) != `1` {
					t.Fatalf("MCP tools/call response=%#v, want id 1 error result", response)
				}
				if got := response.Result.Content[0].Text; got != errorCase.message {
					t.Fatalf("text=%q want %q", got, errorCase.message)
				}
				if version.wantStruct {
					requireMCPStructuredObject(t, response.Result.StructuredContent)
					var structuredValue any
					decodeMCPJSONUseNumber(t, response.Result.StructuredContent, &structuredValue)
					want := map[string]any{"error": map[string]any{"code": errorCase.code, "message": errorCase.message}}
					if !reflect.DeepEqual(structuredValue, want) {
						t.Fatalf("structuredContent=%#v want %#v", structuredValue, want)
					}
				} else if len(response.Result.StructuredContent) != 0 {
					t.Fatalf("legacy structuredContent must be omitted, got %s", response.Result.StructuredContent)
				}
			})
		}
	}
}

func TestMCPHTTPMediaAndMethodContracts(t *testing.T) {
	s, _, apiKey := testServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := newMCPCompatibilityRequest(method, "")
		rec := serveMCPCompatibility(s, apiKey, req)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s status=%d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	s.cfg.CORSAllowedOrigins = []string{"https://allowed.example"}
	preflight := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	preflight.Header.Set("Origin", "https://allowed.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	preflight.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
	preflightRec := serveMCPCompatibility(s, apiKey, preflight)
	if preflightRec.Code != http.StatusNoContent || preflightRec.Body.Len() != 0 {
		t.Fatalf("allowed MCP preflight status=%d body=%q, want 204/empty", preflightRec.Code, preflightRec.Body.String())
	}
	if got := preflightRec.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example" {
		t.Fatalf("allowed MCP preflight ACAO=%q", got)
	}
	blockedPreflight := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	blockedPreflight.Header.Set("Origin", "https://blocked.example")
	blockedPreflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	blockedRec := serveMCPCompatibility(s, apiKey, blockedPreflight)
	if blockedRec.Code != http.StatusForbidden || blockedRec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("blocked MCP preflight status=%d ACAO=%q body=%s", blockedRec.Code, blockedRec.Header().Get("Access-Control-Allow-Origin"), blockedRec.Body.String())
	}

	for _, accept := range []string{"", "*/*", "application/*", "application/json", "application/json, text/event-stream"} {
		req := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		rec := serveMCPCompatibility(s, apiKey, req)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("Accept %q status=%d content-type=%q body=%s", accept, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
	for _, accept := range []string{"text/event-stream", "text/html", "application/json;q=0, text/event-stream;q=1", "application/json;q=0, */*;q=1"} {
		req := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		req.Header.Set("Accept", accept)
		rec := serveMCPCompatibility(s, apiKey, req)
		if rec.Code != http.StatusNotAcceptable {
			t.Fatalf("Accept %q status=%d body=%s", accept, rec.Code, rec.Body.String())
		}
	}
	for _, contentType := range []string{"", "text/plain", "application/jsonrpc"} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := serveMCPCompatibility(s, apiKey, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("Content-Type %q status=%d body=%s", contentType, rec.Code, rec.Body.String())
		}
	}
	for _, contentType := range []string{"application/json; charset=utf-8", "APPLICATION/JSON"} {
		req := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		req.Header.Set("Content-Type", contentType)
		if rec := serveMCPCompatibility(s, apiKey, req); rec.Code != http.StatusOK {
			t.Fatalf("Content-Type %q status=%d body=%s", contentType, rec.Code, rec.Body.String())
		}
	}
}

func TestMCPOriginBoundaryPrecedesAuthentication(t *testing.T) {
	s, _, apiKey := testServer(t)
	s.cfg.CORSAllowedOrigins = []string{"https://allowed.example"}
	req := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	req.Header.Set("Origin", "https://blocked.example")
	rec := serveMCPCompatibility(s, "", req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("blocked Origin status=%d want 403 body=%s", rec.Code, rec.Body.String())
	}
	assertAPIErrorMessage(t, rec.Body.Bytes(), errGatewayInvalidRequest, "origin is not allowed")

	req = newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if rec = serveMCPCompatibility(s, "", req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing Origin status=%d want authentication status; body=%s", rec.Code, rec.Body.String())
	}

	req = newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	req.Header.Set("Origin", "https://allowed.example")
	rec = serveMCPCompatibility(s, apiKey, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed Origin status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example" {
		t.Fatalf("Access-Control-Allow-Origin=%q want %q", got, "https://allowed.example")
	}
	body := decodeMCPCompatibilityResponse(t, rec)
	if _, present := body["error"]; present {
		t.Fatalf("allowed Origin response contains error=%#v; body=%s", body["error"], rec.Body.String())
	}
	if result, ok := body["result"].(map[string]any); !ok {
		t.Fatalf("allowed Origin response result=%#v want object; body=%s", body["result"], rec.Body.String())
	} else if len(result) != 0 {
		t.Fatalf("allowed Origin ping result=%#v want empty object", result)
	}
}

func TestMCPOnlyOfficialClientNotificationsAreAccepted(t *testing.T) {
	var calls atomic.Int32
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		calls.Add(1)
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	})
	defer cleanup()

	// Fixture mirrored from the official schema ClientNotification union for
	// each supported protocol version. Task status was added in 2025-11-25.
	officialFixture := map[string][]string{
		mcpProtocolVersion20250326: {
			"notifications/cancelled",
			"notifications/initialized",
			"notifications/progress",
			"notifications/roots/list_changed",
		},
		mcpProtocolVersion20250618: {
			"notifications/cancelled",
			"notifications/initialized",
			"notifications/progress",
			"notifications/roots/list_changed",
		},
		mcpProtocolVersion20251125: {
			"notifications/cancelled",
			"notifications/initialized",
			"notifications/progress",
			"notifications/roots/list_changed",
			"notifications/tasks/status",
		},
	}
	for version, methods := range officialFixture {
		allowed := mcpClientNotificationMethodsByVersion[version]
		if len(allowed) != len(methods) {
			t.Fatalf("version=%s notification allow-list=%v, want exactly %v", version, allowed, methods)
		}
		for _, method := range methods {
			if _, ok := allowed[method]; !ok {
				t.Fatalf("version=%s official notification %q is missing from allow-list", version, method)
			}
			req := newMCPCompatibilityRequest(http.MethodPost, mcpNotificationPayload(method))
			if version != mcpProtocolVersion20250326 {
				req.Header.Set(mcpProtocolHeader, version)
			}
			rec := serveMCPCompatibility(s, apiKey, req)
			if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
				t.Fatalf("version=%s notification %q status=%d body=%q", version, method, rec.Code, rec.Body.String())
			}
		}
	}
	for _, version := range []string{mcpProtocolVersion20250326, mcpProtocolVersion20250618} {
		req := newMCPCompatibilityRequest(http.MethodPost, mcpNotificationPayload("notifications/tasks/status"))
		if version != mcpProtocolVersion20250326 {
			req.Header.Set(mcpProtocolHeader, version)
		}
		rec := serveMCPCompatibility(s, apiKey, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("version=%s task notification status=%d body=%s, want HTTP 400", version, rec.Code, rec.Body.String())
		}
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"ping"}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"get_customer","arguments":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/message"}`,
	} {
		rec := serveMCPCompatibility(s, apiKey, newMCPCompatibilityRequest(http.MethodPost, body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid notification status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("agent calls=%d want 0 for notifications", got)
	}
}

func mcpNotificationPayload(method string) string {
	switch method {
	case "notifications/cancelled":
		return `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"init"}}`
	case "notifications/progress":
		return `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"init","progress":1}}`
	case "notifications/tasks/status":
		return `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"task-1","status":"working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:00Z","ttl":null}}`
	default:
		return fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":{}}`, method)
	}
}

func TestMCPTaskStatusNotificationVersionedParamsAndIDs(t *testing.T) {
	var calls atomic.Int32
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		calls.Add(1)
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	})
	defer cleanup()

	validParams := `{"taskId":"task-1","status":"working","statusMessage":"still working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":null,"pollInterval":1000,"_meta":{"progressToken":[],"opaque":{"nested":true}}}`
	invalidParams := []struct {
		name   string
		params string
	}{
		{name: "missing taskId", params: `{"status":"working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":null}`},
		{name: "taskId type", params: `{"taskId":1,"status":"working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":null}`},
		{name: "status enum", params: `{"taskId":"task-1","status":"paused","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":null}`},
		{name: "status type", params: `{"taskId":"task-1","status":1,"createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":null}`},
		{name: "createdAt type", params: `{"taskId":"task-1","status":"working","createdAt":null,"lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":null}`},
		{name: "lastUpdatedAt missing", params: `{"taskId":"task-1","status":"working","createdAt":"2025-11-25T00:00:00Z","ttl":null}`},
		{name: "lastUpdatedAt type", params: `{"taskId":"task-1","status":"working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":null,"ttl":null}`},
		{name: "ttl missing", params: `{"taskId":"task-1","status":"working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z"}`},
		{name: "ttl type", params: `{"taskId":"task-1","status":"working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":"forever"}`},
		{name: "ttl non-integer", params: `{"taskId":"task-1","status":"working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":1.5}`},
		{name: "statusMessage type", params: `{"taskId":"task-1","status":"working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":null,"statusMessage":false}`},
		{name: "pollInterval type", params: `{"taskId":"task-1","status":"working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":null,"pollInterval":1.5}`},
		{name: "meta type", params: `{"taskId":"task-1","status":"working","createdAt":"2025-11-25T00:00:00Z","lastUpdatedAt":"2025-11-25T00:00:01Z","ttl":null,"_meta":null}`},
		{name: "params null", params: `null`},
		{name: "params array", params: `[]`},
		{name: "params scalar", params: `"params"`},
	}

	versions := []struct {
		name, header string
		latest       bool
	}{
		{name: mcpProtocolVersion20250326},
		{name: mcpProtocolVersion20250618, header: mcpProtocolVersion20250618},
		{name: mcpProtocolVersion20251125, header: mcpProtocolVersion20251125, latest: true},
	}
	for _, version := range versions {
		version := version
		t.Run(version.name, func(t *testing.T) {
			post := func(params string, withID bool) *httptest.ResponseRecorder {
				id := ""
				if withID {
					id = `,"id":99`
				}
				req := newMCPCompatibilityRequest(http.MethodPost, fmt.Sprintf(`{"jsonrpc":"2.0"%s,"method":"notifications/tasks/status","params":%s}`, id, params))
				if version.header != "" {
					req.Header.Set(mcpProtocolHeader, version.header)
				}
				return serveMCPCompatibility(s, apiKey, req)
			}

			rec := post(validParams, false)
			if version.latest {
				if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
					t.Fatalf("valid task status notification status=%d body=%q", rec.Code, rec.Body.String())
				}
			} else if rec.Code != http.StatusBadRequest {
				t.Fatalf("unsupported version task status notification status=%d body=%s, want HTTP 400", rec.Code, rec.Body.String())
			}

			for _, invalid := range invalidParams {
				t.Run("invalid/"+invalid.name, func(t *testing.T) {
					rec := post(invalid.params, false)
					if version.latest {
						if rec.Code != http.StatusBadRequest {
							t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
						}
						assertMCPErrorMessage(t, rec.Body.Bytes(), -32602, errJSONRPCInvalidParams, "invalid notification params")
						assertMCPErrorIDNull(t, rec.Body.Bytes())
					} else if rec.Code != http.StatusBadRequest {
						t.Fatalf("unsupported version status=%d body=%s, want HTTP 400", rec.Code, rec.Body.String())
					}
				})
			}

			for _, params := range append([]string{validParams}, func() []string {
				out := make([]string, 0, len(invalidParams))
				for _, invalid := range invalidParams {
					out = append(out, invalid.params)
				}
				return out
			}()...) {
				rec := post(params, true)
				if rec.Code != http.StatusOK {
					t.Fatalf("id-bearing task status notification status=%d body=%s", rec.Code, rec.Body.String())
				}
				assertMCPErrorMessage(t, rec.Body.Bytes(), -32600, errJSONRPCInvalidRequest, "notification must not include id")
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("agent calls=%d want 0 for task status notifications", got)
	}
}

func TestMCPNotificationsValidateVersionedParamsAndIDs(t *testing.T) {
	var calls atomic.Int32
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		calls.Add(1)
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	})
	defer cleanup()

	versions := []struct {
		name, header string
	}{
		{name: "2025-03-26"},
		{name: "2025-06-18", header: mcpProtocolVersion20250618},
		{name: "2025-11-25", header: mcpProtocolVersion20251125},
	}
	notifications := []struct {
		method        string
		validParams   string
		paramsOmitted bool
		invalidParams []string
	}{
		{method: "notifications/cancelled", validParams: `{"requestId":42,"reason":"done","_meta":{"progressToken":null,"array":[],"object":{"nested":true},"scalar":"opaque"}}`, invalidParams: []string{`{"reason":"done"}`, `{"requestId":null}`, `{"requestId":42,"_meta":null}`, `{"requestId":42,"_meta":[]}`, `{"requestId":42,"_meta":"opaque"}`}},
		{method: "notifications/initialized", validParams: `{"_meta":{"progressToken":[],"array":[null],"object":{"nested":true},"scalar":false}}`, paramsOmitted: true, invalidParams: []string{`null`, `{"_meta":null}`, `{"_meta":[]}`, `{"_meta":"opaque"}`}},
		{method: "notifications/progress", validParams: `{"progressToken":"p","progress":1.5,"message":"half","total":3,"_meta":{"progressToken":{"opaque":true},"array":[],"object":null}}`, invalidParams: []string{`{"progress":1}`, `{"progressToken":"p","progress":"1"}`, `{"progressToken":"p","progress":1,"_meta":null}`, `{"progressToken":"p","progress":1,"_meta":[]}`, `{"progressToken":"p","progress":1,"_meta":"opaque"}`}},
		{method: "notifications/roots/list_changed", validParams: `{"_meta":{"progressToken":null,"array":{},"object":[],"scalar":7}}`, paramsOmitted: true, invalidParams: []string{`[]`, `{"_meta":null}`, `{"_meta":[]}`, `{"_meta":"opaque"}`}},
	}
	type notificationCase struct {
		name        string
		params      string
		omitParams  bool
		withID      bool
		wantCode    int
		wantRPCCode int
		wantMessage string
	}
	for _, version := range versions {
		version := version
		t.Run(version.name, func(t *testing.T) {
			for _, notification := range notifications {
				notification := notification
				t.Run(notification.method, func(t *testing.T) {
					cases := []notificationCase{
						{name: "valid notification", params: notification.validParams, wantCode: http.StatusAccepted},
						{name: "valid id-bearing", params: notification.validParams, withID: true, wantCode: http.StatusOK, wantRPCCode: -32600, wantMessage: "notification must not include id"},
					}
					if notification.paramsOmitted {
						cases = append(cases, notificationCase{name: "valid notification params omitted", omitParams: true, wantCode: http.StatusAccepted})
						cases = append(cases, notificationCase{name: "id-bearing params omitted", omitParams: true, withID: true, wantCode: http.StatusOK, wantRPCCode: -32600, wantMessage: "notification must not include id"})
					}
					for invalidIndex, invalidParams := range notification.invalidParams {
						cases = append(cases,
							notificationCase{name: fmt.Sprintf("invalid notification %d", invalidIndex), params: invalidParams, wantCode: http.StatusBadRequest, wantRPCCode: -32602, wantMessage: "invalid notification params"},
							notificationCase{name: fmt.Sprintf("invalid id-bearing %d", invalidIndex), params: invalidParams, withID: true, wantCode: http.StatusOK, wantRPCCode: -32600, wantMessage: "notification must not include id"},
						)
					}
					for _, tc := range cases {
						t.Run(tc.name, func(t *testing.T) {
							id := ""
							if tc.withID {
								id = `,"id":"notification-id"`
							}
							params := ""
							if !tc.omitParams {
								params = fmt.Sprintf(`,"params":%s`, tc.params)
							}
							body := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q%s%s}`, notification.method, id, params)
							req := newMCPCompatibilityRequest(http.MethodPost, body)
							if version.header != "" {
								req.Header.Set(mcpProtocolHeader, version.header)
							}
							rec := serveMCPCompatibility(s, apiKey, req)
							if rec.Code != tc.wantCode || (tc.wantCode == http.StatusAccepted && rec.Body.Len() != 0) {
								t.Fatalf("status=%d body=%q want status=%d", rec.Code, rec.Body.String(), tc.wantCode)
							}
							if tc.wantRPCCode != 0 {
								assertMCPErrorMessage(t, rec.Body.Bytes(), tc.wantRPCCode, map[int]string{-32600: errJSONRPCInvalidRequest, -32602: errJSONRPCInvalidParams}[tc.wantRPCCode], tc.wantMessage)
								if tc.wantRPCCode == -32602 {
									assertMCPErrorIDNull(t, rec.Body.Bytes())
								}
							}
						})
					}
				})
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("agent calls=%d want 0 for notification validation failures", got)
	}
}

func TestMCPToolCallArgumentsMustBeObjectWhenPresent(t *testing.T) {
	var calls atomic.Int32
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		calls.Add(1)
		if req.Params == nil {
			t.Errorf("agent params=nil, want empty object for omitted arguments")
		}
		if len(req.Params) != 0 {
			t.Errorf("agent params=%#v, want empty object", req.Params)
		}
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	})
	defer cleanup()

	rec := serveMCPCompatibility(s, apiKey, newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer"}}`))
	if rec.Code != http.StatusOK || decodeMCPCompatibilityResponse(t, rec)["result"] == nil {
		t.Fatalf("omitted arguments status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, raw := range []string{"null", "[]", `"value"`, "1", "true"} {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_customer","arguments":%s}}`, raw)
		rec := serveMCPCompatibility(s, apiKey, newMCPCompatibilityRequest(http.MethodPost, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("arguments=%s status=%d body=%s", raw, rec.Code, rec.Body.String())
		}
		assertMCPErrorMessage(t, rec.Body.Bytes(), -32602, errJSONRPCInvalidParams, "invalid tools/call params")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("agent calls=%d want 1 for omitted arguments only", got)
	}
}

func TestMCPIDTypesRejectInvalidIDsWithoutCallingAgent(t *testing.T) {
	var calls atomic.Int32
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		calls.Add(1)
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	})
	defer cleanup()
	for _, id := range []string{"null", "true", `{}`, `[]`} {
		req := newMCPCompatibilityRequest(http.MethodPost, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"get_customer","arguments":{}}}`, id))
		rec := serveMCPCompatibility(s, apiKey, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("id %s status=%d body=%s", id, rec.Code, rec.Body.String())
		}
		assertMCPErrorMessage(t, rec.Body.Bytes(), -32600, errJSONRPCInvalidRequest, "invalid json rpc request")
		body := decodeMCPCompatibilityResponse(t, rec)
		if body["id"] != nil {
			t.Fatalf("id %s response id=%#v want null", id, body["id"])
		}
	}
	for _, id := range []string{"\"string-id\"", "42"} {
		req := newMCPCompatibilityRequest(http.MethodPost, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"ping"}`, id))
		rec := serveMCPCompatibility(s, apiKey, req)
		if rec.Code != http.StatusOK || decodeMCPCompatibilityResponse(t, rec)["result"] == nil {
			t.Fatalf("valid id %s status=%d body=%s", id, rec.Code, rec.Body.String())
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("agent calls=%d want 0 for invalid IDs", got)
	}
}

func TestMCPToolsExposeOnlyExplicitCapabilityAnnotationHints(t *testing.T) {
	doc := testOpenAPIDoc()
	paths := doc["paths"].(map[string]any)
	post := paths["/api/v1/capabilities/get_customer"].(map[string]any)["post"].(map[string]any)
	post["x-onprest-annotations"] = map[string]any{
		"read_only":   true,
		"destructive": false,
		"idempotent":  true,
		"open_world":  false,
	}
	post["x-onprest-examples"] = []any{map[string]any{"params": map[string]any{"customer_id": 123}}}
	paths["/api/v1/capabilities/partial_hints"] = map[string]any{
		"post": map[string]any{
			"x-onprest-capability": "partial_hints",
			"description":          "Partial hints",
			"x-onprest-annotations": map[string]any{
				"read_only":  false,
				"open_world": "not-a-bool",
				"unknown":    true,
			},
		},
	}
	paths["/api/v1/capabilities/invalid_hints"] = map[string]any{
		"post": map[string]any{
			"x-onprest-capability": "invalid_hints",
			"description":          "Invalid hints",
			"x-onprest-annotations": map[string]any{
				"open_world": "not-a-bool",
				"unknown":    true,
			},
		},
	}
	s, _, apiKey := testServer(t)
	s.cfg.APIKeys[0].Capabilities = capabilities{"*"}
	s.openapi = doc
	for _, version := range []string{mcpProtocolVersion20250326, mcpProtocolVersion20250618, mcpProtocolVersion20251125} {
		t.Run(version, func(t *testing.T) {
			req := newMCPCompatibilityRequest(http.MethodPost, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/list","params":{}}`, version))
			req.Header.Set(mcpProtocolHeader, version)
			rec := serveMCPCompatibility(s, apiKey, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("tools/list status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := decodeMCPCompatibilityResponse(t, rec)
			result, ok := body["result"].(map[string]any)
			if !ok {
				t.Fatalf("tools/list result=%#v", body)
			}
			tool := mcpCompatibilityTool(t, result, "get_customer")
			hints, ok := tool["annotations"].(map[string]any)
			if !ok {
				t.Fatalf("tool annotations=%#v", tool["annotations"])
			}
			if hints["readOnlyHint"] != true || hints["destructiveHint"] != false || hints["idempotentHint"] != true || hints["openWorldHint"] != false {
				t.Fatalf("mapped hints=%#v", hints)
			}
			if _, ok := tool["examples"]; ok {
				t.Fatalf("OpenAPI examples were copied into MCP tool: %#v", tool)
			}
			if _, ok := tool["x-onprest-examples"]; ok {
				t.Fatalf("OpenAPI extension examples were copied into MCP tool: %#v", tool)
			}

			partial := mcpCompatibilityTool(t, result, "partial_hints")
			partialHints, ok := partial["annotations"].(map[string]any)
			if !ok || partialHints["readOnlyHint"] != false {
				t.Fatalf("partial explicit-false hint=%#v", partial["annotations"])
			}
			for _, omitted := range []string{"destructiveHint", "idempotentHint", "openWorldHint", "unknown"} {
				if _, ok := partialHints[omitted]; ok {
					t.Fatalf("partial hint %q was inferred or accepted: %#v", omitted, partialHints)
				}
			}
			invalid := mcpCompatibilityTool(t, result, "invalid_hints")
			if _, ok := invalid["annotations"]; ok {
				t.Fatalf("all-invalid annotation extension was emitted: %#v", invalid)
			}

			search := mcpCompatibilityTool(t, result, "search_orders")
			if _, ok := search["annotations"]; ok {
				t.Fatalf("unannotated tool received inferred annotations: %#v", search)
			}
		})
	}
}

func TestMCPAnnotationsDoNotChangeAuthorizationOrExecution(t *testing.T) {
	var calls atomic.Int32
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		calls.Add(1)
		if req.Capability != "get_customer" || req.Params["id"] != nil {
			t.Fatalf("agent request=%#v, want authorized capability with empty params", req)
		}
		return agentResponse{ID: req.ID, Result: json.RawMessage("{\"rows\":[],\"count\":0}")}
	})
	defer cleanup()
	doc := mcpCompatibilityOpenAPIDoc()
	post := doc["paths"].(map[string]any)["/api/v1/capabilities/get_customer"].(map[string]any)["post"].(map[string]any)
	post["x-onprest-annotations"] = map[string]any{"destructive": false, "read_only": true}
	s.openapi = doc
	req := newMCPCompatibilityRequest(http.MethodPost, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"get_customer\",\"arguments\":{}}}")
	rec := serveMCPCompatibility(s, apiKey, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	response := decodeMCPToolCallEnvelope(t, rec.Body.Bytes())
	if response.Result.IsError || calls.Load() != 1 {
		t.Fatalf("response=%#v calls=%d, want successful authorized execution", response, calls.Load())
	}
}

func TestMCPToolAnnotationAndExampleMetadataStayWithinAPIKeyFilter(t *testing.T) {
	s, _, apiKey := testServer(t)
	doc := testOpenAPIDoc()
	paths := doc["paths"].(map[string]any)
	paths["/api/v1/capabilities/hidden"] = map[string]any{"post": map[string]any{
		"x-onprest-capability":  "hidden",
		"description":           "PRIVATE_DESCRIPTION_SENTINEL",
		"x-onprest-annotations": map[string]any{"read_only": false},
		"x-onprest-examples":    []any{map[string]any{"params": map[string]any{"secret": "PRIVATE_EXAMPLE_SENTINEL"}}},
	}}
	s.openapi = s.finalizeOpenAPI(doc)

	openAPIRequest := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	openAPIRequest.Header.Set("Authorization", "Bearer "+apiKey)
	openAPIResponse := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(openAPIResponse, openAPIRequest)
	if openAPIResponse.Code != http.StatusOK {
		t.Fatalf("filtered OpenAPI status=%d body=%s", openAPIResponse.Code, openAPIResponse.Body.String())
	}
	var filtered map[string]any
	if err := json.Unmarshal(openAPIResponse.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if _, ok := filtered["paths"].(map[string]any)["/api/v1/capabilities/hidden"]; ok {
		t.Fatalf("hidden path survived OpenAPI API-key filter: %#v", filtered["paths"])
	}
	raw, err := json.Marshal(filtered)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE_DESCRIPTION_SENTINEL") || strings.Contains(string(raw), "PRIVATE_EXAMPLE_SENTINEL") {
		t.Fatalf("hidden metadata leaked after OpenAPI API-key filter: %s", raw)
	}

	mcpRequest := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	mcpRequest.Header.Set(mcpProtocolHeader, mcpProtocolVersion20251125)
	mcpResponse := serveMCPCompatibility(s, apiKey, mcpRequest)
	if mcpResponse.Code != http.StatusOK {
		t.Fatalf("filtered tools/list status=%d body=%s", mcpResponse.Code, mcpResponse.Body.String())
	}
	mcpBody := decodeMCPCompatibilityResponse(t, mcpResponse)
	tools := mcpBody["result"].(map[string]any)["tools"].([]any)
	for _, value := range tools {
		if tool, ok := value.(map[string]any); ok && tool["name"] == "hidden" {
			t.Fatalf("hidden tool survived MCP API-key filter: %#v", tools)
		}
	}
	if strings.Contains(mcpResponse.Body.String(), "PRIVATE_DESCRIPTION_SENTINEL") || strings.Contains(mcpResponse.Body.String(), "PRIVATE_EXAMPLE_SENTINEL") {
		t.Fatalf("hidden metadata leaked after MCP API-key filter: %s", mcpResponse.Body.String())
	}
}

func mcpCompatibilityOpenAPIDoc() map[string]any {
	doc := testOpenAPIDoc()
	paths := doc["paths"].(map[string]any)
	getCustomer := paths["/api/v1/capabilities/get_customer"].(map[string]any)
	post := getCustomer["post"].(map[string]any)
	post["responses"] = map[string]any{
		"200": map[string]any{
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": map[string]any{"type": "object", "properties": map[string]any{"rows": map[string]any{"type": "array"}, "count": map[string]any{"type": "integer"}}},
				},
			},
		},
	}
	return doc
}

func TestMCPCompatibilityHelpersDoNotMutateOriginalOpenAPI(t *testing.T) {
	doc := mcpCompatibilityOpenAPIDoc()
	legacy := toolsFromOpenAPI(doc, mcpProtocolVersion20250326)
	modern := toolsFromOpenAPI(doc, mcpProtocolVersion20251125)
	legacyTool := mcpCompatibilityTool(t, map[string]any{"tools": func() []any {
		out := make([]any, len(legacy))
		for i := range legacy {
			out[i] = legacy[i]
		}
		return out
	}()}, "get_customer")
	if _, ok := legacyTool["outputSchema"]; ok {
		t.Fatalf("legacy output schema unexpectedly present: %#v", legacyTool)
	}
	modernTool := mcpCompatibilityTool(t, map[string]any{"tools": func() []any {
		out := make([]any, len(modern))
		for i := range modern {
			out[i] = modern[i]
		}
		return out
	}()}, "get_customer")
	if _, ok := modernTool["outputSchema"]; !ok {
		t.Fatalf("modern output schema missing: %#v", modernTool)
	}
	if strings.Contains(string(bytes.TrimSpace(mustJSON(t, doc))), `"outputSchema"`) {
		t.Fatal("OpenAPI document was mutated with MCP-only outputSchema")
	}
}

type mcpCompatibilityAuthTransport struct {
	base   http.RoundTripper
	apiKey string
}

func (t mcpCompatibilityAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.apiKey)
	return t.base.RoundTrip(clone)
}

type mcpRecordedRequest struct {
	Method string
	Header http.Header
	Body   []byte
}

type mcpRecordingRoundTripper struct {
	base http.RoundTripper
	mu   sync.Mutex
	seen []mcpRecordedRequest
}

func (t *mcpRecordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	t.mu.Lock()
	t.seen = append(t.seen, mcpRecordedRequest{
		Method: req.Method,
		Header: req.Header.Clone(),
		Body:   append([]byte(nil), body...),
	})
	t.mu.Unlock()
	return t.base.RoundTrip(req)
}

func (t *mcpRecordingRoundTripper) requests() []mcpRecordedRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]mcpRecordedRequest(nil), t.seen...)
}

func TestMCPOfficialGoSDKStreamableHTTPClient(t *testing.T) {
	s, _, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"rows":[],"count":0}`)}
	})
	defer cleanup()
	s.openapi = mcpCompatibilityOpenAPIDoc()

	httpServer := httptest.NewServer(s.httpSrv.Handler)
	defer httpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "onprest-compatibility-test", Version: "go-sdk-v1.8.0-pre.2"}, nil)
	recorder := &mcpRecordingRoundTripper{base: http.DefaultTransport}
	transport := &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: mcpCompatibilityAuthTransport{base: recorder, apiKey: apiKey}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, &mcp.ClientSessionOptions{ProtocolVersion: mcpProtocolVersion20251125})
	if err != nil {
		t.Fatalf("official go-sdk Connect: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = session.Close()
		}
	}()
	if got := session.InitializeResult().ProtocolVersion; got != mcpProtocolVersion20251125 {
		t.Fatalf("official client negotiated protocol=%q want %q", got, mcpProtocolVersion20251125)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("official go-sdk ListTools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "get_customer" || tools.Tools[0].OutputSchema == nil {
		t.Fatalf("official go-sdk tools=%#v, want get_customer with output schema", tools.Tools)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_customer", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("official go-sdk CallTool: %v", err)
	}
	if result.IsError || len(result.Content) != 1 || result.StructuredContent == nil {
		t.Fatalf("official go-sdk tool result=%#v, want text and structured content", result)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("official go-sdk Close: %v", err)
	}
	closed = true
	recorded := recorder.requests()
	if len(recorded) < 3 {
		t.Fatalf("official go-sdk recorded %d HTTP requests, want initialize/list/call", len(recorded))
	}
	for i, request := range recorded {
		if request.Method != http.MethodPost {
			t.Fatalf("official go-sdk request %d method=%q, want POST", i, request.Method)
		}
		if sessionID := request.Header.Get("Mcp-Session-Id"); sessionID != "" {
			t.Fatalf("official go-sdk request %d sent Mcp-Session-Id=%q", i, sessionID)
		}
	}
	var initialize struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
	}
	if err := json.Unmarshal(recorded[0].Body, &initialize); err != nil || initialize.JSONRPC != "2.0" || initialize.Method != "initialize" {
		t.Fatalf("official go-sdk first request=%s, want initialize JSON-RPC", recorded[0].Body)
	}
	if got := recorded[0].Header.Get(mcpProtocolHeader); got != "" {
		t.Fatalf("official go-sdk initialize protocol header=%q, want absent", got)
	}
	for i, request := range recorded[1:] {
		if got := request.Header.Get(mcpProtocolHeader); got != mcpProtocolVersion20251125 {
			t.Fatalf("official go-sdk request %d protocol header=%q, want %q", i+1, got, mcpProtocolVersion20251125)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
