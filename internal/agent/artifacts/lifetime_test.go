package artifacts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
)

func TestArtifactsInputDeadlineClampingAndReadyExpiry(t *testing.T) {
	s := store(t, 16)
	s.options.TransferTimeout = 20 * time.Millisecond
	c := inputCapability(t)
	before := time.Now()
	r, e := s.BeginInput(context.Background(), owner(), c, before.Add(time.Hour))
	after := time.Now()
	if e != nil {
		t.Fatal(e)
	}
	if r.Deadline().Before(before.Add(s.options.TransferTimeout)) || r.Deadline().After(after.Add(s.options.TransferTimeout)) {
		t.Fatal("Agent limit did not clamp")
	}
	if d, ok := r.ctx.Deadline(); !ok || !d.Equal(r.Deadline()) {
		t.Fatal("input context did not use the clamped deadline")
	}
	// Control deadline arrival independently of real disk creation/write/sync.
	// BeginInput's clamp and timer wiring are checked above; Sweep's fixed
	// deadline branch is checked below with an uncanceled ready context.
	s.mu.Lock()
	r.cancel()
	r.ctx, r.cancel = context.WithCancel(context.Background())
	s.mu.Unlock()
	v := receive(t, r, "/files/0", []byte("expire"))
	if _, e = r.Commit(owner(), map[string]any{"files": []any{v}}); e != nil {
		t.Fatal(e)
	}
	st := r.files[v.Pointer]
	if r.State() != "ready" || r.ctx.Err() != nil || s.Usage() != 6 || len(s.requests) != 1 || len(s.stages) != 1 {
		t.Fatal("ready input did not retain its file, quota and slot")
	}
	s.mu.Lock()
	r.deadline = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if e = s.Sweep(); e != nil || r.State() != "released" || s.Usage() != 0 || len(s.requests) != 0 || len(s.stages) != 0 || r.ctx.Err() != context.Canceled {
		t.Fatalf("ready expiry %v %s %d %d %d %v", e, r.State(), s.Usage(), len(s.requests), len(s.stages), r.ctx.Err())
	}
	if _, e = s.objects.Stat(st.path); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("expired input file still exists: %v", e)
	}
	_, e = s.BeginInput(context.Background(), owner(), c, time.Now().Add(-time.Second))
	code(t, e, "EXECUTION_TIMEOUT")
}

