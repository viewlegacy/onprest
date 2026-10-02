//go:build windows

package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedConfigurationWindowsDACLAndReparse(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "private")
	secretFile(t, private, []byte("secret"))
	if b, e := readProtected(context.Background(), private); e != nil || string(b) != "secret" {
		t.Fatal(e)
	}
	public := filepath.Join(dir, "public")
	if e := os.WriteFile(public, []byte("secret"), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e := readProtected(context.Background(), public); e == nil {
		t.Fatal("inherited public DACL accepted")
	}
	link := filepath.Join(dir, "link")
	// os.Symlink handles the Windows unprivileged-create flag and its fallback.
	if e := os.Symlink(private, link); e != nil {
		t.Fatal("Windows symbolic-link fixture requires supported OS permission", e)
	}
	if _, e := readProtected(context.Background(), link); e == nil {
		t.Fatal("reparse secret accepted")
	}
	identity := filepath.Join(dir, "identity")
	if _, e := identityKey(identity); e != nil {
		t.Fatal(e)
	}
	f, e := openRegular(filepath.Join(identity, "config-hmac.key"), true)
	if e != nil {
		t.Fatal(e)
	}
	_ = f.Close()
	if _, e := identityKey(public); e == nil {
		t.Fatal("non-directory identity accepted")
	}
}
