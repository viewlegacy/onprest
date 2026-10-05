//go:build linux || darwin

package artifacts

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/contract"
)

func saveOptions(dir string) Options {
	return Options{Directory: filepath.Join(dir, "spool"), QuotaBytes: 8 << 20, MaxPending: 4, MaxObjects: 128, TransferTimeout: time.Minute}
}
func saveValue(body []byte) contract.FileValue {
	sum := sha256.Sum256(body)
	return contract.FileValue{Pointer: "/file", ID: "id", RequestID: "request", Name: "input.bin", MIMEType: "application/octet-stream", SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
}
func saveDirectory(t *testing.T, dir string) *filesystem.Directory {
	t.Helper()
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	d, e := filesystem.OpenDirectory(dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
func saveOpener(body []byte) func(context.Context, contract.FileValue) (io.ReadCloser, error) {
	return func(context.Context, contract.FileValue) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}
func prepareJournal(t *testing.T, s *Store) {
	t.Helper()
	if e := s.root.Mkdir("file-write-cleanup", 0700); e != nil {
		t.Fatal(e)
	}
	if e := s.openSaveJournal(); e != nil {
		t.Fatal(e)
	}
}

func TestSaveAtomicOriginalEmptyOverwriteAndAllDestinationValidation(t *testing.T) {
	for _, size := range []int{0, 1, 130 * 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			base := t.TempDir()
			d := saveDirectory(t, filepath.Join(base, "business"))
			s, e := Open(saveOptions(base))
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			body := bytes.Repeat([]byte{0, 255, 10, 13, 128}, (size+4)/5)[:size]
			v := saveValue(body)
			files := []SaveFile{{Directory: d, Name: "a.bin", Value: v}, {Directory: d, Name: "b.bin", Value: v}}
			installed := 0
			if e = s.Save(context.Background(), "request", files, saveOpener(body), func(ok bool) {
				if ok {
					installed++
				}
			}); e != nil {
				t.Fatal(e)
			}
			if installed != 2 || s.Usage() != 0 {
				t.Fatalf("publication/quota %d %d", installed, s.Usage())
			}
			for _, name := range []string{"a.bin", "b.bin"} {
				b, e := os.ReadFile(filepath.Join(d.Path(), name))
				if e != nil || !bytes.Equal(b, body) {
					t.Fatal("byte mismatch", e)
				}
			}
			if e = s.Save(context.Background(), "conflict", files, saveOpener([]byte("other")), nil); e == nil {
				t.Fatal("existing replaced")
			}
			if s.Usage() != 0 {
				t.Fatal("conflict reservation leaked")
			}
			files[0].Overwrite = true
			files = files[:1]
			e = s.Save(context.Background(), "replace", files, saveOpener(body), nil)
			if runtime.GOOS == "windows" {
				if !errors.Is(e, errors.ErrUnsupported) {
					t.Fatal("unsafe Windows replace", e)
				}
			} else if e != nil {
				t.Fatal(e)
			}
			entries, _ := os.ReadDir(d.Path())
			if len(entries) != 2 {
				t.Fatal("left temp", entries)
			}
		})
	}
	base := t.TempDir()
	d := saveDirectory(t, filepath.Join(base, "business"))
	s, e := Open(saveOptions(base))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	v := saveValue([]byte("payload"))
	for _, name := range []string{"../escape", ".onprest-owned", "NUL", "a:stream", "trailing.", "CONIN$", "LPT¹"} {
		files := []SaveFile{{Directory: d, Name: "first", Value: v}, {Directory: d, Name: name, Value: v}}
		if e = s.Save(context.Background(), "request", files, saveOpener([]byte("payload")), nil); e == nil {
			t.Fatal("invalid save name", name)
		}
		if _, e := os.Stat(filepath.Join(d.Path(), "first")); !os.IsNotExist(e) {
			t.Fatal("early completed file")
		}
	}
	for _, second := range []string{"same", "SAME"} {
		if second == "SAME" && runtime.GOOS == "linux" {
			continue
		}
		if e = s.Save(context.Background(), "duplicate", []SaveFile{{Directory: d, Name: "same", Value: v}, {Directory: d, Name: second, Overwrite: true, Value: v}}, saveOpener([]byte("payload")), nil); e == nil {
			t.Fatal("same destination accepted")
		}
		if _, e := os.Stat(filepath.Join(d.Path(), "same")); !os.IsNotExist(e) {
			t.Fatal("duplicate partial save")
		}
	}
}

type lateReader struct{ sent bool }

func (r *lateReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		copy(p, "x")
		return 1, nil
	}
	return 0, errors.New("private reader failed")
}
func (r *lateReader) Close() error { return nil }
func TestSaveFailuresCancellationQuotaAndCleanupStop(t *testing.T) {
	for _, point := range []string{"reader", "cancel_copy", "cancel_rename", "quota", "cleanup", "write", "disk_full", "sync", "create"} {
		t.Run(point, func(t *testing.T) {
			base := t.TempDir()
			d := saveDirectory(t, filepath.Join(base, "business"))
			o := saveOptions(base)
			if point == "quota" {
				o.QuotaBytes = manifestLimit + 1
			}
			s, e := Open(o)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			prepareJournal(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := []byte("xx")
			v := saveValue(body)
			files := []SaveFile{{Directory: d, Name: "a", Value: v}, {Directory: d, Name: "b", Value: v}}
			open := saveOpener(body)
			if point == "reader" {
				open = func(context.Context, contract.FileValue) (io.ReadCloser, error) { return &lateReader{}, nil }
			}
			if strings.HasPrefix(point, "cancel") {
				s.journal.hook = func(event string) {
					if event == strings.TrimPrefix(point, "cancel_") {
						cancel()
					}
				}
			}
			if point == "cleanup" {
				s.journal.remove = func(string) error { return errors.New("private unlink failed") }
			}
			if point == "create" {
				s.journal.hook = func(event string) {
					if event == "manifest" {
						entries, e := os.ReadDir(filepath.Join(base, "spool", "file-write-cleanup"))
						if e != nil {
							t.Fatal(e)
						}
						for _, entry := range entries {
							if manifestName.MatchString(entry.Name()) {
								b, e := os.ReadFile(filepath.Join(base, "spool", "file-write-cleanup", entry.Name()))
								if e != nil {
									t.Fatal(e)
								}
								var m saveManifest
								if e = json.Unmarshal(b, &m); e != nil {
									t.Fatal(e)
								}
								v := m.Entries[len(m.Entries)-1]
								if e = os.Mkdir(filepath.Join(v.Directory, v.Temp), 0700); e != nil {
									t.Fatal(e)
								}
							}
						}
					}
				}
			}
			if point == "disk_full" {
				calls := 0
				s.write = func(f *os.File, b []byte) (int, error) {
					calls++
					if calls == 2 {
						n, e := f.Write(b[:1])
						if e != nil {
							return n, e
						}
						return n, syscall.ENOSPC
					}
					return f.Write(b)
				}
			}
			if point == "write" {
				calls := 0
				s.write = func(f *os.File, b []byte) (int, error) {
					calls++
					if calls == 2 {
						return f.Write(b[:1])
					}
					return f.Write(b)
				}
			}
			if point == "sync" {
				s.syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
			}
			installed := 0
			e = s.Save(ctx, "request", files, open, func(ok bool) {
				if ok {
					installed++
				}
			})
			if e == nil {
				t.Fatal("failure succeeded")
			}
			want := 0
			if point == "cancel_rename" {
				want = 1
			}
			if point == "cleanup" {
				want = 2
			}
			if installed != want {
				t.Fatalf("installed %d want %d", installed, want)
			}
			for i, name := range []string{"a", "b"} {
				b, e := os.ReadFile(filepath.Join(d.Path(), name))
				if i < want {
					if e != nil || !bytes.Equal(b, body) {
						t.Fatal("completed lost")
					}
				} else if !os.IsNotExist(e) {
					t.Fatal("incomplete published")
				}
			}
			if point == "cleanup" || point == "create" {
				if s.Usage() < manifestLimit || s.journal.active != 1 {
					t.Fatal("failed cleanup forgot reservation/slot")
				}
				s.journal.remove = s.journal.root.Remove
				if e = s.Save(context.Background(), "new", files, saveOpener(body), nil); e == nil {
					t.Fatal("new write after cleanup failure")
				}
			} else if s.Usage() != 0 {
				t.Fatal("quota leaked", s.Usage())
			}
		})
	}
}

func crashSave(t *testing.T, base, point, mode string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSaveCrashRecoveryRealProcess$")
	cmd.Env = append(os.Environ(), "ONPREST_SAVE_CHILD="+base, "ONPREST_SAVE_POINT="+point, "ONPREST_SAVE_MODE="+mode)
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			if scanner.Text() == "SAVE_BARRIER" {
				ready <- scanner.Text()
				return
			}
		}
		ready <- "EOF"
	}()
	select {
	case v := <-ready:
		if v != "SAVE_BARRIER" {
			_ = cmd.Wait()
			t.Fatalf("child failed %s", stderr.String())
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("child barrier timeout", stderr.String())
	}
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e = cmd.Wait(); e == nil {
		t.Fatal("child was not killed")
	}
}
func TestSaveCrashRecoveryRealProcess(t *testing.T) {
	if base := os.Getenv("ONPREST_SAVE_CHILD"); base != "" {
		point, mode := os.Getenv("ONPREST_SAVE_POINT"), os.Getenv("ONPREST_SAVE_MODE")
		barrier := func(event string) {
			if event == point {
				fmt.Println("SAVE_BARRIER")
				for {
					time.Sleep(time.Hour)
				}
			}
		}
		if mode == "recover" {
			saveRecoveryHook = barrier
			s, e := Open(saveOptions(base))
			if e != nil {
				t.Fatal(e)
			}
			s.Close()
			t.Fatal("missing recovery barrier")
		}
		s, e := Open(saveOptions(base))
		if e != nil {
			t.Fatal(e)
		}
		prepareJournal(t, s)
		s.journal.hook = barrier
		d := saveDirectory(t, filepath.Join(base, "business"))
		body := bytes.Repeat([]byte{128, 0, 255}, 45000)
		v := saveValue(body)
		files := []SaveFile{{Directory: d, Name: "a", Overwrite: mode == "overwrite", Value: v}, {Directory: d, Name: "b", Value: v}}
		if e = s.Save(context.Background(), "request", files, saveOpener(body), nil); e != nil {
			t.Fatal(e)
		}
		t.Fatal("missing save barrier")
	}
	for _, mode := range []string{"new", "overwrite"} {
		for _, point := range []string{"manifest_before", "manifest_draft", "manifest", "create", "identity", "copy", "rename", "manifest_delete_before", "cleanup"} {
			t.Run(mode+"/"+point, func(t *testing.T) {
				base := t.TempDir()
				business := filepath.Join(base, "business")
				if e := os.Mkdir(business, 0700); e != nil {
					t.Fatal(e)
				}
				if mode == "overwrite" {
					if e := os.WriteFile(filepath.Join(business, "a"), []byte("previous completed business file"), 0644); e != nil {
						t.Fatal(e)
					}
				}
				crashSave(t, base, point, mode)
				// A recovery crash is repeated against the same durable manifest. Neither
				// the second recovery nor source configuration changes may resume saving.
				if point == "copy" || point == "rename" {
					crashSave(t, base, "recovery_cleanup", "recover")
				}
				s, e := Open(saveOptions(base))
				if e != nil {
					t.Fatal(e)
				}
				if s.Usage() != 0 {
					t.Fatal("recovery quota", s.Usage())
				}
				s.Close()
				entries, e := os.ReadDir(business)
				if e != nil {
					t.Fatal(e)
				}
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".onprest-") {
						t.Fatal("unrecovered temp")
					}
				}
				newCount := 0
				if point == "rename" {
					newCount = 1
				}
				if point == "manifest_delete_before" || point == "cleanup" {
					newCount = 2
				}
				body := bytes.Repeat([]byte{128, 0, 255}, 45000)
				for i, name := range []string{"a", "b"} {
					b, e := os.ReadFile(filepath.Join(business, name))
					if i < newCount {
						if e != nil || !bytes.Equal(b, body) {
							t.Fatal("completed file removed", name, e)
						}
					} else if name == "a" && mode == "overwrite" {
						if e != nil || string(b) != "previous completed business file" {
							t.Fatal("prior file lost")
						}
					} else if !os.IsNotExist(e) {
						t.Fatal("recovery resumed business publication")
					}
				}
			})
		}
	}
}

