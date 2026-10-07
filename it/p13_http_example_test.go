//go:build integration

package it

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/testschema"
)

func TestHTTPExecutorPublishedExampleRealBinaries(t *testing.T) {
	repo, tmp := repoRoot(t), t.TempDir()
	gatewayBin, agentBin := filepath.Join(tmp, "onprest-gateway"), filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")
	var sent atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, e := io.ReadAll(r.Body)
		if e != nil || r.Method != "POST" || r.URL.Path != "/stock" || string(raw) != `{"channel":"onprest","item":"widget-01"}` || r.Header.Get("Content-Type") != "application/json" {
			t.Error("example upstream mismatch", r.Method, r.URL.Path, string(raw), e)
			w.WriteHeader(400)
			return
		}
		sent.Add(1)
		io.WriteString(w, `{"item":"widget-01","available":7,"internal_note":"not published"}`)
	}))
	defer upstream.Close()
	read := func(name string) string {
		data, e := os.ReadFile(filepath.Join(repo, "examples/http", name))
		if e != nil {
			t.Fatal(e)
		}
		return string(data)
	}
	address := freeAddr(t)
	base := "http://" + address
	config := strings.ReplaceAll(read("agent.yaml"), "127.0.0.1:8080", address)
	config = strings.Replace(config, "http://127.0.0.1:8090", upstream.URL, 1)
	path := filepath.Join(tmp, "agent.yaml")
	writeFile(t, path, config)
	envFile := filepath.Join(tmp, "gateway.env")
	writeFile(t, envFile, strings.ReplaceAll(read("gateway.env"), "127.0.0.1:8080", address))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	validate := exec.CommandContext(ctx, agentBin, "validate", "--config", path)
	validate.Dir = tmp
	if raw, e := validate.CombinedOutput(); e != nil {
		t.Fatal(e, string(raw))
	}
	if sent.Load() != 0 {
		t.Fatal("validate sent business HTTP")
	}
	gateway, logs := startShellProcessWithOutput(t, tmp, "set -a; . "+shellQuote(envFile)+"; set +a; exec "+shellQuote(gatewayBin))
	defer stopProcess(t, gateway)
	waitForHTTP(t, base+"/healthz", "", 200)
	doctor := exec.CommandContext(ctx, agentBin, "doctor", "--config", path)
	doctor.Dir = tmp
	if raw, e := doctor.CombinedOutput(); e != nil {
		t.Fatal(e, string(raw))
	}
	if sent.Load() != 0 {
		t.Fatal("doctor sent business HTTP")
	}
	agent := startProcessInDir(t, ctx, tmp, agentBin, []string{"--config", path}, nil)
	defer stopProcess(t, agent)
	key := "orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM"
	waitForHTTP(t, base+"/openapi.json", key, 200)
	if sent.Load() != 0 {
		t.Fatal("startup sent business HTTP")
	}
	req, _ := http.NewRequest("GET", base+"/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	response, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	document, _ := io.ReadAll(response.Body)
	response.Body.Close()
	validator := testschema.OpenAPI(t, document)
	call, _ := http.NewRequest("POST", base+"/api/v1/capabilities/get_inventory", strings.NewReader(`{"item":"widget-01"}`))
	call.Header.Set("Authorization", "Bearer "+key)
	call.Header.Set("Content-Type", "application/json")
	received, err := http.DefaultClient.Do(call)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(received.Body)
	received.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	status := received.StatusCode
	if status != 200 {
		t.Fatal(status, string(raw))
	}
	testschema.HTTPResponse(t, validator, base+"/api/v1/capabilities/get_inventory", http.Header{"Content-Type": {"application/json"}}, raw)
	var envelope map[string]any
	if e := json.Unmarshal(raw, &envelope); e != nil {
		t.Fatal(e)
	}
	result := envelope["result"].(map[string]any)
	data := result["data"].(map[string]any)
	if len(data) != 2 || data["item"] != "widget-01" || data["available"] != float64(7) || len(result["files"].([]any)) != 0 {
		t.Fatal(envelope)
	}
	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
		status, body := postMCPStatusWithProtocol(t, base, key, version, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_inventory","arguments":{"item":"widget-01"}}}`)
		if status != 200 || !bytes.Contains(body, []byte("widget-01")) || !bytes.Contains(body, []byte("available")) || bytes.Contains(body, []byte("isError\":true")) || bytes.Contains(body, []byte("internal_note")) {
			t.Fatal(status, string(body))
		}
	}
	if sent.Load() != 4 {
		t.Fatal("expected exactly one send per REST/MCP call", sent.Load())
	}
	for _, private := range []string{upstream.URL, "internal_note", "channel", "widget-01"} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("gateway log leaked", private)
		}
	}
}
