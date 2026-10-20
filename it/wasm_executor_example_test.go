//go:build integration

package it

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWasmExecutorPublishedExampleRealBinaries(t *testing.T) {
	repo, tmp := repoRoot(t), t.TempDir()
	gatewayBin, agentBin := filepath.Join(tmp, "onprest-gateway"), filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")
	module := filepath.Join(tmp, "module.wasm")
	build := exec.Command("go", "run", "./examples/wasm/build", "-output", module)
	build.Dir = repo
	if raw, e := build.CombinedOutput(); e != nil {
		t.Fatal(e, string(raw))
	}
	read := func(name string) string {
		raw, e := os.ReadFile(filepath.Join(repo, "examples/wasm", name))
		if e != nil {
			t.Fatal(e)
		}
		return string(raw)
	}
	address := freeAddr(t)
	base := "http://" + address
	path := filepath.Join(tmp, "agent.yaml")
	writeFile(t, path, strings.ReplaceAll(read("agent.yaml"), "127.0.0.1:8080", address))
	os.Mkdir(filepath.Join(tmp, "files"), 0700)
	writeFile(t, filepath.Join(tmp, "files", "message.txt"), read("files/message.txt"))
	envFile := filepath.Join(tmp, "gateway.env")
	writeFile(t, envFile, strings.ReplaceAll(read("gateway.env"), "127.0.0.1:8080", address))
	unrelated := filepath.Join(tmp, "unrelated")
	os.Mkdir(unrelated, 0700)
	os.Mkdir(filepath.Join(unrelated, "files"), 0700)
	writeFile(t, filepath.Join(unrelated, "files", "message.txt"), "wrong cwd")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	validate := exec.CommandContext(ctx, agentBin, "validate", "--config", path)
	validate.Dir = unrelated
	validate.Env = append(os.Environ(), "PWD="+unrelated)
	if raw, e := validate.CombinedOutput(); e != nil {
		detail, _ := os.ReadFile(filepath.Join(tmp, "onprest-agent.validate.log"))
		t.Fatal(e, string(raw), string(detail))
	}
	gateway, _ := startShellProcessWithOutput(t, tmp, "set -a; . "+shellQuote(envFile)+"; set +a; exec "+shellQuote(gatewayBin))
	defer stopProcess(t, gateway)
	waitForHTTP(t, base+"/healthz", "", 200)
	doctor := exec.CommandContext(ctx, agentBin, "doctor", "--config", path)
	doctor.Dir = unrelated
	if raw, e := doctor.CombinedOutput(); e != nil {
		t.Fatal(e, string(raw))
	}
	process := startProcessInDir(t, ctx, unrelated, agentBin, []string{"--config", path}, []string{"PWD=" + unrelated})
	defer stopProcess(t, process)
	key := "orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM"
	waitForHTTP(t, base+"/openapi.json", key, 200)
	// Execution owns the validated module bytes, even when the deployed file changes.
	os.WriteFile(module, []byte("changed after prepare"), 0600)
	request := func(method, url, body string) (int, []byte) {
		q, _ := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
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
	arguments := `{"name":"world","attachment":{"name":"input.bin","mime_type":"application/octet-stream","data":"AP8BAg=="}}`
	status, raw := request("POST", base+"/api/v1/capabilities/wasm_copy", arguments)
	if status != 200 {
		detail, _ := os.ReadFile(filepath.Join(tmp, "onprest-agent.log"))
		t.Fatal(status, string(raw), string(detail))
	}
	var envelope map[string]any
	if e := json.Unmarshal(raw, &envelope); e != nil {
		t.Fatal(e)
	}
	result := envelope["result"].(map[string]any)
	data := result["data"].(map[string]any)
	if len(data) != 2 || data["greeting"] != "Hello, world" || data["input_bytes"] != float64(4) || bytes.Contains(raw, []byte("private")) {
		t.Fatal(string(raw))
	}
	files := result["files"].([]any)
	if len(files) != 1 {
		t.Fatal(files)
	}
	file := files[0].(map[string]any)
	if file["name"] != "copy.bin" || file["size_bytes"] != float64(4) {
		t.Fatal(file)
	}
	status, body := request("GET", file["uri"].(string), "")
	if status != 200 || !bytes.Equal(body, []byte{0, 255, 1, 2}) {
		t.Fatal(status, body)
	}
	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
		status, body := postMCPStatusWithProtocol(t, base, key, version, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wasm_copy","arguments":`+arguments+`}}`)
		if status != 200 || !bytes.Contains(body, []byte("Hello, world")) || !bytes.Contains(body, []byte("copy.bin")) || bytes.Contains(body, []byte(`isError":true`)) || bytes.Contains(body, []byte("private")) {
			t.Fatal(version, status, string(body))
		}
	}
	status, raw = request("GET", base+"/openapi.json", "")
	if status != 200 || !bytes.Contains(raw, []byte("wasm_copy")) {
		t.Fatal(status, string(raw))
	}
}
