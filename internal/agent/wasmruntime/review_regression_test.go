package wasmruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	es "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/contract"
)

func reviewSession(t *testing.T, l Limits) (*session, string) {
	t.Helper()
	r, _, _, base := bridgeSetup(t, false, "")
	s := newSession(context.Background(), r.Store, l, &diagnostics{})
	s.bridge = r.Bridge
	s.roots = map[string]*filesystem.Root{}
	t.Cleanup(func() {
		if e := s.cleanup(); e != nil {
			t.Error(e)
		}
	})
	return s, base
}
func reviewCall(t *testing.T, s *session, method string, args map[string]any) (map[string]json.RawMessage, int32) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"method": method, "args": args})
	id, c := s.call(b)
	if c != 0 {
		return nil, c
	}
	var out bytes.Buffer
	for {
		b, n := s.read(id, Chunk)
		if n < 0 {
			t.Fatal(n)
		}
		out.Write(b)
		if n == 0 {
			break
		}
	}
	if n := s.close(id); n != 0 {
		t.Fatal(n)
	}
	v := map[string]json.RawMessage{}
	if e := json.Unmarshal(out.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v, 0
}
func reviewBody(t *testing.T, s *session, path string, flags int) int64 {
	t.Helper()
	v, c := reviewCall(t, s, "files.wasi_open", map[string]any{"path": path, "flags": flags})
	if c != 0 {
		t.Fatal(c)
	}
	var id int64
	json.Unmarshal(v["body_handle"], &id)
	return id
}
func TestWasmReviewReadDirEntryInfoBudget(t *testing.T) {
	for _, on := range []bool{false, true} {
		for count := 0; count <= 2; count++ {
			t.Run(string(rune('0'+count))+map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
				r, m, c, base := bridgeSetup(t, on, "read_write")
				for i := 0; i < count; i++ {
					os.WriteFile(filepath.Join(base, string(rune('a'+i))), []byte("x"), 0600)
				}
				for _, delta := range []int64{-1, 0, 1} {
					l := DefaultLimits()
					l.FileOperations = int64(1+count) + delta
					if l.FileOperations < 1 {
						continue
					}
					row := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{step("files.read_dir", map[string]any{"path": "."}, nil, true)}}, l))[0].(map[string]any)
					code := row["control"].(map[string]any)["error"].(int64)
					body, _ := base64.StdEncoding.DecodeString(row["data"].(string))
					want := count
					if delta < 0 {
						want--
					}
					if len(bytes.Split(bytes.TrimSpace(body), []byte("\n"))) != max(1, want) || (len(body) == 0) != (want == 0) || (code == 4) != (delta < 0) {
						t.Fatal(delta, row, string(body))
					}
					// Raw Go descriptor stat and deferred preopen acquisition are separate
					// operations from ReadDir and each Info.
					l.FileOperations += 2
					if count == 0 {
						l.FileOperations++
					}
					raw := bridgeRun(t, r, m, c, map[string]any{"scenario": "review_rawdir", "path": filesystem.WASIPath(base)}, l).(map[string]any)
					rawWant := count
					if delta < 0 {
						rawWant = 0
					}
					if len(raw["names"].([]any)) != rawWant || (raw["error"] != "") != (delta < 0) {
						t.Fatal(delta, raw)
					}
				}
			})
		}
	}
	// Failed entry inspection also consumes exactly one operation.
	s, base := reviewSession(t, DefaultLimits())
	s.bridge.Sandbox = map[string]any{"enabled": true, "permissions": map[string]any{"filesystem": []any{map[string]any{"root": base, "access": "read"}}}}
	if e := os.Symlink("missing", filepath.Join(base, "entry")); e != nil {
		t.Fatal(e)
	}
	v, c := reviewCall(t, s, "files.read_dir", map[string]any{"path": "."})
	if c != 0 {
		t.Fatal(c)
	}
	var code int32
	json.Unmarshal(v["error"], &code)
	if code != 2 || s.operations != 2 {
		t.Fatal(code, s.operations)
	}
}
func TestWasmReviewTruncateRefundAndBoundaries(t *testing.T) {
	l := DefaultLimits()
	l.WriteBytes = 4
	l.TotalWriteBytes = 4
	s, base := reviewSession(t, l)
	p := filepath.Join(base, "x")
	os.WriteFile(p, []byte("x"), 0600)
	readonly, e := os.Open(p)
	if e != nil {
		t.Fatal(e)
	}
	bad := &business{file: readonly, writable: true, size: 1}
	id := s.id(&handle{kind: "fs", business: bad})
	_, code := reviewCall(t, s, "files.truncate", map[string]any{"body_handle": id, "size": int64(4)})
	if code == 0 || s.writeBytes != 0 || bad.write != 0 || s.bridge.Effects.Outcome() == "committed" {
		t.Fatal(code, s.writeBytes, bad.write)
	}
	body, _ := os.ReadFile(p)
	if string(body) != "x" {
		t.Fatal(string(body))
	}
	s.close(id)
	good := reviewBody(t, s, "x", os.O_RDWR)
	for _, size := range []int64{6, 5, 4} {
		_, code = reviewCall(t, s, "files.truncate", map[string]any{"body_handle": good, "size": size})
		if (code == limit) != (size == 6) {
			t.Fatal(size, code)
		}
	}
	if s.writeBytes != 4 || s.handles[good].business.write != 4 {
		t.Fatal(s.writeBytes)
	}
	// A failed growth reservation has not starved the next normal write.
	s.close(good)
	l = DefaultLimits()
	l.WriteBytes = 4
	l.TotalWriteBytes = 4
	s, base = reviewSession(t, l)
	os.WriteFile(filepath.Join(base, "x"), []byte("x"), 0600)
	f, _ := os.Open(filepath.Join(base, "x"))
	bad = &business{file: f, writable: true}
	id = s.id(&handle{kind: "fs", business: bad})
	reviewCall(t, s, "files.truncate", map[string]any{"body_handle": id, "size": int64(4)})
	s.close(id)
	good = reviewBody(t, s, "out", os.O_WRONLY|os.O_CREATE)
	if n := s.write(good, []byte("abcd")); n != 4 {
		t.Fatal(n)
	}
	s.close(good)
}
func TestWasmReviewEOFCurrentSizeAndSeekABI(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			r, m, c, base := bridgeSetup(t, on, "read_write")
			path := filepath.Join(base, "x")
			for _, size := range []int{0, 1, 2} {
				os.WriteFile(path, []byte("a"), 0600)
				l := DefaultLimits()
				l.ReadBytes = 1
				l.TotalReadBytes = 1
				v := bridgeRun(t, r, m, c, map[string]any{"scenario": "review_eof", "path": "x", "size": size}, l).(map[string]any)
				want := int64(0)
				if size > 1 {
					want = -4
				}
				if v["first"] != int64(1) || v["next"] != want {
					t.Fatal(size, v)
				}
				os.WriteFile(path, []byte("a"), 0600)
				raw := bridgeRun(t, r, m, c, map[string]any{"scenario": "review_raweof", "path": filesystem.WASIPath(path), "size": size}, l).(map[string]any)
				if raw["first"] != int64(1) || raw["next"] != int64(0) || raw["error"] != (size > 1) {
					t.Fatal(size, raw)
				}
			}
			os.WriteFile(path, []byte("abcd"), 0600)
			v := bridgeRun(t, r, m, c, map[string]any{"scenario": "review_seek", "path": "x"}, DefaultLimits()).(map[string]any)
			for i, n := range v["abi"].([]any) {
				want := int64(0)
				if i < 3 {
					want = -1
				}
				if n != want {
					t.Fatal(v)
				}
			}
			for i, n := range v["raw"].([]any) {
				want := int64(0)
				if i < 2 {
					want = 28
				}
				if n != want {
					t.Fatal(v)
				}
			}
			body, _ := os.ReadFile(path)
			if string(body) != "abcd" {
				t.Fatal(string(body))
			}
		})
	}
	for _, quota := range []int64{0, 1} {
		for _, size := range []int64{0, 1, 2, 5} {
			l := DefaultLimits()
			l.ReadBytes = quota
			l.TotalReadBytes = quota
			s, base := reviewSession(t, l)
			p := filepath.Join(base, "x")
			os.WriteFile(p, bytes.Repeat([]byte("a"), int(quota)), 0600)
			id := reviewBody(t, s, "x", os.O_RDONLY)
			if quota > 0 {
				s.read(id, 1)
			}
			os.Truncate(p, size)
			_, code := s.read(id, 1)
			if (code == 0) != (size <= quota) {
				t.Fatal(quota, size, code)
			}
			s.close(id)
		}
	}
	if codeFor(&os.PathError{Op: "seek", Err: syscall.EINVAL}) != invalid {
		t.Fatal("wrapped invalid")
	}
}
func TestWasmReviewHTTPAbortOwnsAllHandles(t *testing.T) {
	var sent atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Write([]byte("response"))
	}))
	defer server.Close()
	for _, mode := range []string{"unclosed", "readerfail", "response"} {
		t.Run(mode, func(t *testing.T) {
			r, m, c, _ := bridgeSetup(t, false, "")
			size := 0
			if mode == "readerfail" {
				size = 4
			}
			before := sent.Load()
			v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "review_httpabort", "url": server.URL, "size": size, "envelope": mode}, DefaultLimits()))
			for _, n := range v {
				if n != int64(-1) {
					t.Fatal(v)
				}
			}
			want := int32(0)
			if mode == "response" {
				want = 1
			}
			if sent.Load()-before != want {
				t.Fatal(sent.Load(), before)
			}
			if mode == "response" && r.Bridge.Effects.Outcome() != "unknown" {
				t.Fatal(r.Bridge.Effects.Outcome())
			}
		})
	}
	s, _ := reviewSession(t, DefaultLimits())
	v, code := reviewCall(t, s, "http.open", map[string]any{"method": "POST", "url": server.URL, "headers": map[string]any{}, "content_length": 0})
	if code != 0 {
		t.Fatal(code)
	}
	var id int64
	json.Unmarshal(v["request_handle"], &id)
	reviewCall(t, s, "http.abort", map[string]any{"request_handle": id})
	if len(s.handles) != 0 {
		t.Fatal(len(s.handles))
	}
	if e := s.cleanup(); e != nil {
		t.Fatal(e)
	}
	if e := s.cleanup(); e != nil {
		t.Fatal(e)
	}
}
func TestWasmReviewCommitAckRequiredForPublication(t *testing.T) {
	for handles := 1; handles <= 5; handles++ {
		t.Run(string(rune('0'+handles)), func(t *testing.T) {
			r, m, c, _ := bridgeSetup(t, false, "")
			l := DefaultLimits()
			l.Handles = handles
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			count := 0
			_, e := r.ExecutePublication(ctx, m, map[string]any{"scenario": "review_commit", "size": max(0, handles-1)}, c, l, func(v contract.ProviderResult) (*contract.PreparedOutput, error) {
				count++
				return contract.ProcessOutput(context.Background(), c, v, r.Store.Scratch())
			})
			if e == nil || count != 0 {
				t.Fatal(e, count)
			}
			if handles >= 3 {
				out, e := r.ExecutePublication(ctx, m, map[string]any{"scenario": "review_commit", "size": handles - 2}, c, l, func(v contract.ProviderResult) (*contract.PreparedOutput, error) {
					count++
					return contract.ProcessOutput(context.Background(), c, v, r.Store.Scratch())
				})
				if e != nil || count != 1 {
					t.Fatal(e, count)
				}
				out.Close()
			}
		})
	}
}
func TestWasmReviewRawCloseLifetime(t *testing.T) {
	s, base := reviewSession(t, DefaultLimits())
	os.Mkdir(filepath.Join(base, "d"), 0700)
	os.WriteFile(filepath.Join(base, "d", "x"), []byte("x"), 0600)
	os.WriteFile(filepath.Join(base, "x"), []byte("x"), 0600)
	fsys := &wasiFS{rpc: s.dispatch, prefix: base + "/"}
	closedFiles := []es.File{}
	for _, path := range []string{"x", "d", "."} {
		f, e := fsys.OpenFile(path, es.O_RDONLY, 0)
		if e != 0 {
			t.Fatal(e)
		}
		if e = f.Close(); e != 0 {
			t.Fatal(e)
		}
		closedFiles = append(closedFiles, f)
		if _, e = f.Read(make([]byte, 1)); e != es.EBADF {
			t.Fatal(path, e)
		}
		if _, e = f.Write([]byte("x")); e != es.EBADF {
			t.Fatal(e)
		}
		if _, e = f.Seek(0, 0); e != es.EBADF {
			t.Fatal(e)
		}
		if _, e = f.Stat(); e != es.EBADF {
			t.Fatal(e)
		}
		if _, e = f.Readdir(-1); e != es.EBADF {
			t.Fatal(e)
		}
		if e = f.Close(); e != es.EBADF {
			t.Fatal(e)
		}
	}
	os.Rename(filepath.Join(base, "d"), filepath.Join(base, "old"))
	os.Mkdir(filepath.Join(base, "d"), 0700)
	os.WriteFile(filepath.Join(base, "d", "replacement"), nil, 0600)
	for _, f := range closedFiles {
		if _, e := f.Readdir(-1); e != es.EBADF {
			t.Fatal(e)
		}
		if _, e := f.Pread(make([]byte, 1), 0); e != es.EBADF {
			t.Fatal(e)
		}
		if _, e := f.Pwrite([]byte("x"), 0); e != es.EBADF {
			t.Fatal(e)
		}
		if e := f.Truncate(0); e != es.EBADF {
			t.Fatal(e)
		}
		if e := f.Sync(); e != es.EBADF {
			t.Fatal(e)
		}
		if _, e := f.IsDir(); e != es.EBADF {
			t.Fatal(e)
		}
	}
	if len(s.handles) != 0 {
		t.Fatal(len(s.handles))
	}
	r, m, c, _ := bridgeSetup(t, false, "")
	for _, path := range []string{base, filepath.Join(base, "x"), "."} {
		v := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "review_rawclose", "path": filesystem.WASIPath(path)}, DefaultLimits()))
		for _, n := range v {
			if n != int64(8) {
				t.Fatal(v)
			}
		}
	}
}

