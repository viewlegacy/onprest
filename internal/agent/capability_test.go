package agent

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

const testAgentPrivateKey = "keEk2aSPeUHiCbhK-XxleMUFj3cwzcJCFUflKSs_CiZOsybztXdoRPcyYZTMd_f9cplE8Qd7VsMz484fWauOvw"

func TestLoadCapabilityFileYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capability.yaml")
	err := os.WriteFile(path, []byte(`
service:
  title: Test
  version: 0.1.0
database:
  driver: postgres
  host: localhost
  port: 5432
  name: legacy
  user: readonly_user
  password: secret
gateway:
  url: ws://localhost:8080/ws/agent
  agent_private_key: keEk2aSPeUHiCbhK-XxleMUFj3cwzcJCFUflKSs_CiZOsybztXdoRPcyYZTMd_f9cplE8Qd7VsMz484fWauOvw
capabilities:
  get_customer:
    sql: select * from customers where id = :id
    params:
      id:
        type: integer
        required: true
    policy:
      timeout: 1s
      max_rows: 1
      max_bytes: 128KB
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := LoadCapabilityFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cf.Capabilities["get_customer"].Name != "get_customer" {
		t.Fatalf("unexpected capability: %#v", cf.Capabilities["get_customer"])
	}
	if cf.Runtime.MaxConcurrentRequests == nil || *cf.Runtime.MaxConcurrentRequests != 16 {
		t.Fatalf("runtime.max_concurrent_requests = %v, want default 16", cf.Runtime.MaxConcurrentRequests)
	}
}

func TestRepositoryExampleCapabilityFileLoads(t *testing.T) {
	cf, err := LoadCapabilityFile(filepath.Join("..", "..", "examples", "capability.postgres.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cf.Runtime.MaxConcurrentRequests == nil || *cf.Runtime.MaxConcurrentRequests != 16 {
		t.Fatalf("example runtime.max_concurrent_requests = %v, want 16", cf.Runtime.MaxConcurrentRequests)
	}
}

func TestCapabilityFileRuntimeMaxConcurrentRequests(t *testing.T) {
	tests := []struct {
		name    string
		runtime string
		want    int
		wantErr string
	}{
		{name: "custom", runtime: "runtime:\n  max_concurrent_requests: 3\n", want: 3},
		{name: "zero", runtime: "runtime:\n  max_concurrent_requests: 0\n", wantErr: "runtime.max_concurrent_requests must be > 0"},
		{name: "negative", runtime: "runtime:\n  max_concurrent_requests: -1\n", wantErr: "runtime.max_concurrent_requests must be > 0"},
		{name: "not integer", runtime: "runtime:\n  max_concurrent_requests: many\n", wantErr: "cannot unmarshal"},
		{name: "unknown field", runtime: "runtime:\n  max_concurrent_request: 3\n", wantErr: "field max_concurrent_request not found"},
		{name: "multiple documents", runtime: "runtime: {}\n---\n", wantErr: "multiple YAML documents"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capability.yaml")
			content := tc.runtime + `database:
  driver: postgres
  host: localhost
  port: 5432
  name: legacy
  user: readonly_user
gateway:
  url: ws://localhost:8080/ws/agent
  agent_private_key: keEk2aSPeUHiCbhK-XxleMUFj3cwzcJCFUflKSs_CiZOsybztXdoRPcyYZTMd_f9cplE8Qd7VsMz484fWauOvw
capabilities:
  get_customer:
    sql: select 1 as id
    result:
      id:
        type: integer
`
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cf, err := LoadCapabilityFile(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadCapabilityFile() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cf.Runtime.MaxConcurrentRequests == nil || *cf.Runtime.MaxConcurrentRequests != tc.want {
				t.Fatalf("runtime.max_concurrent_requests = %v, want %d", cf.Runtime.MaxConcurrentRequests, tc.want)
			}
		})
	}
}

func TestCapabilityFileRejectsWriteSQLWhenReadonly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capability.yaml")
	err := os.WriteFile(path, []byte(`
database:
  driver: postgres
  host: localhost
  port: 5432
  name: legacy
  user: readonly_user
gateway:
  url: ws://localhost:8080/ws/agent
  agent_private_key: keEk2aSPeUHiCbhK-XxleMUFj3cwzcJCFUflKSs_CiZOsybztXdoRPcyYZTMd_f9cplE8Qd7VsMz484fWauOvw
capabilities:
  update_customer:
    sql: update customers set name = :name
    params:
      name:
        type: string
    policy:
      readonly: true
      timeout: 1s
      max_rows: 1
      max_bytes: 128KB
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCapabilityFile(path); err == nil {
		t.Fatal("LoadCapabilityFile() error = nil, want readonly SQL error")
	}
}

func TestReadOnlySQLRejectsWithAndAllowsSelect(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{name: "select", query: "select id from customers where id = :id", want: true},
		{name: "leading comments select", query: "-- comment\n/* block */\nSELECT id FROM customers", want: true},
		{name: "with select", query: "with recent as (select id from customers) select id from recent", want: false},
		{name: "with insert cte", query: "with inserted as (insert into customers(name) values (:name) returning id) select id from inserted", want: false},
		{name: "update", query: "update customers set name = :name", want: false},
		{name: "multiple statements", query: "select 1; update customers set name = 'x'", want: false},
		{name: "semicolon in string", query: "select ';' as value", want: true},
		{name: "semicolon in escaped string", query: `select 'it''s;fine' as value`, want: true},
		{name: "semicolon in line comment", query: "select 1 -- ; ignored\n", want: true},
		{name: "semicolon in block comment", query: "select /* ; ignored */ 1", want: true},
		{name: "semicolon in dollar quote", query: "select $$; ignored$$", want: true},
		{name: "trailing semicolon", query: "select 1; -- trailing comment", want: true},
		{name: "second empty statement", query: "select 1;;", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadOnlySQL(tc.query); got != tc.want {
				t.Fatalf("isReadOnlySQL(%q) = %t, want %t", tc.query, got, tc.want)
			}
		})
	}
}

