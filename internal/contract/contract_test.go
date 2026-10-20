package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
)

const configHeader = `schema_version: 1
service: {title: test, version: "1"}
gateway: {url: wss://gateway.example/ws/agent, agent_private_key_file: ./missing.key}
sources:
  databases:
    db: {driver: postgres, host: db.example, port: 5432, name: test, user: test}
  folders:
    read: {root: ./reports, access: read}
    write: {root: ./exchange, access: read_write}
  http:
    api: {base_url: https://api.example, allowed_methods: [GET, POST]}
`

func fixture(cap, extra string) string {
	return configHeader + extra + "capabilities:\n  test:\n" + indent(cap, 4)
}
func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	return pad + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n"+pad) + "\n"
}
func compile(t *testing.T, cap, extra string) *Capability {
	t.Helper()
	c, e := Compile([]byte(fixture(cap, extra)))
	if e != nil {
		t.Fatalf("compile: %v\n%s", e, fixture(cap, extra))
	}
	return c.Capabilities["test"]
}

const wasm = `executor: {type: wasm, module: ./missing.wasm}`

func canonical(t *testing.T, v any) []byte {
	t.Helper()
	b, e := CanonicalJSON(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func sameDefinition(t *testing.T, a, b *Capability) {
	t.Helper()
	aa := canonical(t, a.Normalized())
	bb := canonical(t, b.Normalized())
	if !bytes.Equal(aa, bb) {
		t.Fatalf("definitions differ\n%s\n%s", aa, bb)
	}
}
func TestVersionedCompilerAndProviderAST(t *testing.T) {
	for name, cap := range map[string]string{
		"db_select": `executor: {type: db, source: db, sql: "SELECT :id"}
input: {properties: {id: {type: integer}}, required: [id]}
output: {items: {properties: {id: {type: integer}}}, data: {}}`,
		"db_dml": `executor: {type: db, source: db, sql: "UPDATE customers SET name=:name"}
input: {properties: {name: {type: string}}}
output: {properties: {count: {type: integer}}, data: {}}
policy: {readonly: false}`,
		"csv_folder": `executor: {type: csv, read: {source: read, selector: {name_regex: 'sales_[0-9]+\.csv'}}}
output: {data: {}, files: {format: csv, filename: report.csv}}`,
		"csv_input": `executor: {type: csv, read: {input: /documents}}
input: {properties: {documents: {items: {type: file}, minItems: 1}}, required: [documents]}
output: {items: {properties: {name: {type: string}}}, files: {format: csv, filename: report.csv}}`,
		"file_read": `executor: {type: file, read: {source: read, selector: {name: file.pdf}}}
output: {files: {delivery: {rest: direct}}}`,
		"file_write": `executor: {type: file, write: [{input: /file, source: write, name: file.pdf}], read: {written: true}}
input: {properties: {file: {type: file}}, required: [file]}
output: {files: {}}`,
		"http": `executor: {type: http, source: api, method: POST, path: /submit, body: {kind: json, value: {a: 1}}, response: {kind: json}}
output: {data: {}}`,
		"wasm": wasm + "\noutput: {data: {}, files: {}}",
	} {
		t.Run(name, func(t *testing.T) {
			c := compile(t, cap, "")
			if c.Executor.Type == "" || c.Input == nil || c.Output.Schema == nil {
				t.Fatal("incomplete AST")
			}
			canonical(t, c.Normalized())
		})
	}
}
func TestCompilerRejectsVersionLegacyAndMalformedYAML(t *testing.T) {
	valid := fixture(wasm, "")
	for name, s := range map[string]string{"missing_version": strings.Replace(valid, "schema_version: 1\n", "", 1), "unsupported": strings.Replace(valid, "schema_version: 1", "schema_version: 2", 1), "null_version": strings.Replace(valid, "schema_version: 1", "schema_version: null", 1), "legacy": valid + "database: {}\n", "multi_document": valid + "---\n{}\n", "duplicate": valid + "schema_version: 1\n", "unknown": valid + "surprise: true\n", "invalid_utf8": valid + string([]byte{0xff}), "cyclic_alias": "schema_version: 1\nx: &x {<<: *x}\n"} {
		t.Run(name, func(t *testing.T) {
			if _, e := Compile([]byte(s)); e == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
func TestSchemaInferencePresenceUnionsAndOrder(t *testing.T) {
	a := compile(t, wasm+`
input:
  properties:
    objects: {items: {properties: {id: {type: integer}}}}
    matrix: {items: {items: {type: string}}}
    dict: {type: object, additionalProperties: {type: integer}}
    anything: {}
output: {items: {properties: {z: {type: string}, a: {type: integer}}}, files: {format: csv, filename: test.csv}}`, "")
	b := compile(t, wasm+`
input:
  type: object
  required: []
  additionalProperties: false
  properties:
    objects: {type: [array], items: {type: object, required: [], additionalProperties: false, properties: {id: {type: integer}}}}
    matrix: {type: array, items: {type: array, items: {type: string}}}
    dict: {type: object, required: [], additionalProperties: {type: integer}}
    anything: {}
output: {type: array, nullable: true, items: {type: object, nullable: true, required: [], additionalProperties: false, properties: {z: {type: [string, "null"]}, a: {type: integer, nullable: true}}}, files: {format: csv, filename: test.csv, columns: [z,a], formula_escape: true, mime_type: text/csv, from: "", delivery: {rest: reference, mcp: reference}}}`, "")
	sameDefinition(t, a, b)
	if got := a.Output.Files["columns"].([]string); strings.Join(got, ",") != "z,a" {
		t.Fatal(got)
	}
	empty := compile(t, wasm+"\ninput: {properties: {nothing: {properties: {}}}}", "")
	if !empty.Input.Properties["nothing"].PropertiesSet {
		t.Fatal("empty properties presence lost")
	}
	value, _, e := a.ValidateInput(map[string]any{"objects": []any{map[string]any{"id": int64(1)}}, "matrix": []any{[]any{"s"}}, "dict": map[string]any{"A": int64(2)}, "anything": []any{nil, int64(3)}})
	if e != nil || value == nil {
		t.Fatal(e)
	}
}
func TestSchemaInvalidContracts(t *testing.T) {
	cases := []string{
		`input: {type: string}`, `input: {type: [object, "null"]}`, `input: {properties: {}, items: {}}`,
		`input: {properties: {x: {items: {type: string}, type: object}}}`, `input: {properties: {x: {properties: {}, type: [object,string]}}}`,
		`input: {properties: {x: {type: array, items: []}}}`, `input: {properties: {x: {minLength: 1}}}`, `input: {properties: {x: {enum: [1]}}}`,
		`input: {required: [missing]}`, `input: {properties: {x: {}}, required: [x,x]}`, `input: {additionalProperties: null}`, `input: {additionalProperties: {properties: {x: {type: file}}}}`,
		`input: {properties: {x: {type: integer, pattern: ""}}}`, `input: {properties: {x: {type: string, format: unknown}}}`, `input: {properties: {x: {type: string, maxLength: 1, minLength: 2}}}`,
		`input: {properties: {x: {type: integer, minimum: 2, maximum: 1}}}`, `input: {properties: {x: {type: integer, exclusiveMinimum: 1, maximum: 1}}}`,
		`input: {properties: {x: {type: string, enum: [x], default: y}}}`, `input: {properties: {x: {type: integer, default: null}}}`, `input: {properties: {x: {type: string, enum: []}}}`,
		`input: {properties: {x: {type: file, default: null}}}`, `input: {properties: {x: {type: [file,"null"]}}}`, `input: {properties: {x: {type: file, extensions: [.csv]}}}`,
		`input: {properties: {x: {type: file, mime_types: [Text/Plain]}}}`, `input: {properties: {x: {type: file, max_bytes: 33MiB}}}`,
		`output: {properties: {x: {type: file}}, data: {}}`, `output: {type: [string,"null"], nullable: false, data: {}}`, `output: {type: string, default: x, data: {}}`,
		`output: {data: null}`, `output: {data: []}`, `output: {data: {extra: true}}`, `output: {files: []}`, `output: {type: string}`, `input: {$ref: remote}`,
	}
	for i, s := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, e := Compile([]byte(fixture(wasm+"\n"+s, ""))); e == nil {
				t.Fatalf("accepted %s", s)
			}
		})
	}
}
func TestPolicyApplicabilityAndCanonicalEquivalence(t *testing.T) {
	defs := `defaults: {readonly: false, max_rows: 200, max_affected_rows: 2, max_output_bytes: 1MB, max_input_file_bytes: 2MiB, mcp_inline_total_bytes: 1MiB, file_ttl: 15m, expose_in_openapi: false}
`
	a := compile(t, wasm+"\noutput: {data: {}}", defs)
	b := compile(t, wasm+"\noutput: {data: {}}\npolicy: {max_output_bytes: 1048576, expose_in_openapi: false}", "")
	sameDefinition(t, a, b)
	for _, k := range []string{"readonly", "max_rows", "max_affected_rows", "max_input_file_bytes", "mcp_inline_total_bytes", "file_ttl"} {
		if _, ok := a.Policy[k]; ok {
			t.Fatalf("non-applicable %s inherited", k)
		}
	}
	selectCap := `executor: {type: db, source: db, sql: SELECT 1}`
	selectDef := compile(t, selectCap, defs)
	if selectDef.Policy["max_rows"] != int64(200) {
		t.Fatal(selectDef.Policy)
	}
	if _, ok := selectDef.Policy["max_affected_rows"]; ok {
		t.Fatal("DML default applied to SELECT")
	}
	dml := compile(t, `executor: {type: db, source: db, sql: DELETE FROM t}`, defs)
	if dml.Policy["readonly"] != false || dml.Policy["max_affected_rows"] != int64(2) {
		t.Fatal(dml.Policy)
	}
	if _, ok := dml.Policy["max_rows"]; ok {
		t.Fatal("SELECT default applied to DML")
	}
	for _, rest := range []string{"reference", "direct"} {
		for _, mcp := range []string{"reference", "inline"} {
			cap := fmt.Sprintf("executor: {type: file, read: {source: read, selector: {name: x}}}\noutput: {files: {delivery: {rest: %s, mcp: %s}}}", rest, mcp)
			c := compile(t, cap, defs)
			if c.Policy["file_ttl"] != "15m0s" {
				t.Fatal("TTL absent")
			}
			_, got := c.Policy["mcp_inline_total_bytes"]
			if got != (mcp == "inline") {
				t.Fatal(c.Policy)
			}
			if mcp != "inline" {
				a := compile(t, cap, defs)
				b := compile(t, cap, strings.Replace(defs, "mcp_inline_total_bytes: 1MiB", "mcp_inline_total_bytes: 2MiB", 1))
				sameDefinition(t, a, b)
			}
			changed := compile(t, cap, strings.Replace(defs, "file_ttl: 15m", "file_ttl: 16m", 1))
			if bytes.Equal(canonical(t, c.Normalized()), canonical(t, changed.Normalized())) {
				t.Fatal("TTL change lost")
			}
		}
	}
	c := compile(t, wasm+"\npolicy: {expose_in_openapi: false}", "defaults: {expose_in_openapi: true}\n")
	if c.Policy["expose_in_openapi"] != false {
		t.Fatal("explicit false lost")
	}
}
func TestPolicyInvalidEvenUnusedDefaults(t *testing.T) {
	for i, s := range []string{`defaults: {max_rows: 0}`, `defaults: {readonly: null}`, `defaults: {max_rows: 1MiB}`, `defaults: {max_bytes: 1MiB}`, `defaults: {unknown: 1}`, `defaults: {timeout: 301s}`, `defaults: {max_output_bytes: 0}`, `defaults: {rate_limit: {requests: 1, per: 1s}}`, `defaults: {rate_limit: {requests: 1, per: 1s, burst: 9223372036854775807}}`} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, e := Compile([]byte(fixture(wasm, s+"\n"))); e == nil {
				t.Fatal(s)
			}
		})
	}
	for _, p := range []string{`readonly: false`, `max_rows: 1`, `max_output_bytes: 1MiB`, `max_input_files: 2`, `file_ttl: 1m`, `mcp_inline_total_bytes: 1MiB`, `timeout: null`} {
		if _, e := Compile([]byte(fixture(wasm+"\npolicy: {"+p+"}", ""))); e == nil {
			t.Fatal(p)
		}
	}
}
func TestByteSizeBoundaries(t *testing.T) {
	for _, v := range []any{int64(1024), "1024", "1024B", "1KiB", "1KB"} {
		n, e := ByteSize(v)
		if e != nil || n != 1024 {
			t.Fatalf("%v: %d %v", v, n, e)
		}
	}
	if n, e := ByteSize(int64(math.MaxInt64)); e != nil || n != math.MaxInt64 {
		t.Fatal(n, e)
	}
	for _, v := range []any{nil, true, int64(0), int64(-1), 1.5, "1.5MiB", "0B", "1TB", "9223372036854775808", "9223372036854775807GiB", "1 MiB", "1mib"} {
		if _, e := ByteSize(v); e == nil {
			t.Fatal(v)
		}
	}
}
func TestInputDefaultsNullPrecisionAndConstraints(t *testing.T) {
	c := compile(t, wasm+`
input:
  properties:
    integer: {type: integer, minimum: 9007199254740993, maximum: 9223372036854775807}
    nullable: {type: [string,"null"], default: x}
    optional: {type: boolean}
    nested: {properties: {value: {type: integer, default: 3}}, default: {}}
    anything: {}
  required: [integer]`, "")
	l := c.Limits()
	v, e := DecodeJSON([]byte(`{"integer":9223372036854775807,"nullable":null,"anything":{"big":9007199254740993}}`), l)
	if e != nil {
		t.Fatal(e)
	}
	out, _, e := c.ValidateInput(v.(map[string]any))
	if e != nil {
		t.Fatal(e)
	}
	if out["integer"] != int64(math.MaxInt64) || out["nullable"] != nil || out["nested"].(map[string]any)["value"] != int64(3) {
		t.Fatal(out)
	}
	if _, ok := out["optional"]; ok {
		t.Fatal("missing optional filled")
	}
	if out["anything"].(map[string]any)["big"] != int64(9007199254740993) {
		t.Fatal("precision lost")
	}
	data := canonical(t, out)
	if !bytes.Contains(data, []byte("9007199254740993")) {
		t.Fatal(string(data))
	}
	for _, v := range []any{json.Number("9007199254740993"), json.Number("9223372036854775807"), json.Number("9007199254740993.0")} {
		out, _, e := c.ValidateInput(map[string]any{"integer": v, "anything": v})
		if e != nil {
			t.Fatal(e)
		}
		if _, ok := out["integer"].(int64); !ok {
			t.Fatal(out)
		}
	}
	for _, v := range []any{int64(9007199254740992), nil, "9223372036854775807", json.Number("9223372036854775808"), float64(9007199254740994), json.Number("1.2")} {
		if _, _, e := c.ValidateInput(map[string]any{"integer": v}); e == nil {
			t.Fatal(v)
		}
	}
	if _, _, e := c.ValidateInput(map[string]any{"integer": int64(math.MaxInt64), "unknown": true}); e == nil {
		t.Fatal("unknown accepted")
	}
}
func TestJSONDecodeCanonicalAndStructuralLimits(t *testing.T) {
	l := Limits{MaxBytes: 1000, MaxDepth: 5, MaxItems: 100}
	v, e := DecodeJSON([]byte(`{"z":-0.0,"html":"<>&","i":9223372036854775807,"n":1.25e2}`), l)
	if e != nil {
		t.Fatal(e)
	}
	if got := string(canonical(t, v)); got != `{"html":"<>&","i":9223372036854775807,"n":125,"z":0}` {
		t.Fatal(got)
	}
	for _, s := range []string{`{"a":1,"a":2}`, `{"x":{"a":1,"a":2}}`, `{} {}`, `9223372036854775808`, `1e999`, `"\ud800"`, `"\udc00"`, `"\ud800x"`, "\"" + string([]byte{0xff}) + "\""} {
		if _, e := DecodeJSON([]byte(s), l); e == nil {
			t.Fatal(s)
		}
	}
	for _, s := range []string{`"\ud83d\ude00"`, `"�"`, `"\\ud800"`} {
		if _, e := DecodeJSON([]byte(s), l); e != nil {
			t.Fatal(s, e)
		}
	}
	for _, test := range []struct {
		input string
		limit Limits
	}{{`"1234"`, Limits{MaxBytes: 5, MaxDepth: 5, MaxItems: 10}}, {`[[[0]]]`, Limits{MaxBytes: 100, MaxDepth: 3, MaxItems: 10}}, {`[1,2]`, Limits{MaxBytes: 100, MaxDepth: 3, MaxItems: 2}}} {
		if _, e := DecodeJSON([]byte(test.input), test.limit); e == nil {
			t.Fatal(test)
		}
	}
}
func file(pointer, name, mt string, size int64) FileValue {
	digest := sha256.Sum256([]byte("fixture"))
	return FileValue{Pointer: pointer, ID: "opaque", RequestID: "request", Name: name, MIMEType: mt, SizeBytes: size, SHA256: hex.EncodeToString(digest[:])}
}
func TestFileLeafConstraintsOrderAndLimits(t *testing.T) {
	c := compile(t, wasm+`
input:
  properties:
    document: {type: file, extensions: [CSV,csv], mime_types: [text/csv], max_bytes: 4, allow_empty: false}
    nested: {items: {properties: {attachments: {items: {type: file}, maxItems: 2}}, required: [attachments]}, maxItems: 2}
  required: [document]
policy: {max_input_file_bytes: 4, max_total_input_file_bytes: 6, max_input_files: 3}`, "")
	value := map[string]any{"document": file("/document", "report.CSV", "text/csv", 2), "nested": []any{map[string]any{"attachments": []any{file("/nested/0/attachments/0", "a.bin", "application/octet-stream", 1), file("/nested/0/attachments/1", "b.bin", "application/octet-stream", 3)}}}}
	out, leaves, e := c.ValidateInput(value)
	if e != nil {
		t.Fatal(e)
	}
	if len(leaves) != 3 || leaves[1].Pointer != "/nested/0/attachments/0" || leaves[2].Value.Name != "b.bin" {
		t.Fatal(leaves)
	}
	b, e := CanonicalInput(out)
	if e != nil || bytes.Contains(b, []byte("opaque")) || bytes.Contains(b, []byte("request")) {
		t.Fatal(string(b), e)
	}
	for _, name := range []string{"report.csv.exe", ".csv", "report.", "report", "../x.csv", "x\\y.csv", "x\n.csv", "..", strings.Repeat("x", 256)} {
		if _, _, e := c.ValidateInput(map[string]any{"document": file("/document", name, "text/csv", 2)}); e == nil {
			t.Fatal(name)
		}
	}
	for _, f := range []FileValue{file("/document", "x.csv", "text/plain", 2), file("/document", "x.csv", "text/csv", 0), file("/document", "x.csv", "text/csv", 5), file("/wrong", "x.csv", "text/csv", 2)} {
		if _, _, e := c.ValidateInput(map[string]any{"document": f}); e == nil {
			t.Fatal(f)
		}
	}
	if _, _, e := c.ValidateInput(map[string]any{"document": nil}); e == nil {
		t.Fatal("file null accepted")
	}
	if _, _, e := c.ValidateInput(map[string]any{"document": map[string]any{"path": "/tmp/file"}}); e == nil {
		t.Fatal("host path accepted")
	}
	a := compile(t, wasm+"\ninput: {properties: {f: {type: file}}}", "")
	bcap := compile(t, wasm+"\ninput: {type: object, properties: {f: {type: file, extensions: [], mime_types: [], allow_empty: true, max_bytes: 32MiB}}}", "")
	sameDefinition(t, a, bcap)
	if _, _, e := a.ValidateInput(map[string]any{"f": file("/f", "empty", "application/octet-stream", 0)}); e != nil {
		t.Fatal(e)
	}
	// File depth/item accounting is one value even though metadata has fields.
	if _, _, e := ValidateInput(a.Input, map[string]any{"f": file("/f", "empty", "application/octet-stream", 0)}, Limits{MaxBytes: 1000, MaxDepth: 2, MaxItems: 3, MaxFiles: 1, MaxFileBytes: 32 << 20, MaxTotalFileBytes: 32 << 20}); e != nil {
		t.Fatal(e)
	}
}

