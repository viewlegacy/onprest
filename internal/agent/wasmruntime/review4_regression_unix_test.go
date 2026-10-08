//go:build linux || darwin

package wasmruntime

import (
	"context"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/contract"
	"golang.org/x/sys/unix"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func review4Run(t *testing.T, r *Runner, m *Module, c *contract.Capability, scenario string, steps []any) []any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, e := r.Execute(ctx, m, map[string]any{"scenario": scenario, "steps": steps}, c, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	return out.Data.([]any)
}

func TestWasmReview4SpecialMutationHostAndRaw(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, socket := range []bool{false, true} {
			t.Run(map[bool]string{false: "off", true: "on"}[on]+map[bool]string{false: "fifo", true: "socket"}[socket], func(t *testing.T) {
				r, m, c, base := bridgeSetup(t, on, "read_write")
				short, e := os.MkdirTemp("/tmp", "p15-016-")
				if e != nil {
					t.Fatal(e)
				}
				base, e = filepath.EvalSymlinks(short)
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { os.RemoveAll(base) })
				r.Bridge.Base = base
				if on {
					r.Bridge.Sandbox["permissions"].(map[string]any)["filesystem"] = []any{map[string]any{"root": base, "access": "read_write"}}
				}
				r.Bridge.Effects.NoChange()
				node := filepath.Join(base, "node")
				var listener net.Listener
				peer := -1
				if socket {
					var e error
					listener, e = net.Listen("unix", node)
					if e != nil {
						t.Fatal(e)
					}
					defer listener.Close()
				} else {
					if e := unix.Mkfifo(node, 0600); e != nil {
						t.Fatal(e)
					}
					var e error
					peer, e = unix.Open(node, unix.O_RDONLY|unix.O_NONBLOCK, 0)
					if e != nil {
						t.Fatal(e)
					}
					defer unix.Close(peer)
				}
				before, e := os.Lstat(node)
				if e != nil {
					t.Fatal(e)
				}
				source := filepath.Join(base, "source")
				os.WriteFile(source, []byte("same"), 0600)
				steps := []any{step("files.remove", map[string]any{"path": "node"}, nil, false), step("files.rename", map[string]any{"old_path": "node", "new_path": "moved"}, nil, false), step("files.rename", map[string]any{"old_path": "source", "new_path": "node"}, nil, false), step("files.mkdir_all", map[string]any{"path": "node/child", "perm": 0700}, nil, false)}
				for _, v := range review4Run(t, r, m, c, "bridge", steps) {
					if callCode(v) != -2 {
						t.Fatal(v)
					}
				}
				raw := []any{step("files.remove", map[string]any{"path": filesystem.WASIPath(node)}, nil, false), step("files.rename", map[string]any{"old_path": filesystem.WASIPath(node), "new_path": filesystem.WASIPath(filepath.Join(base, "moved"))}, nil, false), step("files.rename", map[string]any{"old_path": filesystem.WASIPath(source), "new_path": filesystem.WASIPath(node)}, nil, false), step("files.mkdir_all", map[string]any{"path": filesystem.WASIPath(filepath.Join(node, "child"))}, nil, false)}
				for _, v := range review4Run(t, r, m, c, "review4_raw", raw) {
					if callCode(v) == 0 {
						t.Fatal("raw special admitted", v)
					}
				}
				after, e := os.Lstat(node)
				if e != nil || before.Mode() != after.Mode() || !os.SameFile(before, after) {
					t.Fatal(before, after, e)
				}
				if _, e = os.Lstat(filepath.Join(base, "moved")); !os.IsNotExist(e) {
					t.Fatal(e)
				}
				b, e := os.ReadFile(source)
				if e != nil || string(b) != "same" {
					t.Fatal(e, string(b))
				}
				if !socket {
					b := make([]byte, 16)
					if n, _ := unix.Read(peer, b); n > 0 {
						t.Fatal("FIFO received data", n)
					}
				}
				if r.Bridge.Effects.Outcome() != "no_change" || r.Store.Usage() != 0 {
					t.Fatal(r.Bridge.Effects.Outcome(), r.Store.Usage())
				}
			})
		}
	}
}

