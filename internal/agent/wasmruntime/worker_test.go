package wasmruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/tetratelabs/wazero"
	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

var binaryPath, modulePath string
var fixtureBytes []byte

func TestMain(m *testing.M) {
	dir, e := os.MkdirTemp("", "onprest-wasm-test-")
	if e != nil {
		panic(e)
	}
	binaryPath = filepath.Join(dir, "agent")
	if runtime.GOOS == "windows" {
		binaryPath += ".exe"
	}
	modulePath = filepath.Join(dir, "fixture.wasm")
	build := exec.Command("go", "build", "-trimpath", "-mod=readonly", "-o", binaryPath, "../../../cmd/agent")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, e := build.CombinedOutput(); e != nil {
		fmt.Fprintln(os.Stderr, string(b))
		os.RemoveAll(dir)
		os.Exit(1)
	}
	build = exec.Command("go", "build", "-trimpath", "-mod=readonly", "-o", modulePath, "./testdata/fixture.go")
	build.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if b, e := build.CombinedOutput(); e != nil {
		fmt.Fprintln(os.Stderr, string(b))
		os.RemoveAll(dir)
		os.Exit(1)
	}
	b, e := os.ReadFile(modulePath)
	if e != nil {
		panic(e)
	}
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	compiled, e := rt.CompileModule(ctx, b)
	if e != nil {
		panic(e)
	}
	meta := Metadata{ABI: ABI, MinimumAgentVersion: CompatibilityVersion, Imports: []string{}}
	for _, f := range compiled.ImportedFunctions() {
		module, name, _ := f.Import()
		meta.Imports = append(meta.Imports, module+"."+name)
	}
	sort.Strings(meta.Imports)
	meta.Imports = compactImports(meta.Imports)
	compiled.Close(ctx)
	rt.Close(ctx)
	fixtureBytes = addMetadata(b, meta)
	if e = os.WriteFile(modulePath, fixtureBytes, 0600); e != nil {
		panic(e)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
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
func addMetadata(b []byte, m any) []byte { raw, _ := json.Marshal(m); return addRawMetadata(b, raw) }
func addRawMetadata(b, raw []byte) []byte {
	name := []byte("onprest")
	payload := append(leb(uint32(len(name))), name...)
	payload = append(payload, raw...)
	out := append([]byte(nil), b...)
	out = append(out, 0)
	out = append(out, leb(uint32(len(payload)))...)
	return append(out, payload...)
}
func setup(t *testing.T) (*Runner, *Module, *contract.Capability) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "spool")
	store, e := artifacts.Open(artifacts.Options{Directory: directory, QuotaBytes: 160 << 20, MaxPending: 16, MaxObjects: 256, TransferTimeout: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if n := store.Usage(); n != 0 {
			t.Errorf("spool leaked %d bytes", n)
		}
		entries, err := os.ReadDir(filepath.Join(directory, "objects"))
		if err != nil || len(entries) != 0 {
			t.Errorf("spool objects remained: %d, %v", len(entries), err)
		}
		if e := store.Close(); e != nil {
			t.Error(e)
		}
	})
	r := &Runner{Binary: binaryPath, Store: store, Cache: NewCache(), Diagnostic: func(level, event, message, detail string) {
		if level == "error" {
			t.Log(event, message, detail)
		}
	}}
	// Prepare is bounded by the product's ModuleValidationBudget; do not add a shorter deadline.
	m, e := r.Prepare(context.Background(), modulePath, "", DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := contract.Compile([]byte("schema_version: 1\ngateway: {url: ws://localhost/ws/agent, agent_private_key_file: ./missing.key}\ncapabilities:\n  run:\n    executor: {type: wasm, module: module.wasm}\n    output: {data: {}, files: {}}\n"))
	if e != nil {
		t.Fatal(e)
	}
	return r, m, cfg.Capabilities["run"]
}

// runBudget is the deadline for runs that do not test the deadline.
const runBudget = 60 * time.Second

func run(t *testing.T, r *Runner, m *Module, c *contract.Capability, params map[string]any, timeout time.Duration) (*contract.PreparedOutput, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.Execute(ctx, m, params, c, DefaultLimits())
}
func errorCode(t *testing.T, e error, code string) {
	t.Helper()
	var failure *Error
	if !errors.As(e, &failure) || failure.Code != code {
		t.Fatalf("error=%v detail=%+v, want %s", e, failure, code)
	}
}
func TestWasmRealWorkerResultAndImmutableHash(t *testing.T) {
	r, m, c := setup(t)
	var observation Observation
	r.Observe = func(o Observation) { observation = o }
	out, e := run(t, r, m, c, map[string]any{"scenario": "success"}, runBudget)
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	b, e := io.ReadAll(out.Files[0].Reader)
	if e != nil || !bytes.Equal(b, bytes.Repeat([]byte("F"), 70000)) {
		t.Fatal("file bytes differ", e)
	}
	digest := sha256.Sum256(b)
	if out.Files[0].SHA256 != hex.EncodeToString(digest[:]) || out.Files[0].SizeBytes != 70000 {
		t.Fatal("metadata differs")
	}
	raw, e := contract.CanonicalJSON(out.Data)
	if e != nil || string(raw) != `{"integer":9007199254740993}` {
		t.Fatal("integer precision", string(raw), e)
	}
	ceiling, _ := DefaultLimits().rssLimit()
	if observation.PeakRSS == 0 || observation.PeakRSS >= ceiling || observation.CompileDuration <= 0 {
		t.Fatal("resource observation missing", observation)
	}
	t.Logf("actual worker peak RSS=%d ceiling=%d CPU=%f compile=%s", observation.PeakRSS, ceiling, observation.CPUSeconds, observation.CompileDuration)
	path := filepath.Join(t.TempDir(), "module.wasm")
	os.WriteFile(path, fixtureBytes, 0600)
	// Prepare is bounded by the product's ModuleValidationBudget; do not add a shorter deadline.
	ctx := context.Background()
	copy, e := r.Prepare(ctx, path, m.Digest(), DefaultLimits())
	if e != nil || copy.Digest() != m.Digest() {
		t.Fatal(e)
	}
	os.WriteFile(path, []byte("replaced"), 0600)
	got, e := run(t, r, copy, c, map[string]any{"scenario": "business"}, runBudget)
	if e != nil {
		t.Fatal("path was reread", e)
	}
	got.Close()
	if _, e = r.Prepare(ctx, path, "", DefaultLimits()); e == nil {
		t.Fatal("invalid replacement accepted")
	}
	validations := 0
	r.Observe = func(Observation) { validations++ }
	if _, e = r.Prepare(ctx, modulePath, strings.Repeat("0", 64), DefaultLimits()); e == nil {
		t.Fatal("digest mismatch accepted")
	}
	if validations != 0 {
		t.Fatal("digest mismatch started worker")
	}
	changedPath := filepath.Join(t.TempDir(), "changed.wasm")
	changedBytes := append(append([]byte(nil), fixtureBytes...), wasmSection(0, append(wasmString("test-change"), byte(1)))...)
	os.WriteFile(changedPath, changedBytes, 0600)
	changedModule, err := r.Prepare(ctx, changedPath, "", DefaultLimits())
	if err != nil || changedModule.Digest() == m.Digest() {
		t.Fatal("valid changed bytes digest", err)
	}
	cfg, _ := contract.Compile([]byte("schema_version: 1\ngateway: {url: ws://localhost/ws/agent, agent_private_key_file: ./missing.key}\ncapabilities:\n  run:\n    executor: {type: wasm, module: module.wasm}\n    output: {data: {}}\n"))
	base := t.TempDir()
	opts := providers.ResolveOptions{ModuleDigests: map[string]string{"run": m.Digest()}, Environment: map[string]string{}}
	a, e := providers.Resolve(ctx, cfg, filepath.Join(base, "agent.yaml"), "run", opts)
	if e != nil {
		t.Fatal(e)
	}
	cfg.Capabilities["run"].Executor.Settings["sha256"] = m.Digest()
	bSnapshot, e := providers.Resolve(ctx, cfg, filepath.Join(base, "agent.yaml"), "run", opts)
	if e != nil || a.DefinitionHash() != bSnapshot.DefinitionHash() {
		t.Fatal("expected digest presence changed hash", e)
	}
	opts.ModuleDigests["run"] = changedModule.Digest()
	changed, e := providers.Resolve(ctx, cfg, filepath.Join(base, "agent.yaml"), "run", opts)
	if e != nil || changed.DefinitionHash() == a.DefinitionHash() {
		t.Fatal("changed digest did not affect hash", e)
	}
}
func TestWasmRealWorkerTerminalFailuresAndIsolation(t *testing.T) {
	r, m, c := setup(t)
	for _, test := range []struct{ scenario, code string }{{"missing", "OUTPUT_INVALID"}, {"early", "OUTPUT_INVALID"}, {"duplicate_open", "OUTPUT_INVALID"}, {"foreign", "OUTPUT_INVALID"}, {"duplicate_file", "EXECUTION_FAILED"}, {"duplicate_commit", "OUTPUT_INVALID"}, {"post_io", "OUTPUT_INVALID"}, {"post_crash", "EXECUTION_FAILED"}, {"memory", "EXECUTION_FAILED"}, {"loop", "EXECUTION_TIMEOUT"}, {"post_timeout", "EXECUTION_TIMEOUT"}} {
		t.Run(test.scenario, func(t *testing.T) {
			timeout := runBudget
			runner := r
			if test.code == "EXECUTION_TIMEOUT" {
				timeout = 1500 * time.Millisecond
				// Compilation may use several CPU threads before the busy loop.
				// Exercise the wall deadline independently of the real CPU-budget
				// test below, while retaining actual RSS/counter monitoring.
				copy := *r
				copy.cpuOverride = 30
				runner = &copy
			}
			out, e := run(t, runner, m, c, map[string]any{"scenario": test.scenario}, timeout)
			if out != nil {
				out.Close()
				t.Fatal("failure published output")
			}
			errorCode(t, e, test.code)
			if r.Store.Usage() != 0 {
				t.Fatal("failure leaked quota")
			}
		})
	}
	for _, scenario := range []string{"bad_pointer", "bad_handle", "business", "success"} {
		out, e := run(t, r, m, c, map[string]any{"scenario": scenario}, runBudget)
		if e != nil {
			t.Fatal("other request broken", scenario, e)
		}
		out.Close()
	}
}
func TestWasmRealWorkerTerminalErrors(t *testing.T) {
	r, m, c := setup(t)
	for _, test := range []struct{ envelope, code string }{{`{"error":{"kind":"internal","category":"execution"}}`, "EXECUTION_FAILED"}, {`{"error":{"kind":"internal","category":"output"}}`, "OUTPUT_INVALID"}, {`{"error":{"kind":"internal","category":"limit"}}`, "LIMIT_EXCEEDED"}, {`{"error":{"kind":"internal","category":"bogus"}}`, "EXECUTION_FAILED"}, {`{"error":{"kind":"public","code":"APP_DOCUMENT_INVALID","message":"safe"}}`, "APP_DOCUMENT_INVALID"}, {`{"error":{"kind":"public","code":"APP_DOCUMENT_INVALID","message":"unsafe\n"}}`, "EXECUTION_FAILED"}, {`{"data":null,"files":[],"error":{}}`, "EXECUTION_FAILED"}, {`{"data":1,"data":2,"files":[]}`, "EXECUTION_FAILED"}, {`{"data":"\ud800","files":[]}`, "EXECUTION_FAILED"}} {
		t.Run(test.envelope, func(t *testing.T) {
			out, e := run(t, r, m, c, map[string]any{"scenario": "envelope", "envelope": test.envelope}, runBudget)
			if out != nil {
				out.Close()
				t.Fatal("error published")
			}
			errorCode(t, e, test.code)
			var f *Error
			errors.As(e, &f)
			if test.code == "APP_DOCUMENT_INVALID" && f.Message != "safe" {
				t.Fatal("explicit public message lost")
			}
			if test.code != "APP_DOCUMENT_INVALID" && f.Message != "" {
				t.Fatal("internal detail public")
			}
		})
	}
}
func TestWasmRealWorkerEnvelopeBoundsAndCommonOutput(t *testing.T) {
	r, m, c := setup(t)
	c.Policy["max_output_bytes"] = int64(40 << 20)
	for _, size := range []int{70000, EnvelopeLimit - 1, EnvelopeLimit, EnvelopeLimit + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			out, e := run(t, r, m, c, map[string]any{"scenario": "large", "size": size}, 12*time.Second)
			if size > EnvelopeLimit {
				if out != nil {
					out.Close()
					t.Fatal("oversize published")
				}
				errorCode(t, e, "EXECUTION_FAILED")
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if len(out.Data.(string)) != size-len(`{"data":"","files":[]}`) {
				t.Fatal("large data mismatch")
			}
			out.Close()
		})
	}
	c.Policy["max_output_bytes"] = int64(1024)
	out, e := run(t, r, m, c, map[string]any{"scenario": "large", "size": 70000}, runBudget)
	if out != nil {
		out.Close()
		t.Fatal("public cap bypass")
	}
	var output *contract.OutputError
	if !errors.As(e, &output) || output.Code != "LIMIT_EXCEEDED" {
		t.Fatal(e)
	}
}
func TestWasmRealWorkerLogsDrainBudgetAndTail(t *testing.T) {
	r, m, c := setup(t)
	var mu sync.Mutex
	records := []struct{ level, event, message, detail string }{}
	r.Diagnostic = func(level, event, message, detail string) {
		mu.Lock()
		defer mu.Unlock()
		records = append(records, struct{ level, event, message, detail string }{level, event, message, detail})
	}
	for _, scenario := range []string{"logs", "utf8", "panic_logs"} {
		records = nil
		out, e := run(t, r, m, c, map[string]any{"scenario": scenario}, 8*time.Second)
		if scenario == "panic_logs" {
			errorCode(t, e, "EXECUTION_FAILED")
		} else {
			if e != nil {
				t.Fatal(e)
			}
			out.Close()
		}
		mu.Lock()
		truncated := 0
		tail := ""
		for _, record := range records {
			if !utf8.ValidString(record.message) || !utf8.ValidString(record.detail) || len(record.message) > 16<<10 || len(record.detail) > 16<<10 {
				t.Fatal("unbounded diagnostic")
			}
			if record.event == "log_truncated" {
				truncated++
			}
			if record.event == "wasm_stderr_tail" {
				tail += record.detail
			}
			if strings.Contains(record.message, "DISCARD") {
				t.Fatal("invalid log level text accepted")
			}
		}
		if scenario != "utf8" && truncated != 1 {
			t.Fatal("truncation count", truncated)
		}
		if scenario == "panic_logs" && (!strings.Contains(tail, "PRIVATE-PANIC-DETAIL") || !strings.Contains(tail, "TAIL-MARKER") || len(tail) < 60000) {
			t.Fatal("stderr ring tail missing", len(tail))
		}
		mu.Unlock()
	}
}
func TestWasmRealWorkerOSResourceEnforcement(t *testing.T) {
	r, m, c := setup(t)
	for _, kind := range []string{"rss", "cpu", "counter"} {
		t.Run(kind, func(t *testing.T) {
			copy := *r
			var o Observation
			copy.Observe = func(v Observation) { o = v }
			if kind == "rss" {
				copy.rssOverride = 24 << 20
			}
			if kind == "cpu" {
				copy.cpuOverride = .001
			}
			if kind == "counter" {
				copy.sample = func(*process.Process) (uint64, float64, error) {
					return sampleCounters(context.Background(), &process.Process{Pid: -1})
				}
			}
			out, e := run(t, &copy, m, c, map[string]any{"scenario": "success"}, runBudget)
			if out != nil {
				out.Close()
				t.Fatal("resource failure published")
			}
			code := "LIMIT_EXCEEDED"
			if kind == "counter" {
				code = "DEPENDENCY_UNAVAILABLE"
			}
			errorCode(t, e, code)
			if kind == "rss" && o.PeakRSS <= copy.rssOverride {
				t.Fatal("real RSS threshold not crossed", o)
			}
			if kind == "cpu" && o.CPUSeconds <= copy.cpuOverride {
				t.Fatal("real CPU threshold not crossed", o)
			}
		})
	}
	out, e := run(t, r, m, c, map[string]any{"scenario": "success"}, runBudget)
	if e != nil {
		t.Fatal("resource termination affected next request", e)
	}
	out.Close()
}
func TestWasmPrivateWorkerCannotBeInvoked(t *testing.T) {
	cmd := exec.Command(binaryPath, workerCommand)
	if b, e := cmd.CombinedOutput(); e == nil || len(b) > 4096 {
		t.Fatal("bare internal worker was accepted", e, string(b))
	}
}

