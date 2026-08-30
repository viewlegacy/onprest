//go:build !windows

package agent

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func createValidationLinkLikePath(t *testing.T, path string) validationPathInvariant {
	t.Helper()
	target := path + ".target"
	if err := os.WriteFile(target, []byte("symlink-target-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	return captureValidationPathInvariant(t, path, func(t *testing.T) {
		t.Helper()
		gotTarget, err := os.Readlink(path)
		if err != nil || gotTarget != target {
			t.Fatalf("protected symlink target=%q err=%v", gotTarget, err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "symlink-target-marker" {
			t.Fatalf("protected symlink content=%q err=%v", got, err)
		}
	})
}

func invalidFixedValidationPathFactories() []validationPathFactory {
	return []validationPathFactory{
		{name: "symlink", create: createValidationLinkLikePath},
		{name: "non-regular-fifo", create: func(t *testing.T, path string) validationPathInvariant {
			t.Helper()
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			return captureValidationPathInvariant(t, path, nil)
		}},
	}
}
