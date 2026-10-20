//go:build windows

package agent

import (
	"golang.org/x/sys/windows"
	"testing"
)

func codeUnsafeNativeInputs(t *testing.T, directory string) map[string]string {
	t.Helper()
	pipe := `\\.\pipe\onprest-code-input-` + randomTransportID()
	p, e := windows.UTF16PtrFromString(pipe)
	if e != nil {
		t.Fatal(e)
	}
	h, e := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_INBOUND, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT, 1, 4096, 4096, 0, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	return map[string]string{"nul": "NUL", "con": "CON", "device-namespace": `\\.\NUL`, "long-raw-namespace": `\\?\NUL`, "named-pipe": pipe}
}
