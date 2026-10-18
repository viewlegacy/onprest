package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/viewlegacy/onprest/internal/contract"
)

func TestCodeBuildPairRollback(t *testing.T) {
	for _, phase := range []string{"module", "manifest", "cancel", "renamed-parent", "first-build-manifest"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dest")
			os.Mkdir(path, 0700)
			d, e := openCodeDirectory(path)
			if e != nil {
				t.Fatal(e)
			}
			defer d.Close()
			old := map[string][]byte{"m.wasm": []byte("old-module"), "m.wasm.manifest.json": []byte("old-manifest")}
			if phase == "first-build-manifest" {
				old = map[string][]byte{}
			}
			for n, b := range old {
				if e := os.WriteFile(filepath.Join(path, n), b, 0600); e != nil {
					t.Fatal(e)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e = d.publishPairWithFault(ctx, "m.wasm", "m.wasm.manifest.json", []byte("new-module"), []byte("new-manifest"), old, func(p string) error {
				if phase == p || phase == "first-build-manifest" && p == "manifest" {
					return errors.New("publication fault")
				}
				if p == "manifest" && phase == "cancel" {
					cancel()
				}
				if p == "manifest" && phase == "renamed-parent" {
					if e := os.Rename(path, path+"-moved"); e != nil {
						t.Fatal(e)
					}
					if e := os.Mkdir(path, 0700); e != nil {
						t.Fatal(e)
					}
					return nil
				}
				return nil
			})
			if e == nil {
				t.Fatal("failure accepted")
			}
			if phase == "renamed-parent" {
				path += "-moved"
			}
			for _, n := range []string{"m.wasm", "m.wasm.manifest.json"} {
				b, e := os.ReadFile(filepath.Join(path, n))
				prior, exists := old[n]
				if exists && (e != nil || !bytes.Equal(b, prior)) || !exists && !os.IsNotExist(e) {
					t.Fatalf("%s old pair not restored %q %v", n, b, e)
				}
			}
			entries, _ := os.ReadDir(path)
			if len(entries) != len(old) {
				t.Fatal("private backup/temp leaked", entries)
			}
		})
	}
}

type codeFaultReader struct {
	io.Reader
	mode   string
	closes int
}

func (r *codeFaultReader) Read(b []byte) (int, error) {
	n, e := r.Reader.Read(b)
	if r.mode == "read" && e == io.EOF {
		return 0, io.ErrUnexpectedEOF
	}
	return n, e
}
func (r *codeFaultReader) Close() error {
	r.closes++
	if r.mode == "close" {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func codeTestOutput(reader io.ReadCloser, digest string) *contract.PreparedOutput {
	return &contract.PreparedOutput{Files: []contract.OutputArtifact{{ArtifactMetadata: contract.ArtifactMetadata{Name: "same.PDF", MIMEType: "application/pdf", SizeBytes: 5, SHA256: digest}, Reader: reader}}}
}
func TestCodeOutputPublicationFaults(t *testing.T) {
	for _, mode := range []string{"success", "read", "close", "digest", "size", "cancel", "existing-dir", "existing-file", "parent-replaced", "temp-link", "zero"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out")
			os.Mkdir(path, 0700)
			s, e := beginCodeOutput(path, "01J00000000000000000000000")
			if e != nil {
				t.Fatal(e)
			}
			r := &codeFaultReader{Reader: strings.NewReader("hello"), mode: mode}
			out := codeTestOutput(r, codeHash([]byte("hello")))
			defer out.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "digest":
				out.Files[0].SHA256 = strings.Repeat("a", 64)
			case "size":
				out.Files[0].SizeBytes++
			case "cancel":
				cancel()
			case "existing-dir":
				os.Mkdir(filepath.Join(path, s.id), 0700)
				os.WriteFile(filepath.Join(path, s.id, "keep"), []byte("old"), 0600)
			case "existing-file":
				os.WriteFile(filepath.Join(path, s.id), []byte("old"), 0600)
			case "parent-replaced":
				if e := os.Rename(path, path+"-old"); e != nil {
					t.Fatal(e)
				}
				if e := os.Mkdir(path, 0700); e != nil {
					t.Fatal(e)
				}
			case "temp-link":
				if e := os.Rename(filepath.Join(path, s.tempName), filepath.Join(path, "private-old")); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink("private-old", filepath.Join(path, s.tempName)); e != nil {
					t.Fatal(e)
				}
			case "zero":
				out.Files = nil
			}
			files, e := s.save(ctx, out)
			want := mode == "success" || mode == "zero"
			if (e == nil) != want {
				t.Fatalf("accepted=%v error=%v", want, e)
			}
			closeErr := s.close()
			if mode == "temp-link" {
				if closeErr == nil {
					t.Fatal("cleanup replacement silently accepted")
				}
			} else if closeErr != nil {
				t.Fatal(closeErr)
			}
			if mode == "success" {
				if len(files) != 1 || !s.committed {
					t.Fatal(files)
				}
				b, e := os.ReadFile(files[0].SavedPath)
				if e != nil || string(b) != "hello" {
					t.Fatal(e, string(b))
				}
				if r.closes != 1 {
					t.Fatal("reader not closed before publication", r.closes)
				}
			}
			if mode == "existing-file" {
				b, _ := os.ReadFile(filepath.Join(path, s.id))
				if string(b) != "old" {
					t.Fatal("existing file replaced")
				}
			}
			if mode == "existing-dir" {
				b, _ := os.ReadFile(filepath.Join(path, s.id, "keep"))
				if string(b) != "old" {
					t.Fatal("existing directory replaced")
				}
			}
			if mode != "success" && mode != "existing-dir" && mode != "existing-file" {
				if _, e := os.Stat(filepath.Join(path, s.id)); !os.IsNotExist(e) {
					t.Fatal("failed or zero output published", e)
				}
			}
		})
	}
	for name, want := range map[string]string{"report.PDF": "0001.pdf", "noext": "0001", "a.é": "0001", "a.abcdefghijklmnopq": "0001", "a.1": "0001.1"} {
		if got := codeSavedName(0, name); got != want {
			t.Fatal(name, got, want)
		}
	}
	if codeSavedName(10000, "a.txt") != "10001.txt" {
		t.Fatal("sequence truncated")
	}
}

