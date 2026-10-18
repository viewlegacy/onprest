//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/gateway"
)

const wideInputCapability = `
  wide:
    executor: {type: file, write: [{input: /document, source: exchange, name: saved.bin, overwrite: true}], read: {written: true}}
    input: {properties: {document: {type: file, max_bytes: 16MiB}, value: {}}, required: [document]}
    policy: {max_json_depth: 64, max_json_items: 100200, max_input_bytes: 2MiB, max_input_file_bytes: 16MiB, max_total_input_file_bytes: 16MiB}
    output: {files: {}}
  wide_json:
    executor: {type: file, read: {source: exchange, selector: {name: saved.bin}}}
    input: {properties: {value: {}}}
    policy: {max_json_depth: 64, max_json_items: 100200, max_input_bytes: 2MiB}
    output: {files: {}}
`

func TestInputDeliveryConfiguredStructuralAndEncodedWireLimits(t *testing.T) {
	// The input budget must cover decoding and transferring a 16MiB encoded MCP
	// body. That CPU work takes under 2s normally but 15-20s under the race
	// detector on an idle host, so the budget leaves room for a loaded race run.
	// Every request still completes on its own response, not on this budget.
	f := productionInputHTTPAdjusted(t, nil, func(yaml string) string {
		return strings.Replace(yaml, "transfer_timeout: 2s", "transfer_timeout: 120s", 1) + wideInputCapability
	}, gateway.Config{MaxRequestBodyBytes: 25 << 20})
	for _, test := range []struct {
		name, value string
		status      int
	}{
		{"depth40", strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40), 200},
		{"depth65", strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65), 422},
		{"items100010", "[" + strings.Repeat("0,", 100009) + "0]", 200},
		{"items100201", "[" + strings.Repeat("0,", 100200) + "0]", 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := `{"document":{"name":"` + test.name + `.bin","mime_type":"application/octet-stream","data":"AQ=="},"value":` + test.value + `}`
			for _, route := range []string{"rest", "mcp", "multipart"} {
				path, body, media, version := "/api/v1/capabilities/wide", input, "application/json", ""
				if route == "mcp" {
					path = "/mcp"
					version = "2025-11-25"
					body = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wide","arguments":` + input + `}}`
				}
				if route == "multipart" {
					media = "multipart/form-data; boundary=b"
					body = "--b\r\nContent-Disposition: form-data; name=\"document\"; filename=\"" + test.name + ".bin\"\r\nContent-Type: application/octet-stream\r\n\r\n\x01\r\n--b\r\nContent-Disposition: form-data; name=\"value\"\r\nContent-Type: application/json\r\n\r\n" + test.value + "\r\n--b--\r\n"
				}
				status, raw, _ := f.request(t, path, media, strings.NewReader(body), version)
				if route == "mcp" {
					if status != 200 || (bytes.Contains(raw, []byte(`"isError":true`))) != (test.status != 200) {
						t.Fatal(route, status, string(raw))
					}
					if test.status != 200 && !bytes.Contains(raw, []byte("LIMIT_EXCEEDED")) {
						t.Fatal(string(raw))
					}
				} else if status != test.status {
					t.Fatal(route, status, string(raw))
				}
				t.Logf("%s %s encoded_bytes=%d HTTP=%d policy_expectation=%d", test.name, route, len(body), status, test.status)
				if route != "multipart" {
					plain := `{"value":` + test.value + `}`
					path = "/api/v1/capabilities/wide_json"
					if route == "mcp" {
						path = "/mcp"
						plain = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wide_json","arguments":` + plain + `}}`
					}
					status, raw, _ = f.request(t, path, "application/json", strings.NewReader(plain), version)
					if route == "mcp" {
						if status != 200 || bytes.Contains(raw, []byte(`"isError":true`)) != (test.status != 200) {
							t.Fatal("fileless", route, status, string(raw))
						}
					} else if status != test.status {
						t.Fatal("fileless", route, status, string(raw))
					}
					t.Logf("fileless %s %s HTTP=%d policy_expectation=%d", test.name, route, status, test.status)
				}
			}
		})
	}
	for _, test := range []struct {
		name         string
		size, status int
	}{{"over16MiBEncoded", 12<<20 + 1, 200}, {"decodedOver16MiB", 16<<20 + 1, 422}} {
		t.Run(test.name, func(t *testing.T) {
			input := `{"document":{"name":"` + test.name + `.bin","mime_type":"application/octet-stream","data":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, test.size)) + `"}}`
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wide","arguments":` + input + `}}`
			if len(body) <= 16<<20 || len(body) > 25<<20 {
				t.Fatal("test wire budget", len(body))
			}
			status, raw, _ := f.request(t, "/mcp", "application/json", strings.NewReader(body), "2025-11-25")
			var response struct {
				Result struct {
					IsError bool `json:"isError"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			if status != 200 || json.Unmarshal(raw, &response) != nil || response.Result.IsError != (test.status != 200) {
				t.Fatal(status, string(raw))
			}
			if test.status == 200 {
				f.assertSaved(t, []byte(response.Result.Content[0].Text), bytes.Repeat([]byte{7}, test.size))
			} else if !bytes.Contains(raw, []byte("LIMIT_EXCEEDED")) {
				t.Fatal(string(raw))
			}
			t.Logf("%s decoded_bytes=%d encoded_bytes=%d HTTP=%d policy_expectation=%d", test.name, test.size, len(body), status, test.status)
		})
	}
}

func TestInputDeliveryJSONAndMCPBodyAdmissionDeadline(t *testing.T) {
	f := productionInputHTTPAdjusted(t, nil, func(yaml string) string {
		return strings.Replace(yaml, "transfer_timeout: 2s", "transfer_timeout: 500ms", 1)
	}, gateway.Config{MaxConcurrentBodyReaders: 1})
	for _, route := range []string{"rest", "mcp"} {
		t.Run(route, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pw.Close()
			path := "/api/v1/capabilities/save"
			if route == "mcp" {
				path = "/mcp"
			}
			ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", f.base+path, pr)
			req.Header.Set("Authorization", "Bearer input-key")
			req.Header.Set("Content-Type", "application/json")
			if route == "mcp" {
				req.Header.Set("MCP-Protocol-Version", "2025-11-25")
			}
			before := f.runner.store.Usage()
			started := time.Now()
			done := make(chan int, 1)
			go func() {
				resp, e := http.DefaultClient.Do(req)
				if e != nil {
					done <- 0
					return
				}
				defer resp.Body.Close()
				raw, _ := io.ReadAll(resp.Body)
				if route == "mcp" && !bytes.Contains(raw, []byte("PARSE_ERROR")) {
					done <- 0
					return
				}
				done <- resp.StatusCode
			}()
			if _, e := io.WriteString(pw, `{"`); e != nil {
				t.Fatal(e)
			}
			select {
			case status := <-done:
				want := 504
				if route == "mcp" {
					want = 200
				}
				if status != want {
					t.Fatal(status, want)
				}
			case <-ctx.Done():
				t.Fatal("input body used 15s instead of input admission deadline")
			}
			if time.Since(started) > time.Second || f.runner.store.Usage() != before {
				t.Fatal("body deadline/spool", time.Since(started), f.runner.store.Usage())
			}
			status, raw, _ := f.request(t, "/api/v1/capabilities/save", "application/json", strings.NewReader(inlineInput([]byte{1})), "")
			if status != 200 {
				t.Fatal("body slot did not recover", status, string(raw))
			}
			t.Logf("%s stalled chunked body ended within input deadline; next input succeeded", route)
		})
	}
}
