//go:build integration

package it

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	agentpkg "github.com/viewlegacy/onprest/internal/agent"
)

func TestContainerDBDriverPostgresNullableResultMatchesGeneratedOpenAPI(t *testing.T) {
	if !selectedDBForTest(t, "postgres") {
		return
	}
	cfg := selectedContainerDBConfig(t, "postgres")
	seedCustomerTable(t, "postgres", cfg)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	capabilityFile := writePostgresCapability(t, t.TempDir(), cfg, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, `  nullable_result:
    sql: |
      select text_value, integer_value, number_value, boolean_value
      from (values
        (null::text, null::bigint, null::double precision, null::boolean, 1),
        ('value'::text, 42::bigint, 1.5::double precision, true, 2)
      ) as nullable_values(text_value, integer_value, number_value, boolean_value, ordinal)
      where :marker::text = 'run'
      order by ordinal
    params:
      marker:
        type: string
        required: true
    policy:
      readonly: true
      timeout: 2s
      max_rows: 10
      max_bytes: 128KB
    result:
      text_value: {type: string}
      integer_value: {type: integer}
      number_value: {type: number}
      boolean_value: {type: boolean}
  mutation_count:
    sql: update onprest_it_customers set name = name where id = -1
    policy:
      readonly: false
      timeout: 2s
      max_bytes: 128KB`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, 3*time.Second)
	runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

	openAPIBody := getWithAPIKey(t, baseURL+"/openapi.json", secrets.APIKey)
	document, documentErrors := libopenapi.NewDocument(openAPIBody)
	if documentErrors != nil {
		t.Fatalf("parse generated OpenAPI 3.1 document: %v", documentErrors)
	}
	docValidator, validatorErrors := validator.NewValidator(document)
	if len(validatorErrors) > 0 {
		t.Fatalf("create OpenAPI 3.1 validator: %v", validatorErrors)
	}
	if valid, validationErrors := docValidator.ValidateDocument(); !valid {
		t.Fatalf("generated OpenAPI 3.1 document is invalid: %v", validationErrors)
	}

	status, selectBody := postCapability(t, baseURL, secrets.APIKey, "nullable_result", `{"marker":"run"}`)
	if status != http.StatusOK {
		t.Fatalf("nullable_result status=%d body=%s", status, selectBody)
	}
	var selectResponse struct {
		Rows  []map[string]any `json:"rows"`
		Count int64            `json:"count"`
	}
	if err := json.Unmarshal(selectBody, &selectResponse); err != nil {
		t.Fatal(err)
	}
	if selectResponse.Count != 2 || len(selectResponse.Rows) != 2 {
		t.Fatalf("actual Agent SELECT response=%s", selectBody)
	}
	for _, name := range []string{"text_value", "integer_value", "number_value", "boolean_value"} {
		if value, exists := selectResponse.Rows[0][name]; !exists || value != nil {
			t.Fatalf("NULL row %s exists=%t value=%#v", name, exists, value)
		}
		if value, exists := selectResponse.Rows[1][name]; !exists || value == nil {
			t.Fatalf("non-NULL row %s exists=%t value=%#v", name, exists, value)
		}
	}
	validateOpenAPIResponse(t, docValidator, baseURL, "nullable_result", selectBody, true)

	missingRequired := cloneJSONResponse(t, selectBody)
	delete(missingRequired["rows"].([]any)[0].(map[string]any), "boolean_value")
	validateOpenAPIResponse(t, docValidator, baseURL, "nullable_result", mustMarshalJSON(t, missingRequired), false)
	additionalProperty := cloneJSONResponse(t, selectBody)
	additionalProperty["rows"].([]any)[0].(map[string]any)["unexpected"] = "must be rejected"
	validateOpenAPIResponse(t, docValidator, baseURL, "nullable_result", mustMarshalJSON(t, additionalProperty), false)

	status, mutationBody := postCapability(t, baseURL, secrets.APIKey, "mutation_count", `{}`)
	if status != http.StatusOK || string(mutationBody) != `{"count":0}` {
		t.Fatalf("actual Agent mutation response status=%d body=%s", status, mutationBody)
	}
	validateOpenAPIResponse(t, docValidator, baseURL, "mutation_count", mutationBody, true)
	validateOpenAPIResponse(t, docValidator, baseURL, "mutation_count", []byte(`{"count":null}`), false)

	assertRequestAndMCPInputSchemasStayNonNullable(t, openAPIBody, postMCPPayload(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))

	cancel()
	select {
	case <-errCh:
	case <-time.After(12 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}

func validateOpenAPIResponse(t *testing.T, docValidator validator.Validator, baseURL, capability string, body []byte, wantValid bool) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/capabilities/"+capability, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "application/json")
	recorder.WriteHeader(http.StatusOK)
	_, _ = recorder.Write(body)
	valid, validationErrors := docValidator.ValidateHttpResponse(request, recorder.Result())
	if valid != wantValid {
		t.Fatalf("OpenAPI response valid=%t want=%t body=%s errors=%v", valid, wantValid, body, validationErrors)
	}
}

func cloneJSONResponse(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func mustMarshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertRequestAndMCPInputSchemasStayNonNullable(t *testing.T, openAPIBody, toolsBody []byte) {
	t.Helper()
	var openAPI map[string]any
	if err := json.Unmarshal(openAPIBody, &openAPI); err != nil {
		t.Fatal(err)
	}
	requestSchema := openAPI["paths"].(map[string]any)["/api/v1/capabilities/nullable_result"].(map[string]any)["post"].(map[string]any)["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	marker := requestSchema["properties"].(map[string]any)["marker"].(map[string]any)
	if marker["type"] != "string" || requestSchema["additionalProperties"] != false || fmt.Sprint(requestSchema["required"]) != "[marker]" {
		t.Fatalf("request schema changed by response nullability: %#v", requestSchema)
	}

	var tools struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(toolsBody, &tools); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Result.Tools {
		if tool.Name == "nullable_result" {
			if !reflect.DeepEqual(tool.InputSchema, requestSchema) {
				t.Fatalf("MCP input schema=%#v, OpenAPI request schema=%#v", tool.InputSchema, requestSchema)
			}
			return
		}
	}
	t.Fatalf("nullable_result MCP tool missing: %s", toolsBody)
}
