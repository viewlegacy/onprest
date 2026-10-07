package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

type httpFixture struct {
	store      *artifacts.Store
	controller *ExecutionController
	cap        *contract.Capability
	provider   *httpProvider
}

func newHTTPFixture(t *testing.T, address, executor, input, output, policy string, quota int64) *httpFixture {
	return newHTTPMethodFixture(t, address, "POST", executor, input, output, policy, quota)
}
func newHTTPMethodFixture(t *testing.T, address, method, executor, input, output, policy string, quota int64) *httpFixture {
	t.Helper()
	dir := t.TempDir()
	yaml := fmt.Sprintf("schema_version: 1\ngateway: {url: wss://gateway.example/ws/agent, agent_private_key: %s}\nsources: {http: {api: {base_url: %q, allowed_methods: [GET, POST], allowed_cidrs: [127.0.0.0/8], credential: private-token, headers: {X-Fixed: fixed}}}}\ncapabilities:\n  call:\n    executor: {type: http, source: api, method: %s, path: /submit%s}\n%s%s%s", testAgentPrivateKey, address, method, executor, input, output, policy)
	cfg, err := contract.Compile([]byte(yaml))
	if err != nil {
		t.Fatal(err, yaml)
	}
	f := &httpFixture{cap: cfg.Capabilities["call"]}
	registry, _ := providers.NewRegistry(map[string]providers.Factory{"http": func(ctx context.Context, s *providers.Snapshot) (providers.Provider, error) {
		p, e := prepareHTTPProvider(ctx, s)
		if e == nil {
			f.provider = p.(*httpProvider)
		}
		return p, e
	}})
	prepared, err := registry.Prepare(context.Background(), cfg, filepath.Join(dir, "agent.yaml"), providers.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.store, err = artifacts.Open(artifacts.Options{Directory: filepath.Join(dir, "spool"), QuotaBytes: quota, MaxObjects: 128, MaxPending: 8, TransferTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.controller, err = NewExecutionController(prepared, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
		if err := f.store.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}
func (f *httpFixture) run(ctx context.Context, input map[string]any) (*contract.PreparedOutput, *contract.ErrorValue) {
	return f.controller.Execute(context.WithValue(ctx, artifactStoreKey{}, f.store), ExecutionRequest{ID: "http-call", Capability: "call", Params: input, stager: f.store.Scratch()})
}

type httpRecorded struct {
	method, path, query string
	headers             http.Header
	body                []byte
}

func httpRecorder(t *testing.T) (*httptest.Server, chan httpRecorded) {
	t.Helper()
	records := make(chan httpRecorded, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
		}
		records <- httpRecorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body}
		io.WriteString(w, `{"ok":true,"private":"hidden"}`)
	}))
	t.Cleanup(server.Close)
	return server, records
}
func httpSuccess(t *testing.T, f *httpFixture, input map[string]any) *contract.PreparedOutput {
	t.Helper()
	out, failure := f.run(context.Background(), input)
	if failure != nil {
		t.Fatal(failure)
	}
	t.Cleanup(func() { out.Close() })
	return out
}
func TestHTTPExecutorCanonicalJSONQueryHeadersAndPresence(t *testing.T) {
	server, records := httpRecorder(t)
	input := "    input: {properties: {i: {type: integer}, n: {type: number}, b: {type: boolean}, text: {type: string}, tags: {items: {type: string}}, optional: {type: [string, 'null']}, nested: {properties: {value: {type: string}}}, fallback: {type: string, default: defaulted}}, required: [i,n,b,text,tags]}\n"
	executor := `, query: {z: {input: /i}, tags: {input: /tags}, missing: {input: /optional}, empty: {value: []}}, headers: {X-Number: {input: /i}, X-Flag: {input: /b}, X-Missing: {input: /optional}}, body: {kind: json, fields: {z: {input: /i}, n: {input: /n}, b: {input: /b}, a: {input: /text}, missing: {input: /optional}, nested: {input: /nested/value}, fallback: {input: /fallback}}}, response: {kind: json}`
	f := newHTTPFixture(t, server.URL+"/base/", executor, input, "    output: {properties: {ok: {type: boolean}}, data: {}}\n", "", 1<<20)
	out := httpSuccess(t, f, map[string]any{"i": int64(9223372036854775807), "n": math.Copysign(0, -1), "b": true, "text": "<&> 日本語", "tags": []any{"a b", "+&"}})
	record := <-records
	want := `{"a":"<&> 日本語","b":true,"fallback":"defaulted","n":0,"z":9223372036854775807}`
	if string(record.body) != want || record.path != "/submit" || record.query != "tags=a+b&tags=%2B%26&z=9223372036854775807" || record.headers.Get("X-Number") != "9223372036854775807" || record.headers.Get("X-Flag") != "true" || record.headers.Get("X-Missing") != "" || record.headers.Get("Authorization") != "Bearer private-token" || record.headers.Get("X-Fixed") != "fixed" || record.headers.Get("Content-Type") != "application/json" {
		t.Fatal(record, string(record.body))
	}
	if got := out.Data.(map[string]any); len(got) != 1 || got["ok"] != true {
		t.Fatal(out.Data)
	}
	out.Close()
	if f.store.Usage() != 0 {
		t.Fatal("quota retained", f.store.Usage())
	}
}
func TestHTTPExecutorBodyVariantsAndRootPointer(t *testing.T) {
	cases := []struct{ name, body, input, want, media string }{
		{"root", "{kind: json, input: ''}", `{"x":9223372036854775807}`, `{"x":9223372036854775807}`, "application/json"},
		{"whole array", "{kind: json, input: /x}", `{"x":[null,true,1e+20]}`, `[null,true,1e+20]`, "application/json"},
		{"fixed null", "{kind: json, value: null}", `{}`, `null`, "application/json"},
		{"fixed string", "{kind: json, value: '<&>'}", `{}`, `"<&>"`, "application/json"},
		{"form", "{kind: form, fields: {z: {value: true}, tags: {value: ['a b', '+&']}, blank: {value: ''}, absent: {input: /optional}, n: {value: 1e20}}}", `{}`, `blank=&n=1e%2B20&tags=a+b&tags=%2B%26&z=true`, "application/x-www-form-urlencoded"},
		{"none", "", `{}`, "", ""},
		{"empty form", "{kind: form, fields: {}}", `{}`, "", "application/x-www-form-urlencoded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server, records := httpRecorder(t)
			body := ""
			if c.body != "" {
				body = ", body: " + c.body
			}
			schema := "    input: {properties: {x: {}, optional: {type: string}}, required: []}\n"
			if c.name == "whole array" {
				schema = "    input: {properties: {x: {}}, required: [x]}\n"
			}
			f := newHTTPFixture(t, server.URL, body+", response: {kind: json}", schema, "    output: {data: {}}\n", "", 1<<20)
			parsed, e := contract.DecodeJSON([]byte(c.input), contract.Limits{MaxBytes: 1 << 20, MaxDepth: 32, MaxItems: 1000})
			if e != nil {
				t.Fatal(e)
			}
			httpSuccess(t, f, parsed.(map[string]any))
			r := <-records
			if string(r.body) != c.want || r.headers.Get("Content-Type") != c.media {
				t.Fatal(string(r.body), r.headers)
			}
		})
	}
}
func TestHTTPExecutorMultipartCanonicalFixture(t *testing.T) {
	server, records := httpRecorder(t)
	f := newHTTPFixture(t, server.URL, `, body: {kind: multipart, parts: [{name: text, kind: text, value: '<&>'}, {name: json, kind: json, value: '<&>'}, {name: integer, kind: text, value: 9223372036854775807}, {name: canonical, kind: json, value: {z: 9223372036854775807, a: '<&>', b: true, n: 1e20}}, {name: null_value, kind: json, value: null}, {name: number, kind: text, value: 1e20}, {name: flag, kind: text, value: false}, {name: omitted, kind: text, input: /optional}]}, response: {kind: json}`, "    input: {properties: {optional: {type: string}}}\n", "    output: {data: {}}\n", "", 1<<20)
	f.provider.boundary = "fixture-boundary"
	httpSuccess(t, f, map[string]any{})
	r := <-records
	want := "--fixture-boundary\r\nContent-Disposition: form-data; name=text\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n<&>\r\n--fixture-boundary\r\nContent-Disposition: form-data; name=json\r\nContent-Type: application/json\r\n\r\n\"<&>\"\r\n--fixture-boundary\r\nContent-Disposition: form-data; name=integer\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n9223372036854775807\r\n--fixture-boundary\r\nContent-Disposition: form-data; name=canonical\r\nContent-Type: application/json\r\n\r\n{\"a\":\"<&>\",\"b\":true,\"n\":1e+20,\"z\":9223372036854775807}\r\n--fixture-boundary\r\nContent-Disposition: form-data; name=null_value\r\nContent-Type: application/json\r\n\r\nnull\r\n--fixture-boundary\r\nContent-Disposition: form-data; name=number\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n1e+20\r\n--fixture-boundary\r\nContent-Disposition: form-data; name=flag\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nfalse\r\n--fixture-boundary--\r\n"
	if string(r.body) != want {
		t.Fatalf("byte fixture mismatch: %q", r.body)
	}
}
func TestHTTPExecutorSendBeforeValidationAndLimits(t *testing.T) {
	var sent atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); io.WriteString(w, `{}`) }))
	defer server.Close()
	cases := []struct {
		name, executor, input string
		values                map[string]any
		quota                 int64
	}{
		{"null query", `, query: {x: {input: /x}}`, "    input: {properties: {x: {type: [string, 'null']}}}\n", map[string]any{"x": nil}, 1024},
		{"null header", `, headers: {X-Test: {input: /x}}`, "    input: {properties: {x: {type: [string, 'null']}}}\n", map[string]any{"x": nil}, 1024},
		{"null form", `, body: {kind: form, fields: {x: {input: /x}}}`, "    input: {properties: {x: {type: [string, 'null']}}}\n", map[string]any{"x": nil}, 1024},
		{"null part", `, body: {kind: multipart, parts: [{name: x, kind: text, input: /x}]}`, "    input: {properties: {x: {type: [string, 'null']}}}\n", map[string]any{"x": nil}, 1024},
		{"CRLF", `, headers: {X-Test: {input: /x}}`, "    input: {properties: {x: {type: string}}}\n", map[string]any{"x": "bad\r\nheader"}, 1024},
		{"request limit", `, body: {kind: json, input: /x}, limits: {max_http_request_bytes: 6B}`, "    input: {properties: {x: {type: string}}, required: [x]}\n", map[string]any{"x": "12345"}, 1024},
		{"quota", `, body: {kind: json, value: '12345'}`, "", map[string]any{}, 6},
		{"control limit", `, headers: {X-Test: {input: /x}}`, "    input: {properties: {x: {type: string}}}\n", map[string]any{"x": strings.Repeat("x", 65536)}, 1 << 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := sent.Load()
			f := newHTTPFixture(t, server.URL, c.executor+", response: {kind: json}", c.input, "    output: {data: {}}\n", "", c.quota)
			out, failure := f.run(context.Background(), c.values)
			if out != nil || failure == nil || failure.Outcome != "not_started" || sent.Load() != before || f.store.Usage() != 0 {
				t.Fatal(out, failure, sent.Load(), f.store.Usage())
			}
			want := "VALIDATION_FAILED"
			if c.name == "request limit" || c.name == "quota" {
				want = "LIMIT_EXCEEDED"
			}
			if failure.Code != want {
				t.Fatal(failure)
			}
		})
	}
}
func TestHTTPExecutorRedirectPreservesConfiguredMethodAndBytes(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := make(chan httpRecorded, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				calls <- httpRecorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body}
				if r.URL.Path == "/submit" {
					w.Header().Set("Location", "/finish")
					w.WriteHeader(status)
				} else {
					io.WriteString(w, `true`)
				}
			}))
			defer server.Close()
			f := newHTTPFixture(t, server.URL, `, body: {kind: json, value: {x: '<&>'}}, response: {kind: json, redirect: {enabled: true, max_hops: 1}}`, "", "    output: {data: {}}\n", "", 1<<20)
			out := httpSuccess(t, f, map[string]any{})
			if out.Data != true {
				t.Fatal(out.Data)
			}
			a, b := <-calls, <-calls
			if a.method != "POST" || b.method != "POST" || string(a.body) != `{"x":"<&>"}` || !bytes.Equal(a.body, b.body) || b.headers.Get("Authorization") != "Bearer private-token" {
				t.Fatal(a, b)
			}
			out.Close()
			if f.store.Usage() != 0 {
				t.Fatal(f.store.Usage())
			}
		})
	}
}
func TestHTTPExecutorResponseFailureAndCleanup(t *testing.T) {
	cases := []struct{ name, raw, settings, code string }{
		{"status", "private body", ", success_statuses: [201]", "EXECUTION_FAILED"},
		{"duplicate", `{"x":1,"x":2}`, "", "OUTPUT_INVALID"},
		{"trailing", `{} {}`, "", "OUTPUT_INVALID"},
		{"invalid utf8", string([]byte{'"', 255, '"'}), "", "OUTPUT_INVALID"},
		{"response limit", `"12345"`, "", "LIMIT_EXCEEDED"},
		{"output type", `"wrong"`, "", "OUTPUT_INVALID"},
		{"partial", `{"ok":true}`, "", "DEPENDENCY_UNAVAILABLE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.name == "partial" {
					w.Header().Set("Content-Length", "100")
				}
				io.WriteString(w, c.raw)
			}))
			defer server.Close()
			limits := ""
			if c.name == "response limit" {
				limits = ", limits: {max_http_response_bytes: 6B}"
			}
			output := "    output: {data: {}}\n"
			if c.name == "output type" {
				output = "    output: {type: boolean, data: {}}\n"
			}
			f := newHTTPFixture(t, server.URL, limits+", response: {kind: json"+c.settings+"}", "", output, "", 1<<20)
			out, failure := f.run(context.Background(), map[string]any{})
			if out != nil || failure == nil || failure.Code != c.code || failure.Outcome != "unknown" || f.store.Usage() != 0 {
				t.Fatal(out, failure, f.store.Usage())
			}
			if c.name == "status" && failure.Reason != "UPSTREAM_HTTP_STATUS" {
				t.Fatal(failure)
			}
			raw, _ := json.Marshal(failure)
			if bytes.Contains(raw, []byte("private")) || bytes.Contains(raw, []byte(server.URL)) {
				t.Fatal(string(raw))
			}
		})
	}
}
func TestHTTPExecutorCancellationDuringResponse(t *testing.T) {
	entered := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	f := newHTTPFixture(t, server.URL, ", response: {kind: json}", "", "    output: {data: {}}\n", "", 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *contract.ErrorValue, 1)
	go func() { _, failure := f.run(ctx, map[string]any{}); done <- failure }()
	<-entered
	cancel()
	select {
	case failure := <-done:
		if failure == nil || failure.Code != "EXECUTION_TIMEOUT" || failure.Outcome != "unknown" {
			t.Fatal(failure)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel blocked")
	}
	if f.store.Usage() != 0 {
		t.Fatal(f.store.Usage())
	}
}

func TestHTTPExecutorRedirectRejectionStatusAndNoRetry(t *testing.T) {
	for _, mode := range []string{"disabled", "foreign", "hop", "disconnect", "TLS"} {
		t.Run(mode, func(t *testing.T) {
			var sent atomic.Int64
			foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("foreign credential/request escaped") }))
			defer foreign.Close()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent.Add(1)
				if mode == "disconnect" {
					connection, _, _ := w.(http.Hijacker).Hijack()
					connection.Close()
					return
				}
				location := "/again"
				if mode == "foreign" {
					location = foreign.URL + "/leak"
				}
				w.Header().Set("Location", location)
				w.WriteHeader(302)
			})
			server := httptest.NewServer(handler)
			if mode == "TLS" {
				server.Close()
				server = httptest.NewTLSServer(handler)
			}
			defer server.Close()
			redirect := ""
			if mode == "foreign" || mode == "hop" {
				redirect = ", redirect: {enabled: true, max_hops: 1}"
			}
			f := newHTTPFixture(t, server.URL, ", response: {kind: json"+redirect+"}", "", "    output: {data: {}}\n", "", 1<<20)
			out, failure := f.run(context.Background(), map[string]any{})
			want := int64(1)
			if mode == "hop" {
				want = 2
			}
			if mode == "TLS" {
				want = 0
			}
			if out != nil || failure == nil || sent.Load() != want || f.store.Usage() != 0 {
				t.Fatal(out, failure, sent.Load(), f.store.Usage())
			}
			if mode == "TLS" {
				if failure.Outcome != "not_started" {
					t.Fatal(failure)
				}
			} else if failure.Outcome != "unknown" {
				t.Fatal(failure)
			}
		})
	}
}
func TestHTTPExecutorFileResponseWholeValidation(t *testing.T) {
	for _, mode := range []string{"empty", "original", "omitted", "partial", "limit"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "partial" {
					w.Header().Set("Content-Length", "100")
				}
				if mode != "empty" {
					w.Write([]byte{0, 255, 1})
				}
			}))
			defer server.Close()
			limits := ""
			if mode == "limit" {
				limits = ", limits: {max_http_response_bytes: 2B}"
			}
			output := "    output: {files: {}}\n"
			if mode == "omitted" {
				output = ""
			}
			f := newHTTPFixture(t, server.URL, limits+", response: {kind: file, filename: result.bin, mime_type: application/custom}", "", output, "", 1<<20)
			out, failure := f.run(context.Background(), map[string]any{})
			if mode == "partial" || mode == "limit" {
				want := "OUTPUT_INVALID"
				if mode == "limit" {
					want = "LIMIT_EXCEEDED"
				}
				if out != nil || failure == nil || failure.Code != want || failure.Outcome != "unknown" {
					t.Fatal(out, failure)
				}
			} else {
				if failure != nil {
					t.Fatal(failure)
				}
				if mode == "omitted" {
					if len(out.Files) != 0 || out.Data != nil {
						t.Fatal(out)
					}
				} else {
					if len(out.Files) != 1 || out.Files[0].MIMEType != "application/custom" || out.Files[0].Name != "result.bin" {
						t.Fatal(out)
					}
					raw, e := io.ReadAll(out.Files[0].Reader)
					want := []byte{0, 255, 1}
					if mode == "empty" {
						want = []byte{}
					}
					if e != nil || !bytes.Equal(raw, want) || out.Files[0].SizeBytes != int64(len(want)) {
						t.Fatal(raw, e)
					}
				}
				out.Close()
			}
			if f.store.Usage() != 0 {
				t.Fatal(f.store.Usage())
			}
		})
	}
}
func TestHTTPExecutorConcurrentPublication(t *testing.T) {
	server, records := httpRecorder(t)
	f := newHTTPFixture(t, server.URL, ", response: {kind: json}", "", "    output: {data: {}}\n", "", 1<<20)
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			out, failure := f.run(context.Background(), map[string]any{})
			if failure != nil {
				done <- fmt.Errorf("execution: %v", failure)
				return
			}
			done <- out.Close()
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		<-records
	}
	if f.store.Usage() != 0 {
		t.Fatal(f.store.Usage())
	}
}
func TestHTTPExecutorJSONNullOptionalAndPointerEscapes(t *testing.T) {
	server, records := httpRecorder(t)
	f := newHTTPFixture(t, server.URL, `, body: {kind: json, fields: {nullable: {input: /x}, nested: {input: /nested/value}, escaped: {input: '/a~1b/~0key'}, indexed: {input: /array/1}}}, response: {kind: json}`, "    input: {properties: {x: {type: [string, 'null']}, nested: {properties: {value: {type: string}}, type: [object, 'null']}, 'a/b': {properties: {'~key': {type: string}}}, array: {items: {type: integer}}}}\n", "    output: {data: {}}\n", "", 1<<20)
	httpSuccess(t, f, map[string]any{"x": nil, "nested": nil, "a/b": map[string]any{"~key": "v"}, "array": []any{int64(7), int64(9007199254740993)}})
	if got := string((<-records).body); got != `{"escaped":"v","indexed":9007199254740993,"nullable":null}` {
		t.Fatal(got)
	}
}

