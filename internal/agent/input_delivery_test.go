//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/gateway"
	"github.com/viewlegacy/onprest/internal/testschema"
	"golang.org/x/crypto/bcrypt"
)

type inputHTTPFixture struct {
	base, dir  string
	runner     *Runner
	ctx        context.Context
	logs       *transportLogBuffer
	stopRunner context.CancelFunc
}

func productionInputHTTP(t *testing.T, overrides ...gateway.Config) *inputHTTPFixture {
	return productionInputHTTPWithRoute(t, nil, overrides...)
}
func productionInputHTTPWithRoute(t *testing.T, route func(string) string, overrides ...gateway.Config) *inputHTTPFixture {
	return productionInputHTTPAdjusted(t, route, nil, overrides...)
}
func productionInputHTTPAdjusted(t *testing.T, route func(string) string, adjust func(string) string, overrides ...gateway.Config) *inputHTTPFixture {
	t.Helper()
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "exchange"), 0700)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := listener.Addr().String()
	listener.Close()
	private, _ := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
	public := ed25519.PrivateKey(private).Public().(ed25519.PublicKey)
	hash, _ := bcrypt.GenerateFromPassword([]byte("input-key"), bcrypt.MinCost)
	logs := &transportLogBuffer{}
	base := "http://" + addr
	config := gateway.Config{Addr: addr, PublicBaseURL: base, AgentPublicKey: base64.RawURLEncoding.EncodeToString(public), APIKeys: []gateway.APIKey{{Name: "caller", KeyHash: string(hash), Capabilities: []string{"*"}}}, MaxRequestBodyBytes: 1024, MaxFileRequestBodyBytes: 2 << 20, MaxConcurrentBodyReaders: 2, AgentPingInterval: 20 * time.Millisecond, AgentPongTimeout: time.Second}
	if len(overrides) > 0 {
		if overrides[0].MaxConcurrentBodyReaders > 0 {
			config.MaxConcurrentBodyReaders = overrides[0].MaxConcurrentBodyReaders
		}
		if overrides[0].MaxRequestBodyBytes > 0 {
			config.MaxRequestBodyBytes = overrides[0].MaxRequestBodyBytes
		}
		if overrides[0].MaxFileRequestBodyBytes > 0 {
			config.MaxFileRequestBodyBytes = overrides[0].MaxFileRequestBodyBytes
		}
	}
	server := gateway.NewServer(config, logs)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ListenAndServeContext(ctx) }()
	agentAddr := addr
	if route != nil {
		agentAddr = route(addr)
	}
	yaml := fmt.Sprintf(`schema_version: 1
runtime: {transfer_timeout: 2s, input_limits: {max_pending_requests: 2, max_multipart_parts: 4}}
gateway: {url: ws://%s/ws/agent, agent_private_key: %s}
sources: {folders: {exchange: {root: ./exchange, access: read_write}}}
capabilities:
  save:
    executor: {type: file, write: [{input: /document, source: exchange, name: saved.bin, overwrite: true}], read: {written: true}}
    input:
      properties: {document: {type: file, extensions: [bin], mime_types: [application/octet-stream, application/json], max_bytes: 1MiB}, number: {type: integer}, label: {type: string}, nested: {properties: {file: {type: file}}}}
      required: [document]
    output: {files: {}}
  array:
    executor: {type: file, write: [{input: /documents, source: exchange, name_from: input, overwrite: true}], read: {written: true}}
    input: {properties: {documents: {items: {type: file, max_bytes: 1MiB}, minItems: 1, maxItems: 2}}, required: [documents]}
    output: {files: {}}
  zero:
    executor: {type: file, read: {source: exchange, selector: {name: saved.bin}}}
    input: {properties: {documents: {items: {type: file}, maxItems: 0}}, required: [documents]}
    output: {files: {}}
  optional:
    executor: {type: file, read: {source: exchange, selector: {name: saved.bin}}}
    input: {properties: {document: {type: file}}}
    output: {files: {}}
  limited_count:
    executor: {type: file, write: [{input: /documents, source: exchange, name_from: input}], read: {written: true}}
    input: {properties: {documents: {items: {type: file, max_bytes: 4B}, minItems: 1, maxItems: 2}}, required: [documents]}
    policy: {max_input_files: 1, max_input_file_bytes: 4B, max_total_input_file_bytes: 8B}
    output: {files: {}}
  limited_total:
    executor: {type: file, write: [{input: /documents, source: exchange, name_from: input}], read: {written: true}}
    input: {properties: {documents: {items: {type: file, max_bytes: 4B}, minItems: 1, maxItems: 2}}, required: [documents]}
    policy: {max_input_files: 2, max_input_file_bytes: 4B, max_total_input_file_bytes: 4B}
    output: {files: {}}
  postfailure:
    executor: {type: file, write: [{input: /document, source: exchange, name: sideeffect.bin}], read: {source: exchange, selector: {name: nonexistent.bin}}}
    input: {properties: {document: {type: file, max_bytes: 2B}}, required: [document]}
    output: {files: {}}
  nested:
    executor: {type: file, write: [{input: /nested/file, source: exchange, name_from: input, overwrite: true}], read: {written: true}}
    input: {properties: {nested: {properties: {file: {type: file, max_bytes: 1MiB}}, required: [file]}}, required: [nested]}
    output: {files: {}}
`, agentAddr, testAgentPrivateKey)
	path := filepath.Join(dir, "agent.yaml")
	if adjust != nil {
		yaml = adjust(yaml)
	}
	os.WriteFile(path, []byte(yaml), 0600)
	runner, e := NewRunner(ctx, Config{CapabilityFile: path, ReconnectEvery: 20 * time.Millisecond}, io.Discard)
	if e != nil {
		cancel()
		<-serverDone
		t.Fatal(e)
	}
	runnerDone := make(chan error, 1)
	runnerCtx, stopRunner := context.WithCancel(ctx)
	go func() { runnerDone <- runner.Run(runnerCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runnerDone:
		case <-time.After(5 * time.Second):
			t.Error("runner shutdown")
		}
		select {
		case <-serverDone:
		case <-time.After(7 * time.Second):
			t.Error("gateway shutdown")
		}
	})
	f := &inputHTTPFixture{base: base, dir: dir, runner: runner, ctx: ctx, logs: logs, stopRunner: stopRunner}
	until := time.Now().Add(3 * time.Second)
	for {
		req, _ := http.NewRequest("GET", base+"/openapi.json", nil)
		req.Header.Set("Authorization", "Bearer input-key")
		resp, e := http.DefaultClient.Do(req)
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(until) {
			t.Fatal("metadata", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f
}
func (f *inputHTTPFixture) request(t *testing.T, path, media string, body io.Reader, version string) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequestWithContext(f.ctx, "POST", f.base+path, body)
	req.Header.Set("Authorization", "Bearer input-key")
	req.Header.Set("Content-Type", media)
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	return resp.StatusCode, raw, resp.Header
}
func inlineInput(body []byte) string {
	return `{"document":{"name":"report.bin","mime_type":"application/octet-stream","data":"` + base64.StdEncoding.EncodeToString(body) + `"},"number":9223372036854775807}`
}
func multipartInput(t *testing.T, body []byte, tail string) (string, []byte) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	head := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="document"; filename="report.bin"`}, "Content-Type": {"application/octet-stream"}}
	part, e := writer.CreatePart(head)
	if e != nil {
		t.Fatal(e)
	}
	part.Write(body)
	head = textproto.MIMEHeader{"Content-Disposition": {`form-data; name="number"`}, "Content-Type": {"application/json"}}
	part, _ = writer.CreatePart(head)
	io.WriteString(part, `9223372036854775807`)
	writer.Close()
	return writer.FormDataContentType(), append(buffer.Bytes(), []byte(tail)...)
}
func (f *inputHTTPFixture) assertSaved(t *testing.T, raw []byte, want []byte) {
	t.Helper()
	var envelope contract.SuccessEnvelope
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Result.Files) != 1 {
		t.Fatal(string(raw))
	}
	file, e := os.ReadFile(filepath.Join(f.dir, "exchange", "saved.bin"))
	if e != nil || !bytes.Equal(file, want) {
		t.Fatal("saved", e, len(file))
	}
	req, _ := http.NewRequest("GET", envelope.Result.Files[0].URI, nil)
	req.Header.Set("Authorization", "Bearer input-key")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	received, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(received, want) {
		t.Fatal("returned", resp.StatusCode, len(received))
	}
}
func TestInputDeliveryProductionRESTAndMCP(t *testing.T) {
	f := productionInputHTTP(t)
	body := []byte{0, 255, 1, 2, 3}
	req, _ := http.NewRequest("GET", f.base+"/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer input-key")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	doc, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	validator := testschema.OpenAPI(t, doc)
	for _, empty := range []bool{false, true} {
		input := body
		if empty {
			input = []byte{}
		}
		status, raw, h := f.request(t, "/api/v1/capabilities/save", "application/json", strings.NewReader(inlineInput(input)), "")
		if status != 200 {
			t.Fatalf("inline: %d %s", status, raw)
		}
		f.assertSaved(t, raw, input)
		testschema.HTTPResponse(t, validator, f.base+"/api/v1/capabilities/save", h, raw)
		media, wire := multipartInput(t, input, "")
		status, raw, _ = f.request(t, "/api/v1/capabilities/save", media, bytes.NewReader(wire), "")
		if status != 200 {
			t.Fatalf("multipart: %d %s", status, raw)
		}
		f.assertSaved(t, raw, input)
	}
	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
		status, raw, _ := f.request(t, "/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"save","arguments":`+inlineInput(body)+`}}`), version)
		var rpc struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if status != 200 || json.Unmarshal(raw, &rpc) != nil || rpc.Result.IsError || len(rpc.Result.Content) == 0 {
			t.Fatalf("mcp %s: %d %s", version, status, raw)
		}
		f.assertSaved(t, []byte(rpc.Result.Content[0].Text), body)
	}
	status, raw, _ := f.request(t, "/api/v1/capabilities/optional", "application/json", strings.NewReader(`{}`), "")
	if status != 200 {
		t.Fatalf("optional0: %d %s", status, raw)
	}
	if !strings.Contains(f.logs.String(), `"input_file_bytes":5`) || !strings.Contains(f.logs.String(), `"input_file_count":1`) {
		t.Fatal("confirmed input metrics", f.logs.String())
	}
	if strings.Contains(f.logs.String(), "report.bin") || strings.Contains(f.logs.String(), base64.StdEncoding.EncodeToString(body)) {
		t.Fatal("input leaked to access log")
	}
}
func TestInputDeliveryLateInvalidNeverExecutes(t *testing.T) {
	f := productionInputHTTP(t)
	large := bytes.Repeat([]byte{5}, 128<<10)
	for _, tail := range []string{"epilogue", ""} {
		media, wire := multipartInput(t, large, tail)
		if tail == "" {
			wire = wire[:len(wire)-10]
		}
		status, raw, _ := f.request(t, "/api/v1/capabilities/save", media, bytes.NewReader(wire), "")
		if status != 400 {
			t.Fatalf("late invalid: %d %s", status, raw)
		}
		if _, e := os.Stat(filepath.Join(f.dir, "exchange", "saved.bin")); !os.IsNotExist(e) {
			t.Fatal("business write before EOF")
		}
		waitTransport(t, func() bool { return f.runner.store.Usage() == 0 })
	}
	var wire bytes.Buffer
	writer := multipart.NewWriter(&wire)
	part, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="document"; filename="a.bin"`}, "Content-Type": {"application/octet-stream"}})
	part.Write(large)
	part, _ = writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="number"`}, "Content-Type": {"application/json"}})
	io.WriteString(part, `{broken`)
	writer.Close()
	status, raw, _ := f.request(t, "/api/v1/capabilities/save", writer.FormDataContentType(), bytes.NewReader(wire.Bytes()), "")
	if status != 400 {
		t.Fatal(status, string(raw))
	}
	if _, e := os.Stat(filepath.Join(f.dir, "exchange", "saved.bin")); !os.IsNotExist(e) {
		t.Fatal("last invalid JSON executed")
	}
	waitTransport(t, func() bool { return f.runner.store.Usage() == 0 })
}
func TestInputDeliveryMultipartTransfersBeforeEOFAndCancel(t *testing.T) {
	f := productionInputHTTP(t)
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(f.ctx)
	req, _ := http.NewRequestWithContext(ctx, "POST", f.base+"/api/v1/capabilities/save", pr)
	req.Header.Set("Authorization", "Bearer input-key")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	go func() {
		resp, e := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- e
	}()
	part, e := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="document"; filename="report.bin"`}, "Content-Type": {"application/octet-stream"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = part.Write(bytes.Repeat([]byte{5}, 256<<10)); e != nil {
		t.Fatal(e)
	}
	waitTransport(t, func() bool { return f.runner.store.Usage() > 0 })
	if _, e = os.Stat(filepath.Join(f.dir, "exchange", "saved.bin")); !os.IsNotExist(e) {
		t.Fatal("executed before EOF")
	}
	cancel()
	pw.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP cancel blocked")
	}
	waitTransport(t, func() bool { return f.runner.store.Usage() == 0 })
}

func TestInputDeliveryRejectsWireSchemaAndLimits(t *testing.T) {
	f := productionInputHTTP(t)
	for _, test := range []struct {
		name, input string
		status      int
	}{{"missing", `{}`, 400}, {"unknown", `{"x":1}`, 400}, {"fraction", strings.Replace(inlineInput([]byte{1}), "9223372036854775807", "1.5", 1), 400}, {"overflow", strings.Replace(inlineInput([]byte{1}), "9223372036854775807", "9223372036854775808", 1), 400}, {"noncanonical", strings.Replace(inlineInput([]byte{1}), "AQ==", "AR==", 1), 400}, {"unknownfilekey", strings.Replace(inlineInput([]byte{1}), `"data":`, `"path":"/private", "data":`, 1), 400}, {"duplicatedJSON", `{"document":null,"document":null}`, 400}, {"trailingJSON", inlineInput([]byte{1}) + `{}`, 400}, {"encodedwire", inlineInput(bytes.Repeat([]byte{1}, 1024)), 413}} {
		t.Run(test.name, func(t *testing.T) {
			status, raw, _ := f.request(t, "/api/v1/capabilities/save", "application/json", strings.NewReader(test.input), "")
			if status != test.status {
				t.Fatalf("%d %s", status, raw)
			}
		})
	}
	for _, test := range []struct {
		name, header, body, tail string
		status                   int
	}{{"rawpath", `Content-Disposition: form-data; name="document"; filename="../a.bin"` + "\r\nContent-Type: application/octet-stream", "x", "", 400}, {"duplicateheader", `Content-Disposition: form-data; name="document"; filename="a.bin"` + "\r\nContent-Type: application/octet-stream\r\nContent-Type: application/octet-stream", "x", "", 400}, {"missingfilename", `Content-Disposition: form-data; name="document"` + "\r\nContent-Type: application/octet-stream", "x", "", 400}, {"encoding", `Content-Disposition: form-data; name="document"; filename="a.bin"` + "\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64", "eA==", "", 400}, {"unknownpart", `Content-Disposition: form-data; name="unknown"` + "\r\nContent-Type: application/json", "1", "", 400}, {"fileJSON", `Content-Disposition: form-data; name="document"` + "\r\nContent-Type: application/json", `{"name":"a.bin","mime_type":"application/octet-stream","data":"AQ=="}`, "", 400}, {"extension", `Content-Disposition: form-data; name="document"; filename="a.txt"` + "\r\nContent-Type: application/octet-stream", "x", "", 400}, {"mime", `Content-Disposition: form-data; name="document"; filename="a.bin"` + "\r\nContent-Type: text/plain", "x", "", 400}, {"headersize", `Content-Disposition: form-data; name="document"; filename="a.bin"` + "\r\nContent-Type: application/octet-stream\r\nX-Long: " + strings.Repeat("a", 8192), "x", "", 413}, {"leafbytes", `Content-Disposition: form-data; name="document"; filename="a.bin"` + "\r\nContent-Type: application/octet-stream", strings.Repeat("x", 1<<20+1), "", 422}} {
		t.Run(test.name, func(t *testing.T) {
			wire := "--b\r\n" + test.header + "\r\n\r\n" + test.body + "\r\n--b--\r\n" + test.tail
			status, raw, _ := f.request(t, "/api/v1/capabilities/save", "multipart/form-data; boundary=b", strings.NewReader(wire), "")
			if status != test.status {
				t.Fatalf("%d %s", status, raw)
			}
			waitTransport(t, func() bool { return f.runner.store.Usage() == 0 })
		})
	}
	status, raw, _ := f.request(t, "/api/v1/capabilities/zero", "multipart/form-data; boundary=b", strings.NewReader("--b--\r\n"), "")
	if status != 415 {
		t.Fatalf("required maxItems0 multipart %d %s logs=%s", status, raw, f.logs.String())
	}
	if _, e := os.Stat(filepath.Join(f.dir, "exchange", "saved.bin")); !os.IsNotExist(e) {
		t.Fatal("invalid input executed business writes")
	}
	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
		status, raw, _ = f.request(t, "/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"save","arguments":{}}}`), version)
		if status != 200 || !bytes.Contains(raw, []byte(`"isError":true`)) || !bytes.Contains(raw, []byte(`VALIDATION_FAILED`)) {
			t.Fatal(status, string(raw))
		}
	}
}
func TestInputDeliveryJSONPartCumulativeBoundary(t *testing.T) {
	f := productionInputHTTP(t)
	for _, delta := range []int{-1, 0, 1} {
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		for _, part := range []struct{ name, media, filename, body string }{{"document", "application/octet-stream", "a.bin", "x"}, {"number", "application/json", "", "9223372036854775807"}, {"label", "application/json", "", `"` + strings.Repeat("a", 1003+delta) + `"`}} {
			disposition := `form-data; name="` + part.name + `"`
			if part.filename != "" {
				disposition += `; filename="` + part.filename + `"`
			}
			p, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {disposition}, "Content-Type": {part.media}})
			io.WriteString(p, part.body)
		}
		writer.Close()
		status, raw, _ := f.request(t, "/api/v1/capabilities/save", writer.FormDataContentType(), bytes.NewReader(buffer.Bytes()), "")
		want := 200
		if delta == 1 {
			want = 413
		}
		if status != want {
			t.Fatalf("delta %d: %d %s", delta, status, raw)
		}
	}
}
func TestInputDeliveryMultipartWireLimit(t *testing.T) {
	f := productionInputHTTP(t, gateway.Config{MaxFileRequestBodyBytes: 64 << 10})
	media, wire := multipartInput(t, bytes.Repeat([]byte{3}, 64<<10), "")
	status, raw, _ := f.request(t, "/api/v1/capabilities/save", media, bytes.NewReader(wire), "")
	if status != 413 {
		t.Fatal(status, string(raw))
	}
	waitTransport(t, func() bool { return f.runner.store.Usage() == 0 })
}
func TestInputDeliveryRepeatedFileOrderAndNestedInline(t *testing.T) {
	f := productionInputHTTP(t)
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for _, name := range []string{"b.bin", "a.bin"} {
		p, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="documents"; filename="` + name + `"`}, "Content-Type": {"application/octet-stream"}})
		io.WriteString(p, name)
	}
	writer.Close()
	status, raw, _ := f.request(t, "/api/v1/capabilities/array", writer.FormDataContentType(), bytes.NewReader(buffer.Bytes()), "")
	var envelope contract.SuccessEnvelope
	if status != 200 || json.Unmarshal(raw, &envelope) != nil || len(envelope.Result.Files) != 2 || envelope.Result.Files[0].Name != "b.bin" || envelope.Result.Files[1].Name != "a.bin" {
		t.Fatal(status, string(raw), f.logs.String())
	}
	body := strings.TrimSuffix(inlineInput([]byte{2}), "}") + `,"nested":{"file":{"name":"x.bin","mime_type":"application/octet-stream","data":"Aw=="}}}`
	status, raw, _ = f.request(t, "/api/v1/capabilities/save", "application/json", strings.NewReader(body), "")
	if status != 200 {
		t.Fatal(status, string(raw))
	}
}

func TestInputDeliveryPinnedBrowserAndGeneratedClient(t *testing.T) {
	if os.Getenv("ONPREST_IT_INPUT_CLIENT_WIRE") != "1" {
		t.Skip("set ONPREST_IT_INPUT_CLIENT_WIRE=1 with pinned dev dependencies and ONPREST_IT_INPUT_CLIENT_GENERATOR_JAR")
	}
	f := productionInputHTTP(t)
	script, e := filepath.Abs("../gateway/testdata/input-clients/wire.mjs")
	if e != nil {
		t.Fatal(e)
	}
	command := exec.CommandContext(f.ctx, "node", script, f.base)
	out, e := command.CombinedOutput()
	if e != nil {
		t.Fatalf("real client wire: %v\n%s", e, out)
	}
	t.Log(string(out))
}

func TestInputDeliveryMultipartPartCountAndBinaryJSON(t *testing.T) {
	f := productionInputHTTP(t)
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	for _, part := range []struct{ name, body, media, filename string }{{"document", "{this is binary, not JSON", "application/json", "a.bin"}, {"number", "3", "application/json", ""}, {"label", `"x"`, "application/json", ""}, {"nested", "{}", "application/json", ""}, {"label", `"duplicate"`, "application/json", ""}} {
		header := `form-data; name="` + part.name + `"`
		if part.filename != "" {
			header += `; filename="` + part.filename + `"`
		}
		p, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {header}, "Content-Type": {part.media}})
		io.WriteString(p, part.body)
	}
	writer.Close()
	status, raw, _ := f.request(t, "/api/v1/capabilities/save", writer.FormDataContentType(), bytes.NewReader(b.Bytes()), "")
	if status != 422 {
		t.Fatal(status, string(raw))
	}
	if _, e := os.Stat(filepath.Join(f.dir, "exchange", "saved.bin")); !os.IsNotExist(e) {
		t.Fatal("part overflow executed")
	}
	wire := "--b\r\nContent-Disposition: form-data; name=\"document\"; filename=\"a.bin\"\r\nContent-Type: application/json\r\n\r\n{binary is not JSON\r\n--b--\r\n"
	status, raw, _ = f.request(t, "/api/v1/capabilities/save", "multipart/form-data; boundary=b", strings.NewReader(wire), "")
	if status != 200 {
		t.Fatal(status, string(raw))
	}
	f.assertSaved(t, raw, []byte("{binary is not JSON"))
}

func TestInputDeliveryAgentBusyBeforeMultipartBodyRead(t *testing.T) {
	f := productionInputHTTP(t, gateway.Config{MaxConcurrentBodyReaders: 3})
	var cancels []context.CancelFunc
	var writers []*io.PipeWriter
	responses := make(chan int, 3)
	for range 2 {
		pr, pw := io.Pipe()
		writers = append(writers, pw)
		writer := multipart.NewWriter(pw)
		ctx, cancel := context.WithCancel(f.ctx)
		cancels = append(cancels, cancel)
		req, _ := http.NewRequestWithContext(ctx, "POST", f.base+"/api/v1/capabilities/save", pr)
		req.Header.Set("Authorization", "Bearer input-key")
		req.Header.Set("Content-Type", writer.FormDataContentType())
		go func() {
			resp, e := http.DefaultClient.Do(req)
			if e != nil {
				responses <- 0
				return
			}
			resp.Body.Close()
			responses <- resp.StatusCode
		}()
		part, e := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="document"; filename="a.bin"`}, "Content-Type": {"application/octet-stream"}})
		if e != nil {
			t.Fatal(e)
		}
		part.Write(bytes.Repeat([]byte{5}, 128<<10))
	}
	waitTransport(t, func() bool { return f.runner.store.Usage() >= 128<<10 })
	pr, pw := io.Pipe()
	defer pw.Close()
	req, _ := http.NewRequestWithContext(f.ctx, "POST", f.base+"/api/v1/capabilities/save", pr)
	req.Header.Set("Authorization", "Bearer input-key")
	req.Header.Set("Content-Type", "multipart/form-data; boundary=no-body-sent")
	go func() {
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			responses <- 0
			return
		}
		defer resp.Body.Close()
		responses <- resp.StatusCode
	}()
	select {
	case status := <-responses:
		if status != 503 {
			t.Fatal("Agent admission", status)
		}
	case <-time.After(time.Second):
		t.Fatal("Agent BUSY read body or waited")
	}
	for _, cancel := range cancels {
		cancel()
	}
	for _, writer := range writers {
		writer.Close()
	}
	waitTransport(t, func() bool { return f.runner.store.Usage() == 0 })
}

