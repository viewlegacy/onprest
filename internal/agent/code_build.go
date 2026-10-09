package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/viewlegacy/onprest/internal/agent/wasmruntime"
	"github.com/viewlegacy/onprest/internal/contract"
)

type codeManifest struct {
	SHA256          string   `json:"sha256"`
	ABI             string   `json:"abi"`
	CompilerVersion string   `json:"compiler_version"`
	DependencyHash  string   `json:"dependency_hash"`
	RequiredImports []string `json:"required_imports"`
}

func codeHash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func codeEnvironment(offline bool) []string {
	override := map[string]string{"GOOS": "wasip1", "GOARCH": "wasm", "CGO_ENABLED": "0", "GOFLAGS": "", "GOWORK": "off"}
	if offline {
		override["GOPROXY"] = "off"
		override["GOTOOLCHAIN"] = "local"
		override["GOSUMDB"] = "off"
	}
	env := []string{}
	for _, entry := range os.Environ() {
		k, _, _ := strings.Cut(entry, "=")
		if _, ok := override[k]; !ok {
			env = append(env, entry)
		}
	}
	for k, v := range override {
		env = append(env, k+"="+v)
	}
	return env
}

// Compiler streams share one bounded display budget and are always drained.
type compilerDiagnostics struct {
	mu        sync.Mutex
	used      int
	truncated bool
	sink      *codeDiagnostics
}

