//go:build linux || darwin

package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestArtifactsNativePrivateLinksAndSpecialFiles(t *testing.T) {
	for _, kind := range []string{"public_directory", "root_link", "objects_link", "lock_link", "public_file", "hardlink", "symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			o := options(t, 16)
			if kind == "public_directory" {
				if e := os.Mkdir(o.Directory, 0755); e != nil {
					t.Fatal(e)
				}
				_, e := Open(o)
				if e == nil {
					t.Fatal("public directory accepted")
				}
				return
			}
			if kind == "root_link" {
				if e := os.Symlink(t.TempDir(), o.Directory); e != nil {
					t.Fatal(e)
				}
				_, e := Open(o)
				if e == nil {
					t.Fatal("root link accepted")
				}
				return
			}
			s, e := Open(o)
			if e != nil {
				t.Fatal(e)
			}
			_ = s.Close()
			objects := filepath.Join(o.Directory, "objects")
			path := filepath.Join(objects, "i-00000000000000000000000000000000.spool")
			outside := filepath.Join(t.TempDir(), "secret")
			if e = os.WriteFile(outside, []byte("preserve"), 0600); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "objects_link":
				_ = os.Remove(objects)
				e = os.Symlink(filepath.Dir(outside), objects)
			case "lock_link":
				_ = os.Remove(filepath.Join(o.Directory, ".lock"))
				e = os.Symlink(outside, filepath.Join(o.Directory, ".lock"))
			case "public_file":
				e = os.WriteFile(path, []byte("bad-permission"), 0644)
			case "hardlink":
				e = os.Link(outside, path)
			case "symlink":
				e = os.Symlink(outside, path)
			case "fifo":
				e = unix.Mkfifo(path, 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			_, e = Open(o)
			if e == nil {
				t.Fatal("unsafe spool accepted")
			}
			if b, e := os.ReadFile(outside); e != nil || string(b) != "preserve" {
				t.Fatal("outside modified")
			}
		})
	}
}

func TestArtifactsNativePinnedDirectoryAndPermissions(t *testing.T) {
	o := options(t, 16)
	s, e := Open(o)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	renamed := o.Directory + "-renamed"
	if e = os.Rename(o.Directory, renamed); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(o.Directory, 0700); e != nil {
		t.Fatal(e)
	}
	scope := outputScope(t, s, time.Minute)
	st, e := scope.Begin(context.Background(), "file", "application/octet-stream")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.Write([]byte("pinned")); e != nil {
		t.Fatal(e)
	}
	files, e := os.ReadDir(filepath.Join(renamed, "objects"))
	if e != nil || len(files) != 1 {
		t.Fatal("descriptor rebound")
	}
	i, e := os.Stat(filepath.Join(renamed, "objects", files[0].Name()))
	if e != nil || i.Mode().Perm() != 0600 {
		t.Fatal("file permission")
	}
	if _, e = os.Stat(filepath.Join(o.Directory, "objects")); !os.IsNotExist(e) {
		t.Fatal("replacement directory changed")
	}
	if e = st.Abort(); e != nil {
		t.Fatal(e)
	}
	if s.Usage() != 0 {
		t.Fatal(s.Usage())
	}
}