func TestSaveRecoveryRejectsUnknownIdentityProtectionAndBounds(t *testing.T) {
	for _, attack := range []string{"corrupt", "unknown", "oversize", "count", "directory", "identity", "permission", "link", "missing_directory", "quota", "owner", "agent", "hardlink", "manifest_permission", "duplicate_key", "old_growth"} {
		t.Run(attack, func(t *testing.T) {
			base := t.TempDir()
			os.Mkdir(filepath.Join(base, "business"), 0700)
			if attack == "old_growth" {
				os.WriteFile(filepath.Join(base, "business", "a"), []byte("old"), 0644)
				crashSave(t, base, "rename", "overwrite")
			} else {
				crashSave(t, base, "copy", "new")
			}
			cleanup := filepath.Join(base, "spool", "file-write-cleanup")
			entries, _ := os.ReadDir(cleanup)
			var name string
			for _, v := range entries {
				if manifestName.MatchString(v.Name()) {
					name = v.Name()
				}
			}
			b, e := os.ReadFile(filepath.Join(cleanup, name))
			if e != nil {
				t.Fatal(e)
			}
			var m saveManifest
			if e = json.Unmarshal(b, &m); e != nil {
				t.Fatal(e)
			}
			temp := filepath.Join(m.Entries[0].Directory, m.Entries[0].Temp)
			preserve, _ := os.ReadFile(temp)
			switch attack {
			case "corrupt":
				b = []byte(`{"bad":true}`)
			case "unknown":
				os.WriteFile(filepath.Join(cleanup, "unknown"), nil, 0600)
			case "oversize":
				b = bytes.Repeat([]byte("x"), int(manifestLimit)+1)
			case "count":
				for i := 0; i < 5; i++ {
					os.WriteFile(filepath.Join(cleanup, fmt.Sprintf("m-%032x.json", i)), b, 0600)
				}
			case "directory":
				m.Entries[0].DirectoryID.Inode++
				b, _ = json.Marshal(m)
			case "identity":
				m.Entries[0].Identity.Identity.Inode++
				b, _ = json.Marshal(m)
			case "old_growth":
				preserve = []byte("old writer enlarged")
				os.WriteFile(temp, preserve, 0644)
			case "owner":
				m.Entries[0].Identity.Owner += "-foreign"
				b, _ = json.Marshal(m)
			case "agent":
				m.Agent = "00000000000000000000000000000000"
				b, _ = json.Marshal(m)
			case "hardlink":
				if e = os.Link(temp, filepath.Join(base, "other-link")); e != nil {
					t.Fatal(e)
				}
			case "duplicate_key":
				b = []byte(`{"version":1,"version":1}`)
			case "manifest_permission":
				os.Chmod(filepath.Join(cleanup, name), 0644)
			case "permission":
				os.Chmod(temp, 0644)
			case "link":
				os.Remove(temp)
				outside := filepath.Join(base, "outside")
				os.WriteFile(outside, preserve, 0600)
				if e = os.Symlink(outside, temp); e != nil {
					t.Fatal(e)
				}
			case "missing_directory":
				os.Rename(filepath.Join(base, "business"), filepath.Join(base, "moved"))
			}
			if e = os.WriteFile(filepath.Join(cleanup, name), b, 0600); e != nil {
				t.Fatal(e)
			}
			o := saveOptions(base)
			if attack == "quota" {
				o.QuotaBytes = manifestLimit
			}
			s, e := Open(o)
			if e == nil {
				s.Close()
				t.Fatal("unsafe recovery admitted", attack)
			}
			if _, e := os.Stat(filepath.Join(cleanup, name)); e != nil {
				t.Fatal("manifest removed before full validation")
			}
			actualPath := temp
			if attack == "missing_directory" {
				actualPath = filepath.Join(base, "moved", m.Entries[0].Temp)
			}
			if b, e := os.ReadFile(actualPath); e != nil || !bytes.Equal(b, preserve) {
				t.Fatal("unverified temp changed", e)
			}
		})
	}
}