func TestWasmReview4OrdinarySymlinkAndFailureSemantics(t *testing.T) {
	for _, raw := range []bool{false, true} {
		r, m, c, base := bridgeSetup(t, false, "")
		scenario := "bridge"
		if raw {
			scenario = "review4_raw"
		}
		os.WriteFile(filepath.Join(base, "target"), []byte("same"), 0600)
		os.Mkdir(filepath.Join(base, "dir"), 0700)
		os.Symlink("target", filepath.Join(base, "filelink"))
		os.Symlink("missing", filepath.Join(base, "dangling"))
		os.Symlink("dir", filepath.Join(base, "dirlink"))
		os.WriteFile(filepath.Join(base, "replacement"), []byte("replacement"), 0600)
		os.Symlink("target", filepath.Join(base, "destlink"))
		os.WriteFile(filepath.Join(base, "second"), []byte("second"), 0600)
		os.Symlink("missing", filepath.Join(base, "destdangling"))
		unix.Mkfifo(filepath.Join(base, "fifo"), 0600)
		os.Symlink("fifo", filepath.Join(base, "fifolink"))
		path := func(p string) string {
			if raw {
				return filesystem.WASIPath(filepath.Join(base, p))
			}
			return p
		}
		values := review4Run(t, r, m, c, scenario, []any{step("files.rename", map[string]any{"old_path": path("replacement"), "new_path": path("destlink")}, nil, false), step("files.rename", map[string]any{"old_path": path("second"), "new_path": path("destdangling")}, nil, false), step("files.remove", map[string]any{"path": path("fifolink")}, nil, false), step("files.rename", map[string]any{"old_path": path("dangling"), "new_path": path("movedlink")}, nil, false), step("files.remove", map[string]any{"path": path("filelink")}, nil, false), step("files.mkdir_all", map[string]any{"path": path("dirlink/new/sub"), "perm": 0700}, nil, false)})
		for _, v := range values {
			if raw && callCode(v) != 0 || !raw && callCode(v) <= 0 {
				t.Fatal(v)
			}
		}
		if link, e := os.Readlink(filepath.Join(base, "movedlink")); e != nil || link != "missing" {
			t.Fatal(link, e)
		}
		if _, e := os.Lstat(filepath.Join(base, "dangling")); !os.IsNotExist(e) {
			t.Fatal(e)
		}
		if _, e := os.Lstat(filepath.Join(base, "filelink")); !os.IsNotExist(e) {
			t.Fatal(e)
		}
		b, e := os.ReadFile(filepath.Join(base, "target"))
		if e != nil || string(b) != "same" {
			t.Fatal(e, string(b))
		}
		if i, e := os.Stat(filepath.Join(base, "dir/new/sub")); e != nil || !i.IsDir() {
			t.Fatal(i, e)
		}

		for name, want := range map[string]string{"destlink": "replacement", "destdangling": "second"} {
			info, e := os.Lstat(filepath.Join(base, name))
			if e != nil || !info.Mode().IsRegular() {
				t.Fatal(info, e)
			}
			b, e := os.ReadFile(filepath.Join(base, name))
			if e != nil || string(b) != want {
				t.Fatal(e, string(b))
			}
		}
		if i, e := os.Lstat(filepath.Join(base, "fifo")); e != nil || i.Mode()&fs.ModeNamedPipe == 0 {
			t.Fatal(i, e)
		}
		if _, e := os.Lstat(filepath.Join(base, "fifolink")); !os.IsNotExist(e) {
			t.Fatal(e)
		}
		if r.Bridge.Effects.Outcome() != "committed" {
			t.Fatal(r.Bridge.Effects.Outcome())
		}
	}
	for _, on := range []bool{false, true} {
		s, base := reviewSession(t, DefaultLimits())
		s.bridge.Effects.NoChange()
		s.bridge.Sandbox = map[string]any{"enabled": on}
		if on {
			s.bridge.Sandbox["permissions"] = map[string]any{"filesystem": []any{map[string]any{"root": base, "access": "read_write"}}}
		}
		_, code := reviewCall(t, s, "files.remove", map[string]any{"path": "missing"})
		if code != notFound || s.bridge.Effects.Outcome() != "unknown" {
			t.Fatal(code, s.bridge.Effects.Outcome())
		}
		os.Mkdir(filepath.Join(base, "dir"), 0700)
		os.WriteFile(filepath.Join(base, "dir/child"), []byte("same"), 0600)
		_, code = reviewCall(t, s, "files.remove", map[string]any{"path": "dir"})
		if code >= 0 || s.bridge.Effects.Outcome() == "committed" {
			t.Fatal(code, s.bridge.Effects.Outcome())
		}

		os.WriteFile(filepath.Join(base, "source"), []byte("source"), 0600)
		_, code = reviewCall(t, s, "files.rename", map[string]any{"old_path": "source", "new_path": "dir"})
		if code >= 0 || s.bridge.Effects.Outcome() == "committed" {
			t.Fatal(code, s.bridge.Effects.Outcome())
		}
		src, e := os.ReadFile(filepath.Join(base, "source"))
		if e != nil || string(src) != "source" {
			t.Fatal(e, string(src))
		}
		b, e := os.ReadFile(filepath.Join(base, "dir/child"))
		if e != nil || string(b) != "same" {
			t.Fatal(e, string(b))
		}
	}
}

func TestWasmReview4SpecialFailureSingleOperation(t *testing.T) {
	for _, on := range []bool{false, true} {
		l := DefaultLimits()
		l.FileOperations = 1
		s, base := reviewSession(t, l)
		s.bridge.Effects.NoChange()
		s.bridge.Sandbox = map[string]any{"enabled": on}
		if on {
			s.bridge.Sandbox["permissions"] = map[string]any{"filesystem": []any{map[string]any{"root": base, "access": "read_write"}}}
		}
		unix.Mkfifo(filepath.Join(base, "fifo"), 0600)
		_, code := reviewCall(t, s, "files.remove", map[string]any{"path": "fifo"})
		if code != denied || s.operations != 1 || s.readBytes != 0 || s.writeBytes != 0 || len(s.handles) != 0 || s.bridge.Effects.Outcome() != "no_change" {
			t.Fatal(code, s.operations, s.bridge.Effects.Outcome())
		}
		i, e := os.Lstat(filepath.Join(base, "fifo"))
		if e != nil || i.Mode()&fs.ModeNamedPipe == 0 {
			t.Fatal(i, e)
		}
	}
}