func (d *compilerDiagnostics) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(p)
	left := (1 << 20) - d.used
	if left < 0 {
		left = 0
	}
	keep := len(p)
	if keep > left {
		keep = left
	}
	d.used += n
	if keep > 0 {
		b, _ := json.Marshal(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "level": "error", "source": "agent", "event": "compiler_diagnostic", "message": string(p[:keep])})
		_, _ = d.sink.Write(b)
	}
	if keep < n && !d.truncated {
		d.truncated = true
		b, _ := json.Marshal(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "level": "warn", "source": "agent", "event": "log_truncated", "message": "compiler diagnostics exceeded 1MiB"})
		_, _ = d.sink.Write(b)
	}
	return n, nil
}
func codeGo(ctx context.Context, dir string, offline bool, diagnostics io.Writer, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "go", args...)
	c.Dir = dir
	c.Env = codeEnvironment(offline)
	var output strings.Builder
	c.Stdout = &output
	c.Stderr = diagnostics
	e := c.Run()
	return []byte(output.String()), e
}
func annotateCode(ctx context.Context, b []byte) ([]byte, error) {
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCustomSections(true))
	defer rt.Close(ctx)
	c, e := rt.CompileModule(ctx, b)
	if e != nil {
		return nil, e
	}
	defer c.Close(ctx)
	for _, s := range c.CustomSections() {
		if s.Name() == "onprest" {
			return nil, errors.New("compiler output already contains onprest metadata")
		}
	}
	m := wasmruntime.Metadata{ABI: wasmruntime.ABI, MinimumAgentVersion: wasmruntime.CompatibilityVersion, Imports: []string{}}
	for _, f := range c.ImportedFunctions() {
		module, name, _ := f.Import()
		m.Imports = append(m.Imports, module+"."+name)
	}
	sort.Strings(m.Imports)
	m.Imports = compactCode(m.Imports)
	raw, e := json.Marshal(m)
	if e != nil {
		return nil, e
	}
	leb := func(n uint32) []byte {
		v := []byte{}
		for {
			x := byte(n & 127)
			n >>= 7
			if n != 0 {
				x |= 128
			}
			v = append(v, x)
			if n == 0 {
				return v
			}
		}
	}
	payload := append(leb(7), []byte("onprest")...)
	payload = append(payload, raw...)
	b = append(b, 0)
	b = append(b, leb(uint32(len(payload)))...)
	return append(b, payload...), nil
}
func compactCode(v []string) []string {
	r := v[:0]
	for _, s := range v {
		if len(r) == 0 || r[len(r)-1] != s {
			r = append(r, s)
		}
	}
	return r
}
func codeModulePath(o codeOptions, c *contract.Capability) (string, error) {
	p := o.output
	if c != nil {
		p = c.Executor.Settings["module"].(string)
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(o.config), p)
		}
	}
	return filepath.Abs(p)
}
func buildCode(ctx context.Context, o codeOptions, cfg *contract.Config, cap *contract.Capability, diag *codeDiagnostics) (result codeResult, err error) {
	result = codeResult{Operation: "build"}
	source, e := filepath.Abs(o.source)
	if e != nil {
		return result, e
	}
	info, e := os.Stat(source)
	if e != nil || !info.IsDir() {
		return result, errors.New("source must be a Go package directory")
	}
	output, e := codeModulePath(o, cap)
	if e != nil {
		return result, e
	}
	result.Module = output
	result.Manifest = output + ".manifest.json"
	// Serialize author builds to the same output without touching installed Agent
	// spool/logs or adding a required deployment artifact.
	lockDir := filepath.Join(os.TempDir(), "onprest-code-build-locks")
	if e = os.MkdirAll(lockDir, 0700); e != nil {
		return result, e
	}
	lock, busy, e := acquireValidationLock(filepath.Join(lockDir, codeHash([]byte(output))+".lock"))
	if e != nil || busy {
		return result, errors.Join(e, errors.New("build output is busy"))
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	parent, e := openCodeDirectory(filepath.Dir(output))
	if e != nil {
		return result, e
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	name := filepath.Base(output)
	manifestName := name + ".manifest.json"
	old := map[string][]byte{}
	for _, n := range []string{name, manifestName} {
		b, exists, e := parent.readRegular(n, 32<<20)
		if e != nil {
			return result, e
		}
		if exists {
			if !o.force {
				return result, errors.New("existing module or manifest requires --force")
			}
			old[n] = b
		}
	}
	compiler := &compilerDiagnostics{sink: diag}
	version, e := codeGo(ctx, source, o.offline, compiler, "env", "GOVERSION")
	if e != nil {
		return result, e
	}
	v := strings.TrimSpace(string(version))
	var maj, min, patch int
	if _, e = fmt.Sscanf(v, "go%d.%d.%d", &maj, &min, &patch); e != nil || maj < 1 || maj == 1 && (min < 26 || min == 26 && patch < 9) {
		return result, errors.New("Go 1.26.9 or later is required")
	}
	gomod, e := codeGo(ctx, source, o.offline, compiler, "env", "GOMOD")
	if e != nil {
		return result, e
	}
	modPath := strings.TrimSpace(string(gomod))
	mod, e := os.ReadFile(modPath)
	if e != nil {
		return result, errors.New("source requires a Go module")
	}
	sum, e := os.ReadFile(filepath.Join(filepath.Dir(modPath), "go.sum"))
	if e != nil && !os.IsNotExist(e) {
		return result, e
	}
	dep, _ := json.Marshal([]string{string(mod), string(sum)})
	tmp, e := os.MkdirTemp("", "onprest-code-compile-")
	if e != nil {
		return result, e
	}
	defer func() {
		if e := os.RemoveAll(tmp); e != nil {
			err = errors.Join(err, e)
			_ = checkedWrite(diag.w, []byte(fmt.Sprintf("compiler cleanup failed; inspect %q\n", tmp)))
		}
	}()

	// -mod=readonly still allows checksum updates; only private module files
	// are passed to Go. Local replace targets retain their original base.
	privateMod := filepath.Join(tmp, "go.mod")
	if e = os.WriteFile(privateMod, mod, 0600); e != nil {
		return result, e
	}
	if len(sum) > 0 {
		if e = os.WriteFile(filepath.Join(tmp, "go.sum"), sum, 0600); e != nil {
			return result, e
		}
	}
	description, e := codeGo(ctx, source, o.offline, compiler, "mod", "edit", "-json")
	if e != nil {
		return result, e
	}
	var graph struct {
		Replace []struct {
			Old, New struct{ Path, Version string }
		}
	}
	if e = json.Unmarshal(description, &graph); e != nil {
		return result, e
	}
	for _, replace := range graph.Replace {
		if replace.New.Version == "" && !filepath.IsAbs(replace.New.Path) {
			old := replace.Old.Path
			if replace.Old.Version != "" {
				old += "@" + replace.Old.Version
			}
			target := filepath.Join(filepath.Dir(modPath), replace.New.Path)
			if _, e = codeGo(ctx, source, o.offline, compiler, "mod", "edit", "-modfile", privateMod, "-replace", old+"="+target); e != nil {
				return result, e
			}
		}
	}
	rawPath := filepath.Join(tmp, "raw.wasm")
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-mod=readonly", "-modfile", privateMod, "-o", rawPath, ".")
	command.Dir = source
	command.Env = codeEnvironment(o.offline)
	command.Stdout = compiler
	command.Stderr = compiler
	if e = command.Run(); e != nil {
		return result, e
	}
	after, e := os.ReadFile(modPath)
	if e != nil || string(after) != string(mod) {
		return result, errors.New("compiler changed go.mod")
	}
	after, e = os.ReadFile(filepath.Join(filepath.Dir(modPath), "go.sum"))
	if e != nil && !os.IsNotExist(e) || string(after) != string(sum) {
		return result, errors.New("compiler changed go.sum")
	}
	f, e := os.Open(rawPath)
	if e != nil {
		return result, e
	}
	b, e := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	e = errors.Join(e, f.Close())
	if e != nil || len(b) > 32<<20 {
		return result, errors.Join(e, errors.New("compiled module exceeds byte limit"))
	}
	b, e = annotateCode(ctx, b)
	if e != nil {
		return result, e
	}
	if len(b) > 32<<20 {
		return result, errors.New("module metadata exceeds byte limit")
	}
	ready := filepath.Join(tmp, "ready.wasm")
	if e = os.WriteFile(ready, b, 0600); e != nil {
		return result, e
	}
	limits := wasmruntime.DefaultLimits()
	expected := ""
	if cap != nil {
		limits = wasmLimits(cap)
		expected, _ = cap.Executor.Settings["sha256"].(string)
	}
	module, e := (&wasmruntime.Runner{}).Prepare(ctx, ready, expected, limits)
	if e != nil {
		return result, e
	}
	metadata := module.Metadata()
	manifest := codeManifest{module.Digest(), metadata.ABI, v, codeHash(dep), metadata.Imports}
	mb, e := json.Marshal(manifest)
	if e != nil {
		return result, e
	}
	mb = append(mb, '\n')
	if e = parent.publishPair(ctx, name, manifestName, b, mb, old); e != nil {
		return result, e
	}
	result.OK = true
	result.SHA256 = module.Digest()
	result.ABI = metadata.ABI
	result.Imports = metadata.Imports
	return result, nil
}
func validateCode(ctx context.Context, path string) (codeResult, error) {
	result := codeResult{Operation: "validate"}
	absolute, e := filepath.Abs(path)
	if e != nil {
		return result, e
	}
	result.Module = absolute
	module, e := (&wasmruntime.Runner{}).Prepare(ctx, absolute, "", wasmruntime.DefaultLimits())
	if e != nil {
		return result, e
	}
	parent, e := openCodeDirectory(filepath.Dir(absolute))
	if e != nil {
		return result, e
	}
	defer parent.Close()
	b, exists, e := parent.readRegular(filepath.Base(absolute)+".manifest.json", 64<<10)
	if e != nil {
		return result, e
	}
	m := module.Metadata()
	if exists {
		value, e := contract.DecodeJSON(b, contract.Limits{MaxBytes: 64 << 10, MaxDepth: 4, MaxItems: 1024})
		if e != nil {
			return result, e
		}
		object, ok := value.(map[string]any)
		if _, array := object["required_imports"].([]any); !ok || len(object) != 5 || !array {
			return result, errors.New("invalid module manifest")
		}
		var manifest codeManifest
		decoder := json.NewDecoder(strings.NewReader(string(b)))
		decoder.DisallowUnknownFields()
		if e = decoder.Decode(&manifest); e != nil || manifest.SHA256 != module.Digest() || manifest.ABI != m.ABI || strings.Join(manifest.RequiredImports, "\n") != strings.Join(m.Imports, "\n") || len(manifest.DependencyHash) != 64 || manifest.CompilerVersion == "" {
			return result, errors.New("manifest does not match module")
		}
		if _, e = hex.DecodeString(manifest.DependencyHash); e != nil {
			return result, e
		}
		result.Manifest = absolute + ".manifest.json"
	}
	result.OK = true
	result.SHA256 = module.Digest()
	result.ABI = m.ABI
	result.Imports = m.Imports
	return result, nil
}
