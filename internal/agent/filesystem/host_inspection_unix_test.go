//go:build linux || darwin

package filesystem

import (
	"errors"
	"golang.org/x/sys/unix"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestHostSpecialInspectionNoDeviceMutation(t *testing.T) {
	base, e := os.MkdirTemp("/tmp", "host-inspect-")
	if e != nil {
		t.Fatal(e)
	}
	base, e = filepath.EvalSymlinks(base)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	fifo := filepath.Join(base, "fifo")
	if e := unix.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	sock := filepath.Join(base, "socket")
	listener, e := net.Listen("unix", sock)
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	r, e := OpenRoot(base)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	for _, name := range []string{"fifo", "socket"} {
		path := filepath.Join(base, name)
		for _, e := range []error{InspectOrdinaryMutation(path), InspectOrdinaryDirectory(path), r.HostInspectMutation(name), r.HostInspectDirectory(name)} {
			if !errors.Is(e, fs.ErrPermission) {
				t.Fatal(name, e)
			}
		}
	}
	// This is only read-only type inspection, never remove/rename of a real device.
	before, e := os.Lstat("/dev/null")
	if e != nil {
		t.Fatal(e)
	}
	if e = InspectOrdinaryMutation("/dev/null"); !errors.Is(e, fs.ErrPermission) {
		t.Fatal(e)
	}
	if e = InspectOrdinaryDirectory("/dev/null"); !errors.Is(e, fs.ErrPermission) {
		t.Fatal(e)
	}
	dev, e := OpenRoot("/dev")
	if e != nil {
		t.Fatal(e)
	}
	defer dev.Close()
	if e = dev.HostInspectMutation("null"); !errors.Is(e, fs.ErrPermission) {
		t.Fatal(e)
	}
	if e = dev.HostInspectDirectory("null"); !errors.Is(e, fs.ErrPermission) {
		t.Fatal(e)
	}
	after, e := os.Lstat("/dev/null")
	if e != nil || before.Mode() != after.Mode() || !os.SameFile(before, after) {
		t.Fatal(before, after, e)
	}
	link := filepath.Join(base, "dangling")
	os.Symlink("missing", link)
	if e = InspectOrdinaryMutation(link); e != nil {
		t.Fatal(e)
	}
	if e = r.HostInspectMutation("dangling"); !errors.Is(e, fs.ErrPermission) {
		t.Fatal(e)
	}
	os.Mkdir(filepath.Join(base, "dir"), 0700)
	os.Symlink("dir", filepath.Join(base, "dirlink"))
	if e = InspectOrdinaryDirectory(filepath.Join(base, "dirlink")); e != nil {
		t.Fatal(e)
	}
	if e = r.HostInspectDirectory("dirlink"); !errors.Is(e, fs.ErrPermission) {
		t.Fatal(e)
	}
}