func TestInputLimitsBeforeRecursiveValidationAndExactBytes(t *testing.T) {
	c := compile(t, wasm+"\ninput: {properties: {v: {}}}", "")
	limits := Limits{MaxBytes: 1000, MaxDepth: 3, MaxItems: 100}
	var deep any = true
	for range 20000 {
		deep = []any{deep}
	}
	if _, _, e := ValidateInput(c.Input, map[string]any{"v": deep}, limits); e == nil {
		t.Fatal("deep value accepted")
	}
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	if _, _, e := ValidateInput(c.Input, map[string]any{"v": cyclic}, limits); e == nil {
		t.Fatal("cycle accepted")
	}
	huge := make([]any, 100000)
	if _, _, e := ValidateInput(c.Input, map[string]any{"v": huge}, limits); e == nil {
		t.Fatal("item overflow accepted")
	}
	for _, v := range []any{"<>&\n\t\"\\😀\u2028", map[string]any{"😀": []any{int64(math.MaxInt64), true, nil}}, file("/v", "emoji😀.csv", "text/csv", 0)} {
		value := map[string]any{"v": v}
		b, e := CanonicalInput(value)
		if e != nil {
			t.Fatal(e)
		}
		l := Limits{MaxBytes: int64(len(b)), MaxDepth: 10, MaxItems: 100}
		if e := checkJSONLimits(value, l); e != nil {
			t.Fatal("exact limit", e, string(b))
		}
		l.MaxBytes--
		if e := checkJSONLimits(value, l); e == nil {
			t.Fatal("byte limit +1 accepted")
		}
	}
	for _, number := range []json.Number{"9223372036854775808", "-9223372036854775809", "NaN"} {
		if _, _, e := c.ValidateInput(map[string]any{"v": number}); e == nil {
			t.Fatal(number)
		}
	}
}