func TestArtifactsInputStageAbortInvalidatesWholeRequest(t *testing.T) {
	for _, phase := range []string{"receiving", "ready", "executing", "cleanup failure"} {
		t.Run(phase, func(t *testing.T) {
			s := store(t, 16)
			r, e := s.BeginInput(context.Background(), owner(), inputCapability(t), time.Time{})
			if e != nil {
				t.Fatal(e)
			}
			first := receive(t, r, "/files/0", []byte("first"))
			second := receive(t, r, "/files/1", []byte("second"))
			st := r.files[first.Pointer]
			if phase != "receiving" {
				if _, e = r.Commit(owner(), map[string]any{"files": []any{first, second}}); e != nil {
					t.Fatal(e)
				}
			}
			var reader *Reader
			var execution context.Context
			if phase == "executing" {
				if e = r.Start(owner(), context.Background()); e != nil {
					t.Fatal(e)
				}
				execution, e = r.Context(owner())
				if e != nil {
					t.Fatal(e)
				}
				reader, e = r.Open(owner(), first)
				if e != nil {
					t.Fatal(e)
				}
				defer reader.Close()
			}
			if phase == "cleanup failure" {
				s.remove = func(string) error { return errors.New("private input cleanup failed") }
			}
			e = st.Abort()
			if (e != nil) != (phase == "cleanup failure") {
				t.Fatalf("Abort %v", e)
			}
			// Dispatch runs the worker/provider only after Start accepts every
			// input. A saved stage must not leave a runnable ready request.
			workerRuns, providerRuns := 0, 0
			if e = r.Start(owner(), context.Background()); e == nil {
				workerRuns++
				providerRuns++
			}
			if e == nil || workerRuns != 0 || providerRuns != 0 {
				t.Fatal("aborted input allowed business execution")
			}
			assertObjects := func(want int, bytes int64, slots int) {
				t.Helper()
				dir, e := s.objects.Open(".")
				if e != nil {
					t.Fatal(e)
				}
				entries, e := dir.ReadDir(-1)
				_ = dir.Close()
				if e != nil || len(entries) != want || s.Usage() != bytes || len(s.requests) != slots {
					t.Fatalf("objects=%d bytes=%d slots=%d err=%v", len(entries), s.Usage(), len(s.requests), e)
				}
			}
			if phase == "executing" {
				if r.State() != "aborted" || execution.Err() != context.Canceled {
					t.Fatal("direct input Abort did not cancel worker")
				}
				assertObjects(2, 11, 1)
				if e = s.Sweep(); e != nil {
					t.Fatal(e)
				}
				assertObjects(2, 11, 1)
				if _, e = reader.Read(make([]byte, 1)); !errors.Is(e, context.Canceled) {
					t.Fatal("reader did not share worker cancellation")
				}
				if e = r.ReleaseStopped(owner()); e != nil {
					t.Fatal(e)
				}
				assertObjects(1, 5, 1)
				if e = reader.Close(); e != nil {
					t.Fatal(e)
				}
			} else if phase == "cleanup failure" {
				assertObjects(2, 11, 1)
				if !s.poisoned {
					t.Fatal("failed cleanup did not stop new quota reservations")
				}
				s.remove = s.objects.Remove
				if e = st.Abort(); e != nil {
					t.Fatal(e)
				}
			}
			assertObjects(0, 0, 0)
			if r.State() != "released" {
				t.Fatal(r.State())
			}
			if phase != "cleanup failure" {
				replacement, e := s.BeginInput(context.Background(), owner(), inputCapability(t), time.Time{})
				if e != nil {
					t.Fatal(e)
				}
				if e = st.Abort(); e != nil || s.requests[owner().RequestID] != replacement {
					t.Fatal("repeated Abort removed the replacement request slot")
				}
				if e = replacement.ReleaseStopped(owner()); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestArtifactsInputLeafAndCountBoundsZeroAndDuplicate(t *testing.T) {
	s := store(t, 16)
	c := capability(t, `executor: {type: csv, read: {input: /files}}
input:
  properties: {files: {items: {type: file, max_bytes: 2, extensions: [bin], mime_types: [application/octet-stream]}, minItems: 1, maxItems: 3}}
  required: [files]
policy: {max_input_file_bytes: 8, max_total_input_file_bytes: 12, max_input_files: 2}`)
	r, e := s.BeginInput(context.Background(), owner(), c, time.Time{})
	if e != nil {
		t.Fatal(e)
	}
	st, e := r.NewFile(owner(), "/files/0", "a.bin", "application/octet-stream")
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.NewFile(owner(), "/files/0", "b.bin", "application/octet-stream")
	code(t, e, "CONFLICT")
	_, e = st.Write([]byte("123"))
	code(t, e, "LIMIT_EXCEEDED")
	if s.Usage() != 0 {
		t.Fatal("leaf budget not enforced before I/O")
	}
	if e = st.Close(); e != nil {
		t.Fatal(e)
	}
	v, e := r.Complete(owner(), st, 0, digest(nil))
	if e != nil || v.SizeBytes != 0 {
		t.Fatal("empty input")
	}
	v2 := receive(t, r, "/files/1", []byte("12"))
	_, e = r.NewFile(owner(), "/files/2", "c.bin", "application/octet-stream")
	code(t, e, "LIMIT_EXCEEDED")
	_, e = r.Commit(owner(), map[string]any{"files": []any{v2, v}})
	code(t, e, "VALIDATION_FAILED")
	if _, e = r.Commit(owner(), map[string]any{"files": []any{v, v2}}); e != nil {
		t.Fatal(e)
	}
	_ = r.ReleaseStopped(owner())
	if s.Usage() != 0 {
		t.Fatal(s.Usage())
	}
}

func TestArtifactsRetentionAllFilesValidatedAndNoPartialResult(t *testing.T) {
	s := store(t, 16)
	scope := outputScope(t, s, time.Minute)
	out, e := contract.ProcessOutput(context.Background(), outputCapability(t), contract.ProviderResult{Files: []contract.ProviderFile{{Name: "first.bin", MIMEType: "application/octet-stream", Reader: bytes.NewReader([]byte("first"))}, {Name: "last.bin", MIMEType: "application/octet-stream", Reader: &lateFailure{}}}}, scope)
	if e == nil || out != nil || s.Usage() != 0 {
		t.Fatalf("partial result %v %v %d", out, e, s.Usage())
	}
	scope, out = prepared(t, s, []byte("validated"), time.Minute)
	_, e = scope.Retain(nil)
	code(t, e, "OUTPUT_INVALID")
	tampered := append([]contract.OutputArtifact{}, out.Files...)
	tampered[0].SizeBytes++
	_, e = scope.Retain(tampered)
	code(t, e, "OUTPUT_INVALID")
	if _, e = scope.Retain(out.Files); e != nil {
		t.Fatal(e)
	}
	_ = out.Close()
}

type lateFailure struct{ done bool }

func (r *lateFailure) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("private late reader failure")
	}
	r.done = true
	return copy(p, []byte("part")), nil
}

func TestArtifactsCancellationLeaseAndStoreCloseFailureAccounting(t *testing.T) {
	s := store(t, 16)
	ctx, cancel := context.WithCancel(context.Background())
	scope := outputScope(t, s, time.Minute)
	st, e := scope.Begin(ctx, "cancel.bin", "application/octet-stream")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = st.Write([]byte("cancel"))
	cancel()
	if e = st.Close(); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e = st.Finish(contract.ArtifactMetadata{Name: "cancel.bin", MIMEType: "application/octet-stream", SizeBytes: 6, SHA256: digest([]byte("cancel"))}); e == nil {
		t.Fatal("cancelled stage published")
	}
	if e = st.Abort(); e != nil || s.Usage() != 0 {
		t.Fatal("cancel accounting")
	}
	scope, out := prepared(t, s, []byte("retained"), time.Minute)
	descriptors, e := scope.Retain(out.Files)
	if e != nil {
		t.Fatal(e)
	}
	_ = out.Close()
	reader, e := s.OpenOutput(context.Background(), owner(), descriptors[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	s.remove = func(string) error { return errors.New("cleanup disk failed private-path") }
	if e = s.Close(); e == nil || s.Usage() != 8 {
		t.Fatalf("shutdown hid orphan usage %v %d", e, s.Usage())
	}
	if e = s.Close(); e == nil {
		t.Fatal("repeated Close hid the cleanup failure")
	}
	if _, e = reader.Read(make([]byte, 1)); !errors.Is(e, os.ErrClosed) {
		t.Fatal("shutdown reader not closed")
	}
	s.remove = s.objects.Remove
	// The store has released its lock after shutdown and reports the stranded
	// usage. A new instance validates and recovers that exact ephemeral namespace.
	reopened, e := Open(s.options)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if reopened.Usage() != 0 {
		t.Fatal("restart quota not reset after confirmed recovery")
	}
}

func TestArtifactsProductionGeneratedCSVAndScopeLimit(t *testing.T) {
	s := store(t, 64)
	c := capability(t, `executor: {type: wasm, module: ./test.wasm}
output: {items: {properties: {id: {type: integer}, label: {type: string}}}, files: {format: csv, filename: data.csv}}`)
	scope := outputScope(t, s, time.Minute)
	out, e := contract.ProcessOutput(context.Background(), c, contract.ProviderResult{Data: []any{map[string]any{"id": int64(1), "label": "=formula", "private": "excluded"}}}, scope)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(out.Files[0].Reader)
	if e != nil || string(b) != "id,label\r\n1,'=formula\r\n" || out.Files[0].SHA256 != digest(b) {
		t.Fatalf("CSV %q %v", b, e)
	}
	_ = out.Close()
	if s.Usage() != 0 {
		t.Fatal(s.Usage())
	}
	scope, e = s.Output(owner(), OutputLimits{8, 8, 1, time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	st, e := scope.Begin(context.Background(), "one", "application/octet-stream")
	if e != nil {
		t.Fatal(e)
	}
	_, e = scope.Begin(context.Background(), "two", "application/octet-stream")
	code(t, e, "LIMIT_EXCEEDED")
	_ = st.Abort()
}

func TestArtifactsOutputSurvivesExecutionContextAndReadersBounded(t *testing.T) {
	s := store(t, 16)
	ctx, cancel := context.WithCancel(context.Background())
	scope := outputScope(t, s, time.Minute)
	out, e := contract.ProcessOutput(ctx, outputCapability(t), contract.ProviderResult{Files: []contract.ProviderFile{{Name: "out.bin", MIMEType: "application/octet-stream", Reader: bytes.NewReader([]byte("after"))}}}, scope)
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	d, e := scope.Retain(out.Files)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(out.Files[0].Reader)
	if e != nil || string(b) != "after" {
		t.Fatal("execution cancelled completed bytes")
	}
	_ = out.Close()
	s.options.MaxObjects = 2
	r1, e := s.OpenOutput(context.Background(), owner(), d[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	r2, e := s.OpenOutput(context.Background(), owner(), d[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.OpenOutput(context.Background(), owner(), d[0].ID)
	code(t, e, "BUSY")
	_ = r1.Close()
	r3, e := s.OpenOutput(context.Background(), owner(), d[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	_ = r2.Close()
	_ = r3.Close()
}

func TestArtifactsOptionalZeroFileUsesInputPhase(t *testing.T) {
	s := store(t, 8)
	c := capability(t, `executor: {type: wasm, module: ./test.wasm}
input: {properties: {file: {type: file}, files: {items: {type: file}}}}`)
	r, e := s.BeginInput(context.Background(), owner(), c, time.Time{})
	if e != nil {
		t.Fatal(e)
	}
	if r.State() != "receiving" {
		t.Fatal(r.State())
	}
	params, e := r.Commit(owner(), map[string]any{"files": []any{}})
	if e != nil || len(params) != 1 || r.State() != "ready" {
		t.Fatalf("empty %v %v %s", params, e, r.State())
	}
	if e = r.Start(owner(), context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = r.ReleaseStopped(owner()); e != nil || len(s.requests) != 0 || s.Usage() != 0 {
		t.Fatal("zero-file cleanup")
	}
}

func TestArtifactsRevokeGenerationCancelsWorkerWithoutPrematureCleanup(t *testing.T) {
	s := store(t, 8)
	r, e := s.BeginInput(context.Background(), owner(), inputCapability(t), time.Time{})
	if e != nil {
		t.Fatal(e)
	}
	v := receive(t, r, "/files/0", []byte("input"))
	if _, e = r.Commit(owner(), map[string]any{"files": []any{v}}); e != nil {
		t.Fatal(e)
	}
	if e = r.Start(owner(), context.Background()); e != nil {
		t.Fatal(e)
	}
	worker, e := r.Context(owner())
	if e != nil {
		t.Fatal(e)
	}
	reader, e := r.Open(owner(), v)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.RevokeGeneration(owner().AuthGeneration); e != nil {
		t.Fatal(e)
	}
	if worker.Err() != context.Canceled {
		t.Fatal("worker cancellation missing")
	}
	if _, e = reader.Read(make([]byte, 1)); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if s.Usage() != 5 {
		t.Fatal("worker input prematurely removed")
	}
	_ = r.ReleaseStopped(owner())
	if s.Usage() != 5 {
		t.Fatal("live lease prematurely freed")
	}
	_ = reader.Close()
	if s.Usage() != 0 {
		t.Fatal(s.Usage())
	}
}

func TestArtifactsJoinedCleanupDetailsStayPrivate(t *testing.T) {
	raw := errors.New("secret host path")
	wrapped := safe(errors.Join(fail("LIMIT_EXCEEDED", nil), raw))
	if strings.Contains(wrapped.Error(), "secret") || !errors.Is(wrapped, raw) {
		t.Fatal("joined local cause escaped summary")
	}
	var output *contract.OutputError
	if !errors.As(wrapped, &output) || output.Code != "LIMIT_EXCEEDED" {
		t.Fatal("quota code lost")
	}
}
