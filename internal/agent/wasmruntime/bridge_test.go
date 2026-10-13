package wasmruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func bridgeSetup(t *testing.T, on bool, access string) (*Runner, *Module, *contract.Capability, string) {
	t.Helper()
	r, m, c := setup(t)
	base := t.TempDir()
	sb := map[string]any{"enabled": on}
	if on {
		grants := []any{}
		if access != "" {
			grants = append(grants, map[string]any{"root": base, "access": access})
		}
		sb["permissions"] = map[string]any{"filesystem": grants, "http": []any{}, "environment": []string{}}
	}
	r.Bridge = &Bridge{Base: base, Sandbox: sb, Environment: map[string]string{"PWD": "/unrelated", "WASM_TEST_SECRET": "secret"}, Effects: &providers.Effects{}}
	r.Bridge.Effects.Start()
	return r, m, c, base
}
func bridgeRun(t *testing.T, r *Runner, m *Module, c *contract.Capability, params map[string]any, l Limits) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, e := r.Execute(ctx, m, params, c, l)
	if e != nil {
		t.Fatalf("worker: %+v", e)
	}
	defer out.Close()
	if n := r.Store.Usage(); n != 0 {
		t.Fatalf("spool leak %d", n)
	}
	return out.Data
}
func step(method string, args map[string]any, data []byte, read bool) map[string]any {
	return map[string]any{"method": method, "args": args, "data": base64.StdEncoding.EncodeToString(data), "read": read}
}
func entries(v any) []any  { return v.([]any) }
func callCode(v any) int64 { return v.(map[string]any)["code"].(int64) }
func TestWasmBridgeNineOperationsAndRawWASI(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			r, m, c, base := bridgeSetup(t, on, "read_write")
			payload := []byte{0, 255, 1, 2, 0, 17}
			params := map[string]any{"scenario": "bridge", "steps": []any{
				step("files.mkdir_all", map[string]any{"path": "a/b", "perm": int64(0755)}, nil, false),
				step("files.create", map[string]any{"path": "a/b/x.bin"}, payload, false),
				step("files.open", map[string]any{"path": "a/b/x.bin"}, nil, true),
				step("files.read_file", map[string]any{"path": "a/b/x.bin"}, nil, true),
				step("files.stat", map[string]any{"path": "a/b/x.bin"}, nil, false),
				step("files.read_dir", map[string]any{"path": "a/b"}, nil, true),
				step("files.write_file", map[string]any{"path": "a/b/x.bin", "size": int64(0), "perm": int64(0600)}, nil, false),
				step("files.rename", map[string]any{"old_path": "a/b/x.bin", "new_path": "a/b/y.bin"}, nil, false),
				step("files.remove", map[string]any{"path": "a/b/y.bin"}, nil, false)}}
			v := entries(bridgeRun(t, r, m, c, params, DefaultLimits()))
			for i, item := range v {
				if callCode(item) <= 0 {
					t.Fatalf("step %d rejected %+v", i, item)
				}
			}
			for _, i := range []int{2, 3} {
				if v[i].(map[string]any)["data"] != base64.StdEncoding.EncodeToString(payload) {
					t.Fatal(v[i])
				}
			}
			if _, e := os.Stat(filepath.Join(base, "a/b/y.bin")); !os.IsNotExist(e) {
				t.Fatal(e)
			}
			raw := bridgeRun(t, r, m, c, map[string]any{"scenario": "rawlife", "path": filesystem.WASIPath(base)}, DefaultLimits())
			if raw != base64.StdEncoding.EncodeToString([]byte{'z', 'b', 'c', 'd', 0, 0}) {
				t.Fatal(raw)
			}
		})
	}
}
func TestWasmBridgePermissionsPathsAndHostErrors(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			r, m, c, base := bridgeSetup(t, on, "read")
			os.WriteFile(filepath.Join(base, "ok"), []byte("abc"), 0600)
			steps := []any{step("files.open", map[string]any{"path": "missing"}, nil, true), step("files.open", map[string]any{"path": "bad\x00"}, nil, true), step("files.rename", map[string]any{"old_path": "ok", "new_path": "bad\x00"}, nil, false), step("files.write_file", map[string]any{"path": "ok", "size": int64(4), "perm": int64(0600)}, []byte("evil"), false), map[string]any{"raw": "{\"method\":\"files.open\",\"args\":{\"path\":\"\\ud800\"}}"}}
			v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": steps}, DefaultLimits()))
			if callCode(v[0]) != -3 || callCode(v[1]) != -1 || callCode(v[2]) != -1 {
				t.Fatal(v)
			}
			if on {
				if callCode(v[3]) != -2 {
					t.Fatal(v)
				}
				b, _ := os.ReadFile(filepath.Join(base, "ok"))
				if string(b) != "abc" {
					t.Fatal(string(b))
				}
			}
		})
	}
	r, m, c, base := bridgeSetup(t, true, "")
	os.WriteFile(filepath.Join(base, "deny"), []byte("sensitive"), 0600)
	v := bridgeRun(t, r, m, c, map[string]any{"scenario": "rawfs", "path": filepath.Join(base, "deny")}, DefaultLimits()).(map[string]any)
	if v["body"] != "" || v["error"] == "" {
		t.Fatal(v)
	}
}
func TestWasmBridgeSingleTotalReadWriteAndReopen(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			r, m, c, base := bridgeSetup(t, on, "read_write")
			os.WriteFile(filepath.Join(base, "x"), []byte("abcd"), 0600)
			l := DefaultLimits()
			l.ReadBytes = 4
			l.TotalReadBytes = 7
			l.WriteBytes = 3
			l.TotalWriteBytes = 4
			steps := []any{step("files.open", map[string]any{"path": "x"}, nil, true), step("files.open", map[string]any{"path": "x"}, nil, true), step("files.write_file", map[string]any{"path": "x", "size": int64(4), "perm": int64(0600)}, []byte("zzzz"), false), step("files.create", map[string]any{"path": "out"}, []byte("long"), false)}
			v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": steps}, l))
			if v[0].(map[string]any)["data"] != base64.StdEncoding.EncodeToString([]byte("abcd")) || v[1].(map[string]any)["body_code"] != int64(-4) || callCode(v[2]) != -4 || v[3].(map[string]any)["write_codes"].([]any)[0] != int64(3) {
				t.Fatal(v)
			}
			b, _ := os.ReadFile(filepath.Join(base, "x"))
			if string(b) != "abcd" {
				t.Fatal(string(b))
			}
			b, _ = os.ReadFile(filepath.Join(base, "out"))
			if string(b) != "lon" {
				t.Fatal(string(b))
			}
			l = DefaultLimits()
			l.ReadBytes = 4
			l.TotalReadBytes = 7
			v = entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "rawseek", "path": filesystem.WASIPath(filepath.Join(base, "x"))}, l))
			if v[0] != "abcd" || v[1] == "abcd" || v[2] == "abcd" {
				t.Fatal(v)
			}
		})
	}
}
func TestWasmBridgeInputOwnershipHTTPAndEnvironment(t *testing.T) {
	r, m, c, base := bridgeSetup(t, true, "")
	var opened atomic.Int32
	r.Bridge.Input = map[string]contract.FileValue{"owned": {ID: "owned", SizeBytes: 4}}
	r.Bridge.OpenInput = func(context.Context, contract.FileValue) (io.ReadCloser, error) {
		opened.Add(1)
		return io.NopCloser(strings.NewReader("file")), nil
	}
	steps := []any{step("input.open", map[string]any{"file_id": "foreign"}, nil, true), step("input.open", map[string]any{"file_id": "owned"}, nil, true), step("input.open", map[string]any{"file_id": "owned"}, nil, true)}
	v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": steps}, DefaultLimits()))
	if callCode(v[0]) != -2 || opened.Load() != 2 || v[1].(map[string]any)["data"] != base64.StdEncoding.EncodeToString([]byte("file")) {
		t.Fatal(v, opened.Load())
	}
	r.Bridge.Environment = map[string]string{}
	raw := bridgeRun(t, r, m, c, map[string]any{"scenario": "rawfs", "path": "denied"}, DefaultLimits()).(map[string]any)
	if raw["pwd"] != filesystem.WASIPath(base) || raw["secret"] != "" {
		t.Fatal(raw)
	}
	var sent atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sent.Add(1)
		b, _ := io.ReadAll(req.Body)
		w.WriteHeader(418)
		w.Write(b)
	}))
	defer server.Close()
	r.Bridge.Sandbox = map[string]any{"enabled": false}
	steps = []any{step("http.open", map[string]any{"method": "POST", "url": server.URL, "headers": map[string]any{}, "content_length": int64(4)}, []byte("body"), false), step("http.do", nil, nil, true)}
	v = entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": steps}, DefaultLimits()))
	if sent.Load() != 1 || v[1].(map[string]any)["control"].(map[string]any)["status"] != int64(418) || v[1].(map[string]any)["data"] != base64.StdEncoding.EncodeToString([]byte("body")) {
		t.Fatal(v, sent.Load())
	}
	r.Bridge.Sandbox = map[string]any{"enabled": true, "permissions": map[string]any{"filesystem": []any{}, "http": []any{}, "environment": []string{}}}
	v = entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": steps[:1]}, DefaultLimits()))
	if callCode(v[0]) != -2 || sent.Load() != 1 {
		t.Fatal(v, sent.Load())
	}
	rawJSON, _ := json.Marshal(v)
	if strings.Contains(string(rawJSON), "body") {
		t.Fatal("rejected body leaked", string(rawJSON))
	}
}

