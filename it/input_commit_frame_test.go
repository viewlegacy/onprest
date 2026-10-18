//go:build integration

package it

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
)

// A file-carrying call commits its full JSON params in one internal frame.
// HTML-significant text and params above the management frame size must reach
// the Agent unchanged, and a commit above the frame bound must fail only its
// own request: the Agent connection and earlier file references survive.
func TestInputCommitCarriesLargeParamsWithoutDroppingTheAgentConnection(t *testing.T) {
	repo, tmp := repoRoot(t), t.TempDir()
	gatewayBin, agentBin := filepath.Join(tmp, "onprest-gateway"), filepath.Join(tmp, "onprest-agent")
	buildBinary(t, repo, gatewayBin, "./cmd/gateway")
	buildBinary(t, repo, agentBin, "./cmd/agent")
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	base := "http://" + addr
	if e := os.Mkdir(filepath.Join(tmp, "exchange"), 0o700); e != nil {
		t.Fatal(e)
	}
	configPath := filepath.Join(tmp, "agent.yaml")
	writeFile(t, configPath, fmt.Sprintf(`schema_version: 1
service: {title: Commit frame, version: "1"}
gateway: {url: "ws://%s/ws/agent", agent_private_key: "%s"}
sources:
  folders:
    exchange: {root: ./exchange, access: read_write}
capabilities:
  save_note:
    executor:
      type: file
      write: [{input: /document, source: exchange, name_from: input, overwrite: true}]
      read: {written: true}
    input:
      properties:
        note: {type: string}
        document: {type: file}
      required: [note, document]
    output: {files: {}}
    policy: {max_input_bytes: 32MiB}
`, addr, secrets.AgentPrivateKey))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	gateway := startProcess(t, ctx, gatewayBin, nil, []string{"GATEWAY_ADDR=" + addr, "GATEWAY_AGENT_PUBLIC_KEY=" + secrets.AgentPublicKey, "GATEWAY_API_KEYS_JSON=" + secrets.APIKeysJSON, "ONPREST_PUBLIC_BASE_URL=" + base, "GATEWAY_MAX_REQUEST_BODY_BYTES=" + fmt.Sprint(40<<20)})
	defer stopProcess(t, gateway)
	waitForHTTP(t, base+"/healthz", "", 200)
	agent := startProcessInDir(t, ctx, tmp, agentBin, []string{"--config", configPath}, nil)
	defer stopProcess(t, agent)
	waitForAgentConnected(t, base)
	waitForHTTP(t, base+"/openapi.json", secrets.APIKey, 200)

	post := func(contentType string, body []byte) (int, []byte) {
		t.Helper()
		request, _ := http.NewRequestWithContext(ctx, "POST", base+"/api/v1/capabilities/save_note", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+secrets.APIKey)
		request.Header.Set("Content-Type", contentType)
		response, e := http.DefaultClient.Do(request)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, raw
	}
	document := func(name string) map[string]any {
		return map[string]any{"name": name, "mime_type": "text/plain", "data": base64.StdEncoding.EncodeToString([]byte(name))}
	}
	jsonCall := func(note, name string) (int, []byte) {
		body, _ := json.Marshal(map[string]any{"note": note, "document": document(name)})
		return post("application/json", body)
	}
	saved := func(t *testing.T, status int, raw []byte) string {
		t.Helper()
		var envelope contract.SuccessEnvelope
		if status != 200 || json.Unmarshal(raw, &envelope) != nil || len(envelope.Result.Files) != 1 {
			t.Fatalf("save %d %.300s", status, raw)
		}
		return envelope.Result.Files[0].URI
	}
	fetch := func(uri string) int {
		request, _ := http.NewRequestWithContext(ctx, "GET", uri, nil)
		request.Header.Set("Authorization", "Bearer "+secrets.APIKey)
		response, e := http.DefaultClient.Do(request)
		if e != nil {
			t.Fatal(e)
		}
		response.Body.Close()
		return response.StatusCode
	}
	status, raw := jsonCall("before", "before.txt")
	earlier := saved(t, status, raw)

	html := strings.Repeat("<", 200000)
	large := strings.Repeat("a", 1<<20+4096)
	for name, note := range map[string]string{"html": html, "large": large} {
		t.Run(name+"/rest_json", func(t *testing.T) {
			status, raw := jsonCall(note, name+"-json.txt")
			saved(t, status, raw)
		})
		t.Run(name+"/rest_multipart", func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="note"`}, "Content-Type": {"application/json"}})
			encoded, _ := json.Marshal(note)
			part.Write(encoded)
			part, _ = writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="document"; filename="` + name + `-multipart.txt"`}, "Content-Type": {"text/plain"}})
			part.Write([]byte("multipart"))
			writer.Close()
			status, raw = post(writer.FormDataContentType(), body.Bytes())
			saved(t, status, raw)
		})
		t.Run(name+"/mcp", func(t *testing.T) {
			call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "save_note", "arguments": map[string]any{"note": note, "document": document(name + "-mcp.txt")}}})
			raw := postMCPPayloadWithProtocol(t, base, secrets.APIKey, "2025-11-25", string(call))
			if bytes.Contains(raw, []byte(`"isError":true`)) {
				t.Fatalf("MCP %.300s", raw)
			}
		})
	}
	t.Run("above_commit_frame_bound", func(t *testing.T) {
		status, raw := jsonCall(strings.Repeat("b", 17<<20), "oversized.txt")
		var envelope contract.ErrorEnvelope
		if status != http.StatusUnprocessableEntity || json.Unmarshal(raw, &envelope) != nil || envelope.Error.Code != "LIMIT_EXCEEDED" || envelope.Error.Outcome != "not_started" {
			t.Fatalf("oversized commit %d %.300s", status, raw)
		}
		if _, e := os.Stat(filepath.Join(tmp, "exchange", "oversized.txt")); !os.IsNotExist(e) {
			t.Fatal("oversized request executed", e)
		}
	})
	// One connection carried every call: the first reference is still valid.
	if status := fetch(earlier); status != 200 {
		t.Fatalf("earlier reference after large commits: %d", status)
	}
	status, raw = jsonCall("after", "after.txt")
	saved(t, status, raw)
}
