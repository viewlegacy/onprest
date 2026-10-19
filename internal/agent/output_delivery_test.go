package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/gateway"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/testschema"
	"golang.org/x/crypto/bcrypt"
)

func TestOutputDeliveryProductionFileRESTAndMCP(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "reports")
	if e := os.Mkdir(folder, 0700); e != nil {
		t.Fatal(e)
	}
	binary := bytes.Repeat([]byte{0, 255, 32, 42}, 40000)
	for name, b := range map[string][]byte{"report.bin": binary, "empty.bin": {}, "text.txt": []byte("hello 日本\n"), "slow.txt": bytes.Repeat([]byte("<"), 4<<20)} {
		if e := os.WriteFile(filepath.Join(folder, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := listener.Addr().String()
	listener.Close()
	base := "http://" + addr
	private, _ := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
	public := ed25519.PrivateKey(private).Public().(ed25519.PublicKey)
	hash, _ := bcrypt.GenerateFromPassword([]byte("output-key"), bcrypt.MinCost)
	other, _ := bcrypt.GenerateFromPassword([]byte("output-other-key"), bcrypt.MinCost)
	var logs transportLogBuffer
	server := gateway.NewServer(gateway.Config{Addr: addr, PublicBaseURL: base, AgentPublicKey: base64.RawURLEncoding.EncodeToString(public), APIKeys: []gateway.APIKey{{Name: "caller", KeyHash: string(hash), Capabilities: []string{"*"}}, {Name: "same-name", KeyHash: string(other), Capabilities: []string{"*"}}}, AgentPingInterval: 20 * time.Millisecond, AgentPongTimeout: time.Second}, &logs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ListenAndServeContext(ctx) }()
	// The fixed transfer budget covers Agent collection through the final HTTP
	// flush of four concurrent 4MiB inline responses. Under the race detector
	// that CPU work takes about 8s even on an idle host (under 1s without it),
	// so the budget leaves room for a loaded race run. The test proceeds on
	// response headers and explicit closes, never by waiting for this budget.
	yaml := fmt.Sprintf("schema_version: 1\nruntime: {transfer_timeout: 60s}\ngateway: {url: ws://%s/ws/agent, agent_private_key: %s}\nsources: {folders: {reports: {root: ./reports, access: read}}}\ncapabilities:\n", addr, testAgentPrivateKey)
	for name, setting := range map[string]string{"reference": "", "direct": ", delivery: {rest: direct}", "inline": ", delivery: {mcp: inline}", "too_small": ", delivery: {mcp: inline}", "empty": ", delivery: {rest: direct, mcp: inline}", "text": ", delivery: {mcp: inline}", "expires": "", "slow": ", delivery: {mcp: inline}"} {
		filename := "report.bin"
		if name == "empty" {
			filename = "empty.bin"
		}
		if name == "slow" {
			filename = "slow.txt"
		}
		if name == "text" {
			filename = "text.txt"
		}
		mime := "application/octet-stream"
		if name == "text" || name == "slow" {
			mime = "text/plain"
		}
		policy := ""
		if name == "slow" {
			policy = "    policy: {mcp_inline_total_bytes: 32MiB}\n"
		}
		if name == "too_small" {
			policy = "    policy: {mcp_inline_total_bytes: 128}\n"
		}
		if name == "expires" {
			policy = "    policy: {file_ttl: 50ms}\n"
		}
		yaml += fmt.Sprintf("  %s:\n    executor: {type: file, read: {source: reports, selector: {name: %s}}}\n    output: {files: {mime_type: %s%s}}\n%s", name, filename, mime, setting, policy)
	}
	path := filepath.Join(dir, "agent.yaml")
	if e = os.WriteFile(path, []byte(yaml), 0600); e != nil {
		t.Fatal(e)
	}
	runner, e := NewRunner(context.Background(), Config{CapabilityFile: path, ReconnectEvery: 20 * time.Millisecond}, io.Discard)
	if e != nil {
		cancel()
		<-serverDone
		t.Fatal(e)
	}
	runnerDone := make(chan error, 1)
	runnerCtx, runnerCancel := context.WithCancel(ctx)
	defer runnerCancel()
	go func() { runnerDone <- runner.Run(runnerCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runnerDone:
		case <-time.After(3 * time.Second):
			t.Error("runner shutdown")
		}
		select {
		case <-serverDone:
		case <-time.After(7 * time.Second):
			t.Error("gateway shutdown")
		}
	})
	request := func(method, path, key string, body io.Reader, version string) (int, []byte, http.Header) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, base+path, body)
		req.Header.Set("Authorization", "Bearer "+key)
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		if version != "" {
			req.Header.Set("MCP-Protocol-Version", version)
		}
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		b, e := io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		return resp.StatusCode, b, resp.Header
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		req, _ := http.NewRequest("GET", base+"/openapi.json", nil)
		req.Header.Set("Authorization", "Bearer output-key")
		resp, e := http.DefaultClient.Do(req)
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("metadata not ready", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, openAPIBytes, _ := request("GET", "/openapi.json", "output-key", nil, "")
	openAPIValidator := testschema.OpenAPI(t, openAPIBytes)
	var document map[string]any
	if e := json.Unmarshal(openAPIBytes, &document); e != nil {
		t.Fatal(e)
	}
	_, toolBytes, _ := request("POST", "/mcp", "output-key", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`), "2025-11-25")
	var toolList map[string]any
	json.Unmarshal(toolBytes, &toolList)
	toolSchemas := map[string]any{}
	for _, raw := range toolList["result"].(map[string]any)["tools"].([]any) {
		v := raw.(map[string]any)
		toolSchemas[v["name"].(string)] = v["outputSchema"]
	}
	call := func(name string) contract.SuccessEnvelope {
		t.Helper()
		status, b, header := request("POST", "/api/v1/capabilities/"+name, "output-key", strings.NewReader(`{}`), "")
		var out contract.SuccessEnvelope
		if status != 200 || json.Unmarshal(b, &out) != nil {
			t.Fatalf("call %s: %d %s", name, status, b)
		}
		testschema.HTTPResponse(t, openAPIValidator, base+"/api/v1/capabilities/"+name, header, b)
		return out
	}
	out := call("reference")
	if out.Result.Data != nil || len(out.Result.Files) != 1 {
		t.Fatal(out)
	}
	f := out.Result.Files[0]
	digest := sha256.Sum256(binary)
	if f.URI != base+"/api/v1/files/"+f.ID || f.SizeBytes != int64(len(binary)) || f.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("descriptor", f)
	}
	// Retrieval reads the independent snapshot, never re-runs the producer.
	if e = os.WriteFile(filepath.Join(folder, "report.bin"), []byte("new version"), 0600); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		status, b, h := request("GET", "/api/v1/files/"+f.ID, "output-key", nil, "")
		if status != 200 || !bytes.Equal(b, binary) || h.Get("X-Request-ID") != out.RequestID || h.Get("Content-Length") != fmt.Sprint(len(binary)) {
			t.Fatal("snapshot retrieval", status, h)
		}
	}
	status, _, _ := request("GET", "/api/v1/files/"+f.ID, "output-other-key", nil, "")
	if status != 403 {
		t.Fatal("foreign caller", status)
	}
	req, _ := http.NewRequest("GET", f.URI, nil)
	req.Header.Set("Authorization", "Bearer output-key")
	req.Header.Set("Range", "bytes=0-3")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 416 {
		t.Fatal("range", resp.StatusCode)
	}
	status, b, h := request("POST", "/api/v1/capabilities/direct", "output-key", strings.NewReader(`{}`), "")
	if status != 200 || string(b) != "new version" || !strings.Contains(h.Get("Content-Disposition"), "attachment") {
		t.Fatal("direct", status, string(b), h)
	}
	testschema.HTTPResponse(t, openAPIValidator, base+"/api/v1/capabilities/direct", h, b)
	status, b, h = request("POST", "/api/v1/capabilities/empty", "output-key", strings.NewReader(`{}`), "")
	if status != 200 || len(b) != 0 || h.Get("Content-Length") != "0" {
		t.Fatal("empty direct", status, h)
	}
	testschema.HTTPResponse(t, openAPIValidator, base+"/api/v1/capabilities/empty", h, b)
	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			rpc := func(method, params string) map[string]any {
				t.Helper()
				status, b, _ := request("POST", "/mcp", "output-key", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+params+`}`), version)
				var v map[string]any
				if status != 200 || json.Unmarshal(b, &v) != nil {
					t.Fatalf("MCP %s: %d %s", method, status, b)
				}
				return v
			}
			for _, name := range []string{"reference", "direct", "inline", "text", "empty"} {
				v := rpc("tools/call", `{"name":"`+name+`","arguments":{}}`)
				result := v["result"].(map[string]any)
				if result["isError"] == true {
					t.Fatal(v)
				}
				_, structured := result["structuredContent"]
				if structured != (version != "2025-03-26") {
					t.Fatal("version", v)
				}
				content := result["content"].([]any)
				var envelope map[string]any
				json.Unmarshal([]byte(content[0].(map[string]any)["text"].(string)), &envelope)
				testschema.JSON(t, toolSchemas[name], envelope, true)
				if structured {
					testschema.JSON(t, toolSchemas[name], result["structuredContent"], true)
				}
				// An empty or mismatched descriptor schema must not pass this proof.
				badRaw, _ := json.Marshal(envelope)
				var bad map[string]any
				json.Unmarshal(badRaw, &bad)
				descriptor := bad["result"].(map[string]any)["files"].([]any)[0].(map[string]any)
				delete(descriptor, "sha256")
				testschema.JSON(t, toolSchemas[name], bad, false)

				if name == "reference" || name == "direct" {
					if len(content) != (map[bool]int{true: 1, false: 2}[version == "2025-03-26"]) {
						t.Fatal(v)
					}
				} else {
					resource := content[1].(map[string]any)["resource"].(map[string]any)
					if name == "text" {
						if resource["text"] != "hello 日本\n" {
							t.Fatal(v)
						}
					} else {
						want := "new version"
						if name == "empty" {
							want = ""
						}
						body, e := base64.StdEncoding.DecodeString(resource["blob"].(string))
						if e != nil || string(body) != want {
							t.Fatal(v)
						}
					}
				}
			}
			v := rpc("tools/call", `{"name":"too_small","arguments":{}}`)
			if v["result"].(map[string]any)["isError"] != true {
				t.Fatal("inline fallback", v)
			}
			if len(rpc("resources/list", `{}`)["result"].(map[string]any)["resources"].([]any)) != 0 {
				t.Fatal("resource list")
			}
			v = rpc("resources/read", `{"uri":"`+f.URI+`"}`)
			resource := v["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)
			body, e := base64.StdEncoding.DecodeString(resource["blob"].(string))
			if e != nil || !bytes.Equal(body, binary) {
				t.Fatal("resource bytes")
			}
			if rpc("resources/read", `{"uri":"https://evil.example/file"}`)["error"] == nil {
				t.Fatal("arbitrary resource URL")
			}
			tools := rpc("tools/list", `{}`)["result"].(map[string]any)["tools"].([]any)
			for _, raw := range tools {
				tool := raw.(map[string]any)
				_, present := tool["outputSchema"]
				if present != (version != "2025-03-26") {
					t.Fatal("MCP direct/inline schema", tool)
				}
			}
		})
	}
	expired := call("expires")
	time.Sleep(80 * time.Millisecond)
	status, _, _ = request("GET", "/api/v1/files/"+expired.Result.Files[0].ID, "output-key", nil, "")
	if status != 404 {
		t.Fatal("TTL", status)
	}
	// The pinned official client consumes the same real Streamable HTTP endpoint.
	client := mcp.NewClient(&mcp.Implementation{Name: "output-delivery-test", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: &http.Client{Transport: outputKeyTransport{}}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	tools, e := session.ListTools(ctx, nil)
	if e != nil || len(tools.Tools) != 8 {
		t.Fatal("official tools", e)
	}
	result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "text", Arguments: map[string]any{}})
	if e != nil || result.IsError {
		t.Fatal("official inline", e, result)
	}
	testschema.JSON(t, toolSchemas["text"], result.StructuredContent, true)
	resource, e := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: f.URI})
	if e != nil || len(resource.Contents) != 1 {
		t.Fatal("official resource read", e)
	}
	// Finished Agent transfers must not release the Gateway's response lease
	// while authenticated HTTP receivers keep their sockets open without reading.
	var slowClients []net.Conn
	var slowReaders []*bufio.Reader
	slowDialer := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var setErr error
		if e := c.Control(func(fd uintptr) { setErr = setReceiveBuffer(fd, 1024) }); e != nil {
			return e
		}
		return setErr
	}}
	for i := 0; i < 4; i++ {
		method, params := "tools/call", `{"name":"slow","arguments":{}}`
		if i < 2 {
			retained := call("slow")
			method = "resources/read"
			params = `{"uri":"` + retained.Result.Files[0].URI + `"}`
		}
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
		conn, e := slowDialer.Dial("tcp", addr)
		if e != nil {
			t.Fatal(e)
		}
		slowClients = append(slowClients, conn)
		defer conn.Close()
		fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer output-key\r\nMCP-Protocol-Version: 2025-11-25\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		slowReaders = append(slowReaders, bufio.NewReader(conn))
	}
	for _, reader := range slowReaders {
		response, e := http.ReadResponse(reader, nil)
		if e != nil || response.StatusCode != 200 {
			t.Fatal("slow real MCP response", e)
		}
	}
	runner.controls.mu.Lock()
	activeTransfers := runner.controls.transfers
	runner.controls.mu.Unlock()
	if activeTransfers != 0 {
		t.Fatal("Agent transfer slots remained after HTTP headers", activeTransfers)
	}
	status, _, _ = request("POST", "/mcp", "output-key", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"reference","arguments":{}}}`), "2025-11-25")
	if status != 503 {
		t.Fatal("completed MCP buffers did not retain response slots", status)
	}
	call("reference") // Separate REST business traffic and 20ms ping/pong stay live.
	for _, conn := range slowClients {
		conn.Close()
	}
	waitTransport(t, func() bool {
		runner.controls.mu.Lock()
		defer runner.controls.mu.Unlock()
		return runner.controls.transfers == 0
	})
	deadline = time.Now().Add(time.Second)
	for {
		status, _, _ = request("POST", "/mcp", "output-key", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"reference","arguments":{}}}`), "2025-11-25")
		if status == 200 {
			break
		}
		if status != 503 || time.Now().After(deadline) {
			t.Fatal("canceled response capacity was not reclaimed", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A fresh authenticated Runner generation never accepts an old public handle.
	runnerCancel()
	select {
	case <-runnerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("runner restart shutdown")
	}
	// Ready metadata takes one read token. Unknown canonical lookups consume
	// the remaining fixed Agent tokens even without Gateway source-IP limits.
	yaml = strings.Replace(yaml, "runtime: {transfer_timeout: 60s}", "runtime: {transfer_timeout: 60s, control_limits: {read: {requests: 1, per: 1h, burst: 4}}}", 1)
	if e = os.WriteFile(path, []byte(yaml), 0600); e != nil {
		t.Fatal(e)
	}
	next, e := NewRunner(ctx, Config{CapabilityFile: path}, io.Discard)
	if e != nil {
		t.Fatal(e)
	}
	runnerDone = make(chan error, 1)
	go func() { runnerDone <- next.Run(ctx) }()
	deadline = time.Now().Add(3 * time.Second)
	for {
		status, _, _ = request("GET", "/openapi.json", "output-key", nil, "")
		if status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restart metadata")
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, _, _ = request("GET", "/api/v1/files/"+f.ID, "output-key", nil, "")
	if status != 404 {
		t.Fatal("old generation reference", status)
	}
	retained := call("reference")
	usage := next.store.Usage()
	for _, id := range []string{"unknown_one", "unknown_two", retained.Result.Files[0].ID} {
		status, _, _ = request("GET", "/api/v1/files/"+id, "output-key", nil, "")
		if id == retained.Result.Files[0].ID && status != 429 {
			t.Fatal("Agent lookup-before-rate", status)
		}
		if id != retained.Result.Files[0].ID && status != 404 {
			t.Fatal("unknown lookup public contract", status)
		}
	}
	if next.store.Usage() != usage {
		t.Fatal("rejected lookup changed retained snapshot")
	}
	next.controls.mu.Lock()
	buckets := len(next.controls.buckets)
	next.controls.mu.Unlock()
	if buckets != 4 {
		t.Fatal("unbounded control classes", buckets)
	}
	// Neither budget refusal nor an unknown handle reruns business work or stops
	// normal calls and the signed connection's frequent ping/pong.
	call("reference")
	if strings.Contains(logs.String(), "report.bin") || strings.Contains(logs.String(), "hello 日本") {
		t.Fatal("content/name leaked to logs")
	}
}

type outputKeyTransport struct{}

func (outputKeyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer output-key")
	return http.DefaultTransport.RoundTrip(r)
}

func TestFileProviderProductionReadPreparationSpoolFree(t *testing.T) {
	dir := t.TempDir()
	if e := os.Mkdir(filepath.Join(dir, "reports"), 0700); e != nil {
		t.Fatal(e)
	}
	for _, input := range []bool{false, true} {
		t.Run(fmt.Sprintf("file_input_%t", input), func(t *testing.T) {
			body := fmt.Sprintf("schema_version: 1\ngateway: {url: ws://localhost:8080/ws/agent, agent_private_key: %s}\nsources: {folders: {reports: {root: ./reports, access: read}}}\ncapabilities:\n  read_report:\n    executor: {type: file, read: {source: reports, selector: {name: report.txt}}}\n    output: {files: {}}\n", testAgentPrivateKey)
			if input {
				body += "    input: {properties: {document: {type: file}}}\n"
			}
			path := filepath.Join(dir, "agent.yaml")
			if e := os.WriteFile(path, []byte(body), 0600); e != nil {
				t.Fatal(e)
			}
			var events []string
			var detail bytes.Buffer
			prepared, e := prepareAgent(context.Background(), Config{CapabilityFile: path}, memoryPreparationLog(&events, &detail))
			if e != nil {
				t.Fatalf("input=%t error=%v", input, e)
			}
			if e == nil {
				prepared.closeDatabase()
			}
			if _, e := os.Stat(filepath.Join(dir, "spool")); !os.IsNotExist(e) {
				t.Fatal("preflight acquired runtime spool")
			}
			if _, e := os.Stat(filepath.Join(dir, "reports", "report.txt")); !os.IsNotExist(e) {
				t.Fatal("preflight changed business file")
			}
		})
	}
}

func TestNormalOutputLaterReaderFailurePublishesNothing(t *testing.T) {
	r, provider := transportFixture(t, "ws://localhost/ws/agent", "")
	provider.fn = func(context.Context, map[string]any, *providers.Effects) (contract.ProviderResult, error) {
		return contract.ProviderResult{Files: []contract.ProviderFile{
			{Name: "first.bin", MIMEType: "application/octet-stream", Reader: strings.NewReader("completed")},
			{Name: "later.bin", MIMEType: "application/octet-stream", Reader: badTransportReader{}},
		}}, nil
	}
	session := &transportSession{generation: "normal-output-generation", owners: map[string]retainedOwner{}}
	snapshot, _ := r.versioned.Snapshot("run")
	ctx := context.WithValue(context.Background(), outputSessionKey{}, session)
	result := r.executeVersioned(ctx, protocol.Request{ID: "normal-output", Capability: "run", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), Deadline: time.Now().Add(time.Second), Params: map[string]any{}})
	if result.Error == nil || result.Result != nil || len(session.owners) != 0 || r.store.Usage() != 0 || provider.calls.Load() != 1 {
		t.Fatal("partial normal output retained/published", result, r.store.Usage())
	}
}
