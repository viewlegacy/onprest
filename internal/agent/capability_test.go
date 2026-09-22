package agent

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	goora "github.com/sijms/go-ora/v2"
	"github.com/viewlegacy/onprest/internal/protocol"
)

const testAgentPrivateKey = "keEk2aSPeUHiCbhK-XxleMUFj3cwzcJCFUflKSs_CiZOsybztXdoRPcyYZTMd_f9cplE8Qd7VsMz484fWauOvw"

func writeCapabilityFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capability.yaml")
	content = strings.ReplaceAll(content, "agent_private_key: test", "agent_private_key: "+testAgentPrivateKey)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeParamsCapabilityFixture(t *testing.T, paramsYAML string) string {
	t.Helper()
	paramsYAML = "      " + strings.ReplaceAll(strings.TrimSpace(paramsYAML), "\n", "\n      ")
	return writeCapabilityFixture(t, `service:
  title: Param fixture
gateway:
  url: ws://127.0.0.1:8080/ws/agent
  agent_private_key: test
database:
  driver: postgres
  host: 127.0.0.1
  port: 5432
  name: test
  user: test
  password: test
capabilities:
  param_fixture:
    sql: select 1 as value
    params:
`+paramsYAML+`
    policy:
      timeout: 1s
      max_rows: 1
      max_bytes: 1KB
    result:
      value: {type: integer}
`)
}

func TestConfigurationDiagnosticsDoNotEchoUnsafeDynamicKeys(t *testing.T) {
	const sentinel = "RAW_KEY_SENTINEL"
	tests := []string{
		`"bad\n` + sentinel + `\x1b[31m": {type: unsupported}`,
		`value:
  type: string
  "bad\r` + sentinel + `": true`,
		`"bad ` + sentinel + `": {type: unsupported}`,
		`value:
  type: string
  "bad-\"` + sentinel + `": true`,
	}
	for _, params := range tests {
		_, err := LoadCapabilityFile(writeParamsCapabilityFixture(t, params))
		if err == nil {
			t.Fatalf("unsafe key fixture unexpectedly loaded: %s", params)
		}
		message := err.Error()
		if strings.Contains(message, sentinel) || strings.ContainsAny(message, "\r\n\x1b") || !strings.Contains(message, "<invalid-key>") {
			t.Fatalf("unsafe key reached public diagnostic: %q", message)
		}
	}

	path := writeCapabilityFixture(t, `
gateway:
  url: ws://127.0.0.1:8080/ws/agent
  agent_private_key: test
database: {driver: postgres, host: localhost, port: 5432, name: test, user: test}
capabilities:
  "bad `+sentinel+`": {sql: "select 1"}
`)
	_, err := LoadCapabilityFile(path)
	if err == nil || strings.Contains(err.Error(), sentinel) || strings.ContainsAny(err.Error(), "\r\n\x1b") || !strings.Contains(err.Error(), "<invalid-key>") {
		t.Fatalf("unsafe capability key diagnostic=%q", err)
	}
}

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

