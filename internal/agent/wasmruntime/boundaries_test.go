package wasmruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	agent "github.com/viewlegacy/onprest/internal/agent/localdiagnostics"
)

func TestWasmModuleValidationAndCacheLimits(t *testing.T) {
	r, m, _ := setup(t)
	meta := m.Metadata()
	base := fixtureBytes[:len(fixtureBytes)-(len(addMetadata(nil, meta)))]
	for _, test := range []struct {
		name    string
		bytes   []byte
		version string
	}{
		{"missing_metadata", base, "dev"}, {"duplicate_metadata", addMetadata(fixtureBytes, meta), "dev"}, {"unknown_key", addMetadata(base, map[string]any{"abi": ABI, "minimum_agent_version": "2.0.0", "imports": meta.Imports, "extra": true}), "dev"},
		{"duplicate_key", addRawMetadata(base, []byte(`{"abi":"onprest_host_v1","abi":"onprest_host_v1","minimum_agent_version":"2.0.0","imports":[]}`)), "dev"},
		{"bad_unicode", addRawMetadata(base, []byte("{\"abi\":\"\xff\"}")), "dev"}, {"wrong_abi", addMetadata(base, Metadata{"future", "2.0.0", meta.Imports}), "dev"},
		{"minimum_version", addMetadata(base, Metadata{ABI, "2.0.1", meta.Imports}), "dev"}, {"unknown_agent", fixtureBytes, "nightly"}, {"old_agent", fixtureBytes, "1.9.9"},
		{"declared_import_mismatch", addMetadata(base, Metadata{ABI, "2.0.0", []string{"onprest_host_v1.call"}}), "dev"}, {"unsorted", addMetadata(base, Metadata{ABI, "2.0.0", []string{"z", "a"}}), "dev"},
		{"bad_core", []byte("not wasm"), "dev"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.wasm")
			if e := os.WriteFile(path, test.bytes, 0600); e != nil {
				t.Fatal(e)
			}
			copy := *r
			copy.AgentVersion = test.version
			copy.Diagnostic = nil
			if _, e := copy.Prepare(context.Background(), path, "", DefaultLimits()); e == nil {
				t.Fatal("invalid module accepted")
			}
		})
	}
	// Prepare is bounded by the product's ModuleValidationBudget; do not add a shorter deadline.
	ctx := context.Background()
	l := DefaultLimits()
	l.ModuleBytes = int64(len(fixtureBytes) - 1)
	if _, e := r.Prepare(ctx, modulePath, "", l); e == nil {
		t.Fatal("module byte limit bypass")
	}
	l = DefaultLimits()
	l.MemoryBytes = 65536
	if _, e := r.Prepare(ctx, modulePath, "", l); e == nil {
		t.Fatal("initial memory exceeds configured limit")
	}
	for _, test := range []struct {
		actual, minimum string
		ok              bool
	}{{"dev", "2.0.0", true}, {"2.0.0", "2.0.0", true}, {"2.1.0", "2.0.9", true}, {"2.0.0", "2.0.1", false}, {"dev", "2.1.0", false}, {"2.00.0", "2.0.0", false}, {"2.0.0", "unknown", false}} {
		if versionSupported(test.actual, test.minimum) != test.ok {
			t.Fatal("version comparison", test)
		}
	}
	cache := NewCache()
	for i := 0; i < 64; i++ {
		cache.put(fmt.Sprint(i), &Module{bytes: []byte{byte(i)}})
	}
	cache.get("0")
	cache.put("new", &Module{bytes: []byte{1}})
	if cache.get("0") == nil || cache.get("1") != nil {
		t.Fatal("LRU ordering")
	}
	count, size := cache.Usage()
	metaCost, _ := json.Marshal(Metadata{})
	if count != 64 || size != 64*(1+int64(len(metaCost))) {
		t.Fatal("cache count", count, size)
	}
	cache = NewCache()
	payload := make([]byte, (32<<20)-len(metaCost))
	for i := 0; i < 9; i++ {
		cache.put(fmt.Sprint(i), &Module{bytes: payload})
	}
	count, size = cache.Usage()
	if count != 8 || size != 256<<20 || cache.get("0") != nil {
		t.Fatal("cache byte bound", count, size)
	}
	l = DefaultLimits()
	rss, e := l.rssLimit()
	if e != nil || rss != 1<<30 {
		t.Fatal("default RSS budget", rss, e)
	}
	l.ModuleBytes = math.MaxInt64
	if _, e = l.rssLimit(); e == nil {
		t.Fatal("RSS overflow accepted")
	}
	before := m.Metadata()
	copyMeta := m.Metadata()
	copyMeta.Imports[0] = "injected"
	if m.Metadata().Imports[0] != before.Imports[0] {
		t.Fatal("metadata getter is mutable")
	}
}
func TestWasmResultHandlesQuotasAndCancellation(t *testing.T) {
	store, e := artifacts.Open(artifacts.Options{Directory: filepath.Join(t.TempDir(), "spool"), QuotaBytes: 4096, MaxObjects: 128, MaxPending: 2, TransferTimeout: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := DefaultLimits()
	s := newSession(ctx, store, l, &diagnostics{})
	call := func(session *session, method string, args any) (map[string]int64, int32) {
		b, _ := json.Marshal(map[string]any{"method": method, "args": args})
		id, code := session.call(b)
		if code != 0 {
			return nil, code
		}
		var data []byte
		for {
			b, code := session.read(id, Chunk)
			if code < 0 {
				t.Fatal(code)
			}
			data = append(data, b...)
			if code == 0 {
				break
			}
		}
		if session.close(id) != 0 {
			t.Fatal("close control")
		}
		out := map[string]int64{}
		json.Unmarshal(data, &out)
		return out, 0
	}
	h, code := call(s, "result.file", map[string]any{"name": "f.bin", "mime_type": "application/octet-stream"})
	if code != 0 {
		t.Fatal(code)
	}
	if n := s.write(h["body_handle"], bytes.Repeat([]byte{1}, 4096)); n != 4096 {
		t.Fatal(n)
	}
	if n := s.write(h["body_handle"], []byte{1}); n != limit {
		t.Fatal("shared quota bypass", n)
	}
	other := newSession(ctx, store, l, &diagnostics{})
	if _, n := other.read(h["body_handle"], 1); n != invalid {
		t.Fatal("other instance handle accepted")
	}
	if n := other.write(h["body_handle"], []byte{1}); n != invalid {
		t.Fatal("other instance write accepted")
	}
	cancel()
	if n := s.dispatch(packet{Op: "write", Handle: h["body_handle"], Data: []byte{1}}).Count; n != canceled {
		t.Fatal("cancel ignored")
	}
	if e = s.cleanup(); e != nil {
		t.Fatal(e)
	}
	if store.Usage() != 0 {
		t.Fatal("quota not reclaimed")
	}
	ctx = context.Background()
	l = DefaultLimits()
	l.Handles = 3
	s = newSession(ctx, store, l, &diagnostics{})
	if _, code = call(s, "result.open", map[string]any{}); code != 0 {
		t.Fatal(code)
	}
	if _, code = call(s, "result.file", map[string]any{"name": "f.bin", "mime_type": "application/octet-stream"}); code != limit {
		t.Fatal("handle limit bypass", code)
	}
	s.cleanup()
	for _, raw := range []string{`{"data":null,"files":[{"handle":1}]}`, `{"data":[],"files":null}`, `{"error":{"kind":"internal","category":"output","detail":"secret"}}`, `{"data":null,"files":[],"path":"secret"}`} {
		s = newSession(context.Background(), store, DefaultLimits(), &diagnostics{})
		h, _ := call(s, "result.open", map[string]any{})
		if code = s.write(h["body_handle"], []byte(raw)); code != int32(len(raw)) {
			t.Fatal(code)
		}
		if code = s.close(h["body_handle"]); code != 0 {
			t.Fatal(code)
		}
		if _, code = call(s, "result.commit", map[string]any{"result_handle": h["result_handle"]}); code != invalid || !s.failed {
			t.Fatal("invalid result did not pin failure", raw)
		}
		s.cleanup()
	}
}
func TestWasmDiagnosticsCommonSinkThresholdsAndConcurrency(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			var output bytes.Buffer
			sink, e := agent.NewDiagnosticSink(&output, nil, level)
			if e != nil {
				t.Fatal(e)
			}
			d := &diagnostics{emit: func(level, event, message, detail string) {
				if e := sink.Write(agent.DiagnosticRecord{Level: level, Source: "wasm", Event: event, Message: message, Detail: detail, Capability: "run", RequestID: "req"}); e != nil {
					t.Fatal(e)
				}
			}}
			var writers sync.WaitGroup
			writers.Add(3)
			go func() {
				defer writers.Done()
				for i := 0; i < 70; i++ {
					d.stream(false, []byte(strings.Repeat("界", 6000)))
				}
			}()
			go func() {
				defer writers.Done()
				for i := 0; i < 70; i++ {
					d.stream(true, []byte(strings.Repeat("e", 18000)))
				}
			}()
			go func() {
				defer writers.Done()
				for i := 0; i < 70; i++ {
					d.log("debug", strings.Repeat("l", 18000))
				}
			}()
			writers.Wait()
			d.stream(true, []byte("TAIL-END\xff\x00"))
			d.finish("fixed host reason")
			sink.Close()
			decoder := json.NewDecoder(&output)
			truncated := 0
			tail := ""
			records := 0
			for {
				var record map[string]any
				e := decoder.Decode(&record)
				if e == io.EOF {
					break
				}
				if e != nil {
					t.Fatal(e)
				}
				records++
				if record["source"] != "wasm" || record["capability"] != "run" || record["request_id"] != "req" {
					t.Fatal("diagnostic context")
				}
				if record["event"] == "log_truncated" {
					truncated++
				}
				if record["event"] == "wasm_stderr_tail" {
					tail += record["detail"].(string)
				}
				if level == "error" && record["level"] != "error" {
					t.Fatal("filter applied after emission")
				}
			}
			if level == "error" && truncated != 0 || level != "error" && truncated != 1 {
				t.Fatal("truncation/filter", truncated)
			}
			if records == 0 || !strings.Contains(tail, "TAIL-END") || len(tail) < 60000 {
				t.Fatal("tail lost at threshold", len(tail))
			}
		})
	}
}
func TestWasmRealWorkerExplicitCancellation(t *testing.T) {
	r, m, c := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var once sync.Once
	r.Observe = func(Observation) { once.Do(cancel) }
	timer := time.AfterFunc(1300*time.Millisecond, cancel)
	defer timer.Stop()
	out, e := r.Execute(ctx, m, map[string]any{"scenario": "loop"}, c, DefaultLimits())
	if out != nil {
		out.Close()
		t.Fatal("cancel published")
	}
	errorCode(t, e, "EXECUTION_TIMEOUT")
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("expected explicit cancel")
	}
}

