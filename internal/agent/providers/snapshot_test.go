package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/viewlegacy/onprest/internal/contract"
)

func config(t *testing.T, body string) *contract.Config {
	t.Helper()
	c, e := contract.Compile([]byte("schema_version: 1\ngateway: {url: 'ws://localhost/ws/agent', agent_private_key: '" + base64.RawURLEncoding.EncodeToString(make([]byte, 64)) + "'}\n" + body))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func resolve(t *testing.T, c *contract.Config, path, name string) *Snapshot {
	t.Helper()
	s, e := Resolve(context.Background(), c, path, name, ResolveOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func secretFile(t *testing.T, path string, b []byte) {
	t.Helper()
	f, e := createPrivate(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write(b); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
}

const databaseConfig = "sources:\n  databases:\n    main: {driver: postgres, host: db.internal, port: 5432, name: crm, user: app, password: secret}\n    unrelated: {driver: postgres, host: other.internal, port: 5432, name: other, user: app, password_file: missing}\ncapabilities:\n  read:\n    executor: {type: db, source: main, sql: 'SELECT id FROM customers'}\n    output: {data: {}}\n"
const fileConfig = "sources:\n  folders:\n    reports: {root: ./reports, access: read}\ncapabilities:\n  read:\n    executor: {type: file, read: {source: reports, selector: {name: report.pdf}}}\n    output: {files: {}}\n"

func TestPreparedDefinitionSourceHashAndSecretSnapshots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	c := config(t, databaseConfig)
	original := resolve(t, c, path, "read")
	hash := original.DefinitionHash()
	if len(hash) != 64 {
		t.Fatal(hash)
	}
	db, ok := original.Source(SourceKey{"databases", "main"})
	if !ok || db["password"] != "secret" {
		t.Fatal(db)
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*contract.Config)
		changes bool
	}{
		{"host", func(c *contract.Config) { c.Sources.Databases["main"]["host"] = "new.internal" }, true},
		{"driver", func(c *contract.Config) { c.Sources.Databases["main"]["driver"] = "mysql" }, true},
		{"database", func(c *contract.Config) { c.Sources.Databases["main"]["name"] = "new" }, true},
		{"port", func(c *contract.Config) { c.Sources.Databases["main"]["port"] = int64(5433) }, true},
		{"user", func(c *contract.Config) { c.Sources.Databases["main"]["user"] = "other" }, true},
		{"password", func(c *contract.Config) { c.Sources.Databases["main"]["password"] = "different" }, true},
		{"tls", func(c *contract.Config) { c.Sources.Databases["main"]["tls"].(map[string]any)["mode"] = "require" }, true},
		{"effective_policy", func(c *contract.Config) { c.Capabilities["read"].Policy["timeout"] = "6s" }, true},
		{"unrelated_source", func(c *contract.Config) { c.Sources.Databases["unrelated"]["host"] = "changed" }, false},
		{"logging", func(c *contract.Config) { c.Logging["level"] = "debug" }, false},
		{"presentation_metadata", func(c *contract.Config) {
			c.Capabilities["read"].Description = "new description"
			c.Capabilities["read"].Annotations = map[string]any{"read_only": true}
			c.Capabilities["read"].Examples = []map[string]any{{"params": map[string]any{}}}
		}, false},
		{"runtime_spool", func(c *contract.Config) { c.Runtime["spool_total_bytes"] = int64(1) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := config(t, databaseConfig)
			tc.mutate(next)
			got := resolve(t, next, path, "read").DefinitionHash()
			if (got != hash) != tc.changes {
				t.Fatalf("hash change %v wanted %v", got != hash, tc.changes)
			}
		})
	}
	secretFile(t, filepath.Join(dir, "password"), []byte("secret"))
	fromFile := config(t, strings.Replace(databaseConfig, "password: secret", "password_file: ./password", 1))
	if resolve(t, fromFile, path, "read").DefinitionHash() != hash {
		t.Fatal("storage method changed hash")
	}
	if e := os.WriteFile(filepath.Join(dir, "password"), []byte("new-secret"), 0o600); e != nil {
		t.Fatal(e)
	}
	next := resolve(t, fromFile, path, "read")
	if next.DefinitionHash() == hash {
		t.Fatal("changed bytes retained hash")
	}
	if db["password"] != "secret" {
		t.Fatal("old snapshot changed")
	}
	getter := original.Definition()
	getter.Input.Properties["injected"] = &contract.Schema{}
	getter.Policy["timeout"] = "1s"
	getter.Executor.Settings["sql"] = "DELETE FROM customers"
	db["password"] = "mutation"
	db["tls"].(map[string]any)["mode"] = "require"
	if original.Definition().Policy["timeout"] != "5s" || original.Definition().Executor.Settings["sql"] != "SELECT id FROM customers" {
		t.Fatal("mutable definition escaped")
	}
	db2, _ := original.Source(SourceKey{"databases", "main"})
	if db2["password"] != "secret" || db2["tls"].(map[string]any)["mode"] != "disable" {
		t.Fatal("mutable source escaped")
	}
}

func TestPreparedHashNormalizationPresenceAndIdentity(t *testing.T) {
	for _, body := range []string{databaseConfig, fileConfig} {
		t.Run(strings.Split(body, "\n")[1], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			c := config(t, body)
			first := resolve(t, c, path, "read").DefinitionHash()
			if resolve(t, c, path, "read").DefinitionHash() != first {
				t.Fatal("restart instability")
			}
			if e := os.Remove(filepath.Join(dir, "identity", "config-hmac.key")); e != nil {
				t.Fatal(e)
			}
			if resolve(t, c, path, "read").DefinitionHash() == first {
				t.Fatal("lost identity did not invalidate")
			}
			before := resolve(t, c, path, "read").DefinitionHash()
			if e := os.WriteFile(filepath.Join(dir, "identity", "config-hmac.key"), bytes.Repeat([]byte{9}, 32), 0o600); e != nil {
				t.Fatal(e)
			}
			if resolve(t, c, path, "read").DefinitionHash() == before {
				t.Fatal("rotated identity did not invalidate")
			}
		})
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := resolve(t, config(t, databaseConfig), path, "read").DefinitionHash()
	for _, prefix := range []string{"defaults: {max_rows: 100, timeout: 5000ms, max_output_bytes: 1024KiB}\n", "defaults: {mcp_inline_total_bytes: 2MiB}\n"} {
		if resolve(t, config(t, prefix+databaseConfig), path, "read").DefinitionHash() != base {
			t.Fatal("equivalent/inapplicable defaults changed hash")
		}
	}
	missing := config(t, strings.Replace(databaseConfig, ", password: secret", "", 1))
	empty := config(t, strings.Replace(databaseConfig, "password: secret", "password: ''", 1))
	if resolve(t, missing, path, "read").DefinitionHash() == resolve(t, empty, path, "read").DefinitionHash() {
		t.Fatal("missing/empty password collapsed")
	}
	secretFile(t, filepath.Join(dir, "empty"), nil)
	emptyFile := config(t, strings.Replace(databaseConfig, "password: secret", "password_file: empty", 1))
	if resolve(t, emptyFile, path, "read").DefinitionHash() != resolve(t, empty, path, "read").DefinitionHash() {
		t.Fatal("empty file/inline mismatch")
	}
	if secretTag([]byte("key"), "a", "bc", []byte("d")) == secretTag([]byte("key"), "ab", "c", []byte("d")) {
		t.Fatal("ambiguous HMAC context")
	}
	if secretTag([]byte("key"), "a", "password", []byte("d")) == secretTag([]byte("key"), "a", "token", []byte("d")) {
		t.Fatal("missing HMAC field domain")
	}
}

func TestPreparedTLSMaterialAndRootsAreFixed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	for _, name := range []string{"ca", "cert", "key"} {
		secretFile(t, filepath.Join(dir, name), []byte(name+"-bytes"))
	}
	c := config(t, strings.Replace(databaseConfig, "password: secret", "password: secret, tls: {mode: verify-full, ca_file: ca, cert_file: cert, key_file: key, server_name: db.example}", 1))
	first := resolve(t, c, path, "read")
	for _, name := range []string{"ca", "cert", "key"} {
		t.Run(name, func(t *testing.T) {
			if e := os.WriteFile(filepath.Join(dir, name), []byte("changed-"+name), 0o600); e != nil {
				t.Fatal(e)
			}
			next := resolve(t, c, path, "read")
			if next.DefinitionHash() == first.DefinitionHash() {
				t.Fatal("TLS bytes not hashed")
			}
			old, _ := first.Source(SourceKey{"databases", "main"})
			if string(old["tls"].(map[string]any)[name+"_bytes"].([]byte)) != name+"-bytes" {
				t.Fatal("TLS snapshot reread")
			}
			if e := os.WriteFile(filepath.Join(dir, name), []byte(name+"-bytes"), 0o600); e != nil {
				t.Fatal(e)
			}
		})
	}
	f := resolve(t, config(t, fileConfig), path, "read")
	root, _ := f.Source(SourceKey{"folders", "reports"})
	if root["root"] != filepath.Join(dir, "reports") {
		t.Fatal(root)
	}
	for _, replacement := range []string{"{root: ./changed, access: read}", "{root: ./reports, access: read_write}"} {
		next := config(t, strings.Replace(fileConfig, "{root: ./reports, access: read}", replacement, 1))
		if resolve(t, next, path, "read").DefinitionHash() == f.DefinitionHash() {
			t.Fatal("root/access not hashed")
		}
	}
}

func TestPreparedHashFilePolicyAndDBSecretBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	for _, rest := range []string{"reference", "direct"} {
		for _, mcp := range []string{"reference", "inline"} {
			t.Run(rest+"_"+mcp, func(t *testing.T) {
				body := strings.Replace(fileConfig, "output: {files: {}}", "output: {files: {delivery: {rest: "+rest+", mcp: "+mcp+"}}}", 1)
				original := resolve(t, config(t, body), path, "read").DefinitionHash()
				if changed := resolve(t, config(t, "defaults: {file_ttl: 20m}\n"+body), path, "read").DefinitionHash(); changed == original {
					t.Fatal("file TTL not hashed for delivery")
				}
				changed := resolve(t, config(t, "defaults: {mcp_inline_total_bytes: 2MiB}\n"+body), path, "read").DefinitionHash()
				if (changed != original) != (mcp == "inline") {
					t.Fatal("inline policy applicability not reflected in hash")
				}
			})
		}
	}
	secretFile(t, filepath.Join(dir, "db-password"), []byte("secret\n"))
	fromFile := config(t, strings.Replace(databaseConfig, "password: secret", "password_file: db-password", 1))
	inline := config(t, strings.Replace(databaseConfig, "password: secret", `password: "secret\n"`, 1))
	fileSnapshot := resolve(t, fromFile, path, "read")
	if fileSnapshot.DefinitionHash() != resolve(t, inline, path, "read").DefinitionHash() {
		t.Fatal("password bytes were trimmed")
	}
	db, _ := fileSnapshot.Source(SourceKey{"databases", "main"})
	if db["password"] != "secret\n" {
		t.Fatal("password byte value changed")
	}
}

