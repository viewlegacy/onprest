//go:build windows

package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemWriteWindowsExclusiveRenameAndReplaceFailsClosed(t *testing.T) {
	dir := t.TempDir()
	d, e := OpenDirectory(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	temp := ".onprest-00000000000000000000000000000000.tmp"
	f, _, e := d.CreateTemp(temp)
	if e != nil {
		t.Fatal(e)
	}
	f.Write([]byte("new"))
	f.Sync()
	f.Close()
	installed, e := publishBelow(d.file, temp, "result", false, nil)
	if !installed || e != nil {
		t.Fatal(installed, e)
	}
	f, _, e = d.CreateTemp(temp)
	if e != nil {
		t.Fatal(e)
	}
	f.Write([]byte("replacement"))
	f.Close()
	old, e := d.Inspect("result", false)
	if e != nil {
		t.Fatal(e)
	}
	installed, e = publishBelow(d.file, temp, "result", true, &old)
	if installed || !errors.Is(e, errors.ErrUnsupported) {
		t.Fatal("unsafe existing replacement", installed, e)
	}
	if b, e := os.ReadFile(filepath.Join(dir, "result")); e != nil || string(b) != "new" {
		t.Fatal("existing data changed")
	}
	for _, name := range []string{"a:stream", "CON", "LPT¹", ".ONPREST-test", "trailing.", "trailing "} {
		if SaveName(name) == nil {
			t.Fatal("unsafe name", name)
		}
	}
}

func TestFilesystemWriteWindowsOneCharacterPublication(t *testing.T) {
	dir := t.TempDir()
	d, e := OpenDirectory(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	temp := ".onprest-00000000000000000000000000000001.tmp"
	f, id, e := d.CreateTemp(temp)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.Write([]byte("x"))
	e = errors.Join(e, f.Close())
	if e != nil {
		t.Fatal(e)
	}
	id.Size = 1
	if installed, e := d.Publish(context.Background(), temp, "a", false, id, nil); !installed || e != nil {
		t.Fatal(installed, e)
	}
	if b, e := os.ReadFile(filepath.Join(dir, "a")); e != nil || string(b) != "x" {
		t.Fatal("published file", e)
	}
	child, e := d.CreatePrivateDirectory(".onprest-run")
	if e != nil {
		t.Fatal(e)
	}
	identity := child.Identity()
	if e = child.Close(); e != nil {
		t.Fatal(e)
	}
	if moved, e := d.PublishDirectory(context.Background(), ".onprest-run", "b", identity); !moved || e != nil {
		t.Fatal(moved, e)
	}
	if info, e := os.Stat(filepath.Join(dir, "b")); e != nil || !info.IsDir() {
		t.Fatal("published directory", e)
	}
}
