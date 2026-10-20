//go:build linux || darwin || windows

package filesystem

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestFilesystemNativeLinksRootAndIntermediateRefused(t *testing.T) {
	for _, kind := range []string{"root_link", "inside_link", "outside_link", "intermediate_link", "hardlink_inside", "hardlink_outside"} {
		t.Run(kind, func(t *testing.T) {
			r, p := root(t)
			put(t, p, "safe", []byte("inside"))
			outside := t.TempDir()
			put(t, outside, "secret", []byte("outside-secret"))
			var target string
			switch kind {
			case "root_link":
				target = p + "-link"
				if e := os.Symlink(p, target); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { _ = os.Remove(target) })
				candidate, e := OpenRoot(target)
				if e == nil || candidate != nil {
					t.Fatal("root symlink accepted")
				}
				return
			case "inside_link":
				target = "link"
				if e := os.Symlink(filepath.Join(p, "safe"), filepath.Join(p, target)); e != nil {
					t.Fatal(e)
				}
			case "outside_link":
				target = "link"
				if e := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(p, target)); e != nil {
					t.Fatal(e)
				}
			case "intermediate_link":
				target = "sub/secret"
				if e := os.Symlink(outside, filepath.Join(p, "sub")); e != nil {
					t.Fatal(e)
				}
			case "hardlink_inside":
				target = "alias"
				if e := os.Link(filepath.Join(p, "safe"), filepath.Join(p, target)); e != nil {
					t.Fatal(e)
				}
			case "hardlink_outside":
				target = "alias"
				if e := os.Link(filepath.Join(outside, "secret"), filepath.Join(p, target)); e != nil {
					t.Fatal(e)
				}
			}
			f, e := r.Open(context.Background(), target)
			if f != nil || e == nil {
				t.Fatal("unsafe descriptor accepted")
			}
			code(t, e, "PERMISSION_DENIED")
			selected, e := r.Select(context.Background(), selector(t, "{name_regex: '.*', recursive: true, take: 10}", ""), nil)
			if selected != nil || e == nil {
				t.Fatal("unsafe candidate accepted")
			}
			code(t, e, "PERMISSION_DENIED")
			b, e := os.ReadFile(filepath.Join(outside, "secret"))
			if e != nil || string(b) != "outside-secret" {
				t.Fatalf("outside mutation: %q %v", b, e)
			}
		})
	}
}
func TestFilesystemNativeDirectoryAndSymlinkReplacementDuringSnapshot(t *testing.T) {
	r, p := root(t)
	put(t, p, "sub/data", []byte("inside"))
	outside := t.TempDir()
	put(t, outside, "data", []byte("outside-secret"))
	selected := choose(t, r, selector(t, "{path: sub, name: data}", ""))
	replace := func() error {
		if e := os.Rename(filepath.Join(p, "sub"), filepath.Join(p, "old")); e != nil {
			return e
		}
		return os.Symlink(outside, filepath.Join(p, "sub"))
	}
	store := stager(t)
	if runtime.GOOS == "windows" {
		// NTFS refuses to rename a directory while a descendant is open.
		var refused error
		store.onBegin = func(int) { refused = os.Rename(filepath.Join(p, "sub"), filepath.Join(p, "old")) }
		snap, e := SnapshotFiles(context.Background(), selected, SnapshotLimits{32, 32, 1}, "application/octet-stream", store)
		if !errors.Is(refused, fs.ErrPermission) {
			t.Fatal("open descendant allowed directory rename", refused)
		}
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(snap.Files[0].Reader)
		if e != nil || string(b) != "inside" {
			t.Fatalf("%q %v", b, e)
		}
		if e := snap.Close(); e != nil {
			t.Fatal(e)
		}
		assertClean(t, store)
		if e := replace(); e != nil {
			t.Fatal(e)
		}
		store = stager(t)
		snap, e = SnapshotFiles(context.Background(), selected, SnapshotLimits{32, 32, 1}, "application/octet-stream", store)
		code(t, e, "SOURCE_CHANGED")
		if snap != nil {
			t.Fatal("snapshot escaped root")
		}
		assertClean(t, store)
		return
	}
	store.onBegin = func(int) {
		if e := replace(); e != nil {
			t.Fatal(e)
		}
	}
	snap, e := SnapshotFiles(context.Background(), selected, SnapshotLimits{32, 32, 1}, "application/octet-stream", store)
	code(t, e, "SOURCE_CHANGED")
	if snap != nil {
		t.Fatal("snapshot escaped root")
	}
	assertClean(t, store)
}
func TestFilesystemNativeAtomicProducerAndReplacementRace(t *testing.T) {
	r, p := root(t)
	put(t, p, "entry", []byte("inside"))
	outside := t.TempDir()
	put(t, outside, "secret", []byte("outside-secret"))
	// Verify that this OS/fixture can create links instead of silently skipping
	// the security case when permissions are missing.
	link := filepath.Join(p, ".onprest-link")
	if e := os.Symlink(filepath.Join(outside, "secret"), link); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(link); e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	stop := make(chan struct{})
	actorError := make(chan error, 1)
	var swaps atomic.Int64
	stopped := errors.New("stopped")
	replace := func(from string) error {
		for {
			e := os.Rename(from, filepath.Join(p, "entry"))
			// Windows refuses to replace a name while a reader holds it open.
			if e == nil || runtime.GOOS != "windows" || !errors.Is(e, fs.ErrPermission) {
				return e
			}
			select {
			case <-stop:
				return stopped
			default:
			}
		}
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if e := os.Symlink(filepath.Join(outside, "secret"), link); e != nil {
				actorError <- e
				return
			}
			if e := replace(link); e != nil {
				if e != stopped {
					actorError <- e
				}
				return
			}
			swaps.Add(1)
			if e := os.WriteFile(filepath.Join(p, ".onprest-complete"), []byte("inside"), 0600); e != nil {
				actorError <- e
				return
			}
			if e := replace(filepath.Join(p, ".onprest-complete")); e != nil {
				if e != stopped {
					actorError <- e
				}
				return
			}
		}
	}()
	for i := 0; i < 200 || swaps.Load() == 0; i++ {
		select {
		case <-done:
			t.Fatal(<-actorError)
		default:
		}
		f, e := r.Open(context.Background(), "entry")
		if e != nil {
			continue
		}
		b, _ := io.ReadAll(f)
		_ = f.Close()
		if string(b) != "inside" {
			close(stop)
			<-done
			t.Fatalf("race read unauthorized bytes %q", b)
		}
	}
	close(stop)
	<-done
	select {
	case e := <-actorError:
		t.Fatal(e)
	default:
	}
	// A completed atomic publication is readable with no temporary prefix.
	put(t, p, ".onprest-producer", []byte("complete"))
	if e := os.Rename(filepath.Join(p, ".onprest-producer"), filepath.Join(p, "entry")); e != nil {
		t.Fatal(e)
	}
	f, e := r.Open(context.Background(), "entry")
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(f)
	if e != nil || string(b) != "complete" {
		t.Fatalf("%q %v", b, e)
	}
	if e := f.Close(); e != nil {
		t.Fatal(e)
	}
}
