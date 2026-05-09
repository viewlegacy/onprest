package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingFileWriterRotatesBySize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onprest-agent.log")
	w, err := newRotatingFileWriter(path, 20, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("1234567890\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abcdefghij\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "abcdefghij\n" {
		t.Fatalf("current log = %q", string(current))
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated) != "1234567890\n" {
		t.Fatalf("rotated log = %q", string(rotated))
	}
}

func TestRotatingFileWriterHonorsMaxFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onprest-agent.log")
	w, err := newRotatingFileWriter(path, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{"one\n", "two\n", "three\n", "four\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, path, "four\n")
	assertFileContent(t, path+".1", "three\n")
	assertFileContent(t, path+".2", "two\n")
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected third rotated file: %v", err)
	}
}

func TestAgentDetailLogPathUsesExecutableDirectory(t *testing.T) {
	old := executablePath
	defer func() { executablePath = old }()
	dir := t.TempDir()
	executablePath = func() (string, error) {
		return filepath.Join(dir, "onprest-agent"), nil
	}
	w, err := newAgentDetailLog(LoggingDef{MaxSize: "1KB", MaxFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("detail\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "onprest-agent.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "detail\n" {
		t.Fatalf("detail log = %q", string(b))
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, string(got), want)
	}
}