func TestInputDeliveryAgentFileCountAndTotalLimits(t *testing.T) {
	f := productionInputHTTP(t)
	for _, test := range []struct{ capability, body string }{{"limited_count", "x"}, {"limited_total", "xxx"}} {
		t.Run(test.capability, func(t *testing.T) {
			var wire bytes.Buffer
			writer := multipart.NewWriter(&wire)
			for _, name := range []string{"a.bin", "b.bin"} {
				p, e := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="documents"; filename="` + name + `"`}, "Content-Type": {"application/octet-stream"}})
				if e != nil {
					t.Fatal(e)
				}
				io.WriteString(p, test.body)
			}
			writer.Close()
			status, raw, _ := f.request(t, "/api/v1/capabilities/"+test.capability, writer.FormDataContentType(), bytes.NewReader(wire.Bytes()), "")
			if status != 422 || !bytes.Contains(raw, []byte("LIMIT_EXCEEDED")) {
				t.Fatal(status, string(raw))
			}
			waitTransport(t, func() bool { return f.runner.store.Usage() == 0 })
			entries, e := os.ReadDir(filepath.Join(f.dir, "exchange"))
			if e != nil || len(entries) != 0 {
				t.Fatal("business execution on rejected input", entries, e)
			}
		})
	}
	if !strings.Contains(f.logs.String(), `"input_file_count":1`) || !strings.Contains(f.logs.String(), `"input_file_bytes":1`) {
		t.Fatal("confirmed rejection metrics", f.logs.String())
	}
}

func TestInputDeliveryOfflineBeforeMultipartBodyRead(t *testing.T) {
	f := productionInputHTTP(t)
	f.stopRunner()
	until := time.Now().Add(3 * time.Second)
	for {
		req, _ := http.NewRequest("GET", f.base+"/openapi.json", nil)
		req.Header.Set("Authorization", "Bearer input-key")
		resp, e := http.DefaultClient.Do(req)
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 503 {
				break
			}
		}
		if time.Now().After(until) {
			t.Fatal("Agent disconnect was not observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithTimeout(f.ctx, time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", f.base+"/api/v1/capabilities/save", pr)
	req.Header.Set("Authorization", "Bearer input-key")
	req.Header.Set("Content-Type", "multipart/form-data; boundary=never-sent")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal("offline rejection waited for body", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal(resp.StatusCode)
	}
}

func TestInputDeliveryFailureAfterSavePreservesOutcomeAndMetrics(t *testing.T) {
	f := productionInputHTTP(t)
	status, raw, _ := f.request(t, "/api/v1/capabilities/postfailure", "application/json", strings.NewReader(`{"document":{"name":"a.bin","mime_type":"application/octet-stream","data":"AQ=="}}`), "")
	if status != 404 || !bytes.Contains(raw, []byte(`"outcome":"committed"`)) {
		t.Fatal(status, string(raw))
	}
	saved, e := os.ReadFile(filepath.Join(f.dir, "exchange", "sideeffect.bin"))
	if e != nil || !bytes.Equal(saved, []byte{1}) {
		t.Fatal(saved, e)
	}
	waitTransport(t, func() bool { return f.runner.store.Usage() == 0 })
	if !strings.Contains(f.logs.String(), `"input_file_count":1`) || !strings.Contains(f.logs.String(), `"input_file_bytes":1`) {
		t.Fatal("failure metrics", f.logs.String())
	}
}

func TestInputDeliveryDecodedLeafAndHeaderBoundaries(t *testing.T) {
	f := productionInputHTTP(t)
	var retained int64
	for _, delta := range []int{-1, 0, 1} {
		body := bytes.Repeat([]byte{4}, (1<<20)+delta)
		media, wire := multipartInput(t, body, "")
		status, raw, _ := f.request(t, "/api/v1/capabilities/save", media, bytes.NewReader(wire), "")
		want := 200
		if delta > 0 {
			want = 422
		}
		if status != want {
			t.Fatal("leaf", delta, status, string(raw))
		}
		if delta <= 0 {
			f.assertSaved(t, raw, body)
			retained += int64(len(body))
		}
		waitTransport(t, func() bool { return f.runner.store.Usage() == retained })
	}
	for _, delta := range []int{-1, 0, 1} {
		header := `Content-Disposition: form-data; name="document"; filename="a.bin"` + "\r\nContent-Type: application/octet-stream\r\nX-Pad: "
		wire := "--b\r\n" + header + strings.Repeat("x", 8192+delta-len(header)-4) + "\r\n\r\nx\r\n--b--\r\n"
		status, raw, _ := f.request(t, "/api/v1/capabilities/save", "multipart/form-data; boundary=b", strings.NewReader(wire), "")
		want := 200
		if delta > 0 {
			want = 413
		}
		if status != want {
			t.Fatal("header", delta, status, string(raw))
		}
		if delta <= 0 {
			retained++
		}
		waitTransport(t, func() bool { return f.runner.store.Usage() == retained })
	}
}

func TestInputDeliveryAuthenticationBeforeUnreadBody(t *testing.T) {
	f := productionInputHTTP(t)
	for _, test := range []struct {
		token, path string
		status      int
	}{{"", "/api/v1/capabilities/save", 401}, {"invalid", "/mcp", 401}, {"input-key", "/api/v1/capabilities/undefined", 404}} {
		pr, pw := io.Pipe()
		ctx, cancel := context.WithTimeout(f.ctx, time.Second)
		req, _ := http.NewRequestWithContext(ctx, "POST", f.base+test.path, pr)
		req.Header.Set("Content-Type", "multipart/form-data; boundary=never-sent")
		if test.token != "" {
			req.Header.Set("Authorization", "Bearer "+test.token)
		}
		resp, e := http.DefaultClient.Do(req)
		pw.Close()
		cancel()
		if e != nil {
			t.Fatal("early refusal read input", e)
		}
		resp.Body.Close()
		if resp.StatusCode != test.status {
			t.Fatal(resp.StatusCode, test.status)
		}
	}
	if f.runner.store.Usage() != 0 {
		t.Fatal("unauthorized request admitted input")
	}
}