func TestWasmRealWorkerProjectionLimitsAndReclamation(t *testing.T) {
	r, m, c := setup(t)
	for _, test := range []struct {
		name, scenario string
		data, files    bool
	}{
		{"mixed", "success", true, true}, {"post_commit_close", "post_close", true, true}, {"data_only", "success", true, false},
		{"file_only", "success", false, true}, {"neither", "success", false, false},
		{"unreferenced", "unreferenced", true, true}, {"all_pointer_paths", "chunk_pointer", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := *c
			copy.Output = c.Output
			copy.Output.Data = test.data
			if !test.files {
				copy.Output.Files = nil
			}
			out, err := run(t, r, m, &copy, map[string]any{"scenario": test.scenario}, runBudget)
			if err != nil {
				t.Fatal(err)
			}
			if !test.data && out.Data != nil {
				t.Fatal("unselected data published")
			}
			expected := 0
			if test.files && (test.scenario == "success" || test.scenario == "post_close") {
				expected = 1
			}
			if len(out.Files) != expected {
				t.Fatal("unselected/unreferenced file published", len(out.Files))
			}
			if expected == 1 {
				b, err := io.ReadAll(out.Files[0].Reader)
				if err != nil || !bytes.Equal(b, bytes.Repeat([]byte("F"), 70000)) {
					t.Fatal("selected file bytes", err)
				}
			}
			if err = out.Close(); err != nil {
				t.Fatal(err)
			}
			if r.Store.Usage() != 0 {
				t.Fatal("projection leaked spool")
			}
		})
	}
	for _, kind := range []string{"file_bytes", "total_bytes", "shared_quota", "handles", "file_count", "unready"} {
		t.Run(kind, func(t *testing.T) {
			copy := *r
			l := DefaultLimits()
			scenario := "catch_limit"
			size := 8192
			expected := "LIMIT_EXCEEDED"
			switch kind {
			case "file_bytes":
				l.FileBytes = 4096
			case "total_bytes":
				l.FileBytes = 8192
				l.TotalFileBytes = 8192
				scenario = "catch_total"
			case "shared_quota":
				directory := filepath.Join(t.TempDir(), "spool")
				store, err := artifacts.Open(artifacts.Options{Directory: directory, QuotaBytes: 4096, MaxPending: 2, MaxObjects: 64, TransferTimeout: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				copy.Store = store
			case "handles":
				l.Handles = 2
				scenario = "success"
				expected = "EXECUTION_FAILED"
			case "file_count":
				l.Files = 1
				scenario = "two_files"
				expected = "EXECUTION_FAILED"
			case "unready":
				scenario = "unready_file"
				expected = "EXECUTION_FAILED"
			}
			ctx, cancel := context.WithTimeout(context.Background(), runBudget)
			defer cancel()
			out, err := copy.Execute(ctx, m, map[string]any{"scenario": scenario, "size": size}, c, l)
			if out != nil {
				out.Close()
				t.Fatal("invalid/limited result published")
			}
			errorCode(t, err, expected)
			if copy.Store.Usage() != 0 {
				t.Fatal("limited result leaked quota")
			}
		})
	}
	for _, test := range []struct {
		name, raw    string
		depth, items int64
	}{
		{"depth", `{"data":[[null]],"files":[]}`, 2, 100},
		{"items", `{"data":[null,null],"files":[]}`, 10, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			l := DefaultLimits()
			l.JSONDepth = test.depth
			l.JSONItems = test.items
			ctx, cancel := context.WithTimeout(context.Background(), runBudget)
			defer cancel()
			hidden := *c
			hidden.Output = contract.Output{}
			out, err := r.Execute(ctx, m, map[string]any{"scenario": "envelope", "envelope": test.raw}, &hidden, l)
			if out != nil {
				out.Close()
				t.Fatal("hidden raw invalid data accepted")
			}
			errorCode(t, err, "EXECUTION_FAILED")
		})
	}
}

