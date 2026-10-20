//go:build windows

package filesystem

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFilesystemNativeWindowsADSDevicesAndJunction(t *testing.T) {
	r, p := root(t)
	put(t, p, "safe", []byte("inside"))
	if e := os.WriteFile(filepath.Join(p, "safe:secret"), []byte("outside-secret"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"safe:secret", "NUL", "CON.txt", "COM1", "LPT1", "COM¹", `\\.\NUL`, "safe.", "safe "} {
		if f, e := r.Open(context.Background(), name); e == nil || f != nil {
			t.Fatalf("unsafe Windows open %q", name)
		}
	}
	outside := t.TempDir()
	put(t, outside, "secret", []byte("outside-secret"))
	junction := filepath.Join(p, "junction")
	cmd := exec.Command("cmd", "/c", "mklink", "/J", junction, outside)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("junction fixture: %v %s", e, b)
	}
	if f, e := r.Open(context.Background(), "junction/secret"); e == nil || f != nil {
		t.Fatal("junction followed")
	}
	selected, e := r.Select(context.Background(), selector(t, "{name_regex: '.*', recursive: true}", ""), nil)
	if selected != nil || e == nil {
		t.Fatal("junction selected")
	}
}
