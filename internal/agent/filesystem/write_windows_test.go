//go:build windows

package filesystem

import (
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