func TestWasmRealWorkerShortLivedMonitoringAndMidrunFailure(t *testing.T) {
	r, m, c := setup(t)
	for i := 0; i < 8; i++ {
		out, err := run(t, r, m, c, map[string]any{"scenario": "unreferenced"}, runBudget)
		if err != nil {
			t.Fatalf("short worker %d: %v", i, err)
		}
		out.Close()
		if r.Store.Usage() != 0 {
			t.Fatal("short worker leaked")
		}
	}
	var reads int
	var entered atomic.Bool
	r.Diagnostic = func(_, event, message, _ string) {
		if event == "wasm_stderr" && message == "worker-entered" {
			entered.Store(true)
		}
	}
	r.sample = func(p *process.Process) (uint64, float64, error) {
		reads++
		if entered.Load() {
			return sampleCounters(context.Background(), &process.Process{Pid: -1})
		}
		return sampleCounters(context.Background(), p)
	}
	out, err := run(t, r, m, c, map[string]any{"scenario": "loop"}, runBudget)
	if out != nil {
		out.Close()
		t.Fatal("counter failure published")
	}
	errorCode(t, err, "DEPENDENCY_UNAVAILABLE")
	if !entered.Load() || reads <= 5 || r.Store.Usage() != 0 {
		t.Fatal("midrun monitor/reclamation", reads)
	}
}