type codeBrokenWriter struct{}

func (codeBrokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestCodeDiagnosticBudgetAndWriterFailure(t *testing.T) {
	var out bytes.Buffer
	diag := &codeDiagnostics{w: &out}
	compiler := &compilerDiagnostics{sink: diag}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := bytes.Repeat([]byte{'a'}, 400000)
			if n, e := compiler.Write(p); n != len(p) || e != nil {
				t.Error(n, e)
			}
		}()
	}
	wg.Wait()
	if strings.Count(out.String(), "event=log_truncated") != 1 || out.Len() > 1100000 {
		t.Fatal("compiler display not bounded", out.Len())
	}
	fail := &codeDiagnostics{w: codeBrokenWriter{}}
	if _, e := fail.Write([]byte(`{"ts":"t","level":"error","source":"wasm","event":"wasm_log","message":"private"}`)); e == nil || fail.failure() == nil {
		t.Fatal("diagnostic writer error lost")
	}
	if code := handleCodeCLI(context.Background(), []string{"validate", "--module", "missing"}, codeBrokenWriter{}, io.Discard); code != 1 {
		t.Fatal(code)
	}
}

type codeFaultWriter struct {
	*os.File
	mode string
}

func (w *codeFaultWriter) Write(b []byte) (int, error) {
	if w.mode == "disk-full" {
		n, e := w.File.Write(b[:1])
		return n, errors.Join(e, syscall.ENOSPC)
	}
	return w.File.Write(b)
}
func (w *codeFaultWriter) Sync() error {
	if w.mode == "flush" {
		return errors.New("injected flush failure")
	}
	return w.File.Sync()
}
func (w *codeFaultWriter) Close() error {
	e := w.File.Close()
	if w.mode == "close" {
		return errors.Join(e, errors.New("injected close failure"))
	}
	return e
}
func TestCodeOutputDiskAndPostFinalizeFaults(t *testing.T) {
	for _, mode := range []string{"disk-full", "flush", "close", "post-finalize", "cancel-after-finalize"} {
		t.Run(mode, func(t *testing.T) {
			path := t.TempDir()
			s, e := beginCodeOutput(path, "01J00000000000000000000000")
			if e != nil {
				t.Fatal(e)
			}
			reader := &codeFaultReader{Reader: strings.NewReader("hello")}
			out := codeTestOutput(reader, codeHash([]byte("hello")))
			defer out.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			files, e := s.saveWithIO(ctx, out, func(name string) (codeSaveWriter, error) {
				f, e := s.temp.root.HostOpen(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				return &codeFaultWriter{File: f, mode: mode}, e
			}, func() error {
				if mode == "post-finalize" {
					return errors.New("final directory flush failed")
				}
				if mode == "cancel-after-finalize" {
					cancel()
					return ctx.Err()
				}
				return nil
			})
			if e == nil {
				t.Fatal("output fault accepted")
			}
			if e = s.close(); e != nil {
				t.Fatal(e)
			}
			if mode == "post-finalize" || mode == "cancel-after-finalize" {
				if !s.committed || len(files) != 1 {
					t.Fatal(files)
				}
				b, e := os.ReadFile(files[0].SavedPath)
				if e != nil || string(b) != "hello" {
					t.Fatal("completed output removed", e)
				}
			} else {
				entries, _ := os.ReadDir(path)
				if len(entries) != 0 {
					t.Fatal("failed partial copy retained", entries)
				}
			}
		})
	}
}
