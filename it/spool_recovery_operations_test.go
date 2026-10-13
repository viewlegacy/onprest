//go:build integration

package it

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceFreeSpoolRecoveryAndControlReset(t *testing.T) {
	repo, tmp := repoRoot(t), t.TempDir()
	gatewayBin, agentBin := filepath.Join(tmp, "onprest-gateway"), filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	base := "http://" + addr
	config := filepath.Join(tmp, "capability.yaml")
	writeFile(t, filepath.Join(tmp, "small.bin"), "snapshot-bytes")
	if err := os.WriteFile(filepath.Join(tmp, "large.bin"), bytes.Repeat([]byte("x"), 8<<20), 0600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, config, fmt.Sprintf(`schema_version: 1
service: {title: Source-free operations, version: "2.0"}
gateway: {url: "ws://%s/ws/agent", agent_private_key: "%s"}
runtime:
  spool_dir: ./spool
  identity_dir: ./identity
  spool_total_bytes: 4MiB
  transfer_timeout: 5s
  control_limits:
    transfer: {requests: 1, per: 24h, burst: 3, max_concurrent: 4}
sources:
  folders:
    business: {root: ., access: read_write}
capabilities:
  read_small:
    executor: {type: file, read: {source: business, selector: {name: small.bin}}}
    output: {files: {}}
  read_full:
    executor: {type: file, read: {source: business, selector: {name: large.bin}}}
    output: {files: {}}
  save:
    executor: {type: file, write: [{input: /document, source: business, name: completed.bin}]}
    input: {properties: {document: {type: file}}, required: [document]}
`, addr, secrets.AgentPrivateKey))
	runtimePath := filepath.Join(tmp, "empty-path")
	os.Mkdir(runtimePath, 0700)
	gateway, gatewayLog := startProcessWithOutput(t, tmp, gatewayBin, nil, []string{"PATH=" + runtimePath, "GATEWAY_ADDR=" + addr, "GATEWAY_AGENT_PUBLIC_KEY=" + secrets.AgentPublicKey, "GATEWAY_API_KEYS_JSON=" + secrets.APIKeysJSON, "ONPREST_PUBLIC_BASE_URL=" + base, "GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100", "GATEWAY_RATE_LIMIT_BURST=100"})
	defer stopProcess(t, gateway)
	waitForHTTP(t, base+"/healthz", "", 200)
	agent, agentLog := startProcessWithOutput(t, tmp, agentBin, []string{"--config", config}, []string{"PATH=" + runtimePath})
	defer func() { stopProcess(t, agent) }()
	waitForAgentConnected(t, base)
	waitForHTTP(t, base+"/openapi.json", secrets.APIKey, 200)
	request := func(method, url, body string) (int, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, url, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+secrets.APIKey)
		r.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, raw
	}
	artifact := func() string {
		t.Helper()
		status, raw := request("POST", base+"/api/v1/capabilities/read_small", `{}`)
		if status != 200 {
			t.Fatalf("read %d %s", status, raw)
		}
		var envelope struct {
			Result struct {
				Files []struct {
					URI string `json:"uri"`
				}
			}
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Result.Files) != 1 {
			t.Fatal("envelope", string(raw))
		}
		return envelope.Result.Files[0].URI
	}
	// Atomic business files must outlive snapshots, request cleanup and restart.
	status, raw := request("POST", base+"/api/v1/capabilities/save", fmt.Sprintf(`{"document":{"name":"private-input.bin","mime_type":"application/octet-stream","data":%q}}`, base64.StdEncoding.EncodeToString([]byte("completed-business"))))
	if status != 200 {
		t.Fatalf("save %d %s", status, raw)
	}
	uri := artifact()
	for index := 0; index < 3; index++ {
		status, raw = request("POST", base+"/api/v1/capabilities/read_full", `{}`)
		if status != 422 || !bytes.Contains(raw, []byte(`"code":"LIMIT_EXCEEDED"`)) {
			t.Fatalf("quota %d %s", status, raw)
		}
	}
	// The save input consumed one transfer admission. Two downloads exhaust the
	// same fixed bucket, and refusal cannot evict the retained live snapshot.
	for index := 0; index < 2; index++ {
		status, raw = request("GET", uri, "")
		if status != 200 || string(raw) != "snapshot-bytes" {
			t.Fatalf("download %d %s", status, raw)
		}
	}
	status, raw = request("GET", uri, "")
	if status != 429 || !bytes.Contains(raw, []byte("RATE_LIMITED")) {
		t.Fatalf("control limit %d %s", status, raw)
	}
	if status, _ := request("GET", base+"/healthz", ""); status != 200 {
		t.Fatal("gateway stopped after refusal")
	}
	// Restart resets nonpersistent buckets and invalidates references. A fresh
	// request is usable immediately even though the prior bucket's period is 24h.
	stopProcess(t, agent)
	agent, agentLog = startProcessWithOutput(t, tmp, agentBin, []string{"--config", config}, []string{"PATH=" + runtimePath})
	waitForAgentConnected(t, base)
	waitForHTTP(t, base+"/openapi.json", secrets.APIKey, 200)
	status, _ = request("GET", uri, "")
	if status == 200 {
		t.Fatal("reference survived restart")
	}
	newURI := artifact()
	status, raw = request("GET", newURI, "")
	if status != 200 || string(raw) != "snapshot-bytes" {
		t.Fatalf("restart bucket not reset: %d %s", status, raw)
	}
	completed, err := os.ReadFile(filepath.Join(tmp, "completed.bin"))
	if err != nil || string(completed) != "completed-business" {
		t.Fatal("completed business file lost")
	}
	// A second restart provides a clean transfer bucket for a partially received
	// multipart request. Kill after real input bytes reach disk, then recover.
	stopProcess(t, agent)
	agent, agentLog = startProcessWithOutput(t, tmp, agentBin, []string{"--config", config}, []string{"PATH=" + runtimePath})
	waitForAgentConnected(t, base)
	waitForHTTP(t, base+"/openapi.json", secrets.APIKey, 200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	r, _ := http.NewRequestWithContext(ctx, "POST", base+"/api/v1/capabilities/save", reader)
	r.Header.Set("Authorization", "Bearer "+secrets.APIKey)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=spool-input")
	ended := make(chan struct{})
	go func() {
		resp, _ := http.DefaultClient.Do(r)
		if resp != nil {
			resp.Body.Close()
		}
		close(ended)
	}()
	sent := make(chan struct{})
	go func() {
		fmt.Fprint(writer, "--spool-input\r\nContent-Disposition: form-data; name=\"document\"; filename=\"private-orphan.bin\"\r\nContent-Type: application/octet-stream\r\n\r\n")
		writer.Write(bytes.Repeat([]byte("x"), 256<<10))
		close(sent)
	}()
	spool := filepath.Join(tmp, "spool")
	received := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var size int64
		filepath.WalkDir(spool, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				info, e := entry.Info()
				if e == nil {
					size += info.Size()
				}
			}
			return nil
		})
		if size >= 128<<10 {
			received = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !received {
		cancel()
		writer.Close()
		<-ended
		t.Fatalf("partial input never reached spool: %s", agentLog.String())
	}
	if err := agent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	agent.Wait()
	cancel()
	writer.Close()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request remained after kill")
	}
	select {
	case <-sent:
	case <-time.After(3 * time.Second):
		t.Fatal("input producer remained after kill")
	}
	agent, agentLog = startProcessWithOutput(t, tmp, agentBin, []string{"--config", config}, []string{"PATH=" + runtimePath})
	waitForAgentConnected(t, base)
	waitForHTTP(t, base+"/openapi.json", secrets.APIKey, 200)
	var remaining int64
	filepath.WalkDir(spool, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			info, e := entry.Info()
			if e == nil {
				remaining += info.Size()
			}
		}
		return nil
	})
	if remaining >= 128<<10 {
		t.Fatalf("orphan bytes remain: %d", remaining)
	}
	completed, err = os.ReadFile(filepath.Join(tmp, "completed.bin"))
	if err != nil || string(completed) != "completed-business" {
		t.Fatal("recovery removed completed business file")
	}
	newURI = artifact()
	status, raw = request("GET", newURI, "")
	if status != 200 || string(raw) != "snapshot-bytes" {
		t.Fatalf("recovered Agent unusable %d %s", status, raw)
	}
	for _, secret := range []string{"private-input.bin", "private-orphan.bin", "completed-business", tmp} {
		if strings.Contains(gatewayLog.String(), secret) {
			t.Fatalf("gateway diagnostic leaked %q", secret)
		}
	}
}
