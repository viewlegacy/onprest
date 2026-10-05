package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

func versionedFixture(body string) string {
	return `schema_version: 1
gateway: {url: ws://127.0.0.1:8080/ws/agent, agent_private_key: ` + testAgentPrivateKey + `}
sources: {databases: {primary: {driver: postgres, host: localhost, port: 5432, name: test, user: test}}}
capabilities:
  test:
` + body
}
func versionedCompile(t *testing.T, body string) *contract.Config {
	t.Helper()
	c, e := contract.Compile([]byte(versionedFixture(body)))
	if e != nil {
		t.Fatal(e)
	}
	return c
}

// The compiler owns schema semantics; these cases ensure the public loader
// actually invokes it rather than its old SQL/params/result decoder.
func TestVersionedLoaderCompilerContracts(t *testing.T) {
	body := `    executor: {type: db, source: primary, sql: "select :id"}
    input:
      properties:
        inherited: &str {type: string, default: sample, minLength: 1, pattern: '^s'}
        alias: {<<: *str}
        id: {type: integer, minimum: 1, maximum: 10, default: 3}
    output: {data: {}}
    policy: {timeout: 2s, rate_limit: {requests: 2, per: 1s, burst: 1}}
    examples: [{params: {id: 4}}]
`
	cf, e := LoadCapabilityFile(writeCapabilityFixture(t, versionedFixture(body)))
	if e != nil {
		t.Fatal(e)
	}
	params, _, e := cf.Compiled.Capabilities["test"].ValidateInput(nil)
	if e != nil || params["id"] != int64(3) || params["alias"] != "sample" {
		t.Fatal(params, e)
	}
	if cf.Runtime.MaxConcurrentRequests == nil || *cf.Runtime.MaxConcurrentRequests != 16 {
		t.Fatal(cf.Runtime)
	}
	doc := contract.BuildOpenAPI(cf.Compiled)
	if doc["paths"] == nil {
		t.Fatal("missing generated metadata")
	}
	for name, data := range map[string]string{
		"legacy":             `gateway: {url: ws://localhost/ws/agent, agent_private_key: ` + testAgentPrivateKey + `}\ndatabase: {}\ncapabilities: {x: {sql: select 1}}`,
		"version":            strings.Replace(versionedFixture(body), "schema_version: 1", "schema_version: 2", 1),
		"unknown":            strings.Replace(versionedFixture(body), "minimum: 1", "unknown: 1", 1),
		"null_default":       strings.Replace(versionedFixture(body), "default: 3", "default: null", 1),
		"default_bounds":     strings.Replace(versionedFixture(body), "default: 3", "default: 11", 1),
		"example_bounds":     strings.Replace(versionedFixture(body), "id: 4", "id: 0", 1),
		"rate":               strings.Replace(versionedFixture(body), "requests: 2", "requests: 0", 1),
		"readonly":           strings.Replace(versionedFixture(body), "select :id", "update t set id=:id", 1),
		"runtime":            versionedFixture(body) + "runtime: {max_concurrent_requests: 0}\n",
		"multiple_documents": versionedFixture(body) + "---\n{}\n",
		"gateway_security":   strings.Replace(versionedFixture(body), "127.0.0.1:8080", "public.example", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := LoadCapabilityFile(writeCapabilityFixture(t, data)); e == nil {
				t.Fatal("invalid configuration loaded")
			}
		})
	}
	unsafe := strings.Replace(versionedFixture(body), "minimum: 1", `"bad\\nPRIVATE_SENTINEL": true`, 1)
	_, e = LoadCapabilityFile(writeCapabilityFixture(t, unsafe))
	if e == nil || strings.Contains(e.Error(), "PRIVATE_SENTINEL") || strings.ContainsAny(e.Error(), "\r\n\x1b") {
		t.Fatalf("unsafe diagnostic: %v", e)
	}
}