func TestFileLeafAggregateBoundariesAndOwnership(t *testing.T) {
	c := compile(t, wasm+"\ninput: {properties: {files: {items: {type: file}}}}\npolicy: {max_input_file_bytes: 4,max_total_input_file_bytes: 6,max_input_files: 2}", "")
	input := map[string]any{"files": []any{file("/files/0", "a", "application/octet-stream", 3), file("/files/1", "b", "application/octet-stream", 3)}}
	if _, _, e := c.ValidateInput(input); e != nil {
		t.Fatal(e)
	}
	input["files"].([]any)[1] = file("/files/1", "b", "application/octet-stream", 4)
	if _, _, e := c.ValidateInput(input); e == nil {
		t.Fatal("total +1 accepted")
	}
	input["files"] = []any{file("/files/0", "a", "application/octet-stream", 1), file("/files/1", "b", "application/octet-stream", 1), file("/files/2", "c", "application/octet-stream", 1)}
	if _, _, e := c.ValidateInput(input); e == nil {
		t.Fatal("count +1 accepted")
	}
	for _, modify := range []func(*FileValue){func(f *FileValue) { f.ID = "" }, func(f *FileValue) { f.RequestID = "" }, func(f *FileValue) { f.SHA256 = "bad" }, func(f *FileValue) { f.SizeBytes = -1 }} {
		f := file("/files/0", "a", "application/octet-stream", 1)
		modify(&f)
		if _, _, e := c.ValidateInput(map[string]any{"files": []any{f}}); e == nil {
			t.Fatal("invalid ownership/metadata accepted")
		}
	}
}

