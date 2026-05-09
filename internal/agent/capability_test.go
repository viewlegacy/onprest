package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestDatabaseDSNCurrentConnectionPolicy(t *testing.T) {
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
			name: "sqlserver disables encrypt",
			db:   DatabaseDef{Driver: "sqlserver", Host: "db.example", Port: 1433, Name: "legacy", User: "readonly", Password: "secret"},
			want: "sqlserver://readonly:secret@db.example:1433?database=legacy&encrypt=disable",
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
	}
	if _, err := validateParams(cap, valid); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown", mutate: func(in map[string]any) { in["unknown"] = "x" }},
		{name: "required", mutate: func(in map[string]any) { delete(in, "required") }},
		{name: "string type", mutate: func(in map[string]any) { in["string"] = 1 }},
		{name: "integer type", mutate: func(in map[string]any) { in["integer"] = json.Number("1.5") }},
		{name: "number type", mutate: func(in map[string]any) { in["number"] = "1" }},
		{name: "boolean type", mutate: func(in map[string]any) { in["boolean"] = "true" }},
		{name: "min length", mutate: func(in map[string]any) { in["string"] = "ab" }},
		{name: "max length", mutate: func(in map[string]any) { in["string"] = "abcdef" }},
		{name: "pattern", mutate: func(in map[string]any) { in["string"] = "ABC" }},
		{name: "email", mutate: func(in map[string]any) { in["email"] = "bad" }},
		{name: "uuid", mutate: func(in map[string]any) { in["uuid"] = "bad" }},
		{name: "date", mutate: func(in map[string]any) { in["date"] = "2026-99-99" }},
		{name: "date-time", mutate: func(in map[string]any) { in["datetime"] = "bad" }},
		{name: "uri", mutate: func(in map[string]any) { in["uri"] = "example.com" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := cloneParams(valid)
			tc.mutate(input)
			if _, err := validateParams(cap, input); err == nil {
				t.Fatal("validateParams() error = nil, want error")
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
	logs := detail.String()
	for _, want := range []string{"agent_error", "req-1", "get_customer", "AGENT_VALIDATION_FAILED", "required param missing"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("detail log missing %q: %s", want, logs)
		}
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