func TestCapabilityFileRejectsWithSQLWhenReadonly(t *testing.T) {
	cf := validCapabilityFile()
	cap := cf.Capabilities["get_customer"]
	cap.SQL = "with recent as (select id from customers) select id from recent"
	cf.Capabilities["get_customer"] = cap

	if err := cf.Lint(); err == nil {
		t.Fatal("Lint() error = nil, want readonly SQL error")
	}
}

func TestCapabilityFileLintRequiredFieldsAndPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CapabilityFile)
	}{
		{name: "gateway url", mutate: func(cf *CapabilityFile) { cf.Gateway.URL = "" }},
		{name: "private key missing", mutate: func(cf *CapabilityFile) { cf.Gateway.AgentPrivateKey = "" }},
		{name: "private key invalid", mutate: func(cf *CapabilityFile) { cf.Gateway.AgentPrivateKey = "bad" }},
		{name: "database driver", mutate: func(cf *CapabilityFile) { cf.Database.Driver = "sqlite" }},
		{name: "database host", mutate: func(cf *CapabilityFile) { cf.Database.Host = "" }},
		{name: "database port", mutate: func(cf *CapabilityFile) { cf.Database.Port = 0 }},
		{name: "database name", mutate: func(cf *CapabilityFile) { cf.Database.Name = "" }},
		{name: "database user", mutate: func(cf *CapabilityFile) { cf.Database.User = "" }},
		{name: "capabilities", mutate: func(cf *CapabilityFile) { cf.Capabilities = nil }},
		{name: "capability name", mutate: func(cf *CapabilityFile) {
			cap := cf.Capabilities["get_customer"]
			delete(cf.Capabilities, "get_customer")
			cf.Capabilities["1bad"] = cap
		}},
		{name: "sql", mutate: func(cf *CapabilityFile) {
			cap := cf.Capabilities["get_customer"]
			cap.SQL = ""
			cf.Capabilities["get_customer"] = cap
		}},
		{name: "timeout", mutate: func(cf *CapabilityFile) {
			cap := cf.Capabilities["get_customer"]
			cap.Policy.Timeout = "bad"
			cf.Capabilities["get_customer"] = cap
		}},
		{name: "max rows", mutate: func(cf *CapabilityFile) {
			cap := cf.Capabilities["get_customer"]
			cap.Policy.MaxRows = -1
			cf.Capabilities["get_customer"] = cap
		}},
		{name: "max bytes", mutate: func(cf *CapabilityFile) {
			cap := cf.Capabilities["get_customer"]
			cap.Policy.MaxBytes = "0B"
			cf.Capabilities["get_customer"] = cap
		}},
		{name: "result type", mutate: func(cf *CapabilityFile) {
			cap := cf.Capabilities["get_customer"]
			cap.Result = ResultDef{"bad": {Type: "object"}}
			cf.Capabilities["get_customer"] = cap
		}},
		{name: "logging max size", mutate: func(cf *CapabilityFile) { cf.Logging.MaxSize = "bad" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cf := validCapabilityFile()
			tc.mutate(cf)
			if err := cf.Lint(); err == nil {
				t.Fatal("Lint() error = nil, want error")
			}
		})
	}
}

