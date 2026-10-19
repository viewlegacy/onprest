package wasmruntime

import (
	"bytes"
	"context"
	"encoding/json"
	es "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/contract"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWasmDirectoryDescriptorAndRewind(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, synthetic := range []bool{false, true} {
			t.Run(map[bool]string{false: "off", true: "on"}[on]+map[bool]string{false: "normal", true: "synthetic"}[synthetic], func(t *testing.T) {
				s, base := directSession(t, DefaultLimits())
				s.bridge.Sandbox = map[string]any{"enabled": on}
				if on {
					s.bridge.Sandbox["permissions"] = map[string]any{"filesystem": []any{map[string]any{"root": base, "access": "read"}}}
				}
				dir := filepath.Join(base, "d")
				os.Mkdir(dir, 0700)
				os.WriteFile(filepath.Join(dir, "original"), []byte("x"), 0600)
				prefix, path := base+"/", "d"
				if synthetic {
					prefix = dir + "/"
					path = "."
				}
				fsys := &wasiFS{rpc: s.dispatch, prefix: prefix}
				f, e := fsys.OpenFile(path, es.O_RDONLY, 0)
				if e != 0 {
					t.Fatal(e)
				}
				if synthetic {
					if _, e = f.Stat(); e != 0 {
						t.Fatal(e)
					}
				}
				moved := filepath.Join(base, "old")
				pinned := heldDirectoryPinsPath(on)
				if pinned {
					if os.Rename(dir, moved) == nil {
						t.Fatal("held directory was renamed")
					}
					moved = dir
				} else {
					if e := os.Rename(dir, moved); e != nil {
						t.Fatal(e)
					}
					os.Mkdir(dir, 0700)
					os.WriteFile(filepath.Join(dir, "replacement"), nil, 0600)
				}
				entries, e := f.Readdir(-1)
				if e != 0 || len(entries) != 1 || entries[0].Name != "original" {
					t.Fatal(entries, e)
				}
				os.WriteFile(filepath.Join(moved, "second"), nil, 0600)
				if _, e = f.Seek(0, io.SeekStart); e != 0 {
					t.Fatal(e)
				}
				entries, e = f.Readdir(-1)
				if e != 0 || len(entries) != 2 || entries[0].Name != "original" || entries[1].Name != "second" {
					t.Fatal(entries, e)
				}
				if e = f.Close(); e != 0 {
					t.Fatal(e)
				}
				if _, e = f.Readdir(-1); e != es.EBADF {
					t.Fatal(e)
				}
				if len(s.handles) != 0 {
					t.Fatal(len(s.handles))
				}
				if pinned {
					if e := os.Rename(dir, filepath.Join(base, "old")); e != nil {
						t.Fatal(e)
					}
				}
			})
		}
	}
}

func TestWasmRealDirectoryPathReplacement(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, synthetic := range []bool{false, true} {
			t.Run(map[bool]string{false: "off", true: "on"}[on]+map[bool]string{false: "normal", true: "synthetic"}[synthetic], func(t *testing.T) {
				r, m, c, base := bridgeSetup(t, on, "read")
				dir := filepath.Join(base, "d")
				if !synthetic {
					os.Mkdir(dir, 0700)
				}
				path := filesystem.WASIPath(dir)
				if synthetic {
					dir = base
					path = "."
				}
				os.WriteFile(filepath.Join(dir, "original"), []byte("x"), 0600)
				moved := dir + ".old"
				t.Cleanup(func() { os.RemoveAll(moved) })
				pinned, refused := heldDirectoryPinsPath(on), false
				r.Bridge.Input = map[string]contract.FileValue{"swap": {ID: "swap"}, "rewind": {ID: "rewind"}}
				r.Bridge.OpenInput = func(ctx context.Context, v contract.FileValue) (io.ReadCloser, error) {
					if v.ID == "swap" {
						if e := os.Rename(dir, moved); e != nil {
							if !pinned {
								return nil, e
							}
							refused = true
						} else {
							if e := os.Mkdir(dir, 0700); e != nil {
								return nil, e
							}
							os.WriteFile(filepath.Join(dir, "replacement"), nil, 0600)
						}
					} else if refused {
						os.WriteFile(filepath.Join(dir, "second"), nil, 0600)
					} else {
						os.WriteFile(filepath.Join(moved, "second"), nil, 0600)
					}
					return io.NopCloser(bytes.NewReader(nil)), nil
				}
				out := bridgeRun(t, r, m, c, map[string]any{"scenario": "dir_descriptor", "path": path}, DefaultLimits()).(map[string]any)
				first, last := out["first"].([]any), out["last"].([]any)
				if len(first) != 1 || first[0] != "original" || len(last) != 2 || last[0] != "original" || last[1] != "second" {
					t.Fatal(out)
				}
				if pinned != refused {
					t.Fatal("held directory rename", pinned, refused)
				}
				if refused {
					if e := os.Rename(dir, moved); e != nil {
						t.Fatal(e)
					}
				}
			})
		}
	}
	// Deny-all keeps module initialization and InputFile functional, while actual
	// preopen enumeration obtains no descriptor or metadata without a grant.
	r, m, c, _ := bridgeSetup(t, true, "")
	out := bridgeRun(t, r, m, c, map[string]any{"scenario": "raw_readdir", "path": "."}, DefaultLimits()).(map[string]any)
	if len(out["names"].([]any)) != 0 || out["error"] == "" {
		t.Fatal(out)
	}
}

