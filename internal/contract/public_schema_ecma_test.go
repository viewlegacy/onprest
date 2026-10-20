//go:build schema_validator

package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
)

type standardCase struct {
	Name   string         `json:"-"`
	Schema map[string]any `json:"schema"`
	Value  any            `json:"value"`
	Valid  bool           `json:"-"`
}

func validateECMACases(t *testing.T, cases []standardCase) {
	t.Helper()
	body, e := json.Marshal(cases)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "testdata/schema-validator/validate.mjs")
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, e := cmd.Output()
	if e != nil {
		t.Fatalf("fixed Ajv validator failed (run make test-contract-schema): %v %s", e, stderr.String())
	}
	results := []struct {
		Valid  bool `json:"valid"`
		Errors any  `json:"errors"`
	}{}
	if e := json.Unmarshal(raw, &results); e != nil {
		t.Fatal(e)
	}
	if len(results) != len(cases) {
		t.Fatal("validator dropped cases")
	}
	for i, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if results[i].Valid != c.Valid {
				t.Fatalf("standard valid=%v Agent valid=%v errors=%#v schema=%s value=%s", results[i].Valid, c.Valid, results[i].Errors, canonical(t, c.Schema), canonical(t, c.Value))
			}
		})
	}
}
func TestPublicPatternGoECMADifferential(t *testing.T) {
	cases := []standardCase{}
	for i, fixture := range commonPatternCases() {
		p, e := CompilePattern(fixture.pattern)
		if e != nil {
			t.Fatal(e)
		}
		schema := map[string]any{"type": "string", "pattern": p.ECMAExpression()}
		for j, v := range append(append([]string{}, fixture.yes...), fixture.no...) {
			want := j < len(fixture.yes)
			if p.MatchString(v) != want {
				t.Fatal("Agent fixture no longer valid")
			}
			cases = append(cases, standardCase{fmt.Sprintf("pattern_%d_input_%d", i, j), schema, v, want})
		}
	}
	validateECMACases(t, cases)
}
func TestPublicOutputStandardValidatorMatchesProjectedValues(t *testing.T) {
	cases := []standardCase{}
	for _, tc := range []struct {
		name, output string
		values       []any
	}{
		{"nullable_enum", "properties: {a: {type: integer, enum: [1]}}\ndata: {}", []any{map[string]any{"a": nil}, map[string]any{"a": int64(1)}, map[string]any{}, map[string]any{"a": int64(2)}}},
		{"nullable_const", "properties: {a: {type: string, const: fixed}}\ndata: {}", []any{map[string]any{"a": nil}, map[string]any{"a": "fixed"}, map[string]any{"a": "bad"}}},
		{"nonnull", "properties: {a: {type: integer, nullable: false}}\nrequired: [a]\ndata: {}", []any{map[string]any{"a": nil}, map[string]any{}, map[string]any{"a": int64(1)}}},
		{"untyped_nonnull", "nullable: false\ndata: {}", []any{nil, "text", []any{}, map[string]any{}}},
		{"passthrough", "mode: passthrough\nproperties: {optional: {type: integer}, nested: {properties: {a: {type: boolean}}}}\ndata: {}", []any{map[string]any{"unknown": true}, map[string]any{"nested": map[string]any{"a": true, "unknown": "kept"}}, map[string]any{"optional": "bad"}}},
		{"dictionary", "type: object\nadditionalProperties: {type: integer, nullable: false}\ndata: {}", []any{map[string]any{"a": int64(1)}, map[string]any{"a": nil}}},
		{"scalar", "type: integer\ndata: {}", []any{int64(42), nil, "wrong"}},
		{"nested_arrays", "items: {items: {type: boolean}}\ndata: {}", []any{[]any{[]any{true, nil}}, []any{[]any{"wrong"}}}},
		{"null_type", "type: 'null'\ndata: {}", []any{nil, "wrong"}},
	} {
		c := compile(t, wasm+"\noutput:\n"+indent(tc.output, 2), "")
		for i, v := range tc.values {
			projected, e := c.ProjectOutput(v, nil)
			if e == nil {
				cases = append(cases, standardCase{fmt.Sprintf("%s_%d", tc.name, i), c.SuccessSchema("mcp"), NewSuccessEnvelope("request", projected, nil), true})
			} else {
				cases = append(cases, standardCase{fmt.Sprintf("%s_%d", tc.name, i), c.SuccessSchema("mcp"), NewSuccessEnvelope("request", v, nil), false})
			}
		}
	}
	for _, executor := range []string{selectExecutor, `executor: {type: csv, read: {source: read, selector: {name: data.csv}}}`} {
		c := compile(t, executor+"\noutput: {items: {properties: {id: {type: integer}, name: {type: string}}}, data: {}}", "")
		out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: []any{map[string]any{"id": nil, "name": nil, "secret": "excluded"}}, Columns: []string{"id", "name", "secret"}}, nil)
		if e != nil {
			t.Fatal(e)
		}
		cases = append(cases, standardCase{executor + "_valid", c.SuccessSchema("mcp"), NewSuccessEnvelope("request", out.Data, nil), true})
		out.Close()
		for i, bad := range []any{nil, map[string]any{"rows": nil, "count": int64(0)}, map[string]any{"rows": []any{nil}, "count": int64(1)}, map[string]any{"rows": []any{map[string]any{"id": int64(1)}}, "count": int64(1)}} {
			cases = append(cases, standardCase{fmt.Sprintf("%s_bad_%d", executor, i), c.SuccessSchema("mcp"), NewSuccessEnvelope("request", bad, nil), false})
		}
	}
	// Passthrough does not make optional declaration keys mandatory for row data.
	pass := compile(t, selectExecutor+"\noutput: {mode: passthrough, items: {properties: {optional: {type: string}}}, data: {}}", "")
	out, e := ProcessOutput(context.Background(), pass, ProviderResult{Data: []any{map[string]any{"id": int64(1)}}, Columns: []string{"id"}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	cases = append(cases, standardCase{"passthrough_optional_row", pass.SuccessSchema("mcp"), NewSuccessEnvelope("request", out.Data, nil), true})
	out.Close()
	omitted := compile(t, wasm, "")
	cases = append(cases, standardCase{"omitted", omitted.SuccessSchema("rest"), NewSuccessEnvelope("request", nil, nil), true}, standardCase{"omitted_wrong", omitted.SuccessSchema("rest"), NewSuccessEnvelope("request", true, nil), false})
	validateECMACases(t, cases)
}
func TestPublicDMLCountRetainedAndDictionaryConstraints(t *testing.T) {
	cases := []standardCase{}
	for i, output := range []string{
		"data: {}",
		"type: [object,string]\ndata: {}",
		"mode: passthrough\nproperties: {count: {type: integer}}\nadditionalProperties: false\ndata: {}",
		"type: object\nadditionalProperties: {type: number, minimum: 2, maximum: 4}\ndata: {}",
		"mode: passthrough\ntype: object\nadditionalProperties: {type: integer, minimum: 2, maximum: 4}\ndata: {}",
	} {
		c := compile(t, dmlExecutor+"\noutput:\n"+indent(output, 2), "")
		out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: map[string]any{"count": int64(3)}}, nil)
		if e != nil {
			t.Fatal(e)
		}
		cases = append(cases, standardCase{fmt.Sprintf("dml_%d_valid", i), c.SuccessSchema("mcp"), NewSuccessEnvelope("request", out.Data, nil), true})
		out.Close()
		for j, invalid := range []any{map[string]any{}, map[string]any{"count": nil}, map[string]any{"count": -1}, map[string]any{"count": 1.5}} {
			cases = append(cases, standardCase{fmt.Sprintf("dml_%d_invalid_%d", i, j), c.SuccessSchema("mcp"), NewSuccessEnvelope("request", invalid, nil), false})
		}
		if i >= 3 {
			for _, count := range []int64{1, 5} {
				invalid := map[string]any{"count": count}
				if _, e := c.ProjectOutput(invalid, nil); e == nil {
					t.Fatal("DML dictionary count constraint bypassed")
				}
				cases = append(cases, standardCase{fmt.Sprintf("dml_%d_dictionary_%d", i, count), c.SuccessSchema("mcp"), NewSuccessEnvelope("request", invalid, nil), false})
			}
		}
	}
	validateECMACases(t, cases)
}
func TestPublicCSVClosedPassthroughStandardValidator(t *testing.T) {
	cases := []standardCase{}
	for i, executor := range []string{wasm, selectExecutor} {
		c := compile(t, executor+"\noutput: {mode: passthrough, items: {properties: {b: {type: string}, a: {type: string}}, additionalProperties: false}, data: {}, files: {format: csv, filename: x.csv}}", "")
		out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: []any{map[string]any{"a": "A", "b": "B"}}, Columns: []string{"a", "b"}}, &memoryStager{})
		if e != nil {
			t.Fatal(e)
		}
		file := out.Files[0]
		files := []PublicFileDescriptor{{ID: "file", ArtifactMetadata: file.ArtifactMetadata, URI: "https://example.test/file", ExpiresAt: "2026-10-01T00:00:00Z"}}
		cases = append(cases, standardCase{fmt.Sprintf("closed_passthrough_%d_valid", i), c.SuccessSchema("mcp"), NewSuccessEnvelope("request", out.Data, files), true})
		out.Close()
		invalid := any([]any{map[string]any{"a": "A", "b": "B", "secret": "hidden"}})
		if rowProvider(c) {
			invalid = map[string]any{"rows": invalid, "count": int64(1)}
		}
		cases = append(cases, standardCase{fmt.Sprintf("closed_passthrough_%d_unknown_key", i), c.SuccessSchema("mcp"), NewSuccessEnvelope("request", invalid, files), false})
	}
	validateECMACases(t, cases)
}
func TestPublicInputStandardValidatorMatchesAgent(t *testing.T) {
	c := compile(t, wasm+"\n"+`input:
  properties:
    id: {type: integer, default: 1, minimum: 1}
    title: {type: [string, 'null'], enum: [valid, null]}
    objects: {items: {properties: {value: {type: boolean}}, required: [value]}}
  required: [id]
output: {data: {}}`, "")
	cases := []standardCase{}
	for i, v := range []map[string]any{{}, {"id": int64(1), "title": nil}, {"id": int64(0)}, {"title": "invalid"}, {"objects": []any{map[string]any{"value": true}}}, {"objects": []any{map[string]any{}}}, {"unknown": true}, {"id": nil}} {
		_, _, e := c.ValidateInput(v)
		cases = append(cases, standardCase{fmt.Sprintf("input_%d", i), c.InputMediaSchemas().JSON, v, e == nil})
	}
	// File syntax and server byte/extension metadata are distinct contracts.
	f := compile(t, wasm+"\ninput: {properties: {document: {type: file, extensions: [csv]}, documents: {items: {type: file}, maxItems: 2}}, required: [document]}", "")
	file := map[string]any{"name": "x.csv", "mime_type": "text/csv", "data": "eA=="}
	for i, v := range []any{map[string]any{"document": file}, map[string]any{"document": nil}, map[string]any{"document": map[string]any{"name": "x.csv", "mime_type": "text/csv", "data": "", "url": "https://unexpected"}}, map[string]any{"document": file, "documents": []any{}}, map[string]any{"document": file, "documents": []any{file, file, file}}} {
		cases = append(cases, standardCase{fmt.Sprintf("file_%d", i), f.InputMediaSchemas().JSON, v, i == 0 || i == 3})
	}
	validateECMACases(t, cases)
}
func TestPublicErrorSchemaStandardValidator(t *testing.T) {
	cases := []standardCase{}
	for i, value := range []ErrorValue{
		{Code: "APP_A", Message: "safe", Outcome: "no_change"}, {Code: "APP_" + strings.Repeat("A", 60), Message: strings.Repeat("x", 1024), Outcome: "committed"},
		{Code: "APP_" + strings.Repeat("A", 61), Message: "safe", Outcome: "unknown"}, {Code: "APP_A", Message: "safe", Reason: "NOT_ALLOWED", Outcome: "unknown"},
		{Code: "APP_A", Message: "unsafe\n", Outcome: "unknown"}, {Code: "APP_A", Message: "", Outcome: "unknown"}, {Code: "APP_A", Message: "safe", Outcome: "wrong"},
		{Code: "EXECUTION_TIMEOUT", Message: "safe", Reason: "UPSTREAM_RESPONSE_TIMEOUT", Outcome: "unknown"}, {Code: "UNKNOWN", Message: "safe", Outcome: "unknown"},
	} {
		cases = append(cases, standardCase{fmt.Sprintf("error_%d", i), ErrorSchema(), ErrorEnvelope{"request", value}, value.Validate() == nil})
	}
	cases = append(cases, standardCase{"result_error_exclusive", ErrorSchema(), map[string]any{"request_id": "request", "error": ErrorValue{Code: "APP_A", Message: "safe", Outcome: "unknown"}, "result": map[string]any{"data": nil, "files": []any{}}}, false})
	validateECMACases(t, cases)
}
func TestPublicGeneratedOpenAPIDocumentValid(t *testing.T) {
	for _, cap := range []string{
		wasm + "\ninput: {properties: {file: {type: file}, list: {items: {type: file}, minItems: 0}}}\noutput: {data: {}, files: {}}",
		selectExecutor + "\noutput: {items: {properties: {id: {type: integer}}}, data: {}, files: {format: csv, filename: rows.csv}}",
		wasm + "\noutput: {files: {delivery: {rest: direct}}}",
	} {
		cfg, e := Compile([]byte(fixture(cap, "")))
		if e != nil {
			t.Fatal(e)
		}
		b, e := json.Marshal(BuildOpenAPI(cfg))
		if e != nil {
			t.Fatal(e)
		}
		doc, e := libopenapi.NewDocument(b)
		if e != nil {
			t.Fatal(e)
		}
		v, errs := validator.NewValidator(doc)
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		if valid, errs := v.ValidateDocument(); !valid {
			t.Fatalf("OpenAPI invalid: %v", errs)
		}
	}
}
