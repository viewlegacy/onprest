//go:build windows

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func createValidationLinkLikePath(t *testing.T, path string) validationPathInvariant {
	t.Helper()
	target := path + ".target"
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "reparse-target-marker")
	if err := os.WriteFile(marker, []byte("reparse-target-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", path, target).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v\n%s", err, output)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		t.Fatalf("junction is not a reparse point: attributes=%x err=%v", attributes, err)
	}
	return captureValidationPathInvariant(t, path, func(t *testing.T) {
		t.Helper()
		attributes, err := windows.GetFileAttributes(name)
		if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
			t.Fatalf("protected junction changed: attributes=%x err=%v", attributes, err)
		}
		got, err := os.ReadFile(marker)
		if err != nil || string(got) != "reparse-target-content" {
			t.Fatalf("protected reparse target content=%q err=%v", got, err)
		}
	})
}

func invalidFixedValidationPathFactories() []validationPathFactory {
	return []validationPathFactory{{name: "reparse-junction", create: createValidationLinkLikePath}}
}
