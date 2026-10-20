// This example-only helper appends actual import metadata to a compiler output.
// It is not part of either distributed binary or the author CLI.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"github.com/tetratelabs/wazero"
	"github.com/viewlegacy/onprest/internal/agent/wasmruntime"
	"os"
	"os/exec"
	"sort"
)

func leb(n uint32) []byte {
	var b []byte
	for {
		v := byte(n & 127)
		n >>= 7
		if n != 0 {
			v |= 128
		}
		b = append(b, v)
		if n == 0 {
			return b
		}
	}
}
func main() {
	output := flag.String("output", "/tmp/onprest-example.wasm", "output path outside the source tree")
	flag.Parse()
	tmp, e := os.CreateTemp("", "onprest-example-*.wasm")
	if e != nil {
		panic(e)
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	c := exec.Command("go", "build", "-trimpath", "-mod=readonly", "-o", name, "./examples/wasm/main.go")
	c.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if e = c.Run(); e != nil {
		panic(e)
	}
	b, e := os.ReadFile(name)
	if e != nil {
		panic(e)
	}
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)
	m, e := r.CompileModule(ctx, b)
	if e != nil {
		panic(e)
	}
	defer m.Close(ctx)
	meta := wasmruntime.Metadata{ABI: wasmruntime.ABI, MinimumAgentVersion: wasmruntime.CompatibilityVersion, Imports: []string{}}
	for _, f := range m.ImportedFunctions() {
		module, name, _ := f.Import()
		meta.Imports = append(meta.Imports, module+"."+name)
	}
	sort.Strings(meta.Imports)
	unique := meta.Imports[:0]
	for _, value := range meta.Imports {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	meta.Imports = unique
	raw, e := json.Marshal(meta)
	if e != nil {
		panic(e)
	}
	payload := append(leb(7), []byte("onprest")...)
	payload = append(payload, raw...)
	b = append(b, 0)
	b = append(b, leb(uint32(len(payload)))...)
	b = append(b, payload...)
	if e = os.WriteFile(*output, b, 0600); e != nil {
		panic(e)
	}
}
