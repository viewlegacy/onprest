//go:build schema_validator

package testschema

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const Enabled = true

func validateJSON(t TestingT, schema, value any, want bool) {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(source), "../contract/testdata/schema-validator/validate.mjs")
	body, e := json.Marshal([]any{map[string]any{"schema": schema, "value": value}})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", script)
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, e := cmd.Output()
	if e != nil {
		t.Fatalf("pinned Ajv failed: %v %s", e, stderr.String())
	}
	var results []struct {
		Valid  bool `json:"valid"`
		Errors any  `json:"errors"`
	}
	if e = json.Unmarshal(raw, &results); e != nil || len(results) != 1 {
		t.Fatal("validator result", e)
	}
	if results[0].Valid != want {
		t.Fatalf("published schema valid=%t want=%t errors=%v", results[0].Valid, want, results[0].Errors)
	}
}
