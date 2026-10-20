package contract

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func assertPublicOnly(t *testing.T, value any) {
	t.Helper()
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if strings.HasPrefix(k, "x-") {
					continue
				}
				if k == "nullable" || k == "mode" {
					t.Fatalf("private schema key %s", k)
				}
				if k == "type" && v == "file" {
					t.Fatal("file type leaked")
				}
				visit(v)
			}
		case []any:
			for _, v := range x {
				visit(v)
			}
		}
	}
	visit(value)
}
func TestPublicSchemaInlineMultipartAndMCPVersions(t *testing.T) {
	c := compile(t, wasm+"\n"+`input:
  properties:
    document: {type: file, extensions: [CSV], max_bytes: 3B, mime_types: [application/json]}
    attachments: {items: {type: file}, minItems: 0, maxItems: 3}
    nested: {properties: {document: {type: file}}}
    exact: {type: integer, default: 9223372036854775807}
  required: [document, exact]
output: {data: {}}`, "")
	media := c.InputMediaSchemas()
	assertPublicOnly(t, media.JSON)
	assertPublicOnly(t, media.Multipart)
	props := media.JSON["properties"].(map[string]any)
	file := props["document"].(map[string]any)
	if file["type"] != "object" || file["additionalProperties"] != false {
		t.Fatal(file)
	}
	constraints := file["x-onprest-file"].(map[string]any)
	if constraints["max_bytes"] != int64(3) || constraints["extensions"].([]string)[0] != "csv" {
		t.Fatal(constraints)
	}
	data := file["properties"].(map[string]any)["data"].(map[string]any)
	if _, ok := data["maxLength"]; ok {
		t.Fatal("decoded bytes misrepresented as encoded maxLength")
	}
	if data["contentEncoding"] != "base64" {
		t.Fatal(data)
	}
	if contains(media.JSON["required"].([]string), "exact") {
		t.Fatal("defaulted input incorrectly remains required on the wire")
	}
	mp := media.Multipart["properties"].(map[string]any)
	if mp["document"].(map[string]any)["format"] != "binary" || mp["attachments"].(map[string]any)["minItems"] != int64(1) {
		t.Fatal(mp)
	}
	if media.Encoding["document"].(map[string]any)["contentType"] != "application/json" || media.Encoding["attachments"].(map[string]any)["contentType"] != "*/*" || media.Encoding["nested"].(map[string]any)["contentType"] != "application/json" {
		t.Fatal(media.Encoding)
	}
	cfg := &Config{Capabilities: map[string]*Capability{"test": c}}
	for _, version := range []string{"", "2025-03-26", "2025-06-18", "2025-11-25"} {
		tools, e := BuildMCPTools(cfg, version)
		if e != nil || len(tools) != 1 {
			t.Fatal(tools, e)
		}
		tool := tools[0]
		if !equalJSON(tool["inputSchema"], media.JSON) {
			t.Fatal("MCP input differs")
		}
		_, has := tool["outputSchema"]
		if has != (version == "2025-06-18" || version == "2025-11-25") {
			t.Fatal("versioned outputSchema wrong")
		}
		if has && tool["outputSchema"].(map[string]any)["type"] != "object" {
			t.Fatal("scalar business data broke object outputSchema")
		}
	}
	if _, e := BuildMCPTools(cfg, "unsupported"); e == nil {
		t.Fatal("unknown MCP version accepted")
	}
	body, e := json.Marshal(media.JSON)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(body), "9223372036854775807") {
		t.Fatal("default metadata lost int64 precision")
	}
}
func TestPublicSchemaMultipartZeroArraysAndOmittedOutputs(t *testing.T) {
	for _, required := range []bool{false, true} {
		suffix := ""
		if required {
			suffix = "\n  required: [files]"
		}
		c := compile(t, wasm+"\ninput:\n  properties: {files: {items: {type: file}, maxItems: 0}}"+suffix, "")
		media := c.InputMediaSchemas()
		if required {
			if media.Multipart != nil {
				t.Fatal("impossible multipart media advertised")
			}
		} else {
			if _, ok := media.Multipart["properties"].(map[string]any)["files"]; ok {
				t.Fatal("zero-only optional array advertised in multipart")
			}
		}
		if media.JSON == nil {
			t.Fatal("JSON empty array contract missing")
		}
	}
	c := compile(t, wasm, "")
	result := c.SuccessSchema("rest")["properties"].(map[string]any)["result"].(map[string]any)["properties"].(map[string]any)
	if result["data"].(map[string]any)["type"] != "null" || result["files"].(map[string]any)["maxItems"] != int64(0) {
		t.Fatal("omitted outputs have incorrect schema")
	}
}
func TestPublicMetadataVisibilityAnnotationsAndDirect(t *testing.T) {
	config, e := Compile([]byte(fixture(wasm+"\ndescription: useful\nannotations: {read_only: false, destructive: true}\ninput: {properties: {n: {type: integer}}}\nexamples: [{params: {n: 9223372036854775807}}]\noutput: {data: {}}", "")))
	if e != nil {
		t.Fatal(e)
	}
	hidden := compile(t, selectExecutor+"\npolicy: {expose_in_openapi: false}", "")
	config.Capabilities["hidden"] = hidden
	before := string(canonical(t, config.Capabilities["test"].Normalized()))
	doc := BuildOpenAPI(config)
	if len(doc["paths"].(map[string]any)) != 1 {
		t.Fatal("hidden capability advertised")
	}
	tool, e := BuildMCPTools(config, "2025-11-25")
	if e != nil || len(tool) != 1 {
		t.Fatal(tool, e)
	}
	if tool[0]["annotations"].(map[string]any)["readOnlyHint"] != false {
		t.Fatal("false annotation dropped")
	}
	if _, ok := tool[0]["x-onprest-examples"]; ok {
		t.Fatal("OpenAPI-only examples copied into MCP")
	}
	b := canonical(t, doc)
	for _, secret := range []string{"SELECT id", "missing.wasm", "db.example", "agent.key", "secret"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("private executor information leaked: %s", secret)
		}
	}
	if !strings.Contains(string(b), "9223372036854775807") {
		t.Fatal("example precision lost")
	}
	// Returned schema maps do not share mutable storage with compiler schemas.
	op := doc["paths"].(map[string]any)["/api/v1/capabilities/test"].(map[string]any)["post"].(map[string]any)
	op["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)["n"].(map[string]any)["maximum"] = int64(1)
	if before != string(canonical(t, config.Capabilities["test"].Normalized())) {
		t.Fatal("metadata generation mutated definition")
	}
	direct := compile(t, wasm+"\noutput: {files: {mime_type: application/pdf, delivery: {rest: direct, mcp: inline}}}", "")
	config.Capabilities = map[string]*Capability{"test": direct}
	op = BuildOpenAPI(config)["paths"].(map[string]any)["/api/v1/capabilities/test"].(map[string]any)["post"].(map[string]any)
	content := op["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)
	if len(content) != 1 || content["application/pdf"].(map[string]any)["schema"].(map[string]any)["format"] != "binary" {
		t.Fatal(content)
	}
	success := direct.SuccessSchema("mcp")
	result := success["properties"].(map[string]any)["result"].(map[string]any)["properties"].(map[string]any)
	descriptor := result["files"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if _, ok := descriptor["uri"]; ok {
		t.Fatal("inline descriptor incorrectly promises a fetchable URI")
	}
}
func TestPublicSchemaEquivalentDefinitionsAndProviderRequired(t *testing.T) {
	a := compile(t, wasm+"\noutput: {properties: {x: {type: [integer, 'null']}}, data: {}}", "")
	b := compile(t, wasm+"\noutput: {type: object, properties: {x: {type: integer, nullable: true}}, required: [], additionalProperties: false, data: {}}", "")
	if !equalJSON(a.SuccessSchema("rest"), b.SuccessSchema("rest")) {
		t.Fatal("equivalent definitions generate different schemas")
	}
	c := compile(t, selectExecutor+"\noutput: {items: {properties: {id: {type: integer}, label: {type: string}}}, data: {}}", "")
	data := c.dataSchema()
	rows := data["properties"].(map[string]any)["rows"].(map[string]any)
	if rows["type"] != "array" {
		t.Fatal(rows)
	}
	item := rows["items"].(map[string]any)
	if len(item["required"].([]string)) != 2 || item["type"] != "object" {
		t.Fatal(item)
	}
	count := data["properties"].(map[string]any)["count"].(map[string]any)
	if count["maximum"] != int64(math.MaxInt64) {
		t.Fatal(count)
	}
}
