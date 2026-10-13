//go:build integration

package it

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/testschema"
)

func TestSDKCustomerPatternPublishedExampleRealBinaries(t *testing.T) {
	repo, tmp := repoRoot(t), t.TempDir()
	gatewayBin, agentBin := filepath.Join(tmp, "gateway"), filepath.Join(tmp, "agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")
	module := filepath.Join(tmp, "customer.wasm")
	build := exec.Command("go", "run", "./examples/sdk/build", "--output", module)
	build.Dir = repo
	if raw, e := build.CombinedOutput(); e != nil {
		t.Fatal(e, string(raw))
	}
	read := func(path string) string {
		t.Helper()
		b, e := os.ReadFile(filepath.Join(repo, path))
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	// Use the published configuration verbatim, rather than a less constrained fixture.
	config := read("examples/sdk/agent.yaml")
	if !strings.Contains(config, "pattern: '^[A-Za-z0-9 ]{1,64}$'") {
		t.Fatal("published pattern changed")
	}
	address := freeAddr(t)
	base := "http://" + address
	path := filepath.Join(tmp, "agent.yaml")
	writeFile(t, path, strings.ReplaceAll(config, "127.0.0.1:8080", address))
	env := filepath.Join(tmp, "gateway.env")
	writeFile(t, env, strings.ReplaceAll(read("examples/sdk/gateway.env"), "127.0.0.1:8080", address))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	validate := exec.CommandContext(ctx, agentBin, "validate", "--config", path)
	validate.Dir = tmp
	if raw, e := validate.CombinedOutput(); e != nil {
		t.Fatal(e, string(raw))
	}
	gateway, logs := startShellProcessWithOutput(t, tmp, "set -a; . "+shellQuote(env)+"; set +a; exec "+shellQuote(gatewayBin))
	defer stopProcess(t, gateway)
	waitForHTTP(t, base+"/healthz", "", 200)
	agent, agentLogs := startProcessWithOutput(t, tmp, agentBin, []string{"--config", path}, nil)
	defer stopProcess(t, agent)
	key := "orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM"
	waitForHTTP(t, base+"/openapi.json", key, 200)
	request := func(t *testing.T, method, url string, body []byte) (int, []byte, http.Header) {
		t.Helper()
		q, e := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		q.Header.Set("Authorization", "Bearer "+key)
		q.Header.Set("Content-Type", "application/json")
		response, e := http.DefaultClient.Do(q)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		raw, e := io.ReadAll(response.Body)
		if e != nil {
			t.Fatal(e)
		}
		return response.StatusCode, raw, response.Header
	}
	_, doc, _ := request(t, "GET", base+"/openapi.json", nil)
	published := testschema.OpenAPI(t, doc)
	cfg, e := contract.Compile([]byte(strings.ReplaceAll(config, "127.0.0.1:8080", address)))
	if e != nil {
		t.Fatal(e)
	}
	capability := cfg.Capabilities["customer_csv"]
	checkSuccess := func(t *testing.T, raw []byte) string {
		t.Helper()
		var envelope contract.SuccessEnvelope
		if e := json.Unmarshal(raw, &envelope); e != nil || len(envelope.Result.Files) != 1 {
			t.Fatal(e, string(raw))
		}
		data, ok := envelope.Result.Data.(map[string]any)
		if !ok || len(data) != 1 || data["customer"] != "Alice" {
			t.Fatal(data)
		}
		file := envelope.Result.Files[0]
		want := []byte("customer\nAlice\n")
		hash := sha256.Sum256(want)
		status, got, _ := request(t, "GET", file.URI, nil)
		if status != 200 || !bytes.Equal(got, want) || file.Name != "customer.csv" || file.MIMEType != "text/csv" || file.SizeBytes != int64(len(want)) || file.SHA256 != hex.EncodeToString(hash[:]) {
			t.Fatal(status, string(got), file)
		}
		return file.URI
	}
	for _, version := range []string{"REST", "2025-03-26", "2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			args := map[string]any{"customer": "Alice"}
			if version == "REST" {
				body, _ := json.Marshal(args)
				status, raw, header := request(t, "POST", base+"/api/v1/capabilities/customer_csv", body)
				if status != 200 {
					t.Fatal(status, string(raw))
				}
				testschema.HTTPResponse(t, published, base+"/api/v1/capabilities/customer_csv", header, raw)
				checkSuccess(t, raw)
			} else {
				call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "customer_csv", "arguments": args}})
				status, raw := postMCPStatusWithProtocol(t, base, key, version, string(call))
				if status != 200 {
					t.Fatal(status, string(raw))
				}
				var response mcpToolCallResponse
				if e := json.Unmarshal(raw, &response); e != nil {
					t.Fatal(e)
				}
				wantContents := 2
				if version == "2025-03-26" {
					wantContents = 1
				}
				if response.JSONRPC != "2.0" || string(response.ID) != "1" || len(response.Error) != 0 || response.Result.IsError || len(response.Result.Content) != wantContents || response.Result.Content[0].Type != "text" {
					t.Fatal("MCP file response", string(raw))
				}
				if e := validateMCPStructuredContent(response.Result.StructuredContent, version != "2025-03-26"); e != nil {
					t.Fatal(e)
				}
				uri := checkSuccess(t, []byte(response.Result.Content[0].Text))
				if version != "2025-03-26" {
					var wire map[string]any
					if e := json.Unmarshal(raw, &wire); e != nil {
						t.Fatal(e)
					}
					link := wire["result"].(map[string]any)["content"].([]any)[1].(map[string]any)
					if link["type"] != "resource_link" || link["uri"] != uri || link["name"] != "customer.csv" || link["mimeType"] != "text/csv" || link["size"] != float64(15) {
						t.Fatal("MCP resource link mismatch", link)
					}
				}
				if version != "2025-03-26" {
					var value any
					json.Unmarshal(response.Result.StructuredContent, &value)
					testschema.JSON(t, capability.SuccessSchema("mcp"), value, true)
				}
			}
			for _, invalid := range []string{"=SUM(A1)", "Alice!", strings.Repeat("a", 65)} {
				args = map[string]any{"customer": invalid}
				testschema.JSON(t, capability.InputMediaSchemas().JSON, args, false)
				var raw []byte
				if version == "REST" {
					body, _ := json.Marshal(args)
					status, b, _ := request(t, "POST", base+"/api/v1/capabilities/customer_csv", body)
					raw = b
					if status != 400 {
						t.Fatal(status, string(raw))
					}
				} else {
					call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "customer_csv", "arguments": args}})
					status, b := postMCPStatusWithProtocol(t, base, key, version, string(call))
					raw = b
					if status != 200 || !bytes.Contains(raw, []byte(`"isError":true`)) {
						t.Fatal(status, string(raw))
					}
				}
				if !bytes.Contains(raw, []byte("VALIDATION_FAILED")) || !bytes.Contains(raw, []byte("not_started")) || bytes.Contains(raw, []byte("customer.csv")) || bytes.Contains(raw, []byte("wasm_compile_ms")) {
					t.Fatal("invalid input reached Wasm result path", string(raw))
				}
			}
		})
	}
	started, rejected := 0, 0
	for _, line := range strings.Split(logs.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["event"] != "request" {
			continue
		}
		if record["error_code"] == "VALIDATION_FAILED" {
			rejected++
			if record["outcome"] != "not_started" || record["executor_kind"] != nil || record["file_count"] != nil {
				t.Fatal("invalid input dispatched an executor", record)
			}
		} else if record["executor_kind"] == "wasm" {
			started++
		}
	}
	if started != 4 || rejected != 12 {
		t.Fatal("real business dispatch count", started, rejected, logs.String())
	}
	if strings.Contains(logs.String(), "invalid internal input schema") || strings.Contains(agentLogs.String(), "PRIVATE") {
		t.Fatal("metadata/log failure", logs.String(), agentLogs.String())
	}
}
