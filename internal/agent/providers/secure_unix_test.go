//go:build !windows

package providers

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedConfigurationRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "private")
	secretFile(t, target, []byte("secret"))
	for _, kind := range []string{"public_permissions", "symlink", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(dir, kind)
			switch kind {
			case "public_permissions":
				if e := os.WriteFile(path, []byte("secret"), 0o644); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				if e := os.Symlink(target, path); e != nil {
					t.Fatal(e)
				}
			case "fifo":
				if e := unix.Mkfifo(path, 0o600); e != nil {
					t.Fatal(e)
				}
			case "directory":
				if e := os.Mkdir(path, 0o700); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := readProtected(context.Background(), path); e == nil {
				t.Fatal("unsafe configuration file accepted")
			}
		})
	}
	publicIdentity := filepath.Join(dir, "public_identity")
	if e := os.Mkdir(publicIdentity, 0o755); e != nil {
		t.Fatal(e)
	}
	if _, e := identityKey(publicIdentity); e == nil {
		t.Fatal("public identity directory accepted")
	}
	linkedIdentity := filepath.Join(dir, "linked_identity")
	if e := os.Symlink(dir, linkedIdentity); e != nil {
		t.Fatal(e)
	}
	if _, e := identityKey(linkedIdentity); e == nil {
		t.Fatal("symlink identity directory accepted")
	}
}
