package agent

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func codeUnsafeInputs(t *testing.T, directory string) map[string]string {
	t.Helper()
	regular := filepath.Join(directory, "regular.txt")
	if e := os.WriteFile(regular, []byte("valid"), 0600); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(directory, "regular-link.txt")
	if e := os.Symlink(regular, link); e != nil {
		t.Fatal("native file symlink fixture required", e)
	}
	cases := codeUnsafeNativeInputs(t, directory)
	cases["symlink"] = link
	cases["directory"] = directory
	return cases
}
func TestCodeInputStrictNativeOpen(t *testing.T) {
	directory := t.TempDir()
	unsafe := codeUnsafeInputs(t, directory)
	for name, path := range unsafe {
		t.Run(name, func(t *testing.T) {
			f, e := codeOpenInput(path)
			if f != nil {
				f.Close()
			}
			if e == nil {
				t.Fatal("nonregular or linked input accepted", path)
			}
		})
	}
	path := filepath.Join(directory, "regular.txt")
	f, e := codeOpenInput(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	renamed := filepath.Join(directory, "original.txt")
	if e := os.Rename(path, renamed); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte("replacement"), 0600); e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(f)
	if e != nil || string(b) != "valid" {
		t.Fatal("input descriptor rebound after rename", string(b), e)
	}
}