func TestCapabilityFileDefaultsLogging(t *testing.T) {
	cf := validCapabilityFile()
	if err := cf.Lint(); err != nil {
		t.Fatal(err)
	}
	if cf.Logging.MaxSize != "10MB" || cf.Logging.MaxFiles != 3 {
		t.Fatalf("Logging = %#v, want default max_size/max_files", cf.Logging)
	}
}

func TestDatabaseDSNConnectionPolicy(t *testing.T) {
	tests := []struct {
		name string
		db   DatabaseDef
		want string
	}{
		{
			name: "postgres disables sslmode",
			db:   DatabaseDef{Driver: "postgres", Host: "db.example", Port: 5432, Name: "legacy", User: "readonly", Password: "secret"},
			want: "postgres://readonly:secret@db.example:5432/legacy?sslmode=disable",
		},
		{
			name: "postgres verify full TLS",
			db: DatabaseDef{Driver: "postgres", Host: "db.example", Port: 5432, Name: "legacy", User: "readonly", Password: "secret", TLS: DatabaseTLSDef{
				Mode: "verify-full", CAFile: "/certs/ca.pem", CertFile: "/certs/client.pem", KeyFile: "/certs/client.key",
			}},
			want: "postgres://readonly:secret@db.example:5432/legacy?sslcert=%2Fcerts%2Fclient.pem&sslkey=%2Fcerts%2Fclient.key&sslmode=verify-full&sslrootcert=%2Fcerts%2Fca.pem",
		},
		{
			name: "sqlserver disables encrypt",
			db:   DatabaseDef{Driver: "sqlserver", Host: "db.example", Port: 1433, Name: "legacy", User: "readonly", Password: "secret"},
			want: "sqlserver://readonly:secret@db.example:1433?database=legacy&encrypt=disable",
		},
		{
			name: "sqlserver verified TLS",
			db: DatabaseDef{Driver: "sqlserver", Host: "db.example", Port: 1433, Name: "legacy", User: "readonly", Password: "secret", TLS: DatabaseTLSDef{
				Mode: "verify-full", CAFile: "/certs/ca.pem", ServerName: "sql.internal.example",
			}},
			want: "sqlserver://readonly:secret@db.example:1433?TrustServerCertificate=false&certificate=%2Fcerts%2Fca.pem&database=legacy&encrypt=true&hostNameInCertificate=sql.internal.example",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.db.DSN(); got != tc.want {
				t.Fatalf("DSN() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMySQLDSNRoundTripsSpecialCredentials(t *testing.T) {
	db := DatabaseDef{Driver: "mysql", Host: "db.example", Port: 3306, Name: "legacy/name", User: "reader@domain/name", Password: `p@ss:/word?&=#`}
	dsn := db.DSN()
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", dsn, err)
	}
	if parsed.User != db.User || parsed.Passwd != db.Password || parsed.Addr != "db.example:3306" || parsed.DBName != db.Name {
		t.Fatalf("round-trip = user:%q password:%q addr:%q db:%q", parsed.User, parsed.Passwd, parsed.Addr, parsed.DBName)
	}
}

func TestDatabaseTLSLintRejectsInvalidOrUnsupportedConfiguration(t *testing.T) {
	tests := []DatabaseDef{
		{Driver: "postgres", Host: "db", Port: 5432, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "optional"}},
		{Driver: "mysql", Host: "db", Port: 3306, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "require"}},
		{Driver: "postgres", Host: "db", Port: 5432, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "verify-full", CertFile: "client.pem"}},
	}
	for _, db := range tests {
		if err := db.lint(); err == nil {
			t.Fatalf("lint(%#v) error = nil", db.TLS)
		}
	}
}

func TestCapabilityFileMergesPolicyDefaults(t *testing.T) {
	cf := validCapabilityFile()
	defaultReadonly := false
	capReadonly := true
	cf.Defaults = PolicyDef{Readonly: &defaultReadonly, Timeout: "30s", MaxRows: 1000, MaxBytes: "2MB"}
	cap := cf.Capabilities["get_customer"]
	cap.Policy = PolicyDef{Readonly: &capReadonly, MaxRows: 5}
	cf.Capabilities["get_customer"] = cap

	if err := cf.Lint(); err != nil {
		t.Fatal(err)
	}
	got := cf.Capabilities["get_customer"].Policy
	if !readonly(got) || got.Timeout != "30s" || got.MaxRows != 5 || got.MaxBytes != "2MB" {
		t.Fatalf("merged policy = %#v", got)
	}
}