func TestWasmDiagnosticsFullInvalidByteTailReplay(t *testing.T) {
	d := &diagnostics{}
	var tail string
	count := 0
	d.emit = func(level, event, message, detail string) {
		if !utf8.ValidString(message) || !utf8.ValidString(detail) || len(message) > 16<<10 || len(detail) > 16<<10 {
			t.Fatal("diagnostic payload bound")
		}
		if event == "wasm_stderr_tail" {
			if level != "error" {
				t.Fatal("tail filter")
			}
			tail += detail
			count++
		}
	}
	d.stream(false, bytes.Repeat([]byte("x"), 1<<20))
	raw := bytes.Repeat([]byte{0xff, 0}, Chunk/2)
	d.stream(true, append(bytes.Repeat([]byte("old"), 30000), raw...))
	if len(d.tail) != Chunk || !bytes.Equal(d.tail, raw) || d.raw != 1<<20 {
		t.Fatal("raw ring/budget")
	}
	d.finish("fixed reason")
	if tail != strings.ToValidUTF8(string(raw), "\uFFFD") || count < 4 || count > 9 {
		t.Fatal("full ordered tail replay", count, len(tail))
	}
}

func TestWasmDiagnosticsUTF8SplitPipeBoundary(t *testing.T) {
	var messages string
	d := &diagnostics{emit: func(_, event, message, _ string) {
		if event == "wasm_stdout" {
			messages += message
		}
	}}
	prefix := strings.Repeat("a", (16<<10)-1)
	runeBytes := []byte("界")
	d.stream(false, append([]byte(prefix), runeBytes[0]))
	d.stream(false, append(runeBytes[1:], []byte("x\n")...))
	d.finish("")
	if messages != prefix+"界x" {
		t.Fatal("valid rune was replaced across pipe reads")
	}
}

