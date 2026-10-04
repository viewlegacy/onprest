package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/contract"
)

func owner() Owner { return Owner{"request", "caller", "test", "definition", "generation"} }
func options(t *testing.T, quota int64) Options {
	t.Helper()
	return Options{Directory: filepath.Join(t.TempDir(), "spool"), QuotaBytes: quota, MaxPending: 2, MaxObjects: 32, TransferTimeout: time.Minute}
}
func store(t *testing.T, quota int64) *Store {
	t.Helper()
	s, e := Open(options(t, quota))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if s.closed {
			return
		}
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	})
	return s
}
func code(t *testing.T, e error, want string) {
	t.Helper()
	var typed *Error
	if !errors.As(e, &typed) || typed.Code != want {
		t.Fatalf("error %v want %s", e, want)
	}
}
func capability(t *testing.T, body string) *contract.Capability {
	t.Helper()
	config, e := contract.Compile([]byte("schema_version: 1\nservice: {title: test, version: '1'}\ngateway: {url: wss://gateway.example/ws/agent, agent_private_key_file: ./key}\ncapabilities:\n  test:\n    " + strings.ReplaceAll(strings.TrimSpace(body), "\n", "\n    ") + "\n"))
	if e != nil {
		t.Fatal(e)
	}
	return config.Capabilities["test"]
}
func inputCapability(t *testing.T) *contract.Capability {
	return capability(t, `executor: {type: csv, read: {input: /files}}
input:
  properties:
    files: {items: {type: file, max_bytes: 8}, minItems: 1, maxItems: 2}
    label: {type: string, default: fixed}
  required: [files]
policy: {max_input_file_bytes: 8, max_total_input_file_bytes: 12, max_input_files: 2}`)
}
func outputCapability(t *testing.T) *contract.Capability {
	return capability(t, `executor: {type: wasm, module: ./test.wasm}
output: {data: {}, files: {}}
policy: {max_output_file_bytes: 256KiB, max_total_output_file_bytes: 512KiB}`)
}
func digest(b []byte) string { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }
func receive(t *testing.T, r *Request, pointer string, b []byte) contract.FileValue {
	t.Helper()
	st, e := r.NewFile(r.owner, pointer, "a.bin", "application/octet-stream")
	if e != nil {
		t.Fatal(e)
	}
	if n, e := st.Write(b); e != nil || n != len(b) {
		t.Fatalf("write %d %v", n, e)
	}
	if e = st.Close(); e != nil {
		t.Fatal(e)
	}
	v, e := r.Complete(r.owner, st, int64(len(b)), digest(b))
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func outputScope(t *testing.T, s *Store, ttl time.Duration) *OutputScope {
	t.Helper()
	v, e := s.Output(owner(), OutputLimits{256 << 10, 512 << 10, 4, ttl})
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func prepared(t *testing.T, s *Store, b []byte, ttl time.Duration) (*OutputScope, *contract.PreparedOutput) {
	t.Helper()
	v := outputScope(t, s, ttl)
	o, e := contract.ProcessOutput(context.Background(), outputCapability(t), contract.ProviderResult{Data: "json", Files: []contract.ProviderFile{{Name: "a.bin", MIMEType: "application/octet-stream", Reader: bytes.NewReader(b)}}}, v)
	if e != nil {
		t.Fatal(e)
	}
	return v, o
}

func TestArtifactsQuotaAtomicSharedAndNoEviction(t *testing.T) {
	s := store(t, 16)
	v, o := prepared(t, s, []byte("live"), time.Minute)
	descriptors, e := v.Retain(o.Files)
	if e != nil {
		t.Fatal(e)
	}
	if e = o.Close(); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	var reservations []*Reservation
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, e := s.Reserve(4)
			if e == nil {
				mu.Lock()
				reservations = append(reservations, r)
				mu.Unlock()
			} else {
				var typed *Error
				if !errors.As(e, &typed) || typed.Code != "LIMIT_EXCEEDED" {
					t.Errorf("reserve %v", e)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(reservations) != 3 || s.Usage() != 16 {
		t.Fatalf("reservations=%d usage=%d", len(reservations), s.Usage())
	}
	r, e := s.OpenOutput(context.Background(), owner(), descriptors[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(r)
	if e != nil || string(b) != "live" {
		t.Fatalf("live bytes %q %v", b, e)
	}
	_ = r.Close()
	for _, reservation := range reservations {
		reservation.Release()
		reservation.Release()
	}
	if s.Usage() != 4 {
		t.Fatal(s.Usage())
	}
	_, e = s.Reserve(17)
	code(t, e, "LIMIT_EXCEEDED")
	_, e = s.Reserve(-1)
	code(t, e, "VALIDATION_FAILED")
}

func TestArtifactsInputLimitsReadyAndOwner(t *testing.T) {
	s := store(t, 20)
	r, e := s.BeginInput(context.Background(), owner(), inputCapability(t), time.Time{})
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.NewFile(owner(), "/files/00", "x", "application/octet-stream")
	code(t, e, "VALIDATION_FAILED")
	_, e = r.NewFile(owner(), "/label", "x", "application/octet-stream")
	code(t, e, "VALIDATION_FAILED")
	st, e := r.NewFile(owner(), "/files/0", "x", "application/octet-stream")
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.Commit(owner(), map[string]any{"files": []any{}})
	code(t, e, "VALIDATION_FAILED")
	if _, e = st.Write([]byte("123456789")); e == nil {
		t.Fatal("leaf overrun")
	}
	if s.Usage() != 0 {
		t.Fatal("reserved rejected write")
	}
	_, e = st.Write([]byte("12345678"))
	if e != nil {
		t.Fatal(e)
	}
	if e = st.Close(); e != nil {
		t.Fatal(e)
	}
	_, e = r.Complete(owner(), st, 7, digest([]byte("12345678")))
	code(t, e, "VALIDATION_FAILED")
	v, e := r.Complete(owner(), st, 8, digest([]byte("12345678")))
	if e != nil {
		t.Fatal(e)
	}
	second, e := r.NewFile(owner(), "/files/1", "x", "application/octet-stream")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = second.Write([]byte("12345")); e == nil {
		t.Fatal("aggregate overrun")
	}
	if _, e = second.Write([]byte("1234")); e != nil {
		t.Fatal(e)
	}
	if e = second.Close(); e != nil {
		t.Fatal(e)
	}
	v2, e := r.Complete(owner(), second, 4, digest([]byte("1234")))
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.NewFile(owner(), "/files/2", "x", "application/octet-stream")
	code(t, e, "VALIDATION_FAILED")
	mutated := v
	mutated.SHA256 = digest(nil)
	_, e = r.Commit(owner(), map[string]any{"files": []any{mutated, v2}})
	code(t, e, "VALIDATION_FAILED")
	params, e := r.Commit(owner(), map[string]any{"files": []any{v, v2}})
	if e != nil || params["label"] != "fixed" {
		t.Fatalf("commit %v %v", params, e)
	}
	_, e = r.Commit(owner(), params)
	code(t, e, "CONFLICT")
	_, e = r.Open(owner(), v)
	code(t, e, "CONFLICT")
	if e = r.Start(owner(), context.Background()); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 5; i++ {
		bad := owner()
		switch i {
		case 0:
			bad.RequestID += "x"
		case 1:
			bad.CallerBinding += "x"
		case 2:
			bad.Capability += "x"
		case 3:
			bad.DefinitionHash += "x"
		case 4:
			bad.AuthGeneration += "x"
		}
		_, e = r.Open(bad, v)
		code(t, e, "PERMISSION_DENIED")
	}
	reader, e := r.Open(owner(), v)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(reader)
	if e != nil || string(b) != "12345678" {
		t.Fatalf("input %q %v", b, e)
	}
	if e = r.ReleaseStopped(owner()); e != nil {
		t.Fatal(e)
	}
	if s.Usage() != 8 {
		t.Fatalf("active reader prematurely released: %d", s.Usage())
	}
	_, e = r.Open(owner(), v)
	code(t, e, "CONFLICT")
	if e = reader.Close(); e != nil {
		t.Fatal(e)
	}
	if s.Usage() != 0 || len(s.requests) != 0 {
		t.Fatalf("cleanup usage=%d slots=%d", s.Usage(), len(s.requests))
	}
}

func TestArtifactsFixedDeadlinePendingCancelAndExecutionLifetime(t *testing.T) {
	s := store(t, 20)
	c := inputCapability(t)
	ctx, cancel := context.WithCancel(context.Background())
	r, e := s.BeginInput(ctx, owner(), c, time.Now().Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	fixed := r.Deadline()
	v := receive(t, r, "/files/0", []byte("byte"))
	if !r.Deadline().Equal(fixed) {
		t.Fatal("deadline renewed")
	}
	o2 := owner()
	o2.RequestID = "second"
	r2, e := s.BeginInput(context.Background(), o2, c, time.Now().Add(time.Minute))
	if e != nil {
		t.Fatal(e)
	}
	o3 := owner()
	o3.RequestID = "third"
	_, e = s.BeginInput(context.Background(), o3, c, time.Time{})
	code(t, e, "BUSY")
	cancel()
	if e = s.Sweep(); e != nil {
		t.Fatal(e)
	}
	if r.State() != "released" || s.Usage() != 0 {
		t.Fatal("cancel cleanup")
	}
	_ = r2.ReleaseStopped(o2)
	r, e = s.BeginInput(context.Background(), owner(), c, time.Now().Add(time.Minute))
	if e != nil {
		t.Fatal(e)
	}
	v = receive(t, r, "/files/0", []byte("keep"))
	if _, e = r.Commit(owner(), map[string]any{"files": []any{v}}); e != nil {
		t.Fatal(e)
	}
	execution, stop := context.WithCancel(context.Background())
	if e = r.Start(owner(), execution); e != nil {
		t.Fatal(e)
	}
	reader, e := r.Open(owner(), v)
	if e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	r.deadline = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if e = s.Sweep(); e != nil || s.Usage() != 4 {
		t.Fatal("executing input TTL deletion")
	}
	if e = r.Abort(owner()); e != nil {
		t.Fatal(e)
	}
	if e = s.Sweep(); e != nil || s.Usage() != 4 {
		t.Fatal("aborted worker still owns input")
	}
	stop()
	if _, e = reader.Read(make([]byte, 1)); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e = r.ReleaseStopped(owner()); e != nil || s.Usage() != 4 {
		t.Fatal("reader lease not retained")
	}
	_ = reader.Close()
	if s.Usage() != 0 {
		t.Fatal(s.Usage())
	}
}

func TestArtifactsInputOutputIndependentCopyAndExpiry(t *testing.T) {
	s := store(t, 8)
	r, e := s.BeginInput(context.Background(), owner(), inputCapability(t), time.Time{})
	if e != nil {
		t.Fatal(e)
	}
	v := receive(t, r, "/files/0", []byte("copy"))
	_, e = r.Commit(owner(), map[string]any{"files": []any{v}})
	if e != nil {
		t.Fatal(e)
	}
	_ = r.Start(owner(), context.Background())
	reader, e := r.Open(owner(), v)
	if e != nil {
		t.Fatal(e)
	}
	scope := outputScope(t, s, time.Minute)
	out, e := contract.ProcessOutput(context.Background(), outputCapability(t), contract.ProviderResult{Files: []contract.ProviderFile{{Name: v.Name, MIMEType: v.MIMEType, Reader: reader}}}, scope)
	if e != nil {
		t.Fatal(e)
	}
	descriptors, e := scope.Retain(out.Files)
	if e != nil {
		t.Fatal(e)
	}
	if s.Usage() != 8 {
		t.Fatalf("double physical storage %d", s.Usage())
	}
	_ = out.Close()
	_ = r.ReleaseStopped(owner())
	if s.Usage() != 4 {
		t.Fatal(s.Usage())
	}
	download, e := s.OpenOutput(context.Background(), owner(), descriptors[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	scope.stages[0].expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	_ = s.Sweep()
	_, e = s.OpenOutput(context.Background(), owner(), descriptors[0].ID)
	code(t, e, "NOT_FOUND")
	if s.Usage() != 4 {
		t.Fatal("expiry freed active download")
	}
	b, e := io.ReadAll(download)
	if e != nil || string(b) != "copy" || descriptors[0].SHA256 != digest(b) {
		t.Fatalf("independent output %q %v", b, e)
	}
	_ = download.Close()
	if s.Usage() != 0 {
		t.Fatal(s.Usage())
	}
}

func TestArtifactsOutputOwnerRetentionAndGeneration(t *testing.T) {
	s := store(t, 16)
	scope, out := prepared(t, s, []byte("private"), time.Minute)
	descriptors, e := scope.Retain(out.Files)
	if e != nil {
		t.Fatal(e)
	}
	_, e = scope.Retain(out.Files)
	code(t, e, "OUTPUT_INVALID")
	_ = out.Close()
	for i := 0; i < 5; i++ {
		bad := owner()
		switch i {
		case 0:
			bad.RequestID += "x"
		case 1:
			bad.CallerBinding += "x"
		case 2:
			bad.Capability += "x"
		case 3:
			bad.DefinitionHash += "x"
		case 4:
			bad.AuthGeneration += "x"
		}
		_, e = s.OpenOutput(context.Background(), bad, descriptors[0].ID)
		code(t, e, "NOT_FOUND")
	}
	reader, e := s.OpenOutput(context.Background(), owner(), descriptors[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	_ = s.RevokeGeneration(owner().AuthGeneration)
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal("old-generation reader remained usable", err)
	}
	_, e = s.OpenOutput(context.Background(), owner(), descriptors[0].ID)
	code(t, e, "NOT_FOUND")
	if s.Usage() != 7 {
		t.Fatal("revoked live read quota")
	}
	_ = reader.Close()
	if s.Usage() != 0 {
		t.Fatal(s.Usage())
	}
}

func TestArtifactsProductionStagerOutputAndFilesystem(t *testing.T) {
	for _, quota := range []int64{0, 1, 130 << 10} {
		t.Run(fmt.Sprint(quota), func(t *testing.T) {
			data := bytes.Repeat([]byte{0, 255, 13, 10}, (130<<10)/4)
			s := store(t, max(1, quota))
			scope := outputScope(t, s, time.Minute)
			out, e := contract.ProcessOutput(context.Background(), outputCapability(t), contract.ProviderResult{Files: []contract.ProviderFile{{Name: "a.bin", MIMEType: "application/octet-stream", Reader: bytes.NewReader(data)}}}, scope)
			if quota < lenInt(data) {
				var oe *contract.OutputError
				if out != nil || !errors.As(e, &oe) || oe.Code != "LIMIT_EXCEEDED" || s.Usage() != 0 {
					t.Fatalf("output quota: %v %v usage=%d", out, e, s.Usage())
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				b, e := io.ReadAll(out.Files[0].Reader)
				if e != nil || !bytes.Equal(b, data) || out.Files[0].SHA256 != digest(b) {
					t.Fatal("output integrity")
				}
				_ = out.Close()
			}
			rootPath := t.TempDir()
			if e = os.WriteFile(filepath.Join(rootPath, "a.bin"), data, 0600); e != nil {
				t.Fatal(e)
			}
			root, e := filesystem.OpenRoot(rootPath)
			if e != nil {
				t.Fatal(e)
			}
			defer root.Close()
			selector, e := filesystem.PrepareSelector(map[string]any{"name": "a.bin", "path": ".", "recursive": false, "take": int64(1), "scan_limit": int64(10), "sort": map[string]any{"by": "name", "order": "asc"}}, nil)
			if e != nil {
				t.Fatal(e)
			}
			selected, e := root.Select(context.Background(), selector, nil)
			if e != nil {
				t.Fatal(e)
			}
			snapshot, e := filesystem.SnapshotFiles(context.Background(), selected, filesystem.SnapshotLimits{MaxFileBytes: 256 << 10, MaxTotalBytes: 512 << 10, MaxFiles: 4}, "application/octet-stream", outputScope(t, s, time.Minute))
			if quota < lenInt(data) {
				var fe *filesystem.Error
				if snapshot != nil || !errors.As(e, &fe) || fe.Code != "LIMIT_EXCEEDED" || s.Usage() != 0 {
					t.Fatalf("filesystem quota %v %v", snapshot, e)
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				b, e := io.ReadAll(snapshot.Files[0].Reader)
				if e != nil || !bytes.Equal(b, data) {
					t.Fatal("snapshot bytes")
				}
				_ = snapshot.Close()
			}
			if s.Usage() != 0 {
				t.Fatal(s.Usage())
			}
		})
	}
}
func lenInt(b []byte) int64 { return int64(len(b)) }

func TestArtifactsFaultsCleanupAccountingAndCloseConcurrency(t *testing.T) {
	for _, fault := range []string{"short", "disk_full", "sync", "cleanup"} {
		t.Run(fault, func(t *testing.T) {
			s := store(t, 16)
			scope := outputScope(t, s, time.Minute)
			switch fault {
			case "short":
				s.write = func(f *os.File, p []byte) (int, error) { return f.Write(p[:2]) }
			case "disk_full":
				s.write = func(f *os.File, p []byte) (int, error) {
					n, _ := f.Write(p[:2])
					return n, errors.New("disk full private-path")
				}
			case "sync":
				s.syncFile = func(*os.File) error { return errors.New("sync private-path") }
			case "cleanup":
				s.remove = func(string) error { return errors.New("remove private-path") }
			}
			out, e := contract.ProcessOutput(context.Background(), outputCapability(t), contract.ProviderResult{Files: []contract.ProviderFile{{Name: "a.bin", MIMEType: "application/octet-stream", Reader: bytes.NewReader([]byte("abcdef"))}}}, scope)
			if fault == "cleanup" {
				if e != nil {
					t.Fatal(e)
				}
				e = out.Close()
				if e == nil || s.Usage() != 6 {
					t.Fatal("failed cleanup released quota")
				}
				_, e = s.Reserve(1)
				code(t, e, "DEPENDENCY_UNAVAILABLE")
				s.remove = s.objects.Remove
				_ = scope.stages[0].Abort()
				if s.Usage() != 0 {
					t.Fatal(s.Usage())
				}
			} else {
				if e == nil || out != nil || strings.Contains(e.Error(), "private-path") || s.Usage() != 0 {
					t.Fatalf("fault %v %v usage %d", out, e, s.Usage())
				}
			}
		})
	}
	s := store(t, 16)
	scope, out := prepared(t, s, []byte("close"), time.Minute)
	d, e := scope.Retain(out.Files)
	if e != nil {
		t.Fatal(e)
	}
	_ = out.Close()
	r, e := s.OpenOutput(context.Background(), owner(), d[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.Read(make([]byte, 1)); _ = r.Close(); _ = s.Close() }()
	}
	wg.Wait()
	if s.Usage() != 0 {
		t.Fatal(s.Usage())
	}
}

func TestArtifactsRestartRecoveryRealProcess(t *testing.T) {
	if dir := os.Getenv("ONPREST_ARTIFACTS_CHILD_DIRECTORY"); dir != "" {
		s, e := Open(Options{dir, 16, 2, 32, time.Minute})
		if e != nil {
			t.Fatal(e)
		}
		scope, e := s.Output(owner(), OutputLimits{8, 16, 2, time.Minute})
		if e != nil {
			t.Fatal(e)
		}
		st, e := scope.Begin(context.Background(), "private.bin", "application/octet-stream")
		if e != nil {
			t.Fatal(e)
		}
		_, _ = st.Write([]byte("orphan"))
		os.Exit(0)
	}
	o := options(t, 16)
	cmd := exec.Command(os.Args[0], "-test.run=^TestArtifactsRestartRecoveryRealProcess$")
	cmd.Env = append(os.Environ(), "ONPREST_ARTIFACTS_CHILD_DIRECTORY="+o.Directory)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("child %v %s", e, b)
	}
	entries, e := os.ReadDir(filepath.Join(o.Directory, "objects"))
	if e != nil || len(entries) != 1 {
		t.Fatalf("missing orphan %v %v", entries, e)
	}
	if e = os.Mkdir(filepath.Join(o.Directory, "cleanup"), 0700); e != nil {
		t.Fatal(e)
	}
	manifest := filepath.Join(o.Directory, "cleanup", "future-journal")
	if e = os.WriteFile(manifest, []byte("preserve"), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := Open(o)
	if e != nil {
		t.Fatal(e)
	}
	entries, e = os.ReadDir(filepath.Join(o.Directory, "objects"))
	if e != nil || len(entries) != 0 || s.Usage() != 0 {
		t.Fatal("restart orphan not removed")
	}
	if b, e := os.ReadFile(manifest); e != nil || string(b) != "preserve" {
		t.Fatal("journal modified")
	}
	_ = s.Close()
}

func TestArtifactsLockRecoveryUnknownAndCapacity(t *testing.T) {
	o := options(t, 16)
	s, e := Open(o)
	if e != nil {
		t.Fatal(e)
	}
	_, e = Open(o)
	code(t, e, "BUSY")
	_ = s.Close()
	unknown := filepath.Join(o.Directory, "objects", "business-file")
	if e = os.WriteFile(unknown, []byte("unchanged"), 0600); e != nil {
		t.Fatal(e)
	}
	_, e = Open(o)
	code(t, e, "PERMISSION_DENIED")
	if b, _ := os.ReadFile(unknown); string(b) != "unchanged" {
		t.Fatal("unknown deleted")
	}
	_ = os.Remove(unknown)
	for i := 0; i < 3; i++ {
		p := filepath.Join(o.Directory, "objects", fmt.Sprintf("i-%032x.spool", i))
		if e = os.WriteFile(p, nil, 0600); e != nil {
			t.Fatal(e)
		}
	}
	o.MaxObjects = 2
	_, e = Open(o)
	code(t, e, "LIMIT_EXCEEDED")
	entries, _ := os.ReadDir(filepath.Join(o.Directory, "objects"))
	if len(entries) != 3 {
		t.Fatal("partial recovery")
	}
}

func TestArtifactsStageSealIntegrityZeroAndInvalid(t *testing.T) {
	s := store(t, 8)
	scope := outputScope(t, s, time.Minute)
	st, e := scope.Begin(context.Background(), "zero.bin", "application/octet-stream")
	if e != nil {
		t.Fatal(e)
	}
	_, e = st.Finish(contract.ArtifactMetadata{})
	if e == nil {
		t.Fatal("early finish")
	}
	if e = st.Close(); e != nil {
		t.Fatal(e)
	}
	_, e = st.Write([]byte("x"))
	if e == nil {
		t.Fatal("sealed write")
	}
	reader, e := st.Finish(contract.ArtifactMetadata{Name: "zero.bin", MIMEType: "application/octet-stream", SizeBytes: 0, SHA256: digest(nil)})
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(reader)
	if e != nil || len(b) != 0 {
		t.Fatal("empty byte result")
	}
	_ = reader.Close()
	_ = st.Abort()
	if s.Usage() != 0 {
		t.Fatal(s.Usage())
	}
	_, e = scope.Begin(context.Background(), "../secret", "application/octet-stream")
	code(t, e, "VALIDATION_FAILED")
	_, e = scope.Begin(context.Background(), "x", "text/plain; charset=utf-8")
	code(t, e, "VALIDATION_FAILED")
	if _, e = s.Reserve(0); e == nil {
		t.Fatal("zero reservation")
	}
	if _, e = Open(Options{Directory: "relative"}); e == nil {
		t.Fatal("invalid options")
	}
	if _, e = s.OpenOutput(context.Background(), owner(), "../secret"); e == nil {
		t.Fatal("unknown ID")
	}
}
