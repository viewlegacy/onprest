//go:build darwin

package filesystem

import (
	"os"
	"testing"
)

func TestFilesystemNativeDarwinMountBoundary(t *testing.T) {
	// macOS's APFS Data volume supplies a native, unprivileged mount fixture.
	// Refuse an absent/same-filesystem fixture rather than claiming a mount test.
	r, e := OpenRoot("/")
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	mounted, e := OpenRoot("/System/Volumes/Data")
	if e != nil {
		t.Fatal(e)
	}
	defer mounted.Close()
	if sameFilesystem(r.identity, mounted.identity) {
		t.Fatal("Data volume fixture is not a filesystem crossing")
	}
	f, _, e := r.open("System/Volumes/Data")
	if f != nil || e == nil {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("mount boundary accepted")
	}
	code(t, e, "PERMISSION_DENIED")
	if _, e := os.Stat("/System/Volumes/Data"); e != nil {
		t.Fatal(e)
	}
}
