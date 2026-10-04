//go:build linux

package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemNativeLinuxMountBoundary(t *testing.T) {
	// procfs is a real crossing reachable without privileged mount operations;
	// the kernel NO_XDEV resolution rejects it even when the leaf is regular.
	r, e := OpenRoot("/")
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	f, e := r.Open(context.Background(), "proc/version")
	if f != nil || e == nil {
		t.Fatal("mount boundary accepted")
	}
	code(t, e, "PERMISSION_DENIED")
}

func TestFilesystemNativeLinuxInvalidDirectoryName(t *testing.T) {
	r, p := root(t)
	name := string([]byte{0xff})
	if e := os.WriteFile(filepath.Join(p, name), []byte("secret"), 0600); e != nil {
		t.Fatal(e)
	}
	selected, e := r.Select(context.Background(), selector(t, "{name_regex: '.*'}", ""), nil)
	if selected != nil || e == nil {
		t.Fatal("invalid directory name exposed")
	}
	code(t, e, "PERMISSION_DENIED")
}