func TestWasmPositionedBudgetAndOffset(t *testing.T) {
	for _, write := range []bool{false, true} {
		for _, operations := range []int64{1, 2, 3} {
			t.Run(map[bool]string{false: "pread", true: "pwrite"}[write]+string(rune('0'+operations)), func(t *testing.T) {
				l := DefaultLimits()
				l.FileOperations = operations
				s, base := directSession(t, l)
				p := filepath.Join(base, "x")
				os.WriteFile(p, []byte("abcd"), 0600)
				fsys := &wasiFS{rpc: s.dispatch, prefix: base + "/"}
				flags := es.O_RDONLY
				if write {
					flags = es.O_RDWR
				}
				f, e := fsys.OpenFile("x", flags, 0)
				if e != 0 {
					t.Fatal(e)
				}
				b := make([]byte, 2)
				var n int
				if write {
					n, e = f.Pwrite([]byte("XY"), 1)
				} else {
					n, e = f.Pread(b, 1)
				}
				if operations == 1 {
					if e != es.ERANGE || n != 0 {
						t.Fatal(n, e)
					}
				} else {
					if n != 2 || e != 0 {
						t.Fatal(n, e)
					}
					if !write && string(b) != "bc" {
						t.Fatal(string(b))
					}
				}
				if s.operations != min(operations, int64(2)) {
					t.Fatal(s.operations)
				}
				pos, err := s.handles[f.(*wasiFile).id].business.file.Seek(0, io.SeekCurrent)
				if err != nil || pos != 0 {
					t.Fatal(pos, err)
				}
				if e = f.Close(); e != 0 {
					t.Fatal(e)
				}
				body, _ := os.ReadFile(p)
				want := "abcd"
				if write && operations >= 2 {
					want = "aXYd"
				}
				if string(body) != want {
					t.Fatal(string(body))
				}
			})
		}
	}
	for _, write := range []bool{false, true} {
		for _, offset := range []int64{-1, 1<<63 - 1} {
			s, base := directSession(t, DefaultLimits())
			os.WriteFile(filepath.Join(base, "x"), []byte("abcd"), 0600)
			id := directOpenBody(t, s, "x", os.O_RDWR)
			op := "pread"
			if write {
				op = "pwrite"
			}
			out := s.dispatch(packet{Op: op, Handle: id, Offset: offset, Count: 2, Data: []byte("XX")})
			if out.Count != invalid || s.readBytes != 0 || s.writeBytes != 0 || s.operations != 2 {
				t.Fatal(out, s.readBytes, s.writeBytes, s.operations)
			}
			s.close(id)
		}
	}
}

