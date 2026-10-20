package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
)

const fixtureHeader = `schema_version: 1
service: {title: filesystem test, version: "1"}
gateway: {url: wss://gateway.example/ws/agent, agent_private_key_file: ./unused.key}
sources: {folders: {reports: {root: ./reports, access: read}}}
capabilities:
  read:
    executor:
      type: file
      read:
        source: reports
        selector: `

func selector(t *testing.T, config, input string) *Selector {
	t.Helper()
	c, e := contract.Compile([]byte(fixtureHeader + config + "\n    output: {files: {}}\n" + input))
	if e != nil {
		t.Fatal(e)
	}
	cap := c.Capabilities["read"]
	normalized := cap.Executor.Settings["read"].(map[string]any)["selector"].(map[string]any)
	s, e := PrepareSelector(normalized, cap.Input)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func root(t *testing.T) (*Root, string) {
	t.Helper()
	p := t.TempDir()
	r, e := OpenRoot(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := r.Close(); e != nil {
			t.Error(e)
		}
	})
	return r, p
}
func put(t *testing.T, root, relative string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(relative))
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, data, 0600); e != nil {
		t.Fatal(e)
	}
}
func code(t *testing.T, err error, want string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("error %v, want code %s", err, want)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}
func paths(items []Selection) []string {
	out := []string{}
	for _, item := range items {
		out = append(out, item.RelativePath)
	}
	return out
}
func choose(t *testing.T, r *Root, s *Selector) []Selection {
	t.Helper()
	selected, e := r.Select(context.Background(), s, nil)
	if e != nil {
		t.Fatal(e)
	}
	return selected
}
func TestFilesystemSelectorSortTieAndRecursion(t *testing.T) {
	r, p := root(t)
	for _, file := range []struct {
		name  string
		size  int
		mtime int64
	}{{"a/same.bin", 1, 1}, {"b/same.bin", 1, 1}, {"z.bin", 3, 3}, {"B.bin", 2, 2}, {"é.bin", 2, 2}} {
		put(t, p, file.name, bytes.Repeat([]byte("x"), file.size))
		tm := time.Unix(file.mtime, 0)
		if e := os.Chtimes(filepath.Join(p, filepath.FromSlash(file.name)), tm, tm); e != nil {
			t.Fatal(e)
		}
	}
	for _, tc := range []struct {
		by, order string
		want      []string
	}{
		{"name", "asc", []string{"B.bin", "a/same.bin", "b/same.bin", "z.bin", "é.bin"}},
		{"name", "desc", []string{"é.bin", "z.bin", "a/same.bin", "b/same.bin", "B.bin"}},
		{"size", "asc", []string{"a/same.bin", "b/same.bin", "B.bin", "é.bin", "z.bin"}},
		{"size", "desc", []string{"z.bin", "B.bin", "é.bin", "a/same.bin", "b/same.bin"}},
		{"modified_at", "asc", []string{"a/same.bin", "b/same.bin", "B.bin", "é.bin", "z.bin"}},
		{"modified_at", "desc", []string{"z.bin", "B.bin", "é.bin", "a/same.bin", "b/same.bin"}},
	} {
		t.Run(tc.by+tc.order, func(t *testing.T) {
			s := selector(t, fmt.Sprintf("{name_regex: '.*[.]bin', recursive: true, take: 20, sort: {by: %s, order: %s}}", tc.by, tc.order), "")
			if got := paths(choose(t, r, s)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	got := paths(choose(t, r, selector(t, "{name_regex: '.*[.]bin', take: 20}", "")))
	if !reflect.DeepEqual(got, []string{"B.bin", "z.bin", "é.bin"}) {
		t.Fatal(got)
	}
	got = paths(choose(t, r, selector(t, "{path: a, name: same.bin, recursive: true}", "")))
	if !reflect.DeepEqual(got, []string{"a/same.bin"}) {
		t.Fatal(got)
	}
	got = paths(choose(t, r, selector(t, "{name: same.bin, recursive: true, take: 1}", "")))
	if !reflect.DeepEqual(got, []string{"a/same.bin"}) {
		t.Fatal(got)
	}
}
func TestFilesystemSelectorScanLimitAndNoPartialResult(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		files        []string
		want         string
	}{
		{"exact", "{name_regex: '.*', take: 1, scan_limit: 2}", []string{"a", "b"}, ""},
		{"one_over", "{name_regex: '.*', take: 1, scan_limit: 1}", []string{"a", "b"}, "LIMIT_EXCEEDED"},
		{"fixed_nonmatch_counts", "{name: a, scan_limit: 1}", []string{"a", "z"}, "LIMIT_EXCEEDED"},
		{"directory_counts", "{name: a, scan_limit: 1}", []string{"a", "dir/ignored"}, "LIMIT_EXCEEDED"},
		{"recursive_total_counts", "{name: a, recursive: true, scan_limit: 2}", []string{"a", "dir/ignored"}, "LIMIT_EXCEEDED"},
		{"reserved_counts", "{name: a, scan_limit: 1}", []string{"a", ".onprest-ignored"}, "LIMIT_EXCEEDED"},
		{"zero", "{name: absent}", []string{"a"}, "NOT_FOUND"},
		{"empty", "{name: absent}", nil, "NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p := root(t)
			for _, f := range tc.files {
				put(t, p, f, []byte("x"))
			}
			selected, e := r.Select(context.Background(), selector(t, tc.config, ""), nil)
			if tc.want != "" {
				code(t, e, tc.want)
				if selected != nil {
					t.Fatal("partial selection exposed")
				}
			} else if e != nil || len(selected) != 1 {
				t.Fatalf("%v %v", selected, e)
			}
		})
	}
}
func TestFilesystemSelectorRegexWholeNameAndUTF8Order(t *testing.T) {
	r, p := root(t)
	for _, name := range []string{"a", "B", "ab", "xa", "ax", "é"} {
		put(t, p, name, []byte("x"))
	}
	for _, tc := range []struct {
		pattern string
		want    []string
	}{{"a|B", []string{"B", "a"}}, {"a", []string{"a"}}, {"B", []string{"B"}}, {"é|é", []string{"é"}}, {"a.*", []string{"a", "ab", "ax"}}} {
		s := selector(t, "{name_regex: '"+tc.pattern+"', take: 20}", "")
		if got := paths(choose(t, r, s)); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s got %v", tc.pattern, got)
		}
	}
}
func TestFilesystemSelectorInputBindingNoFallbackAndImmutable(t *testing.T) {
	r, p := root(t)
	put(t, p, "document.bin", []byte("body"))
	put(t, p, "other.bin", []byte("other"))
	input := "    input:\n      properties:\n        nested:\n          properties:\n            'a/b~c': {items: {type: string}, minItems: 1}\n          required: ['a/b~c']\n      required: [nested]\n"
	s := selector(t, "{name: {input: /nested/a~1b~0c/0}}", input)
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{"nested", map[string]any{"nested": map[string]any{"a/b~c": []any{"document.bin"}}}},
		{"absent", nil}, {"null", map[string]any{"nested": nil}}, {"empty_array", map[string]any{"nested": map[string]any{"a/b~c": []any{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, e := r.Select(context.Background(), s, tc.params)
			if tc.name == "nested" {
				if e != nil || !reflect.DeepEqual(paths(got), []string{"document.bin"}) {
					t.Fatalf("%v %v", got, e)
				}
			} else {
				code(t, e, "VALIDATION_FAILED")
				if got != nil {
					t.Fatal("fallback result")
				}
			}
		})
	}
	for _, name := range []any{"../other.bin", "a/b", "a\\b", ".", "..", "", "line\nsecret", "nul\x00secret", string([]byte{0xff}), nil, int64(1)} {
		got, e := r.Select(context.Background(), s, map[string]any{"nested": map[string]any{"a/b~c": []any{name}}})
		code(t, e, "VALIDATION_FAILED")
		if got != nil {
			t.Fatal("invalid name fell back")
		}
	}
	c, e := contract.Compile([]byte(fixtureHeader + "{name: document.bin}\n    output: {files: {}}\n"))
	if e != nil {
		t.Fatal(e)
	}
	config := c.Capabilities["read"].Executor.Settings["read"].(map[string]any)["selector"].(map[string]any)
	fixed, e := PrepareSelector(config, c.Capabilities["read"].Input)
	if e != nil {
		t.Fatal(e)
	}
	config["name"] = "other.bin"
	config["sort"].(map[string]any)["order"] = "desc"
	if got := paths(choose(t, r, fixed)); !reflect.DeepEqual(got, []string{"document.bin"}) {
		t.Fatal("mutable selector", got)
	}
}
func TestFilesystemDescriptorRootIdentityAndClose(t *testing.T) {
	r, p := root(t)
	put(t, p, "safe", []byte("inside"))
	moved := p + "-moved"
	if e := os.Rename(p, moved); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	put(t, p, "safe", []byte("outside-secret"))
	f, e := r.Open(context.Background(), "safe")
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(f)
	if e != nil || string(b) != "inside" {
		t.Fatalf("%q %v", b, e)
	}
	if e := f.Close(); e != nil {
		t.Fatal(e)
	}
	if e := f.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := f.Read(make([]byte, 1)); !errors.Is(e, os.ErrClosed) {
		t.Fatal(e)
	}
	if e := r.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Open(context.Background(), "safe"); !errors.Is(e, os.ErrClosed) {
		t.Fatal(e)
	}
}
func TestFilesystemDescriptorTraversalReservedAndCancellation(t *testing.T) {
	r, p := root(t)
	put(t, p, "safe", []byte("inside"))
	put(t, p, ".onprest-hidden/deep", []byte("hidden"))
	for _, name := range []string{"../safe", "/safe", "a/../../safe", "a\\safe", "safe\x00secret", string([]byte{0xff}), ".onprest-hidden/deep"} {
		if f, e := r.Open(context.Background(), name); e == nil || f != nil {
			t.Fatalf("unsafe open %q", name)
		}
	}
	s := selector(t, "{name_regex: '.*', recursive: true, take: 20}", "")
	if got := paths(choose(t, r, s)); !reflect.DeepEqual(got, []string{"safe"}) {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f, e := r.Open(ctx, "safe"); !errors.Is(e, context.Canceled) || f != nil {
		t.Fatal(e)
	}
	if items, e := r.Select(ctx, s, nil); !errors.Is(e, context.Canceled) || items != nil {
		t.Fatal(e)
	}
	if _, e := OpenRoot("relative"); e == nil {
		t.Fatal("relative root")
	}
}
func TestFilesystemSelectedReplacementAndOpenedDescriptor(t *testing.T) {
	r, p := root(t)
	put(t, p, "safe", []byte("inside"))
	selection := choose(t, r, selector(t, "{name: safe}", ""))[0]
	f, e := selection.Open(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(filepath.Join(p, "safe"), filepath.Join(p, "old")); e != nil {
		t.Fatal(e)
	}
	put(t, p, "safe", []byte("outside-secret"))
	b, e := io.ReadAll(f)
	code(t, e, "SOURCE_CHANGED")
	if string(b) != "inside" {
		t.Fatalf("descriptor read other file: %q", b)
	}
	code(t, f.Close(), "SOURCE_CHANGED")
	if replacement, e := selection.Open(context.Background()); e == nil || replacement != nil {
		t.Fatal("selection rebound")
	} else {
		code(t, e, "SOURCE_CHANGED")
	}
	// Public metadata mutation never alters the private selected binding.
	selection.RelativePath = "safe"
	selection.Name = "forged"
	selection.SizeBytes = 999
	if f, e := selection.Open(context.Background()); e == nil || f != nil {
		t.Fatal("forged selection rebound")
	}
}
func TestFilesystemConcurrentRootAndSelector(t *testing.T) {
	r, p := root(t)
	put(t, p, "safe", []byte("inside"))
	s := selector(t, "{name: safe}", "")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			for j := 0; j < 10; j++ {
				items, e := r.Select(context.Background(), s, nil)
				if e != nil {
					t.Error(e)
					return
				}
				f, e := items[0].Open(context.Background())
				if e != nil {
					t.Error(e)
					return
				}
				b, e := io.ReadAll(f)
				if e != nil || string(b) != "inside" {
					t.Error(e)
				}
				if e := f.Close(); e != nil {
					t.Error(e)
				}
			}
		})
	}
	wg.Wait()
}

type diskStager struct {
	directory string
	stages    []*diskStage
	fault     string
	onBegin   func(int)
	onWrite   func(int)
	onFinish  func(int)
}
type diskStage struct {
	owner           *diskStager
	index           int
	file            *os.File
	filename        string
	aborted, closed int
	reader          *countedReader
	metadata        contract.ArtifactMetadata
	maxWrite        int
}
type countedReader struct {
	*os.File
	closes int
}

func (r *countedReader) Close() error { r.closes++; return r.File.Close() }
func (s *diskStager) Begin(ctx context.Context, name, mime string) (contract.ArtifactStage, error) {
	index := len(s.stages)
	if s.onBegin != nil {
		s.onBegin(index)
	}
	if s.fault == "begin" {
		return nil, errors.New("secret-begin")
	}
	if s.fault == "typed_nil_stage" {
		return (*diskStage)(nil), nil
	}
	if s.fault == "nil_stage" {
		return nil, nil
	}
	f, e := os.CreateTemp(s.directory, "stage-")
	if e != nil {
		return nil, e
	}
	stage := &diskStage{owner: s, index: index, file: f, filename: f.Name()}
	s.stages = append(s.stages, stage)
	return stage, nil
}
func (s *diskStage) Write(b []byte) (int, error) {
	s.maxWrite = max(s.maxWrite, len(b))
	if s.owner.onWrite != nil {
		s.owner.onWrite(s.index)
	}
	if s.owner.fault == "write" {
		return 0, errors.New("secret-write")
	}
	if s.owner.fault == "short" {
		return 0, nil
	}
	return s.file.Write(b)
}
func (s *diskStage) Close() error {
	s.closed++
	syncErr := s.file.Sync()
	closeErr := s.file.Close()
	if s.owner.fault == "flush" {
		return errors.Join(errors.New("secret-flush"), syncErr, closeErr)
	}
	return errors.Join(syncErr, closeErr)
}
func (s *diskStage) Finish(m contract.ArtifactMetadata) (io.ReadCloser, error) {
	s.metadata = m
	if s.owner.onFinish != nil {
		s.owner.onFinish(s.index)
	}
	if s.owner.fault == "finish" {
		return nil, errors.New("secret-finish")
	}
	if s.owner.fault == "typed_nil_reader" {
		return (*countedReader)(nil), nil
	}
	if s.owner.fault == "nil_reader" {
		return nil, nil
	}
	f, e := os.Open(s.filename)
	if e != nil {
		return nil, e
	}
	s.reader = &countedReader{File: f}
	if s.owner.fault == "finish_reader_error" {
		return s.reader, errors.New("secret-finish")
	}
	return s.reader, nil
}
func (s *diskStage) Abort() error {
	s.aborted++
	if s.closed == 0 {
		_ = s.file.Close()
	}
	e := os.Remove(s.filename)
	if s.owner.fault == "abort" {
		return errors.Join(errors.New("secret-abort"), e)
	}
	return e
}
func stager(t *testing.T) *diskStager { t.Helper(); return &diskStager{directory: t.TempDir()} }
func assertClean(t *testing.T, s *diskStager) {
	t.Helper()
	entries, e := os.ReadDir(s.directory)
	if e != nil || len(entries) != 0 {
		t.Fatalf("stage leak %v %v", entries, e)
	}
	for _, stage := range s.stages {
		if stage.aborted != 1 {
			t.Fatal("abort count", stage.aborted)
		}
		if stage.reader != nil && stage.reader.closes != 1 {
			t.Fatal("reader close count", stage.reader.closes)
		}
	}
}
func TestFilesystemSnapshotOriginalBytesDigestEmptyAndOwnership(t *testing.T) {
	r, p := root(t)
	payload := make([]byte, 130*1024+17)
	for i := range payload {
		payload[i] = byte(i)
	}
	put(t, p, "a.bin", payload)
	put(t, p, "empty.bin", nil)
	selected := choose(t, r, selector(t, "{name_regex: '.*[.]bin', take: 2}", ""))
	selected[0].Name = "forged"
	selected[0].SizeBytes = 999
	store := stager(t)
	snap, e := SnapshotFiles(context.Background(), selected, SnapshotLimits{int64(len(payload)), int64(len(payload)), 2}, "application/octet-stream", store)
	if e != nil {
		t.Fatal(e)
	}
	if len(snap.Files) != 2 {
		t.Fatal(snap.Files)
	}
	for i, want := range [][]byte{payload, {}} {
		f := snap.Files[i]
		data, e := io.ReadAll(f.Reader)
		digest := sha256.Sum256(want)
		if e != nil || !bytes.Equal(data, want) || f.SizeBytes != int64(len(want)) || f.SHA256 != hex.EncodeToString(digest[:]) || f.MIMEType != "application/octet-stream" {
			t.Fatalf("%+v %v", f, e)
		}
		if store.stages[i].maxWrite > 64*1024 {
			t.Fatal("unbounded write")
		}
	}
	if snap.Files[0].Name != "a.bin" || snap.Files[1].Name != "empty.bin" {
		t.Fatal("metadata forged")
	}
	if e := snap.Close(); e != nil {
		t.Fatal(e)
	}
	if e := snap.Close(); e != nil {
		t.Fatal(e)
	}
	assertClean(t, store)
}
func TestFilesystemSnapshotLimitsAndNoPartialPublication(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits SnapshotLimits
		want   string
	}{{"exact", SnapshotLimits{3, 6, 2}, ""}, {"single_over", SnapshotLimits{2, 6, 2}, "LIMIT_EXCEEDED"}, {"total_over", SnapshotLimits{3, 5, 2}, "LIMIT_EXCEEDED"}, {"count_over", SnapshotLimits{3, 6, 1}, "LIMIT_EXCEEDED"}, {"zero", SnapshotLimits{0, 6, 2}, "VALIDATION_FAILED"}, {"inconsistent", SnapshotLimits{7, 6, 2}, "VALIDATION_FAILED"}} {
		t.Run(tc.name, func(t *testing.T) {
			r, p := root(t)
			put(t, p, "a", []byte("abc"))
			put(t, p, "b", []byte("def"))
			selected := choose(t, r, selector(t, "{name_regex: '.*', take: 2}", ""))
			store := stager(t)
			snap, e := SnapshotFiles(context.Background(), selected, tc.limits, "application/octet-stream", store)
			if tc.want == "" {
				if e != nil {
					t.Fatal(e)
				}
				if e := snap.Close(); e != nil {
					t.Fatal(e)
				}
			} else {
				code(t, e, tc.want)
				if snap != nil {
					t.Fatal("partial snapshot")
				}
			}
			assertClean(t, store)
		})
	}
}
func TestFilesystemSnapshotStagerFaultsCancellationAndSourceChanges(t *testing.T) {
	for _, kind := range []string{"begin", "nil_stage", "typed_nil_stage", "write", "short", "flush", "finish", "nil_reader", "typed_nil_reader", "finish_reader_error", "cancel", "cancel_finish", "rewrite", "grow", "truncate", "replace", "later_replace", "later_replace_abort"} {
		t.Run(kind, func(t *testing.T) {
			r, p := root(t)
			put(t, p, "a", []byte("aaaa"))
			put(t, p, "b", []byte("bbbb"))
			selected := choose(t, r, selector(t, "{name_regex: '.*', take: 2}", ""))
			store := stager(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := "EXECUTION_FAILED"
			switch kind {
			case "cancel":
				store.onWrite = func(int) { cancel() }
			case "cancel_finish":
				store.onFinish = func(int) { cancel() }
			case "rewrite", "grow", "truncate":
				expected = "SOURCE_CHANGED"
				once := false
				store.onWrite = func(int) {
					if once {
						return
					}
					once = true
					body := []byte("xxxx")
					if kind == "grow" {
						body = []byte("xxxxxx")
					}
					if kind == "truncate" {
						body = []byte("x")
					}
					put(t, p, "a", body)
				}
			case "replace":
				expected = "SOURCE_CHANGED"
				store.onBegin = func(i int) {
					if i == 0 {
						if e := os.Rename(filepath.Join(p, "a"), filepath.Join(p, "old")); e != nil {
							t.Fatal(e)
						}
						put(t, p, "a", []byte("secret-outside"))
					}
				}
			case "later_replace", "later_replace_abort":
				if kind == "later_replace_abort" {
					store.fault = "abort"
				}
				expected = "SOURCE_CHANGED"
				store.onFinish = func(i int) {
					if i == 0 {
						if e := os.Rename(filepath.Join(p, "b"), filepath.Join(p, "old")); e != nil {
							t.Fatal(e)
						}
						put(t, p, "b", []byte("secret-outside"))
					}
				}
			default:
				store.fault = kind
			}
			snap, e := SnapshotFiles(ctx, selected, SnapshotLimits{20, 40, 2}, "application/octet-stream", store)
			if kind == "cancel" || kind == "cancel_finish" {
				if !errors.Is(e, context.Canceled) {
					t.Fatal(e)
				}
			} else {
				code(t, e, expected)
			}
			if snap != nil {
				t.Fatal("failed snapshot published")
			}
			assertClean(t, store)
		})
	}
}

func TestFilesystemSelectorStaticBindingAndPreparedRejections(t *testing.T) {
	for _, input := range []string{
		"    input: {properties: {name: {type: string}}}\n",
		"    input: {properties: {name: {type: integer}}, required: [name]}\n",
		"    input: {properties: {name: {type: [string, null]}}, required: [name]}\n",
		"    input: {properties: {nested: {properties: {name: {type: string}}, required: [name]}}}\n",
	} {
		pointer := "/name"
		if strings.Contains(input, "nested") {
			pointer = "/nested/name"
		}
		_, e := contract.Compile([]byte(fixtureHeader + "{name: {input: '" + pointer + "'}}\n    output: {files: {}}\n" + input))
		if e == nil {
			t.Fatal("unsafe static selector binding")
		}
	}
	c, e := contract.Compile([]byte(fixtureHeader + "{name: safe}\n    output: {files: {}}\n"))
	if e != nil {
		t.Fatal(e)
	}
	baseline := c.Capabilities["read"].Executor.Settings["read"].(map[string]any)["selector"].(map[string]any)
	for _, bad := range []struct {
		key   string
		value any
	}{{"path", "../escape"}, {"name", map[string]any{"input": "/missing"}}, {"take", int64(0)}, {"scan_limit", int64(0)}, {"name_regex", "["}, {"sort", map[string]any{"by": "arbitrary", "order": "asc"}}} {
		config := map[string]any{}
		for k, v := range baseline {
			config[k] = v
		}
		config[bad.key] = bad.value
		if s, e := PrepareSelector(config, c.Capabilities["read"].Input); s != nil || e == nil {
			t.Fatal("invalid prepared selector accepted", bad)
		}
	}
}
func TestFilesystemSnapshotInvalidArgumentsAndSafeCleanupError(t *testing.T) {
	r, p := root(t)
	put(t, p, "safe", []byte("inside"))
	selected := choose(t, r, selector(t, "{name: safe}", ""))
	limits := SnapshotLimits{6, 6, 1}
	for _, mime := range []string{"", "Text/Plain", "text/plain; charset=utf-8", "text/*"} {
		snap, e := SnapshotFiles(context.Background(), selected, limits, mime, stager(t))
		if e == nil || snap != nil {
			t.Fatal("invalid MIME")
		}
		code(t, e, "VALIDATION_FAILED")
	}
	var nilStore *diskStager
	if snap, e := SnapshotFiles(context.Background(), selected, limits, "text/plain", nilStore); e == nil || snap != nil {
		t.Fatal("typed nil store")
	}
	if snap, e := SnapshotFiles(context.Background(), []Selection{{Name: "forged"}}, limits, "text/plain", stager(t)); e == nil || snap != nil {
		t.Fatal("forged selection")
	}
	store := stager(t)
	store.fault = "abort"
	snap, e := SnapshotFiles(context.Background(), selected, limits, "text/plain", store)
	if e != nil {
		t.Fatal(e)
	}
	snap.Files = nil // consumer metadata cannot discard the privately owned reader
	code(t, snap.Close(), "EXECUTION_FAILED")
	assertClean(t, store)
}

func TestFilesystemSelectorRegexCaseSensitivityAndCompilerAgreement(t *testing.T) {
	r, p := root(t)
	put(t, p, "B", []byte("inside"))
	items, e := r.Select(context.Background(), selector(t, "{name_regex: b}", ""), nil)
	if items != nil {
		t.Fatal("case-insensitive regex")
	}
	code(t, e, "NOT_FOUND")
	c, e := contract.Compile([]byte(fixtureHeader + "{name_regex: B}\n    output: {files: {}}\n"))
	if e != nil {
		t.Fatal(e)
	}
	config := c.Capabilities["read"].Executor.Settings["read"].(map[string]any)["selector"].(map[string]any)
	for _, pattern := range []string{"(?i)b", "(?i:b)", "(?im-s:b)", "x(?i)b", "(?i-i)b", "(?i:123)"} {
		_, e := contract.Compile([]byte(fixtureHeader + "{name_regex: '" + pattern + "'}\n    output: {files: {}}\n"))
		if e == nil {
			t.Fatal("compiler enabled case folding", pattern)
		}
		config["name_regex"] = pattern
		if s, e := PrepareSelector(config, c.Capabilities["read"].Input); s != nil || e == nil {
			t.Fatal("API enabled case folding", pattern)
		}
	}
}

func TestFilesystemSnapshotCancellationKeepsCleanupDiagnosticsPrivate(t *testing.T) {
	r, p := root(t)
	put(t, p, "safe", []byte("inside"))
	selected := choose(t, r, selector(t, "{name: safe}", ""))
	store := stager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.onWrite = func(int) { cancel() }
	store.fault = "abort"
	snap, e := SnapshotFiles(ctx, selected, SnapshotLimits{6, 6, 1}, "text/plain", store)
	if snap != nil || !errors.Is(e, context.Canceled) || strings.Contains(e.Error(), "secret") {
		t.Fatalf("unsafe cancellation %v", e)
	}
	var containsCause func(error) bool
	containsCause = func(err error) bool {
		if err == nil {
			return false
		}
		if strings.Contains(err.Error(), "secret-abort") {
			return true
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, cause := range joined.Unwrap() {
				if containsCause(cause) {
					return true
				}
			}
			return false
		}
		return containsCause(errors.Unwrap(err))
	}
	if !containsCause(e) {
		t.Fatal("cleanup diagnosis lost")
	}
	assertClean(t, store)
}

func TestFilesystemRecursiveQueuedDirectoryChangesRejectPartialSelection(t *testing.T) {
	for _, kind := range []string{"replace", "remove"} {
		t.Run(kind, func(t *testing.T) {
			r, p := root(t)
			put(t, p, "top.bin", []byte("already selected"))
			put(t, p, "child/inside.bin", []byte("original"))
			s := selector(t, "{name_regex: '.*[.]bin', recursive: true, take: 20}", "")
			barrierCalls := 0
			selected, e := r.selectFiles(context.Background(), s, nil, func(relative string) {
				if relative != "child" {
					t.Fatalf("unexpected queued directory %q", relative)
				}
				barrierCalls++
				// Parent enumeration and its own binding verification have finished.
				// No child descriptor remains open while this real-filesystem actor runs.
				saved := p + "-saved"
				if e := os.Rename(filepath.Join(p, "child"), saved); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { _ = os.RemoveAll(saved) })
				if kind == "replace" {
					put(t, p, "child/new.bin", []byte("secret-new-selection"))
				}
			})
			if barrierCalls != 1 {
				t.Fatal("adversary barrier not reached", barrierCalls)
			}
			code(t, e, "SOURCE_CHANGED")
			if selected != nil {
				t.Fatalf("partial or reselected result %v", paths(selected))
			}
		})
	}
}

func TestFilesystemSelectorUnterminatedQuotePreservesGoMeaning(t *testing.T) {
	r, p := root(t)
	for _, name := range []string{"abc", "xabc", "abcx"} {
		put(t, p, name, []byte(name))
	}
	// Compile the actual YAML, then prepare and execute the resulting common
	// selector against real files. The original definition retains Go syntax.
	config, e := contract.Compile([]byte(fixtureHeader + `{name_regex: '\Qabc', take: 10}` + "\n    output: {files: {}}\n"))
	if e != nil {
		t.Fatal(e)
	}
	capability := config.Capabilities["read"]
	normalized := capability.Executor.Settings["read"].(map[string]any)["selector"].(map[string]any)
	if normalized["name_regex"] != `\Qabc` {
		t.Fatal("original regexp definition changed")
	}
	prepared, e := PrepareSelector(normalized, capability.Input)
	if e != nil {
		t.Fatal(e)
	}
	if got := paths(choose(t, r, prepared)); !reflect.DeepEqual(got, []string{"abc"}) {
		t.Fatalf("literal/full-name semantics lost: %v", got)
	}
}
