//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

type fileFixture struct {
	ctl      *ExecutionController
	prepared *providers.Prepared
	store    *artifacts.Store
	provider *fileProvider
	base     string
	cap      *contract.Capability
	owner    artifacts.Owner
	ctx      context.Context
	cf       *CapabilityFile
}

func fileController(t *testing.T, executor, output, policy string) *fileFixture {
	t.Helper()
	base := t.TempDir()
	for _, dir := range []string{"exchange", "reports", "exchange/sub"} {
		if e := os.MkdirAll(filepath.Join(base, dir), 0700); e != nil {
			t.Fatal(e)
		}
	}
	body := `schema_version: 1
service: {title: File fixture, version: test}
gateway: {url: ws://localhost/ws/agent, agent_private_key: ` + testAgentPrivateKey + `}
sources:
  folders:
    exchange: {root: ./exchange, access: read_write}
    reports: {root: ./reports, access: read}
capabilities:
  run:
    executor: ` + executor + `
    input:
      properties: {document: {type: file}, documents: {items: {type: file}, minItems: 1}, extra: {type: integer, default: 9}}
      required: [document]
` + output + policy
	if strings.Contains(executor, "input: /documents") {
		body = strings.Replace(body, "required: [document]", "required: [document, documents]", 1)
	}
	cfg, e := contract.Compile([]byte(body))
	if e != nil {
		t.Fatal(e)
	}
	store, e := artifacts.Open(artifacts.Options{Directory: filepath.Join(base, "spool"), QuotaBytes: 8 << 20, MaxPending: 4, MaxObjects: 128, TransferTimeout: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	f := &fileFixture{store: store, base: base, cap: cfg.Capabilities["run"]}
	f.cf, e = capabilityRuntimeView(cfg, filepath.Join(base, "agent.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	registry, _ := providers.NewRegistry(map[string]providers.Factory{"file": func(ctx context.Context, s *providers.Snapshot) (providers.Provider, error) {
		p, e := prepareFileProvider(ctx, s, store)
		if p != nil {
			f.provider = p.(*fileProvider)
		}
		return p, e
	}})
	f.prepared, e = registry.Prepare(context.Background(), cfg, filepath.Join(base, "agent.yaml"), providers.ResolveOptions{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.prepared.Close() })
	f.ctl, e = NewExecutionController(f.prepared, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	snapshot, _ := f.prepared.Snapshot("run")
	f.owner = artifacts.Owner{RequestID: "request", CallerBinding: "caller", Capability: "run", DefinitionHash: snapshot.DefinitionHash(), AuthGeneration: "generation"}
	return f
}
func (f *fileFixture) execute(t *testing.T, names []string, bodies [][]byte) (*contract.PreparedOutput, *contract.ErrorValue) {
	t.Helper()
	r, e := f.store.BeginInput(context.Background(), f.owner, f.cap, time.Now().Add(time.Minute))
	if e != nil {
		t.Fatal(e)
	}
	in := map[string]any{}
	for i, name := range names {
		pointer := "/document"
		if i > 0 {
			pointer = fmt.Sprintf("/documents/%d", i-1)
		}
		st, e := r.NewFile(f.owner, pointer, name, "application/pdf")
		if e != nil {
			t.Fatal(e)
		}
		for offset := 0; offset < len(bodies[i]); offset += 64 * 1024 {
			end := min(offset+64*1024, len(bodies[i]))
			if _, e = st.Write(bodies[i][offset:end]); e != nil {
				t.Fatal(e)
			}
		}
		if e = st.Close(); e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(bodies[i])
		v, e := r.Complete(f.owner, st, int64(len(bodies[i])), hex.EncodeToString(sum[:]))
		if e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			in["document"] = v
		} else {
			a, _ := in["documents"].([]any)
			in["documents"] = append(a, v)
		}
	}
	in, e = r.Commit(f.owner, in)
	if e != nil {
		t.Fatal(e)
	}
	req := ExecutionRequest{ID: f.owner.RequestID, Capability: "run", DefinitionHash: f.owner.DefinitionHash, Params: in, input: r, owner: f.owner}
	if f.cap.Output.Files != nil {
		p := f.cap.Policy
		ttl, _ := time.ParseDuration(p["file_ttl"].(string))
		scope, e := f.store.Output(f.owner, artifacts.OutputLimits{MaxFileBytes: p["max_output_file_bytes"].(int64), MaxTotalBytes: p["max_total_output_file_bytes"].(int64), MaxFiles: p["max_output_files"].(int64), TTL: ttl})
		if e != nil {
			t.Fatal(e)
		}
		req.stager = scope
	}
	ctx := f.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return f.ctl.Execute(ctx, req)
}
func TestFileProviderWrittenArraySaveOnlyAndSeparateRead(t *testing.T) {
	for _, mode := range []string{"save_only", "written", "separate"} {
		t.Run(mode, func(t *testing.T) {
			executor := `{type: file, write: [{input: /document, source: exchange, path: sub, name: fixed.pdf}, {input: /documents, source: exchange, name_from: input}]`
			output := ""
			switch mode {
			case "written":
				executor += `, read: {written: true}`
				output = "    output: {files: {}}\n"
			case "separate":
				executor += `, read: {source: reports, selector: {name: response.pdf}}`
				output = "    output: {files: {mime_type: application/custom}}\n"
			}
			executor += "}"
			f := fileController(t, executor, output, "")
			os.WriteFile(filepath.Join(f.base, "reports", "response.pdf"), []byte("independent report"), 0600)
			bodies := [][]byte{bytes.Repeat([]byte{255, 0, 128}, 45000), {}, []byte("third")}
			names := []string{"input.pdf", "empty.pdf", "last.pdf"}
			out, failure := f.execute(t, names, bodies)
			if failure != nil {
				t.Fatal(failure)
			}
			defer out.Close()
			if out.Data != nil {
				t.Fatal("unexpected data")
			}
			for i, path := range []string{"sub/fixed.pdf", "empty.pdf", "last.pdf"} {
				b, e := os.ReadFile(filepath.Join(f.base, "exchange", path))
				if e != nil || !bytes.Equal(b, bodies[i]) {
					t.Fatal("saved bytes", path, e)
				}
			}
			if mode == "save_only" {
				if len(out.Files) != 0 || f.store.Usage() != 0 {
					t.Fatal("save-only output/quota")
				}
				return
			}
			expected := bodies
			expectedNames := []string{"fixed.pdf", "empty.pdf", "last.pdf"}
			if mode == "separate" {
				expected = [][]byte{[]byte("independent report")}
				expectedNames = []string{"response.pdf"}
			}
			if len(out.Files) != len(expected) {
				t.Fatal("file count")
			}
			for i, file := range out.Files {
				b, e := io.ReadAll(file.Reader)
				sum := sha256.Sum256(expected[i])
				if e != nil || !bytes.Equal(b, expected[i]) || file.Name != expectedNames[i] || file.SHA256 != hex.EncodeToString(sum[:]) {
					t.Fatal("snapshot byte/metadata mismatch", e)
				}
				wantMime := "application/pdf"
				if mode == "separate" {
					wantMime = "application/custom"
				}
				if file.MIMEType != wantMime {
					t.Fatal("MIME", file.MIMEType)
				}
			}
			if e := out.Close(); e != nil {
				t.Fatal(e)
			}
			if f.store.Usage() != 0 {
				t.Fatal("input/output reservation leak")
			}
		})
	}
}
func TestFileProviderFailureOutcomeBeforePartialAndAfterCommit(t *testing.T) {
	for _, mode := range []string{"conflict", "invalid_name", "source_link", "missing_read", "output_limit", "cancel_after_save"} {
		t.Run(mode, func(t *testing.T) {
			executor := `{type: file, write: [{input: /document, source: exchange, name_from: input}]`
			output := ""
			policy := ""
			if mode == "cancel_after_save" {
				executor += `, read: {written: true}`
				output = "    output: {files: {}}\n"
			}
			if mode == "missing_read" {
				executor += `, read: {source: reports, selector: {name: absent}}`
				output = "    output: {files: {}}\n"
			}
			if mode == "output_limit" {
				executor += `, read: {written: true}`
				output = "    output: {files: {}}\n"
				policy = "    policy: {max_output_file_bytes: 1, max_total_output_file_bytes: 1}\n"
			}
			executor += "}"
			f := fileController(t, executor, output, policy)
			name := "saved.pdf"
			if mode == "conflict" {
				os.WriteFile(filepath.Join(f.base, "exchange", name), []byte("previous"), 0600)
			}
			if mode == "invalid_name" {
				name = ".onprest-fake"
			}
			if mode == "source_link" {
				outside := t.TempDir()
				os.Symlink(filepath.Join(outside, "secret"), filepath.Join(f.base, "exchange", name))
			}
			if mode == "cancel_after_save" {
				var cancel context.CancelFunc
				f.ctx, cancel = context.WithCancel(context.Background())
				defer cancel()
				f.provider.afterSave = cancel
			}
			out, failure := f.execute(t, []string{name}, [][]byte{[]byte("body")})
			if out != nil || failure == nil {
				t.Fatal("expected failure", failure)
			}
			outcome := "not_started"
			code := "CONFLICT"
			if mode == "invalid_name" {
				code = "VALIDATION_FAILED"
			}
			if mode == "source_link" {
				code = "PERMISSION_DENIED"
			}
			if mode == "missing_read" {
				outcome = "committed"
				code = "NOT_FOUND"
			}
			if mode == "output_limit" {
				outcome = "committed"
				code = "LIMIT_EXCEEDED"
			}
			if mode == "cancel_after_save" {
				outcome = "committed"
				code = "EXECUTION_TIMEOUT"
			}
			if failure.Outcome != outcome || failure.Code != code {
				t.Fatalf("%s got %v", mode, failure)
			}
			if strings.Contains(failure.Message, f.base) {
				t.Fatal("path leaked")
			}
			if outcome == "committed" {
				if b, e := os.ReadFile(filepath.Join(f.base, "exchange", name)); e != nil || string(b) != "body" {
					t.Fatal("completed file lost")
				}
			}
			if f.store.Usage() != 0 {
				t.Fatal("failed input/output leaked")
			}
		})
	}
}
func TestFileProviderReadOnlyAndNonDestructivePreparation(t *testing.T) {
	f := fileController(t, `{type: file, read: {source: reports, selector: {name_regex: '.*\.pdf', take: 2}}}`, "    output: {files: {}}\n", "")
	if _, e := os.Stat(filepath.Join(f.base, "spool", "file-write-cleanup")); !os.IsNotExist(e) {
		t.Fatal("preflight created save journal")
	}
	os.WriteFile(filepath.Join(f.base, "reports", "b.pdf"), []byte("B"), 0600)
	os.WriteFile(filepath.Join(f.base, "reports", "a.pdf"), []byte("A"), 0600)
	out, failure := f.execute(t, []string{"unused.pdf"}, [][]byte{[]byte("unused")})
	if failure != nil {
		t.Fatal(failure)
	}
	defer out.Close()
	if len(out.Files) != 2 || out.Files[0].Name != "a.pdf" || out.Files[1].Name != "b.pdf" {
		t.Fatal("selector order")
	}
	for i, file := range out.Files {
		b, e := io.ReadAll(file.Reader)
		if e != nil || string(b) != []string{"A", "B"}[i] {
			t.Fatal("read byte mismatch")
		}
	}
}

func TestFileProviderPartialPublicationDoesNotRollback(t *testing.T) {
	f := fileController(t, `{type: file, write: [{input: /document, source: exchange, name: a.pdf}, {input: /document, source: exchange, name: b.pdf}]}`, "", "")
	f.provider.afterInstalled = func(n int) {
		if n == 1 {
			if e := os.WriteFile(filepath.Join(f.base, "exchange", "b.pdf"), []byte("other writer"), 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
	out, failure := f.execute(t, []string{"input.pdf"}, [][]byte{[]byte("body")})
	if out != nil || failure == nil || failure.Code != "CONFLICT" || failure.Outcome != "unknown" {
		t.Fatalf("partial result: %v", failure)
	}
	for name, want := range map[string]string{"a.pdf": "body", "b.pdf": "other writer"} {
		b, e := os.ReadFile(filepath.Join(f.base, "exchange", name))
		if e != nil || string(b) != want {
			t.Fatal(name, e, string(b))
		}
	}
	if f.store.Usage() != 0 {
		t.Fatal("partial cleanup leak")
	}
}

func TestFileProviderMissingDirectoryPreparationHasNoSideEffects(t *testing.T) {
	f := fileController(t, `{type: file, write: [{input: /document, source: exchange, path: sub, name: result.pdf}]}`, "", "")
	if e := os.Remove(filepath.Join(f.base, "exchange", "sub")); e != nil {
		t.Fatal(e)
	}
	registry, _ := providers.NewRegistry(map[string]providers.Factory{"file": func(ctx context.Context, s *providers.Snapshot) (providers.Provider, error) {
		return prepareFileProvider(ctx, s, f.store)
	}})
	prepared, e := registry.Prepare(context.Background(), f.cf.Compiled, f.cf.ConfigPath, providers.ResolveOptions{})
	if e == nil {
		prepared.Close()
		t.Fatal("missing directory passed prepare")
	}
	entries, e := os.ReadDir(filepath.Join(f.base, "exchange"))
	if e != nil || len(entries) != 0 {
		t.Fatal("prepare wrote business files", entries, e)
	}
	if _, e = os.Stat(filepath.Join(f.base, "spool", "file-write-cleanup")); !os.IsNotExist(e) {
		t.Fatal("prepare acquired cleanup journal")
	}
	if f.store.Usage() != 0 {
		t.Fatal("prepare reservation")
	}
}

func TestFileProviderProductionPrepareWriteAndSpoolFree(t *testing.T) {
	f := fileController(t, `{type: file, write: [{input: /document, source: exchange, name: x}]}`, "", "")
	untouched := filepath.Join(f.base, "preflight-spool")
	f.cf.Compiled.Runtime["spool_dir"] = untouched
	preflightDriver.reset()
	withPreflightSeams(t, f.cf)
	var events []string
	var detail bytes.Buffer
	prepared, e := prepareAgent(context.Background(), Config{CapabilityFile: f.cf.ConfigPath}, memoryPreparationLog(&events, &detail))
	if e != nil {
		t.Fatal(e)
	}
	prepared.closeDatabase()
	for _, event := range preflightDriver.snapshot() {
		if event != "load" {
			t.Fatal("file-only preparation opened database", event)
		}
	}
	if _, e = os.Stat(untouched); !os.IsNotExist(e) {
		t.Fatal("preflight acquired spool")
	}
}
