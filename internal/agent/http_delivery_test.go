//go:build linux || darwin

package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/gateway"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/testschema"
)

func productionHTTPExecutor(t *testing.T, upstream string) *inputHTTPFixture {
	return productionInputHTTPAdjusted(t, nil, func(yaml string) string {
		yaml = strings.Replace(yaml, "sources: {folders: {exchange: {root: ./exchange, access: read_write}}}", fmt.Sprintf("sources: {folders: {exchange: {root: ./exchange, access: read_write}}, http: {api: {base_url: %q, allowed_methods: [GET, POST], allowed_cidrs: [127.0.0.0/8], credential: private-http-token}}}", upstream), 1)
		yaml = strings.Replace(yaml, "runtime: {", "runtime: {control_limits: {write: {requests: 1000, per: 1s, burst: 1000, max_concurrent: 16}, transfer: {requests: 1000, per: 1s, burst: 1000, max_concurrent: 16}}, ", 1)
		for _, kind := range []string{"json", "form", "multipart", "raw", "none", "array", "scalar", "null", "file_reference", "file_direct", "file_inline", "bad", "limited"} {
			input := `properties: {payload: {properties: {text: {type: string}, count: {type: integer}, optional: {type: [string, 'null']}}, required: [text,count]}, document: {type: file}, documents: {items: {type: file}, maxItems: 2}}, required: [payload]`
			body := `body: {kind: json, fields: {payload: {input: /payload}, fixed: {value: true}}},`
			switch kind {
			case "form":
				body = `body: {kind: form, fields: {text: {input: /payload/text}, count: {input: /payload/count}, tags: {value: ['a b','+']}, absent: {input: /payload/optional}}},`
			case "multipart":
				body = `body: {kind: multipart, parts: [{name: text, kind: text, input: /payload/text}, {name: json, kind: json, input: /payload/text}, {name: upload, kind: file, input: /documents, content_type: application/custom}, {name: omitted, kind: file, input: /document}]},`
			case "raw":
				body = `body: {kind: file, input: /document},`
				input = strings.Replace(input, "required: [payload]", "required: [payload,document]", 1)
			case "none":
				body = ""
			}
			response := `kind: json`
			output := `properties: {ok: {type: boolean}, count: {type: integer}}, data: {}`
			switch kind {
			case "array":
				output = `items: {type: integer}, data: {}`
			case "scalar":
				output = `type: string, data: {}`
			case "null":
				output = `type: 'null', data: {}`
			case "file_reference", "file_direct", "file_inline":
				response = `kind: file, filename: reply.bin, mime_type: application/custom`
				output = `files: {}`
				if kind == "file_direct" {
					output = `files: {delivery: {rest: direct}}`
				}
				if kind == "file_inline" {
					output = `files: {delivery: {mcp: inline}}`
				}
			}
			limits := ""
			if kind == "limited" {
				limits = ", limits: {max_http_request_bytes: 4B}"
			}
			yaml += fmt.Sprintf("  http_%s:\n    executor: {type: http, source: api, method: POST, path: /%s, query: {count: {input: /payload/count}}, headers: {X-Input: {input: /payload/text}}, %s response: {%s}, effects: none%s}\n    input: {%s}\n    output: {%s}\n    policy: {rate_limit: {requests: 1000, per: 1s, burst: 1000}}\n", kind, kind, body, response, limits, input, output)
		}
		return yaml
	}, gateway.Config{MaxRequestBodyBytes: 64 << 10})
}
func httpInlineParams(files bool) string {
	input := map[string]any{"payload": map[string]any{"text": "<&> 日本語", "count": int64(9223372036854775807)}}
	if files {
		a := map[string]any{"name": "a.bin", "mime_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString([]byte{0, 255, 1})}
		b := map[string]any{"name": "b.json", "mime_type": "application/json", "data": base64.StdEncoding.EncodeToString([]byte(`{"x":1}`))}
		input["document"] = a
		input["documents"] = []any{a, b}
	}
	b, _ := json.Marshal(input)
	return string(b)
}
func httpDeliveryUpstream(t *testing.T) (*httptest.Server, chan httpRecorded) {
	records := make(chan httpRecorded, 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		records <- httpRecorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body}
		switch r.URL.Path {
		case "/array":
			io.WriteString(w, `[9223372036854775807,9007199254740993]`)
		case "/scalar":
			io.WriteString(w, `"reply"`)
		case "/null":
			io.WriteString(w, `null`)
		case "/bad":
			io.WriteString(w, `{"ok":true,"ok":false}`)
		case "/file_reference", "/file_direct", "/file_inline":
			w.Write([]byte{0, 255, 1, 2})
		default:
			io.WriteString(w, `{"ok":true,"count":9223372036854775807,"secret":"hidden"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server, records
}
func assertHTTPUpstream(t *testing.T, kind string, record httpRecorded, files bool) {
	t.Helper()
	if record.method != "POST" || record.path != "/"+kind || record.query != "count=9223372036854775807" || record.headers.Get("Authorization") != "Bearer private-http-token" || record.headers.Get("X-Input") != "<&> 日本語" {
		t.Fatal(record)
	}
	switch kind {
	case "raw":
		if !bytes.Equal(record.body, []byte{0, 255, 1}) {
			t.Fatal(record.body)
		}
	case "none":
		if len(record.body) != 0 || record.headers.Get("Content-Type") != "" {
			t.Fatal(record)
		}
	case "form":
		if string(record.body) != "count=9223372036854775807&tags=a+b&tags=%2B&text=%3C%26%3E+%E6%97%A5%E6%9C%AC%E8%AA%9E" {
			t.Fatal(string(record.body))
		}
	case "multipart":
		_, params, e := mime.ParseMediaType(record.headers.Get("Content-Type"))
		if e != nil {
			t.Fatal(e)
		}
		reader := multipart.NewReader(bytes.NewReader(record.body), params["boundary"])
		names := []string{"text", "json"}
		want := [][]byte{[]byte("<&> 日本語"), []byte(`"<&> 日本語"`)}
		mimes := []string{"text/plain; charset=utf-8", "application/json"}
		filenames := []string{"", ""}
		if files {
			names = append(names, "upload", "upload", "omitted")
			want = append(want, []byte{0, 255, 1}, []byte(`{"x":1}`), []byte{0, 255, 1})
			mimes = append(mimes, "application/custom", "application/custom", "application/octet-stream")
			filenames = append(filenames, "a.bin", "b.json", "a.bin")
		}
		for i, name := range names {
			part, e := reader.NextPart()
			if e != nil {
				t.Fatal(e)
			}
			body, e := io.ReadAll(part)
			if e != nil || part.FormName() != name || part.FileName() != filenames[i] || part.Header.Get("Content-Type") != mimes[i] || !bytes.Equal(body, want[i]) {
				t.Fatal(i, part.Header, body, e)
			}
		}
		if _, e = reader.NextPart(); e != io.EOF {
			t.Fatal("extra multipart", e)
		}
	default:
		if string(record.body) != `{"fixed":true,"payload":{"count":9223372036854775807,"text":"<&> 日本語"}}` {
			t.Fatal(string(record.body))
		}
	}
}
func TestHTTPExecutorProductionRESTMCPAndSchema(t *testing.T) {
	server, records := httpDeliveryUpstream(t)
	f := productionHTTPExecutor(t, server.URL)
	metadataResponse := f.runner.executeVersioned(f.ctx, protocol.Request{ID: "http-metadata", Capability: "meta"})
	var metadata protocol.Metadata
	if err := json.Unmarshal(metadataResponse.Result, &metadata); err != nil {
		t.Fatal(err)
	}
	for name, capability := range metadata.Capabilities {
		if strings.HasPrefix(name, "http_") && (metadata.ResponseKinds[name] != "generic" || capability.ExecutorKind != "http" || capability.EffectClass != "none") {
			t.Fatal(name, capability)
		}
	}
	select {
	case r := <-records:
		t.Fatal("prepare sent upstream", r)
	default:
	}
	req, _ := http.NewRequest("GET", f.base+"/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer input-key")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	document, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	validator := testschema.OpenAPI(t, document)
	for _, private := range []string{server.URL, "private-http-token", "credential", "allowed_cidrs"} {
		if bytes.Contains(document, []byte(private)) {
			t.Fatal("private metadata leaked", private)
		}
	}
	for _, kind := range []string{"json", "form", "multipart", "raw", "none", "array", "scalar", "null", "file_reference", "file_direct", "file_inline"} {
		files := kind == "raw" || kind == "multipart"
		t.Run("REST_"+kind, func(t *testing.T) {
			path := "/api/v1/capabilities/http_" + kind
			status, raw, headers := f.request(t, path, "application/json", strings.NewReader(httpInlineParams(files)), "")
			if status != 200 {
				t.Fatal(status, string(raw))
			}
			testschema.HTTPResponse(t, validator, f.base+path, headers, raw)
			assertHTTPUpstream(t, kind, <-records, files)
			if kind == "file_direct" {
				if !bytes.Equal(raw, []byte{0, 255, 1, 2}) {
					t.Fatal(raw)
				}
			} else {
				result := csvEnvelope(t, raw)["result"].(map[string]any)
				if strings.HasPrefix(kind, "file_") {
					descriptor := result["files"].([]any)[0].(map[string]any)
					if result["data"] != nil || descriptor["name"] != "reply.bin" || descriptor["mime_type"] != "application/custom" || !bytes.Equal(csvGet(t, f, descriptor["uri"].(string)), []byte{0, 255, 1, 2}) {
						t.Fatal(result)
					}
				} else if kind == "array" {
					v := result["data"].([]any)
					if len(v) != 2 || v[0] != json.Number("9223372036854775807") || v[1] != json.Number("9007199254740993") {
						t.Fatal(v)
					}
				} else if kind == "scalar" {
					if result["data"] != "reply" {
						t.Fatal(result)
					}
				} else if kind == "null" {
					if result["data"] != nil {
						t.Fatal(result)
					}
				} else {
					data := result["data"].(map[string]any)
					if len(data) != 2 || data["ok"] != true || data["count"] != json.Number("9223372036854775807") {
						t.Fatal(data)
					}
				}
			}
		})
		for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
			t.Run("MCP_"+kind+"_"+version, func(t *testing.T) {
				status, raw, _ := f.request(t, "/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"http_`+kind+`","arguments":`+httpInlineParams(files)+`}}`), version)
				rpc := csvEnvelope(t, raw)
				result, ok := rpc["result"].(map[string]any)
				if status != 200 || !ok || result["isError"] == true {
					t.Fatal(status, string(raw))
				}
				assertHTTPUpstream(t, kind, <-records, files)
				if version != "2025-03-26" {
					envelope := result["structuredContent"].(map[string]any)
					snapshot, _ := f.runner.versioned.Snapshot("http_" + kind)
					testschema.JSON(t, snapshot.Definition().SuccessSchema("mcp"), envelope, true)
					if kind == "file_inline" {
						content := result["content"].([]any)
						found := false
						for _, part := range content {
							resource, ok := part.(map[string]any)["resource"].(map[string]any)
							if ok {
								data, _ := base64.StdEncoding.DecodeString(resource["blob"].(string))
								found = bytes.Equal(data, []byte{0, 255, 1, 2})
							}
						}
						if !found {
							t.Fatal(result)
						}
					}
				}
			})
		}
	}

	// Optional file positions absent still use the common begin/commit barrier.
	statusOptional, rawOptional, _ := f.request(t, "/api/v1/capabilities/http_multipart", "application/json", strings.NewReader(httpInlineParams(false)), "")
	if statusOptional != 200 {
		t.Fatal(statusOptional, string(rawOptional))
	}
	assertHTTPUpstream(t, "multipart", <-records, false)
	// An explicit zero-byte raw file remains one file with its declared MIME.
	zeroParams := strings.Replace(httpInlineParams(true), `"data":"AP8B"`, `"data":""`, 1)
	statusZero, rawZero, _ := f.request(t, "/api/v1/capabilities/http_raw", "application/json", strings.NewReader(zeroParams), "")
	if statusZero != 200 {
		t.Fatal(statusZero, string(rawZero))
	}
	zero := <-records
	if len(zero.body) != 0 || zero.headers.Get("Content-Type") != "application/octet-stream" {
		t.Fatal(zero)
	}
	// REST multipart ingress uses the same FileValues as inline and MCP.
	var wire bytes.Buffer
	writer := multipart.NewWriter(&wire)
	part, _ := writer.CreateFormFile("document", "a.bin")
	part.Write([]byte{0, 255, 1})
	// CreateFormFile supplies application/octet-stream, as required by ingress.
	header := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="payload"`}, "Content-Type": {"application/json"}}
	part, _ = writer.CreatePart(header)
	io.WriteString(part, `{"text":"<&> 日本語","count":9223372036854775807}`)
	writer.Close()
	status, raw, _ := f.request(t, "/api/v1/capabilities/http_raw", writer.FormDataContentType(), bytes.NewReader(wire.Bytes()), "")
	if status != 200 {
		t.Fatal(status, string(raw))
	}
	assertHTTPUpstream(t, "raw", <-records, true)
	until := time.Now().Add(time.Second)
	for f.runner.store.Usage() != 48 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if f.runner.store.Usage() != 48 {
		t.Fatal("HTTP/input scratch remains beyond the 12 retained four-byte outputs", f.runner.store.Usage())
	}
}
func TestHTTPExecutorProductionRejectsWithoutSendAndSafeFailure(t *testing.T) {
	server, records := httpDeliveryUpstream(t)
	f := productionHTTPExecutor(t, server.URL)
	for _, c := range []struct {
		name, params  string
		status        int
		code, outcome string
	}{
		{"form", `{"payload":{"text":"x","count":1,"optional":null}}`, 400, "VALIDATION_FAILED", "not_started"},
		{"raw", `{"payload":{"text":"x","count":1}}`, 400, "VALIDATION_FAILED", "not_started"},
		{"limited", httpInlineParams(false), 422, "LIMIT_EXCEEDED", "not_started"},
	} {
		status, raw, _ := f.request(t, "/api/v1/capabilities/http_"+c.name, "application/json", strings.NewReader(c.params), "")
		envelope := csvEnvelope(t, raw)
		failure := envelope["error"].(map[string]any)
		if status != c.status || failure["code"] != c.code || failure["outcome"] != c.outcome {
			t.Fatal(status, envelope)
		}
		select {
		case record := <-records:
			t.Fatal("invalid sent upstream", record)
		default:
		}
	}
	status, raw, _ := f.request(t, "/api/v1/capabilities/http_bad", "application/json", strings.NewReader(httpInlineParams(false)), "")
	failure := csvEnvelope(t, raw)["error"].(map[string]any)
	if status != 502 || failure["code"] != "OUTPUT_INVALID" || failure["outcome"] != "unknown" {
		t.Fatal(status, string(raw))
	}
	<-records
	for _, private := range []string{server.URL, "private-http-token", "hidden", "Authorization"} {
		if bytes.Contains(raw, []byte(private)) || bytes.Contains([]byte(f.logs.String()), []byte(private)) {
			t.Fatal("public diagnostics leaked", private)
		}
	}
}
