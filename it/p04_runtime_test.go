//go:build integration

package it

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
	"github.com/viewlegacy/onprest/internal/contract"
)

// This test intentionally inspects the real envelopes, without the business
// projections used by the earlier conformance assertions.
func TestContainerDBDriverVersionedRuntimePublication(t *testing.T) {
	for _, driver := range []string{"postgres", "mysql", "sqlserver", "oracle"} {
		if !selectedDBForTest(t, driver) {
			continue
		}
		t.Run(driver, func(t *testing.T) {
			db := selectedContainerDBConfig(t, driver)
			statements := []string{"drop table if exists onprest_p04_mutation", "create table onprest_p04_mutation (id bigint primary key, value integer not null)", "insert into onprest_p04_mutation values (1,1)", "insert into onprest_p04_mutation values (2,1)"}
			if driver == "oracle" {
				statements = []string{"begin execute immediate 'drop table onprest_p04_mutation purge'; exception when others then if sqlcode != -942 then raise; end if; end;", "create table onprest_p04_mutation (id number(19,0) primary key, value number(10,0) not null)", "insert into onprest_p04_mutation values (1,1)", "insert into onprest_p04_mutation values (2,1)"}
			}
			execDBStatements(t, driver, db, statements)
			normalization := map[string]string{
				"postgres":  `select :id::bigint as id, decode('00ff01','hex') as binary_value, '日本'::text as text_value, 9007199254740993.123456789012::numeric(28,12) as decimal_value, '2024-01-02 03:04:05+09'::timestamptz as timestamp_value, NULL::text as null_value`,
				"mysql":     `select cast(:id as signed) as id, cast(UNHEX('00ff01') as binary(3)) as binary_value, cast('日本' as char) as text_value, cast(9007199254740993.123456789012 as decimal(28,12)) as decimal_value, cast('2024-01-02 03:04:05' as datetime) as timestamp_value, NULL as null_value`,
				"sqlserver": `select cast(:id as bigint) as id, cast(0x00ff01 as varbinary(3)) as binary_value, cast(N'日本' as nvarchar(10)) as text_value, cast(9007199254740993.123456789012 as decimal(28,12)) as decimal_value, cast('2024-01-02T03:04:05+09:00' as datetimeoffset) as timestamp_value, cast(NULL as nvarchar(10)) as null_value`,
				"oracle":    `select cast(:id as number(19,0)) as "id", hextoraw('00ff01') as "binary_value", cast('日本' as nvarchar2(10)) as "text_value", cast(9007199254740993.123456789012 as number(28,12)) as "decimal_value", to_timestamp_tz('2024-01-02 03:04:05 +09:00','YYYY-MM-DD HH24:MI:SS TZH:TZM') as "timestamp_value", cast(NULL as nvarchar2(10)) as "null_value" from dual`,
			}[driver]
			selectAll := "select id, value from onprest_p04_mutation order by id"
			if driver == "oracle" {
				selectAll = `select id as "id", value as "value" from onprest_p04_mutation order by id`
			}
			secrets := newITSecrets(t)
			addr := freeAddr(t)
			tmp := t.TempDir()
			capabilities := fmt.Sprintf(`normalize:
  executor: {type: db, source: primary, sql: %s}
  input: {properties: {id: {type: integer}}, required: [id]}
  output:
    items:
      properties:
        id: {type: integer}
        binary_value: {type: string}
        text_value: {type: string}
        decimal_value: {type: string}
        timestamp_value: {type: string}
        null_value: {type: [string, "null"]}
    data: {}
limited:
  executor: {type: db, source: primary, sql: %s}
  policy: {max_rows: 1}
  output: {data: {}}
rollback_output:
  executor: {type: db, source: primary, sql: 'update onprest_p04_mutation set value=2 where id=1'}
  policy: {readonly: false}
  output: {properties: {count: {type: integer, maximum: 0}}, data: {}}
rollback_capacity:
  executor: {type: db, source: primary, sql: 'update onprest_p04_mutation set value=2 where id=1'}
  policy: {readonly: false, max_output_bytes: 1}
  output: {data: {}}
rollback_rows:
  executor: {type: db, source: primary, sql: 'update onprest_p04_mutation set value=2'}
  policy: {readonly: false, max_affected_rows: 1}
  output: {data: {}}
suppressed:
  executor: {type: db, source: primary, sql: 'update onprest_p04_mutation set value=3 where id=1'}
  policy: {readonly: false}
committed:
  executor: {type: db, source: primary, sql: 'update onprest_p04_mutation set value=4 where id=1'}
  policy: {readonly: false}
  output: {properties: {count: {type: integer}}, data: {}}
`, yamlString(normalization), yamlString(selectAll))
			path := writeContainerCapability(t, tmp, driver, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, "select 1")
			data, e := osReadCapability(path)
			if e != nil {
				t.Fatal(e)
			}
			a := strings.Index(data, "capabilities:")
			if a < 0 {
				t.Fatal("fixture missing capabilities")
			}
			data = data[:a] + "capabilities:\n  " + strings.ReplaceAll(strings.TrimSpace(capabilities), "\n", "\n  ") + "\n"
			writeFile(t, path, data)
			var logs bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			base := startInternalGatewayWithLog(t, ctx, addr, secrets, time.Second, &logs)
			runner, e := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: path, ReconnectEvery: 50 * time.Millisecond}, nil)
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			waitForHTTP(t, base+"/openapi.json", secrets.APIKey, 200)
			checkNormalize := func(raw []byte) {
				t.Helper()
				var envelope contract.SuccessEnvelope
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.UseNumber()
				if e := dec.Decode(&envelope); e != nil || len(envelope.RequestID) != 26 || len(envelope.Result.Files) != 0 {
					t.Fatalf("invalid envelope %s: %v", raw, e)
				}
				object, ok := envelope.Result.Data.(map[string]any)
				if !ok {
					t.Fatalf("invalid DB data: %v", envelope.Result.Data)
				}
				rows, ok := object["rows"].([]any)
				if !ok || len(rows) != 1 {
					t.Fatalf("rows=%v", envelope.Result.Data)
				}
				r := rows[0].(map[string]any)
				if r["id"] != json.Number("9007199254740993") || r["binary_value"] != "AP8B" || r["text_value"] != "日本" || r["decimal_value"] != "9007199254740993.123456789012" || r["null_value"] != nil {
					t.Fatalf("normalized row=%#v", r)
				}
				timestamp, ok := r["timestamp_value"].(string)
				wantTime, wantOffset := "2024-01-02 03:04:05", "+0900"
				if driver == "postgres" { // pgx decodes timestamptz in the session's UTC location.
					wantTime, wantOffset = "2024-01-01 18:04:05", "+0000"
				}
				if !ok || !strings.Contains(timestamp, wantTime) || (driver != "mysql" && !strings.Contains(timestamp, wantOffset)) {
					t.Fatalf("timestamp=%v, want time %s offset %s", r["timestamp_value"], wantTime, wantOffset)
				}
			}
			status, body := postCapabilityEnvelope(t, base, secrets.APIKey, "normalize", `{"id":9007199254740993}`)
			if status != 200 {
				t.Fatalf("normalize status=%d body=%s logs=%s", status, body, logs.String())
			}
			checkNormalize(body)
			mcp := requireMCPToolCallResponse(t, postMCPPayloadWithProtocol(t, base, secrets.APIKey, modernMCPProtocolVersion, `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"normalize","arguments":{"id":9007199254740993}}}`), "42", true)
			checkNormalize([]byte(mcp.Result.Content[0].Text))
			if !bytes.Equal([]byte(mcp.Result.Content[0].Text), mcp.Result.StructuredContent) {
				var text, structured any
				_ = json.Unmarshal([]byte(mcp.Result.Content[0].Text), &text)
				_ = json.Unmarshal(mcp.Result.StructuredContent, &structured)
				a, _ := json.Marshal(text)
				b, _ := json.Marshal(structured)
				if !bytes.Equal(a, b) {
					t.Fatal("MCP envelopes disagree")
				}
			}
			for _, c := range []struct {
				name, code string
				status     int
			}{{"rollback_output", "OUTPUT_INVALID", 502}, {"rollback_capacity", "LIMIT_EXCEEDED", 422}, {"rollback_rows", "LIMIT_EXCEEDED", 422}} {
				status, raw := postCapabilityEnvelope(t, base, secrets.APIKey, c.name, `{}`)
				var env contract.ErrorEnvelope
				_ = json.Unmarshal(raw, &env)
				if status != c.status || env.Error.Code != c.code || env.Error.Outcome != "rolled_back" || len(env.RequestID) != 26 {
					t.Fatalf("%s: status=%d body=%s", c.name, status, raw)
				}
				assertP04Value(t, driver, db, 1)
			}
			status, body = postCapabilityEnvelope(t, base, secrets.APIKey, "limited", `{}`)
			var limited contract.SuccessEnvelope
			_ = json.Unmarshal(body, &limited)
			if status != 200 || len(limited.Result.Data.(map[string]any)["rows"].([]any)) != 1 {
				t.Fatalf("max_rows no longer truncates: %s", body)
			}
			for _, name := range []string{"suppressed", "committed"} {
				status, body = postCapabilityEnvelope(t, base, secrets.APIKey, name, `{}`)
				var env contract.SuccessEnvelope
				_ = json.Unmarshal(body, &env)
				if status != 200 || len(env.RequestID) != 26 || len(env.Result.Files) != 0 {
					t.Fatalf("%s status=%d body=%s", name, status, body)
				}
				if name == "suppressed" && env.Result.Data != nil {
					t.Fatal("omitted output exposed data")
				}
				if name == "committed" && env.Result.Data.(map[string]any)["count"] != float64(1) {
					t.Fatal("incorrect affected data")
				}
				want := 3
				if name == "committed" {
					want = 4
				}
				assertP04Value(t, driver, db, want)
			}
			status, body = postCapabilityEnvelope(t, base, secrets.APIKey, "normalize", `{"id":1.5}`)
			var invalid contract.ErrorEnvelope
			_ = json.Unmarshal(body, &invalid)
			if status != 400 || invalid.Error.Code != "VALIDATION_FAILED" || invalid.Error.Outcome != "not_started" {
				t.Fatalf("invalid input: %s", body)
			}
			if !strings.Contains(logs.String(), `"affected_rows":1`) || !strings.Contains(logs.String(), `"returned_rows":1`) || !strings.Contains(logs.String(), `"response_bytes":4`) {
				t.Fatal("real typed execution metrics missing")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("runner failed to stop")
			}
		})
	}
}
func osReadCapability(path string) (string, error) { b, e := os.ReadFile(path); return string(b), e }
func assertP04Value(t *testing.T, driver string, cfg postgresConfig, want int) {
	t.Helper()
	db, e := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, cfg))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var got int
	if e = db.QueryRow("select value from onprest_p04_mutation where id=1").Scan(&got); e != nil || got != want {
		t.Fatalf("DB value=%d want=%d err=%v", got, want, e)
	}
}
