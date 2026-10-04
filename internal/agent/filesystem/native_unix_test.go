//go:build linux || darwin

package filesystem

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemNativeSpecialFilesAndInvalidNames(t *testing.T) {
	for _, kind := range []string{"fifo", "socket"} {
		t.Run(kind, func(t *testing.T) {
			p, e := os.MkdirTemp("/tmp", "p05-")
			if e != nil {
				t.Fatal(e)
			}
			defer os.RemoveAll(p)
			r, e := OpenRoot(p)
			if e != nil {
				t.Fatal(e)
			}
			defer r.Close()
			target := "special"
			switch kind {
			case "fifo":
				if e := unix.Mkfifo(filepath.Join(p, target), 0600); e != nil {
					t.Fatal(e)
				}
			case "socket":
				fd, e := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
				if e != nil {
					t.Fatal(e)
				}
				defer unix.Close(fd)
				if e := unix.Bind(fd, &unix.SockaddrUnix{Name: filepath.Join(p, target)}); e != nil {
					t.Fatal(e)
				}
			}
			if f, e := r.Open(context.Background(), target); e == nil || f != nil {
				t.Fatal("special open")
			}
			got, e := r.Select(context.Background(), selector(t, "{name_regex: '.*'}", ""), nil)
			if e == nil || got != nil {
				t.Fatal("special selected")
			}
		})
	}
	// An absolute root may be a mount itself; crossings below it are refused.
	r, e := OpenRoot("/")
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if f, e := r.Open(context.Background(), "dev/null"); e == nil || f != nil {
		t.Fatal("device accepted")
	}
}