func TestWasmBridgeMetadataOperationsLinksAndBarrier(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			r, m, c, base := bridgeSetup(t, on, "read_write")
			os.WriteFile(filepath.Join(base, "a"), []byte("abcd"), 0600)
			stat := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{step("files.stat", map[string]any{"path": "a"}, nil, false)}}, DefaultLimits()))[0].(map[string]any)["control"].(map[string]any)
			encoded, e := contract.CanonicalJSON(stat)
			if e != nil {
				t.Fatal(e)
			}
			for _, delta := range []int64{-1, 0, 1} {
				l := DefaultLimits()
				l.ReadBytes = int64(len(encoded)) + delta
				l.TotalReadBytes = l.ReadBytes
				v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{step("files.stat", map[string]any{"path": "a"}, nil, false)}}, l))
				if (callCode(v[0]) > 0) != (delta >= 0) {
					t.Fatal(delta, v)
				}
			}
			l := DefaultLimits()
			l.FileOperations = 2
			v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{step("files.open", map[string]any{"path": "missing"}, nil, false), step("files.mkdir_all", map[string]any{"path": "one/two/three", "perm": int64(0700)}, nil, false), step("files.stat", map[string]any{"path": "a"}, nil, false)}}, l))
			if callCode(v[0]) != -3 || callCode(v[1]) != -4 || callCode(v[2]) != -4 {
				t.Fatal(v)
			}
			if _, e := os.Stat(filepath.Join(base, "one/two")); !os.IsNotExist(e) {
				t.Fatal("unreserved mkdir executed", e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, e := r.Execute(ctx, m, map[string]any{"scenario": "aftercommitfs", "path": filesystem.WASIPath(filepath.Join(base, "a"))}, c, DefaultLimits())
			if e == nil {
				out.Close()
				t.Fatal("postcommit business read accepted")
			}
			if r.Store.Usage() != 0 {
				t.Fatal("postcommit cleanup leaked")
			}
		})
	}
	r, m, c, base := bridgeSetup(t, true, "read_write")
	outside := filepath.Join(t.TempDir(), "private")
	os.WriteFile(outside, []byte("private"), 0600)
	if e := os.Symlink(outside, filepath.Join(base, "link")); e != nil {
		t.Fatal(e)
	}
	if e := os.Link(outside, filepath.Join(base, "hard")); e != nil {
		t.Fatal(e)
	}
	v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{step("files.open", map[string]any{"path": "link"}, nil, true), step("files.write_file", map[string]any{"path": "hard", "size": int64(4)}, []byte("evil"), false), step("files.open", map[string]any{"path": outside}, nil, true)}}, DefaultLimits()))
	for _, x := range v {
		if callCode(x) != -2 {
			t.Fatal(v)
		}
	}
	raw, _ := os.ReadFile(outside)
	if string(raw) != "private" {
		t.Fatal("outside mutated", raw)
	}
}