func TestValidateParamsAppliesContract(t *testing.T) {
	minLen := 3
	maxLen := 12
	min := int64(1)
	max := int64(500)
	cap := CapabilityDef{
		Params: map[string]ParamDef{
			"status": {
				Type:      "string",
				Default:   "active",
				Enum:      []any{"active", "inactive"},
				MinLength: &minLen,
				MaxLength: &maxLen,
			},
			"limit": {
				Type:    "integer",
				Minimum: &min,
				Maximum: &max,
				Default: int64(50),
			},
		},
	}

	params, err := validateParams(cap, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if params["status"] != "active" || params["limit"] != int64(50) {
		t.Fatalf("unexpected defaulted params: %#v", params)
	}

	if _, err := validateParams(cap, map[string]any{"status": "deleted"}); err == nil {
		t.Fatal("validateParams() error = nil, want enum error")
	}
	if _, err := validateParams(cap, map[string]any{"limit": int64(501)}); err == nil {
		t.Fatal("validateParams() error = nil, want maximum error")
	}
}

func TestValidateIntegerParamsPreservesInt64AndEnumBoundaries(t *testing.T) {
	cap := CapabilityDef{Params: map[string]ParamDef{
		"id": {Type: "integer", Required: true, Enum: []any{json.Number("9007199254740993"), json.Number("9223372036854775807")}},
	}}
	for _, raw := range []string{"9007199254740993", "9223372036854775807"} {
		params, err := validateParams(cap, map[string]any{"id": json.Number(raw)})
		if err != nil {
			t.Fatalf("validate %s: %v", raw, err)
		}
		want, _ := json.Number(raw).Int64()
		if params["id"] != want {
			t.Fatalf("id %s = %#v, want %d", raw, params["id"], want)
		}
	}
	for _, value := range []any{json.Number("9223372036854775808"), json.Number("-9223372036854775809"), float64(9007199254740994)} {
		if _, err := validateParams(cap, map[string]any{"id": value}); err == nil {
			t.Fatalf("overflow/imprecise id %#v accepted", value)
		}
	}
}

func TestNumericParamsRejectNonFiniteValues(t *testing.T) {
	cap := CapabilityDef{Params: map[string]ParamDef{"value": {Type: "number", Required: true}}}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := validateParams(cap, map[string]any{"value": value}); err == nil {
			t.Fatalf("non-finite value %v accepted", value)
		}
	}
}