func TestVersionedSigningKeyFileAndReleaseTemplate(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if e := os.WriteFile(keyPath, []byte(testAgentPrivateKey), 0600); e != nil {
		t.Fatal(e)
	}
	body := `    executor: {type: db, source: primary, sql: select 1}
`
	data := strings.Replace(versionedFixture(body), "agent_private_key: "+testAgentPrivateKey, "agent_private_key_file: ./key", 1)
	path := filepath.Join(dir, "capability.yaml")
	if e := os.WriteFile(path, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadCapabilityFile(path); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(keyPath, []byte(testAgentPrivateKey+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadCapabilityFile(path); e == nil {
		t.Fatal("key bytes silently trimmed")
	}
	template, e := os.ReadFile(filepath.Join("..", "..", "release", "capability.yaml.example"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e := LoadCapabilityFile(writeCapabilityFixture(t, string(template))); e == nil {
		t.Fatal("incomplete template loaded")
	}
	for _, x := range [][2]string{{"replace-with-create-agent-secret-private-key", testAgentPrivateKey}, {"capabilities: {}", "capabilities:\n  test:\n" + body}} {
		template = bytes.ReplaceAll(template, []byte(x[0]), []byte(x[1]))
	}
	if _, e := LoadCapabilityFile(writeCapabilityFixture(t, string(template))); e != nil {
		t.Fatal(e)
	}
}

func TestVersionedPreparationLoaderAndUnavailableProviders(t *testing.T) {
	preflightDriver.reset()
	oldOpen := openDatabaseForPreparation
	openDatabaseForPreparation = func(DatabaseDef) (*sql.DB, error) { return sql.Open(preflightTestDriverName, "") }
	t.Cleanup(func() { openDatabaseForPreparation = oldOpen })
	body := `    executor: {type: db, source: primary, sql: select 1}
    output: {data: {}}
`
	path := writeCapabilityFixture(t, versionedFixture(body))
	var events []string
	var detail bytes.Buffer
	p, e := prepareAgent(context.Background(), Config{CapabilityFile: path}, memoryPreparationLog(&events, &detail))
	if e != nil {
		t.Fatal(e)
	}
	if p.versioned == nil || p.controller == nil || p.db != nil {
		t.Fatal("legacy preparation used")
	}
	r := &Runner{cf: p.cf, versioned: p.versioned, controller: p.controller}
	meta := r.executeVersioned(context.Background(), protocol.Request{Type: "request", Deadline: time.Now().Add(time.Minute), ID: "meta-id", Capability: "meta"})
	if meta.Type != "metadata" {
		t.Fatal(meta)
	}
	s, _ := p.versioned.Snapshot("test")
	missingHash := r.executeVersioned(context.Background(), protocol.Request{Type: "request", ID: "missing-hash", Capability: "test", Deadline: time.Now().Add(time.Second)})
	if missingHash.Error == nil || missingHash.Error.Code != "INVALID_REQUEST" || missingHash.Error.Outcome != "not_started" || strings.Join(preflightDriver.snapshot(), ",") != "open,ping,explain" {
		t.Fatal("hashless public request reached the provider")
	}
	resp := r.executeVersioned(context.Background(), protocol.Request{Type: "request", ID: "request-id", Capability: "test", DefinitionHash: s.DefinitionHash(), Deadline: time.Now().Add(time.Second), Params: map[string]any{}})
	if resp.Error != nil || resp.Metrics == nil || resp.Metrics.ReturnedRows == nil || *resp.Metrics.ReturnedRows != 0 || resp.ID != "request-id" {
		t.Fatal(resp)
	}
	if e := p.finishValidationSuccess(); e != nil {
		t.Fatal(e)
	}
	if got := strings.Join(preflightDriver.snapshot(), ","); got != "open,ping,explain,explain,close" {
		t.Fatalf("actual prepared path=%s", got)
	}
	for name, b := range map[string]string{"wasm": "    executor: {type: wasm, module: ./missing.wasm}\n", "db_delivery": body + "    output: {files: {format: json, filename: x.json}}\n"} {
		t.Run(name, func(t *testing.T) {
			var log bytes.Buffer
			var ev []string
			_, e := prepareAgent(context.Background(), Config{CapabilityFile: writeCapabilityFixture(t, versionedFixture(b))}, memoryPreparationLog(&ev, &log))
			if e == nil {
				t.Fatal("unavailable executor/delivery prepared")
			}
		})
	}
}

func TestVersionedStartupValidateDoctorEntryPoints(t *testing.T) {
	oldExe, oldOpen := executablePath, openDatabaseForPreparation
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	openDatabaseForPreparation = func(definition DatabaseDef) (*sql.DB, error) {
		if !definition.TLS.Snapshot || definition.Host != "localhost" || definition.Name != "test" {
			t.Fatal("DB connector did not receive the versioned source snapshot")
		}
		return sql.Open(preflightTestDriverName, "")
	}
	t.Cleanup(func() { executablePath, openDatabaseForPreparation = oldExe, oldOpen })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws/agent/challenge":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"challenge":"versioned-doctor-challenge"}`)
		case "/ws/agent/verify":
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	data := strings.Replace(versionedFixture("    executor: {type: db, source: primary, sql: select 1}\n"), "ws://127.0.0.1:8080", "ws"+strings.TrimPrefix(server.URL, "http"), 1)
	data += "runtime: {identity_dir: " + string(protocol.MustJSON(filepath.Join(dir, "identity"))) + "}\n"
	path := filepath.Join(dir, "capability.yaml")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	// Exercise the real binary-adjacent resolver without replacing os.Executable.
	defaultPath := defaultCapabilityFile()
	file, err := os.OpenFile(defaultPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(defaultPath) })
	if _, err := file.WriteString(data); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, resolver := range []string{"config", "alias", "environment", "default"} {
		for _, entry := range []string{"startup", "validate", "doctor"} {
			t.Run(entry+"/"+resolver, func(t *testing.T) {
				preflightDriver.reset()
				t.Setenv("AGENT_CAPABILITY_FILE", "")
				args := []string{}
				selectedPath := path
				switch resolver {
				case "config":
					args = []string{"--config", path, "--capability-file", "missing.yaml"}
					t.Setenv("AGENT_CAPABILITY_FILE", "missing-env.yaml")
				case "alias":
					args = []string{"--capability-file", path}
					t.Setenv("AGENT_CAPABILITY_FILE", "missing-env.yaml")
				case "environment":
					t.Setenv("AGENT_CAPABILITY_FILE", path)
				case "default":
					selectedPath = defaultPath
				}
				var stdout, stderr bytes.Buffer
				if entry == "startup" {
					cfg, err := LoadConfig(args)
					if err != nil || cfg.CapabilityFile != selectedPath {
						t.Fatalf("startup resolver: %+v %v", cfg, err)
					}
					runner, err := NewRunner(context.Background(), cfg, &stdout)
					if err != nil {
						t.Fatal(err)
					}
					if runner.cf.Compiled == nil || runner.cf.ConfigPath != selectedPath || runner.versioned == nil || runner.controller == nil {
						t.Fatal("startup bypassed versioned loader/prepare")
					}
					_ = runner.store.Close()
					_ = runner.versioned.Close()
					_ = runner.detailLogCloser.Close()
				} else {
					handled, code := HandleCLI(context.Background(), append([]string{entry}, append(args, "--format", "json")...), &stdout, &stderr)
					var output map[string]any
					statusKey := "ok"
					if entry == "validate" {
						statusKey = "valid"
					}
					if !handled || code != 0 || json.Unmarshal(stdout.Bytes(), &output) != nil || output[statusKey] != true || stderr.Len() != 0 {
						t.Fatalf("%s CLI: code=%d stdout=%s stderr=%s", entry, code, &stdout, &stderr)
					}
				}
				if got := strings.Join(preflightDriver.snapshot(), ","); got != "open,ping,explain,close" {
					t.Fatalf("%s did not use shared versioned DB preparation: %s", entry, got)
				}
			})
		}
	}
}

func TestVersionedStartupValidateDoctorRejectUnsafeConfiguration(t *testing.T) {
	oldExe, oldOpen := executablePath, openDatabaseForPreparation
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	openDatabaseForPreparation = func(DatabaseDef) (*sql.DB, error) { return sql.Open(preflightTestDriverName, "") }
	t.Cleanup(func() { executablePath, openDatabaseForPreparation = oldExe, oldOpen })
	valid := versionedFixture("    executor: {type: db, source: primary, sql: \"select 'PRIVATE_SQL'\"}\n")
	for name, data := range map[string]string{
		"invalid_schema":       strings.Replace(valid, "schema_version: 1", "schema_version: 2", 1),
		"unknown_provider":     versionedFixture("    executor: {type: PRIVATE_UNKNOWN_EXECUTOR}\n"),
		"unavailable_executor": versionedFixture("    executor: {type: wasm, module: ./PRIVATE_MODULE.wasm}\n"),
		"db_failure":           valid,
	} {
		for _, entry := range []string{"startup", "validate", "doctor"} {
			t.Run(entry+"/"+name, func(t *testing.T) {
				preflightDriver.reset()
				if name == "db_failure" {
					preflightDriver.queryErr = errors.New("PRIVATE_DB_ERROR password=PRIVATE_PASSWORD")
				}
				path := writeCapabilityFixture(t, data)
				var stdout, stderr bytes.Buffer
				if entry == "startup" {
					_, err := NewRunner(context.Background(), Config{CapabilityFile: path}, &stdout)
					if err == nil || strings.Contains(err.Error(), "PRIVATE_") {
						t.Fatalf("startup accepted configuration or exposed detail: %v", err)
					}
				} else {
					handled, code := HandleCLI(context.Background(), []string{entry, "--config", path, "--format", "json"}, &stdout, &stderr)
					var output map[string]any
					statusKey := "ok"
					if entry == "validate" {
						statusKey = "valid"
					}
					if !handled || code != 1 || json.Unmarshal(stdout.Bytes(), &output) != nil || output[statusKey] != false {
						t.Fatalf("%s accepted configuration: %s %s", entry, &stdout, &stderr)
					}
				}
				if strings.Contains(stdout.String()+stderr.String(), "PRIVATE_") {
					t.Fatal("entrypoint exposed configuration/driver details")
				}
				got := strings.Join(preflightDriver.snapshot(), ",")
				if name == "db_failure" {
					if got != "open,ping,explain,close" {
						t.Fatalf("failed versioned DB preparation did not close: %s", got)
					}
				} else if got != "" {
					t.Fatalf("invalid/unavailable configuration opened DB: %s", got)
				}
			})
		}
	}
}

func TestDBPublicationValidationBeforeCommit(t *testing.T) {
	for _, tc := range []struct {
		name, output             string
		rows                     int64
		commit, rollback         error
		code, outcome, wantCalls string
	}{
		{name: "valid", output: "    output: {properties: {count: {type: integer, maximum: 2}}, data: {}}\n", rows: 1, wantCalls: "begin,exec,rows,commit"},
		{name: "output_constraint", output: "    output: {properties: {count: {type: integer, maximum: 0}}, data: {}}\n", rows: 1, code: "OUTPUT_INVALID", outcome: "rolled_back", wantCalls: "begin,exec,rows,rollback"},
		{name: "capacity", output: "    output: {data: {}}\n    policy: {readonly: false, max_output_bytes: 1}\n", rows: 1, code: "LIMIT_EXCEEDED", outcome: "rolled_back", wantCalls: "begin,exec,rows,rollback"},
		{name: "suppressed", rows: 2, wantCalls: "begin,exec,rows,commit"},
		{name: "commit_unknown", rows: 1, commit: errors.New("private commit error"), code: "OUTCOME_UNKNOWN", outcome: "unknown", wantCalls: "begin,exec,rows,commit"},
		{name: "rollback_unknown", output: "    output: {properties: {count: {type: integer, maximum: 0}}, data: {}}\n", rows: 1, rollback: errors.New("private rollback error"), code: "OUTCOME_UNKNOWN", outcome: "unknown", wantCalls: "begin,exec,rows,rollback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "    executor: {type: db, source: primary, sql: update t set x=1}\n    policy: {readonly: false}\n" + tc.output
			if tc.name == "capacity" {
				body = strings.Replace(body, "    policy: {readonly: false}\n", "", 1)
			}
			cfg := versionedCompile(t, body)
			state := &mutationDriverState{rows: tc.rows, commitErr: tc.commit, rollbackErr: tc.rollback}
			db := mutationRunner(t, state).db
			registry, _ := providers.NewRegistry(map[string]providers.Factory{"db": func(_ context.Context, s *providers.Snapshot) (providers.Provider, error) {
				return &dbProvider{db: db, database: DatabaseDef{Driver: "postgres"}, cap: s.Definition()}, nil
			}})
			prepared, e := registry.Prepare(context.Background(), cfg, "/tmp/config.yaml", providers.ResolveOptions{})
			if e != nil {
				t.Fatal(e)
			}
			defer prepared.Close()
			c, e := NewExecutionController(prepared, nil, nil)
			if e != nil {
				t.Fatal(e)
			}
			out, failure := c.Execute(context.Background(), ExecutionRequest{ID: "one-id", Capability: "test", Deadline: time.Now().Add(time.Second), Params: map[string]any{}})
			if tc.code != "" {
				if failure == nil || failure.Code != tc.code || failure.Outcome != tc.outcome {
					t.Fatalf("failure=%+v", failure)
				}
			} else {
				if failure != nil {
					t.Fatal(failure)
				}
				defer out.Close()
				if out.AffectedRows == nil || *out.AffectedRows != tc.rows {
					t.Fatal("unmeasured execution")
				}
				if tc.name == "suppressed" && out.Data != nil {
					t.Fatal("omitted output exposed data")
				}
			}
			state.mu.Lock()
			calls := strings.Join(state.calls, ",")
			state.mu.Unlock()
			if calls != tc.wantCalls {
				t.Fatalf("calls=%s, want %s", calls, tc.wantCalls)
			}
		})
	}
}

func TestDBValuesNormalizeBeforeOutputValidation(t *testing.T) {
	for _, input := range []any{float64(9007199254740993), float64(9223372036854775807), float64(-9223372036854775808), float64(1), float32(1)} {
		if _, err := normalizeDBValue(input, "ORACLE_INTEGER"); err == nil {
			t.Fatal("accepted floating-point scale-zero NUMBER")
		}
	}
	for _, input := range []string{"9223372036854775808", "-9223372036854775809"} {
		if _, err := normalizeDBValue(input, "ORACLE_INTEGER"); err == nil {
			t.Fatal("accepted scale-zero NUMBER outside int64")
		}
	}
	for _, kind := range []string{"NUMERIC", "DECIMAL", "NUMBER"} {
		for _, input := range []any{float64(9007199254740993), float64(0.1), float32(1.25)} {
			if _, err := normalizeDBValue(input, kind); err == nil {
				t.Fatalf("accepted inexact decimal %s", kind)
			}
		}
	}
	for _, tc := range []struct {
		kind        string
		input, want any
	}{{"BYTEA", []byte{0, 255}, "AP8="}, {"BLOB", []byte{1}, "AQ=="}, {"TEXT", []byte("日本"), "日本"}, {"NUMERIC", []byte("123.456789012345678901"), "123.456789012345678901"}, {"ORACLE_INTEGER", "12", int64(12)}, {"ORACLE_INTEGER", "9007199254740993", int64(9007199254740993)}, {"ORACLE_INTEGER", "9223372036854775807", int64(9223372036854775807)}, {"ORACLE_INTEGER", []byte("-9223372036854775808"), int64(-9223372036854775808)}, {"ORACLE_INTEGER", int64(9007199254740993), int64(9007199254740993)}, {"INTEGER", int64(12), int64(12)}, {"TEXT", nil, nil}} {
		got, e := normalizeDBValue(tc.input, tc.kind)
		if e != nil || got != tc.want {
			t.Fatal(tc.kind, got, e)
		}
	}
	if _, e := normalizeDBValue([]byte{255}, "VARCHAR"); e == nil {
		t.Fatal("invalid UTF-8 text accepted")
	}
	cap := versionedCompile(t, "    executor: {type: db, source: primary, sql: select 1}\n    output: {items: {properties: {id: {type: integer}}}, data: {}}\n").Capabilities["test"]
	if _, e := cap.ProjectOutput([]any{map[string]any{"id": "1"}}, []string{"id"}); e == nil {
		t.Fatal("declared output coerced database text to integer")
	}
}

func TestVersionedRunnerRejectsMissingOrMismatchedGatewayMajor(t *testing.T) {
	for _, major := range []string{"", "2"} {
		t.Run("major-"+major, func(t *testing.T) {
			accepted := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/ws/agent/challenge" {
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, `{"challenge":"major-test-challenge"}`)
					return
				}
				if req.Header.Get(protocol.MajorHeader) != protocol.Major {
					t.Error("Agent did not send its major")
				}
				if major != "" {
					w.Header().Set(protocol.MajorHeader, major)
				}
				conn, err := ws.Accept(w, req)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				accepted <- struct{}{}
				if msg, err := conn.ReadText(); err == nil {
					t.Errorf("Agent sent business/control data to invalid peer: %s", msg)
				}
			}))
			defer server.Close()
			logs := &bytes.Buffer{}
			runner := &Runner{cf: &CapabilityFile{Gateway: GatewayDef{URL: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/agent", AgentPrivateKey: testAgentPrivateKey}}, cfg: Config{ReconnectEvery: time.Second}, logOut: logs}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			select {
			case <-accepted:
			case <-time.After(3 * time.Second):
				t.Fatal("handshake not attempted")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Runner did not stop")
			}
			if strings.Contains(logs.String(), `"event":"gateway_connected"`) || !strings.Contains(logs.String(), "unsupported gateway protocol major") {
				t.Fatalf("logs=%s", logs.String())
			}
		})
	}
}