func TestWasmBridgeAllocateRawUnsupportedAndQuota(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			r, m, c, base := bridgeSetup(t, on, "read_write")
			path := filepath.Join(base, "a")
			os.WriteFile(path, []byte("abcd"), 0600)
			l := DefaultLimits()
			l.WriteBytes = 3
			v := bridgeRun(t, r, m, c, map[string]any{"scenario": "rawallocate", "path": filesystem.WASIPath(path), "size": 4}, l).(map[string]any)
			if v["allocation"].(int64) == 0 || v["advice"].(int64) != 52 {
				t.Fatal(v)
			}
			raw, _ := os.ReadFile(path)
			if string(raw) != "abcd" {
				t.Fatal("allocation limit mutated", raw)
			}
			v = bridgeRun(t, r, m, c, map[string]any{"scenario": "rawallocate", "path": filesystem.WASIPath(path), "size": 4}, DefaultLimits()).(map[string]any)
			if runtime.GOOS == "linux" {
				if v["allocation"].(int64) != 0 {
					t.Fatal(v)
				}
			} else if v["allocation"].(int64) != 52 {
				t.Fatal("non-native allocate was accepted", v)
			}
			raw, _ = os.ReadFile(path)
			if string(raw) != "abcd" {
				t.Fatal(raw)
			}
		})
	}
}

