//go:build linux || darwin

package wasmruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// metadataFixture creates a readable file, a write-only file, a dangling link
// and a FIFO whose writer stays blocked until somebody opens it for reading.
func metadataFixture(t *testing.T, base string) (dir string, opened <-chan error) {
	t.Helper()
	dir = filepath.Join(base, "d")
	os.Mkdir(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "a"), []byte("readable"), 0o600)
	os.WriteFile(filepath.Join(dir, "w"), []byte("write-only"), 0o200)
	if e := os.Symlink("missing", filepath.Join(dir, "z")); e != nil {
		t.Fatal(e)
	}
	fifo := filepath.Join(dir, "p")
	if e := unix.Mkfifo(fifo, 0o600); e != nil {
		t.Fatal(e)
	}
	writer := make(chan error, 1)
	go func() {
		f, e := os.OpenFile(fifo, os.O_WRONLY, 0)
		if e == nil {
			f.Close()
		}
		writer <- e
	}()
	t.Cleanup(func() {
		// Release the blocked writer by opening the reader ourselves.
		if f, e := os.OpenFile(fifo, os.O_RDONLY|unix.O_NONBLOCK, 0); e == nil {
			<-writer
			f.Close()
		}
	})
	return dir, writer
}
func assertFIFONotOpened(t *testing.T, opened <-chan error) {
	t.Helper()
	select {
	case e := <-opened:
		t.Fatal("FIFO was opened for reading by a listing or stat", e)
	case <-time.After(100 * time.Millisecond):
	}
}

// Listing and stat describe entries from their metadata, as a native process
// sees them, without opening the entry: the FIFO's writer stays blocked, a
// write-only file can be stat'ed, and special or dangling entries are listed
// with their type instead of failing the listing. Both the SDK (host ABI) and
// raw WASI see the same entries, with the sandbox off and on.
func TestWasmMetadataNeverOpensEntries(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			r, m, c, base := sdkSetup(t, on, "read_write")
			_, opened := metadataFixture(t, base)
			v := sdkData(t, r, m, c, map[string]any{"scenario": "read-dir-partial", "path": "d"}, DefaultLimits())
			if fmt.Sprint(v["names"]) != "[a p w z]" || v["failed"] != false {
				t.Fatal("SDK listing", v)
			}
			v = sdkData(t, r, m, c, map[string]any{"scenario": "stat-path", "path": "d/w"}, DefaultLimits())
			if v["failed"] != false || fmt.Sprint(v["size"]) != "10" {
				t.Fatal("SDK write-only stat", v)
			}
			v = sdkData(t, r, m, c, map[string]any{"scenario": "stat-path", "path": "d/p"}, DefaultLimits())
			if mode, _ := v["mode"].(string); v["failed"] != false || len(mode) == 0 || mode[0] != 'p' {
				t.Fatal("SDK FIFO stat", v)
			}
			assertFIFONotOpened(t, opened)

			raw, rm, rc, rawBase := bridgeSetup(t, on, "read")
			_, rawOpened := metadataFixture(t, rawBase)
			out := bridgeRun(t, raw, rm, rc, map[string]any{"scenario": "raw_readdir", "path": "d"}, DefaultLimits()).(map[string]any)
			if fmt.Sprint(out["names"]) != "[a p w z]" || out["error"] != "" {
				t.Fatal("raw WASI listing", out)
			}
			assertFIFONotOpened(t, rawOpened)
		})
	}
}
