package providers

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"

	"github.com/viewlegacy/onprest/internal/contract"
)

// Provider must honor context, use only its prepared snapshot, and support
// concurrent Execute. Close is called once after all execution leases end.
// Execute must stop request-owned business I/O before returning, transferring
// every returned reader to the controller even when it also returns an error.
// Even a nonnil Provider returned with a prepare error transfers ownership.
type Provider interface {
	Execute(context.Context, map[string]any, *Effects) (contract.ProviderResult, error)
	Close() error
}

// PublicationProvider lets a transactional provider complete public validation
// before committing, with the controller retaining ownership of publication.
type PublicationProvider interface {
	ExecutePublication(context.Context, map[string]any, *Effects, func(contract.ProviderResult) (*contract.PreparedOutput, error)) (*contract.PreparedOutput, error)
}
type Factory func(context.Context, *Snapshot) (Provider, error)
type Registry struct{ factories map[string]Factory }

// NewRegistry makes registrations explicit and immutable. There are no default
// providers; later packages register only their completed implementations.
func NewRegistry(factories map[string]Factory) (*Registry, error) {
	r := &Registry{factories: map[string]Factory{}}
	for kind, f := range factories {
		if f == nil || (kind != "db" && kind != "csv" && kind != "http" && kind != "file" && kind != "wasm") {
			return nil, errors.New("invalid provider registration")
		}
		r.factories[kind] = f
	}
	return r, nil
}

type entry struct {
	snapshot *Snapshot
	provider Provider
}
type Prepared struct {
	mu        sync.Mutex
	entries   map[string]entry
	runtime   map[string]any
	order     []string
	closing   bool
	ctx       context.Context
	cancel    context.CancelFunc
	active    sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

func (r *Registry) Prepare(ctx context.Context, cfg *contract.Config, path string, opts ResolveOptions) (*Prepared, error) {
	if opts.Environment == nil {
		opts.Environment = ProcessEnvironment()
	}
	names := make([]string, 0, len(cfg.Capabilities))
	for name := range cfg.Capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	// Refuse unsupported configurations before resolving secrets or creating the
	// identity directory. Unsupported definitions never become available metadata.
	for _, name := range names {
		if r.factories[cfg.Capabilities[name].Executor.Type] == nil {
			return nil, errors.New("executor provider is not available")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := &Prepared{entries: map[string]entry{}, runtime: copyMap(cfg.Runtime)}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	fail := func(err error) (*Prepared, error) {
		if closeErr := p.Close(); closeErr != nil {
			return nil, errors.New("executor preparation and cleanup failed")
		}
		return nil, err
	}
	for _, name := range names {
		s, err := Resolve(ctx, cfg, path, name, opts)
		if err != nil {
			return fail(err)
		}
		provider, err := prepareProvider(ctx, r.factories[s.cap.Executor.Type], s.clone())
		if !nilProvider(provider) {
			p.entries[name] = entry{s, provider}
			p.order = append(p.order, name)
		}
		if err != nil {
			return fail(errors.New("executor preparation failed"))
		}
		if nilProvider(provider) {
			return fail(errors.New("executor preparation returned no provider"))
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
	}
	return p, nil
}
func nilProvider(p Provider) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func prepareProvider(ctx context.Context, f Factory, s *Snapshot) (p Provider, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("executor preparation failed")
		}
	}()
	return f(ctx, s)
}

func (p *Prepared) Names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return []string{}
	}
	return append([]string{}, p.order...)
}
func (p *Prepared) Runtime() map[string]any { return copyMap(p.runtime) }
func (p *Prepared) Snapshot(name string) (*Snapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[name]
	if !ok || p.closing {
		return nil, false
	}
	return e.snapshot.clone(), true
}

// Lease extends provider ownership through input validation and publication.
// Release must follow every Acquire, including validation/admission failures.
type Lease struct {
	entry   entry
	parent  context.Context
	release func()
	once    sync.Once
}

func (p *Prepared) Acquire(name string) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return nil, errors.New("prepared execution is closed")
	}
	e, ok := p.entries[name]
	if !ok {
		return nil, errors.New("capability is not available")
	}
	p.active.Add(1)
	return &Lease{entry: e, parent: p.ctx, release: p.active.Done}, nil
}
func (l *Lease) Release()            { l.once.Do(l.release) }
func (l *Lease) Snapshot() *Snapshot { return l.entry.snapshot.clone() }
func (l *Lease) Context(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(l.parent, cancel)
	return ctx, func() { stop(); cancel() }
}
func (l *Lease) ExecutePublication(ctx context.Context, input map[string]any, effects *Effects, publish func(contract.ProviderResult) (*contract.PreparedOutput, error)) (*contract.PreparedOutput, error, bool) {
	p, ok := l.entry.provider.(PublicationProvider)
	if !ok {
		return nil, nil, false
	}
	out, err := p.ExecutePublication(ctx, input, effects, publish)
	return out, err, true
}
func (l *Lease) Execute(ctx context.Context, input map[string]any, effects *Effects) (contract.ProviderResult, error) {
	return l.entry.provider.Execute(ctx, input, effects)
}
func (p *Prepared) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closing = true
		p.cancel()
		p.mu.Unlock()
		p.active.Wait()
		var errs []error
		for i := len(p.order) - 1; i >= 0; i-- {
			errs = append(errs, closeProvider(p.entries[p.order[i]].provider))
		}
		p.closeErr = errors.Join(errs...)
	})
	return p.closeErr
}
func closeProvider(p Provider) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("executor cleanup failed")
		}
	}()
	return p.Close()
}

// Effects records observed business operations independently of annotations,
// executor.effects declarations, logging level, error code, or cancellation.
// Separate effect handles prevent one DB rollback from hiding another effect.
type Effects struct {
	mu                sync.Mutex
	started, noChange bool
	operations        []*Effect
}
type Effect struct {
	owner *Effects
	state string
}

func (e *Effects) Start()    { e.mu.Lock(); e.started = true; e.mu.Unlock() }
func (e *Effects) NoChange() { e.mu.Lock(); e.noChange = true; e.mu.Unlock() }

// NotStarted records proven absence of any business publication in a provider
// that prepared private temps. It cannot erase an observed effect.
func (e *Effects) NotStarted() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.operations) == 0 {
		e.started = false
	}
}
func (e *Effects) Begin() *Effect {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := &Effect{owner: e, state: "unknown"}
	e.operations = append(e.operations, v)
	return v
}
func (e *Effect) Committed()  { e.finish("committed") }
func (e *Effect) RolledBack() { e.finish("rolled_back") }
func (e *Effect) finish(state string) {
	e.owner.mu.Lock()
	defer e.owner.mu.Unlock()
	if e.state == "unknown" {
		e.state = state
	} else if e.state != state {
		e.state = "uncertain"
	}
}
func (e *Effects) Outcome() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return "not_started"
	}
	if len(e.operations) == 0 {
		if e.noChange {
			return "no_change"
		}
		return "unknown"
	}
	state := e.operations[0].state
	if state != "committed" && state != "rolled_back" {
		return "unknown"
	}
	for _, v := range e.operations[1:] {
		if v.state != state {
			return "unknown"
		}
	}
	return state
}
