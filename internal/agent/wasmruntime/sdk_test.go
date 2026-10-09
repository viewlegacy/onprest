package wasmruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/contract"
)

var sdkBuild struct {
	sync.Once
	path string
	err  error
}

func sdkFixture(t *testing.T) string {
	t.Helper()
	sdkBuild.Do(func() {
		sdkBuild.path = filepath.Join(filepath.Dir(binaryPath), "sdk.wasm")
		build := exec.Command("go", "build", "-trimpath", "-mod=readonly", "-o", sdkBuild.path, ".")
		build.Dir = "testdata/sdkfixture"
		build.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
		if b, e := build.CombinedOutput(); e != nil {
			sdkBuild.err = fmt.Errorf("SDK fixture: %v: %s", e, b)
			return
		}
		raw, e := os.ReadFile(sdkBuild.path)
		if e != nil {
			sdkBuild.err = e
			return
		}
		ctx := context.Background()
		r := wazero.NewRuntime(ctx)
		defer r.Close(ctx)
		m, e := r.CompileModule(ctx, raw)
		if e != nil {
			sdkBuild.err = e
			return
		}
		defer m.Close(ctx)
		meta := Metadata{ABI: ABI, MinimumAgentVersion: CompatibilityVersion}
		for _, f := range m.ImportedFunctions() {
			a, b, _ := f.Import()
			meta.Imports = append(meta.Imports, a+"."+b)
		}
		sort.Strings(meta.Imports)
		meta.Imports = compactImports(meta.Imports)
		sdkBuild.err = os.WriteFile(sdkBuild.path, addMetadata(raw, meta), 0600)
	})
	if sdkBuild.err != nil {
		t.Fatal(sdkBuild.err)
	}
	return sdkBuild.path
}
func sdkSetup(t *testing.T, on bool, access string) (*Runner, *Module, *contract.Capability, string) {
	t.Helper()
	path := sdkFixture(t)
	r, _, c, base := bridgeSetup(t, on, access)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m, e := r.Prepare(ctx, path, "", DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	return r, m, c, base
}
func sdkRun(t *testing.T, r *Runner, m *Module, c *contract.Capability, params map[string]any, l Limits) (*contract.PreparedOutput, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return r.Execute(ctx, m, params, c, l)
}
func sdkData(t *testing.T, r *Runner, m *Module, c *contract.Capability, params map[string]any, l Limits) map[string]any {
	t.Helper()
	out, e := sdkRun(t, r, m, c, params, l)
	if e != nil {
		t.Fatalf("SDK: %+v", e)
	}
	defer out.Close()
	return out.Data.(map[string]any)
}
func TestSDKRealWasmDecodePrecisionNullAndMismatch(t *testing.T) {
	r, m, c, _ := sdkSetup(t, false, "")
	logs := []string{}
	r.Diagnostic = func(level, event, message, detail string) { logs = append(logs, message+detail) }
	v := sdkData(t, r, m, c, map[string]any{"scenario": "decode", "i32": int64(-2147483648), "i64": int64(9007199254740993), "uint": int64(9223372036854775807), "int": int64(9223372036854775807), "float": float64(1.5), "custom": int64(4), "pointer": nil, "raw": nil, "any": map[string]any{"n": int64(9007199254740993)}, "embedded": "value"}, DefaultLimits())
	if v["i64"] != int64(9007199254740993) || v["uint"] != int64(9223372036854775807) || v["int"] != int64(9223372036854775807) || v["bits"] != int64(64) || v["pointer"] != nil || v["raw"] != nil || v["custom"] != int64(5) || v["embedded"] != "value" || v["any"].(map[string]any)["n"] != int64(9007199254740993) {
		t.Fatal(v)
	}
	for _, test := range []struct {
		key   string
		value any
	}{{"i32", int64(2147483648)}, {"uint", int64(-1)}, {"i64", float64(1.5)}, {"float", float64(1e100)}, {"i32", nil}, {"unknown", "private-value"}, {"I64", int64(1)}, {"custom", int64(2147483648)}, {"any", map[string]any{"__onprest_input_file_v1": map[string]any{"file_id": "private-id"}}}} {
		t.Run(test.key+fmt.Sprint(test.value), func(t *testing.T) {
			logs = nil
			_, e := sdkRun(t, r, m, c, map[string]any{"scenario": "decode", test.key: test.value}, DefaultLimits())
			errorCode(t, e, "EXECUTION_FAILED")
			joined := strings.Join(logs, "\n")
			if strings.Contains(joined, "SDK_HANDLER_ENTERED") || strings.Contains(joined, "private-value") || strings.Contains(joined, "private-id") {
				t.Fatal(joined)
			}
		})
	}
}
func TestSDKRealWasmResultFilesAndOwnership(t *testing.T) {
	for _, scenario := range []string{"data", "nil", "files", "mixed", "invalid-data", "nil-reader", "read-error", "close-error", "handler-error", "early-close", "panic"} {
		t.Run(scenario, func(t *testing.T) {
			r, m, c, base := sdkSetup(t, false, "")
			os.WriteFile(filepath.Join(base, "value"), []byte("value"), 0600)
			var logs []string
			r.Diagnostic = func(level, event, message, detail string) { logs = append(logs, message+detail) }
			out, e := sdkRun(t, r, m, c, map[string]any{"scenario": scenario, "path": "value"}, DefaultLimits())
			expected := map[string]string{"invalid-data": "OUTPUT_INVALID", "nil-reader": "OUTPUT_INVALID", "read-error": "OUTPUT_INVALID", "close-error": "OUTPUT_INVALID", "handler-error": "APP_PRIMARY", "early-close": "OUTPUT_INVALID", "panic": "EXECUTION_FAILED"}[scenario]
			if expected != "" {
				errorCode(t, e, expected)
				if out != nil {
					t.Fatal("failed output published")
				}
			} else {
				if e != nil {
					t.Fatalf("%+v", e)
				}
				want := 0
				if scenario == "files" || scenario == "mixed" {
					want = 2
				}
				if len(out.Files) != want {
					t.Fatal(out.Files)
				}
				for i, f := range out.Files {
					b, err := io.ReadAll(f.Reader)
					if err != nil {
						t.Fatal(err)
					}
					payload := []byte{}
					if i == 0 {
						payload = []byte{0, 255, 1, 2}
					}
					sum := sha256.Sum256(payload)
					if !bytes.Equal(b, payload) || f.SizeBytes != int64(len(b)) || f.SHA256 != hex.EncodeToString(sum[:]) {
						t.Fatal(f.ArtifactMetadata, b)
					}
				}
				if err := out.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "read-error" || scenario == "close-error" || scenario == "handler-error" {
				joined := strings.Join(logs, "\n")
				if strings.Count(joined, "EXTERNAL_CLOSE:1") != 1 || strings.Contains(joined, "EXTERNAL_CLOSE:2") {
					t.Fatal(joined)
				}
			}
			if r.Store.Usage() != 0 {
				t.Fatal(r.Store.Usage())
			}
		})
	}
}
func TestSDKRealWasmFilesPathsAndLimits(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprint(on), func(t *testing.T) {
			r, m, c, base := sdkSetup(t, on, "read_write")
			v := sdkData(t, r, m, c, map[string]any{"scenario": "fs", "path": "."}, DefaultLimits())
			if v["bytes"] != int64(4) || v["copy"] != true || v["sys_nil"] != true || v["name"] != "file.bin" || v["size"] != int64(4) {
				t.Fatal(v)
			}
			if _, e := os.Stat(filepath.Join(base, "child")); !os.IsNotExist(e) {
				t.Fatal(e)
			}
			v = sdkData(t, r, m, c, map[string]any{"scenario": "path"}, DefaultLimits())
			if v["invalid"] != true || v["replacement"] != true {
				t.Fatal(v)
			}
			for _, n := range []int64{65535, 65536, 65537} {
				v = sdkData(t, r, m, c, map[string]any{"scenario": "chunks", "path": "chunks", "i64": n}, DefaultLimits())
				if v["bytes"] != n || v["equal"] != true {
					t.Fatal(v)
				}
			}
			v = sdkData(t, r, m, c, map[string]any{"scenario": "fs-errors", "path": "."}, DefaultLimits())
			for _, key := range []string{"notfound", "no_parent", "invalid", "link", "empty", "perm_invalid"} {
				if v[key] != true {
					t.Fatal(v)
				}
			}
			target := filepath.Join(base, "write-budget")
			for _, n := range []int64{4, 5, 6} {
				if e := os.WriteFile(target, []byte("original"), 0600); e != nil {
					t.Fatal(e)
				}
				l := DefaultLimits()
				l.WriteBytes = n
				l.TotalWriteBytes = n
				v = sdkData(t, r, m, c, map[string]any{"scenario": "write-limit", "path": "write-budget"}, l)
				b, _ := os.ReadFile(target)
				if n < 5 {
					if v["limit"] != true || string(b) != "original" {
						t.Fatal(v, string(b))
					}
				} else if v["limit"] != false || string(b) != "12345" {
					t.Fatal(v, string(b))
				}
			}
			lwrite := DefaultLimits()
			lwrite.WriteBytes = 4
			lwrite.TotalWriteBytes = 4
			v = sdkData(t, r, m, c, map[string]any{"scenario": "stream-limit", "path": "write-budget"}, lwrite)
			b, _ := os.ReadFile(target)
			if v["count"] != int64(4) || v["limit"] != true || v["closed"] != true || string(b) != "1234" {
				t.Fatal(v, string(b))
			}
			if e := os.WriteFile(target, []byte("12345"), 0600); e != nil {
				t.Fatal(e)
			}
			lread := DefaultLimits()
			lread.ReadBytes = 5
			lread.TotalReadBytes = 9
			v = sdkData(t, r, m, c, map[string]any{"scenario": "total-limit", "path": "write-budget"}, lread)
			if v["limit"] != true {
				t.Fatal(v)
			}
			os.WriteFile(filepath.Join(base, "large"), []byte("12345"), 0600)
			for _, n := range []int64{4, 5, 6} {
				l := DefaultLimits()
				l.ReadBytes = n
				l.TotalReadBytes = n
				out, e := sdkRun(t, r, m, c, map[string]any{"scenario": "limit", "path": "large"}, l)
				if n < 5 {
					errorCode(t, e, "LIMIT_EXCEEDED")
				} else {
					if e != nil {
						t.Fatalf("%+v", e)
					}
					out.Close()
				}
			}
			l := DefaultLimits()
			l.ReadBytes = 4
			l.TotalReadBytes = 4
			v = sdkData(t, r, m, c, map[string]any{"scenario": "caught-limit", "path": "large"}, l)
			if v["still_limited"] != true {
				t.Fatal(v)
			}
		})
	}
}
func TestSDKRealWasmDenyAllFSAndHTTP(t *testing.T) {
	r, m, c, base := sdkSetup(t, true, "")
	target := filepath.Join(base, "value")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	v := sdkData(t, r, m, c, map[string]any{"scenario": "fs-permission", "path": "value"}, DefaultLimits())
	if v["permission"] != true || v["link"] != true {
		t.Fatal(v)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/allowed" {
			w.Header().Set("Location", "/forbidden")
			w.WriteHeader(302)
		}
	}))
	defer server.Close()
	v = sdkData(t, r, m, c, map[string]any{"scenario": "http-denied", "url": server.URL}, DefaultLimits())
	if v["permission"] != true || calls.Load() != 0 {
		t.Fatal(v, calls.Load())
	}
	r.Bridge.Sandbox["permissions"].(map[string]any)["http"] = []any{map[string]any{"origin": server.URL, "methods": []string{"POST", "GET"}, "path_prefix": "/allowed", "allowed_cidrs": []string{"127.0.0.0/8"}}}
	v = sdkData(t, r, m, c, map[string]any{"scenario": "redirect-denied", "url": server.URL + "/allowed"}, DefaultLimits())
	if v["permission"] != true || calls.Load() != 1 {
		t.Fatal(v, calls.Load())
	}
	b, _ := os.ReadFile(target)
	if string(b) != "original" {
		t.Fatal(string(b))
	}
}
func TestSDKRealWasmHTTPClientLibraryAndCancellation(t *testing.T) {
	var calls atomic.Int64
	var auth atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		calls.Add(1)
		if q.Header.Get("Authorization") == "Bearer fixture-token" {
			auth.Store(true)
		}
		b, _ := io.ReadAll(q.Body)
		if len(b) > 0 && string(b) != "payload" && q.Method != "GET" {
			t.Errorf("request body %q", b)
		}
		switch q.URL.Path {
		case "/wait":
			<-q.Context().Done()
			return
		case "/slowbody":
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-q.Context().Done()
			return
		case "/redirect":
			w.Header().Set("Location", "/ok")
			w.WriteHeader(302)
			return
		}
		w.Header().Set("X-Fixture", "present")
		w.WriteHeader(503)
		io.WriteString(w, "response")
	}))
	defer server.Close()
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprint(on), func(t *testing.T) {
			r, m, c, _ := sdkSetup(t, on, "")
			if on {
				r.Bridge.Sandbox["permissions"].(map[string]any)["http"] = []any{map[string]any{"origin": server.URL, "methods": []string{"POST", "GET"}, "path_prefix": "/", "allowed_cidrs": []string{"127.0.0.0/8"}}}
			}
			for _, scenario := range []string{"http", "library", "deadline", "cancel", "response-cancel", "redirect", "default-redirect", "request-read-error", "request-close-error", "request-upload-cancel"} {
				path := "/ok"
				if scenario == "deadline" || scenario == "cancel" {
					path = "/wait"
				}
				if scenario == "response-cancel" {
					path = "/slowbody"
				}
				if scenario == "redirect" || scenario == "default-redirect" {
					path = "/redirect"
				}
				t.Log("HTTP scenario", scenario)
				before := calls.Load()
				v := sdkData(t, r, m, c, map[string]any{"scenario": scenario, "url": server.URL + path}, DefaultLimits())
				switch scenario {
				case "default-redirect":
					if v["status"] != int64(302) {
						t.Fatal(v)
					}
				case "request-read-error", "request-close-error", "request-upload-cancel":
					if v["failed"] != true || v["closes"] != int64(1) {
						t.Fatal(v)
					}
				case "deadline":
					if v["deadline"] != true {
						t.Fatal(v)
					}
				case "cancel", "response-cancel":
					if v["cancel"] != true {
						t.Fatal(v)
					}
				default:
					if v["status"] != int64(503) || v["body"] != "response" || v["header"] != "present" {
						t.Fatal(v)
					}
				}
				want := int64(1)
				if scenario == "request-read-error" || scenario == "request-upload-cancel" {
					want = 0
				}
				if scenario == "redirect" {
					want = 2
				}
				if calls.Load()-before != want {
					t.Fatalf("%s sent %d", scenario, calls.Load()-before)
				}
			}
			out, err := sdkRun(t, r, m, c, map[string]any{"scenario": "http-file", "url": server.URL + "/ok"}, DefaultLimits())
			if err != nil || len(out.Files) != 1 {
				t.Fatal(err)
			}
			b, err := io.ReadAll(out.Files[0].Reader)
			if err != nil || string(b) != "response" {
				t.Fatal(err, string(b))
			}
			if err = out.Close(); err != nil {
				t.Fatal(err)
			}

		})
	}
	if !auth.Load() {
		t.Fatal("external OAuth2 client did not use injected Transport")
	}
}
func TestSDKRealWasmInputFilesAndLogPublicErrors(t *testing.T) {
	r, m, c, _ := sdkSetup(t, true, "")
	payload := []byte{0, 255, 1}
	sum := sha256.Sum256(payload)
	f := contract.FileValue{ID: "owned", Name: "input.bin", MIMEType: "application/octet-stream", SizeBytes: 3, SHA256: hex.EncodeToString(sum[:])}
	r.Bridge.Input = map[string]contract.FileValue{"owned": f}
	r.Bridge.OpenInput = func(ctx context.Context, v contract.FileValue) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
	descriptor := map[string]any{"__onprest_input_file_v1": map[string]any{"file_id": f.ID, "name": f.Name, "mime_type": f.MIMEType, "size_bytes": f.SizeBytes, "sha256": f.SHA256}}
	out, e := sdkRun(t, r, m, c, map[string]any{"scenario": "input", "document": descriptor, "documents": []any{descriptor}, "nested": map[string]any{"key": map[string]any{"document": descriptor}}}, DefaultLimits())
	if e != nil {
		t.Fatalf("%+v", e)
	}
	b, e := io.ReadAll(out.Files[0].Reader)
	if e != nil || !bytes.Equal(b, payload) || out.Data.(map[string]any)["reopen"] != int64(3) {
		t.Fatal(out.Data, b, e)
	}
	out.Close()
	_, e = sdkRun(t, r, m, c, map[string]any{"scenario": "input-data", "document": descriptor}, DefaultLimits())
	errorCode(t, e, "OUTPUT_INVALID")
	var logs []string
	r.Diagnostic = func(level, event, message, detail string) { logs = append(logs, level+":"+event+":"+message+detail) }
	v := sdkData(t, r, m, c, map[string]any{"scenario": "log"}, DefaultLimits())
	if v["ok"] != true || !strings.Contains(strings.Join(logs, "\n"), "invalid_log_level") {
		t.Fatal(v, logs)
	}
	for _, test := range []struct{ scenario, code, message, want string }{{"public", "APP_REJECTED", "refusal", "APP_REJECTED"}, {"public", "LIMIT_EXCEEDED", "private", "EXECUTION_FAILED"}, {"public", "APP_A", strings.Repeat("x", 1025), "EXECUTION_FAILED"}, {"public", "APP_A", "control\n", "EXECUTION_FAILED"}, {"general", "", "", "EXECUTION_FAILED"}, {"joined", "", "", "LIMIT_EXCEEDED"}} {
		_, e = sdkRun(t, r, m, c, map[string]any{"scenario": test.scenario, "code": test.code, "message": test.message}, DefaultLimits())
		errorCode(t, e, test.want)
	}
}
func TestSDKAsyncBoundsCancellationAndCommitBarrier(t *testing.T) {
	r, _, _, _ := bridgeSetup(t, false, "")
	s := newSession(context.Background(), r.Store, DefaultLimits(), &diagnostics{})
	s.bridge = r.Bridge
	s.roots = map[string]*filesystem.Root{}
	defer s.cleanup()
	s.l.HostCalls = 1
	blocked := make(chan struct{})
	released := make(chan struct{})
	s.bridge.Input = map[string]contract.FileValue{"id": {ID: "id"}}
	s.bridge.OpenInput = func(ctx context.Context, _ contract.FileValue) (io.ReadCloser, error) {
		close(blocked)
		<-ctx.Done()
		close(released)
		return nil, ctx.Err()
	}
	call := func(method string, args any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"method": method, "args": args})
		p := s.dispatch(packet{Op: "call", Data: raw})
		if p.Count < 0 {
			t.Fatalf("call %s: %d", method, p.Count)
		}
		v := s.dispatch(packet{Op: "read", Handle: p.Handle, Count: Chunk})
		s.dispatch(packet{Op: "close", Handle: p.Handle})
		var m map[string]any
		d := json.NewDecoder(bytes.NewReader(v.Data))
		d.UseNumber()
		if err := d.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	opened := call("runtime.begin", map[string]any{"op": "call", "deadline_ns": 0, "request": map[string]any{"method": "input.open", "args": map[string]any{"file_id": "id"}}})
	id, _ := numberHandle(opened["operation_handle"])
	<-blocked
	logStart := time.Now()
	call("log.write", map[string]any{"level": "info", "message": "during pending I/O"})
	if time.Since(logStart) > time.Second {
		t.Fatal("diagnostic blocked scheduler")
	}
	raw, _ := json.Marshal(map[string]any{"method": "runtime.begin", "args": map[string]any{"op": "read", "deadline_ns": 0, "handle": 1, "count": 1}})
	if v := s.dispatch(packet{Op: "call", Data: raw}); v.Count != limit {
		t.Fatal(v)
	}
	// Exhaust the shared slots after admission: reserved control/data still let
	// cancellation and a terminal poll drain the job without allocating.
	s.async.mu.Lock()
	var extra []int64
	for s.handleCount() < s.l.Handles {
		extra = append(extra, s.asyncID(&asyncEntry{kind: "buffer"}))
	}
	s.async.mu.Unlock()
	call("runtime.cancel", map[string]any{"operation_handle": id})
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("cancel blocked behind host I/O")
	}
	for {
		v := call("runtime.poll", map[string]any{"operation_handle": id})
		if v["ready"] == true {
			if n, _ := v["count"].(json.Number).Int64(); n != int64(canceled) {
				t.Fatal(v)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	s.async.mu.Lock()
	for _, id := range extra {
		s.asyncDelete(id)
	}
	s.async.mu.Unlock()
	if s.asyncJobs() != 0 || s.handleCount() != 0 {
		t.Fatal(s.asyncJobs(), s.handleCount())
	}
	blocked = make(chan struct{})
	released = make(chan struct{})
	call("runtime.begin", map[string]any{"op": "call", "deadline_ns": 0, "request": map[string]any{"method": "input.open", "args": map[string]any{"file_id": "id"}}})
	<-blocked
	commit, _ := json.Marshal(map[string]any{"method": "result.commit", "args": map[string]any{"result_handle": 1}})
	start := time.Now()
	if v := s.dispatch(packet{Op: "call", Data: commit}); v.Count != invalid || time.Since(start) > time.Second || !s.asyncFailure.Load() {
		t.Fatal("commit admitted unfinished work", v)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("commit barrier did not cancel job")
	}
	s.stopAsync()
	if s.asyncJobs() != 0 || s.handleCount() != 0 {
		t.Fatal("stop leaked jobs/slots")
	}

}

func TestSDKPublishedCustomerExample(t *testing.T) {
	r, _, c, _ := bridgeSetup(t, true, "")
	module := filepath.Join(t.TempDir(), "customer.wasm")
	build := exec.Command("go", "run", "./examples/sdk/build", "--output", module)
	build.Dir = "../../.."
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m, e := r.Prepare(ctx, module, "", DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	out, e := r.Execute(ctx, m, map[string]any{"customer": "Alice"}, c, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	if out.Data.(map[string]any)["customer"] != "Alice" || len(out.Files) != 1 {
		t.Fatal(out.Data, len(out.Files))
	}
	b, e := io.ReadAll(out.Files[0].Reader)
	if e != nil || string(b) != "customer\nAlice\n" || out.Files[0].Name != "customer.csv" || out.Files[0].MIMEType != "text/csv" {
		t.Fatal(string(b), e, out.Files[0].ArtifactMetadata)
	}
}

func TestSDKAsyncAdmissionMalformedAndOpenReclamation(t *testing.T) {
	r, _, _, _ := bridgeSetup(t, false, "")
	s := newSession(context.Background(), r.Store, DefaultLimits(), &diagnostics{})
	s.bridge = r.Bridge
	s.roots = map[string]*filesystem.Root{}
	defer s.cleanup()
	invoke := func(method string, args any) packet {
		raw, _ := json.Marshal(map[string]any{"method": method, "args": args})
		return s.dispatch(packet{Op: "call", Data: raw})
	}
	decodeControl := func(p packet) map[string]any {
		t.Helper()
		if p.Count < 0 {
			t.Fatal(p)
		}
		b := s.dispatch(packet{Op: "read", Handle: p.Handle, Count: Chunk})
		s.dispatch(packet{Op: "close", Handle: p.Handle})
		d := json.NewDecoder(bytes.NewReader(b.Data))
		d.UseNumber()
		var v map[string]any
		if e := d.Decode(&v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	for _, args := range []any{
		map[string]any{"op": "call", "deadline_ns": 0, "request": map[string]any{"method": "http.do", "args": "malformed"}},
		map[string]any{"op": "read", "deadline_ns": 0, "handle": 1, "count": Chunk + 1},
		map[string]any{"op": "call", "deadline_ns": -1, "request": map[string]any{"method": "input.open", "args": map[string]any{}}},
	} {
		if p := invoke("runtime.begin", args); p.Count != invalid || s.asyncJobs() != 0 {
			t.Fatal(p, s.asyncJobs())
		}
	}
	v := decodeControl(invoke("runtime.buffer", map[string]any{}))
	buffer, _ := numberHandle(v["body_handle"])
	if p := s.dispatch(packet{Op: "write", Handle: buffer, Data: make([]byte, Chunk+1)}); p.Count != limit {
		t.Fatal(p)
	}
	s.async.mu.Lock()
	s.asyncDelete(buffer)
	s.async.mu.Unlock()
	blocked, release := make(chan struct{}), make(chan struct{})
	var closes atomic.Int64
	s.bridge.Input = map[string]contract.FileValue{"id": {ID: "id"}}
	s.bridge.OpenInput = func(context.Context, contract.FileValue) (io.ReadCloser, error) {
		close(blocked)
		<-release
		return &sdkCountCloser{Reader: bytes.NewReader(nil), closes: &closes}, nil
	}
	v = decodeControl(invoke("runtime.begin", map[string]any{"op": "call", "deadline_ns": 0, "request": map[string]any{"method": "input.open", "args": map[string]any{"file_id": "id"}}}))
	id, _ := numberHandle(v["operation_handle"])
	<-blocked
	s.async.mu.Lock()
	for s.handleCount() < s.l.Handles {
		s.asyncID(&asyncEntry{kind: "buffer"})
	}
	s.async.mu.Unlock()
	close(release)
	for {
		v = decodeControl(invoke("runtime.poll", map[string]any{"operation_handle": id}))
		if v["ready"] == true {
			n, _ := v["count"].(json.Number).Int64()
			if n != int64(limit) {
				t.Fatal(v)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	if closes.Load() != 1 || s.asyncJobs() != 0 {
		t.Fatal(closes.Load(), s.asyncJobs())
	}
	s.stopAsync()
	if s.handleCount() != 0 {
		t.Fatal(s.handleCount())
	}
}

type sdkCountCloser struct {
	io.Reader
	closes *atomic.Int64
}

func (r *sdkCountCloser) Close() error { r.closes.Add(1); return nil }