func TestSaveCleanupUnknownGrowthReaccountsAndPoisonsZeroByteAdmission(t *testing.T) {
	base := t.TempDir()
	d := saveDirectory(t, filepath.Join(base, "business"))
	s, e := Open(saveOptions(base))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	prepareJournal(t, s)
	os.WriteFile(filepath.Join(d.Path(), "a"), []byte("old"), 0644)
	s.journal.hook = func(event string) {
		if event == "rename" {
			entries, e := os.ReadDir(d.Path())
			if e != nil {
				t.Fatal(e)
			}
			for _, v := range entries {
				if filesystem.ValidTempName(v.Name()) {
					info, _ := v.Info()
					if info.Mode().Perm() == 0644 {
						if e = os.WriteFile(filepath.Join(d.Path(), v.Name()), bytes.Repeat([]byte("x"), 1<<20), 0644); e != nil {
							t.Fatal(e)
						}
					}
				}
			}
		}
	}
	value := saveValue(nil)
	confirmed := 0
	e = s.Save(context.Background(), "empty", []SaveFile{{Directory: d, Name: "a", Overwrite: true, Value: value}}, saveOpener(nil), func(ok bool) {
		if ok {
			confirmed++
		}
	})
	if e == nil || confirmed != 1 || s.Usage() < manifestReservation+(1<<20) || s.journal.active != 1 {
		t.Fatal("unknown cleanup accounting", e, confirmed, s.Usage())
	}
	if b, e := os.ReadFile(filepath.Join(d.Path(), "a")); e != nil || len(b) != 0 {
		t.Fatal("completed empty file lost", e)
	}
	if _, e = s.Reserve(1); e == nil {
		t.Fatal("common quota admitted after poison")
	}
	if e = s.Save(context.Background(), "new-empty", []SaveFile{{Directory: d, Name: "b", Value: value}}, saveOpener(nil), nil); e == nil {
		t.Fatal("zero-byte write bypassed poison")
	}
}

