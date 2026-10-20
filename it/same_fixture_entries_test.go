//go:build integration

package it

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/testschema"
)

// sameFixtureFile is one published file reduced to what every entry must agree on.
type sameFixtureFile struct {
	contract.ArtifactMetadata
	Body []byte
}

// sameFixtureResult is one call reduced to the public contract shared by REST,
// multipart, every MCP version and the author CLI.
type sameFixtureResult struct {
	Data  string
	Files []sameFixtureFile
	Error *contract.ErrorValue
}

const sameFixtureMonth = "2026-08"

var sameFixtureCSV = []byte("id,month,amount\r\na1,2026-08,100\r\na2,2026-09,5\r\na3,2026-08,9007199254740993\r\n")

// One input fixture crosses every executor that accepts it, every public entry
// and the author CLI through two real binaries. Each executor must observe the
// same bytes and metadata, and each entry must publish the same result.
func TestSameFixtureAcrossExecutorsEntriesAndCLI(t *testing.T) {
	db := postgresContainerConfig(t)
	repo, tmp := repoRoot(t), t.TempDir()
	gatewayBin, agentBin := filepath.Join(tmp, "onprest-gateway"), filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	base := "http://" + addr
	sum := sha256.Sum256(sameFixtureCSV)
	fixtureDigest := hex.EncodeToString(sum[:])

	// The upstream records exactly what the HTTP executor sent.
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path == "/fail" {
			http.Error(w, "PRIVATE_UPSTREAM_FAILURE", http.StatusInternalServerError)
			return
		}
		media, params, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if e != nil || media != "multipart/form-data" {
			http.Error(w, "media", http.StatusBadRequest)
			return
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		observed := map[string]any{}
		for {
			part, e := reader.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				http.Error(w, "part", http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(part)
			switch part.FormName() {
			case "month":
				observed["month"] = string(body)
			case "document":
				digest := sha256.Sum256(body)
				observed["name"], observed["mime_type"] = part.FileName(), part.Header.Get("Content-Type")
				observed["size_bytes"], observed["sha256"] = len(body), hex.EncodeToString(digest[:])
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(observed)
	}))
	defer upstream.Close()

	// The Wasm capability is an ordinary author module built by the shipped CLI.
	author := filepath.Join(tmp, "author")
	if e := os.Mkdir(author, 0o700); e != nil {
		t.Fatal(e)
	}
	writeFile(t, filepath.Join(author, "go.mod"), "module same-fixture\n\ngo 1.26.9\n\nrequire github.com/viewlegacy/onprest/sdk/go v1.0.0\n\nreplace github.com/viewlegacy/onprest/sdk/go => "+filepath.Join(repo, "sdk/go")+"\n")
	writeFile(t, filepath.Join(author, "main.go"), `package main

import (
	"context"

	onprest "github.com/viewlegacy/onprest/sdk/go"
)

type Params struct {
	Month    string            `+"`json:\"month\"`"+`
	Document onprest.InputFile `+"`json:\"document\"`"+`
}

func main() {
	onprest.Run(func(ctx context.Context, in Params) (onprest.Result, error) {
		reader, err := in.Document.Open(ctx)
		if err != nil {
			return onprest.Result{}, err
		}
		return onprest.Result{
			Data:  map[string]any{"month": in.Month, "name": in.Document.Name(), "mime_type": in.Document.MIMEType(), "size_bytes": in.Document.Size(), "sha256": in.Document.SHA256()},
			Files: []onprest.File{{Name: in.Document.Name(), MIMEType: in.Document.MIMEType(), Reader: reader}},
		}, nil
	})
}
`)
	build := exec.Command(agentBin, "code", "build", "--source", author, "--output", filepath.Join(tmp, "inspect.wasm"), "--offline")
	if raw, e := build.CombinedOutput(); e != nil {
		t.Fatalf("author build: %v %s", e, raw)
	}
	for _, dir := range []string{"exchange", "cli-input", "cli-output"} {
		if e := os.Mkdir(filepath.Join(tmp, dir), 0o700); e != nil {
			t.Fatal(e)
		}
	}
	writeFile(t, filepath.Join(tmp, "exchange", "taken.csv"), "existing")

	fileInput := "    input:\n      properties:\n        month: {type: string}\n        document: {type: file, extensions: [csv]}\n      required: [month, document]\n"
	config := fmt.Sprintf(`schema_version: 1
service: {title: Same fixture, version: "1"}
gateway: {url: "ws://%s/ws/agent", agent_private_key: "%s"}
sources:
  databases:
    primary: {driver: postgres, host: "%s", port: %s, name: "%s", user: "%s", password: "%s"}
  folders:
    exchange: {root: ./exchange, access: read_write}
  http:
    upstream: {base_url: "%s", allowed_methods: [POST], allowed_cidrs: [127.0.0.0/8]}
capabilities:
  db_month:
    executor: {type: db, source: primary, sql: "select :month::text as month"}
    input: {properties: {month: {type: string}}, required: [month]}
    output: {data: {}}
  csv_upload:
    executor:
      type: csv
      read: {input: /document}
      csv: {columns: {amount: {type: integer}}}
      where: [{column: month, op: eq, input: /month}]
%s    output:
      data: {}
      items:
        properties:
          id: {type: string}
          amount: {type: integer}
      files: {format: csv, filename: selected.csv}
  file_save:
    executor:
      type: file
      write: [{input: /document, source: exchange, name_from: input, overwrite: true}]
      read: {written: true}
%s    output: {files: {}}
  file_conflict:
    executor:
      type: file
      write: [{input: /document, source: exchange, name: taken.csv}]
%s  http_forward:
    executor:
      type: http
      source: upstream
      method: POST
      path: /forward
      body:
        kind: multipart
        parts:
          - {name: month, kind: text, input: /month}
          - {name: document, kind: file, input: /document}
      response: {kind: json}
%s    output: {data: {}}
  http_failure:
    executor:
      type: http
      source: upstream
      method: POST
      path: /fail
      body: {kind: file, input: /document}
      response: {kind: json}
%s    output: {data: {}}
  wasm_inspect:
    executor: {type: wasm, module: ./inspect.wasm}
%s    output: {data: {}, files: {}}
    policy: {timeout: 30s}
  file_direct:
    executor:
      type: file
      write: [{input: /document, source: exchange, name: direct.csv, overwrite: true}]
      read: {written: true}
%s    output: {files: {delivery: {rest: direct}}}
  wasm_direct:
    executor: {type: wasm, module: ./inspect.wasm}
%s    output: {files: {delivery: {rest: direct}}}
    policy: {timeout: 30s}
`, addr, secrets.AgentPrivateKey, db.Host, db.Port, db.Name, db.User, db.Password, upstream.URL, fileInput, fileInput, fileInput, fileInput, fileInput, fileInput, fileInput, fileInput)
	configPath := filepath.Join(tmp, "agent.yaml")
	writeFile(t, configPath, config)
	if raw, e := exec.Command(agentBin, "validate", "--config", configPath).CombinedOutput(); e != nil {
		t.Fatalf("validate: %v %s", e, raw)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	gateway := startProcess(t, ctx, gatewayBin, nil, []string{"GATEWAY_ADDR=" + addr, "GATEWAY_AGENT_PUBLIC_KEY=" + secrets.AgentPublicKey, "GATEWAY_API_KEYS_JSON=" + secrets.APIKeysJSON, "ONPREST_PUBLIC_BASE_URL=" + base, "GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=1000", "GATEWAY_RATE_LIMIT_BURST=1000"})
	defer stopProcess(t, gateway)
	waitForHTTP(t, base+"/healthz", "", 200)
	agent := startProcessInDir(t, ctx, tmp, agentBin, []string{"--config", configPath}, nil)
	defer stopProcess(t, agent)
	waitForAgentConnected(t, base)
	waitForHTTP(t, base+"/openapi.json", secrets.APIKey, 200)

	do := func(method, url, contentType string, body []byte, header map[string]string) (int, http.Header, []byte) {
		t.Helper()
		request, e := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		request.Header.Set("Authorization", "Bearer "+secrets.APIKey)
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		for k, v := range header {
			request.Header.Set(k, v)
		}
		response, e := http.DefaultClient.Do(request)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		raw, e := io.ReadAll(response.Body)
		if e != nil {
			t.Fatal(e)
		}
		return response.StatusCode, response.Header, raw
	}
	_, _, openAPIRaw := do("GET", base+"/openapi.json", "", nil, nil)
	published := testschema.OpenAPI(t, openAPIRaw)
	var openAPI map[string]any
	if e := json.Unmarshal(openAPIRaw, &openAPI); e != nil {
		t.Fatal(e)
	}
	tools := map[string]map[string]any{}
	for _, version := range []string{"2025-06-18", "2025-11-25"} {
		var listed struct {
			Result struct {
				Tools []map[string]any `json:"tools"`
			} `json:"result"`
		}
		if e := json.Unmarshal(postMCPPayloadWithProtocol(t, base, secrets.APIKey, version, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`), &listed); e != nil {
			t.Fatal(e)
		}
		for _, tool := range listed.Result.Tools {
			tools[version+"/"+tool["name"].(string)] = tool
		}
	}
	requestSchema := func(capability string) any {
		operation := openAPI["paths"].(map[string]any)["/api/v1/capabilities/"+capability].(map[string]any)["post"].(map[string]any)
		return operation["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	}
	canonicalData := func(raw json.RawMessage) string {
		t.Helper()
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if e := decoder.Decode(&value); e != nil {
			t.Fatal(e, string(raw))
		}
		out, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		return string(out)
	}
	// Each published reference is fetched both by HTTP GET and, for MCP
	// versions with resource support, by resources/read.
	fetch := func(uri, version string) []byte {
		t.Helper()
		status, _, body := do("GET", uri, "", nil, nil)
		if status != 200 {
			t.Fatalf("GET %s: %d %s", uri, status, body)
		}
		if version == "2025-06-18" || version == "2025-11-25" {
			read, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "resources/read", "params": map[string]any{"uri": uri}})
			var resource struct {
				Result struct {
					Contents []struct{ Text, Blob string } `json:"contents"`
				} `json:"result"`
			}
			if e := json.Unmarshal(postMCPPayloadWithProtocol(t, base, secrets.APIKey, version, string(read)), &resource); e != nil || len(resource.Result.Contents) != 1 {
				t.Fatal("resources/read", e)
			}
			viaMCP := []byte(resource.Result.Contents[0].Text)
			if resource.Result.Contents[0].Blob != "" {
				viaMCP, _ = base64.StdEncoding.DecodeString(resource.Result.Contents[0].Blob)
			}
			if !bytes.Equal(viaMCP, body) {
				t.Fatalf("resources/read differs from GET: %q %q", viaMCP, body)
			}
		}
		return body
	}
	reduce := func(entry string, raw []byte) sameFixtureResult {
		t.Helper()
		if bytes.Contains(raw, []byte("PRIVATE_")) || bytes.Contains(raw, []byte(tmp)) || bytes.Contains(raw, []byte("file_id")) {
			t.Fatalf("%s exposed private detail: %s", entry, raw)
		}
		var envelope struct {
			Result *struct {
				Data  json.RawMessage                 `json:"data"`
				Files []contract.PublicFileDescriptor `json:"files"`
			} `json:"result"`
			Error *contract.ErrorValue `json:"error"`
		}
		if e := json.Unmarshal(raw, &envelope); e != nil {
			t.Fatal(entry, e, string(raw))
		}
		if envelope.Error != nil {
			if envelope.Result != nil || envelope.Error.Validate() != nil {
				t.Fatalf("%s invalid error envelope: %s", entry, raw)
			}
			return sameFixtureResult{Error: envelope.Error}
		}
		out := sameFixtureResult{Data: canonicalData(envelope.Result.Data), Files: []sameFixtureFile{}}
		version, _ := strings.CutPrefix(entry, "MCP ")
		for _, file := range envelope.Result.Files {
			out.Files = append(out.Files, sameFixtureFile{file.ArtifactMetadata, fetch(file.URI, version)})
		}
		return out
	}
	inline := func(name, mimeType string, body []byte) map[string]any {
		return map[string]any{"name": name, "mime_type": mimeType, "data": base64.StdEncoding.EncodeToString(body)}
	}
	restJSON := func(capability string, params map[string]any) sameFixtureResult {
		t.Helper()
		body, _ := json.Marshal(params)
		testschema.JSON(t, requestSchema(capability), params, true)
		status, header, raw := do("POST", base+"/api/v1/capabilities/"+capability, "application/json", body, nil)
		if status == 200 {
			testschema.HTTPResponse(t, published, base+"/api/v1/capabilities/"+capability, header, raw)
		}
		return reduce("REST JSON", raw)
	}
	restMultipart := func(capability, month, name, mimeType string, content []byte) sameFixtureResult {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		monthPart, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="month"`}, "Content-Type": {"application/json"}})
		json.NewEncoder(monthPart).Encode(month)
		filePart, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="document"; filename="` + name + `"`}, "Content-Type": {mimeType}})
		filePart.Write(content)
		writer.Close()
		status, header, raw := do("POST", base+"/api/v1/capabilities/"+capability, writer.FormDataContentType(), body.Bytes(), nil)
		if status == 200 {
			testschema.HTTPResponse(t, published, base+"/api/v1/capabilities/"+capability, header, raw)
		}
		return reduce("REST multipart", raw)
	}
	mcpCall := func(version, capability string, arguments map[string]any) sameFixtureResult {
		t.Helper()
		call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": capability, "arguments": arguments}})
		var rpc struct {
			Result struct {
				IsError           bool                          `json:"isError"`
				Content           []struct{ Type, Text string } `json:"content"`
				StructuredContent json.RawMessage               `json:"structuredContent"`
			} `json:"result"`
		}
		raw := postMCPPayloadWithProtocol(t, base, secrets.APIKey, version, string(call))
		if e := json.Unmarshal(raw, &rpc); e != nil || len(rpc.Result.Content) == 0 || rpc.Result.Content[0].Type != "text" {
			t.Fatal(version, e, string(raw))
		}
		text := reduce("MCP "+version, []byte(rpc.Result.Content[0].Text))
		if (text.Error != nil) != rpc.Result.IsError {
			t.Fatalf("MCP %s isError does not match envelope: %s", version, raw)
		}
		if tool, ok := tools[version+"/"+capability]; ok {
			testschema.JSON(t, tool["inputSchema"], arguments, true)
			if len(rpc.Result.StructuredContent) == 0 || !bytes.Equal([]byte(canonicalData(rpc.Result.StructuredContent)), []byte(canonicalData(json.RawMessage(rpc.Result.Content[0].Text)))) {
				t.Fatalf("MCP %s structuredContent differs from text: %s", version, raw)
			}
			if !rpc.Result.IsError {
				var structured any
				decoder := json.NewDecoder(bytes.NewReader(rpc.Result.StructuredContent))
				decoder.UseNumber()
				decoder.Decode(&structured)
				testschema.JSON(t, tool["outputSchema"], structured, true)
			}
		} else if len(rpc.Result.StructuredContent) != 0 {
			t.Fatalf("legacy MCP %s returned structuredContent: %s", version, raw)
		}
		return text
	}
	cli := func(capability, month, path string) sameFixtureResult {
		t.Helper()
		params, _ := json.Marshal(map[string]any{"month": month, "document": nil})
		output, e := os.MkdirTemp(filepath.Join(tmp, "cli-output"), "run")
		if e != nil {
			t.Fatal(e)
		}
		command := exec.CommandContext(ctx, agentBin, "code", "test", "--config", configPath, "--capability", capability, "--params", string(params), "--input-file", "/document="+path, "--format", "json", "--output-dir", output, "--log-level", "error")
		command.Dir = tmp
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		runErr := command.Run()
		var result struct {
			OK    bool            `json:"ok"`
			Data  json.RawMessage `json:"data"`
			Files []struct {
				contract.ArtifactMetadata
				SavedPath string `json:"saved_path"`
			} `json:"files"`
			Error *contract.ErrorValue `json:"error"`
		}
		if e := json.Unmarshal(stdout.Bytes(), &result); e != nil {
			t.Fatal("CLI stdout", e, stdout.String(), stderr.String())
		}
		if (runErr == nil) != result.OK || bytes.Contains(stdout.Bytes(), []byte(sameFixtureCSV[:16])) {
			t.Fatalf("CLI exit/body mismatch: %v %s", runErr, stdout.String())
		}
		if result.Error != nil {
			return sameFixtureResult{Error: result.Error}
		}
		out := sameFixtureResult{Data: canonicalData(result.Data), Files: []sameFixtureFile{}}
		for _, file := range result.Files {
			saved, e := os.ReadFile(file.SavedPath)
			if e != nil {
				t.Fatal(e)
			}
			out.Files = append(out.Files, sameFixtureFile{file.ArtifactMetadata, saved})
		}
		return out
	}
	cliPath := func(name string, content []byte) string {
		dir, e := os.MkdirTemp(filepath.Join(tmp, "cli-input"), "file")
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(dir, name)
		if e := os.WriteFile(path, content, 0o600); e != nil {
			t.Fatal(e)
		}
		return path
	}
	// every collects one result per entry and requires them to be identical.
	every := func(t *testing.T, capability, name, mimeType string, content []byte, withCLI bool) sameFixtureResult {
		t.Helper()
		params := map[string]any{"month": sameFixtureMonth, "document": inline(name, mimeType, content)}
		results := map[string]sameFixtureResult{
			"REST JSON":      restJSON(capability, params),
			"REST multipart": restMultipart(capability, sameFixtureMonth, name, mimeType, content),
		}
		for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
			results["MCP "+version] = mcpCall(version, capability, params)
		}
		if withCLI {
			results["CLI code test"] = cli(capability, sameFixtureMonth, cliPath(name, content))
		}
		reference := results["REST JSON"]
		for entry, got := range results {
			if entry == "CLI code test" && got.Error != nil && reference.Error != nil {
				// The CLI validates in the agent without a gateway ingress, so only
				// the safe generic message wording may differ; the decision may not.
				local, remote := *got.Error, *reference.Error
				local.Message, remote.Message = "", ""
				got = sameFixtureResult{Error: &local}
				if !reflect.DeepEqual(got, sameFixtureResult{Error: &remote}) {
					t.Fatalf("%s CLI error decision differs: %#v %#v", capability, local, remote)
				}
				continue
			}
			if !reflect.DeepEqual(got, reference) {
				gotJSON, _ := json.Marshal(got)
				referenceJSON, _ := json.Marshal(reference)
				t.Fatalf("%s %s differs from REST JSON:\n%s\n%s", capability, entry, gotJSON, referenceJSON)
			}
		}
		return reference
	}

	document := contract.ArtifactMetadata{Name: "sales.csv", MIMEType: "text/csv", SizeBytes: int64(len(sameFixtureCSV)), SHA256: fixtureDigest}
	observed := fmt.Sprintf(`{"mime_type":"text/csv","month":%q,"name":"sales.csv","sha256":%q,"size_bytes":%d}`, sameFixtureMonth, fixtureDigest, len(sameFixtureCSV))

	t.Run("db", func(t *testing.T) {
		params := map[string]any{"month": sameFixtureMonth}
		reference := restJSON("db_month", params)
		for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
			if got := mcpCall(version, "db_month", params); !reflect.DeepEqual(got, reference) {
				t.Fatalf("MCP %s differs: %#v %#v", version, got, reference)
			}
		}
		if reference.Data != `{"count":1,"rows":[{"month":"2026-08"}]}` || len(reference.Files) != 0 {
			t.Fatalf("db result %#v", reference)
		}
	})
	t.Run("csv", func(t *testing.T) {
		got := every(t, "csv_upload", "sales.csv", "text/csv", sameFixtureCSV, false)
		if got.Data != `{"count":2,"rows":[{"amount":100,"id":"a1"},{"amount":9007199254740993,"id":"a3"}]}` || len(got.Files) != 1 {
			t.Fatalf("csv result %#v", got)
		}
		generated := []byte("id,amount\r\na1,100\r\na3,9007199254740993\r\n")
		digest := sha256.Sum256(generated)
		if want := (sameFixtureFile{contract.ArtifactMetadata{Name: "selected.csv", MIMEType: "text/csv", SizeBytes: int64(len(generated)), SHA256: hex.EncodeToString(digest[:])}, generated}); !reflect.DeepEqual(got.Files[0], want) {
			t.Fatalf("generated CSV %#v", got.Files[0])
		}
	})
	t.Run("file", func(t *testing.T) {
		got := every(t, "file_save", "sales.csv", "text/csv", sameFixtureCSV, false)
		if got.Data != "null" || len(got.Files) != 1 || !reflect.DeepEqual(got.Files[0], sameFixtureFile{document, sameFixtureCSV}) {
			t.Fatalf("file result %#v", got)
		}
		saved, e := os.ReadFile(filepath.Join(tmp, "exchange", "sales.csv"))
		if e != nil || !bytes.Equal(saved, sameFixtureCSV) {
			t.Fatal("saved bytes", e)
		}
	})
	t.Run("http", func(t *testing.T) {
		before := upstreamCalls.Load()
		got := every(t, "http_forward", "sales.csv", "text/csv", sameFixtureCSV, false)
		if got.Data != observed || len(got.Files) != 0 {
			t.Fatalf("http result %#v", got)
		}
		if calls := upstreamCalls.Load() - before; calls != 5 {
			t.Fatalf("upstream calls %d want one per entry", calls)
		}
	})
	t.Run("wasm", func(t *testing.T) {
		got := every(t, "wasm_inspect", "sales.csv", "text/csv", sameFixtureCSV, true)
		if got.Data != observed || len(got.Files) != 1 || !reflect.DeepEqual(got.Files[0], sameFixtureFile{document, sameFixtureCSV}) {
			t.Fatalf("wasm result %#v", got)
		}
	})
	// A direct file keeps the MIME type the call produced; the published schema
	// must accept it rather than claim a fixed octet-stream body.
	t.Run("direct_media", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"month": sameFixtureMonth, "document": inline("sales.csv", "text/csv", sameFixtureCSV)})
		for _, capability := range []string{"file_direct", "wasm_direct"} {
			status, header, raw := do("POST", base+"/api/v1/capabilities/"+capability, "application/json", body, nil)
			if status != 200 || !strings.HasPrefix(header.Get("Content-Type"), "text/csv") || !bytes.Equal(raw, sameFixtureCSV) {
				t.Fatalf("%s direct %d %v %q", capability, status, header, raw)
			}
			testschema.HTTPResponse(t, published, base+"/api/v1/capabilities/"+capability, header, raw)
			media := openAPI["paths"].(map[string]any)["/api/v1/capabilities/"+capability].(map[string]any)["post"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)
			if _, ok := media["*/*"]; !ok || len(media) != 1 {
				t.Fatalf("%s published direct media %v", capability, media)
			}
		}
	})
	// A rejected extension is the same pre-execution error on every entry and
	// for every executor; nothing reaches the upstream.
	t.Run("extension_error", func(t *testing.T) {
		before := upstreamCalls.Load()
		var reference *contract.ErrorValue
		for _, capability := range []string{"csv_upload", "file_save", "http_forward", "wasm_inspect"} {
			got := every(t, capability, "sales.txt", "text/csv", sameFixtureCSV, capability == "wasm_inspect")
			if got.Error == nil || got.Error.Code != "VALIDATION_FAILED" || got.Error.Outcome != "not_started" {
				t.Fatalf("%s extension error %#v", capability, got)
			}
			if reference == nil {
				reference = got.Error
			} else if *got.Error != *reference {
				t.Fatalf("%s error differs between executors: %#v %#v", capability, got.Error, reference)
			}
		}
		if upstreamCalls.Load() != before {
			t.Fatal("rejected input reached the upstream")
		}
		if _, e := os.Stat(filepath.Join(tmp, "exchange", "sales.txt")); !os.IsNotExist(e) {
			t.Fatal("rejected input was saved", e)
		}
	})
	// Executor failures are public codes without detail, while the agent's local
	// diagnostic log keeps one common record per failed request and executor.
	t.Run("executor_failures_local_log", func(t *testing.T) {
		failures := map[string]string{}
		for capability, content := range map[string][]byte{"csv_upload": []byte("id,month,amount\r\na1,2026-08,\"PRIVATE_BROKEN"), "file_conflict": sameFixtureCSV, "http_failure": sameFixtureCSV} {
			params := map[string]any{"month": sameFixtureMonth, "document": inline("sales.csv", "text/csv", content)}
			body, _ := json.Marshal(params)
			status, _, raw := do("POST", base+"/api/v1/capabilities/"+capability, "application/json", body, nil)
			var envelope contract.ErrorEnvelope
			if e := json.Unmarshal(raw, &envelope); e != nil || status < 400 || envelope.Error.Code == "" || bytes.Contains(raw, []byte("PRIVATE_")) {
				t.Fatalf("%s failure %d %s", capability, status, raw)
			}
			failures[envelope.RequestID] = capability
		}
		existing, e := os.ReadFile(filepath.Join(tmp, "exchange", "taken.csv"))
		if e != nil || string(existing) != "existing" {
			t.Fatal("conflicting write changed the existing file", e)
		}
		kinds := map[string]string{"csv_upload": "csv", "file_conflict": "file", "http_failure": "http"}
		deadline := time.Now().Add(10 * time.Second)
		for {
			matches, _ := filepath.Glob(filepath.Join(tmp, "onprest-agent.log*"))
			found := map[string]bool{}
			for _, path := range matches {
				raw, _ := os.ReadFile(path)
				for _, line := range bytes.Split(raw, []byte("\n")) {
					var record map[string]any
					if json.Unmarshal(line, &record) != nil || record["event"] != "agent_error" {
						continue
					}
					id, _ := record["request_id"].(string)
					if capability, ok := failures[id]; ok && record["source"] == kinds[capability] && record["capability"] == capability && record["error_code"] != "" {
						found[id] = true
					}
				}
			}
			if len(found) == len(failures) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("local diagnostic records found for %v of %v", found, failures)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}
