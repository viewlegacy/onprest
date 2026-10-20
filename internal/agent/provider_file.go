package agent

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strconv"
	"strings"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

// fileProvider shares the controller/transfer boundary for reads and writes.
// Preparation validates file definitions without acquiring runtime input storage.
type fileProvider struct {
	cap            *contract.Capability
	store          *artifacts.Store
	roots          map[string]*filesystem.Root
	paths          map[string]string
	selector       *filesystem.Selector
	afterSave      func() // package-test barrier; production does not set it
	afterInstalled func(int)
}

func prepareFileProvider(ctx context.Context, s *providers.Snapshot, store *artifacts.Store) (providers.Provider, error) {
	p := &fileProvider{cap: s.Definition(), store: store, roots: map[string]*filesystem.Root{}, paths: map[string]string{}}
	if p.cap.Executor.Type != "file" {
		return nil, errors.New("file provider needs private storage")
	}
	fail := func(e error) (providers.Provider, error) { return nil, errors.Join(e, p.Close()) }
	for _, key := range s.SourceKeys() {
		if e := ctx.Err(); e != nil {
			return fail(e)
		}
		source, _ := s.Source(key)
		root, e := filesystem.OpenRoot(source["root"].(string))
		if e != nil {
			return fail(e)
		}
		p.roots[key.Name] = root
		p.paths[key.Name] = source["root"].(string)
	}
	if writes, ok := p.cap.Executor.Settings["write"].([]any); ok {
		// Crash-safe saving needs directory sync on the spool and on every
		// destination; a platform or filesystem without it refuses the
		// capability now instead of failing, or leaving state behind, later.
		// Validation prepares without the spool; the running agent probes it.
		if store != nil {
			if e := store.ProbeDurableSaves(); e != nil {
				return fail(e)
			}
		}
		for _, raw := range writes {
			w := raw.(map[string]any)
			source := w["source"].(string)
			d, e := p.roots[source].Directory(w["path"].(string), p.paths[source])
			if e != nil {
				return fail(e)
			}
			if e = d.Sync(); e != nil {
				d.Close()
				return fail(errors.Join(errors.New("save directory cannot be synced durably"), e))
			}
			if e = d.Close(); e != nil {
				return fail(e)
			}
		}
	}
	if read, ok := p.cap.Executor.Settings["read"].(map[string]any); ok && read["written"] != true {
		selector, e := filesystem.PrepareSelector(read["selector"].(map[string]any), p.cap.Input)
		if e != nil {
			return fail(e)
		}
		p.selector = selector
	}
	return p, nil
}
func (p *fileProvider) Close() error {
	var errs []error
	for _, root := range p.roots {
		errs = append(errs, root.Close())
	}
	return errors.Join(errs...)
}
func filePointer(v any, pointer string) (any, error) {
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = x[token]
			if !ok {
				return nil, fs.ErrInvalid
			}
		case []any:
			i, e := strconv.Atoi(token)
			if e != nil || i < 0 || i >= len(x) {
				return nil, fs.ErrInvalid
			}
			v = x[i]
		default:
			return nil, fs.ErrInvalid
		}
	}
	return v, nil
}
func providerFileFailure(e error) error {
	var a *artifacts.Error
	var f *filesystem.Error
	if errors.As(e, &a) {
		return &providers.Failure{Code: a.Code, Cause: e}
	}
	if errors.As(e, &f) {
		return &providers.Failure{Code: f.Code, Cause: e}
	}
	if errors.Is(e, fs.ErrPermission) {
		return &providers.Failure{Code: "PERMISSION_DENIED", Cause: e}
	}
	if errors.Is(e, fs.ErrNotExist) {
		return &providers.Failure{Code: "NOT_FOUND", Cause: e}
	}
	return &providers.Failure{Code: "EXECUTION_FAILED", Cause: e}
}
func (p *fileProvider) Execute(ctx context.Context, in map[string]any, effects *providers.Effects) (result contract.ProviderResult, err error) {
	result = contract.ProviderResult{Files: []contract.ProviderFile{}}
	writes, _ := p.cap.Executor.Settings["write"].([]any)
	files := []artifacts.SaveFile{}
	dirs := []*filesystem.Directory{}
	published := false
	defer func() {
		for _, d := range dirs {
			err = errors.Join(err, d.Close())
		}
		if len(writes) > 0 && !published {
			effects.NotStarted()
		}
		if err != nil {
			err = providerFileFailure(err)
		}
	}()
	for _, raw := range writes {
		w := raw.(map[string]any)
		value, e := filePointer(in, w["input"].(string))
		if e != nil {
			return result, e
		}
		values := []contract.FileValue{}
		switch v := value.(type) {
		case contract.FileValue:
			values = append(values, v)
		case []any:
			for _, raw := range v {
				f, ok := raw.(contract.FileValue)
				if !ok {
					return result, fs.ErrInvalid
				}
				values = append(values, f)
			}
		default:
			return result, fs.ErrInvalid
		}
		source := w["source"].(string)
		d, e := p.roots[source].Directory(w["path"].(string), p.paths[source])
		if e != nil {
			return result, e
		}
		dirs = append(dirs, d)
		for _, v := range values {
			name, _ := w["name"].(string)
			if name == "" {
				name = v.Name
			}
			if e := filesystem.SaveName(name); e != nil {
				return result, e
			}
			files = append(files, artifacts.SaveFile{Directory: d, Name: name, Overwrite: w["overwrite"].(bool), Value: v})
		}
	}
	if len(files) > 0 {
		var effect *providers.Effect
		confirmed := 0
		store := p.store
		if store == nil {
			store, _ = ctx.Value(artifactStoreKey{}).(*artifacts.Store)
		}
		if store == nil {
			return result, errors.New("private storage is unavailable")
		}
		err = store.Save(ctx, files[0].Value.RequestID, files, OpenInput, func(confirmedFile bool) {
			published = true
			if effect == nil {
				effect = effects.Begin()
			}
			if confirmedFile {
				confirmed++
				if p.afterInstalled != nil {
					p.afterInstalled(confirmed)
				}
			}
			if confirmed == len(files) {
				effect.Committed()
			}
		})
		if err != nil {
			return result, err
		}
		if p.afterSave != nil {
			p.afterSave()
		}
	} else {
		effects.NoChange()
	}
	read, _ := p.cap.Executor.Settings["read"].(map[string]any)
	if read == nil {
		return result, nil
	}
	media := "application/octet-stream"
	if v, ok := p.cap.Output.Files["mime_type"].(string); ok {
		media = v
	}
	if read["written"] == true {
		for _, v := range files {
			r, e := OpenInput(ctx, v.Value)
			if e != nil {
				return result, e
			}
			m := v.Value.MIMEType
			if _, ok := p.cap.Output.Files["mime_type"]; ok {
				m = media
			}
			size := v.Value.SizeBytes
			result.Files = append(result.Files, contract.ProviderFile{Name: v.Name, MIMEType: m, Reader: r, SizeBytes: &size, SHA256: v.Value.SHA256})
		}
	} else {
		selected, e := p.roots[read["source"].(string)].Select(ctx, p.selector, in)
		if e != nil {
			return result, e
		}
		for _, v := range selected {
			r, e := v.Open(ctx)
			if e != nil {
				return result, e
			}
			size := v.SizeBytes
			result.Files = append(result.Files, contract.ProviderFile{Name: v.Name, MIMEType: media, Reader: r, SizeBytes: &size})
		}
	}
	return result, nil
}

var _ io.Closer = (*fileProvider)(nil)