func TestWasmBridgeHTTPStrictBodyReadyTLSAndCancellation(t *testing.T) {
	r, m, c, _ := bridgeSetup(t, true, "")
	var sent atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { sent.Add(1); b, _ := io.ReadAll(q.Body); w.Write(b) }))
	defer server.Close()
	r.Bridge.Sandbox["permissions"].(map[string]any)["http"] = []any{map[string]any{"origin": server.URL, "methods": []string{"POST"}, "path_prefix": "/allowed", "allowed_cidrs": []string{"127.0.0.0/8"}}}
	open := func(url string, size int64) map[string]any {
		return step("http.open", map[string]any{"method": "POST", "url": url, "headers": map[string]any{}, "content_length": size}, []byte("ab"), false)
	}
	v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{open(server.URL+"/allowed", 2), step("http.do", map[string]any{}, nil, true)}}, DefaultLimits()))
	if callCode(v[0]) < 1 || callCode(v[1]) < 1 || v[1].(map[string]any)["data"] != "YWI=" || sent.Load() != 1 {
		t.Fatal(v, sent.Load())
	}
	v = entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{open(server.URL+"/allowed-more", 2), open(server.URL+"/allowed", 3), step("http.do", map[string]any{}, nil, true)}}, DefaultLimits()))
	if callCode(v[0]) != -2 || callCode(v[2]) >= 0 || sent.Load() != 1 {
		t.Fatal("denied/incomplete body sent", v, sent.Load())
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { sent.Add(1) }))
	defer tlsServer.Close()
	r.Bridge.Sandbox["enabled"] = false
	v = entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{open(tlsServer.URL+"/allowed", 2), step("http.do", map[string]any{}, nil, true)}}, DefaultLimits()))
	if callCode(v[1]) >= 0 || sent.Load() != 1 {
		t.Fatal("untrusted TLS sent", v, sent.Load())
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		io.Copy(io.Discard, q.Body)
		entered <- struct{}{}
		select {
		case <-q.Context().Done():
		case <-release:
		}
	}))
	defer blocked.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		out, e := r.Execute(ctx, m, map[string]any{"scenario": "bridge", "steps": []any{open(blocked.URL+"/allowed", 2), step("http.do", map[string]any{}, nil, true)}}, c, DefaultLimits())
		if out != nil {
			out.Close()
		}
		done <- e
	}()
	select {
	case <-entered:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP never entered")
	}
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("cancel accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker/host cancel blocked")
	}
	if r.Store.Usage() != 0 {
		t.Fatal("HTTP cancellation spool leaked")
	}
}

