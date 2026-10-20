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
	key := "orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM"
	waitForReady := func(url, key string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		var last string
		for ctx.Err() == nil {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			res, err := http.DefaultClient.Do(req)
			if err == nil {
				body, readErr := io.ReadAll(res.Body)
				res.Body.Close()
				if readErr == nil && res.StatusCode == http.StatusOK {
					return
				}
				last = fmt.Sprintf("status=%d body=%s read_error=%v", res.StatusCode, body, readErr)
			} else {
				last = err.Error()
			}
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatalf("timed out waiting for %s: %v; %s", url, ctx.Err(), last)
	}
	post := func(t *testing.T, route, protocolVersion string, payload []byte) (int, []byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+route, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		if protocolVersion != "" {
			req.Header.Set("MCP-Protocol-Version", protocolVersion)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("read %s response: %v (context: %v)", route, err, ctx.Err())
		}
		return res.StatusCode, body
	}
	read := func(name string) string {
		b, e := os.ReadFile(filepath.Join(repo, "examples/wasm", name))
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	cfg := strings.Split(read("agent.yaml"), "capabilities:")[0] + `capabilities:
  wasm_error:
    executor: {type: wasm, module: ./error.wasm, effects: possible, sandbox: {enabled: true, permissions: {filesystem: [{root: ./data, access: read_write}]}}}
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
	// The code grant covers business files only, never the private spool.
	data := filepath.Join(tmp, "data")
	if e := os.Mkdir(data, 0o700); e != nil {
		t.Fatal(e)
	}
	envFile := filepath.Join(tmp, "gateway.env")
	writeFile(t, envFile, strings.ReplaceAll(strings.ReplaceAll(read("gateway.env"), "wasm_copy", "wasm_error"), "127.0.0.1:8080", addr))
	gateway, _ := startShellProcessWithOutput(t, tmp, "set -a; . "+shellQuote(envFile)+"; set +a; exec "+shellQuote(gatewayBin))
	defer stopProcess(t, gateway)
	waitForReady(base+"/healthz", "")
	validateCtx, cancelValidate := context.WithTimeout(t.Context(), 120*time.Second)
	validate := exec.CommandContext(validateCtx, agentBin, "validate", "--config", path)
	validate.Dir = tmp
	raw, validateErr := validate.CombinedOutput()
	cancelValidate()
	if validateErr != nil {
		detail, _ := os.ReadFile(filepath.Join(tmp, "onprest-agent.validate.log"))
		t.Fatal(validateErr, string(raw), string(detail))
	}
	// The Agent serves every case; individual operation deadlines must not
	// terminate it when their cumulative duration exceeds one operation's budget.
	agent := startProcessInDir(t, t.Context(), tmp, agentBin, []string{"--config", path}, nil)
	defer stopProcess(t, agent)
	waitForReady(base+"/openapi.json", key)
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
		// The module ignores the refused commit and exits 0; the refusal decides.
		{"empty_ignored", "envelope_ignore", public("APP_EMPTY", ""), "EXECUTION_FAILED", "", "no_change", 502},
		{"invalidcode_ignored", "envelope_ignore", public("APP_lower", "PRIVATE_RAW_MESSAGE"), "EXECUTION_FAILED", "", "no_change", 502},
		// A valid terminal committed before a protocol violation is not
		// published; the violation is an invalid output.
		{"read_after_commit", "envelope_then_read", public("APP_DENIED", message), "OUTPUT_INVALID", "output does not satisfy the configured contract", "no_change", 502},
		{"write_after_commit", "envelope_then_write", public("APP_DENIED", message), "OUTPUT_INVALID", "output does not satisfy the configured contract", "no_change", 502},
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
				if tc.scenario == "envelope_then_read" {
					if e := os.WriteFile(filepath.Join(data, businessPath), []byte("readable"), 0600); e != nil {
						t.Fatal(e)
					}
				}
				if !strings.HasPrefix(tc.name, "create_") {
					return
				}
				if tc.name == "create_new" {
					os.Remove(filepath.Join(data, businessPath))
				} else {
					if e := os.WriteFile(filepath.Join(data, businessPath), []byte("same"), 0600); e != nil {
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
				raw, e := os.ReadFile(filepath.Join(data, businessPath))
				if e != nil || string(raw) != want {
					t.Fatal(e, string(raw), want)
				}
			}
			prepare()
			params := map[string]any{"scenario": tc.scenario, "envelope": tc.envelope, "path": "data/" + businessPath}
			b, _ := json.Marshal(params)
			status, raw := post(t, "/api/v1/capabilities/wasm_error", "", b)
			if status != tc.status {
				detail, _ := os.ReadFile(filepath.Join(tmp, "onprest-agent.log"))
				t.Fatal(status, string(raw), string(detail))
			}
			check(t, raw, tc)
			assertBody()
			for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
				prepare()
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wasm_error","arguments":%s}}`, b)
				status, raw := post(t, "/mcp", version, []byte(body))
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
				effect, _ := os.ReadFile(filepath.Join(data, "effect.txt"))
				if string(effect) != "observed" {
					t.Fatal(string(effect))
				}
			}
		})
	}
}
