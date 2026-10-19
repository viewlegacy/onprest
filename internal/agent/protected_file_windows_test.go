//go:build windows

package agent

import (
	"context"
	"os"
	"testing"

	"github.com/viewlegacy/onprest/internal/agent/providers"
	"golang.org/x/sys/windows"
)

func writeProtectedTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	sa, err := privateSecurityAttributes()
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(h), path)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := providers.ReadProtectedConfiguration(context.Background(), path); err != nil {
		t.Fatalf("protected fixture rejected: %v", err)
	}
}
