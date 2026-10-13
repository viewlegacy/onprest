package wasmruntime

import (
	"context"
	"errors"
	es "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWasmAtomicCreateObservedEffects(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			exists bool
			flags  int
			write  bool
			want   string
			body   string
		}{
			{"existing", true, os.O_WRONLY | os.O_CREATE, false, "no_change", "same"},
			{"newempty", false, os.O_WRONLY | os.O_CREATE, false, "committed", ""},
			{"truncate", true, os.O_WRONLY | os.O_CREATE | os.O_TRUNC, false, "committed", ""},
			{"write", true, os.O_WRONLY | os.O_CREATE, true, "committed", "Xame"},
			{"exclusivefailure", true, os.O_WRONLY | os.O_CREATE | os.O_EXCL, false, "unknown", "same"},
		} {
			t.Run(tc.name+map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
				s, base := directSession(t, DefaultLimits())
				s.bridge.Sandbox = map[string]any{"enabled": on}
				if on {
					s.bridge.Sandbox["permissions"] = map[string]any{"filesystem": []any{map[string]any{"root": base, "access": "read_write"}}}
				}
				s.bridge.Effects.Start()
				p := filepath.Join(base, "x")
				if tc.exists {
					os.WriteFile(p, []byte("same"), 0600)
				}
				fsys := &wasiFS{rpc: s.dispatch, prefix: base + "/"}
				flags := es.O_WRONLY | es.O_CREAT
				if tc.flags&os.O_TRUNC != 0 {
					flags |= es.O_TRUNC
				}
				if tc.flags&os.O_EXCL != 0 {
					flags |= es.O_EXCL
				}
				f, e := fsys.OpenFile("x", flags, 0600)
				if tc.name == "exclusivefailure" {
					if e != es.EEXIST {
						t.Fatal(e)
					}
				} else {
					if e != 0 {
						t.Fatal(e)
					}
					if tc.write {
						if n, e := f.Write([]byte("X")); n != 1 || e != 0 {
							t.Fatal(n, e)
						}
					}
					if e := f.Close(); e != 0 {
						t.Fatal(e)
					}
				}
				s.bridge.Effects.NoChange()
				if got := s.bridge.Effects.Outcome(); got != tc.want {
					t.Fatal(got, tc.want)
				}
				b, err := os.ReadFile(p)
				if err != nil || string(b) != tc.body {
					t.Fatal(err, string(b))
				}
				if s.writeBytes != map[bool]int64{false: 0, true: 1}[tc.write] {
					t.Fatal(s.writeBytes)
				}
			})
		}
	}
}

func TestWasmCreateCloseFailureRemainsUnknown(t *testing.T) {
	s, base := directSession(t, DefaultLimits())
	s.bridge.Effects.Start()
	id := directOpenBody(t, s, "new", os.O_WRONLY|os.O_CREATE)
	if e := s.handles[id].business.file.Close(); e != nil {
		t.Fatal(e)
	}
	if c := s.close(id); c >= 0 {
		t.Fatal(c)
	}
	s.bridge.Effects.NoChange()
	if s.bridge.Effects.Outcome() != "unknown" {
		t.Fatal(s.bridge.Effects.Outcome())
	}
	if i, e := os.Stat(filepath.Join(base, "new")); e != nil || i.Size() != 0 {
		t.Fatal(i, e)
	}
}

func TestWasmRealAtomicCreateTerminals(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, tc := range []struct{ name, outcome string }{
			{"existing", "no_change"}, {"new", "committed"}, {"truncate", "committed"}, {"write", "committed"}, {"failed", "unknown"},
		} {
			t.Run(tc.name+map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
				r, m, c, base := bridgeSetup(t, on, "read_write")
				p := filepath.Join(base, "x")
				if tc.name != "new" {
					if e := os.WriteFile(p, []byte("same"), 0600); e != nil {
						t.Fatal(e)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				out, e := r.Execute(ctx, m, map[string]any{"scenario": "envelope_create_" + tc.name, "path": filesystem.WASIPath(p), "envelope": `{"error":{"kind":"public","code":"APP_CREATE","message":"observed"}}`}, c, DefaultLimits())
				if out != nil {
					out.Close()
					t.Fatal("unexpected success")
				}
				var terminal *Error
				if !errors.As(e, &terminal) || terminal.Code != "APP_CREATE" || terminal.Message != "observed" {
					t.Fatal(e)
				}
				r.Bridge.Effects.NoChange()
				if r.Bridge.Effects.Outcome() != tc.outcome {
					t.Fatal(r.Bridge.Effects.Outcome(), tc.outcome)
				}
				if r.Store.Usage() != 0 {
					t.Fatal(r.Store.Usage())
				}
			})
		}
	}
	// OFF follows a dangling link chain through atomic creation; ON continues
	// rejecting links rather than translating them into another authorized path.
	for _, on := range []bool{false, true} {
		r, m, c, base := bridgeSetup(t, on, "read_write")
		os.Symlink("chain", filepath.Join(base, "link"))
		os.Symlink("target", filepath.Join(base, "chain"))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, e := r.Execute(ctx, m, map[string]any{"scenario": "envelope_create_new", "path": filesystem.WASIPath(filepath.Join(base, "link")), "envelope": `{"error":{"kind":"public","code":"APP_CREATE","message":"observed"}}`}, c, DefaultLimits())
		if out != nil {
			out.Close()
			t.Fatal("unexpected success")
		}
		if on {
			if e == nil {
				t.Fatal("expected denied link")
			}
			if _, e := os.Stat(filepath.Join(base, "target")); !os.IsNotExist(e) {
				t.Fatal(e)
			}
			if r.Bridge.Effects.Outcome() == "committed" {
				t.Fatal("denied link committed")
			}
		} else {
			var terminal *Error
			if !errors.As(e, &terminal) || terminal.Code != "APP_CREATE" {
				t.Fatal(e)
			}
			if i, e := os.Stat(filepath.Join(base, "target")); e != nil || i.Size() != 0 {
				t.Fatal(i, e)
			}
			if r.Bridge.Effects.Outcome() != "committed" {
				t.Fatal(r.Bridge.Effects.Outcome())
			}
		}
	}
}
