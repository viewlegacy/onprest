package contract

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestBindingStaticPresencePointerAndTypes(t *testing.T) {
	input := `
input:
  properties:
    nested: {properties: {document: {type: file}, name: {type: string}}, required: [document,name]}
    list: {items: {type: file}, minItems: 2}
    filters: {properties: {month: {type: string}}, required: [month]}
    "a/b~c": {type: file}
  required: [nested,list,filters,"a/b~c"]`
	for _, p := range []string{"/nested/document", "/list", "/list/0", "/list/1", "/a~1b~0c"} {
		compile(t, `executor: {type: csv, read: {input: '`+p+`'}}`+input, "")
	}
	for _, p := range []string{"/list/2", "/list/01", "/list/-1", "/nested/missing", "/nested/name", "list", "/a~2b", "/list/*"} {
		if _, e := Compile([]byte(fixture(`executor: {type: csv, read: {input: '`+p+`'}}`+input, ""))); e == nil {
			t.Fatal(p)
		}
	}
	optional := strings.Replace(input, "required: [nested,list,filters,\"a/b~c\"]", "required: [list,filters,\"a/b~c\"]", 1)
	if _, e := Compile([]byte(fixture(`executor: {type: csv, read: {input: /nested/document}}`+optional, ""))); e == nil {
		t.Fatal("optional ancestor accepted")
	}
	compile(t, `executor: {type: csv, read: {source: read, selector: {name: {input: /nested/name}}}, where: [{column: month, op: eq, input: /filters/month}]}`+input, "")
	for _, s := range []string{`executor: {type: db, source: db, sql: SELECT 1}` + input, `executor: {type: db, source: db, sql: "SELECT :unknown"}`, `executor: {type: db, source: db, sql: "SELECT :a"}
input: {properties: {a: {properties: {x: {type: integer}}}}}`, `executor: {type: db, source: db, sql: "WITH x AS (SELECT 1) SELECT * FROM x"}`, `executor: {type: db, source: db, sql: "UPDATE t SET x=1 RETURNING x"}
policy: {readonly: false}`} {
		if _, e := Compile([]byte(fixture(s, ""))); e == nil {
			t.Fatal(s)
		}
	}
	compile(t, `executor: {type: db, source: db, sql: "SELECT ':unknown' /* outer /* inner */ :unknown */"}`, "")
}
func TestCSVSettingsNormalizationAndStaticWhere(t *testing.T) {
	base := `executor: {type: csv, read: {source: read, selector: {name: input.csv}}}`
	a := compile(t, base, "")
	b := compile(t, strings.Replace(base, "}}}", "}}, csv: {header: true, encoding: utf-8, delimiter: comma, columns: {}}}", 1), "")
	sameDefinition(t, a, b)
	compile(t, `executor:
  type: csv
  read: {source: read, selector: {name: input.csv}}
  csv: {columns: {amount: {type: integer}}}
  where: [{column: month, op: in, input: /months}, {column: amount, op: gte, input: /minimum}]
input: {properties: {months: {items: {type: string}}, minimum: {type: integer}}, required: [months,minimum]}`, "")
	compile(t, `executor: {type: csv, read: {source: read, selector: {name: x}}, csv: {header: false, columns: {a: {position: 0}, b: {position: 1, type: integer}}}}`, "")
	for _, csv := range []string{`{header: null}`, `{header: false}`, `{header: false, columns: {a: {position: 1}}}`, `{header: false, columns: {a: {position: 0}, b: {position: 0}}}`, `{columns: {a: {position: 0}}}`, `{columns: {a: {type: object}}}`, `{encoding: unknown}`, `{delimiter: xx}`, `{delimiter: '"'}`, `{columns: {a: {unknown: true}}}`} {
		cap := `executor: {type: csv, read: {source: read, selector: {name: x}}, csv: ` + csv + `}`
		if _, e := Compile([]byte(fixture(cap, ""))); e == nil {
			t.Fatal(csv)
		}
	}
	for _, schema := range []string{`type: array`, `type: [array, string], items: {type: string}`, `items: {}`, `items: {type: integer}`} {
		// The unconfigured column is string; in needs a typed, array-only input.
		cap := `executor: {type: csv, read: {source: read, selector: {name: input.csv}}, where: [{column: month, op: in, input: /months}]}
input: {properties: {months: {` + schema + `}}, required: [months]}`
		if _, e := Compile([]byte(fixture(cap, ""))); e == nil {
			t.Fatal(schema)
		}
	}
}
func TestOutputSyntaxProviderShapeAndCSVColumnLint(t *testing.T) {
	for _, cap := range []string{
		`executor: {type: db, source: db, sql: SELECT 1}
output: {properties: {rows: {}}, data: {}}`,
		`executor: {type: db, source: db, sql: UPDATE t SET x=1}
policy: {readonly: false}
output: {properties: {}, data: {}}`,
		`executor: {type: db, source: db, sql: UPDATE t SET x=1}
policy: {readonly: false}
output: {properties: {count: {type: string}}, data: {}}`,
		`executor: {type: file, read: {source: read, selector: {name: x}}}`, `executor: {type: file, read: {source: read, selector: {name: x}}}
output: {data: {}, files: {}}`,
		`executor: {type: csv, read: {source: read, selector: {name: x}}}
output: {files: {}}`,
		wasm + `
output: {files: {format: csv, filename: x.csv}}`,
		wasm + `
output: {items: {properties: {a: {type: string}}}, files: {format: csv, filename: x.csv, columns: [secret]}}`,
		wasm + `
output: {properties: {rows: {items: {properties: {a: {type: string}}}}}, files: {format: csv, filename: x.csv, from: /missing}}`,
		wasm + `
output: {files: {from: /rows}}`, wasm + `
output: {data: {}, files: {delivery: {rest: direct}}}`,
		`executor: {type: file, read: {source: read, selector: {name: x, take: 2}}}
output: {files: {delivery: {rest: direct}}}`,
	} {
		if _, e := Compile([]byte(fixture(cap, ""))); e == nil {
			t.Fatal(cap)
		}
	}
	c := compile(t, wasm+`
output: {properties: {rows: {items: {properties: {z: {type: string}, a: {type: integer}}}}}, files: {format: csv, filename: x.csv, from: /rows}}`, "")
	cols := c.Output.Files["columns"].([]string)
	if strings.Join(cols, ",") != "z,a" {
		t.Fatal(cols)
	}
}
func TestHTTPBindingsFormsPartsAndHeaderContracts(t *testing.T) {
	input := `
input:
  properties:
    month: {type: [string,"null"]}
    tags: {items: {type: string}}
    document: {type: file}
    documents: {items: {type: file}}
    payload: {properties: {x: {type: integer}}}
  required: [document,payload]`
	bodies := []string{`{kind: json, fields: {month: {input: /month}, fixed: {value: null}}}`, `{kind: json, input: /payload}`, `{kind: json, value: [true, 9223372036854775807]}`, `{kind: form, fields: {month: {input: /month}, tags: {input: /tags}}}`, `{kind: file, input: /document}`, `{kind: multipart, parts: [{name: month, kind: text, input: /month}, {name: payload, kind: json, value: {x: true}}, {name: documents, kind: file, input: /documents, content_type: application/pdf}]}`}
	for _, body := range bodies {
		compile(t, `executor: {type: http, source: api, method: POST, path: /submit, query: {tag: {input: /tags}}, headers: {X-Source: {value: fixed}}, body: `+body+`, response: {kind: json}}`+input, "")
	}
	for _, body := range []string{`{kind: json}`, `{kind: json, value: null, input: /payload}`, `{kind: json, input: /month}`, `{kind: json, fields: {file: {input: /document}}}`, `{kind: form, fields: {x: {value: null}}}`, `{kind: file, input: /documents}`, `{kind: file, input: /document, value: x}`, `{kind: multipart, parts: [{name: x, kind: text, value: x}, {name: x, kind: text, value: y}]}`, `{kind: multipart, parts: [{name: x, kind: file, value: x}]}`, `{kind: multipart, parts: [{name: x, kind: text, value: x, content_type: text/plain}]}`} {
		if _, e := Compile([]byte(fixture(`executor: {type: http, source: api, method: POST, path: /submit, body: `+body+`, response: {kind: json}}`+input, ""))); e == nil {
			t.Fatal(body)
		}
	}
	for _, headers := range []string{`{X-Test: {value: a}, x-test: {value: a}}`, `{Authorization: {value: x}}`, `{Host: {value: x}}`, `{Content-Type: {value: text/plain}}`, `{X-Test: {value: "x\r\ny"}}`, `{X-Test: {value: x, input: /month}}`, `{X-Test: {}}`} {
		if _, e := Compile([]byte(fixture(`executor: {type: http, source: api, method: POST, path: /submit, body: {kind: json, value: {}}, headers: `+headers+`, response: {kind: json}}`+input, ""))); e == nil {
			t.Fatal(headers)
		}
	}
	f := fixture(`executor: {type: http, source: api, method: GET, path: /, headers: {x-test: {value: x}}, response: {kind: file}}`, "")
	f = strings.Replace(f, "allowed_methods: [GET, POST]", "allowed_methods: [GET, POST], headers: {X-Test: x}", 1)
	if _, e := Compile([]byte(f)); e == nil {
		t.Fatal("cross-source duplicate accepted")
	}
}
func TestFilesystemSelectorWriteSandboxAndSourceRejections(t *testing.T) {
	for _, cap := range []string{
		`executor: {type: csv, read: {source: read, selector: {}}}`, `executor: {type: csv, read: {source: read, selector: {name: x, name_regex: x}}}`, `executor: {type: csv, read: {source: read, selector: {name: ../x}}}`, `executor: {type: csv, read: {source: read, selector: {name: x, path: ../x}}}`, `executor: {type: csv, read: {source: read, selector: {name_regex: '['}}}`, `executor: {type: csv, read: {source: read, selector: {name: x, take: 0}}}`,
		`executor: {type: file, write: []}`, `executor: {type: file, read: {written: true}}`, `executor: {type: file, read: {written: false}}`,
		`executor: {type: file, write: [{input: /f, source: read, name: x}]}
input: {properties: {f: {type: file}}, required: [f]}`,
		`executor: {type: wasm, module: x, sandbox: {enabled: false, permissions: {}}}`, `executor: {type: wasm, module: x, sha256: ''}`, `executor: {type: wasm, module: x, limits: {max_file_read_bytes: 300MiB}}`,
		`executor: {type: wasm, module: x, sandbox: {enabled: true, permissions: {http: [{origin: 'https://*.example', methods: [GET], path_prefix: /}]}}}`,
	} {
		if _, e := Compile([]byte(fixture(cap, ""))); e == nil {
			t.Fatal(cap)
		}
	}
	for _, name := range []string{".onprest-x", "CON", "file.txt:", "x.", "x ", "NUL.csv"} {
		cap := fmt.Sprintf("executor: {type: file, write: [{input: /f, source: write, name: '%s'}]}\ninput: {properties: {f: {type: file}}, required: [f]}", name)
		if _, e := Compile([]byte(fixture(cap, ""))); e == nil {
			t.Fatal(name)
		}
	}
	compile(t, `executor: {type: wasm, module: x, sandbox: {enabled: true, permissions: {filesystem: [{root: ./reports, access: read}], http: [{origin: https://api.example, methods: [GET], path_prefix: /items, allowed_cidrs: [10.0.0.0/8]}], environment: [REGION]}}}`, "")
	for _, replacement := range []string{`allowed_methods: []`, `allowed_methods: [GET], credential: token, credential_file: ./token`, `allowed_methods: [GET], credential: ''`, `allowed_methods: [GET], credential: token, headers: {Authorization: token}`, `allowed_methods: [GET], headers: {X: x, x: x}`, `allowed_methods: [GET], allowed_cidrs: [bad]`} {
		data := strings.Replace(fixture(wasm, ""), "allowed_methods: [GET, POST]", replacement, 1)
		if _, e := Compile([]byte(data)); e == nil {
			t.Fatal(replacement)
		}
	}
}
func TestYAMLAliasesMergePresenceAndSafeDiagnostics(t *testing.T) {
	a := compile(t, wasm+`
input:
  properties:
    a: &str {type: string, default: inherited}
    b: {<<: *str, default: direct}
    c: {<<: [*str, {default: fallback}]}
policy: {expose_in_openapi: false}`, "")
	out, _, e := a.ValidateInput(nil)
	if e != nil || out["a"] != "inherited" || out["b"] != "direct" || out["c"] != "inherited" {
		t.Fatal(out, e)
	}
	data := fixture(wasm+"\ninput: {\"bad\\nkey\": true}", "")
	_, e = Compile([]byte(data))
	if e == nil || strings.Contains(e.Error(), "bad\nkey") {
		t.Fatal(e)
	}
}
func TestConfiguredExamplesUseSameValidatorWithoutInternalMetadata(t *testing.T) {
	c := compile(t, wasm+`
input: {properties: {document: {type: file, extensions: [csv], max_bytes: 4}, label: {type: string, default: sample}}, required: [document]}
examples: [{params: {document: {name: sample.csv, mime_type: text/csv, data: YSxiCg==}}}]`, "")
	b := canonical(t, c.Normalized())
	if bytes.Contains(b, []byte("configuration-example")) || bytes.Contains(b, []byte("example:/")) || !bytes.Contains(b, []byte("YSxiCg==")) || !bytes.Contains(b, []byte(`"label":"sample"`)) {
		t.Fatal(string(b))
	}
	for _, file := range []string{`{name: x.csv, mime_type: text/csv, data: YQ}`, `{name: x.csv, mime_type: text/csv, data: YR==}`, `{name: x.csv, mime_type: text/csv, data: "YQ==\n"}`, `{name: x.csv, mime_type: text/csv, data: YWJjZGU=}`, `{name: x.pdf, mime_type: text/csv, data: YQ==}`, `{name: x.csv, mime_type: text/csv, data: YQ==, path: /tmp/x}`} {
		cap := wasm + "\ninput: {properties: {f: {type: file, extensions: [csv], max_bytes: 4}}, required: [f]}\nexamples: [{params: {f: " + file + "}}]"
		if _, e := Compile([]byte(fixture(cap, ""))); e == nil {
			t.Fatal(file)
		}
	}
	compile(t, wasm+"\ninput: {properties: {f: {type: file}}}\nexamples: [{params: {}}]", "")
}

