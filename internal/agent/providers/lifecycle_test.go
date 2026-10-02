package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/viewlegacy/onprest/internal/contract"
)

type fixtureProvider struct {
	execute    func(context.Context, map[string]any, *Effects) (contract.ProviderResult, error)
	closed     atomic.Int32
	closeErr   error
	closePanic bool
}

func (p *fixtureProvider) Execute(ctx context.Context, in map[string]any, e *Effects) (contract.ProviderResult, error) {
	if p.execute != nil {
		return p.execute(ctx, in, e)
	}
	e.NoChange()
	return contract.ProviderResult{}, nil
}
func (p *fixtureProvider) Close() error {
	p.closed.Add(1)
	if p.closePanic {
		panic("private close panic")
	}
	return p.closeErr
}

func TestProviderRegistryDBLessMultiSourceAndNoImplicitRegistration(t *testing.T) {
	body := "sources:\n  folders:\n    same: {root: ./read, access: read}\n    a: {root: ./a, access: read_write}\n    b: {root: ./b, access: read_write}\n  http:\n    same: {base_url: 'https://example.internal', allowed_methods: [GET]}\ncapabilities:\n  copy:\n    input: {properties: {file: {type: file}}, required: [file]}\n    executor:\n      type: file\n      write: [{input: /file, source: a, name: a.bin}, {input: /file, source: b, name: b.bin}]\n      read: {source: same, selector: {name: report.pdf}}\n    output: {files: {}}\n  fetch:\n    executor: {type: http, source: same, method: GET, path: /data, response: {kind: json}}\n    output: {data: {}}\n"
	c := config(t, body)
	path := filepath.Join(t.TempDir(), "agent.yaml")
	empty, _ := NewRegistry(nil)
	if p, e := empty.Prepare(context.Background(), c, path, ResolveOptions{}); e == nil || p != nil {
		t.Fatal("unsupported provider accepted")
	}
	if _, e := os.Stat(filepath.Join(filepath.Dir(path), "identity")); !os.IsNotExist(e) {
		t.Fatal("unsupported prepare touched identity", e)
	}
	var made []*fixtureProvider
	factory := func(ctx context.Context, s *Snapshot) (Provider, error) {
		p := &fixtureProvider{}
		made = append(made, p)
		return p, nil
	}
	registrations := map[string]Factory{"file": factory, "http": factory}
	r, e := NewRegistry(registrations)
	if e != nil {
		t.Fatal(e)
	}
	delete(registrations, "file")
	p, e := r.Prepare(context.Background(), c, path, ResolveOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	copy, _ := p.Snapshot("copy")
	if !reflect.DeepEqual(copy.SourceKeys(), []SourceKey{{"folders", "a"}, {"folders", "b"}, {"folders", "same"}}) {
		t.Fatal(copy.SourceKeys())
	}
	fetch, _ := p.Snapshot("fetch")
	if !reflect.DeepEqual(fetch.SourceKeys(), []SourceKey{{"http", "same"}}) {
		t.Fatal(fetch.SourceKeys())
	}
	if len(p.Names()) != 2 {
		t.Fatal(p.Names())
	}
	if _, ok := p.Snapshot("missing"); ok {
		t.Fatal("unknown advertised")
	}
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	for _, made := range made {
		if made.closed.Load() != 1 {
			t.Fatal("not exactly once")
		}
	}
	if len(p.Names()) != 0 {
		t.Fatal("closed names advertised")
	}
}

func TestProviderPrepareFailureCancellationAndCleanup(t *testing.T) {
	body := "capabilities:\n  a: {executor: {type: wasm, module: a.wasm}}\n  b: {executor: {type: wasm, module: b.wasm}}\n"
	for _, fault := range []string{"failure", "resolve_failure", "nil", "typed_nil", "cancel", "panic", "close_failure", "close_panic"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var made []*fixtureProvider
			r, _ := NewRegistry(map[string]Factory{"wasm": func(ctx context.Context, s *Snapshot) (Provider, error) {
				p := &fixtureProvider{}
				if fault == "close_failure" {
					p.closeErr = errors.New("private secret close detail")
				}
				if fault == "close_panic" {
					p.closePanic = true
				}
				if s.Definition().Name == "b" {
					switch fault {
					case "failure", "close_failure", "close_panic":
						made = append(made, p)
						return p, errors.New("private secret prepare detail")
					case "nil":
						return nil, nil
					case "typed_nil":
						return (*fixtureProvider)(nil), nil
					case "cancel":
						made = append(made, p)
						cancel()
						return p, nil
					case "panic":
						panic("private secret panic detail")
					}
				}
				made = append(made, p)
				return p, nil
			}})
			options := ResolveOptions{ModuleDigests: map[string]string{"a": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "b": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}
			if fault == "resolve_failure" {
				delete(options.ModuleDigests, "b")
			}
			p, e := r.Prepare(ctx, config(t, body), filepath.Join(t.TempDir(), "config.yaml"), options)
			if p != nil || e == nil {
				t.Fatal("failure accepted")
			}
			if contains := strings.Contains(e.Error(), "private secret"); contains {
				t.Fatal("private detail exposed", e)
			}
			for _, p := range made {
				if p.closed.Load() != 1 {
					t.Fatal("partial resources not closed exactly once")
				}
			}
		})
	}
}

