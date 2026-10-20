//go:build windows

package artifacts

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestArtifactsNativeWindowsPrivateDACLAndReparse(t *testing.T) {
	o := options(t, 16)
	s, e := Open(o)
	if e != nil {
		t.Fatal(e)
	}
	scope := outputScope(t, s, time.Minute)
	st, e := scope.Begin(context.Background(), "file", "application/octet-stream")
	if e != nil {
		t.Fatal(e)
	}
	if e = protect(st.(*Stage).file, false); e != nil {
		t.Fatal(e)
	}
	_, e = st.Write([]byte("private"))
	if e != nil {
		t.Fatal(e)
	}
	_ = st.Abort()
	_ = s.Close()
	outside := t.TempDir()
	if e = os.Remove(filepath.Join(o.Directory, "objects")); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(o.Directory, "objects"), outside)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("junction fixture %v %s", e, b)
	}
	if _, e = Open(o); e == nil {
		t.Fatal("junction accepted")
	}
	_ = os.Remove(filepath.Join(o.Directory, "objects"))
	sd, e := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if e != nil {
		t.Fatal(e)
	}
	dacl, _, e := sd.DACL()
	if e != nil {
		t.Fatal(e)
	}
	if e = windows.SetNamedSecurityInfo(o.Directory, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); e != nil {
		t.Fatal(e)
	}
	if _, e = Open(o); e == nil {
		t.Fatal("public DACL accepted")
	}
}