func TestPreparedHTTPSecretsAndFailClosedRead(t *testing.T) {
	body := "sources:\n  http:\n    api: {base_url: 'https://example.internal/api', allowed_methods: [GET], credential: token}\ncapabilities:\n  read:\n    executor: {type: http, source: api, method: GET, path: /data, response: {kind: json}}\n    output: {data: {}}\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	first := resolve(t, config(t, body), path, "read")
	secretFile(t, filepath.Join(dir, "token"), []byte("token"))
	fileBody := strings.Replace(body, "credential: token", "credential_file: token", 1)
	if resolve(t, config(t, fileBody), path, "read").DefinitionHash() != first.DefinitionHash() {
		t.Fatal("token storage changed hash")
	}
	for _, content := range [][]byte{nil, []byte("token\n"), []byte("Bearer token"), {0xff}, bytes.Repeat([]byte("a"), secretLimit+1)} {
		t.Run("invalid_token", func(t *testing.T) {
			if e := os.WriteFile(filepath.Join(dir, "token"), content, 0o600); e != nil {
				t.Fatal(e)
			}
			_, e := Resolve(context.Background(), config(t, fileBody), path, "read", ResolveOptions{})
			if e == nil || strings.Contains(e.Error(), "Bearer token") {
				t.Fatal("unsafe or missing rejection", e)
			}
		})
	}
	for _, source := range []string{"missing", "directory"} {
		t.Run(source, func(t *testing.T) {
			if source == "directory" {
				if e := os.Mkdir(filepath.Join(dir, source), 0o700); e != nil {
					t.Fatal(e)
				}
			}
			c := config(t, strings.Replace(body, "credential: token", "credential_file: "+source, 1))
			if _, e := Resolve(context.Background(), c, path, "read", ResolveOptions{}); e == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
	for _, modify := range []func(*contract.Config){func(c *contract.Config) { c.Sources.HTTP["api"]["base_url"] = "https://new.internal/api" }, func(c *contract.Config) { c.Sources.HTTP["api"]["allowed_cidrs"] = []string{"10.0.0.0/8"} }, func(c *contract.Config) { c.Sources.HTTP["api"]["credential"] = "other" }, func(c *contract.Config) { c.Sources.HTTP["api"]["headers"].(map[string]any)["x-fixed"] = "fixed" }} {
		c := config(t, body)
		modify(c)
		if resolve(t, c, path, "read").DefinitionHash() == first.DefinitionHash() {
			t.Fatal("HTTP execution config not hashed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := Resolve(ctx, config(t, body), path, "read", ResolveOptions{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestPreparedWasmTrustedMaterialBaseAndEnvironment(t *testing.T) {
	body := "capabilities:\n  run:\n    executor:\n      type: wasm\n      module: module.wasm\n      sandbox:\n        enabled: true\n        permissions:\n          filesystem: [{root: ./files, access: read}]\n          environment: [REGION]\n    output: {data: {}}\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := config(t, body)
	opts := ResolveOptions{ModuleDigests: map[string]string{"run": strings.Repeat("a", 64)}, Environment: map[string]string{"REGION": "private-region", "UNRELATED": "other"}}
	if _, e := Resolve(context.Background(), c, path, "run", ResolveOptions{}); e == nil {
		t.Fatal("missing module snapshot accepted")
	}
	first, e := Resolve(context.Background(), c, path, "run", opts)
	if e != nil {
		t.Fatal(e)
	}
	if first.Environment()["REGION"] != "private-region" || len(first.Environment()) != 1 {
		t.Fatal(first.Environment())
	}
	grant := first.Definition().Executor.Settings["sandbox"].(map[string]any)["permissions"].(map[string]any)["filesystem"].([]any)[0].(map[string]any)
	if grant["root"] != filepath.Join(dir, "files") {
		t.Fatal(grant)
	}
	opts.Environment["UNRELATED"] = "changed"
	same, e := Resolve(context.Background(), c, path, "run", opts)
	if e != nil || same.DefinitionHash() != first.DefinitionHash() {
		t.Fatal("unregistered environment affected strict hash", e)
	}
	opts.Environment["REGION"] = "new-region"
	changed, e := Resolve(context.Background(), c, path, "run", opts)
	if e != nil || changed.DefinitionHash() == first.DefinitionHash() {
		t.Fatal("environment secret not hashed", e)
	}
	if first.Environment()["REGION"] != "private-region" {
		t.Fatal("environment reread")
	}
	getter := first.Environment()
	getter["REGION"] = "injected"
	if first.Environment()["REGION"] != "private-region" {
		t.Fatal("environment getter mutable")
	}
	opts.ModuleDigests["run"] = strings.Repeat("b", 64)
	changed, e = Resolve(context.Background(), c, path, "run", opts)
	if e != nil || changed.DefinitionHash() == first.DefinitionHash() {
		t.Fatal("module digest not hashed", e)
	}
	// Hold identity and grant root constant so only the Wasm relative-operation
	// base can explain this hash change.
	c.Runtime["identity_dir"] = filepath.Join(dir, "identity")
	c.Capabilities["run"].Executor.Settings["sandbox"].(map[string]any)["permissions"].(map[string]any)["filesystem"].([]any)[0].(map[string]any)["root"] = filepath.Join(dir, "files")
	other, e := Resolve(context.Background(), c, filepath.Join(t.TempDir(), "config.yaml"), "run", opts)
	if e != nil || other.DefinitionHash() == changed.DefinitionHash() {
		t.Fatal("base not hashed", e)
	}
	// Expected digest presence is not a distinct definition field.
	opts.Environment["REGION"] = "private-region"
	opts.ModuleDigests["run"] = strings.Repeat("a", 64)
	c.Capabilities["run"].Executor.Settings["sha256"] = strings.Repeat("a", 64)
	same, e = Resolve(context.Background(), c, path, "run", opts)
	if e != nil || same.DefinitionHash() != first.DefinitionHash() {
		t.Fatal("expected hash presence not normalized", e)
	}
}

func TestPreparedIdentityConcurrentPublicationAndCorruption(t *testing.T) {
	dir := t.TempDir()
	identity := filepath.Join(dir, "identity")
	var wg sync.WaitGroup
	keys := make(chan []byte, 16)
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); k, e := identityKey(identity); keys <- k; errs <- e }()
	}
	wg.Wait()
	close(keys)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var first []byte
	for k := range keys {
		if first == nil {
			first = k
		} else if !bytes.Equal(k, first) {
			t.Fatal("concurrent preparers used different keys")
		}
	}
	entries, e := os.ReadDir(identity)
	if e != nil || len(entries) != 1 || entries[0].Name() != "config-hmac.key" {
		t.Fatal(entries, e)
	}
	if e := os.WriteFile(filepath.Join(identity, "config-hmac.key"), []byte("broken"), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e := identityKey(identity); e == nil {
		t.Fatal("corrupt key replaced silently")
	}
}