func TestWasmPositionedShortGrowthRefundAndEffects(t *testing.T) {
	l := DefaultLimits()
	l.ReadBytes = 8
	l.TotalReadBytes = 8
	l.WriteBytes = 8
	l.TotalWriteBytes = 8
	s, base := directSession(t, l)
	p := filepath.Join(base, "x")
	os.WriteFile(p, []byte("abcd"), 0600)
	id := directOpenBody(t, s, "x", os.O_RDWR)
	if out := s.dispatch(packet{Op: "pread", Handle: id, Offset: 3, Count: 4}); out.Count != 1 || string(out.Data) != "d" || s.readBytes != 1 {
		t.Fatal(out, s.readBytes)
	}
	if out := s.dispatch(packet{Op: "pread", Handle: id, Offset: 4, Count: 4}); out.Count != 0 || s.readBytes != 1 {
		t.Fatal(out, s.readBytes)
	}
	if out := s.dispatch(packet{Op: "pwrite", Handle: id, Offset: 8, Data: []byte("Z")}); out.Count != 1 || s.writeBytes != 5 {
		t.Fatal(out, s.writeBytes)
	}
	pos, _ := s.handles[id].business.file.Seek(0, io.SeekCurrent)
	if pos != 0 {
		t.Fatal(pos)
	}
	s.close(id)
	if s.bridge.Effects.Outcome() != "committed" {
		t.Fatal(s.bridge.Effects.Outcome())
	}
	body, _ := os.ReadFile(p)
	if !bytes.Equal(body, []byte{'a', 'b', 'c', 'd', 0, 0, 0, 0, 'Z'}) {
		t.Fatal(body)
	}
	// Append WriteAt is invalid before reservation, and sparse growth cannot use
	// a partial reservation. An OS failure refunds all unused reserved bytes.
	s, base = directSession(t, l)
	os.WriteFile(filepath.Join(base, "x"), []byte("abcd"), 0600)
	appendID := directOpenBody(t, s, "x", os.O_WRONLY|os.O_APPEND)
	out := s.dispatch(packet{Op: "pwrite", Handle: appendID, Offset: 0, Data: []byte("X")})
	if out.Count != invalid || s.writeBytes != 0 || s.bridge.Effects.Outcome() == "committed" {
		t.Fatal(out, s.writeBytes)
	}
	s.close(appendID)
	id = directOpenBody(t, s, "x", os.O_RDWR)
	out = s.dispatch(packet{Op: "pwrite", Handle: id, Offset: 20, Data: []byte("Z")})
	if out.Count != limit || s.writeBytes != 0 {
		t.Fatal(out, s.writeBytes)
	}
	s.close(id)
	f, _ := os.Open(filepath.Join(base, "x"))
	b := &business{file: f, writable: true}
	id = s.id(&handle{kind: "fs", business: b})
	out = s.dispatch(packet{Op: "pwrite", Handle: id, Offset: 0, Data: []byte("fail")})
	if out.Count >= 0 || s.writeBytes != 0 || b.write != 0 {
		t.Fatal(out, s.writeBytes, b.write)
	}
	s.close(id)
	if s.bridge.Effects.Outcome() == "committed" {
		t.Fatal("failed WriteAt became committed on close")
	}
	body, _ = os.ReadFile(filepath.Join(base, "x"))
	if string(body) != "abcd" {
		t.Fatal(string(body))
	}
	for _, quota := range []int64{1, 2, 3} {
		s, base = directSession(t, l)
		s.l.WriteBytes = quota
		s.l.TotalWriteBytes = quota
		os.WriteFile(filepath.Join(base, "x"), []byte("abcd"), 0600)
		id = directOpenBody(t, s, "x", os.O_RDWR)
		out = s.dispatch(packet{Op: "pwrite", Handle: id, Offset: 0, Data: []byte("XY")})
		if out.Count != int32(min(quota, int64(2))) || s.writeBytes != int64(out.Count) {
			t.Fatal(quota, out, s.writeBytes)
		}
		s.close(id)
	}
}

func TestWasmReadOnlySyncAndRealPositioned(t *testing.T) {
	s, base := directSession(t, DefaultLimits())
	os.WriteFile(filepath.Join(base, "x"), []byte("abcd"), 0600)
	os.Mkdir(filepath.Join(base, "d"), 0700)
	fsys := &wasiFS{rpc: s.dispatch, prefix: base + "/"}
	for _, path := range []string{"x", "d", "."} {
		f, e := fsys.OpenFile(path, es.O_RDONLY, 0)
		if e != 0 {
			t.Fatal(e)
		}
		if path == "." {
			if _, e = f.Stat(); e != 0 {
				t.Fatal(e)
			}
		}
		native := s.handles[f.(*wasiFile).id].business.file
		nativeErr := native.Sync()
		want := errno(codeFor(nativeErr))
		if nativeErr == nil {
			want = 0
		}
		if e = f.Sync(); e != want {
			t.Fatal(path, e, want)
		}
		if e = f.Datasync(); e != want {
			t.Fatal(path, e, want)
		}
		if e = f.Close(); e != 0 {
			t.Fatal(e)
		}
		if e = f.Sync(); e != es.EBADF {
			t.Fatal(e)
		}
		if e = f.Datasync(); e != es.EBADF {
			t.Fatal(e)
		}
	}
	if s.writeBytes != 0 || s.bridge.Effects.Outcome() == "committed" {
		t.Fatal(s.writeBytes, s.bridge.Effects.Outcome())
	}
	for _, on := range []bool{false, true} {
		r, m, c, base := bridgeSetup(t, on, "read_write")
		path := filepath.Join(base, "x")
		os.WriteFile(path, []byte("abcd"), 0600)
		v := bridgeRun(t, r, m, c, map[string]any{"scenario": "positioned_io", "path": filesystem.WASIPath(path)}, DefaultLimits()).(map[string]any)
		want := nativeReadOnlySync(t, path)
		if v["read"] != "bc" || v["written"] != int64(2) || v["before"] != v["after"] || v["sync"] != want || v["datasync"] != want || v["closed_sync"] != int64(8) {
			t.Fatal(v, want)
		}
		bytes, _ := os.ReadFile(path)
		if string(bytes) != "aXYd" {
			t.Fatal(string(bytes))
		}
	}
}

