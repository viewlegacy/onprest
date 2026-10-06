//go:build integration

package it

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/gateway"
	"github.com/viewlegacy/onprest/internal/testschema"
)

func TestPostgresFileOutputPublicationThroughRESTAndMCP(t *testing.T) {
	if !selectedDBForTest(t, "postgres") {
		return
	}
	db := selectedContainerDBConfig(t, "postgres")
	execDBStatements(t, "postgres", db, []string{"drop sequence if exists onprest_p09_sequence", "create sequence onprest_p09_sequence"})
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	base := "http://" + addr
	tmp := t.TempDir()
	path := writeContainerCapability(t, tmp, "postgres", db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, "select 1")
	yaml, e := osReadCapability(path)
	if e != nil {
		t.Fatal(e)
	}
	at := strings.Index(yaml, "capabilities:")
	yaml = yaml[:at] + "capabilities:\n"
	for name, delivery := range map[string]string{"mixed": "", "only": "", "direct": ", delivery: {rest: direct}", "inline": ", delivery: {mcp: inline}"} {
		data := ""
		if name == "mixed" {
			data = "      data: {}\n"
		}
		yaml += fmt.Sprintf("  %s:\n    executor: {type: db, source: primary, sql: \"select nextval('onprest_p09_sequence') as counter, :id::bigint as id, '=secret'::text as excluded\"}\n    input: {properties: {id: {type: integer}}, required: [id]}\n    output:\n      items:\n        properties:\n          id: {type: integer}\n          counter: {type: integer}\n%s      files: {format: csv, filename: report.csv%s}\n", name, data, delivery)
	}
	writeFile(t, path, yaml)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startInternalGatewayWithConfig(t, ctx, addr, secrets, time.Second, io.Discard, func(c *gateway.Config) { c.PublicBaseURL = base })
	runner, e := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: path, ReconnectEvery: 20 * time.Millisecond}, io.Discard)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("runner shutdown")
		}
	})
	waitForHTTP(t, base+"/openapi.json", secrets.APIKey, 200)
	request := func(path, body string) (int, []byte, http.Header) {
		t.Helper()
		method := "GET"
		var input io.Reader
		if body != "" {
			method = "POST"
			input = strings.NewReader(body)
		}
		r, _ := http.NewRequestWithContext(ctx, method, base+path, input)
		r.Header.Set("Authorization", "Bearer "+secrets.APIKey)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("MCP-Protocol-Version", "2025-11-25")
		resp, e := http.DefaultClient.Do(r)
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
	_, openAPIBytes, _ := request("/openapi.json", "")
	published := testschema.OpenAPI(t, openAPIBytes)
	_, toolBytes, _ := request("/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	var toolList map[string]any
	json.Unmarshal(toolBytes, &toolList)
	schemas := map[string]any{}
	for _, raw := range toolList["result"].(map[string]any)["tools"].([]any) {
		tool := raw.(map[string]any)
		schemas[tool["name"].(string)] = tool["outputSchema"]
	}
	for _, name := range []string{"mixed", "only", "direct"} {
		t.Run(name, func(t *testing.T) {
			status, b, header := request("/api/v1/capabilities/"+name, `{"id":9007199254740993}`)
			if status != 200 {
				t.Fatalf("%d %s", status, b)
			}
			testschema.HTTPResponse(t, published, base+"/api/v1/capabilities/"+name, header, b)
			if name == "direct" {
				if !bytes.HasPrefix(b, []byte("id,counter\r\n9007199254740993,")) || bytes.Contains(b, []byte("secret")) {
					t.Fatalf("CSV %s", b)
				}
				return
			}
			var out contract.SuccessEnvelope
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.UseNumber()
			if e := dec.Decode(&out); e != nil || len(out.Result.Files) != 1 {
				t.Fatal(e, string(b))
			}
			if (out.Result.Data == nil) != (name == "only") {
				t.Fatal("data publication", string(b))
			}
			if name == "mixed" {
				row := out.Result.Data.(map[string]any)["rows"].([]any)[0].(map[string]any)
				if row["id"] != json.Number("9007199254740993") || row["excluded"] != nil {
					t.Fatal(row)
				}
			}
			uri := out.Result.Files[0].URI
			status, csv, _ := request(strings.TrimPrefix(uri, base), "")
			if status != 200 || !bytes.HasPrefix(csv, []byte("id,counter\r\n9007199254740993,")) || bytes.Contains(csv, []byte("secret")) {
				t.Fatalf("CSV %d %s", status, csv)
			}
			_, again, _ := request(strings.TrimPrefix(uri, base), "")
			if !bytes.Equal(csv, again) {
				t.Fatal("GET re-executed sequence")
			}
		})
	}
	for _, name := range []string{"mixed", "only", "direct", "inline"} {
		status, body, _ := request("/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":{"id":9007199254740993}}}`)
		var response map[string]any
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if status != 200 || decoder.Decode(&response) != nil {
			t.Fatal("MCP schema response", status)
		}
		result := response["result"].(map[string]any)
		testschema.JSON(t, schemas[name], result["structuredContent"], true)
		content := result["content"].([]any)
		var envelope any
		decoder = json.NewDecoder(strings.NewReader(content[0].(map[string]any)["text"].(string)))
		decoder.UseNumber()
		if e := decoder.Decode(&envelope); e != nil {
			t.Fatal(e)
		}
		testschema.JSON(t, schemas[name], envelope, true)
	}
	status, b, _ := request("/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"inline","arguments":{"id":9007199254740993}}}`)
	var rpc struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type     string `json:"type"`
				Resource struct {
					Text string `json:"text"`
					Blob string `json:"blob"`
				} `json:"resource"`
			} `json:"content"`
		} `json:"result"`
	}
	if status != 200 || json.Unmarshal(b, &rpc) != nil || rpc.Result.IsError || len(rpc.Result.Content) != 2 {
		t.Fatalf("MCP %d %s", status, b)
	}
	resource := rpc.Result.Content[1].Resource
	csv := []byte(resource.Text)
	if resource.Blob != "" {
		csv, _ = base64.StdEncoding.DecodeString(resource.Blob)
	}
	if !bytes.HasPrefix(csv, []byte("id,counter\r\n9007199254740993,")) || bytes.Contains(csv, []byte("secret")) {
		t.Fatalf("MCP CSV %s", csv)
	}
}
