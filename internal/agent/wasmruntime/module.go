// Package wasmruntime owns isolated Wasm execution and the request-scoped
// business bridge used by the production Agent provider.
package wasmruntime

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
)

const ABI = "onprest_host_v1"
const CompatibilityVersion = "2.0.0"
const Chunk = 64 << 10
const EnvelopeLimit = 32 << 20

type Limits struct {
	MemoryBytes                                            int64 `json:"memory_bytes"`
	ModuleBytes                                            int64 `json:"module_bytes"`
	Handles                                                int   `json:"handles"`
	HostCalls                                              int   `json:"host_calls"`
	JSONDepth                                              int64 `json:"json_depth"`
	JSONItems                                              int64 `json:"json_items"`
	FileBytes                                              int64 `json:"file_bytes"`
	TotalFileBytes                                         int64 `json:"total_file_bytes"`
	Files                                                  int   `json:"files"`
	ReadBytes, WriteBytes, TotalReadBytes, TotalWriteBytes int64
	FileOperations                                         int64
	HTTPRequestBytes, HTTPResponseBytes                    int64
}

func DefaultLimits() Limits {
	return Limits{MemoryBytes: 128 << 20, ModuleBytes: 32 << 20, Handles: 64, HostCalls: 8, JSONDepth: 32, JSONItems: 100000, FileBytes: 32 << 20, TotalFileBytes: 64 << 20, Files: 16, ReadBytes: 64 << 20, WriteBytes: 64 << 20, TotalReadBytes: 256 << 20, TotalWriteBytes: 256 << 20, FileOperations: 65536, HTTPRequestBytes: 32 << 20, HTTPResponseBytes: 32 << 20}
}
func (l Limits) rssLimit() (uint64, error) {
	if l.MemoryBytes < 65536 || l.MemoryBytes > 1<<32 || l.ModuleBytes < 1 || l.ModuleBytes > (math.MaxInt64-(512<<20)-2*l.MemoryBytes)/8 || l.Handles < 1 || l.HostCalls < 1 || l.JSONDepth < 1 || l.JSONDepth > math.MaxInt64-4 || l.JSONItems < 1 || l.Files > (math.MaxInt64-8)/3 || l.JSONItems > math.MaxInt64-int64(l.Files)*3-8 || l.FileBytes < 1 || l.TotalFileBytes < l.FileBytes || l.Files < 1 || l.ReadBytes < 1 || l.WriteBytes < 1 || l.TotalReadBytes < 1 || l.TotalWriteBytes < 1 || l.FileOperations < 1 || l.HTTPRequestBytes < 1 || l.HTTPResponseBytes < 1 {
		return 0, errors.New("invalid worker limits")
	}
	return uint64((512 << 20) + 2*l.MemoryBytes + 8*l.ModuleBytes), nil
}

// Module owns precisely the validated bytes; callers never receive its backing
// slice and execution never opens the module path again.
type Module struct {
	bytes    []byte
	digest   string
	metadata Metadata
}

func (m *Module) Digest() string { return m.digest }
func (m *Module) Size() int      { return len(m.bytes) }
func (m *Module) Metadata() Metadata {
	v := m.metadata
	v.Imports = append([]string{}, v.Imports...)
	return v
}

type cacheEntry struct {
	key    string
	module *Module
	bytes  int64
}
type Cache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List
	bytes   int64
}

func NewCache() *Cache { return &Cache{entries: map[string]*list.Element{}, lru: list.New()} }
func (c *Cache) get(key string) *Module {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil {
		c.lru.MoveToFront(e)
		return e.Value.(cacheEntry).module
	}
	return nil
}
func (c *Cache) put(key string, m *Module) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil {
		c.lru.MoveToFront(e)
		return
	}
	meta, _ := json.Marshal(m.metadata)
	cost := int64(len(m.bytes)) + int64(len(meta))
	if cost > 256<<20 {
		return
	}
	e := c.lru.PushFront(cacheEntry{key, m, cost})
	c.entries[key] = e
	c.bytes += cost
	for len(c.entries) > 64 || c.bytes > 256<<20 {
		e = c.lru.Back()
		v := e.Value.(cacheEntry)
		c.bytes -= v.bytes
		delete(c.entries, v.key)
		c.lru.Remove(e)
	}
}
func (c *Cache) Usage() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.bytes
}

// Prepare reads once and validates without running either a Wasm start section
// or _start. Compilation occurs in the same isolated binary as execution.
func (r *Runner) Prepare(ctx context.Context, path, expected string, l Limits) (*Module, error) {
	if _, e := l.rssLimit(); e != nil {
		return nil, e
	}
	f, e := os.Open(filepath.Clean(path))
	if e != nil {
		return nil, e
	}
	defer f.Close()
	stat, e := f.Stat()
	if e != nil || !stat.Mode().IsRegular() {
		return nil, errors.New("module is not a regular file")
	}
	if stat.Size() > l.ModuleBytes {
		return nil, errors.New("module exceeds byte limit")
	}
	b, e := io.ReadAll(io.LimitReader(f, l.ModuleBytes+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > l.ModuleBytes {
		return nil, errors.New("module exceeds byte limit")
	}
	digest := sha256.Sum256(b)
	d := hex.EncodeToString(digest[:])
	if expected != "" && expected != d {
		return nil, errors.New("module digest does not match")
	}
	key := d + ":" + r.version() + ":" + limitsKey(l)
	if r.Cache != nil {
		if m := r.Cache.get(key); m != nil {
			return m, nil
		}
	}
	m := &Module{bytes: b, digest: d}
	validation, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	metadata, e := r.validate(validation, m, l)
	if e != nil {
		return nil, e
	}
	m.metadata = metadata
	if r.Cache != nil {
		r.Cache.put(key, m)
	}
	return m, nil
}
func limitsKey(l Limits) string {
	b, _ := contract.CanonicalJSON(map[string]any{"memory": l.MemoryBytes, "module": l.ModuleBytes})
	return string(b)
}
