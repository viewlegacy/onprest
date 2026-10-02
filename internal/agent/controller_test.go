package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

type controllerFixture struct {
	fn            func(context.Context, map[string]any, *providers.Effects) (contract.ProviderResult, error)
	calls, closed atomic.Int32
}

func (p *controllerFixture) Execute(ctx context.Context, in map[string]any, e *providers.Effects) (contract.ProviderResult, error) {
	p.calls.Add(1)
	return p.fn(ctx, in, e)
}
func (p *controllerFixture) Close() error { p.closed.Add(1); return nil }
func controllerConfig(t *testing.T, extra string) *contract.Config {
	t.Helper()
	body := "schema_version: 1\ngateway: {url: 'ws://localhost/ws/agent', agent_private_key: '" + base64.RawURLEncoding.EncodeToString(make([]byte, 64)) + "'}\nruntime: {max_concurrent_requests: 1}\nsources:\n  http:\n    api: {base_url: 'https://example.internal', allowed_methods: [GET]}\ncapabilities:\n  run:\n    executor: {type: http, source: api, method: GET, path: /data, response: {kind: json}}\n    input: {properties: {n: {type: integer, default: 7}}, required: [n]}\n    output: {data: {}}\n"
	if extra == "files" {
		body = strings.Replace(body, "response: {kind: json}", "response: {kind: file}", 1)
		body = strings.Replace(body, "output: {data: {}}", "output: {files: {}}", 1)
		extra = ""
	}
	c, e := contract.Compile([]byte(body + extra))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func controller(t *testing.T, c *contract.Config, p *controllerFixture, stage contract.ArtifactStager, sink *DiagnosticSink) (*ExecutionController, *providers.Prepared) {
	t.Helper()
	r, e := providers.NewRegistry(map[string]providers.Factory{"http": func(context.Context, *providers.Snapshot) (providers.Provider, error) { return p, nil }})
	if e != nil {
		t.Fatal(e)
	}
	prepared, e := r.Prepare(context.Background(), c, filepath.Join(t.TempDir(), "config.yaml"), providers.ResolveOptions{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := prepared.Close(); e != nil {
			t.Error(e)
		}
	})
	ctl, e := NewExecutionController(prepared, stage, sink)
	if e != nil {
		t.Fatal(e)
	}
	return ctl, prepared
}
func request() ExecutionRequest {
	return ExecutionRequest{ID: "request", Capability: "run", Params: map[string]any{}}
}

func TestControllerValidationAdmissionAndImmutableExecution(t *testing.T) {
	var logs bytes.Buffer
	sink, _ := NewDiagnosticSink(&logs, nil, "info")
	c := controllerConfig(t, "")
	p := &controllerFixture{fn: func(ctx context.Context, in map[string]any, e *providers.Effects) (contract.ProviderResult, error) {
		if in["n"] != int64(7) {
			t.Fatal("default not applied", in)
		}
		e.NoChange()
		return contract.ProviderResult{Data: map[string]any{"n": in["n"]}}, nil
	}}
	ctl, prepared := controller(t, c, p, nil, sink)
	snap, _ := prepared.Snapshot("run")
	hash := snap.DefinitionHash()
	c.Capabilities["run"].Input.Properties["n"].Default = int64(9)
	c.Capabilities["run"].Policy["timeout"] = "1ns"
	for _, tc := range []struct {
		name, code string
		mutate     func(*ExecutionRequest)
	}{
		{"unknown", "NOT_FOUND", func(r *ExecutionRequest) { r.Capability = "missing" }},
		{"invalid_type", "VALIDATION_FAILED", func(r *ExecutionRequest) { r.Params["n"] = "private secret" }},
		{"unknown_param", "VALIDATION_FAILED", func(r *ExecutionRequest) { r.Params["private secret"] = 1 }},
		{"stale_definition", "CONFLICT", func(r *ExecutionRequest) { r.DefinitionHash = strings.Repeat("f", 64) }},
		{"expired_deadline", "EXECUTION_TIMEOUT", func(r *ExecutionRequest) { r.Deadline = time.Now().Add(-time.Second) }},
		{"invalid_id", "INVALID_REQUEST", func(r *ExecutionRequest) { r.ID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request()
			tc.mutate(&r)
			out, fail := ctl.Execute(context.Background(), r)
			if out != nil || fail == nil || fail.Code != tc.code || fail.Outcome != "not_started" {
				t.Fatal(out, fail)
			}
			if p.calls.Load() != 0 {
				t.Fatal("rejection invoked provider")
			}
			b, _ := json.Marshal(fail)
			if strings.Contains(string(b), "private secret") {
				t.Fatal("validation detail leaked")
			}
		})
	}
	r := request()
	r.DefinitionHash = hash
	out, fail := ctl.Execute(context.Background(), r)
	if fail != nil || out.Data.(map[string]any)["n"] != int64(7) {
		t.Fatal(out, fail)
	}
	if e := out.Close(); e != nil {
		t.Fatal(e)
	}
	if snap.DefinitionHash() != hash {
		t.Fatal("execution changed hash")
	}
	if logs.Len() != 0 {
		t.Fatal("success logged private input")
	}
}

func TestControllerOutcomeSafeFailuresAndReaderOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		observe    func(*providers.Effects)
	}{
		{"unobserved", "unknown", func(e *providers.Effects) {}},
		{"read", "no_change", func(e *providers.Effects) { e.NoChange() }},
		{"rollback", "rolled_back", func(e *providers.Effects) { e.Begin().RolledBack() }},
		{"commit", "committed", func(e *providers.Effects) { e.Begin().Committed() }},
		{"mixed", "unknown", func(e *providers.Effects) { e.Begin().RolledBack(); e.Begin().Committed() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readers := []*controllerReader{{Reader: strings.NewReader("secret body")}, {Reader: strings.NewReader("another secret body"), closeErr: errors.New("secret close error")}}
			p := &controllerFixture{fn: func(ctx context.Context, in map[string]any, e *providers.Effects) (contract.ProviderResult, error) {
				tc.observe(e)
				return contract.ProviderResult{Files: []contract.ProviderFile{{Reader: readers[0]}, {Reader: readers[1]}}}, errors.New("private upstream error")
			}}
			var logs bytes.Buffer
			sink, _ := NewDiagnosticSink(&logs, nil, "error")
			ctl, _ := controller(t, controllerConfig(t, ""), p, nil, sink)
			out, fail := ctl.Execute(context.Background(), request())
			if out != nil || fail == nil || fail.Code != "EXECUTION_FAILED" || fail.Outcome != tc.want {
				t.Fatal(out, fail)
			}
			for _, r := range readers {
				if r.closed.Load() != 1 || r.reads.Load() != 0 {
					t.Fatal("error result reader ownership", r.closed.Load(), r.reads.Load())
				}
			}
			b, _ := json.Marshal(fail)
			if bytes.Contains(b, []byte("private")) || bytes.Contains(b, []byte("secret")) {
				t.Fatal("raw detail escaped")
			}
			if !bytes.Contains(logs.Bytes(), []byte("private upstream error")) || !bytes.Contains(logs.Bytes(), []byte("secret close error")) {
				t.Fatal("local diagnosis missing", logs.String())
			}
		})
	}
	for _, tc := range []struct {
		name, code, reason string
		cause              error
	}{
		{"system_category", "PERMISSION_DENIED", "SANDBOX_DENIED", &providers.Failure{Code: "PERMISSION_DENIED", Reason: "SANDBOX_DENIED", Cause: errors.New("private")}},
		{"invalid_reason", "EXECUTION_FAILED", "", &providers.Failure{Code: "PERMISSION_DENIED", Reason: "private\nreason"}},
		{"invalid_code", "EXECUTION_FAILED", "", &providers.Failure{Code: "PRIVATE_INTERNAL"}},
		{"application_deferred", "EXECUTION_FAILED", "", &providers.Failure{Code: "APP_VALID"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &controllerFixture{fn: func(context.Context, map[string]any, *providers.Effects) (contract.ProviderResult, error) {
				return contract.ProviderResult{}, tc.cause
			}}
			ctl, _ := controller(t, controllerConfig(t, ""), p, nil, nil)
			_, fail := ctl.Execute(context.Background(), request())
			if fail.Code != tc.code || fail.Reason != tc.reason {
				t.Fatal(fail)
			}
			if e := fail.Validate(); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestControllerSingleDeadlineCoversProviderAndPublication(t *testing.T) {
	for _, stage := range []string{"provider", "publication", "outer_deadline", "cancel_before_start", "prepared_close"} {
		t.Run(stage, func(t *testing.T) {
			c := controllerConfig(t, "")
			if stage == "publication" {
				c = controllerConfig(t, "files")
			}
			c.Capabilities["run"].Policy["timeout"] = "35ms"
			if stage == "outer_deadline" {
				c.Capabilities["run"].Policy["timeout"] = "5s"
			}
			p := &controllerFixture{}
			entered := make(chan struct{})
			reader := &controllerReader{}
			p.fn = func(ctx context.Context, in map[string]any, e *providers.Effects) (contract.ProviderResult, error) {
				close(entered)
				e.Begin().Committed()
				if stage == "publication" {
					reader.Reader = &contextReader{ctx: ctx}
					return contract.ProviderResult{Files: []contract.ProviderFile{{Name: "file.bin", MIMEType: "application/octet-stream", Reader: reader}}}, nil
				}
				<-ctx.Done()
				return contract.ProviderResult{}, errors.New("private timeout detail")
			}
			stager := &controllerStager{}
			ctl, prepared := controller(t, c, p, stager, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := request()
			if stage == "outer_deadline" {
				r.Deadline = time.Now().Add(15 * time.Millisecond)
			}
			if stage == "cancel_before_start" {
				cancel()
			}
			type answer struct {
				out *contract.PreparedOutput
				err *contract.ErrorValue
			}
			done := make(chan answer, 1)
			go func() { out, err := ctl.Execute(ctx, r); done <- answer{out, err} }()
			if stage == "prepared_close" {
				<-entered
				go prepared.Close()
			}
			result := <-done
			if result.out != nil || result.err == nil || result.err.Code != "EXECUTION_TIMEOUT" {
				t.Fatal(result)
			}
			if stage == "cancel_before_start" {
				if p.calls.Load() != 0 || result.err.Outcome != "not_started" {
					t.Fatal("canceled request started")
				}
			} else if result.err.Outcome != "committed" {
				t.Fatal("committed effect lost", result.err)
			}
			if stage == "publication" {
				if reader.closed.Load() != 1 || len(stager.stages) != 1 || stager.stages[0].aborted.Load() != 1 {
					t.Fatal("timeout output cleanup")
				}
			}
		})
	}
}

func TestControllerBusyRateLimitAndCloseConcurrency(t *testing.T) {
	c := controllerConfig(t, "")
	started := make(chan struct{})
	release := make(chan struct{})
	p := &controllerFixture{fn: func(ctx context.Context, in map[string]any, e *providers.Effects) (contract.ProviderResult, error) {
		close(started)
		select {
		case <-release:
			e.NoChange()
			return contract.ProviderResult{Data: 1}, nil
		case <-ctx.Done():
			return contract.ProviderResult{}, ctx.Err()
		}
	}}
	ctl, prepared := controller(t, c, p, nil, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, fail := ctl.Execute(context.Background(), request())
		if fail != nil {
			t.Error(fail)
		} else {
			_ = out.Close()
		}
	}()
	<-started
	_, fail := ctl.Execute(context.Background(), request())
	if fail == nil || fail.Code != "BUSY" || fail.Outcome != "not_started" || p.calls.Load() != 1 {
		t.Fatal(fail)
	}
	close(release)
	<-done
	if e := prepared.Close(); e != nil || p.closed.Load() != 1 {
		t.Fatal(e, p.closed.Load())
	}
	c = controllerConfig(t, "")
	c.Capabilities["run"].Policy["rate_limit"] = map[string]any{"requests": int64(1), "burst": int64(1), "per": "1h"}
	p = &controllerFixture{fn: func(ctx context.Context, in map[string]any, e *providers.Effects) (contract.ProviderResult, error) {
		e.NoChange()
		return contract.ProviderResult{Data: 1}, nil
	}}
	ctl, _ = controller(t, c, p, nil, nil)
	bad := request()
	bad.Params = map[string]any{"n": "wrong"}
	if _, fail = ctl.Execute(context.Background(), bad); fail.Code != "VALIDATION_FAILED" {
		t.Fatal(fail)
	}
	out, fail := ctl.Execute(context.Background(), request())
	if fail != nil {
		t.Fatal("invalid input consumed token", fail)
	}
	_ = out.Close()
	_, fail = ctl.Execute(context.Background(), request())
	if fail.Code != "RATE_LIMITED" || fail.Outcome != "not_started" || p.calls.Load() != 1 {
		t.Fatal(fail)
	}
}

func TestControllerOutputFailurePreservesCommitAndAbortsAllStages(t *testing.T) {
	c := controllerConfig(t, "files")
	readers := []*controllerReader{{Reader: strings.NewReader("one")}, {Reader: &failingReader{}}}
	stager := &controllerStager{}
	p := &controllerFixture{fn: func(ctx context.Context, in map[string]any, e *providers.Effects) (contract.ProviderResult, error) {
		e.Begin().Committed()
		return contract.ProviderResult{Files: []contract.ProviderFile{{Name: "one.bin", MIMEType: "application/octet-stream", Reader: readers[0]}, {Name: "two.bin", MIMEType: "application/octet-stream", Reader: readers[1]}}}, nil
	}}
	ctl, _ := controller(t, c, p, stager, nil)
	out, fail := ctl.Execute(context.Background(), request())
	if out != nil || fail.Code != "OUTPUT_INVALID" || fail.Outcome != "committed" {
		t.Fatal(out, fail)
	}
	for _, r := range readers {
		if r.closed.Load() != 1 {
			t.Fatal("reader not closed")
		}
	}
	if len(stager.stages) != 2 {
		t.Fatal(stager.stages)
	}
	for _, stage := range stager.stages {
		if stage.aborted.Load() != 1 {
			t.Fatal("stage not aborted")
		}
	}
}

func TestControllerPanicAndDiagnosticLevelDoNotChangeOutcome(t *testing.T) {
	for _, level := range []string{"debug", "error"} {
		t.Run(level, func(t *testing.T) {
			var logs bytes.Buffer
			sink, _ := NewDiagnosticSink(&logs, nil, level)
			p := &controllerFixture{fn: func(ctx context.Context, in map[string]any, e *providers.Effects) (contract.ProviderResult, error) {
				e.Begin().Committed()
				panic("private panic detail")
			}}
			ctl, _ := controller(t, controllerConfig(t, ""), p, nil, sink)
			_, fail := ctl.Execute(context.Background(), request())
			if fail.Code != "EXECUTION_FAILED" || fail.Outcome != "committed" || !strings.Contains(logs.String(), "private panic detail") {
				t.Fatal(fail, logs.String())
			}
		})
	}
}

type controllerReader struct {
	io.Reader
	reads, closed atomic.Int32
	closeErr      error
}

func (r *controllerReader) Read(b []byte) (int, error) { r.reads.Add(1); return r.Reader.Read(b) }
func (r *controllerReader) Close() error               { r.closed.Add(1); return r.closeErr }

type contextReader struct{ ctx context.Context }

func (r *contextReader) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, r.ctx.Err() }

type failingReader struct{}

func (*failingReader) Read([]byte) (int, error) {
	return 0, errors.New("private second reader failure")
}

type controllerStager struct {
	mu     sync.Mutex
	stages []*controllerStage
}

func (s *controllerStager) Begin(context.Context, string, string) (contract.ArtifactStage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stage := &controllerStage{}
	s.stages = append(s.stages, stage)
	return stage, nil
}

type controllerStage struct {
	bytes.Buffer
	aborted atomic.Int32
}

func (s *controllerStage) Close() error { return nil }
func (s *controllerStage) Finish(contract.ArtifactMetadata) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.Bytes())), nil
}
func (s *controllerStage) Abort() error { s.aborted.Add(1); return nil }