func TestWasmReviewCommitAckStateAndReadClose(t *testing.T) {
	for _, full := range []bool{false, true} {
		l := DefaultLimits()
		l.Handles = 3
		s, _ := reviewSession(t, l)
		v, code := reviewCall(t, s, "result.open", nil)
		if code != 0 {
			t.Fatal(code)
		}
		var marker, body int64
		json.Unmarshal(v["result_handle"], &marker)
		json.Unmarshal(v["body_handle"], &body)
		if n := s.write(body, []byte(`{"data":"ack-owned","files":[]}`)); n < 1 {
			t.Fatal(n)
		}
		if c := s.close(body); c != 0 {
			t.Fatal(c)
		}
		one, c := s.control(map[string]any{})
		if c != 0 {
			t.Fatal(c)
		}
		if full {
			if _, c = s.control(map[string]any{}); c != 0 {
				t.Fatal(c)
			}
		}
		_, code = reviewCall(t, s, "result.commit", map[string]any{"result_handle": marker})
		if full {
			if code != limit || s.committed || !s.failed || s.value.Data != nil {
				t.Fatal(code, s.committed, s.failed, s.value)
			}
		} else {
			if code != 0 || !s.committed || s.value.Data != "ack-owned" {
				t.Fatal(code, s.committed, s.value)
			}
		}
		s.close(one)
	}
}