// Run the public loader, signed handshake and WS reader admission together.
// The DB seam only makes preparation independent of a local database.
func TestVersionedSignedWebSocketRejectsMissingAndMismatchedHashes(t *testing.T) {
	preflightDriver.reset()
	oldExe, oldOpen := executablePath, openDatabaseForPreparation
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	openDatabaseForPreparation = func(DatabaseDef) (*sql.DB, error) { return sql.Open(preflightTestDriverName, "") }
	t.Cleanup(func() { executablePath, openDatabaseForPreparation = oldExe, oldOpen })
	privateKey, err := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := ed25519.PrivateKey(privateKey).Public().(ed25519.PublicKey)
	checked := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/ws/agent/challenge" {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"challenge":"hash-admission-challenge"}`)
			return
		}
		signature, err := base64.RawURLEncoding.DecodeString(req.Header.Get("X-Agent-Signature"))
		message := protocol.AgentAuthMessage(req.URL.Path, req.Header.Get("X-Agent-Timestamp"), req.Header.Get("X-Agent-Nonce"), req.Header.Get("X-Agent-Challenge"), req.Header.Get("Sec-WebSocket-Key"))
		if err != nil || !ed25519.Verify(publicKey, message, signature) || req.Header.Get("X-Agent-Challenge") != "hash-admission-challenge" || req.Header.Get(protocol.MajorHeader) != protocol.Major {
			http.Error(w, "invalid handshake", http.StatusUnauthorized)
			checked <- errors.New("signed major-1 handshake was not verified")
			return
		}
		w.Header().Set(protocol.MajorHeader, protocol.Major)
		conn, err := ws.Accept(w, req)
		if err != nil {
			checked <- err
			return
		}
		defer conn.Close()
		for _, tc := range []struct{ hash, code string }{{"", "INVALID_REQUEST"}, {strings.Repeat("f", 64), "CONFLICT"}} {
			request := protocol.Request{Type: "request", ID: "hash-admission-" + tc.code, Capability: "test", DefinitionHash: tc.hash, Deadline: time.Now().Add(time.Second)}
			if err := conn.WriteText(protocol.MustJSON(request)); err != nil {
				checked <- err
				return
			}
			message, err := conn.ReadText()
			var response protocol.Response
			if err != nil || json.Unmarshal(message, &response) != nil || response.ID != request.ID || response.Error == nil || response.Error.Code != tc.code || response.Error.Outcome != "not_started" {
				checked <- errors.New("WS hash admission did not return the expected code and outcome")
				return
			}
		}
		checked <- nil
	}))
	defer server.Close()
	data := strings.Replace(versionedFixture("    executor: {type: db, source: primary, sql: select 1}\n"), "ws://127.0.0.1:8080", "ws"+strings.TrimPrefix(server.URL, "http"), 1)
	data += "runtime: {identity_dir: " + string(protocol.MustJSON(filepath.Join(dir, "identity"))) + "}\n"
	runner, err := NewRunner(context.Background(), Config{CapabilityFile: writeCapabilityFixture(t, data), ReconnectEvery: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Runner did not stop")
		}
	}()
	select {
	case err := <-checked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("signed WS admission timed out")
	}
	if got := strings.Join(preflightDriver.snapshot(), ","); got != "open,ping,explain" {
		t.Fatalf("hash rejection reached DB execution: %s", got)
	}
}
