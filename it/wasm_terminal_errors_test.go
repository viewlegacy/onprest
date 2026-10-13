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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/viewlegacy/onprest/internal/agent/wasmruntime"
)

func buildWasmErrorFixture(t *testing.T, repo, module string) {
	t.Helper()
	c := exec.Command("go", "build", "-trimpath", "-mod=readonly", "-o", module, "./internal/agent/wasmruntime/testdata/fixture.go")
	c.Dir = repo
	c.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	attachWasmMetadata(t, module)
}

func attachWasmMetadata(t *testing.T, module string) {
	t.Helper()
	b, e := os.ReadFile(module)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)
	m, e := r.CompileModule(ctx, b)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close(ctx)
	imports := map[string]bool{}
	for _, f := range m.ImportedFunctions() {
		a, n, _ := f.Import()
		imports[a+"."+n] = true
	}
	meta := wasmruntime.Metadata{ABI: wasmruntime.ABI, MinimumAgentVersion: wasmruntime.CompatibilityVersion, Imports: []string{}}
	for n := range imports {
		meta.Imports = append(meta.Imports, n)
	}
	sort.Strings(meta.Imports)
	leb := func(n uint32) []byte {
		out := []byte{}
		for {
			v := byte(n & 127)
			n >>= 7
			if n != 0 {
				v |= 128
			}
			out = append(out, v)
			if n == 0 {
				return out
			}
		}
	}
	raw, _ := json.Marshal(meta)
	payload := append(leb(7), []byte("onprest")...)
	payload = append(payload, raw...)
	b = append(b, 0)
	b = append(b, leb(uint32(len(payload)))...)
	b = append(b, payload...)
	if e = os.WriteFile(module, b, 0600); e != nil {
		t.Fatal(e)
	}
}

