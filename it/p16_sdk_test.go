//go:build integration

package it

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSDKPublicResultsRealBinariesRESTAndMCP(t *testing.T) {
	repo, tmp := repoRoot(t), t.TempDir()
	gatewayBin, agentBin := filepath.Join(tmp, "gateway"), filepath.Join(tmp, "agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")
	module := filepath.Join(tmp, "sdk.wasm")
	build := exec.Command("go", "build", "-trimpath", "-mod=readonly", "-o", module, ".")
	build.Dir = filepath.Join(repo, "internal/agent/wasmruntime/testdata/sdkfixture")
	build.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if raw, e := build.CombinedOutput(); e != nil {
		t.Fatal(e, string(raw))
	}
	attachWasmMetadata(t, module)
	read := func(name string) string {
		b, e := os.ReadFile(filepath.Join(repo, "examples/wasm", name))
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	address := freeAddr(t)
	base := "http://" + address
	config := strings.Split(read("agent.yaml"), "capabilities:")[0] + `capabilities:
  sdk:
    executor:
      type: wasm
      module: ./sdk.wasm
      limits: {memory_bytes: 512MiB, max_file_read_bytes: 4, max_total_file_read_bytes: 8}
      sandbox: {enabled: true, permissions: {filesystem: [{root: ., access: read_write}]}}
    input:
      properties:
        scenario: {type: string}
        path: {type: string}
        code: {type: string}
        message: {type: string}
        i32: {type: integer}
        i64: {type: integer, default: 9007199254740993}
        document: {type: file}
      required: [scenario]
    output: {data: {}, files: {}}
    policy: {timeout: 30s}
`
	path := filepath.Join(tmp, "agent.yaml")
	writeFile(t, path, strings.ReplaceAll(config, "127.0.0.1:8080", address))
	writeFile(t, filepath.Join(tmp, "large"), "12345")
	env := filepath.Join(tmp, "gateway.env")
	writeFile(t, env, strings.ReplaceAll(strings.ReplaceAll(read("gateway.env"), "wasm_copy", "sdk"), "127.0.0.1:8080", address))
	gateway, _ := startShellProcessWithOutput(t, tmp, "set -a; . "+shellQuote(env)+"; set +a; exec "+shellQuote(gatewayBin))
	defer stopProcess(t, gateway)
	waitForHTTP(t, base+"/healthz", "", 200)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	agent := startProcessInDir(t, ctx, repo, agentBin, []string{"--config", path}, nil)
	defer stopProcess(t, agent)
	key := "orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM"
	waitForHTTP(t, base+"/openapi.json", key, 200)
	request := func(method, url string, body []byte) (int, []byte) {
		t.Helper()
		q, _ := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		q.Header.Set("Authorization", "Bearer "+key)
		q.Header.Set("Content-Type", "application/json")
		v, e := http.DefaultClient.Do(q)
		if e != nil {
			t.Fatal(e)
		}
		defer v.Body.Close()
		raw, e := io.ReadAll(v.Body)
		if e != nil {
			t.Fatal(e)
		}
		return v.StatusCode, raw
	}
	for index, test := range []struct{ scenario, code, message, want, outcome string }{{"public", "APP_DECLINED", "説明", "APP_DECLINED", "no_change"}, {"public", "APP_A", strings.Repeat("a", 1024), "APP_A", "no_change"}, {"public", "APP_A", strings.Repeat("a", 1025), "EXECUTION_FAILED", "no_change"}, {"public", "LIMIT_EXCEEDED", "private detail", "EXECUTION_FAILED", "no_change"}, {"public", "APP_A", "private\ncontrol", "EXECUTION_FAILED", "no_change"}, {"general", "", "", "EXECUTION_FAILED", "no_change"}, {"limit", "", "", "LIMIT_EXCEEDED", "no_change"}, {"joined", "", "", "LIMIT_EXCEEDED", "no_change"}, {"envelope-boundary", "", "", "LIMIT_EXCEEDED", "no_change"}, {"side-effect", "", "", "APP_AFTER_WRITE", "committed"}} {
		t.Run(fmt.Sprintf("%02d_%s_%s", index, test.scenario, test.want), func(t *testing.T) {
			args := map[string]any{"scenario": test.scenario, "code": test.code, "message": test.message, "path": "large"}
			if test.scenario == "envelope-boundary" {
				args["i64"] = int64(32<<20) + 1
			}
			body, _ := json.Marshal(args)
			for _, protocol := range []string{"REST", "2025-03-26", "2025-06-18", "2025-11-25"} {
				var status int
				var raw []byte
				if protocol == "REST" {
					status, raw = request("POST", base+"/api/v1/capabilities/sdk", body)
					if status != 422 && !(test.want == "EXECUTION_FAILED" && status == 502) {
						t.Fatal(status, string(raw))
					}
				} else {
					call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "sdk", "arguments": args}})
					status, raw = postMCPStatusWithProtocol(t, base, key, protocol, string(call))
					if status != 200 || !bytes.Contains(raw, []byte(`"isError":true`)) {
						t.Fatal(status, string(raw))
					}
				}
				if !bytes.Contains(raw, []byte(test.want)) || !bytes.Contains(raw, []byte(test.outcome)) || bytes.Contains(raw, []byte("private")) || bytes.Contains(raw, []byte("file_id")) {
					t.Fatal(protocol, string(raw))
				}
				if strings.HasPrefix(test.want, "APP_") && test.scenario == "public" {
					var parsed any
					if err := json.Unmarshal(raw, &parsed); err != nil {
						t.Fatal(err)
					}
					if !recursiveString(parsed, test.message) {
						t.Fatal("public message changed", protocol, string(raw))
					}
				}
			}
		})
	}
	// Each request must begin with the same immutable read-limit fixture.
	// side-effect changes its target; restore it before later scenarios.
	writeFile(t, filepath.Join(tmp, "large"), "12345")
	for _, scenario := range []string{"mixed", "input", "decode"} {
		t.Run(scenario, func(t *testing.T) {
			args := map[string]any{"scenario": scenario}
			if scenario == "input" {
				args["document"] = map[string]any{"name": "input.bin", "mime_type": "application/octet-stream", "data": "AP8B"}
			}
			body, _ := json.Marshal(args)
			status, raw := request("POST", base+"/api/v1/capabilities/sdk", body)
			if status != 200 {
				t.Fatal(status, string(raw))
			}
			var envelope struct {
				Result struct {
					Data  json.RawMessage `json:"data"`
					Files []struct {
						URI  string `json:"uri"`
						Size int64  `json:"size_bytes"`
					} `json:"files"`
				} `json:"result"`
			}
			if e := json.Unmarshal(raw, &envelope); e != nil {
				t.Fatal(e)
			}
			if scenario == "decode" {
				if !bytes.Contains(envelope.Result.Data, []byte(`9007199254740993`)) {
					t.Fatal(string(raw))
				}
			} else {
				if len(envelope.Result.Files) == 0 {
					t.Fatal(string(raw))
				}
				for i, f := range envelope.Result.Files {
					status, b := request("GET", f.URI, nil)
					want := []byte{}
					if scenario == "input" {
						want = []byte{0, 255, 1}
					} else if i == 0 {
						want = []byte{0, 255, 1, 2}
					}
					if status != 200 || !bytes.Equal(b, want) || f.Size != int64(len(want)) {
						t.Fatal(status, b, f)
					}
				}
			}
			for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
				call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "sdk", "arguments": args}})
				status, b := postMCPStatusWithProtocol(t, base, key, version, string(call))
				if status != 200 || bytes.Contains(b, []byte(`"isError":true`)) || bytes.Contains(b, []byte("file_id")) || (scenario == "decode" && !bytes.Contains(b, []byte("9007199254740993"))) {
					t.Fatal(version, status, string(b))
				}
			}
			if bytes.Contains(raw, []byte("file_id")) || bytes.Contains(raw, []byte("__onprest")) {
				t.Fatal(string(raw))
			}
		})
	}
	// YAML-valid but Go-incompatible input fails before the SDK handler.
	status, raw := request("POST", base+"/api/v1/capabilities/sdk", []byte(`{"scenario":"decode","i32":2147483648}`))
	if status != 502 || !bytes.Contains(raw, []byte("EXECUTION_FAILED")) || bytes.Contains(raw, []byte("i32")) {
		t.Fatal(status, string(raw))
	}
}
func recursiveString(v any, want string) bool {
	switch x := v.(type) {
	case string:
		if x == want {
			return true
		}
		var embedded any
		if json.Unmarshal([]byte(x), &embedded) == nil {
			return recursiveString(embedded, want)
		}
	case map[string]any:
		for _, v := range x {
			if recursiveString(v, want) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if recursiveString(v, want) {
				return true
			}
		}
	}
	return false
}