func TestHTTPExecutorOmittedBodyGETAndPOSTFraming(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			server, records := httpRecorder(t)
			f := newHTTPMethodFixture(t, server.URL, method, ", response: {kind: json}", "", "    output: {data: {}}\n", "", 1<<20)
			httpSuccess(t, f, map[string]any{})
			record := <-records
			if record.method != method || len(record.body) != 0 || record.headers.Get("Content-Type") != "" || record.headers.Get("Transfer-Encoding") != "" {
				t.Fatal(record)
			}
			if method == "GET" && record.headers.Get("Content-Length") != "" {
				t.Fatal(record.headers)
			}
			if method == "POST" && record.headers.Get("Content-Length") != "0" {
				t.Fatal(record.headers)
			}
		})
	}
}

func TestHTTPExecutorJSONGeneratedCSVUsesCommonSelection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":9223372036854775807,"value":7,"private":"hidden"}]`)
	}))
	defer server.Close()
	f := newHTTPFixture(t, server.URL, ", response: {kind: json}", "", "    output: {items: {properties: {id: {type: integer}, value: {type: integer}}}, data: {}, files: {format: csv, filename: result.csv}}\n", "", 1<<20)
	out := httpSuccess(t, f, map[string]any{})
	rows, ok := out.Data.([]any)
	if !ok || len(rows) != 1 || len(rows[0].(map[string]any)) != 2 || rows[0].(map[string]any)["id"] != int64(9223372036854775807) || len(out.Files) != 1 {
		t.Fatal(out)
	}
	raw, err := io.ReadAll(out.Files[0].Reader)
	if err != nil || string(raw) != "id,value\r\n9223372036854775807,7\r\n" {
		t.Fatal(string(raw), err)
	}
	out.Close()
	if f.store.Usage() != 0 {
		t.Fatal(f.store.Usage())
	}
}

func TestHTTPExecutorExplicitSuccessStatuses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `true`)
	}))
	defer server.Close()
	f := newHTTPFixture(t, server.URL, ", response: {kind: json, success_statuses: [201]}", "", "    output: {type: boolean, data: {}}\n", "", 1<<20)
	out := httpSuccess(t, f, map[string]any{})
	if out.Data != true {
		t.Fatal(out.Data)
	}
	out.Close()
	if f.store.Usage() != 0 {
		t.Fatal(f.store.Usage())
	}
}
