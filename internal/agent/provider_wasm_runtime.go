package agent

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/agent/wasmruntime"
	"github.com/viewlegacy/onprest/internal/contract"
)

type wasmProvider struct {
	snapshot *providers.Snapshot
	module   *wasmruntime.Module
	limits   wasmruntime.Limits
	binary   string
}

func wasmLimits(c *contract.Capability) wasmruntime.Limits {
	l := wasmruntime.DefaultLimits()
	m := c.Executor.Settings["limits"].(map[string]any)
	l.MemoryBytes = m["memory_bytes"].(int64)
	l.ModuleBytes = m["max_module_bytes"].(int64)
	l.Handles = int(m["max_handles"].(int64))
	l.HostCalls = int(m["max_host_calls"].(int64))
	l.ReadBytes = m["max_file_read_bytes"].(int64)
	l.WriteBytes = m["max_file_write_bytes"].(int64)
	l.TotalReadBytes = m["max_total_file_read_bytes"].(int64)
	l.TotalWriteBytes = m["max_total_file_write_bytes"].(int64)
	l.FileOperations = m["max_file_operations"].(int64)
	l.HTTPRequestBytes = m["max_http_request_bytes"].(int64)
	l.HTTPResponseBytes = m["max_http_response_bytes"].(int64)
	l.JSONDepth = c.Policy["max_json_depth"].(int64)
	l.JSONItems = c.Policy["max_json_items"].(int64)
	if c.Output.Files != nil {
		l.FileBytes = c.Policy["max_output_file_bytes"].(int64)
		l.TotalFileBytes = c.Policy["max_total_output_file_bytes"].(int64)
		l.Files = int(c.Policy["max_output_files"].(int64))
	}
	return l
}
func prepareWasmModules(ctx context.Context, cfg *contract.Config, path string) (map[string]*wasmruntime.Module, providers.ResolveOptions, error) {
	modules := map[string]*wasmruntime.Module{}
	options := providers.ResolveOptions{ModuleDigests: map[string]string{}, Environment: providers.ProcessEnvironment()}
	absolute, e := filepath.Abs(path)
	if e != nil {
		return nil, options, e
	}
	r := &wasmruntime.Runner{Cache: wasmruntime.NewCache()}
	for name, c := range cfg.Capabilities {
		if c.Executor.Type != "wasm" {
			continue
		}
		path := c.Executor.Settings["module"].(string)
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(absolute), path)
		}
		expected, _ := c.Executor.Settings["sha256"].(string)
		m, e := r.Prepare(ctx, path, expected, wasmLimits(c))
		if e != nil {
			return nil, options, e
		}
		modules[name] = m
		options.ModuleDigests[name] = m.Digest()
	}
	return modules, options, nil
}
func (p *wasmProvider) Close() error { return nil }
func (p *wasmProvider) Execute(context.Context, map[string]any, *providers.Effects) (contract.ProviderResult, error) {
	return contract.ProviderResult{}, errors.New("Wasm requires the publication controller")
}
func (p *wasmProvider) ExecutePublication(ctx context.Context, input map[string]any, effects *providers.Effects, publish func(contract.ProviderResult) (*contract.PreparedOutput, error)) (*contract.PreparedOutput, error) {
	defer effects.NoChange()
	store, _ := ctx.Value(artifactStoreKey{}).(*artifacts.Store)
	if store == nil {
		return nil, &providers.Failure{Code: "EXECUTION_FAILED", Cause: errors.New("Wasm requires request artifact store")}
	}
	bridge := &wasmruntime.Bridge{Base: p.snapshot.BaseDirectory(), Sandbox: p.snapshot.Definition().Executor.Settings["sandbox"].(map[string]any), Environment: p.snapshot.Environment(), Input: map[string]contract.FileValue{}, OpenInput: OpenInput, Effects: effects}
	var convert func(any) any
	convert = func(v any) any {
		switch x := v.(type) {
		case contract.FileValue:
			bridge.Input[x.ID] = x
			return map[string]any{"__onprest_input_file_v1": map[string]any{"file_id": x.ID, "name": x.Name, "mime_type": x.MIMEType, "size_bytes": x.SizeBytes, "sha256": x.SHA256}}
		case map[string]any:
			m := map[string]any{}
			for k, v := range x {
				m[k] = convert(v)
			}
			return m
		case []any:
			a := make([]any, len(x))
			for i, v := range x {
				a[i] = convert(v)
			}
			return a
		}
		return v
	}
	params := convert(input).(map[string]any)
	r := &wasmruntime.Runner{Binary: p.binary, Store: store, Bridge: bridge, Diagnostic: func(level, event, message, detail string) {
		if sink, _ := ctx.Value(wasmDiagnosticKey{}).(*DiagnosticSink); sink != nil {
			_ = sink.Write(DiagnosticRecord{Level: level, Source: "wasm", Event: event, Message: message, Detail: detail, Capability: p.snapshot.Definition().Name, RequestID: requestIDFromContext(ctx)})
		}
	}}
	out, err := r.ExecutePublication(ctx, p.module, params, p.snapshot.Definition(), p.limits, publish)
	if err != nil {
		var w *wasmruntime.Error
		if errors.As(err, &w) {
			if w.Message != "" {
				public, err := providers.NewPublicTerminal(w.Code, w.Message)
				if err != nil {
					return nil, err
				}
				return nil, public
			}
			return nil, &providers.Failure{Code: w.Code, Cause: errors.New(w.Detail)}
		}
		return nil, err
	}
	if effects.Outcome() == "unknown" {
		effects.NoChange()
	}
	return out, nil
}

type wasmDiagnosticKey struct{}
type wasmRequestIDKey struct{}

func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(wasmRequestIDKey{}).(string)
	return v
}