func TestCompilerOperationalConfigurationAndDeferredIO(t *testing.T) {
	valid := fixture(wasm, "")
	for name, replacement := range map[string][2]string{
		"gateway_key_exclusive": {"agent_private_key_file: ./missing.key", "agent_private_key_file: ./missing.key, agent_private_key: ''"},
		"gateway_nonloopback":   {"wss://gateway.example/ws/agent", "ws://gateway.example/ws/agent"},
		"gateway_path":          {"wss://gateway.example/ws/agent", "wss://gateway.example/wrong"},
		"db_secret_exclusive":   {"user: test}", "user: test,password: '',password_file: x}"},
		"db_port":               {"port: 5432", "port: 65536"},
		"db_tls_pair":           {"user: test}", "user: test,tls: {mode: require,cert_file: x}}"},
		"db_tls_disabled":       {"user: test}", "user: test,tls: {mode: disable,ca_file: x}}"},
		"db_tls_ca_mode":        {"user: test}", "user: test,tls: {mode: require,ca_file: x}}"},
		"http_secret_null":      {"allowed_methods: [GET, POST]", "allowed_methods: [GET, POST], credential: null"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := Compile([]byte(strings.Replace(valid, replacement[0], replacement[1], 1))); e == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
	for _, extra := range []string{"runtime: {max_concurrent_requests: 0}\n", "runtime: {input_limits: {max_pending_requests: 0}}\n", "runtime: {control_limits: {bytes_per_second: 0}}\n", "runtime: {control_limits: {read: {per: 0s}}}\n", "runtime: {control_limits: {cancel: {max_concurrent: 0}}}\n", "runtime: {transfer_timeout: 301s}\n", "logging: {max_files: 0}\n", "logging: {level: trace}\n", "logging: {max_size: null}\n"} {
		if _, e := Compile([]byte(fixture(wasm, extra))); e == nil {
			t.Fatal(extra)
		}
	}
	// Static compile does not resolve missing signing/credential/module files or
	// require any database source for non-DB definitions.
	minimal := `schema_version: 1
gateway: {url: ws://localhost/ws/agent,agent_private_key_file: ./missing.key}
capabilities: {test: {executor: {type: wasm,module: ./missing.wasm}}}
`
	cfg, e := Compile([]byte(minimal))
	if e != nil {
		t.Fatal(e)
	}
	ctrl := cfg.Runtime["control_limits"].(map[string]any)
	if ctrl["bytes_per_second"] != int64(8<<20) || ctrl["burst_bytes"] != int64(16<<20) || ctrl["max_concurrent_transfers"] != int64(4) {
		t.Fatal(ctrl)
	}
	key := base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	if _, e := Compile([]byte(strings.Replace(minimal, "agent_private_key_file: ./missing.key", "agent_private_key: "+key, 1))); e != nil {
		t.Fatal(e)
	}
	for _, tag := range []string{"!!null {}", "!!str {}", "!!str []"} {
		if _, e := Compile([]byte(fixture(wasm+"\ninput: "+tag, ""))); e == nil {
			t.Fatal(tag)
		}
	}
}

func TestOutputDynamicCSVFromAndFixedHTTPBudgets(t *testing.T) {
	for _, output := range []string{
		`output: {files: {format: csv,from: /rows,columns: [id],filename: rows.csv}}`,
		`output: {properties: {}, mode: passthrough, files: {format: csv,from: /rows,columns: [id],filename: rows.csv}}`,
		`output: {properties: {rows: {}},files: {format: csv,from: /rows,columns: [id],filename: rows.csv}}`,
	} {
		compile(t, wasm+"\n"+output, "")
	}
	compile(t, `executor: {type: db,source: db,sql: DELETE FROM t}
policy: {readonly: false}
output: {properties: {count: {type: number}},data: {}}`, "")
	cap := `executor: {type: http,source: api,method: POST,path: /,body: {kind: json,value: [[true]]},response: {kind: json}}`
	compile(t, cap+"\npolicy: {max_json_depth: 3,max_json_items: 3,max_input_bytes: 8}", "")
	for _, p := range []string{"max_json_depth: 2", "max_json_items: 2", "max_input_bytes: 7"} {
		if _, e := Compile([]byte(fixture(cap+"\npolicy: {"+p+"}", ""))); e == nil {
			t.Fatal(p)
		}
	}
	// Inline sample metadata is one logical file, including with very small
	// logical limits; the encoded sample is still finite and validated.
	compile(t, wasm+`
input: {properties: {f: {type: file,max_bytes: 1}},required: [f]}
policy: {max_json_depth: 2,max_json_items: 3,max_input_bytes: 200,max_input_files: 1,max_input_file_bytes: 1,max_total_input_file_bytes: 1}
examples: [{params: {f: {name: x,mime_type: text/plain,data: YQ==}}}]`, "")
}

func TestOutputFileOnlyHTTPRejectsCSVGeneration(t *testing.T) {
	cap := `executor: {type: http,source: api,method: GET,path: /,response: {kind: file}}`
	compile(t, cap+"\noutput: {files: {}}", "")
	if _, e := Compile([]byte(fixture(cap+"\noutput: {files: {format: csv,columns: [id],filename: x.csv}}", ""))); e == nil {
		t.Fatal("file-only HTTP generated CSV")
	}
}

func TestOutputProviderCompatibleBroadDeclarations(t *testing.T) {
	for _, cap := range []string{
		`executor: {type: db,source: db,sql: SELECT 1}
output: {type: [array,string],data: {}}`,
		`executor: {type: csv,read: {source: read,selector: {name: x}}}
output: {type: [array,object],data: {}}`,
		`executor: {type: db,source: db,sql: DELETE FROM t}
policy: {readonly: false}
output: {properties: {count: {type: number,nullable: true}},data: {}}`,
	} {
		compile(t, cap, "")
	}
}

func TestCompilerDatabaseEmptyPasswordPresenceAndExclusivity(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		data := fixture(wasm, "")
		if explicit {
			data = strings.Replace(data, "user: test}", `user: test,password: ""}`, 1)
		}
		cfg, e := Compile([]byte(data))
		if e != nil {
			t.Fatal(e)
		}
		value, present := cfg.Sources.Databases["db"]["password"]
		if present != explicit || explicit && value != "" {
			t.Fatal("password presence lost", cfg.Sources.Databases["db"])
		}
	}
	data := strings.Replace(fixture(wasm, ""), "user: test}", `user: test,password: "",password_file: missing}`, 1)
	if _, e := Compile([]byte(data)); e == nil {
		t.Fatal("empty inline plus file accepted")
	}
}
