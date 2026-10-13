package filesystem

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestObservedCreateAtomicRaceAndBound(t *testing.T) {
	// A deletion between EXCL collision and existing-only open is retried; only
	// the subsequent successful EXCL can establish creation.
	p := filepath.Join(t.TempDir(), "x")
	calls := 0
	f, created, e := observedCreate(p, os.O_WRONLY|os.O_CREATE, func(n string, flags int) (*os.File, error) {
		calls++
		switch calls {
		case 1:
			if flags&os.O_EXCL == 0 {
				t.Fatal(flags)
			}
			return nil, fs.ErrExist
		case 2:
			if flags&os.O_CREATE != 0 {
				t.Fatal(flags)
			}
			return nil, fs.ErrNotExist
		default:
			return os.OpenFile(n, flags, 0600)
		}
	}, false)
	if e != nil || !created || calls != 3 {
		t.Fatal(e, created, calls)
	}
	f.Close()
	calls = 0
	_, created, e = observedCreate(p, os.O_WRONLY|os.O_CREATE, func(_ string, flags int) (*os.File, error) {
		calls++
		if flags&os.O_EXCL != 0 {
			return nil, fs.ErrExist
		}
		return nil, fs.ErrNotExist
	}, false)
	if e == nil || created || calls != 128 {
		t.Fatal(e, created, calls)
	}
}

func TestObservedOrdinaryCreateLinksAndExisting(t *testing.T) {
	base := t.TempDir()
	p := filepath.Join(base, "existing")
	os.WriteFile(p, []byte("same"), 0600)
	f, created, e := OpenOrdinaryObserved(p, os.O_WRONLY|os.O_CREATE, 0600)
	if e != nil || created {
		t.Fatal(created, e)
	}
	f.Close()
	b, _ := os.ReadFile(p)
	if string(b) != "same" {
		t.Fatal(string(b))
	}
	// Relative dangling chains preserve ordinary follow semantics. Native
	// Windows link creation requires operator privileges and is outside this test.
	if e := os.Symlink("chain", filepath.Join(base, "link")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("target", filepath.Join(base, "chain")); e != nil {
		t.Fatal(e)
	}
	f, created, e = OpenOrdinaryObserved(filepath.Join(base, "link"), os.O_WRONLY|os.O_CREATE, 0600)
	if e != nil || !created {
		t.Fatal(created, e)
	}
	f.Close()
	f, created, e = OpenOrdinaryObserved(filepath.Join(base, "link"), os.O_WRONLY|os.O_CREATE, 0600)
	if e != nil || created {
		t.Fatal(created, e)
	}
	f.Close()
	if e := os.Symlink("loop", filepath.Join(base, "loop")); e != nil {
		t.Fatal(e)
	}
	f, created, e = OpenOrdinaryObserved(filepath.Join(base, "loop"), os.O_WRONLY|os.O_CREATE, 0600)
	if e == nil || created || f != nil {
		t.Fatal(created, e)
	}
	// Exclusive collision is not reported as creation, and cannot change bytes.
	f, created, e = OpenOrdinaryObserved(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if !errors.Is(e, fs.ErrExist) || created || f != nil {
		t.Fatal(created, e)
	}
}

func TestObservedCreateRealDisappearanceKeepsDescriptor(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	if e := os.WriteFile(p, []byte("old"), 0600); e != nil {
		t.Fatal(e)
	}
	calls := 0
	f, created, e := observedCreate(p, os.O_WRONLY|os.O_CREATE, func(n string, flags int) (*os.File, error) {
		calls++
		if calls == 2 {
			if e := os.Remove(n); e != nil {
				t.Fatal(e)
			}
		}
		return OpenOrdinary(n, flags, 0600)
	}, false)
	if e != nil || !created || calls != 3 {
		t.Fatal(e, created, calls)
	}
	defer f.Close()
	old := p + ".old"
	if e := os.Rename(p, old); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte("replacement"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := f.Write([]byte("owned")); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(old)
	if e != nil || string(b) != "owned" {
		t.Fatal(e, string(b))
	}
	b, e = os.ReadFile(p)
	if e != nil || string(b) != "replacement" {
		t.Fatal(e, string(b))
	}
}
