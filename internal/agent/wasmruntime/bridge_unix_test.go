//go:build linux || darwin

package wasmruntime

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestWasmBridgeSpecialFilesFailClosedBeforeBlocking(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			r, m, c, base := bridgeSetup(t, on, "read_write")
			fifo := filepath.Join(base, "fifo")
			if e := unix.Mkfifo(fifo, 0600); e != nil {
				t.Fatal(e)
			}
			peer, e := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if e != nil {
				t.Fatal(e)
			}
			defer unix.Close(peer)
			cases := []any{step("files.open", map[string]any{"path": "fifo"}, nil, true), step("files.create", map[string]any{"path": "fifo"}, []byte("must not write"), false)}
			values := entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": cases}, DefaultLimits()))
			for _, value := range values {
				if callCode(value) != -2 {
					t.Fatal("special file admitted", values)
				}
			}
			info, e := os.Lstat(fifo)
			if e != nil || info.Mode()&os.ModeNamedPipe == 0 {
				t.Fatal("rejection changed FIFO", info, e)
			}
			buffer := make([]byte, 32)
			if n, _ := unix.Read(peer, buffer); n > 0 {
				t.Fatal("rejected write reached FIFO peer", n)
			}
			// OFF also has OS permission for the device, but the regular-file API rejects it.
			if !on {
				values = entries(bridgeRun(t, r, m, c, map[string]any{"scenario": "bridge", "steps": []any{step("files.open", map[string]any{"path": "/dev/null"}, nil, true)}}, DefaultLimits()))
				if callCode(values[0]) != -2 {
					t.Fatal("device admitted", values)
				}
			}
		})
	}
}
