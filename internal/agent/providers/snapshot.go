// Package providers owns the internal prepared execution contract. It contains
// no built-in executor registrations; runtime migration is a separate change.
package providers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/contract"
)

const secretLimit = 1 << 20

// ResolveOptions contains trusted preparation material, never request params.
// ModuleDigests must come from the module validator's immutable byte snapshot.
// This layer neither loads nor validates Wasm modules. A missing digest refuses
// preparation rather than hashing a mutable path or an optional expected hash.
type ResolveOptions struct {
	ModuleDigests map[string]string
	Environment   map[string]string
}

type SourceKey struct{ Group, Name string }

// Snapshot retains private definition and execution material. Accessors return
// copies; raw resolved source/secret data is exclusively for local executors.
type Snapshot struct {
	cap         *contract.Capability
	sources     map[SourceKey]map[string]any
	base        string
	environment map[string]string
	hash        string
}

func (s *Snapshot) Definition() *contract.Capability { return s.cap.Clone() }
func (s *Snapshot) DefinitionHash() string           { return s.hash }
func (s *Snapshot) BaseDirectory() string            { return s.base }
func (s *Snapshot) Source(key SourceKey) (map[string]any, bool) {
	v, ok := s.sources[key]
	if !ok {
		return nil, false
	}
	return copyMap(v), true
}
func (s *Snapshot) SourceKeys() []SourceKey {
	keys := make([]SourceKey, 0, len(s.sources))
	for key := range s.sources {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Group != keys[j].Group {
			return keys[i].Group < keys[j].Group
		}
		return keys[i].Name < keys[j].Name
	})
	return keys
}
func (s *Snapshot) Environment() map[string]string {
	m := map[string]string{}
	for k, v := range s.environment {
		m[k] = v
	}
	return m
}
func (s *Snapshot) clone() *Snapshot {
	v := &Snapshot{cap: s.cap.Clone(), sources: map[SourceKey]map[string]any{}, base: s.base, hash: s.hash, environment: s.Environment()}
	for k, m := range s.sources {
		v.sources[k] = copyMap(m)
	}
	return v
}

