//go:build linux || darwin

package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viewlegacy/onprest/internal/contract"
)

func TestInputPatternProductionFileMetadataAndNoInvalidExecution(t *testing.T) {
	f := productionInputHTTPAdjusted(t, nil, func(yaml string) string {
		return strings.Replace(yaml, "label: {type: string}", "label: {type: string, pattern: '^[A-Za-z0-9 ]{1,64}$'}", 1)
	})
	snapshot, ok := f.runner.versioned.Snapshot("save")
	if !ok {
		t.Fatal("snapshot missing")
	}
	hash := snapshot.DefinitionHash()
	canonical, err := contract.CanonicalJSON(snapshot.Definition().Normalized())
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"REST", "2025-03-26", "2025-06-18", "2025-11-25"} {
		path := "/api/v1/capabilities/save"
		body := strings.TrimSuffix(inlineInput([]byte("valid")), "}") + `,"label":"Alice"}`
		protocol := ""
		if version != "REST" {
			path = "/mcp"
			protocol = version
			body = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"save","arguments":` + body + `}}`
		}
		status, raw, _ := f.request(t, path, "application/json", strings.NewReader(body), protocol)
		if status != 200 || bytes.Contains(raw, []byte(`"isError":true`)) {
			t.Fatal(version, status, string(raw))
		}
		saved, e := os.ReadFile(filepath.Join(f.dir, "exchange", "saved.bin"))
		if e != nil || string(saved) != "valid" {
			t.Fatal(e, string(saved))
		}
		for _, invalid := range []string{"=SUM(A1)", "Alice!", strings.Repeat("a", 65)} {
			usage := f.runner.store.Usage()
			body = strings.TrimSuffix(inlineInput([]byte("must not be written")), "}") + `,"label":"` + invalid + `"}`
			if version != "REST" {
				body = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"save","arguments":` + body + `}}`
			}
			status, raw, _ = f.request(t, path, "application/json", strings.NewReader(body), protocol)
			if (version == "REST" && status != 400) || (version != "REST" && (status != 200 || !bytes.Contains(raw, []byte(`"isError":true`)))) || !bytes.Contains(raw, []byte("VALIDATION_FAILED")) {
				t.Fatal(version, status, string(raw))
			}
			if f.runner.store.Usage() != usage {
				t.Fatal("invalid input acquired Agent spool ownership")
			}
			saved, e = os.ReadFile(filepath.Join(f.dir, "exchange", "saved.bin"))
			if e != nil || string(saved) != "valid" {
				t.Fatal("invalid input executed file provider", e, string(saved))
			}
		}
	}

	snapshot, _ = f.runner.versioned.Snapshot("save")
	current, err := contract.CanonicalJSON(snapshot.Definition().Normalized())
	if err != nil || snapshot.DefinitionHash() != hash || !bytes.Equal(canonical, current) {
		t.Fatal("metadata/requests changed prepared hash or schema", err)
	}
}