func TestWasmRealReadOnlySyncDirectories(t *testing.T) {
	for _, on := range []bool{false, true} {
		r, m, c, base := bridgeSetup(t, on, "read")
		os.Mkdir(filepath.Join(base, "d"), 0700)
		for _, path := range []string{filesystem.WASIPath(filepath.Join(base, "d")), "."} {
			nativePath := filepath.Join(base, "d")
			if path == "." {
				nativePath = base
			}
			want := nativeReadOnlySync(t, nativePath)
			v := bridgeRun(t, r, m, c, map[string]any{"scenario": "readonly_sync", "path": path}, DefaultLimits()).(map[string]any)
			if v["sync"] != want || v["datasync"] != want || v["closed_sync"] != int64(8) || v["closed_datasync"] != int64(8) {
				t.Fatal(path, v, want)
			}
			if r.Bridge.Effects.Outcome() == "committed" {
				t.Fatal("sync committed effect")
			}
		}
	}
}

// nativeReadOnlySync is the WASI errno a guest sees for Sync on a read-only
// native descriptor of p.
func nativeReadOnlySync(t *testing.T, p string) int64 {
	t.Helper()
	f, e := os.Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if e = f.Sync(); e != nil {
		return int64(wasiErrno(codeFor(e)))
	}
	return 0
}

// heldDirectoryPinsPath reports whether a held directory descriptor refuses a
// rename of its path: Windows OFF uses ordinary opens without delete sharing.
func heldDirectoryPinsPath(on bool) bool { return !on && runtime.GOOS == "windows" }

func TestWasmPositionedReadQuotaAndFailureRefund(t *testing.T) {
	for _, quota := range []int64{1, 2, 3} {
		l := DefaultLimits()
		l.ReadBytes = quota
		l.TotalReadBytes = quota
		s, base := directSession(t, l)
		os.WriteFile(filepath.Join(base, "x"), []byte("abcd"), 0600)
		id := directOpenBody(t, s, "x", os.O_RDONLY)
		out := s.dispatch(packet{Op: "pread", Handle: id, Offset: 1, Count: 2})
		if out.Count != int32(min(quota, int64(2))) || s.readBytes != int64(out.Count) || string(out.Data) != "bc"[:out.Count] {
			t.Fatal(quota, out, s.readBytes)
		}
		s.close(id)
	}
	s, base := directSession(t, DefaultLimits())
	os.WriteFile(filepath.Join(base, "x"), []byte("abcd"), 0600)
	f, e := os.OpenFile(filepath.Join(base, "x"), os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	b := &business{file: f}
	id := s.id(&handle{kind: "fs", business: b})
	out := s.dispatch(packet{Op: "pread", Handle: id, Count: 2})
	if out.Count >= 0 || s.readBytes != 0 || b.read != 0 {
		t.Fatal(out, s.readBytes, b.read)
	}
	s.close(id)
}

// A descriptor listing stopped part way (here by the operation budget) keeps
// the valid leading entries and reports the error; links are listed, not
// treated as errors.
func TestWasmDescriptorEnumerationPartialError(t *testing.T) {
	l := DefaultLimits()
	l.FileOperations = 3
	s, base := directSession(t, l)
	s.bridge.Sandbox = map[string]any{"enabled": true, "permissions": map[string]any{"filesystem": []any{map[string]any{"root": base, "access": "read"}}}}
	os.Mkdir(filepath.Join(base, "d"), 0700)
	os.WriteFile(filepath.Join(base, "d", "a"), []byte("x"), 0600)
	if e := os.Symlink("a", filepath.Join(base, "d", "z")); e != nil {
		t.Fatal(e)
	}
	id := directOpenBody(t, s, "d", os.O_RDONLY)
	out, c := directCall(t, s, "files.readdir", map[string]any{"body_handle": id})
	var errorCode, metaID int64
	json.Unmarshal(out["error"], &errorCode)
	json.Unmarshal(out["body_handle"], &metaID)
	if c != 0 || errorCode != int64(-limit) || s.operations != 3 {
		t.Fatal(out, c, s.operations)
	}
	raw, n := s.read(metaID, 4096)
	if n <= 0 || !bytes.Contains(raw, []byte(`"name":"a"`)) || bytes.Contains(raw, []byte(`"name":"z"`)) {
		t.Fatal(string(raw), n)
	}
	s.close(metaID)
	s.close(id)
	if len(s.handles) != 0 {
		t.Fatal(len(s.handles))
	}
}