func TestProviderMutationAndConcurrentCloseCancelsLease(t *testing.T) {
	c := config(t, databaseConfig)
	path := filepath.Join(t.TempDir(), "config.yaml")
	var captured *Snapshot
	pvd := &fixtureProvider{}
	r, _ := NewRegistry(map[string]Factory{"db": func(ctx context.Context, s *Snapshot) (Provider, error) {
		captured = s
		s.cap.Policy["timeout"] = "1s"
		s.sources[SourceKey{"databases", "main"}]["password"] = "provider mutation"
		return pvd, nil
	}})
	p, e := r.Prepare(context.Background(), c, path, ResolveOptions{})
	if e != nil {
		t.Fatal(e)
	}
	first, _ := p.Snapshot("read")
	if first.Definition().Policy["timeout"] != "5s" {
		t.Fatal("provider modified definition")
	}
	db, _ := first.Source(SourceKey{"databases", "main"})
	if db["password"] != "secret" {
		t.Fatal("provider modified source")
	}
	c.Capabilities["read"].Policy["timeout"] = "2s"
	c.Sources.Databases["main"]["password"] = "caller mutation"
	captured.cap.Input.Properties["new"] = &contract.Schema{}
	lease, e := p.Acquire("read")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := lease.Context(context.Background())
	defer cancel()
	if lease.Snapshot().DefinitionHash() != first.DefinitionHash() {
		t.Fatal("prepared hash changed")
	}
	runtime := p.Runtime()
	runtime["max_concurrent_requests"] = int64(1)
	if p.Runtime()["max_concurrent_requests"] != int64(16) {
		t.Fatal("runtime mutable")
	}
	var wg sync.WaitGroup
	closed := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := p.Close(); e != nil {
				t.Error(e)
			}
		}()
	}
	go func() { wg.Wait(); close(closed) }()
	<-ctx.Done()
	if pvd.closed.Load() != 0 {
		t.Fatal("closed active provider before lease release")
	}
	if _, e := p.Acquire("read"); e == nil {
		t.Fatal("close admitted new lease")
	}
	lease.Release()
	lease.Release()
	<-closed
	if pvd.closed.Load() != 1 {
		t.Fatal("concurrent close not once")
	}
}

func TestObservedEffectsDoNotInferOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		observe    func(*Effects)
	}{
		{"not_started", "not_started", func(e *Effects) {}},
		{"unobserved_execution", "unknown", func(e *Effects) { e.Start() }},
		{"no_change", "no_change", func(e *Effects) { e.Start(); e.NoChange() }},
		{"pending_effect", "unknown", func(e *Effects) { e.Start(); e.NoChange(); e.Begin() }},
		{"commit", "committed", func(e *Effects) { e.Start(); e.Begin().Committed() }},
		{"rollback", "rolled_back", func(e *Effects) { e.Start(); e.Begin().RolledBack() }},
		{"mixed", "unknown", func(e *Effects) { e.Start(); e.Begin().Committed(); e.Begin().RolledBack() }},
		{"partial", "unknown", func(e *Effects) { e.Start(); e.Begin().Committed(); e.Begin() }},
		{"contradictory", "unknown", func(e *Effects) { e.Start(); v := e.Begin(); v.Committed(); v.RolledBack(); v.Committed() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Effects{}
			tc.observe(e)
			if e.Outcome() != tc.want {
				t.Fatalf("%s != %s", e.Outcome(), tc.want)
			}
		})
	}
}