func TestInputSchemaFileLimitsAreRequestLocal(t *testing.T) {
	for _, explicit := range []int64{0, 5, 10, 15} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			property := `{type: file}`
			if explicit > 0 {
				property = fmt.Sprintf(`{type: file,max_bytes: %d}`, explicit)
			}
			s, e := CompileSchema([]byte(`properties: {f: `+property+`}`), true)
			if e != nil {
				t.Fatal(e)
			}
			before := canonical(t, s.Normalized())
			limit := int64(10)
			effective := limit
			if explicit > 0 {
				effective = min(limit, explicit)
			}
			l := Limits{MaxBytes: 1000, MaxDepth: 3, MaxItems: 10, MaxFileBytes: limit, MaxTotalFileBytes: 20, MaxFiles: 1}
			for _, size := range []int64{1, effective - 1, effective, effective + 1} {
				_, leaves, e := ValidateInput(s, map[string]any{"f": file("/f", "x", "text/plain", size)}, l)
				if (e == nil) != (size <= effective) {
					t.Fatalf("size=%d effective=%d: %v", size, effective, e)
				}
				if e == nil && (len(leaves) != 1 || leaves[0].Schema.File.MaxBytes != effective) {
					t.Fatal(leaves)
				}
			}
			// A second caller resolves its own omitted/policy cap independently.
			l.MaxFileBytes = 2
			if _, _, e := ValidateInput(s, map[string]any{"f": file("/f", "x", "text/plain", 3)}, l); e == nil {
				t.Fatal("second policy not applied")
			}
			l.MaxFileBytes = 10
			if _, _, e := ValidateInput(s, map[string]any{"f": file("/f", "x", "text/plain", effective)}, l); e != nil {
				t.Fatal(e)
			}
			var workers sync.WaitGroup
			failures := make(chan error, 16)
			for i := range 16 {
				workers.Add(1)
				go func() {
					defer workers.Done()
					local := l
					local.MaxFileBytes = int64(1 + i%2)
					_, leaves, err := ValidateInput(s, map[string]any{"f": file("/f", "x", "text/plain", local.MaxFileBytes)}, local)
					if err != nil {
						failures <- err
					} else if leaves[0].Schema.File.MaxBytes != local.MaxFileBytes {
						failures <- fmt.Errorf("caller limit lost")
					}
				}()
			}
			workers.Wait()
			close(failures)
			for err := range failures {
				t.Fatal(err)
			}
			if s.Properties["f"].File.MaxBytes != explicit || !bytes.Equal(before, canonical(t, s.Normalized())) {
				t.Fatal("shared schema mutated")
			}
		})
	}
}

