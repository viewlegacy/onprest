//go:build integration

package it

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A configuration that declares reference delivery while the Gateway has no
// ONPREST_PUBLIC_BASE_URL keeps every capability unavailable. Business calls
// must report the missing dependency instead of an offline Agent.
func TestMissingFileOriginRealBinariesReportDependencyUnavailable(t *testing.T) {
	repo, tmp := repoRoot(t), t.TempDir()
	gatewayBin, agentBin := filepath.Join(tmp, "onprest-gateway"), filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")
	address := freeAddr(t)
	base := "http://" + address
	if e := os.MkdirAll(filepath.Join(tmp, "reports"), 0o700); e != nil {
		t.Fatal(e)
	}
	writeFile(t, filepath.Join(tmp, "reports", "report.txt"), "completed report\n")
	config := writeFile(t, filepath.Join(tmp, "agent.yaml"), `schema_version: 1
gateway:
  url: ws://`+address+`/ws/agent
  agent_private_key: keEk2aSPeUHiCbhK-XxleMUFj3cwzcJCFUflKSs_CiZOsybztXdoRPcyYZTMd_f9cplE8Qd7VsMz484fWauOvw
sources:
  folders:
    reports: {root: ./reports, access: read}
  http:
    inventory: {base_url: 'http://127.0.0.1:9', allowed_methods: [POST], allowed_cidrs: [127.0.0.0/8]}
capabilities:
  download_report:
    executor: {type: file, read: {source: reports, selector: {name: report.txt}}}
    output: {files: {}}
  json_only:
    executor: {type: http, source: inventory, method: POST, path: /stock, response: {kind: json}}
    output: {data: {}}
`)
	env := writeFile(t, filepath.Join(tmp, "gateway.env"), `GATEWAY_AGENT_PUBLIC_KEY=TrMm87V3aET3MmGUzHf3_XKZRPEHe1bDM-POH1mrjr8
GATEWAY_API_KEYS_JSON='[{"name":"dev","key_hash":"$2a$10$INgs32pPDl8EQAOTcQ1NN.eZUpNkDtyTKXh2luqxE32vNBmaLpy7m","capabilities":["*"]}]'
GATEWAY_ADDR=`+address+"\n")
	gateway, logs := startShellProcessWithOutput(t, tmp, "set -a; . "+shellQuote(env)+"; set +a; exec "+shellQuote(gatewayBin))
	defer stopProcess(t, gateway)
	waitForHTTP(t, base+"/healthz", "", 200)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := startProcessInDir(t, ctx, tmp, agentBin, []string{"--config", config}, nil)
	defer stopProcess(t, agent)
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(logs.String(), "ONPREST_PUBLIC_BASE_URL is required for file reference delivery") {
		if time.Now().After(deadline) {
			t.Fatal("metadata rejection not observed", logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	key := "orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM"
	do := func(method, path, body string, header map[string]string) (int, []byte) {
		t.Helper()
		req, e := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, raw
	}
	status, raw := do("GET", "/healthz", "", nil)
	if status != 200 || !bytes.Contains(raw, []byte(`"agent_connected":true`)) {
		t.Fatal("healthz", status, string(raw))
	}
	type failure struct {
		Error struct{ Code, Message, Outcome string } `json:"error"`
	}
	assertMissingOrigin := func(t *testing.T, raw []byte) {
		t.Helper()
		var f failure
		if e := json.Unmarshal(raw, &f); e != nil || f.Error.Code != "DEPENDENCY_UNAVAILABLE" || f.Error.Outcome != "not_started" || !strings.Contains(f.Error.Message, "ONPREST_PUBLIC_BASE_URL") {
			t.Fatal("missing origin result", e, string(raw))
		}
	}
	for _, name := range []string{"download_report", "json_only"} {
		t.Run("REST/"+name, func(t *testing.T) {
			status, raw := do("POST", "/api/v1/capabilities/"+name, "{}", map[string]string{"Content-Type": "application/json"})
			if status != 503 {
				t.Fatal(status, string(raw))
			}
			assertMissingOrigin(t, raw)
		})
		for _, version := range []string{"2025-03-26", "2025-11-25"} {
			t.Run("MCP/"+version+"/"+name, func(t *testing.T) {
				status, raw := postMCPStatusWithProtocol(t, base, key, version, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":{}}}`)
				var response mcpToolCallResponse
				if status != 200 || json.Unmarshal(raw, &response) != nil || !response.Result.IsError || len(response.Result.Content) == 0 {
					t.Fatal(status, string(raw))
				}
				assertMissingOrigin(t, []byte(response.Result.Content[0].Text))
			})
		}
	}
	t.Run("REST/multipart", func(t *testing.T) {
		status, raw := do("POST", "/api/v1/capabilities/json_only", "--b--\r\n", map[string]string{"Content-Type": "multipart/form-data; boundary=b"})
		if status != 503 {
			t.Fatal(status, string(raw))
		}
		assertMissingOrigin(t, raw)
	})
	// Discovery keeps reporting that no usable metadata is available.
	status, raw = do("GET", "/openapi.json", "", nil)
	if status != 503 || !bytes.Contains(raw, []byte("AGENT_OFFLINE")) {
		t.Fatal("openapi", status, string(raw))
	}
	status, raw = postMCPStatusWithProtocol(t, base, key, "2025-11-25", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if status != 200 || !bytes.Contains(raw, []byte(`"error"`)) || !bytes.Contains(raw, []byte("AGENT_OFFLINE")) {
		t.Fatal("tools/list", status, string(raw))
	}
}