func TestWasmBridgePreparedEnvironmentBaseAndImmutableSnapshot(t *testing.T) {
	for _, mode := range []string{"off", "denyall", "read", "environment"} {
		t.Run(mode, func(t *testing.T) {
			r, m, c, base := bridgeSetup(t, false, "")
			os.WriteFile(filepath.Join(base, "ok"), []byte("base-owned"), 0600)
			sandbox := "{enabled: false}"
			if mode != "off" {
				sandbox = "{enabled: true, permissions: {filesystem: [], environment: []}}"
			}
			if mode == "read" || mode == "environment" {
				names := "[]"
				if mode == "environment" {
					names = "[WASM_TEST_SECRET]"
				}
				sandbox = "{enabled: true, permissions: {filesystem: [{root: ., access: read}], environment: " + names + "}}"
			}
			cfg, e := contract.Compile([]byte("schema_version: 1\ngateway: {url: ws://localhost/ws/agent, agent_private_key_file: ./key}\ncapabilities:\n  run:\n    executor: {type: wasm, module: ./module.wasm, sandbox: " + sandbox + "}\n    output: {data: {}, files: {}}\n"))
			if e != nil {
				t.Fatal(e)
			}
			env := map[string]string{"PWD": "/ambient-before", "WASM_TEST_SECRET": "snapshot-secret"}
			options := providers.ResolveOptions{ModuleDigests: map[string]string{"run": m.Digest()}, Environment: env}
			snapshot, e := providers.Resolve(context.Background(), cfg, filepath.Join(base, "agent.yaml"), "run", options)
			if e != nil {
				t.Fatal(e)
			}
			env["PWD"] = "/ambient-after"
			same, e := providers.Resolve(context.Background(), cfg, filepath.Join(base, "agent.yaml"), "run", options)
			if e != nil || same.DefinitionHash() != snapshot.DefinitionHash() {
				t.Fatal("ambient PWD changed hash", e)
			}
			env["WASM_TEST_SECRET"] = "changed-after-prepare"
			getter := snapshot.Environment()
			getter["WASM_TEST_SECRET"] = "getter-tamper"
			r.Bridge.Base = snapshot.BaseDirectory()
			r.Bridge.Environment = snapshot.Environment()
			r.Bridge.Sandbox = snapshot.Definition().Executor.Settings["sandbox"].(map[string]any)
			v := bridgeRun(t, r, m, c, map[string]any{"scenario": "rawfs", "path": "ok"}, DefaultLimits()).(map[string]any)
			if v["pwd"] != filesystem.WASIPath(base) {
				t.Fatal(v)
			}
			expected := ""
			if mode == "off" || mode == "environment" {
				expected = "snapshot-secret"
			}
			if v["secret"] != expected {
				t.Fatal("snapshot environment", v)
			}
			if mode == "denyall" {
				if v["body"] != "" || v["error"] == "" {
					t.Fatal(v)
				}
			} else if v["body"] != base64.StdEncoding.EncodeToString([]byte("base-owned")) || v["error"] != "" {
				t.Fatal("relative path used ambient cwd", v)
			}
		})
	}
}