func TestWasmRealAgentProductionPreparation(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "agent.yaml")
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("schema_version: 1\ngateway: {url: ws://localhost/ws/agent, agent_private_key: %q}\ncapabilities:\n  run:\n    executor: {type: wasm, module: %q}\n    output: {data: {}}\n", base64.RawURLEncoding.EncodeToString(key), modulePath)
	if err := os.WriteFile(config, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binaryPath, "validate", "--config", config, "--format", "json")
	command.Dir = dir
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("production prepare failed: %v %s %s", err, output, &stderr)
	}
	var result map[string]any
	if json.Unmarshal(output, &result) != nil || result["valid"] != true {
		t.Fatalf("wrong production rejection %s %s", output, &stderr)
	}
	help, err := exec.Command(binaryPath, "help").Output()
	if err != nil || strings.Contains(string(help), workerCommand) || strings.Contains(strings.ToLower(string(help)), "wasm") {
		t.Fatal("internal runtime advertised in help", err, string(help))
	}
}

func TestWasmRealWorkerDeadlineIncludesBootstrap(t *testing.T) {
	r, m, c := setup(t)
	var observation Observation
	r.Observe = func(v Observation) { observation = v }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	out, err := r.Execute(ctx, m, map[string]any{"scenario": "success"}, c, DefaultLimits())
	if out != nil {
		out.Close()
		t.Fatal("bootstrap timeout published")
	}
	errorCode(t, err, "EXECUTION_TIMEOUT")
	if time.Since(started) > time.Second || observation.CompileDuration != 0 || r.Store.Usage() != 0 {
		t.Fatal("bootstrap deadline/Wait/reclamation", observation)
	}
}