func TestLoadCapabilityFileRejectsExplicitInvalidPolicyValues(t *testing.T) {
	base := `database:
  driver: postgres
  host: localhost
  port: 5432
  name: legacy
  user: readonly_user
gateway:
  url: ws://localhost:8080/ws/agent
  agent_private_key: ` + testAgentPrivateKey + `
defaults:
  max_rows: 100
capabilities:
  get_customer:
    sql: select 1 as id
    policy:
%s
    result:
      id:
        type: integer
`
	for _, tc := range []struct {
		name, policy, want string
	}{
		{name: "explicit zero max rows", policy: "      max_rows: 0", want: "max_rows must be > 0"},
		{name: "negative max rows", policy: "      max_rows: -1", want: "max_rows must be > 0"},
		{name: "explicit zero max affected rows", policy: "      max_affected_rows: 0", want: "max_affected_rows must be > 0"},
		{name: "negative max affected rows", policy: "      max_affected_rows: -1", want: "max_affected_rows must be > 0"},
		{name: "fractional max affected rows", policy: "      max_affected_rows: 1.5", want: "max_affected_rows must be an integer"},
		{name: "overflow max affected rows", policy: "      max_affected_rows: 9223372036854775808", want: "max_affected_rows must be an int64"},
		{name: "zero timeout", policy: "      timeout: 0s", want: "timeout must be > 0"},
		{name: "negative timeout", policy: "      timeout: -1s", want: "timeout must be > 0"},
		{name: "rate limit null", policy: "      rate_limit: null", want: "rate_limit must be an object"},
		{name: "rate limit missing requests", policy: "      rate_limit: {per: 1s, burst: 1}", want: "requests must be > 0"},
		{name: "rate limit missing per", policy: "      rate_limit: {requests: 1, burst: 1}", want: "per is required"},
		{name: "rate limit missing burst", policy: "      rate_limit: {requests: 1, per: 1s}", want: "burst must be > 0"},
		{name: "rate limit zero requests", policy: "      rate_limit: {requests: 0, per: 1s, burst: 1}", want: "requests must be > 0"},
		{name: "rate limit negative requests", policy: "      rate_limit: {requests: -1, per: 1s, burst: 1}", want: "requests must be > 0"},
		{name: "rate limit zero per", policy: "      rate_limit: {requests: 1, per: 0s, burst: 1}", want: "per must be > 0"},
		{name: "rate limit negative per", policy: "      rate_limit: {requests: 1, per: -1s, burst: 1}", want: "per must be > 0"},
		{name: "rate limit invalid per", policy: "      rate_limit: {requests: 1, per: soon, burst: 1}", want: "per is invalid"},
		{name: "rate limit duration overflow", policy: "      rate_limit: {requests: 1, per: 2562048h, burst: 1}", want: "per is invalid"},
		{name: "rate limit zero burst", policy: "      rate_limit: {requests: 1, per: 1s, burst: 0}", want: "burst must be > 0"},
		{name: "rate limit negative burst", policy: "      rate_limit: {requests: 1, per: 1s, burst: -1}", want: "burst must be > 0"},
		{name: "rate limit derived overflow", policy: "      rate_limit: {requests: 1, per: 2ns, burst: 9223372036854775807}", want: "burst and per are too large"},
		{name: "rate limit sequence", policy: "      rate_limit: [1, 1s, 1]", want: "rate_limit must be an object"},
		{name: "rate limit unknown field", policy: "      rate_limit: {requests: 1, per: 1s, burst: 1, window: fixed}", want: "field window not found"},
		{name: "rate limit fractional requests", policy: "      rate_limit: {requests: 1.5, per: 1s, burst: 1}", want: "rate_limit.requests must be an integer"},
		{name: "rate limit string requests", policy: `      rate_limit: {requests: "1", per: 1s, burst: 1}`, want: "rate_limit.requests must be an integer"},
		{name: "rate limit requests overflow", policy: "      rate_limit: {requests: 9223372036854775808, per: 1s, burst: 1}", want: "rate_limit.requests must be an int64"},
		{name: "rate limit numeric per", policy: "      rate_limit: {requests: 1, per: 1, burst: 1}", want: "per is invalid"},
		{name: "rate limit fractional burst", policy: "      rate_limit: {requests: 1, per: 1s, burst: 1.5}", want: "rate_limit.burst must be an integer"},
		{name: "rate limit string burst", policy: `      rate_limit: {requests: 1, per: 1s, burst: "1"}`, want: "rate_limit.burst must be an integer"},
		{name: "rate limit burst overflow", policy: "      rate_limit: {requests: 1, per: 1ns, burst: 9223372036854775808}", want: "rate_limit.burst must be an int64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capability.yaml")
			content := strings.Replace(base, "%s", tc.policy, 1)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCapabilityFile(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadCapabilityFile() error=%v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCapabilityRateLimitDefaultsAndObjectOverride(t *testing.T) {
	path := writeCapabilityFixture(t, `
gateway:
  url: ws://127.0.0.1:8080/ws/agent
  agent_private_key: test
database: {driver: postgres, host: localhost, port: 5432, name: test, user: test}
defaults:
  rate_limit: {requests: 60, per: 1m, burst: 10}
capabilities:
  inherited:
    sql: select 1 as id
    result: {id: {type: integer}}
  overridden:
    sql: select 2 as id
    policy:
      rate_limit: {requests: 2, per: 1s, burst: 1}
    result: {id: {type: integer}}
`)
	cf, err := LoadCapabilityFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inherited := cf.Capabilities["inherited"].Policy.RateLimit
	if inherited == nil || inherited.Requests != 60 || inherited.Per != "1m" || inherited.Burst != 10 {
		t.Fatalf("inherited rate limit = %#v", inherited)
	}
	overridden := cf.Capabilities["overridden"].Policy.RateLimit
	if overridden == nil || overridden.Requests != 2 || overridden.Per != "1s" || overridden.Burst != 1 {
		t.Fatalf("overridden rate limit = %#v", overridden)
	}

	without := validCapabilityFile()
	if err := without.Lint(); err != nil {
		t.Fatal(err)
	}
	for name, capability := range without.Capabilities {
		if capability.Policy.RateLimit != nil {
			t.Fatalf("%s unexpectedly has rate limit %#v", name, capability.Policy.RateLimit)
		}
	}
}

func TestLoadCapabilityFileRejectsExplicitNullDefaultForEveryParamType(t *testing.T) {
	base := `database:
  driver: postgres
  host: localhost
  port: 5432
  name: legacy
  user: readonly_user
gateway:
  url: ws://localhost:8080/ws/agent
  agent_private_key: ` + testAgentPrivateKey + `
capabilities:
  get_customer:
    sql: select 1 as id
    params:
      value:
        type: %s
        default: null
    result:
      id:
        type: integer
`
	for _, paramType := range []string{"string", "integer", "number", "boolean"} {
		t.Run(paramType, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capability.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf(base, paramType)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCapabilityFile(path); err == nil || !strings.Contains(err.Error(), ".default is invalid") {
				t.Fatalf("LoadCapabilityFile() error=%v, want explicit null default lint error", err)
			}
		})
	}
}

func TestLoadCapabilityFilePreservesEmptyPatternAndFormatPresence(t *testing.T) {
	for _, constraint := range []string{"pattern", "format"} {
		for _, paramType := range []string{"integer", "number", "boolean"} {
			t.Run("reject empty "+constraint+" on "+paramType, func(t *testing.T) {
				path := writeCapabilityFixture(t, fmt.Sprintf(`service:
  title: Presence test
gateway:
  url: ws://127.0.0.1:8080/ws/agent
  agent_private_key: test
database:
  driver: postgres
  host: 127.0.0.1
  port: 5432
  name: test
  user: test
  password: test
capabilities:
  invalid_empty_constraint:
    sql: select :value
    params:
      value:
        type: %s
        %s: ""
    policy:
      timeout: 1s
      max_rows: 1
      max_bytes: 1KB
    result:
      value: {type: integer}
`, paramType, constraint))
				if _, err := LoadCapabilityFile(path); err == nil || !strings.Contains(err.Error(), "string constraints are supported only for string") {
					t.Fatalf("LoadCapabilityFile() error=%v, want %s %s startup lint error", err, paramType, constraint)
				}
			})
		}
	}

	valid := writeCapabilityFixture(t, `service:
  title: Presence test
gateway:
  url: ws://127.0.0.1:8080/ws/agent
  agent_private_key: test
database:
  driver: postgres
  host: 127.0.0.1
  port: 5432
  name: test
  user: test
  password: test
capabilities:
  valid_empty_string_constraints:
    sql: select :value
    params:
      value:
        type: string
        pattern: ""
        format: ""
    policy:
      timeout: 1s
      max_rows: 1
      max_bytes: 1KB
    result:
      value: {type: string}
`)
	if _, err := LoadCapabilityFile(valid); err != nil {
		t.Fatalf("valid string empty pattern/format rejected: %v", err)
	}
}

func TestLoadCapabilityFileSupportsParamYAMLMergeAndAliasPresence(t *testing.T) {
	t.Run("merge alias inherits default presence", func(t *testing.T) {
		path := writeParamsCapabilityFixture(t, `template: &template
  type: string
  default: inherited
value:
  <<: *template`)
		cf, err := LoadCapabilityFile(path)
		if err != nil {
			t.Fatal(err)
		}
		value := cf.Capabilities["param_fixture"].Params["value"]
		if value.Type != "string" || value.Default != "inherited" || !value.DefaultSet {
			t.Fatalf("merged param=%#v", value)
		}
		validated, err := validateParams(cf.Capabilities["param_fixture"], map[string]any{})
		if err != nil || validated["value"] != "inherited" {
			t.Fatalf("merged default validation=%#v error=%v", validated, err)
		}
	})

	t.Run("sequence merge inherits effective fields", func(t *testing.T) {
		path := writeParamsCapabilityFixture(t, `first: &first
  type: string
  default: inherited
second: &second
  type: string
  description: merged
value:
  <<: [*first, *second]`)
		cf, err := LoadCapabilityFile(path)
		if err != nil {
			t.Fatal(err)
		}
		value := cf.Capabilities["param_fixture"].Params["value"]
		if value.Type != "string" || value.Default != "inherited" || !value.DefaultSet || value.Description != "merged" {
			t.Fatalf("sequence-merged param=%#v", value)
		}
	})

	t.Run("parameter definition may be an alias", func(t *testing.T) {
		path := writeParamsCapabilityFixture(t, `template: &template
  type: string
  default: inherited
value: *template`)
		cf, err := LoadCapabilityFile(path)
		if err != nil {
			t.Fatal(err)
		}
		value := cf.Capabilities["param_fixture"].Params["value"]
		if value.Type != "string" || value.Default != "inherited" || !value.DefaultSet {
			t.Fatalf("aliased param=%#v", value)
		}
	})

	t.Run("direct values override merge alias", func(t *testing.T) {
		path := writeParamsCapabilityFixture(t, `template: &template
  type: integer
  default: 1
value:
  <<: *template
  type: string
  default: direct`)
		cf, err := LoadCapabilityFile(path)
		if err != nil {
			t.Fatal(err)
		}
		value := cf.Capabilities["param_fixture"].Params["value"]
		if value.Type != "string" || value.Default != "direct" || !value.DefaultSet {
			t.Fatalf("direct override param=%#v", value)
		}
	})

	t.Run("merge alias inherits explicit null default presence", func(t *testing.T) {
		path := writeParamsCapabilityFixture(t, `template: &template
  type: string
  default: null
value:
  <<: *template`)
		if _, err := LoadCapabilityFile(path); err == nil || !strings.Contains(err.Error(), ".default is invalid") {
			t.Fatalf("LoadCapabilityFile() error=%v, want inherited explicit null default rejection", err)
		}
	})

	for _, constraint := range []string{"pattern", "format"} {
		t.Run("merged empty "+constraint+" remains present", func(t *testing.T) {
			path := writeParamsCapabilityFixture(t, fmt.Sprintf(`template: &template
  type: string
  %s: ""
value:
  <<: *template
  type: integer`, constraint))
			if _, err := LoadCapabilityFile(path); err == nil || !strings.Contains(err.Error(), "string constraints are supported only for string") {
				t.Fatalf("LoadCapabilityFile() error=%v, want inherited empty %s lint error", err, constraint)
			}
		})
	}

	for _, tc := range []struct {
		name, params, want string
	}{
		{name: "unknown field in merge source", want: "field unknown_merged_field", params: `value:
  <<: &template
    type: string
    unknown_merged_field: true`},
		{name: "unknown direct field", want: "field unknown_direct_field", params: `value:
  type: string
  unknown_direct_field: true`},
		{name: "quoted merge spelling is an unknown direct field", want: "field <invalid-key>", params: `value:
  type: string
  "<<": {type: string}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadCapabilityFile(writeParamsCapabilityFixture(t, tc.params)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadCapabilityFile() error=%v, want unknown field rejection", err)
			}
		})
	}

	t.Run("cyclic merge is rejected", func(t *testing.T) {
		path := writeParamsCapabilityFixture(t, `value: &value
  type: string
  <<: *value`)
		if _, err := LoadCapabilityFile(path); err == nil || !strings.Contains(err.Error(), "cyclic YAML merge") {
			t.Fatalf("LoadCapabilityFile() error=%v, want cyclic merge rejection", err)
		}
	})
}

func TestLoadCapabilityFileRejectsInvalidParamContracts(t *testing.T) {
	tests := []struct {
		name, definition, want string
	}{
		{name: "default type", definition: "type: integer\ndefault: wrong", want: ".default is invalid"},
		{name: "default enum", definition: "type: string\nenum: [allowed]\ndefault: denied", want: ".default is invalid"},
		{name: "default range", definition: "type: integer\nminimum: 5\ndefault: 4", want: ".default is invalid"},
		{name: "default length", definition: "type: string\nminLength: 3\ndefault: ab", want: ".default is invalid"},
		{name: "default pattern", definition: "type: string\npattern: '^[A-Z]+$'\ndefault: lower", want: ".default is invalid"},
		{name: "default format", definition: "type: string\nformat: email\ndefault: invalid", want: ".default is invalid"},
		{name: "invalid enum member type", definition: "type: integer\nenum: [1, wrong]", want: ".enum[1] is invalid"},
		{name: "invalid enum member constraint", definition: "type: string\npattern: '^[A-Z]+$'\nenum: [VALID, invalid]", want: ".enum[1] is invalid"},
		{name: "unknown parameter field", definition: "type: string\nunknown_contract_field: true", want: "field unknown_contract_field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := "value:\n  " + strings.ReplaceAll(tc.definition, "\n", "\n  ")
			if _, err := LoadCapabilityFile(writeParamsCapabilityFixture(t, params)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadCapabilityFile() error=%v, want containing %q", err, tc.want)
			}
		})
	}

	valid := writeParamsCapabilityFixture(t, `value:
  type: string
  enum: [USER@example.com]
  minLength: 5
  maxLength: 64
  pattern: '^[A-Za-z]+@example[.]com$'
  format: email
  default: USER@example.com`)
	cf, err := LoadCapabilityFile(valid)
	if err != nil {
		t.Fatalf("valid constrained default rejected: %v", err)
	}
	validated, err := validateParams(cf.Capabilities["param_fixture"], map[string]any{})
	if err != nil || validated["value"] != "USER@example.com" {
		t.Fatalf("valid default validation=%#v error=%v", validated, err)
	}
}

func TestParamLintRejectsStaticConstraintDefaultAndEnumContradictions(t *testing.T) {
	min, max := int64(10), int64(5)
	minLength := 2
	tests := []struct {
		name string
		def  ParamDef
	}{
		{name: "minimum greater than maximum", def: ParamDef{Type: "integer", Minimum: &min, Maximum: &max}},
		{name: "minimum on string", def: ParamDef{Type: "string", Minimum: &min}},
		{name: "pattern on integer", def: ParamDef{Type: "integer", Pattern: "x"}},
		{name: "length on boolean", def: ParamDef{Type: "boolean", MinLength: &minLength}},
		{name: "default wrong type", def: ParamDef{Type: "integer", Default: "1"}},
		{name: "default outside range", def: ParamDef{Type: "integer", Minimum: &min, Default: 1}},
		{name: "default outside enum", def: ParamDef{Type: "string", Enum: []any{"a"}, Default: "b"}},
		{name: "enum wrong type", def: ParamDef{Type: "boolean", Enum: []any{"true"}}},
		{name: "enum violates format", def: ParamDef{Type: "string", Format: "email", Enum: []any{"invalid"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.def.lint("param"); err == nil {
				t.Fatal("lint() error=nil")
			}
		})
	}
	validMin, validMax := int64(1), int64(10)
	valid := ParamDef{Type: "integer", Minimum: &validMin, Maximum: &validMax, Enum: []any{1, 2}, Default: 2}
	if err := valid.lint("param"); err != nil {
		t.Fatalf("valid param rejected: %v", err)
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
	for name, capability := range cf.Capabilities {
		rate := capability.Policy.RateLimit
		if rate == nil || rate.Requests != 60 || rate.Per != "1m" || rate.Burst != 10 {
			t.Fatalf("example %s rate limit = %#v, want 60/1m burst 10", name, rate)
		}
	}
}

func TestReleaseCapabilityExampleLoads(t *testing.T) {
	templatePath := filepath.Join("..", "..", "release", "capability.yaml.example")
	content, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatal(err)
	}
	configured := strings.ReplaceAll(string(content), "replace-with-create-agent-secret-private-key", testAgentPrivateKey)
	path := filepath.Join(t.TempDir(), "capability.yaml")
	if err := os.WriteFile(path, []byte(configured), 0o600); err != nil {
		t.Fatal(err)
	}
	cf, err := LoadCapabilityFile(path)
	if err != nil {
		t.Fatalf("LoadCapabilityFile(release example): %v", err)
	}
	if cf.Service.Title != "Example Onprest Service" || cf.Database.Driver != "postgres" {
		t.Fatalf("release example loaded unexpected contract: service=%q driver=%q", cf.Service.Title, cf.Database.Driver)
	}
	if _, ok := cf.Capabilities["get_customer"]; !ok {
		t.Fatal("release example missing get_customer capability")
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
		name   string
		driver string
		query  string
		want   bool
	}{
		{name: "select", driver: "postgres", query: "select id from customers where id = :id", want: true},
		{name: "leading comments select", driver: "postgres", query: "-- comment\n/* block */\nSELECT id FROM customers", want: true},
		{name: "with select", driver: "postgres", query: "with recent as (select id from customers) select id from recent", want: false},
		{name: "with insert cte", driver: "postgres", query: "with inserted as (insert into customers(name) values (:name) returning id) select id from inserted", want: false},
		{name: "update", driver: "postgres", query: "update customers set name = :name", want: false},
		{name: "multiple statements", driver: "postgres", query: "select 1; update customers set name = 'x'", want: false},
		{name: "semicolon in string", driver: "postgres", query: "select ';' as value", want: true},
		{name: "semicolon in escaped string", driver: "postgres", query: `select 'it''s;fine' as value`, want: true},
		{name: "semicolon in line comment", driver: "postgres", query: "select 1 -- ; ignored\n", want: true},
		{name: "semicolon in block comment", driver: "postgres", query: "select /* ; ignored */ 1", want: true},
		{name: "semicolon in dollar quote", driver: "postgres", query: "select $$; ignored$$", want: true},
		{name: "trailing semicolon", driver: "postgres", query: "select 1; -- trailing comment", want: true},
		{name: "second empty statement", driver: "postgres", query: "select 1;;", want: false},
		{name: "postgres standard string backslash does not escape quote", driver: "postgres", query: `SELECT '\'; UPDATE protected_table SET value = 99`, want: false},
		{name: "postgres explicit escape string", driver: "postgres", query: `SELECT E'escaped\'; still string'`, want: true},
		{name: "sqlserver backslash does not escape quote", driver: "sqlserver", query: `SELECT '\'; UPDATE protected_table SET value = 99`, want: false},
		{name: "oracle backslash does not escape quote", driver: "oracle", query: `SELECT '\' FROM dual; UPDATE protected_table SET value = 99`, want: false},
		{name: "mysql conservative across sql modes", driver: "mysql", query: `SELECT '\'; UPDATE protected_table SET value = 99`, want: false},
		{name: "mysql backtick identifier", driver: "mysql", query: "select `semi;colon` from customers", want: true},
		{name: "sqlserver bracket identifier", driver: "sqlserver", query: "select [semi;colon] from customers", want: true},
		{name: "oracle alternative quote", driver: "oracle", query: "select q'[semi;colon]' from dual", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadOnlySQL(tc.driver, tc.query); got != tc.want {
				t.Fatalf("isReadOnlySQL(%q, %q) = %t, want %t", tc.driver, tc.query, got, tc.want)
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
			cap.Policy.MaxRows = intPtr(-1)
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

func TestGatewayURLRequiresWSSOutsideLoopback(t *testing.T) {
	tests := []struct {
		name          string
		url           string
		allowInsecure bool
		wantErr       bool
	}{
		{name: "production wss", url: "wss://gateway.example.com/ws/agent"},
		{name: "loopback hostname", url: "ws://localhost:8080/ws/agent"},
		{name: "loopback ipv4", url: "ws://127.0.0.1:8080/ws/agent"},
		{name: "loopback ipv6", url: "ws://[::1]:8080/ws/agent"},
		{name: "non-loopback plaintext", url: "ws://10.0.0.8:8080/ws/agent", wantErr: true},
		{name: "development override", url: "ws://gateway:8080/ws/agent", allowInsecure: true},
		{name: "wrong path", url: "wss://gateway.example.com/agent", wantErr: true},
		{name: "query forbidden", url: "wss://gateway.example.com/ws/agent?token=x", wantErr: true},
		{name: "credentials forbidden", url: "wss://user@gateway.example.com/ws/agent", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := lintGatewayURL(tc.url, tc.allowInsecure)
			if (err != nil) != tc.wantErr {
				t.Fatalf("lintGatewayURL(%q, %t) error = %v, wantErr %t", tc.url, tc.allowInsecure, err, tc.wantErr)
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
				Mode: "verify-full", CAFile: "/certs/ca.pem", CertFile: "/certs/client.pem", KeyFile: "/certs/client.key", ServerName: "postgres.internal",
			}},
			want: "postgres://readonly:secret@db.example:5432/legacy?sslcert=%2Fcerts%2Fclient.pem&sslkey=%2Fcerts%2Fclient.key&sslmode=verify-full&sslrootcert=%2Fcerts%2Fca.pem",
		},
		{
			name: "mysql require TLS",
			db:   DatabaseDef{Driver: "mysql", Host: "db.example", Port: 3306, Name: "legacy", User: "readonly", Password: "secret", TLS: DatabaseTLSDef{Mode: "require"}},
			want: "readonly:secret@tcp(db.example:3306)/legacy?tls=skip-verify",
		},
		{
			name: "mysql verified TLS",
			db:   DatabaseDef{Driver: "mysql", Host: "db.example", Port: 3306, Name: "legacy", User: "readonly", Password: "secret", TLS: DatabaseTLSDef{Mode: "verify-full", ServerName: "mysql.internal"}},
			want: "readonly:secret@tcp(db.example:3306)/legacy?tls=true",
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
		{
			name: "oracle require TLS",
			db:   DatabaseDef{Driver: "oracle", Host: "db.example", Port: 1521, Name: "legacy", User: "readonly", Password: "secret", TLS: DatabaseTLSDef{Mode: "require"}},
			want: "oracle://readonly:secret@db.example:1521/legacy?SSL=enable&SSL+VERIFY=false",
		},
		{
			name: "oracle verified TLS",
			db:   DatabaseDef{Driver: "oracle", Host: "db.example", Port: 1521, Name: "legacy", User: "readonly", Password: "secret", TLS: DatabaseTLSDef{Mode: "verify-full", ServerName: "oracle.internal"}},
			want: "oracle://readonly:secret@db.example:1521/legacy?SSL=enable&SSL+VERIFY=true",
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
	db := DatabaseDef{Driver: "mysql", Host: "db.example", Port: 3306, Name: `legacy name;"日本`, User: `reader name;"日本`, Password: "p@ss:/word?&=# ;\"日本"}
	dsn := db.DSN()
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", dsn, err)
	}
	if parsed.User != db.User || parsed.Passwd != db.Password || parsed.Addr != "db.example:3306" || parsed.DBName != db.Name {
		t.Fatalf("round-trip = user:%q password:%q addr:%q db:%q", parsed.User, parsed.Passwd, parsed.Addr, parsed.DBName)
	}
}

func TestOracleDSNRoundTripsSpecialCredentialsAndTLS(t *testing.T) {
	db := DatabaseDef{
		Driver: "oracle", Host: "db.example", Port: 1521, Name: `legacy name;"日本`,
		User: `reader name;"日本`, Password: "p@ss:/word?&=# ;\"日本",
		TLS: DatabaseTLSDef{Mode: "verify-full", ServerName: "oracle.internal"},
	}
	config, err := goora.ParseConfig(db.DSN())
	if err != nil {
		t.Fatalf("ParseConfig(%q): %v", db.DSN(), err)
	}
	if config.UserID != db.User || config.Password != db.Password || config.ServiceName != db.Name || len(config.Servers) != 1 || config.Servers[0].Addr != db.Host || config.Servers[0].Port != db.Port || !config.SSL || config.SSLVerify != true {
		t.Fatalf("round-trip config = %#v", config)
	}
}

func TestPostgresPublicDriverAndDSNRoundTripToPGX(t *testing.T) {
	db := DatabaseDef{Driver: "postgres", Host: "db.example", Port: 5432, Name: "legacy/name", User: "reader@domain/name", Password: `p@ss:/word?&=#`, TLS: DatabaseTLSDef{Mode: "verify-full", ServerName: "postgres.internal"}}
	config, err := postgresConnectionConfig(db)
	if err != nil {
		t.Fatalf("pgx ParseConfig(%q): %v", db.DSN(), err)
	}
	if driverName(db.Driver) != "pgx" || config.Config.Host != db.Host || config.Config.Port != uint16(db.Port) || config.Config.Database != db.Name || config.Config.User != db.User || config.Config.Password != db.Password {
		t.Fatalf("driver=%q config=%#v", driverName(db.Driver), config.Config)
	}
	if config.Config.TLSConfig == nil || config.Config.TLSConfig.ServerName != db.TLS.ServerName {
		t.Fatalf("TLS config=%#v", config.Config.TLSConfig)
	}
	t.Run("TLS paths with spaces and literal plus", func(t *testing.T) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IsCA: true, BasicConstraintsValid: true,
			KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		db := db
		db.TLS.CAFile = filepath.Join(dir, "root CA+test.pem")
		db.TLS.CertFile = filepath.Join(dir, "client cert+test.pem")
		db.TLS.KeyFile = filepath.Join(dir, "client key+test.pem")
		for path, block := range map[string]*pem.Block{
			db.TLS.CAFile:   {Type: "CERTIFICATE", Bytes: der},
			db.TLS.CertFile: {Type: "CERTIFICATE", Bytes: der},
			db.TLS.KeyFile:  {Type: "PRIVATE KEY", Bytes: keyDER},
		} {
			if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		config, err := postgresConnectionConfig(db)
		if err != nil {
			t.Fatalf("ParseConfig with TLS paths containing spaces and plus: %v", err)
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		tlsConfig := config.Config.TLSConfig
		if tlsConfig == nil || tlsConfig.RootCAs == nil || !tlsConfig.RootCAs.Equal(roots) || len(tlsConfig.Certificates) != 1 || !bytes.Equal(tlsConfig.Certificates[0].Certificate[0], der) {
			t.Fatal("TLS config did not load the configured CA and client certificate")
		}
	})
}

func TestDatabaseTLSLintRejectsInvalidOrUnsupportedConfiguration(t *testing.T) {
	tests := []struct {
		name string
		db   DatabaseDef
	}{
		{name: "unknown mode", db: DatabaseDef{Driver: "postgres", Host: "db", Port: 5432, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "optional"}}},
		{name: "mysql verify ca", db: DatabaseDef{Driver: "mysql", Host: "db", Port: 3306, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "verify-ca"}}},
		{name: "incomplete client pair", db: DatabaseDef{Driver: "postgres", Host: "db", Port: 5432, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "verify-full", CertFile: "client.pem"}}},
		{name: "sqlserver client cert", db: DatabaseDef{Driver: "sqlserver", Host: "db", Port: 1433, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "verify-full", CertFile: "client.pem", KeyFile: "client.key"}}},
		{name: "oracle client cert", db: DatabaseDef{Driver: "oracle", Host: "db", Port: 1521, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "verify-full", CertFile: "client.pem", KeyFile: "client.key"}}},
		{name: "server name without verification", db: DatabaseDef{Driver: "mysql", Host: "db", Port: 3306, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "require", ServerName: "db.internal"}}},
		{name: "ca without verification", db: DatabaseDef{Driver: "postgres", Host: "db", Port: 5432, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "require", CAFile: "ca.pem"}}},
		{name: "fields with disabled TLS", db: DatabaseDef{Driver: "oracle", Host: "db", Port: 1521, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "disable", CAFile: "ca.pem"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.db.lint(); err == nil {
				t.Fatalf("lint(%#v) error = nil", tc.db.TLS)
			}
		})
	}
}

func TestDatabaseTLSLintAcceptanceMatrix(t *testing.T) {
	ports := map[string]int{"postgres": 5432, "mysql": 3306, "sqlserver": 1433, "oracle": 1521}
	for driver, port := range ports {
		for _, mode := range []string{"disable", "require", "verify-full"} {
			t.Run(driver+"/"+mode, func(t *testing.T) {
				db := DatabaseDef{Driver: driver, Host: "db", Port: port, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: mode}}
				if mode == "verify-full" {
					db.TLS.ServerName = driver + ".internal"
				}
				if err := db.lint(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	for _, driver := range []string{"postgres", "mysql"} {
		t.Run(driver+"/client-certificate", func(t *testing.T) {
			db := DatabaseDef{Driver: driver, Host: "db", Port: ports[driver], Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "require", CertFile: "client.pem", KeyFile: "client.key"}}
			if err := db.lint(); err != nil {
				t.Fatal(err)
			}
		})
	}
	postgresVerifyCA := DatabaseDef{Driver: "postgres", Host: "db", Port: 5432, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "verify-ca", CAFile: "ca.pem"}}
	if err := postgresVerifyCA.lint(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityFileMergesPolicyDefaults(t *testing.T) {
	cf := validCapabilityFile()
	defaultReadonly := false
	capReadonly := true
	defaultMaxAffectedRows := int64(20)
	cf.Defaults = PolicyDef{Readonly: &defaultReadonly, Timeout: "30s", MaxRows: intPtr(1000), MaxBytes: "2MB", MaxAffectedRows: &defaultMaxAffectedRows}
	cap := cf.Capabilities["get_customer"]
	cap.SQL = "update customers set id = id"
	capReadonly = false
	capMaxAffectedRows := int64(5)
	cap.Policy = PolicyDef{Readonly: &capReadonly, MaxRows: intPtr(5), MaxAffectedRows: &capMaxAffectedRows}
	cf.Capabilities["get_customer"] = cap

	if err := cf.Lint(); err != nil {
		t.Fatal(err)
	}
	got := cf.Capabilities["get_customer"].Policy
	if readonly(got) || got.Timeout != "30s" || resolvedMaxRows(got) != 5 || got.MaxBytes != "2MB" || got.MaxAffectedRows == nil || *got.MaxAffectedRows != 5 {
		t.Fatalf("merged policy = %#v", got)
	}
}

func TestCapabilityFileRejectsMaxAffectedRowsForSelectIncludingDefaults(t *testing.T) {
	for _, source := range []struct {
		name     string
		defaults bool
	}{
		{name: "capability policy"},
		{name: "defaults", defaults: true},
	} {
		t.Run(source.name, func(t *testing.T) {
			cf := validCapabilityFile()
			limit := int64(1)
			if source.defaults {
				cf.Defaults.MaxAffectedRows = &limit
			} else {
				cap := cf.Capabilities["get_customer"]
				cap.Policy.MaxAffectedRows = &limit
				cf.Capabilities["get_customer"] = cap
			}
			if err := cf.Lint(); err == nil || !strings.Contains(err.Error(), "max_affected_rows is only supported for DML") {
				t.Fatalf("Lint() error=%v, want SELECT max_affected_rows rejection", err)
			}
		})
	}
}

func TestLoadCapabilityFileMaxAffectedRowsYAMLMergeAndLint(t *testing.T) {
	const merged = `service:
  title: Mutation limit fixture
gateway:
  url: ws://127.0.0.1:8080/ws/agent
  agent_private_key: test
database:
  driver: postgres
  host: 127.0.0.1
  port: 5432
  name: fixture
  user: fixture
  password: fixture
defaults:
  readonly: false
  max_affected_rows: 9
capabilities:
  source:
    sql: update customers set name = name
    policy: &mutation_policy
      readonly: false
      timeout: 1s
      max_rows: 100
      max_bytes: 128KB
      max_affected_rows: 9223372036854775807
  direct:
    sql: update customers set name = name
    policy:
      <<: *mutation_policy
      max_affected_rows: 7
  inherited:
    sql: delete from customers where id = 1
`
	cf, err := LoadCapabilityFile(writeCapabilityFixture(t, merged))
	if err != nil {
		t.Fatal(err)
	}
	source := cf.Capabilities["source"].Policy
	direct := cf.Capabilities["direct"].Policy
	inherited := cf.Capabilities["inherited"].Policy
	if source.MaxAffectedRows == nil || *source.MaxAffectedRows != math.MaxInt64 {
		t.Fatalf("source max_affected_rows=%#v, want MaxInt64", source.MaxAffectedRows)
	}
	if direct.MaxAffectedRows == nil || *direct.MaxAffectedRows != 7 {
		t.Fatalf("direct max_affected_rows=%#v, want direct YAML value 7", direct.MaxAffectedRows)
	}
	if direct.Readonly == nil || *direct.Readonly || direct.Timeout != "1s" || resolvedMaxRows(direct) != 100 || direct.MaxBytes != "128KB" {
		t.Fatalf("YAML merge fields were not preserved: %#v", direct)
	}
	if inherited.MaxAffectedRows == nil || *inherited.MaxAffectedRows != 9 {
		t.Fatalf("inherited max_affected_rows=%#v, want defaults value 9", inherited.MaxAffectedRows)
	}

	const selectWithEffectiveLimit = `service:
  title: Select limit fixture
gateway:
  url: ws://127.0.0.1:8080/ws/agent
  agent_private_key: test
database:
  driver: postgres
  host: 127.0.0.1
  port: 5432
  name: fixture
  user: fixture
  password: fixture
defaults:
  max_affected_rows: 1
capabilities:
  read:
    sql: select 1 as value
`
	if _, err := LoadCapabilityFile(writeCapabilityFixture(t, selectWithEffectiveLimit)); err == nil || !strings.Contains(err.Error(), "max_affected_rows is only supported for DML") {
		t.Fatalf("LoadCapabilityFile() error=%v, want effective SELECT rejection", err)
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
	types, ok := id["type"].([]any)
	if !ok || len(types) != 2 || types[0] != "integer" || types[1] != "null" || id["format"] != "int64" || id["description"] != "Customer ID" {
		t.Fatalf("result column schema = %#v", id)
	}
	if required := items["required"].([]string); len(required) != 1 || required[0] != "id" {
		t.Fatalf("result required=%v", required)
	}
}

func TestCapabilityAnnotationsAndExamplesReachOpenAPIWithoutHiddenMetadata(t *testing.T) {
	content := strings.Join([]string{
		"service:",
		"  title: AI contract fixture",
		"gateway:",
		"  url: ws://127.0.0.1:8080/ws/agent",
		"  agent_private_key: test",
		"database:",
		"  driver: postgres",
		"  host: 127.0.0.1",
		"  port: 5432",
		"  name: fixture",
		"  user: fixture",
		"  password: fixture",
		"capabilities:",
		"  visible:",
		"    description: Public customer lookup.",
		"    sql: select :customer_id as customer_id",
		"    annotations:",
		"      read_only: true",
		"      destructive: false",
		"      idempotent: true",
		"      open_world: false",
		"    examples:",
		"      - params:",
		"          customer_id: 123",
		"    params:",
		"      customer_id:",
		"        type: integer",
		"        required: true",
		"        minimum: 1",
		"        description: Customer ID.",
		"    result:",
		"      customer_id:",
		"        type: integer",
		"        description: Customer ID.",
		"  hidden:",
		"    description: PRIVATE_DESCRIPTION_SENTINEL",
		"    sql: select :customer_id as customer_id",
		"    annotations:",
		"      read_only: false",
		"    examples:",
		"      - params:",
		"          customer_id: 456",
		"    params:",
		"      customer_id:",
		"        type: integer",
		"        required: true",
		"    policy:",
		"      expose_in_openapi: false",
	}, "\n")
	cf, err := LoadCapabilityFile(writeCapabilityFixture(t, content))
	if err != nil {
		t.Fatal(err)
	}
	visible := cf.Capabilities["visible"]
	if visible.Annotations == nil || visible.Annotations.ReadOnly == nil || !*visible.Annotations.ReadOnly || visible.Annotations.Destructive == nil || *visible.Annotations.Destructive || visible.Annotations.Idempotent == nil || !*visible.Annotations.Idempotent || visible.Annotations.OpenWorld == nil || *visible.Annotations.OpenWorld {
		t.Fatalf("annotations=%#v", visible.Annotations)
	}
	if len(visible.Examples) != 1 || visible.Examples[0].Params["customer_id"] != 123 {
		t.Fatalf("examples=%#v", visible.Examples)
	}

	doc := BuildOpenAPI(cf)
	paths := doc["paths"].(map[string]any)
	if _, ok := paths["/api/v1/capabilities/hidden"]; ok {
		t.Fatalf("hidden capability exposed: %#v", paths)
	}
	post := paths["/api/v1/capabilities/visible"].(map[string]any)["post"].(map[string]any)
	annotations, ok := post["x-onprest-annotations"].(map[string]any)
	if !ok || annotations["read_only"] != true || annotations["destructive"] != false || annotations["idempotent"] != true || annotations["open_world"] != false {
		t.Fatalf("OpenAPI annotations=%#v", post["x-onprest-annotations"])
	}
	examples, ok := post["x-onprest-examples"].([]any)
	if !ok || len(examples) != 1 || examples[0].(map[string]any)["params"].(map[string]any)["customer_id"] != 123 {
		t.Fatalf("OpenAPI examples=%#v", post["x-onprest-examples"])
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE_DESCRIPTION_SENTINEL") || strings.Contains(string(raw), "456") {
		t.Fatalf("hidden metadata leaked into OpenAPI: %s", raw)
	}
}

func TestCapabilityAnnotationsPreserveOmittedFieldsAndExplicitFalse(t *testing.T) {
	cf := validCapabilityFile()
	cap := cf.Capabilities["get_customer"]
	cap.Annotations = &CapabilityAnnotations{Destructive: boolPtr(false)}
	cf.Capabilities["get_customer"] = cap
	if err := cf.Lint(); err != nil {
		t.Fatal(err)
	}
	post := BuildOpenAPI(cf)["paths"].(map[string]any)["/api/v1/capabilities/get_customer"].(map[string]any)["post"].(map[string]any)
	annotations, ok := post["x-onprest-annotations"].(map[string]any)
	if !ok || len(annotations) != 1 || annotations["destructive"] != false {
		t.Fatalf("annotations=%#v", post["x-onprest-annotations"])
	}
	if _, ok := annotations["read_only"]; ok {
		t.Fatalf("omitted read_only was synthesized: %#v", annotations)
	}
	cap.Annotations = nil
	cf.Capabilities["get_customer"] = cap
	if err := cf.Lint(); err != nil {
		t.Fatal(err)
	}
	post = BuildOpenAPI(cf)["paths"].(map[string]any)["/api/v1/capabilities/get_customer"].(map[string]any)["post"].(map[string]any)
	if _, ok := post["x-onprest-annotations"]; ok {
		t.Fatalf("nil annotations unexpectedly emitted: %#v", post["x-onprest-annotations"])
	}
}

func TestLoadCapabilityFileRejectsInvalidCapabilityAnnotationsAndExamples(t *testing.T) {
	base := strings.Join([]string{
		"service:",
		"  title: AI contract fixture",
		"gateway:",
		"  url: ws://127.0.0.1:8080/ws/agent",
		"  agent_private_key: test",
		"database:",
		"  driver: postgres",
		"  host: 127.0.0.1",
		"  port: 5432",
		"  name: fixture",
		"  user: fixture",
		"  password: fixture",
		"capabilities:",
		"  visible:",
		"    sql: select :customer_id as customer_id",
		"%s",
		"    params:",
		"      customer_id:",
		"        type: integer",
		"        required: true",
		"        minimum: 1",
		"    result:",
		"      customer_id: {type: integer}",
	}, "\n")
	tests := []struct {
		name    string
		insert  string
		wantErr string
	}{
		{name: "unknown capability field", insert: "    unsupported_metadata: true", wantErr: "field unsupported_metadata not found in capability definition"},
		{name: "unknown annotation key", insert: "    annotations:\n      unknown: true", wantErr: "field unknown not found"},
		{name: "wrong annotation type", insert: "    annotations:\n      read_only: 1", wantErr: "cannot unmarshal"},
		{name: "null annotation type", insert: "    annotations:\n      read_only: null", wantErr: "cannot unmarshal"},
		{name: "missing example params", insert: "    examples:\n      - {}", wantErr: "examples[0].params must be an object"},
		{name: "unknown example field", insert: "    examples:\n      - extra: true\n        params:\n          customer_id: 1", wantErr: "field extra not found in capability example definition"},
		{name: "null example params", insert: "    examples:\n      - params: null", wantErr: "cannot unmarshal capability example.params"},
		{name: "tagged null example params", insert: "    examples:\n      - params: !!null {}", wantErr: "cannot unmarshal capability example.params"},
		{name: "tagged null example params alias", insert: "    description: &null !!null {}\n    examples:\n      - params: *null", wantErr: "cannot unmarshal capability example.params"},
		{name: "null example item", insert: "    examples:\n      - null", wantErr: "cannot unmarshal capability.examples[0]"},
		{name: "tilde example item", insert: "    examples:\n      - ~", wantErr: "cannot unmarshal capability.examples[0]"},
		{name: "tagged null scalar example item", insert: "    examples:\n      - !!null null", wantErr: "cannot unmarshal capability.examples[0]"},
		{name: "tagged null mapping example item", insert: "    examples:\n      - !!null {}", wantErr: "cannot unmarshal capability.examples[0]"},
		{name: "tagged null sequence example item", insert: "    examples:\n      - !!null []", wantErr: "cannot unmarshal capability.examples[0]"},
		{name: "null alias example item", insert: "    policy: &null null\n    examples:\n      - *null", wantErr: "cannot unmarshal capability.examples[0]"},
		{name: "tagged null collection alias example item", insert: "    policy: &null !!null {}\n    examples:\n      - *null", wantErr: "cannot unmarshal capability.examples[0]"},
		{name: "tagged null sequence alias example item", insert: "    policy: &null !!null []\n    examples:\n      - *null", wantErr: "cannot unmarshal capability.examples[0]"},
		{name: "unknown example param", insert: "    examples:\n      - params:\n          unknown: 1\n          customer_id: 1", wantErr: "examples[0].params: unknown param: unknown"},
		{name: "missing required example param", insert: "    examples:\n      - params: {}", wantErr: "examples[0].params: required param missing: customer_id"},
		{name: "wrong example param type", insert: "    examples:\n      - params:\n          customer_id: wrong", wantErr: "examples[0].params: customer_id: must be integer"},
		{name: "example below minimum", insert: "    examples:\n      - params:\n          customer_id: 0", wantErr: "examples[0].params: customer_id: below minimum"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := fmt.Sprintf(base, tc.insert)
			if _, err := LoadCapabilityFile(writeCapabilityFixture(t, content)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadCapabilityFile() error=%v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadCapabilityFileRejectsExplicitNullCapabilityMetadata(t *testing.T) {
	base := `service:
  title: null metadata fixture
gateway:
  url: ws://127.0.0.1:8080/ws/agent
  agent_private_key: test
database:
  driver: postgres
  host: 127.0.0.1
  port: 5432
  name: fixture
  user: fixture
  password: fixture
capabilities:
  get_value:
    sql: select 1 as value
    params: {}
    result:
      value: {type: integer}
`
	tests := []struct {
		name   string
		insert string
	}{
		{name: "annotations null", insert: "    annotations: null\n"},
		{name: "examples null", insert: "    examples: null\n"},
		{name: "annotations tagged null sequence", insert: "    annotations: !!null []\n"},
		{name: "annotations tagged null mapping", insert: "    annotations: !!null {}\n"},
		{name: "examples tagged null sequence", insert: "    examples: !!null []\n"},
		{name: "examples tagged null mapping", insert: "    examples: !!null {}\n"},
		{name: "annotations null alias", insert: "    description: &null null\n    annotations: *null\n"},
		{name: "examples null alias", insert: "    description: &null null\n    examples: *null\n"},
		{name: "annotations tagged null sequence alias", insert: "    description: &null !!null []\n    annotations: *null\n"},
		{name: "examples tagged null mapping alias", insert: "    description: &null !!null {}\n    examples: *null\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Replace(base, "    params: {}\n", tc.insert+"    params: {}\n", 1)
			if _, err := LoadCapabilityFile(writeCapabilityFixture(t, content)); err == nil || !strings.Contains(err.Error(), "cannot unmarshal capability") {
				t.Fatalf("LoadCapabilityFile() error=%v, want explicit null metadata rejection", err)
			}
		})
	}
	if _, err := LoadCapabilityFile(writeCapabilityFixture(t, base)); err != nil {
		t.Fatalf("omitted annotations/examples should remain valid: %v", err)
	}
}

func TestLoadCapabilityFileAcceptsAliasedExampleObjects(t *testing.T) {
	content := `service:
  title: aliased examples fixture
gateway:
  url: ws://127.0.0.1:8080/ws/agent
  agent_private_key: test
database:
  driver: postgres
  host: 127.0.0.1
  port: 5432
  name: fixture
  user: fixture
  password: fixture
capabilities:
  get_value:
    sql: select :customer_id as customer_id
    examples:
      - &example
        params: &params
          customer_id: 1
      - *example
    params:
      customer_id:
        type: integer
        required: true
        minimum: 1
    result:
      customer_id: {type: integer}
`
	cf, err := LoadCapabilityFile(writeCapabilityFixture(t, content))
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Capabilities["get_value"].Examples) != 2 {
		t.Fatalf("aliased examples=%#v, want two decoded examples", cf.Capabilities["get_value"].Examples)
	}
	for i, example := range cf.Capabilities["get_value"].Examples {
		if example.Params["customer_id"] != 1 {
			t.Fatalf("example[%d]=%#v, want aliased customer_id=1", i, example.Params)
		}
	}
}

func TestBuildOpenAPISelectResultTypesAllowSQLNull(t *testing.T) {
	cap := CapabilityDef{Result: ResultDef{
		"text":    {Type: "string"},
		"integer": {Type: "integer"},
		"number":  {Type: "number"},
		"boolean": {Type: "boolean"},
	}}
	properties := responseSchema(cap)["properties"].(map[string]any)["rows"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	for name, declared := range map[string]string{"text": "string", "integer": "integer", "number": "number", "boolean": "boolean"} {
		types, ok := properties[name].(map[string]any)["type"].([]any)
		if !ok || len(types) != 2 || types[0] != declared || types[1] != "null" {
			t.Fatalf("%s schema=%#v", name, properties[name])
		}
	}
	if properties["integer"].(map[string]any)["format"] != "int64" {
		t.Fatalf("integer schema=%#v", properties["integer"])
	}
}

func TestBuildOpenAPIMutationSchemaAndInternalKinds(t *testing.T) {
	cf := validCapabilityFile()
	cap := cf.Capabilities["get_customer"]
	cap.SQL = "update customers set name='x' where id=:id"
	cap.Policy.Readonly = boolPtr(false)
	cap.Result = nil
	cf.Capabilities["update_customer"] = cap
	hidden := cap
	hidden.Policy.ExposeInOpenAPI = boolPtr(false)
	cf.Capabilities["hidden_update"] = hidden
	if err := cf.Lint(); err != nil {
		t.Fatal(err)
	}
	paths := BuildOpenAPI(cf)["paths"].(map[string]any)
	post := paths["/api/v1/capabilities/update_customer"].(map[string]any)["post"].(map[string]any)
	schema := post["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	if _, ok := props["rows"]; ok {
		t.Fatalf("mutation schema contains rows: %#v", schema)
	}
	count := props["count"].(map[string]any)
	if count["format"] != "int64" || count["minimum"] != int64(0) || schema["additionalProperties"] != false {
		t.Fatalf("mutation schema=%#v", schema)
	}
	kinds := responseKinds(cf)
	if kinds["update_customer"] != "mutation" || kinds["hidden_update"] != "mutation" || kinds["get_customer"] != "select" {
		t.Fatalf("response kinds=%#v", kinds)
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

func TestCapabilityOperationMatrixAndDMLClauses(t *testing.T) {
	tests := []struct {
		name     string
		driver   string
		sql      string
		readonly bool
		result   ResultDef
		want     sqlOperation
		wantErr  string
	}{
		{"readonly select", "postgres", "-- lead\nSELECT 1; /* tail */", true, nil, sqlOperationSelect, ""},
		{"writable select", "postgres", "select 1", false, nil, sqlOperationSelect, ""},
		{"insert", "postgres", "insert into t(v) values ('returning; output')", false, nil, sqlOperationInsert, ""},
		{"update", "mysql", "UPDATE t SET v=1", false, nil, sqlOperationUpdate, ""},
		{"delete", "oracle", "delete from t where id=1", false, nil, sqlOperationDelete, ""},
		{"readonly mutation", "postgres", "insert into t(v) values (1)", true, nil, "", "must be SELECT"},
		{"multiple select", "postgres", "select 1; update t set v=2", false, nil, "", "exactly one statement"},
		{"multiple mutation", "sqlserver", "update t set v=1; delete from t", false, nil, "", "exactly one statement"},
		{"postgres nested leading comment", "postgres", "/* outer /* inner */ SELECT */ UPDATE t SET v=1", true, nil, "", "must be SELECT"},
		{"sqlserver nested leading comment", "sqlserver", "/* outer /* inner */ SELECT */ UPDATE t SET v=1", true, nil, "", "must be SELECT"},
		{"postgres nested trailing trivia", "postgres", "SELECT 1; /* outer /* inner */ still outer */", true, nil, sqlOperationSelect, ""},
		{"sqlserver nested trailing trivia", "sqlserver", "SELECT 1; /* outer /* inner */ still outer */", true, nil, sqlOperationSelect, ""},
		{"cte", "postgres", "WITH x AS (SELECT 1) SELECT * FROM x", false, nil, "", "operation is not supported"},
		{"returning", "postgres", "insert into t(v) values (1) RETURNING id", false, nil, "", "returning rows"},
		{"output", "sqlserver", "update t set v=1 OUTPUT inserted.id", false, nil, "", "returning rows"},
		{"empty result", "postgres", "delete from t", false, ResultDef{}, "", "result is only supported"},
		{"postgres nested leading comment", "postgres", "/* outer /* inner */ SELECT */ UPDATE t SET v=1", true, nil, "", "must be SELECT"},
		{"sqlserver nested leading comment", "sqlserver", "/* outer /* inner */ SELECT */ UPDATE t SET v=1", true, nil, "", "must be SELECT"},
		{"postgres nested trailing trivia", "postgres", "SELECT 1; /* outer /* inner */ tail */", true, nil, sqlOperationSelect, ""},
		{"sqlserver nested trailing trivia", "sqlserver", "SELECT 1; /* outer /* inner */ tail */", true, nil, sqlOperationSelect, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cf := validCapabilityFile()
			cf.Database.Driver = tc.driver
			cap := cf.Capabilities["get_customer"]
			cap.SQL, cap.Policy.Readonly, cap.Result = tc.sql, boolPtr(tc.readonly), tc.result
			cf.Capabilities["get_customer"] = cap
			err := cf.Lint()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Lint() error=%v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cf.Capabilities["get_customer"].Operation; got != tc.want {
				t.Fatalf("operation=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestMutationPayloadIsObjectAndPreservesInt64(t *testing.T) {
	payload, err := buildMutationPayload(9223372036854775807, 128)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(payload); got != `{"count":9223372036854775807}` {
		t.Fatalf("payload=%s", got)
	}
	response := protocol.Response{ID: "1", Result: payload}
	if got := string(protocol.MustJSON(response)); !strings.Contains(got, `"result":{"count":9223372036854775807}`) {
		t.Fatalf("wire=%s", got)
	}
	if _, err := buildMutationPayload(1, 1); !errors.Is(err, errMutationResponseTooLarge) {
		t.Fatalf("small maxBytes error=%v", err)
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
				Policy:      PolicyDef{Readonly: &readonly, Timeout: "1s", MaxRows: intPtr(1), MaxBytes: "128KB"},
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

func intPtr(v int) *int {
	return &v
}