func TestWasmBridgeReadDirInvalidNamesAndWriteBoundaries(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			r, m, c, base := bridgeSetup(t, on, "read_write")
			for _, size := range []int64{3, 4, 5} {
				path := filepath.Join(base, "write")
				os.WriteFile(path, []byte("original"), 0600)
				l := DefaultLimits()
				l.WriteBytes = 4
				l.TotalWriteBytes = 4
				v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{step("files.write_file", map[string]any{"path": "write", "size": size}, bytes.Repeat([]byte{255}, int(size)), false)}}, l))
				raw, _ := os.ReadFile(path)
				if size <= 4 {
					if callCode(v[0]) <= 0 || !bytes.Equal(raw, bytes.Repeat([]byte{255}, int(size))) {
						t.Fatal(size, v, raw)
					}
				} else if callCode(v[0]) != -4 || string(raw) != "original" {
					t.Fatal("overlimit truncated", v, raw)
				}
			}
			l := DefaultLimits()
			l.WriteBytes = 4
			l.TotalWriteBytes = 4
			hole := bridgeRun(t, r, m, c, map[string]any{"scenario": "knownhole", "path": "write"}, l).(map[string]any)
			if hole["write"] != int64(-4) || hole["close"] != int64(0) {
				t.Fatal("known reservation bypassed through seek", hole)
			}
			raw, _ := os.ReadFile(filepath.Join(base, "write"))
			if len(raw) != 0 {
				t.Fatal("unreserved sparse write", len(raw))
			}
			if runtime.GOOS != "windows" {
				invalidName := string([]byte{0xff})
				err := os.WriteFile(filepath.Join(base, invalidName), []byte("x"), 0600)
				if err == nil {
					v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{step("files.read_dir", map[string]any{"path": "."}, nil, true)}}, DefaultLimits()))
					control := v[0].(map[string]any)["control"].(map[string]any)
					if control["error"] != int64(1) {
						t.Fatal("invalid name replaced", v)
					}
				} else if runtime.GOOS != "darwin" {
					t.Fatal("cannot set invalid name fixture", err)
				}
				invalid := bridgeRun(t, r, m, c, map[string]any{"scenario": "rawinvalid"}, DefaultLimits()).(map[string]any)
				if invalid["error"] == "" || invalid["body"] != "" {
					t.Fatal("raw invalid UTF8 path accepted", invalid)
				}
			}
		})
	}
}

func TestWasmBridgeAllFixedHostErrorCodes(t *testing.T) {
	r, m, c, base := bridgeSetup(t, false, "")
	os.WriteFile(filepath.Join(base, "existing"), []byte("unchanged"), 0600)
	r.Bridge.Input = map[string]contract.FileValue{"canceled": {ID: "canceled"}, "io": {ID: "io"}}
	r.Bridge.OpenInput = func(_ context.Context, f contract.FileValue) (io.ReadCloser, error) {
		if f.ID == "canceled" {
			return nil, context.Canceled
		}
		return nil, io.ErrUnexpectedEOF
	}
	l := DefaultLimits()
	l.WriteBytes = 3
	cases := []any{
		step("files.open", map[string]any{"path": "bad\x00"}, nil, false),
		step("input.open", map[string]any{"file_id": "foreign"}, nil, false),
		step("files.open", map[string]any{"path": "missing"}, nil, false),
		step("files.write_file", map[string]any{"path": "existing", "size": int64(4)}, nil, false),
		step("input.open", map[string]any{"file_id": "canceled"}, nil, false),
		step("input.open", map[string]any{"file_id": "io"}, nil, false),
		step("files.unsupported", map[string]any{}, nil, false),
		step("files.wasi_open", map[string]any{"path": "existing", "flags": int64(os.O_WRONLY | os.O_CREATE | os.O_EXCL), "perm": int64(0600)}, nil, false),
	}
	values := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": cases}, l))
	for i, v := range values {
		if callCode(v) != -int64(i+1) {
			t.Fatalf("fixed category %d: %+v", i+1, v)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(base, "existing"))
	if string(raw) != "unchanged" {
		t.Fatal("rejected host operation changed file", raw)
	}
}