func TestWasmPublicTerminalRealBinariesRESTAndMCP(t *testing.T) {
	repo, tmp := repoRoot(t), t.TempDir()
	gatewayBin, agentBin := filepath.Join(tmp, "onprest-gateway"), filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")
	buildWasmErrorFixture(t, repo, filepath.Join(tmp, "error.wasm"))
	addr := freeAddr(t)
	base := "http://" + addr
	read := func(name string) string {
		b, e := os.ReadFile(filepath.Join(repo, "examples/wasm", name))
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	cfg := strings.Split(read("agent.yaml"), "capabilities:")[0] + `capabilities:
  wasm_error:
    executor: {type: wasm, module: ./error.wasm, effects: possible, sandbox: {enabled: true, permissions: {filesystem: [{root: ., access: read_write}]}}}
    input:
      properties:
        scenario: {type: string}
        envelope: {type: string}
        path: {type: string}
      required: [scenario]
    output: {data: {}}
`
	path := filepath.Join(tmp, "agent.yaml")
	writeFile(t, path, strings.ReplaceAll(cfg, "127.0.0.1:8080", addr))
	envFile := filepath.Join(tmp, "gateway.env")
	writeFile(t, envFile, strings.ReplaceAll(strings.ReplaceAll(read("gateway.env"), "wasm_copy", "wasm_error"), "127.0.0.1:8080", addr))
	gateway, _ := startShellProcessWithOutput(t, tmp, "set -a; . "+shellQuote(envFile)+"; set +a; exec "+shellQuote(gatewayBin))
	defer stopProcess(t, gateway)
	waitForHTTP(t, base+"/healthz", "", 200)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	validate := exec.CommandContext(ctx, agentBin, "validate", "--config", path)
	validate.Dir = tmp
	if raw, e := validate.CombinedOutput(); e != nil {
		detail, _ := os.ReadFile(filepath.Join(tmp, "onprest-agent.validate.log"))
		t.Fatal(e, string(raw), string(detail))
	}
	agent := startProcessInDir(t, ctx, tmp, agentBin, []string{"--config", path}, nil)
	defer stopProcess(t, agent)
	key := "orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM"
	waitForHTTP(t, base+"/openapi.json", key, 200)
	message := "  業務上の拒否: " + strings.Repeat("a", 30) + "  "
	public := func(code, message string) string {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"kind": "public", "code": code, "message": message}})
		return string(b)
	}
	cases := []struct {
		name, scenario, envelope, code, message, outcome string
		status                                           int
	}{
		{"valid", "envelope", public("APP_DENIED", message), "APP_DENIED", message, "no_change", 422},
		{"min", "envelope", public("APP_MIN", "a"), "APP_MIN", "a", "no_change", 422},
		{"empty", "envelope", public("APP_EMPTY", ""), "EXECUTION_FAILED", "", "no_change", 502},
		{"systemcode", "envelope", public("EXECUTION_FAILED", "PRIVATE_RAW_MESSAGE"), "EXECUTION_FAILED", "", "no_change", 502},
		{"max", "envelope", public("APP_BOUNDARY", strings.Repeat("a", 1024)), "APP_BOUNDARY", strings.Repeat("a", 1024), "no_change", 422},
		{"invalidcode", "envelope", public("APP_lower", "PRIVATE_RAW_MESSAGE"), "EXECUTION_FAILED", "", "no_change", 502},
		{"long", "envelope", public("APP_DENIED", strings.Repeat("a", 1025)), "EXECUTION_FAILED", "", "no_change", 502},
		{"control", "envelope", public("APP_DENIED", "PRIVATE_RAW_MESSAGE\n"), "EXECUTION_FAILED", "", "no_change", 502},
		{"internal", "envelope", `{"error":{"kind":"internal","category":"execution"}}`, "EXECUTION_FAILED", "", "no_change", 502},
		{"general", "privatefailure", "", "EXECUTION_FAILED", "", "no_change", 502},
		{"create_existing", "envelope_create_existing", public("APP_CREATE", message), "APP_CREATE", message, "no_change", 422},
		{"create_new", "envelope_create_new", public("APP_CREATE", message), "APP_CREATE", message, "committed", 422},
		{"create_truncate", "envelope_create_truncate", public("APP_CREATE", message), "APP_CREATE", message, "committed", 422},
		{"create_write", "envelope_create_write", public("APP_CREATE", message), "APP_CREATE", message, "committed", 422},
		{"create_failed", "envelope_create_failed", public("APP_CREATE", message), "APP_CREATE", message, "unknown", 422},
		{"effect", "envelope_effect", public("APP_AFTER_WRITE", message), "APP_AFTER_WRITE", message, "committed", 422},
	}
	check := func(t *testing.T, b []byte, tc struct {
		name, scenario, envelope, code, message, outcome string
		status                                           int
	}) {
		t.Helper()
		if bytes.Contains(b, []byte("PRIVATE_")) || bytes.Contains(b, []byte(tmp)) {
			t.Fatal("private detail escaped", string(b))
		}
		var v map[string]any
		if e := json.Unmarshal(b, &v); e != nil {
			t.Fatal(e, string(b))
		}
		err, ok := v["error"].(map[string]any)
		if !ok || err["code"] != tc.code || err["outcome"] != tc.outcome {
			t.Fatal(string(b))
		}
		if tc.message != "" && err["message"] != tc.message {
			t.Fatal("message changed", string(b))
		}
		if tc.message == "" && err["message"] != "capability execution failed" {
			t.Fatal("system message changed", string(b))
		}
		if !strings.HasPrefix(tc.code, "APP_") && err["message"] == tc.envelope {
			t.Fatal("private envelope exposed", string(b))
		}
		if strings.HasPrefix(tc.code, "APP_") {
			if _, ok := err["reason"]; ok {
				t.Fatal("APP reason exposed", string(b))
			}
		}
		if v["request_id"] == nil || v["request_id"] == "" {
			t.Fatal("request ID missing", string(b))
		}
		if err["message"] == "" {
			t.Fatal(string(b))
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			businessPath := "effect.txt"
			if strings.HasPrefix(tc.name, "create_") {
				businessPath = tc.name + ".txt"
			}
			prepare := func() {
				if !strings.HasPrefix(tc.name, "create_") {
					return
				}
				if tc.name == "create_new" {
					os.Remove(filepath.Join(tmp, businessPath))
				} else {
					if e := os.WriteFile(filepath.Join(tmp, businessPath), []byte("same"), 0600); e != nil {
						t.Fatal(e)
					}
				}
			}
			assertBody := func() {
				if !strings.HasPrefix(tc.name, "create_") {
					return
				}
				want := "same"
				if tc.name == "create_new" || tc.name == "create_truncate" {
					want = ""
				}
				if tc.name == "create_write" {
					want = "Xame"
				}
				raw, e := os.ReadFile(filepath.Join(tmp, businessPath))
				if e != nil || string(raw) != want {
					t.Fatal(e, string(raw), want)
				}
			}
			prepare()
			params := map[string]any{"scenario": tc.scenario, "envelope": tc.envelope, "path": businessPath}
			b, _ := json.Marshal(params)
			q, _ := http.NewRequestWithContext(ctx, "POST", base+"/api/v1/capabilities/wasm_error", bytes.NewReader(b))
			q.Header.Set("Authorization", "Bearer "+key)
			q.Header.Set("Content-Type", "application/json")
			res, e := http.DefaultClient.Do(q)
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != tc.status {
				detail, _ := os.ReadFile(filepath.Join(tmp, "onprest-agent.log"))
				t.Fatal(res.StatusCode, string(raw), string(detail))
			}
			check(t, raw, tc)
			assertBody()
			for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
				prepare()
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wasm_error","arguments":%s}}`, b)
				status, raw := postMCPStatusWithProtocol(t, base, key, version, body)
				if status != 200 {
					t.Fatal(status, string(raw))
				}
				var v map[string]any
				json.Unmarshal(raw, &v)
				result, ok := v["result"].(map[string]any)
				if !ok || result["isError"] != true {
					t.Fatal(string(raw))
				}
				content := result["content"].([]any)
				check(t, []byte(content[0].(map[string]any)["text"].(string)), tc)
				if version != "2025-03-26" {
					structured, _ := json.Marshal(result["structuredContent"])
					check(t, structured, tc)
				} else if _, ok := result["structuredContent"]; ok {
					t.Fatal(string(raw))
				}
				assertBody()
			}
			if tc.name == "effect" {
				data, _ := os.ReadFile(filepath.Join(tmp, "effect.txt"))
				if string(data) != "observed" {
					t.Fatal(string(data))
				}
			}
		})
	}
}