func TestInputNullHonorsEnumConstAndDefault(t *testing.T) {
	for name, constraint := range map[string]string{"enum": "enum: [x]", "const": "const: x", "both": "enum: [x,null], const: x"} {
		t.Run(name, func(t *testing.T) {
			c := compile(t, wasm+"\n"+`input: {properties: {v: {type: [string,"null"], `+constraint+`}}}`, "")
			if _, _, e := c.ValidateInput(map[string]any{"v": nil}); e == nil {
				t.Fatal("null escaped value constraint")
			}
			if _, _, e := c.ValidateInput(map[string]any{"v": "x"}); e != nil {
				t.Fatal(e)
			}
			if _, e := Compile([]byte(fixture(wasm+"\n"+`input: {properties: {v: {type: [string,"null"], `+constraint+`, default: null}}}`, ""))); e == nil {
				t.Fatal("invalid null default accepted")
			}
		})
	}
	for name, schema := range map[string]string{
		"enum_null":       `{type: [string,"null"], enum: [x,null],default: null}`,
		"const_null":      `{type: [string,"null"],const: null,default: null}`,
		"enum_const_null": `{type: [string,"null"],enum: [null],const: null,default: null}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := compile(t, wasm+"\ninput: {properties: {v: "+schema+"},required: [v]}", "")
			for _, params := range []map[string]any{nil, {"v": nil}} {
				out, _, e := c.ValidateInput(params)
				if e != nil || out["v"] != nil {
					t.Fatal(out, e)
				}
			}
		})
	}
	for _, schema := range []string{`{type: string,enum: [null]}`, `{type: string,const: null}`, `{type: [string,"null"],enum: [x],const: null}`} {
		if _, e := CompileSchema([]byte("properties: {v: "+schema+"}"), true); e == nil {
			t.Fatal(schema)
		}
	}
	output, e := CompileSchema([]byte(`type: string
enum: [x]
const: x`), false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := output.validate(nil, "", false, nil); e != nil {
		t.Fatal("output nullable bypass changed", e)
	}
}