func TestSaveZeroByteStillReservesManifestAndAllSlots(t *testing.T) {
	base := t.TempDir()
	d := saveDirectory(t, filepath.Join(base, "business"))
	o := saveOptions(base)
	o.QuotaBytes = manifestReservation - 1
	s, e := Open(o)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	e = s.Save(context.Background(), "zero", []SaveFile{{Directory: d, Name: "empty", Value: saveValue(nil)}}, saveOpener(nil), nil)
	var failure *Error
	if !errors.As(e, &failure) || failure.Code != "LIMIT_EXCEEDED" {
		t.Fatal("empty bypassed manifest quota", e)
	}
	entries, e := os.ReadDir(d.Path())
	if e != nil || len(entries) != 0 || s.Usage() != 0 {
		t.Fatal("quota failure changed business directory", entries, e)
	}
}
func TestSaveMacOSNormalizationCollisionBeforeAnyPublication(t *testing.T) {
	if runtime.GOOS != "darwin" {
		return
	}
	base := t.TempDir()
	d := saveDirectory(t, filepath.Join(base, "business"))
	s, e := Open(saveOptions(base))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	value := saveValue(nil)
	e = s.Save(context.Background(), "normalization", []SaveFile{{Directory: d, Name: "é.pdf", Value: value}, {Directory: d, Name: "e\u0301.pdf", Overwrite: true, Value: value}}, saveOpener(nil), nil)
	var failure *Error
	if !errors.As(e, &failure) || failure.Code != "CONFLICT" {
		t.Fatal("normalization collision", e)
	}
	entries, e := os.ReadDir(d.Path())
	if e != nil || len(entries) != 0 {
		t.Fatal("collision published", entries, e)
	}
}
