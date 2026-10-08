package wasmruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func wasmSection(id byte, payload []byte) []byte {
	return append(append([]byte{id}, leb(uint32(len(payload)))...), payload...)
}
func wasmString(s string) []byte { return append(leb(uint32(len(s))), []byte(s)...) }

// Independently assembled Core fixtures probe ABI validation and a trap in the
// start section; normal result acceptance remains backed by the Go compiler.
func tinyModule(importModule, importName string, start bool) []byte {
	b := []byte{0, 97, 115, 109, 1, 0, 0, 0}
	b = append(b, wasmSection(1, []byte{1, 0x60, 0, 0})...)
	index := byte(0)
	imports := []string{}
	if importModule != "" {
		payload := append([]byte{1}, wasmString(importModule)...)
		payload = append(payload, wasmString(importName)...)
		payload = append(payload, 0, 0)
		b = append(b, wasmSection(2, payload)...)
		index = 1
		imports = append(imports, importModule+"."+importName)
	}
	b = append(b, wasmSection(3, []byte{1, 0})...)
	b = append(b, wasmSection(5, []byte{1, 0, 1})...)
	exports := append([]byte{2}, wasmString("_start")...)
	exports = append(exports, 0, index)
	exports = append(exports, wasmString("memory")...)
	exports = append(exports, 2, 0)
	b = append(b, wasmSection(7, exports)...)
	if start {
		b = append(b, wasmSection(8, []byte{index})...)
	}
	b = append(b, wasmSection(10, []byte{1, 3, 0, 0, 0x0b})...)
	return addMetadata(b, Metadata{ABI, CompatibilityVersion, imports})
}
func TestWasmNonexecutingStartAndImportSignatures(t *testing.T) {
	r, _, c := setup(t)
	for _, test := range []struct {
		name, module, function string
		start, valid           bool
	}{
		{"start_trap", "", "", true, true}, {"host_signature", ABI, "call", false, false},
		{"WASI_signature", "wasi_snapshot_preview1", "fd_write", false, false},
		{"business_WASI", "wasi_snapshot_preview1", "path_open", false, false},
		{"unknown_import", "other", "execute", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "module.wasm")
			os.WriteFile(path, tinyModule(test.module, test.function, test.start), 0600)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			m, err := r.Prepare(ctx, path, "", DefaultLimits())
			if !test.valid {
				if err == nil {
					t.Fatal("invalid actual import accepted")
				}
				return
			}
			if err != nil {
				t.Fatal("validation executed trapping start section", err)
			}
			out, err := r.Execute(ctx, m, map[string]any{}, c, DefaultLimits())
			if out != nil {
				out.Close()
				t.Fatal("trap published")
			}
			errorCode(t, err, "EXECUTION_FAILED")
			if r.Store.Usage() != 0 {
				t.Fatal("trap leaked")
			}
		})
	}
}
func TestWasmMetadataTypesAndRawDataStructure(t *testing.T) {
	r, m, _ := setup(t)
	meta := m.Metadata()
	base := fixtureBytes[:len(fixtureBytes)-len(addMetadata(nil, meta))]
	for _, key := range []string{"abi", "minimum_agent_version", "imports"} {
		t.Run(key, func(t *testing.T) {
			raw := map[string]any{"abi": ABI, "minimum_agent_version": CompatibilityVersion, "imports": meta.Imports}
			raw[key] = int64(1)
			path := filepath.Join(t.TempDir(), "module.wasm")
			os.WriteFile(path, addMetadata(base, raw), 0600)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := r.Prepare(ctx, path, "", DefaultLimits()); err == nil {
				t.Fatal("metadata type accepted")
			}
		})
	}
	for _, test := range []struct {
		raw          string
		depth, items int64
		valid        bool
	}{
		{`null`, 1, 1, true}, {`[null]`, 2, 2, true}, {`[null]`, 1, 2, false}, {`[null]`, 2, 1, false},
		{`{"x":null}`, 2, 3, true}, {`{"x":null}`, 2, 2, false},
	} {
		var value any
		json.Unmarshal([]byte(test.raw), &value)
		if dataWithinLimits(value, test.depth, test.items) != test.valid {
			t.Fatal("raw structure", test)
		}
	}
}
