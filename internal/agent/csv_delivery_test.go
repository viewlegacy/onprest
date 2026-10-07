//go:build linux || darwin

package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/gateway"
	"github.com/viewlegacy/onprest/internal/testschema"
)

func productionCSVHTTP(t *testing.T) *inputHTTPFixture {
	t.Helper()
	return productionInputHTTPAdjusted(t, nil, func(yaml string) string {
		yaml = strings.Replace(yaml, "runtime: {", "runtime: {control_limits: {read: {requests: 1000, per: 1s, burst: 1000, max_concurrent: 16}, write: {requests: 1000, per: 1s, burst: 1000, max_concurrent: 16}, transfer: {requests: 1000, per: 1s, burst: 1000, max_concurrent: 16}}, ", 1)
		for _, source := range []string{"folder", "single", "array"} {
			for _, output := range []string{"mixed", "data", "direct", "inline"} {
				read := `source: exchange, selector: {name_regex: 'part_[0-9]+[.]csv', take: 4}`
				input := `filters: {properties: {secret: {type: string}}, required: [secret]}`
				required := "filters"
				if source != "folder" {
					read = "input: /documents"
					file := `type: file, extensions: [csv]`
					if source == "array" {
						file = `items: {type: file, extensions: [csv]}, minItems: 1, maxItems: 4`
					}
					input += ", documents: {" + file + "}"
					required += ", documents"
				}
				public := `items: {properties: {id: {type: string}, amount: {type: integer}}}`
				switch output {
				case "mixed":
					public += `, data: {}, files: {format: csv, filename: sales.csv}`
				case "data":
					public += `, data: {}`
				case "direct":
					public += `, files: {format: csv, filename: sales.csv, delivery: {rest: direct}}`
				case "inline":
					public += `, files: {format: csv, filename: sales.csv, delivery: {mcp: inline}}`
				}
				yaml += fmt.Sprintf(`  csv_%s_%s:
    executor: {type: csv, read: {%s}, csv: {columns: {amount: {type: integer}}}, where: [{column: secret, op: eq, input: /filters/secret}]}
    input: {properties: {%s}, required: [%s]}
    output: {%s}
    policy: {max_rows: 2, rate_limit: {requests: 1000, per: 1s, burst: 1000}}
`, source, output, read, input, required, public)
			}
		}
		return yaml
	}, gateway.Config{MaxRequestBodyBytes: 64 << 10})
}
func csvInlineParams(source string, files ...[]byte) string {
	in := map[string]any{"filters": map[string]any{"secret": "allow"}}
	values := []any{}
	for i, file := range files {
		values = append(values, map[string]any{"name": fmt.Sprintf("upload_%d.csv", i), "mime_type": "text/csv", "data": base64.StdEncoding.EncodeToString(file)})
	}
	if source == "single" {
		in["documents"] = values[0]
	} else if source == "array" {
		in["documents"] = values
	}
	b, _ := json.Marshal(in)
	return string(b)
}
func csvMultipartParams(t *testing.T, files ...[]byte) (string, []byte) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for i, file := range files {
		p, e := w.CreatePart(textproto.MIMEHeader{"Content-Disposition": {fmt.Sprintf(`form-data; name="documents"; filename="upload_%d.csv"`, i)}, "Content-Type": {"text/csv"}})
		if e != nil {
			t.Fatal(e)
		}
		p.Write(file)
	}
	p, e := w.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="filters"`}, "Content-Type": {"application/json"}})
	if e != nil {
		t.Fatal(e)
	}
	io.WriteString(p, `{"secret":"allow"}`)
	w.Close()
	return w.FormDataContentType(), b.Bytes()
}
func csvEnvelope(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var envelope map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if e := d.Decode(&envelope); e != nil {
		t.Fatal(string(raw), e)
	}
	return envelope
}
func csvGet(t *testing.T, f *inputHTTPFixture, uri string) []byte {
	t.Helper()
	req, _ := http.NewRequest("GET", uri, nil)
	req.Header.Set("Authorization", "Bearer p10-key")
	r, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	b, e := io.ReadAll(r.Body)
	if e != nil || r.StatusCode != 200 {
		t.Fatal(r.StatusCode, e, string(b))
	}
	return b
}
func assertCSVResult(t *testing.T, f *inputHTTPFixture, envelope map[string]any, source, output string, wantRows int, wantCSV string) {
	t.Helper()
	if envelope["error"] != nil {
		t.Fatal(envelope)
	}
	result := envelope["result"].(map[string]any)
	if output == "mixed" || output == "data" {
		data := result["data"].(map[string]any)
		rows := data["rows"].([]any)
		if len(rows) != wantRows || data["count"] != json.Number(fmt.Sprint(wantRows)) {
			t.Fatal(data)
		}
		for _, row := range rows {
			if len(row.(map[string]any)) != 2 || row.(map[string]any)["secret"] != nil {
				t.Fatal(row)
			}
		}
		if wantRows == 2 {
			if rows[0].(map[string]any)["id"] != "=cmd" || rows[0].(map[string]any)["amount"] != json.Number("9223372036854775807") || rows[1].(map[string]any)["id"] != "00123" {
				t.Fatal(rows)
			}
		}
	} else if result["data"] != nil {
		t.Fatal(result)
	}
	files := result["files"].([]any)
	if output == "data" {
		if len(files) != 0 {
			t.Fatal(files)
		}
		return
	}
	if len(files) != 1 {
		t.Fatal(files)
	}
	descriptor := files[0].(map[string]any)
	hash := sha256.Sum256([]byte(wantCSV))
	if descriptor["name"] != "sales.csv" || descriptor["size_bytes"] != json.Number(fmt.Sprint(len(wantCSV))) || descriptor["sha256"] != hex.EncodeToString(hash[:]) {
		t.Fatal(descriptor)
	}
	if output != "inline" {
		if got := csvGet(t, f, descriptor["uri"].(string)); string(got) != wantCSV {
			t.Fatal(string(got), wantCSV)
		}
	}
}
func TestCSVDeliveryProductionFolderUploadRESTAndMCP(t *testing.T) {
	f := productionCSVHTTP(t)
	a := []byte("id,amount,secret\r\n=cmd,9223372036854775807,allow\r\nskip,0,deny\r\n")
	b := []byte("id,amount,secret\r\n00123,7,allow\r\n")
	joined := append(append([]byte{}, a...), b[len("id,amount,secret\r\n"):]...)
	for name, body := range map[string][]byte{"part_1.csv": a, "part_2.csv": b} {
		os.WriteFile(filepath.Join(f.dir, "exchange", name), body, 0600)
	}
	wantCSV := "id,amount\r\n'=cmd,9223372036854775807\r\n00123,7\r\n"
	req, _ := http.NewRequest("GET", f.base+"/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer p10-key")
	r, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	doc, _ := io.ReadAll(r.Body)
	r.Body.Close()
	validator := testschema.OpenAPI(t, doc)
	for _, source := range []string{"folder", "single", "array"} {
		files := [][]byte{joined}
		if source == "array" {
			files = [][]byte{a, b}
		}
		for _, output := range []string{"mixed", "data", "direct"} {
			t.Run(source+"_"+output, func(t *testing.T) {
				path := "/api/v1/capabilities/csv_" + source + "_" + output
				status, raw, h := f.request(t, path, "application/json", strings.NewReader(csvInlineParams(source, files...)), "")
				if status != 200 {
					t.Fatal(status, string(raw))
				}
				testschema.HTTPResponse(t, validator, f.base+path, h, raw)
				if output == "direct" {
					if string(raw) != wantCSV || h.Get("Content-Disposition") == "" {
						t.Fatal(string(raw), h)
					}
				} else {
					assertCSVResult(t, f, csvEnvelope(t, raw), source, output, 2, wantCSV)
				}
			})
		}
		if source != "folder" {
			media, wire := csvMultipartParams(t, files...)
			status, raw, _ := f.request(t, "/api/v1/capabilities/csv_"+source+"_mixed", media, bytes.NewReader(wire), "")
			if status != 200 {
				t.Fatal(status, string(raw))
			}
			assertCSVResult(t, f, csvEnvelope(t, raw), source, "mixed", 2, wantCSV)
		}
		for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
			for _, output := range []string{"mixed", "inline"} {
				name := "csv_" + source + "_" + output
				status, raw, _ := f.request(t, "/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+csvInlineParams(source, files...)+`}}`), version)
				rpc := csvEnvelope(t, raw)
				result, ok := rpc["result"].(map[string]any)
				if status != 200 || !ok || result["isError"] == true {
					t.Fatal(version, string(raw))
				}
				content := result["content"].([]any)
				envelope := csvEnvelope(t, []byte(content[0].(map[string]any)["text"].(string)))
				assertCSVResult(t, f, envelope, source, output, 2, wantCSV)
				snapshot, _ := f.runner.versioned.Snapshot(name)
				schema := snapshot.Definition().SuccessSchema("mcp")
				testschema.JSON(t, schema, envelope, true)
				if version != "2025-03-26" {
					testschema.JSON(t, schema, result["structuredContent"], true)
				} else if result["structuredContent"] != nil {
					t.Fatal("old MCP structured content")
				}
				if output == "inline" {
					resource := content[1].(map[string]any)["resource"].(map[string]any)
					var got string
					if v, ok := resource["text"].(string); ok {
						got = v
					} else {
						decoded, e := base64.StdEncoding.DecodeString(resource["blob"].(string))
						if e != nil {
							t.Fatal(e)
						}
						got = string(decoded)
					}
					if got != wantCSV {
						t.Fatal(got)
					}
				}
			}
		}
	}
	entries, _ := os.ReadDir(filepath.Join(f.dir, "exchange"))
	if len(entries) != 2 {
		t.Fatal("CSV upload saved into business folder", entries)
	}
	if strings.Contains(f.logs.String(), "=cmd") || strings.Contains(f.logs.String(), "upload_") {
		t.Fatal("CSV content/name leaked", f.logs.String())
	}
}
func TestCSVDeliveryZeroMultipleLimitAndLateFailureNonpublication(t *testing.T) {
	f := productionCSVHTTP(t)
	good := []byte("id,amount,secret\n00123,7,allow\n")
	cases := []struct {
		name  string
		files [][]byte
		code  string
	}{
		{"filtered0", [][]byte{[]byte("id,amount,secret\nunused,0,deny\n")}, ""},
		{"header0", [][]byte{[]byte("id,amount,secret\n")}, ""},
		{"late quote", [][]byte{good, []byte("id,amount,secret\nvalid,1,allow\n\"broken")}, "EXECUTION_FAILED"},
		{"late type", [][]byte{good, []byte("id,amount,secret\ninvalid,no,deny\n")}, "EXECUTION_FAILED"},
		{"header reorder", [][]byte{good, []byte("amount,id,secret\n")}, "EXECUTION_FAILED"},
		{"missing where zero", [][]byte{[]byte("id,amount\n")}, "EXECUTION_FAILED"},
		{"sum rows", [][]byte{good, []byte("id,amount,secret\nx,1,allow\ny,2,allow\n")}, "LIMIT_EXCEEDED"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, transport := range []string{"json", "multipart", "mcp"} {
				name := "csv_array_mixed"
				path := "/api/v1/capabilities/" + name
				media := "application/json"
				wire := []byte(csvInlineParams("array", c.files...))
				version := ""
				if transport == "multipart" {
					media, wire = csvMultipartParams(t, c.files...)
				}
				if transport == "mcp" {
					path = "/mcp"
					version = "2025-11-25"
					wire = []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + string(wire) + `}}`)
				}
				before := f.runner.store.Usage()
				status, raw, _ := f.request(t, path, media, bytes.NewReader(wire), version)
				envelope := csvEnvelope(t, raw)
				if transport == "mcp" {
					result := envelope["result"].(map[string]any)
					envelope = csvEnvelope(t, []byte(result["content"].([]any)[0].(map[string]any)["text"].(string)))
					if (result["isError"] == true) != (c.code != "") {
						t.Fatal(result)
					}
				}
				if c.code == "" {
					if status != 200 {
						t.Fatal(status, string(raw))
					}
					assertCSVResult(t, f, envelope, "array", "mixed", 0, "id,amount\r\n")
				} else {
					failure, ok := envelope["error"].(map[string]any)
					if !ok || failure["code"] != c.code || failure["outcome"] != "no_change" || envelope["result"] != nil {
						t.Fatal(status, string(raw))
					}
					if f.runner.store.Usage() != before {
						t.Fatal("failed CSV leaked private stages", before, f.runner.store.Usage())
					}
					if transport != "mcp" && status != 502 && status != 422 {
						t.Fatal(status)
					}
				}
			}
		})
	}
	// Same late corruption through the folder selector also cannot publish a prefix.
	os.WriteFile(filepath.Join(f.dir, "exchange", "part_1.csv"), good, 0600)
	os.WriteFile(filepath.Join(f.dir, "exchange", "part_2.csv"), []byte("id,amount,secret\n\"broken"), 0600)
	before := f.runner.store.Usage()
	status, raw, _ := f.request(t, "/api/v1/capabilities/csv_folder_mixed", "application/json", strings.NewReader(csvInlineParams("folder")), "")
	var failure contract.ErrorEnvelope
	if status != 502 || json.Unmarshal(raw, &failure) != nil || failure.Error.Code != "EXECUTION_FAILED" || f.runner.store.Usage() != before {
		t.Fatal(status, string(raw), f.runner.store.Usage(), before)
	}
}