// Resolve snapshots only sources used by the named capability. No source is
// opened and no business code is invoked. The caller supplies a compiled AST.
func Resolve(ctx context.Context, cfg *contract.Config, configPath, name string, opts ResolveOptions) (*Snapshot, error) {
	if opts.Environment == nil {
		opts.Environment = ProcessEnvironment()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cap, ok := cfg.Capabilities[name]
	if !ok {
		return nil, errors.New("capability is not defined")
	}
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return nil, errors.New("invalid configuration path")
	}
	base := filepath.Dir(absolute)
	s := &Snapshot{cap: cap.Clone(), sources: map[SourceKey]map[string]any{}, base: base, environment: map[string]string{}}
	keys := referencedSources(s.cap)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var group map[string]map[string]any
		switch key.Group {
		case "databases":
			group = cfg.Sources.Databases
		case "folders":
			group = cfg.Sources.Folders
		case "http":
			group = cfg.Sources.HTTP
		}
		raw, ok := group[key.Name]
		if !ok {
			return nil, errors.New("referenced source is not defined")
		}
		resolved, err := resolveSource(ctx, base, key.Group, raw)
		if err != nil {
			return nil, err
		}
		s.sources[key] = resolved
	}
	definition := s.cap.Normalized()
	// Descriptions, annotations and author examples are public presentation
	// metadata; the binding covers the adopted execution-definition fields.
	delete(definition, "description")
	delete(definition, "annotations")
	delete(definition, "examples")
	if s.cap.Executor.Type == "wasm" {
		digest := opts.ModuleDigests[name]
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
			return nil, errors.New("validated module snapshot is required")
		}
		x := definition["executor"].(map[string]any)
		delete(x, "module")
		delete(x, "sha256")
		x["module_digest"] = digest
		x["base_directory"] = base
		s.cap.Executor.Settings["module"] = resolvePath(base, s.cap.Executor.Settings["module"].(string))
		s.cap.Executor.Settings["module_digest"] = digest
		delete(s.cap.Executor.Settings, "sha256")
		sb := s.cap.Executor.Settings["sandbox"].(map[string]any)
		if sb["enabled"] == true {
			p := sb["permissions"].(map[string]any)
			for _, raw := range p["filesystem"].([]any) {
				f := raw.(map[string]any)
				f["root"] = resolvePath(base, f["root"].(string))
			}
			for _, k := range p["environment"].([]string) {
				if v, ok := opts.Environment[k]; ok {
					s.environment[k] = v
				}
			}
			x["sandbox"] = copyMap(sb)
		} else {
			for k, v := range opts.Environment {
				s.environment[k] = v
			}
		}
		s.environment["PWD"] = filesystem.WASIPath(base)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity := resolvePath(base, cfg.Runtime["identity_dir"].(string))
	var roots []string
	for key, source := range s.sources {
		if key.Group == "folders" {
			roots = append(roots, source["root"].(string))
		}
	}
	if s.cap.Executor.Type == "wasm" {
		if sb := s.cap.Executor.Settings["sandbox"].(map[string]any); sb["enabled"] == true {
			for _, raw := range sb["permissions"].(map[string]any)["filesystem"].([]any) {
				roots = append(roots, raw.(map[string]any)["root"].(string))
			}
		}
	}
	if err := checkPrivateDirectories(roots, []string{resolvePath(base, cfg.Runtime["spool_dir"].(string)), identity}); err != nil {
		return nil, err
	}
	key, err := identityKey(identity)
	if err != nil {
		return nil, errors.New("configuration identity could not be prepared")
	}
	defer clear(key)
	materials := map[string]any{}
	for _, ref := range s.SourceKeys() {
		m := copyMap(s.sources[ref])
		for _, field := range []string{"password", "credential"} {
			if v, ok := m[field].(string); ok {
				m[field] = secretTag(key, name, ref.Group+"."+ref.Name+"."+field, []byte(v))
			}
		}
		if t, ok := m["tls"].(map[string]any); ok {
			if v, ok := t["key_bytes"].([]byte); ok {
				t["key_bytes"] = secretTag(key, name, ref.Group+"."+ref.Name+".tls.key", v)
			}
			digestBytes(t, "ca_bytes")
			digestBytes(t, "cert_bytes")
		}
		if h, ok := m["headers"].(map[string]any); ok {
			if v, ok := h["authorization"].(string); ok {
				h["authorization"] = secretTag(key, name, ref.Group+"."+ref.Name+".headers.authorization", []byte(v))
			}
		}
		g, ok := materials[ref.Group].(map[string]any)
		if !ok {
			g = map[string]any{}
			materials[ref.Group] = g
		}
		g[ref.Name] = m
	}
	definition["sources"] = materials
	// Bind even secret-free definitions to this identity generation. Losing the
	// identity must invalidate every old binding, not just password-bearing ones.
	definition["identity_binding"] = secretTag(key, name, "identity", nil)
	if s.cap.Executor.Type == "wasm" {
		env := map[string]any{}
		for k, v := range s.environment {
			env[k] = secretTag(key, name, "environment."+k, []byte(v))
		}
		definition["environment"] = env
	}
	b, err := contract.CanonicalJSON(definition)
	if err != nil {
		return nil, errors.New("prepared definition is invalid")
	}
	h := sha256.Sum256(b)
	s.hash = hex.EncodeToString(h[:])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func referencedSources(c *contract.Capability) []SourceKey {
	keys := map[SourceKey]bool{}
	add := func(group string, m map[string]any) {
		if name, ok := m["source"].(string); ok {
			keys[SourceKey{group, name}] = true
		}
	}
	switch c.Executor.Type {
	case "db":
		add("databases", c.Executor.Settings)
	case "http":
		add("http", c.Executor.Settings)
	case "csv", "file":
		if read, ok := c.Executor.Settings["read"].(map[string]any); ok {
			add("folders", read)
		}
		if writes, ok := c.Executor.Settings["write"].([]any); ok {
			for _, w := range writes {
				add("folders", w.(map[string]any))
			}
		}
	}
	out := []SourceKey{}
	for k := range keys {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func resolveSource(ctx context.Context, base, group string, raw map[string]any) (map[string]any, error) {
	m := copyMap(raw)
	if group == "folders" {
		m["root"] = resolvePath(base, m["root"].(string))
		return m, nil
	}
	field := "password"
	if group == "http" {
		field = "credential"
	}
	if _, a := m[field]; a {
		if _, b := m[field+"_file"]; b {
			return nil, errors.New("secret and secret file are exclusive")
		}
	}
	if path, ok := m[field+"_file"].(string); ok {
		b, err := readProtected(ctx, resolvePath(base, path))
		if err != nil {
			return nil, errors.New("configured secret file could not be read")
		}
		m[field] = string(b)
		clear(b)
		delete(m, field+"_file")
	}
	if secret, ok := m[field].(string); ok {
		if !utf8.ValidString(secret) || group == "http" && (secret == "" || strings.ContainsAny(secret, "\r\n\x00") || strings.HasPrefix(secret, "Bearer ")) {
			return nil, errors.New("configured secret is invalid")
		}
	}
	if group == "databases" {
		t := m["tls"].(map[string]any)
		for _, field := range []string{"ca", "cert", "key"} {
			if path, ok := t[field+"_file"].(string); ok {
				var b []byte
				var err error
				if field == "key" {
					b, err = readProtected(ctx, resolvePath(base, path))
				} else {
					b, err = readRegular(ctx, resolvePath(base, path), false)
				}
				if err != nil {
					return nil, errors.New("configured TLS material could not be read")
				}
				t[field+"_bytes"] = b
				delete(t, field+"_file")
			}
		}
	}
	if h, ok := m["headers"].(map[string]any); ok {
		if _, auth := h["authorization"]; auth {
			if _, token := m["credential"]; token {
				return nil, errors.New("credential and Authorization header are exclusive")
			}
		}
	}
	return m, nil
}
func resolvePath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}
func digestBytes(m map[string]any, k string) {
	if b, ok := m[k].([]byte); ok {
		h := sha256.Sum256(b)
		m[k] = hex.EncodeToString(h[:])
	}
}
func secretTag(key []byte, name, field string, value []byte) string {
	h := hmac.New(sha256.New, key)
	for _, b := range [][]byte{[]byte("onprest-config-secret-v1"), []byte(name), []byte(field), value} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		_, _ = h.Write(n[:])
		_, _ = h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func readProtected(ctx context.Context, path string) ([]byte, error) {
	return readRegular(ctx, path, true)
}
func readRegular(ctx context.Context, path string, private bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := openRegular(path, private)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, secretLimit+1))
	if err != nil || len(b) > secretLimit {
		clear(b)
		return nil, errors.New("configuration material exceeds limit or cannot be read")
	}
	if err := ctx.Err(); err != nil {
		clear(b)
		return nil, err
	}
	return b, nil
}
func copyMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = copyValue(v)
	}
	return out
}
func copyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return copyMap(x)
	case []any:
		a := make([]any, len(x))
		for i, v := range x {
			a[i] = copyValue(v)
		}
		return a
	case []string:
		return append([]string{}, x...)
	case []byte:
		return append([]byte{}, x...)
	default:
		return v
	}
}

// ProcessEnvironment captures process environment once. Only Wasm resolution
// uses it; strict mode selects registered names, OFF retains OS-user access.
func ProcessEnvironment() map[string]string {
	out := map[string]string{}
	for _, v := range os.Environ() {
		k, val, ok := strings.Cut(v, "=")
		if ok {
			out[k] = val
		}
	}
	return out
}

// ReadProtectedConfiguration applies the same finite protected-file boundary to
// the Agent signing key as to source secrets; values are never trimmed.
func ReadProtectedConfiguration(ctx context.Context, path string) ([]byte, error) {
	return readProtected(ctx, path)
}