func TestWasmModuleValidationBudgetRefusesUnfinishedValidation(t *testing.T) {
	// The fixed budget recorded in architecture.md and the public Docs.
	if ModuleValidationBudget != 60*time.Second {
		t.Fatal("module validation budget changed without updating its record", ModuleValidationBudget)
	}
	r, _, _ := setup(t)
	fresh := func() *Runner {
		copy := *r
		copy.Cache = NewCache()
		return &copy
	}
	runner := fresh()
	// One budget expires before the worker starts, the other while the worker
	// compiles; both refuse the module with a diagnostic naming the budget.
	for _, budget := range []time.Duration{time.Nanosecond, 50 * time.Millisecond} {
		validationBudget = budget
		m, e := runner.Prepare(context.Background(), modulePath, "", DefaultLimits())
		validationBudget = ModuleValidationBudget
		var w *Error
		if m != nil || e == nil || !errors.As(e, &w) || w.Code != "EXECUTION_TIMEOUT" || !strings.Contains(e.Error(), "Wasm module validation did not finish within "+budget.String()) {
			t.Fatal("unfinished validation was not refused with the budget diagnostic", budget, m, e)
		}
	}
	// A refusal is not cached: the fixed budget then validates the same module.
	if m, e := runner.Prepare(context.Background(), modulePath, "", DefaultLimits()); e != nil || m == nil {
		t.Fatal("module refused after a budget refusal", e)
	}
	// A caller deadline that ends first is the caller's, not the budget's.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if _, e := fresh().Prepare(ctx, modulePath, "", DefaultLimits()); e == nil || strings.Contains(e.Error(), "did not finish within") {
		t.Fatal("caller deadline reported as the validation budget", e)
	}
}