func TestValidateParamsRejectsContractViolations(t *testing.T) {
	minLen := 3
	maxLen := 5
	min := int64(1)
	max := int64(10)
	cap := CapabilityDef{Params: map[string]ParamDef{
		"required": {Type: "string", Required: true},
		"string":   {Type: "string", MinLength: &minLen, MaxLength: &maxLen, Pattern: `^[a-z]+$`},
		"integer":  {Type: "integer", Minimum: &min, Maximum: &max},
		"number":   {Type: "number", Minimum: &min, Maximum: &max},
		"boolean":  {Type: "boolean"},
		"email":    {Type: "string", Format: "email"},
		"uuid":     {Type: "string", Format: "uuid"},
		"date":     {Type: "string", Format: "date"},
		"datetime": {Type: "string", Format: "date-time"},
		"uri":      {Type: "string", Format: "uri"},
		"enum":     {Type: "string", Enum: []any{"red", "blue"}},
	}}
	valid := map[string]any{
		"required": "ok",
		"string":   "abc",
		"integer":  json.Number("5"),
		"number":   json.Number("5.5"),
		"boolean":  true,
		"email":    "dev@example.com",
		"uuid":     "550e8400-e29b-41d4-a716-446655440000",
		"date":     "2026-05-04",
		"datetime": "2026-05-04T12:00:00Z",
		"uri":      "https://example.com",
		"enum":     "red",
	}
	if _, err := validateParams(cap, valid); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{name: "unknown", mutate: func(in map[string]any) { in["unknown"] = "x" }, wantErr: "unknown param: unknown"},
		{name: "required", mutate: func(in map[string]any) { delete(in, "required") }, wantErr: "required param missing: required"},
		{name: "string type", mutate: func(in map[string]any) { in["string"] = 1 }, wantErr: "string: must be string"},
		{name: "integer type", mutate: func(in map[string]any) { in["integer"] = json.Number("1.5") }, wantErr: "integer: must be integer"},
		{name: "number type", mutate: func(in map[string]any) { in["number"] = "1" }, wantErr: "number: must be number"},
		{name: "boolean type", mutate: func(in map[string]any) { in["boolean"] = "true" }, wantErr: "boolean: must be boolean"},
		{name: "min length", mutate: func(in map[string]any) { in["string"] = "ab" }, wantErr: "string: below minLength"},
		{name: "max length", mutate: func(in map[string]any) { in["string"] = "abcdef" }, wantErr: "string: above maxLength"},
		{name: "pattern", mutate: func(in map[string]any) { in["string"] = "ABC" }, wantErr: "string: does not match pattern"},
		{name: "integer minimum", mutate: func(in map[string]any) { in["integer"] = json.Number("0") }, wantErr: "integer: below minimum"},
		{name: "integer maximum", mutate: func(in map[string]any) { in["integer"] = json.Number("11") }, wantErr: "integer: above maximum"},
		{name: "number minimum", mutate: func(in map[string]any) { in["number"] = json.Number("0") }, wantErr: "number: below minimum"},
		{name: "number maximum", mutate: func(in map[string]any) { in["number"] = json.Number("11") }, wantErr: "number: above maximum"},
		{name: "enum", mutate: func(in map[string]any) { in["enum"] = "green" }, wantErr: "enum: not in enum"},
		{name: "email", mutate: func(in map[string]any) { in["email"] = "bad" }, wantErr: "email: must be email"},
		{name: "uuid", mutate: func(in map[string]any) { in["uuid"] = "bad" }, wantErr: "uuid: must be uuid"},
		{name: "date", mutate: func(in map[string]any) { in["date"] = "2026-99-99" }, wantErr: "date: must be date"},
		{name: "date-time", mutate: func(in map[string]any) { in["datetime"] = "bad" }, wantErr: "datetime: must be date-time"},
		{name: "uri scheme", mutate: func(in map[string]any) { in["uri"] = "example.com" }, wantErr: "uri: must include URI scheme"},
		{name: "uri parse", mutate: func(in map[string]any) { in["uri"] = "http://[::1" }, wantErr: "uri: must be uri"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := cloneParams(valid)
			tc.mutate(input)
			_, err := validateParams(cap, input)
			if err == nil {
				t.Fatal("validateParams() error = nil, want error")
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("validateParams() error = %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestBuildOpenAPIReflectsServiceParamsResultAndExposePolicy(t *testing.T) {
	cf := validCapabilityFile()
	hidden := cf.Capabilities["get_customer"]
	hidden.Policy.ExposeInOpenAPI = boolPtr(false)
	cf.Capabilities["hidden"] = hidden
	cap := cf.Capabilities["get_customer"]
	cap.Result = ResultDef{"id": {Type: "integer", Description: "Customer ID"}}
	cf.Capabilities["get_customer"] = cap
	if err := cf.Lint(); err != nil {
		t.Fatal(err)
	}
	doc := BuildOpenAPI(cf)
	info := doc["info"].(map[string]any)
	if info["title"] != "Test Service" || info["version"] != "1.2.3" {
		t.Fatalf("info = %#v", info)
	}
	paths := doc["paths"].(map[string]any)
	if _, ok := paths["/api/v1/capabilities/hidden"]; ok {
		t.Fatalf("hidden capability exposed: %#v", paths)
	}
	path := paths["/api/v1/capabilities/get_customer"].(map[string]any)
	post := path["post"].(map[string]any)
	if post["x-onprest-capability"] != "get_customer" {
		t.Fatalf("post = %#v", post)
	}
	responses := post["responses"].(map[string]any)
	okResp := responses["200"].(map[string]any)
	content := okResp["content"].(map[string]any)
	app := content["application/json"].(map[string]any)
	schema := app["schema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	rows := props["rows"].(map[string]any)
	items := rows["items"].(map[string]any)
	rowProps := items["properties"].(map[string]any)
	id := rowProps["id"].(map[string]any)
	if id["type"] != "integer" || id["description"] != "Customer ID" {
		t.Fatalf("result column schema = %#v", id)
	}
}

func TestApplyResultContractFiltersToAllowlistedColumns(t *testing.T) {
	rows := []map[string]any{
		{"id": int64(1), "name": "Ada", "email": "ada@example.com", "internal_note": "secret"},
	}
	got, err := applyResultContract(rows, []string{"id", "name", "email", "internal_note"}, ResultDef{
		"id":   {Type: "integer"},
		"name": {Type: "string"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("filtered rows = %#v", got)
	}
	if _, ok := got[0]["email"]; ok {
		t.Fatalf("non-allowlisted column leaked: %#v", got)
	}
	if _, ok := got[0]["internal_note"]; ok {
		t.Fatalf("non-allowlisted column leaked: %#v", got)
	}
	if got[0]["id"] != int64(1) || got[0]["name"] != "Ada" {
		t.Fatalf("filtered row = %#v", got[0])
	}
}

func TestApplyResultContractCoercesDeclaredResultTypes(t *testing.T) {
	rows := []map[string]any{
		{"id": "7", "score": "12.5", "active": "true", "name": 42},
	}
	got, err := applyResultContract(rows, []string{"id", "score", "active", "name"}, ResultDef{
		"id":     {Type: "integer"},
		"score":  {Type: "number"},
		"active": {Type: "boolean"},
		"name":   {Type: "string"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0]["id"] != int64(7) ||
		got[0]["score"] != 12.5 ||
		got[0]["active"] != true ||
		got[0]["name"] != "42" {
		t.Fatalf("coerced row = %#v", got[0])
	}
}

func TestApplyResultContractWithoutResultExposesNoColumns(t *testing.T) {
	got, err := applyResultContract([]map[string]any{{"id": int64(1), "secret": "hidden"}}, []string{"id", "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("filtered rows = %#v, want one empty row", got)
	}
}

func TestApplyResultContractRejectsMissingDeclaredColumn(t *testing.T) {
	if _, err := applyResultContract([]map[string]any{{"id": int64(1)}}, []string{"id"}, ResultDef{"email": {Type: "string"}}); err == nil {
		t.Fatal("applyResultContract() error = nil, want missing column error")
	}
}

func TestRunnerWritesDetailedAgentErrorsToDetailLog(t *testing.T) {
	detail := &bytes.Buffer{}
	r := &Runner{
		caps:      validCapabilityFile().ByName(),
		detailLog: detail,
		cf:        validCapabilityFile(),
	}
	resp := r.handle(t.Context(), wireRequest{
		ID:         "req-1",
		Capability: "get_customer",
		Params:     map[string]any{},
	})
	if resp.Error == nil || resp.Error.Code != "AGENT_VALIDATION_FAILED" {
		t.Fatalf("response = %#v", resp)
	}
	if resp.Error.Message != "required param missing: id" {
		t.Fatalf("response message = %q", resp.Error.Message)
	}
	logs := detail.String()
	for _, want := range []string{"agent_error", "req-1", "get_customer", "AGENT_VALIDATION_FAILED", "required param missing"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("detail log missing %q: %s", want, logs)
		}
	}
}

func TestStartupDetailLogUsesStartupMessageAndKeepsDetailLocal(t *testing.T) {
	detail := &bytes.Buffer{}
	writeStartupDetail(detail, "AGENT_STARTUP_FAILED", "explain failed on private table")
	var entry map[string]any
	if err := json.Unmarshal(detail.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["error_code"] != "AGENT_STARTUP_FAILED" ||
		entry["message"] != "agent startup failed" ||
		entry["detail"] != "explain failed on private table" {
		t.Fatalf("startup detail entry = %#v", entry)
	}
}

func validCapabilityFile() *CapabilityFile {
	readonly := true
	return &CapabilityFile{
		Service: ServiceDef{Title: "Test Service", Version: "1.2.3", Description: "Test"},
		Gateway: GatewayDef{
			URL:             "ws://localhost:8080/ws/agent",
			AgentPrivateKey: testAgentPrivateKey,
		},
		Database: DatabaseDef{Driver: "postgres", Host: "localhost", Port: 5432, Name: "legacy", User: "readonly_user", Password: "secret"},
		Capabilities: map[string]CapabilityDef{
			"get_customer": {
				Description: "Get customer",
				SQL:         "select id from customers where id = :id",
				Params:      map[string]ParamDef{"id": {Type: "integer", Required: true}},
				Policy:      PolicyDef{Readonly: &readonly, Timeout: "1s", MaxRows: 1, MaxBytes: "128KB"},
			},
		},
	}
}

func cloneParams(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func boolPtr(v bool) *bool {
	return &v
}
