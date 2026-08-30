//go:build integration

package it

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
)

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func TestContainerDBDriverSmoke(t *testing.T) {
	agentBin := filepath.Join(t.TempDir(), "onprest-agent")
	buildBinary(t, repoRoot(t), agentBin, "./cmd/agent")
	for _, tc := range []struct {
		name   string
		driver string
		sql    string
	}{
		{name: "postgres", driver: "postgres", sql: "select id, name, email from onprest_it_customers where id = :id"},
		{name: "mysql", driver: "mysql", sql: "select id, name, email from onprest_it_customers where id = :id"},
		{name: "sqlserver", driver: "sqlserver", sql: "select id, name, email from onprest_it_customers where id = :id"},
		{name: "oracle", driver: "oracle", sql: `select id as "id", name as "name", email as "email" from onprest_it_customers where id = :id`},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			db := selectedContainerDBConfig(t, tc.driver)
			seedCustomerTable(t, tc.driver, db)
			secrets := newITSecrets(t)
			addr := freeAddr(t)
			tmp := t.TempDir()
			capabilityFile := writeContainerCapability(t, tmp, tc.driver, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, tc.sql)
			validate := exec.Command(agentBin, "validate", "--config", capabilityFile, "--format", "json")
			validate.Dir = tmp
			validateOutput, err := validate.CombinedOutput()
			if err != nil {
				t.Fatalf("production validate failed for %s: %v\n%s", tc.driver, err, validateOutput)
			}
			validation := string(validateOutput)
			if !strings.Contains(validation, `"valid":true`) || !strings.Contains(validation, `"database_driver":"`+tc.driver+`"`) || strings.Contains(validation, "agent_ready") || strings.Contains(validation, "gateway_") {
				t.Fatalf("production validate output for %s: %s", tc.driver, validation)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			baseURL := startInternalGateway(t, ctx, addr, secrets, 2*time.Second)
			runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
			if err != nil {
				t.Fatal(err)
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runner.Run(ctx) }()
			waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

			status, body := postCapability(t, baseURL, secrets.APIKey, "get_customer", `{"id":7}`)
			if status != http.StatusOK {
				t.Fatalf("get_customer status=%d body=%s", status, string(body))
			}
			if !strings.Contains(string(body), `"count":1`) ||
				!strings.Contains(string(body), `"Ada"`) ||
				!strings.Contains(string(body), `"ada@example.com"`) {
				t.Fatalf("unexpected response: %s", string(body))
			}
			mcpBody := postMCPPayload(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{"id":8}}}`)
			if !strings.Contains(string(mcpBody), `"structuredContent"`) ||
				!strings.Contains(string(mcpBody), `"count":1`) ||
				!strings.Contains(string(mcpBody), `"Grace"`) ||
				!strings.Contains(string(mcpBody), `"grace@example.com"`) {
				t.Fatalf("unexpected MCP tools/call response: %s", string(mcpBody))
			}
			cancel()
			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Fatal("agent runner did not stop")
			}
		})
	}
}

func TestContainerDBDriverMCPInt64BoundaryReachesRealDatabaseExactly(t *testing.T) {
	const boundary = "9007199254740993"
	for _, tc := range []struct {
		driver string
		sql    string
	}{
		{driver: "postgres", sql: "select :id::bigint as id, 'boundary'::text as name, 'x'::text as email"},
		{driver: "mysql", sql: "select cast(:id as signed) as id, 'boundary' as name, 'x' as email"},
		{driver: "sqlserver", sql: "select cast(:id as bigint) as id, cast('boundary' as nvarchar(20)) as name, cast('x' as nvarchar(20)) as email"},
		{driver: "oracle", sql: `select cast(:id as number(19)) as "id", 'boundary' as "name", 'x' as "email" from dual`},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.driver, func(t *testing.T) {
			db := selectedContainerDBConfig(t, tc.driver)
			secrets := newITSecrets(t)
			addr := freeAddr(t)
			capabilityFile := writeContainerCapability(t, t.TempDir(), tc.driver, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, tc.sql)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			baseURL := startInternalGateway(t, ctx, addr, secrets, 3*time.Second)
			runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
			if err != nil {
				t.Fatal(err)
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runner.Run(ctx) }()
			waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
			body := postMCPPayload(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{"id":`+boundary+`}}}`)
			if !strings.Contains(string(body), `"id":`+boundary) {
				t.Fatalf("MCP -> agent -> %s changed int64 boundary: %s", tc.driver, body)
			}
			cancel()
			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Fatal("agent runner did not stop")
			}
		})
	}
}

func TestContainerDBDriverMutationMatrix(t *testing.T) {
	for _, driver := range []string{"postgres", "mysql", "sqlserver", "oracle"} {
		if !selectedDBForTest(t, driver) {
			continue
		}
		t.Run(driver, func(t *testing.T) {
			dbCfg := selectedContainerDBConfig(t, driver)
			setup := []string{
				"drop table if exists onprest_it_mutations",
				"drop table if exists onprest_it_mutation_parents",
				"create table onprest_it_mutation_parents (id integer primary key)",
				"insert into onprest_it_mutation_parents (id) values (1)",
				"create table onprest_it_mutations (id integer primary key, name varchar(100) not null unique, score integer not null, parent_id integer not null, constraint onprest_it_mutations_score_ck check (score >= 0), constraint onprest_it_mutations_parent_fk foreign key (parent_id) references onprest_it_mutation_parents(id))",
			}
			if driver == "sqlserver" {
				setup[0] = "if object_id('dbo.onprest_it_mutations', 'U') is not null drop table dbo.onprest_it_mutations"
				setup[1] = "if object_id('dbo.onprest_it_mutation_parents', 'U') is not null drop table dbo.onprest_it_mutation_parents"
			}
			if driver == "oracle" {
				setup[0] = "begin execute immediate 'drop table onprest_it_mutations purge'; exception when others then if sqlcode != -942 then raise; end if; end;"
				setup[1] = "begin execute immediate 'drop table onprest_it_mutation_parents purge'; exception when others then if sqlcode != -942 then raise; end if; end;"
			}
			execDBStatements(t, driver, dbCfg, setup)
			secrets := newITSecrets(t)
			addr := freeAddr(t)
			capabilityFile := writeMutationCapability(t, t.TempDir(), driver, dbCfg, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gatewayLogs := &lockedBuffer{}
			baseURL := startInternalGatewayWithLog(t, ctx, addr, secrets, 8*time.Second, gatewayLogs)
			runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if driver == "oracle" {
				assertOraclePlanRows(t, dbCfg, 0)
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runner.Run(ctx) }()
			waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
			assertMutation := func(cap, body string, want int) {
				status, payload := postCapability(t, baseURL, secrets.APIKey, cap, body)
				if status != http.StatusOK || string(payload) != fmt.Sprintf(`{"count":%d}`, want) {
					t.Fatalf("%s status=%d payload=%s", cap, status, payload)
				}
			}
			assertMutation("insert_row", `{"id":1,"name":"Ada"}`, 1)
			assertMutation("insert_many", `{}`, 2)
			assertMutationName(t, driver, dbCfg, 10, "Multi-A")
			assertMutationName(t, driver, dbCfg, 11, "Multi-B")
			if driver == "mysql" {
				assertMutation("mysql_upsert", `{}`, 2)
				assertMutationName(t, driver, dbCfg, 10, "Multi-A-updated")
			}
			assertMutation("update_row", `{"id":1,"name":"Grace"}`, 1)
			assertMutation("update_row", `{"id":999,"name":"Nobody"}`, 0)
			assertMutation("delete_row", `{"id":1}`, 1)
			assertMutation("delete_row", `{"id":1}`, 0)
			mcp := postMCPPayload(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"insert_row","arguments":{"id":2,"name":"MCP"}}}`)
			if !strings.Contains(string(mcp), `"structuredContent":{"count":1}`) {
				t.Fatalf("MCP mutation response=%s", mcp)
			}
			status, body := postCapability(t, baseURL, secrets.APIKey, "insert_row", `{"id":2,"name":"duplicate"}`)
			if status != http.StatusConflict {
				t.Fatalf("constraint status=%d body=%s", status, body)
			}
			requireAPIErrorCode(t, body, "AGENT_CONSTRAINT_VIOLATION")
			for _, constraintCapability := range []string{"insert_not_null", "insert_check", "insert_fk"} {
				status, body = postCapability(t, baseURL, secrets.APIKey, constraintCapability, `{}`)
				if status != http.StatusConflict {
					t.Fatalf("%s constraint status=%d body=%s", constraintCapability, status, body)
				}
				requireAPIErrorCode(t, body, "AGENT_CONSTRAINT_VIOLATION")
			}
			if got := mutationRowCount(t, driver, dbCfg); got != 3 {
				t.Fatalf("constraint failures changed DB row count to %d", got)
			}
			status, body = postCapability(t, baseURL, secrets.APIKey, "list_rows", `{}`)
			if status != http.StatusOK || !strings.Contains(string(body), `"name":"MCP"`) {
				t.Fatalf("select compatibility status=%d body=%s", status, body)
			}
			blocker := lockMutationRow(t, driver, dbCfg, 2)
			var oraclePolicyUnlock <-chan struct{}
			if driver == "oracle" {
				// go-ora sends a Break on context cancellation, but an UPDATE waiting
				// on a row lock does not return until the blocker is released. Release
				// it just after policy.timeout so the Agent can observe the expired
				// exec context, roll back explicitly, and return its policy-timeout
				// classification. The Gateway-timeout case below deliberately keeps
				// the lock until after the public timeout to exercise cancel cleanup.
				unlocked := make(chan struct{})
				oraclePolicyUnlock = unlocked
				go func() {
					timer := time.NewTimer(2500 * time.Millisecond)
					defer timer.Stop()
					<-timer.C
					_ = blocker.Rollback()
					close(unlocked)
				}()
			}
			status, body = postCapability(t, baseURL, secrets.APIKey, "update_row", `{"id":2,"name":"policy-timeout"}`)
			if status != http.StatusBadGateway && status != http.StatusGatewayTimeout {
				t.Fatalf("policy timeout status=%d body=%s", status, body)
			}
			if !strings.Contains(string(body), `"code":"AGENT_TRANSACTION_OUTCOME_UNKNOWN"`) && !strings.Contains(string(body), `"code":"AGENT_QUERY_TIMEOUT"`) {
				t.Fatalf("policy timeout returned unsafe classification: %s", body)
			}
			if oraclePolicyUnlock != nil {
				<-oraclePolicyUnlock
			} else {
				_ = blocker.Rollback()
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if mutationName(t, driver, dbCfg, 2) == "MCP" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("policy-canceled mutation changed DB state")
				}
				time.Sleep(50 * time.Millisecond)
			}

			blocker = lockMutationRow(t, driver, dbCfg, 2)
			status, body = postCapability(t, baseURL, secrets.APIKey, "slow_update", `{"id":2,"name":"gateway-timeout"}`)
			if status != http.StatusGatewayTimeout {
				t.Fatalf("gateway timeout status=%d body=%s", status, body)
			}
			requireAPIErrorCode(t, body, "GATEWAY_TIMEOUT")
			_ = blocker.Rollback()
			deadline = time.Now().Add(5 * time.Second)
			for {
				if mutationName(t, driver, dbCfg, 2) == "MCP" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("canceled mutation changed DB state")
				}
				time.Sleep(50 * time.Millisecond)
			}
			if driver == "oracle" {
				assertOraclePlanRows(t, dbCfg, 0)
			}
			assertMutationGatewayCountLogs(t, gatewayLogs.String())
			cancel()
			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Fatal("agent runner did not stop")
			}
		})
	}
}

func writeMutationCapability(t *testing.T, dir, driver string, db postgresConfig, gatewayURL, agentPrivateKey string) string {
	t.Helper()
	selectSQL := "select id, name from onprest_it_mutations order by id"
	if driver == "oracle" {
		selectSQL = `select id as "id", name as "name" from onprest_it_mutations order by id`
	}
	multiInsert := "insert into onprest_it_mutations (id, name, score, parent_id) values (10, 'Multi-A', 0, 1), (11, 'Multi-B', 0, 1)"
	if driver == "oracle" {
		multiInsert = "insert all into onprest_it_mutations (id, name, score, parent_id) values (10, 'Multi-A', 0, 1) into onprest_it_mutations (id, name, score, parent_id) values (11, 'Multi-B', 0, 1) select 1 from dual"
	}
	mysqlUpsertBlock := ""
	if driver == "mysql" {
		mysqlUpsertBlock = `  mysql_upsert:
    sql: insert into onprest_it_mutations (id, name, score, parent_id) values (10, 'ignored', 0, 1) on duplicate key update name = 'Multi-A-updated'
    policy: {readonly: false, timeout: 2s, max_bytes: 128KB}
`
	}
	content := fmt.Sprintf(`service:
  title: Mutation IT
  version: 0.1.0
database:
  driver: %s
  host: %s
  port: %s
  name: %s
  user: %s
  password: %s
gateway:
  url: %s
  agent_private_key: %s
capabilities:
  insert_row:
    sql: %s
    params:
      id: {type: integer, required: true}
      name: {type: string, required: true}
    policy: {readonly: false, timeout: 2s, max_rows: 1, max_bytes: 128KB}
  insert_many:
    sql: %s
    policy: {readonly: false, timeout: 2s, max_bytes: 128KB}
%s  insert_not_null:
    sql: insert into onprest_it_mutations (id, name, score, parent_id) values (20, NULL, 0, 1)
    policy: {readonly: false, timeout: 2s, max_bytes: 128KB}
  insert_check:
    sql: insert into onprest_it_mutations (id, name, score, parent_id) values (21, 'bad-check', -1, 1)
    policy: {readonly: false, timeout: 2s, max_bytes: 128KB}
  insert_fk:
    sql: insert into onprest_it_mutations (id, name, score, parent_id) values (22, 'bad-fk', 0, 999)
    policy: {readonly: false, timeout: 2s, max_bytes: 128KB}
  update_row:
    sql: %s
    params:
      id: {type: integer, required: true}
      name: {type: string, required: true}
    policy: {readonly: false, timeout: 2s, max_rows: 1, max_bytes: 128KB}
  delete_row:
    sql: %s
    params:
      id: {type: integer, required: true}
    policy: {readonly: false, timeout: 2s, max_rows: 1, max_bytes: 128KB}
  slow_update:
    sql: %s
    params:
      id: {type: integer, required: true}
      name: {type: string, required: true}
    policy: {readonly: false, timeout: 10s, max_rows: 1, max_bytes: 128KB}
  list_rows:
    sql: %s
    policy: {readonly: false, timeout: 2s, max_rows: 100, max_bytes: 128KB}
    result:
      id: {type: integer}
      name: {type: string}
`, driver, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey),
		yamlString("insert into onprest_it_mutations (id, name, score, parent_id) values (:id, :name, 0, 1)"), yamlString(multiInsert), mysqlUpsertBlock, yamlString("update onprest_it_mutations set name = :name where id = :id"), yamlString("delete from onprest_it_mutations where id = :id"), yamlString("update onprest_it_mutations set name = :name where id = :id"), yamlString(selectSQL))
	return writeFile(t, filepath.Join(dir, "capability.mutations."+driver+".yaml"), content)
}

func lockMutationRow(t *testing.T, driver string, cfg postgresConfig, id int) *sql.Tx {
	t.Helper()
	db, err := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	query := fmt.Sprintf("select id from onprest_it_mutations where id = %d for update", id)
	if driver == "sqlserver" {
		query = fmt.Sprintf("select id from onprest_it_mutations with (updlock, holdlock) where id = %d", id)
	}
	var got int
	if err := tx.QueryRow(query).Scan(&got); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	return tx
}

func mutationName(t *testing.T, driver string, cfg postgresConfig, id int) string {
	t.Helper()
	db, err := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRow(fmt.Sprintf("select name from onprest_it_mutations where id = %d", id)).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}
func mutationRowCount(t *testing.T, driver string, cfg postgresConfig) int {
	t.Helper()
	db, err := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("select count(*) from onprest_it_mutations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func mutationGuardValue(t *testing.T, driver string, cfg postgresConfig, table string) int {
	t.Helper()
	db, err := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value int
	if err := db.QueryRow("select value from " + table + " where id = 1").Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func assertMutationGatewayCountLogs(t *testing.T, raw string) {
	t.Helper()
	events := parseJSONLines(t, raw)
	var successTwo, successZero, errorNull bool
	for _, event := range events {
		if event["event"] != "request" {
			continue
		}
		status := int(event["http_status"].(float64))
		count := event["count"]
		switch {
		case status == http.StatusOK && count == float64(2):
			successTwo = true
		case status == http.StatusOK && count == float64(0):
			successZero = true
		case status >= 400 && count == nil:
			errorNull = true
		}
	}
	if !successTwo || !successZero || !errorNull {
		t.Fatalf("gateway count logs missing success/error contract: successTwo=%t successZero=%t errorNull=%t logs=%s", successTwo, successZero, errorNull, raw)
	}
}
func assertMutationName(t *testing.T, driver string, cfg postgresConfig, id int, want string) {
	t.Helper()
	if got := mutationName(t, driver, cfg, id); got != want {
		t.Fatalf("mutation name=%q want=%q", got, want)
	}
}
func assertOraclePlanRows(t *testing.T, cfg postgresConfig, want int) {
	t.Helper()
	db, err := sql.Open(sqlDriverName("oracle"), integrationDBDSN("oracle", cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got int
	if err := db.QueryRow("select count(*) from plan_table where statement_id like 'onprest_%'").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Oracle PLAN_TABLE rows=%d want=%d", got, want)
	}
}

func TestMySQLDSNSpecialCredentialsConnectToRealDatabase(t *testing.T) {
	if !selectedDBForTest(t, "mysql") {
		return
	}
	admin := selectedContainerDBConfig(t, "mysql")
	seedCustomerTable(t, "mysql", admin)
	root := admin
	root.User = "root"
	special := admin
	special.User = "onprest@special/name"
	special.Password = "p@ss:/word?&=#"
	execDBStatements(t, "mysql", root, []string{
		"CREATE USER IF NOT EXISTS 'onprest@special/name'@'%' IDENTIFIED BY 'p@ss:/word?&=#'",
		"GRANT SELECT ON `" + admin.Name + "`.* TO 'onprest@special/name'@'%'",
		"FLUSH PRIVILEGES",
	})
	t.Cleanup(func() {
		execDBStatements(t, "mysql", root, []string{"DROP USER IF EXISTS 'onprest@special/name'@'%'"})
	})

	secrets := newITSecrets(t)
	addr := freeAddr(t)
	capabilityFile := writeContainerCapability(t, t.TempDir(), "mysql", special, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey,
		"select id, name, email from onprest_it_customers where id = :id")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, 2*time.Second)
	runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("NewRunner with special MySQL credentials: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
	status, body := postCapability(t, baseURL, secrets.APIKey, "get_customer", `{"id":7}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"Ada"`) {
		t.Fatalf("special MySQL credential query status=%d body=%s", status, body)
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}

func TestContainerDBDriverReadonlyBackslashQuoteCannotExecuteSecondStatement(t *testing.T) {
	for _, tc := range []struct {
		driver string
		attack string
		setup  []string
	}{
		{driver: "postgres", attack: `SELECT '\'; UPDATE onprest_readonly_guard SET value = 99 WHERE id = 1`, setup: []string{
			"drop table if exists onprest_readonly_guard",
			"create table onprest_readonly_guard (id integer primary key, value integer not null)",
			"insert into onprest_readonly_guard values (1, 7)",
		}},
		{driver: "mysql", attack: `SELECT '\'; UPDATE onprest_readonly_guard SET value = 99 WHERE id = 1`, setup: []string{
			"drop table if exists onprest_readonly_guard",
			"create table onprest_readonly_guard (id integer primary key, value integer not null)",
			"insert into onprest_readonly_guard values (1, 7)",
		}},
		{driver: "sqlserver", attack: `SELECT '\'; UPDATE dbo.onprest_readonly_guard SET value = 99 WHERE id = 1`, setup: []string{
			"if object_id('dbo.onprest_readonly_guard', 'U') is not null drop table dbo.onprest_readonly_guard",
			"create table dbo.onprest_readonly_guard (id int primary key, value int not null)",
			"insert into dbo.onprest_readonly_guard values (1, 7)",
		}},
		{driver: "oracle", attack: `SELECT '\' FROM dual; UPDATE onprest_readonly_guard SET value = 99 WHERE id = 1`, setup: []string{
			"begin execute immediate 'drop table onprest_readonly_guard purge'; exception when others then if sqlcode != -942 then raise; end if; end;",
			"create table onprest_readonly_guard (id number(10) primary key, value number(10) not null)",
			"insert into onprest_readonly_guard values (1, 7)",
		}},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.driver, func(t *testing.T) {
			cfg := selectedContainerDBConfig(t, tc.driver)
			execDBStatements(t, tc.driver, cfg, tc.setup)
			secrets := newITSecrets(t)
			path := writeContainerCapability(t, t.TempDir(), tc.driver, cfg, "ws://127.0.0.1:1/ws/agent", secrets.AgentPrivateKey, tc.attack)
			if _, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: path, ReconnectEvery: time.Second}, nil); err == nil {
				t.Fatal("readonly lint accepted a backslash-quote multi-statement attack")
			}

			db, err := sql.Open(sqlDriverName(tc.driver), integrationDBDSN(tc.driver, cfg))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var value int
			if err := db.QueryRowContext(ctx, "select value from onprest_readonly_guard where id = 1").Scan(&value); err != nil {
				t.Fatal(err)
			}
			if value != 7 {
				t.Fatalf("protected real DB value changed to %d", value)
			}
		})
	}
}

func TestPostgresNestedBlockCommentCannotBypassReadonly(t *testing.T) {
	if !selectedDBForTest(t, "postgres") {
		return
	}
	cfg := selectedContainerDBConfig(t, "postgres")
	execDBStatements(t, "postgres", cfg, []string{
		"drop table if exists onprest_nested_comment_guard",
		"create table onprest_nested_comment_guard (id integer primary key, value integer not null)",
		"insert into onprest_nested_comment_guard values (1, 7)",
	})
	secrets := newITSecrets(t)
	attack := "/* outer /* inner */ SELECT */ UPDATE onprest_nested_comment_guard SET value = 99 WHERE id = 1"
	path := writeContainerCapability(t, t.TempDir(), "postgres", cfg, "ws://127.0.0.1:1/ws/agent", secrets.AgentPrivateKey, attack)
	if _, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: path, ReconnectEvery: time.Second}, nil); err == nil {
		t.Fatal("readonly lint accepted a nested block-comment classification bypass")
	}
	db, err := sql.Open(sqlDriverName("postgres"), integrationDBDSN("postgres", cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value int
	if err := db.QueryRow("select value from onprest_nested_comment_guard where id = 1").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 7 {
		t.Fatalf("nested-comment bypass changed protected value to %d", value)
	}
}

func TestContainerDBDriverWritableMultipleStatementsCannotReachDatabase(t *testing.T) {
	for _, driver := range []string{"postgres", "mysql", "sqlserver", "oracle"} {
		if !selectedDBForTest(t, driver) {
			continue
		}
		t.Run(driver, func(t *testing.T) {
			cfg := selectedContainerDBConfig(t, driver)
			setup := []string{
				"drop table if exists onprest_writable_guard",
				"create table onprest_writable_guard (id integer primary key, value integer not null)",
				"insert into onprest_writable_guard values (1, 7)",
			}
			if driver == "sqlserver" {
				setup[0] = "if object_id('dbo.onprest_writable_guard', 'U') is not null drop table dbo.onprest_writable_guard"
			}
			if driver == "oracle" {
				setup[0] = "begin execute immediate 'drop table onprest_writable_guard purge'; exception when others then if sqlcode != -942 then raise; end if; end;"
			}
			execDBStatements(t, driver, cfg, setup)
			secrets := newITSecrets(t)
			attack := "update onprest_writable_guard set value = 99 where id = 1; delete from onprest_writable_guard where id = 1"
			path := writeContainerWritableCapability(t, t.TempDir(), driver, cfg, "ws://127.0.0.1:1/ws/agent", secrets.AgentPrivateKey, attack)
			if _, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: path, ReconnectEvery: time.Second}, nil); err == nil {
				t.Fatal("writable lint accepted multiple statements")
			}
			if got := mutationGuardValue(t, driver, cfg, "onprest_writable_guard"); got != 7 {
				t.Fatalf("multiple-statement capability changed protected value to %d", got)
			}
		})
	}
}

func TestContainerDBDriverErrorsAreHiddenFromGatewayResponse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		driver     string
		sql        string
		payload    string
		resultType string
	}{
		{name: "postgres", driver: "postgres", sql: "select (1 / :denominator::int)::int as value", payload: `{"denominator":0}`, resultType: "integer"},
		{name: "mysql", driver: "mysql", sql: "select json_extract(:payload, '$.x') as value", payload: `{"payload":"not-json"}`, resultType: "string"},
		{name: "sqlserver", driver: "sqlserver", sql: "select cast(1 / :denominator as int) as value", payload: `{"denominator":0}`, resultType: "integer"},
		{name: "oracle", driver: "oracle", sql: "select cast(1 / :denominator as number) as value from dual", payload: `{"denominator":0}`, resultType: "integer"},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			db := selectedContainerDBConfig(t, tc.driver)
			secrets := newITSecrets(t)
			addr := freeAddr(t)
			tmp := t.TempDir()
			capabilityFile := writeContainerErrorCapability(t, tmp, tc.driver, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, tc.sql, tc.resultType)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			baseURL := startInternalGateway(t, ctx, addr, secrets, 2*time.Second)
			runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
			if err != nil {
				t.Fatal(err)
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runner.Run(ctx) }()
			waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

			status, body := postCapability(t, baseURL, secrets.APIKey, "driver_error", tc.payload)
			if status != http.StatusBadGateway {
				t.Fatalf("driver_error status=%d body=%s", status, string(body))
			}
			requireAPIErrorCode(t, body, "AGENT_QUERY_FAILED")
			if strings.Contains(strings.ToLower(string(body)), "json") ||
				strings.Contains(strings.ToLower(string(body)), "divide") ||
				strings.Contains(strings.ToLower(string(body)), "ora-") ||
				strings.Contains(strings.ToLower(string(body)), "sql") {
				t.Fatalf("driver-specific detail leaked in gateway response: %s", string(body))
			}
			cancel()
			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Fatal("agent runner did not stop")
			}
		})
	}
}

func TestContainerDBDriverTimeoutsAreNormalized(t *testing.T) {
	for _, tc := range []struct {
		name               string
		driver             string
		sql                string
		policyTimeout      time.Duration
		blockRuntimeSelect bool
	}{
		{name: "postgres", driver: "postgres", sql: "select 'done'::text as slept from pg_sleep(1)", policyTimeout: 100 * time.Millisecond},
		{name: "mysql", driver: "mysql", sql: "select sleep(1) as slept", policyTimeout: 100 * time.Millisecond},
		{name: "sqlserver", driver: "sqlserver", sql: "select cast(value as bigint) as slept from dbo.onprest_it_timeout where id = 1", policyTimeout: 500 * time.Millisecond, blockRuntimeSelect: true},
		{name: "oracle", driver: "oracle", sql: `select count(*) as "slept" from dual connect by level <= 100000000`, policyTimeout: 100 * time.Millisecond},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			db := selectedContainerDBConfig(t, tc.driver)
			if tc.blockRuntimeSelect {
				execDBStatements(t, tc.driver, db, []string{
					"if object_id('dbo.onprest_it_timeout', 'U') is not null drop table dbo.onprest_it_timeout",
					"create table dbo.onprest_it_timeout (id int primary key, value bigint not null)",
					"insert into dbo.onprest_it_timeout (id, value) values (1, 1)",
				})
			}
			secrets := newITSecrets(t)
			addr := freeAddr(t)
			tmp := t.TempDir()
			capabilityFile := writeContainerTimeoutCapability(t, tmp, tc.driver, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, tc.sql, tc.policyTimeout)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			baseURL := startInternalGateway(t, ctx, addr, secrets, 1500*time.Millisecond)
			runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
			if err != nil {
				t.Fatal(err)
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runner.Run(ctx) }()
			waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)
			if tc.blockRuntimeSelect {
				holdSQLServerTimeoutRowLock(t, db)
			}

			started := time.Now()
			status, body := postCapability(t, baseURL, secrets.APIKey, "slow_query", `{}`)
			elapsed := time.Since(started)
			if status != http.StatusGatewayTimeout {
				t.Fatalf("slow_query status=%d want %d; body=%s", status, http.StatusGatewayTimeout, string(body))
			}
			requireAPIErrorCode(t, body, "AGENT_QUERY_TIMEOUT")
			if tc.blockRuntimeSelect && elapsed+25*time.Millisecond < tc.policyTimeout {
				t.Fatalf("slow_query returned before policy timeout: elapsed=%s timeout=%s", elapsed, tc.policyTimeout)
			}
			if strings.Contains(strings.ToLower(string(body)), "sleep") ||
				strings.Contains(strings.ToLower(string(body)), "waitfor") ||
				strings.Contains(strings.ToLower(string(body)), "ora-") ||
				strings.Contains(strings.ToLower(string(body)), "timeout") && strings.Contains(strings.ToLower(string(body)), "context") {
				t.Fatalf("driver-specific timeout detail leaked in gateway response: %s", string(body))
			}

			cancel()
			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Fatal("agent runner did not stop")
			}
		})
	}
}

func holdSQLServerTimeoutRowLock(t *testing.T, cfg postgresConfig) {
	t.Helper()
	db, err := sql.Open(sqlDriverName("sqlserver"), integrationDBDSN("sqlserver", cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := tx.ExecContext(ctx, "update dbo.onprest_it_timeout set value = value + 1 where id = 1"); err != nil {
		t.Fatalf("hold SQL Server timeout row lock: %v", err)
	}
}

func TestContainerDBDriverPermissionErrorsAreHiddenFromGatewayResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		driver string
	}{
		{name: "postgres", driver: "postgres"},
		{name: "mysql", driver: "mysql"},
		{name: "sqlserver", driver: "sqlserver"},
		{name: "oracle", driver: "oracle"},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			admin := selectedContainerDBConfig(t, tc.driver)
			restricted, query, revoke := preparePermissionRevocationScenario(t, tc.driver, admin)
			secrets := newITSecrets(t)
			addr := freeAddr(t)
			tmp := t.TempDir()
			agentBin := filepath.Join(tmp, "onprest-agent")
			buildBinary(t, repoRoot(t), agentBin, "./cmd/agent")
			capabilityFile := writeContainerPermissionCapability(t, tmp, tc.driver, restricted, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, query)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			baseURL := startInternalGateway(t, ctx, addr, secrets, 2*time.Second)
			cmd, output := startProcessWithOutput(t, tmp, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + capabilityFile})
			defer stopProcess(t, cmd)
			waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

			status, body := postCapability(t, baseURL, secrets.APIKey, "permission_probe", `{}`)
			if status != http.StatusOK || string(body) != `{"count":1}` {
				t.Fatalf("permission_probe before revoke status=%d body=%s output=%s", status, string(body), output.String())
			}
			revoke()
			status, body = postCapability(t, baseURL, secrets.APIKey, "permission_probe", `{}`)
			if status != http.StatusBadGateway {
				t.Fatalf("permission_probe after revoke status=%d body=%s", status, string(body))
			}
			requireAPIErrorCode(t, body, "AGENT_QUERY_FAILED")
			lowerBody := strings.ToLower(string(body))
			for _, leaked := range []string{"permission", "denied", "select", "ora-", "sqlstate", restricted.User, restricted.Password} {
				if leaked != "" && strings.Contains(lowerBody, strings.ToLower(leaked)) {
					t.Fatalf("permission detail leaked in gateway response for %s: %s", tc.driver, string(body))
				}
			}
			logs, err := os.ReadFile(agentBin + ".log")
			if err != nil {
				t.Fatalf("read agent detail log: %v; output=%s", err, output.String())
			}
			lowerLog := bytes.ToLower(logs)
			if !bytes.Contains(logs, []byte("AGENT_QUERY_FAILED")) ||
				!(bytes.Contains(lowerLog, []byte("permission")) ||
					bytes.Contains(lowerLog, []byte("denied")) ||
					bytes.Contains(lowerLog, []byte("ora-"))) {
				t.Fatalf("agent detail log missing DB permission detail for %s: %s", tc.driver, string(logs))
			}
			if bytes.Contains(logs, []byte(restricted.Password)) {
				t.Fatalf("agent detail log leaked DB password for %s: %s", tc.driver, string(logs))
			}
		})
	}
}

func TestContainerDBDriverStartupDMLExplainPermissionErrorsArePrivate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		driver string
	}{
		{name: "postgres", driver: "postgres"},
		{name: "mysql", driver: "mysql"},
		{name: "sqlserver", driver: "sqlserver"},
		{name: "oracle", driver: "oracle"},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			admin := selectedContainerDBConfig(t, tc.driver)
			restricted, query, revoke := preparePermissionRevocationScenario(t, tc.driver, admin)
			revoke()
			if got := permissionProbeValue(t, tc.driver, admin); got != 1 {
				t.Fatalf("protected value before startup=%d, want 1", got)
			}

			secrets := newITSecrets(t)
			addr := freeAddr(t)
			tmp := t.TempDir()
			agentBin := filepath.Join(tmp, "onprest-agent")
			buildBinary(t, repoRoot(t), agentBin, "./cmd/agent")
			capabilityFile := writeContainerPermissionCapability(t, tmp, tc.driver, restricted, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, query)
			validate := exec.Command(agentBin, "validate", "--config", capabilityFile, "--format", "json")
			validate.Dir = tmp
			validateOutput, validateErr := validate.CombinedOutput()
			if exitCodeOf(validateErr) != 1 || !bytes.Contains(validateOutput, []byte(`"stage":"capability_explain"`)) {
				t.Fatalf("%s validate permission failure exit=%d output=%s", tc.driver, exitCodeOf(validateErr), validateOutput)
			}
			for _, leaked := range []string{query, restricted.User, restricted.Password} {
				if leaked != "" && bytes.Contains(bytes.ToLower(validateOutput), bytes.ToLower([]byte(leaked))) {
					t.Fatalf("validate output leaked %s detail %q: %s", tc.driver, leaked, validateOutput)
				}
			}
			validateLog, err := os.ReadFile(filepath.Join(tmp, "onprest-agent.validate.log"))
			if err != nil || bytes.Contains(validateLog, []byte(restricted.Password)) {
				t.Fatalf("validate detail log error=%v content=%s", err, validateLog)
			}
			if _, err := os.Stat(agentBin + ".log"); !os.IsNotExist(err) {
				t.Fatalf("validate touched runtime log: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			baseURL := startInternalGateway(t, ctx, addr, secrets, 2*time.Second)
			cmd, output := startProcessWithOutput(t, tmp, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + capabilityFile})
			defer stopProcess(t, cmd)
			if err := waitForExit(t, cmd, 15*time.Second); err == nil {
				t.Fatalf("%s agent connected despite startup DML EXPLAIN permission denial", tc.driver)
			}

			healthResponse, err := http.Get(baseURL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			healthBody, err := io.ReadAll(healthResponse.Body)
			_ = healthResponse.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if healthResponse.StatusCode != http.StatusOK || !bytes.Contains(healthBody, []byte(`"agent_connected":false`)) {
				t.Fatalf("health status=%d body=%s", healthResponse.StatusCode, healthBody)
			}
			status, publicBody := postCapability(t, baseURL, secrets.APIKey, "permission_probe", `{}`)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("offline capability status=%d body=%s", status, publicBody)
			}
			requireAPIErrorCode(t, publicBody, "GATEWAY_AGENT_OFFLINE")
			if got := permissionProbeValue(t, tc.driver, admin); got != 1 {
				t.Fatalf("startup EXPLAIN changed protected value to %d", got)
			}

			logs, err := os.ReadFile(agentBin + ".log")
			if err != nil {
				t.Fatalf("read startup detail log: %v; output=%s", err, output.String())
			}
			lowerLog := bytes.ToLower(logs)
			if !bytes.Contains(logs, []byte("AGENT_STARTUP_FAILED")) ||
				!(bytes.Contains(lowerLog, []byte("permission")) || bytes.Contains(lowerLog, []byte("denied")) || bytes.Contains(lowerLog, []byte("ora-"))) {
				t.Fatalf("startup detail log missing DML EXPLAIN permission detail for %s: %s", tc.driver, logs)
			}
			for surface, value := range map[string]string{"stdout": output.String(), "public response": string(publicBody)} {
				lower := strings.ToLower(value)
				for _, leaked := range []string{"permission", "denied", "ora-", "sqlstate", query, restricted.User, restricted.Password} {
					if leaked != "" && strings.Contains(lower, strings.ToLower(leaked)) {
						t.Fatalf("%s leaked startup detail/credential for %s: %s", surface, tc.driver, value)
					}
				}
			}
			if bytes.Contains(logs, []byte(restricted.Password)) {
				t.Fatalf("agent detail log leaked DB password for %s: %s", tc.driver, logs)
			}
		})
	}
}

func TestContainerDBDriverStartupSyntaxErrorsAreLoggedLocally(t *testing.T) {
	for _, tc := range []struct {
		name   string
		driver string
		sql    string
	}{
		{name: "postgres", driver: "postgres", sql: "select from"},
		{name: "mysql", driver: "mysql", sql: "select from"},
		{name: "sqlserver", driver: "sqlserver", sql: "select from"},
		{name: "oracle", driver: "oracle", sql: "select from"},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			db := selectedContainerDBConfig(t, tc.driver)
			secrets := newITSecrets(t)
			tmp := t.TempDir()
			agentBin := filepath.Join(tmp, "onprest-agent")
			buildBinary(t, repoRoot(t), agentBin, "./cmd/agent")
			capabilityFile := writeContainerStartupCapability(t, tmp, tc.driver, db, "ws://127.0.0.1:1/ws/agent", secrets.AgentPrivateKey, tc.sql)

			cmd, output := startProcessWithOutput(t, tmp, agentBin, nil, []string{"AGENT_CAPABILITY_FILE=" + capabilityFile})
			if err := waitForExit(t, cmd, 10*time.Second); err == nil {
				t.Fatalf("%s agent started with invalid SQL syntax", tc.driver)
			}
			logs, err := os.ReadFile(agentBin + ".log")
			if err != nil {
				t.Fatalf("read startup detail log: %v; output=%s", err, output.String())
			}
			if !bytes.Contains(logs, []byte("AGENT_STARTUP_FAILED")) {
				t.Fatalf("startup detail log missing syntax failure for %s: %s", tc.driver, string(logs))
			}
			if strings.Contains(output.String(), tc.sql) || strings.Contains(output.String(), db.Password) {
				t.Fatalf("startup output leaked syntax detail for %s: %s", tc.driver, output.String())
			}
		})
	}
}

func TestContainerDBDriverPolicySemantics(t *testing.T) {
	for _, tc := range []struct {
		name     string
		driver   string
		rowsSQL  string
		largeSQL string
	}{
		{
			name:     "postgres",
			driver:   "postgres",
			rowsSQL:  "select n::int as n from generate_series(1, 3) as n",
			largeSQL: "select repeat('x', 2048)::text as payload",
		},
		{
			name:     "mysql",
			driver:   "mysql",
			rowsSQL:  "select 1 as n union all select 2 union all select 3",
			largeSQL: "select repeat('x', 2048) as payload",
		},
		{
			name:     "sqlserver",
			driver:   "sqlserver",
			rowsSQL:  "select 1 as n union all select 2 union all select 3",
			largeSQL: "select replicate(cast('x' as varchar(max)), 2048) as payload",
		},
		{
			name:     "oracle",
			driver:   "oracle",
			rowsSQL:  `select level as "n" from dual connect by level <= 3`,
			largeSQL: `select rpad('x', 2048, 'x') as "payload" from dual`,
		},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			db := selectedContainerDBConfig(t, tc.driver)
			secrets := newITSecrets(t)
			addr := freeAddr(t)
			tmp := t.TempDir()
			capabilityFile := writeContainerPolicyCapability(t, tmp, tc.driver, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, tc.rowsSQL, tc.largeSQL)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			baseURL := startInternalGateway(t, ctx, addr, secrets, 2*time.Second)
			runner, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
			if err != nil {
				t.Fatal(err)
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runner.Run(ctx) }()
			waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

			status, body := postCapability(t, baseURL, secrets.APIKey, "limited_rows", `{}`)
			if status != http.StatusOK {
				t.Fatalf("limited_rows status=%d body=%s", status, string(body))
			}
			if !strings.Contains(string(body), `"count":2`) ||
				!strings.Contains(string(body), `"n":1`) ||
				!strings.Contains(string(body), `"n":2`) {
				t.Fatalf("limited_rows did not enforce max_rows across %s: %s", tc.driver, string(body))
			}

			status, body = postCapability(t, baseURL, secrets.APIKey, "too_large", `{}`)
			if status != http.StatusBadGateway {
				t.Fatalf("too_large status=%d want %d; body=%s", status, http.StatusBadGateway, string(body))
			}
			requireAPIErrorCode(t, body, "AGENT_QUERY_FAILED")

			cancel()
			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Fatal("agent runner did not stop")
			}
		})
	}
}

func TestContainerDBDriverStartupRejectsUnreachableDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		driver string
		sql    string
	}{
		{name: "postgres", driver: "postgres", sql: "select 1::int as id"},
		{name: "mysql", driver: "mysql", sql: "select 1 as id"},
		{name: "sqlserver", driver: "sqlserver", sql: "select cast(1 as int) as id"},
		{name: "oracle", driver: "oracle", sql: `select cast(1 as number) as "id" from dual`},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			port, err := freeHostPort()
			if err != nil {
				t.Fatal(err)
			}
			tmp := t.TempDir()
			secrets := newITSecrets(t)
			db := postgresConfig{
				Host:     "127.0.0.1",
				Port:     port,
				Name:     itDBName,
				User:     itDBUser,
				Password: itDBPassword,
			}
			capabilityFile := writeContainerStartupCapability(t, tmp, tc.driver, db, "ws://127.0.0.1:1/ws/agent", secrets.AgentPrivateKey, tc.sql)
			if _, err := agentpkg.NewRunner(context.Background(), agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil); err == nil {
				t.Fatalf("%s agent runner started against an unreachable DB, want startup failure", tc.driver)
			}
		})
	}
}

func preparePermissionRevocationScenario(t *testing.T, driver string, admin postgresConfig) (postgresConfig, string, func()) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	password := "Strong1_" + suffix
	switch driver {
	case "postgres":
		user := "onprest_denied_" + suffix
		execDBStatements(t, driver, admin, []string{
			"drop table if exists public.onprest_it_permission",
			"create table public.onprest_it_permission (id integer primary key, value integer not null)",
			"insert into public.onprest_it_permission (id, value) values (1, 1)",
			"create user " + user + " with password '" + password + "'",
			"grant connect on database " + itDBName + " to " + user,
			"grant usage on schema public to " + user,
			"grant select, update on table public.onprest_it_permission to " + user,
		})
		restricted := admin
		restricted.User = user
		restricted.Password = password
		return restricted, "update public.onprest_it_permission set value = value + 1 where id = 1", func() {
			execDBStatements(t, driver, admin, []string{"revoke update on table public.onprest_it_permission from " + user})
		}
	case "mysql":
		user := "opden" + suffix
		root := admin
		root.User = "root"
		execDBStatements(t, driver, root, []string{
			"drop table if exists onprest_it_permission",
			"create table onprest_it_permission (id int primary key, value int not null)",
			"insert into onprest_it_permission (id, value) values (1, 1)",
			"drop user if exists '" + user + "'@'%'",
			"create user '" + user + "'@'%' identified by '" + password + "'",
			"grant select, update on " + itDBName + ".onprest_it_permission to '" + user + "'@'%'",
		})
		restricted := admin
		restricted.User = user
		restricted.Password = password
		return restricted, "update onprest_it_permission set value = value + 1 where id = 1", func() {
			execDBStatements(t, driver, root, []string{"revoke update on " + itDBName + ".onprest_it_permission from '" + user + "'@'%'"})
		}
	case "sqlserver":
		user := "opden" + suffix
		execDBStatements(t, driver, admin, []string{
			"if object_id('dbo.onprest_it_permission', 'U') is not null drop table dbo.onprest_it_permission",
			"create table dbo.onprest_it_permission (id int primary key, value int not null)",
			"insert into dbo.onprest_it_permission (id, value) values (1, 1)",
			"if exists (select 1 from sys.database_principals where name = N'" + user + "') drop user [" + user + "]",
			"if exists (select 1 from sys.server_principals where name = N'" + user + "') drop login [" + user + "]",
			"create login [" + user + "] with password = N'" + password + "', check_policy = off",
			"create user [" + user + "] for login [" + user + "]",
			"grant showplan to [" + user + "]",
			"grant select, update on object::dbo.onprest_it_permission to [" + user + "]",
		})
		restricted := admin
		restricted.User = user
		restricted.Password = password
		return restricted, "update dbo.onprest_it_permission set value = value + 1 where id = 1", func() {
			execDBStatements(t, driver, admin, []string{"revoke update on object::dbo.onprest_it_permission from [" + user + "]"})
		}
	case "oracle":
		user := "OPDEN" + suffix
		system := admin
		system.User = "system"
		system.Password = itDBPassword
		execDBStatements(t, driver, system, []string{
			"begin execute immediate 'drop user " + user + " cascade'; exception when others then if sqlcode != -1918 then raise; end if; end;",
			"begin execute immediate 'drop table onprest_it_permission purge'; exception when others then if sqlcode != -942 then raise; end if; end;",
			"create table onprest_it_permission (id number(10) primary key, value number(10) not null)",
			"insert into onprest_it_permission (id, value) values (1, 1)",
			"create user " + user + " identified by \"" + password + "\"",
			"grant create session to " + user,
			"grant select, update on system.onprest_it_permission to " + user,
		})
		restricted := admin
		restricted.User = user
		restricted.Password = password
		return restricted, `update system.onprest_it_permission set value = value + 1 where id = 1`, func() {
			execDBStatements(t, driver, system, []string{"revoke update on system.onprest_it_permission from " + user})
		}
	default:
		t.Fatalf("unsupported permission scenario driver %q", driver)
		return postgresConfig{}, "", nil
	}
}

func permissionProbeValue(t *testing.T, driver string, admin postgresConfig) int {
	t.Helper()
	query := "select value from onprest_it_permission where id = 1"
	elevated := admin
	switch driver {
	case "postgres":
		query = "select value from public.onprest_it_permission where id = 1"
	case "mysql":
		elevated.User = "root"
	case "sqlserver":
		query = "select value from dbo.onprest_it_permission where id = 1"
	case "oracle":
		elevated.User = "system"
		elevated.Password = itDBPassword
		query = "select value from system.onprest_it_permission where id = 1"
	}
	db, err := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, elevated))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var value int
	if err := db.QueryRowContext(ctx, query).Scan(&value); err != nil {
		t.Fatalf("read protected permission probe value for %s: %v", driver, err)
	}
	return value
}

func execDBStatements(t *testing.T, driver string, cfg postgresConfig, statements []string) {
	t.Helper()
	db, err := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s statement failed: %v\n%s", driver, err, stmt)
		}
	}
}

func writeContainerCapability(t *testing.T, dir, driver string, db postgresConfig, gatewayURL, agentPrivateKey, sql string) string {
	t.Helper()
	content := fmt.Sprintf(`service:
  title: Onprest Container DB IT
  version: 0.1.0
database:
  driver: %s
  host: %s
  port: %s
  name: %s
  user: %s
  password: %s
gateway:
  url: %s
  agent_private_key: %s
logging:
  max_size: 10MB
  max_files: 3
capabilities:
  get_customer:
    sql: %s
    params:
      id:
        type: integer
        required: true
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer
      name:
        type: string
      email:
        type: string
`, driver, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), yamlString(sql))
	return writeFile(t, dir+"/capability."+driver+".yaml", content)
}

func writeContainerErrorCapability(t *testing.T, dir, driver string, db postgresConfig, gatewayURL, agentPrivateKey, sql, resultType string) string {
	t.Helper()
	paramBlock := `      denominator:
        type: integer
        required: true`
	if driver == "mysql" {
		paramBlock = `      payload:
        type: string
        required: true`
	}
	content := fmt.Sprintf(`service:
  title: Onprest Container DB Error IT
  version: 0.1.0
database:
  driver: %s
  host: %s
  port: %s
  name: %s
  user: %s
  password: %s
gateway:
  url: %s
  agent_private_key: %s
logging:
  max_size: 10MB
  max_files: 3
capabilities:
  driver_error:
    sql: %s
    params:
%s
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      value:
        type: %s
`, driver, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), yamlString(sql), paramBlock, resultType)
	return writeFile(t, dir+"/capability."+driver+".error.yaml", content)
}

func writeContainerPolicyCapability(t *testing.T, dir, driver string, db postgresConfig, gatewayURL, agentPrivateKey, rowsSQL, largeSQL string) string {
	t.Helper()
	content := fmt.Sprintf(`service:
  title: Onprest Container DB Policy IT
  version: 0.1.0
database:
  driver: %s
  host: %s
  port: %s
  name: %s
  user: %s
  password: %s
gateway:
  url: %s
  agent_private_key: %s
logging:
  max_size: 10MB
  max_files: 3
capabilities:
  limited_rows:
    sql: %s
    policy:
      readonly: true
      timeout: 2s
      max_rows: 2
      max_bytes: 128KB
    result:
      n:
        type: integer
  too_large:
    sql: %s
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 1KB
    result:
      payload:
        type: string
`, driver, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), yamlString(rowsSQL), yamlString(largeSQL))
	return writeFile(t, dir+"/capability."+driver+".policy.yaml", content)
}

func writeContainerStartupCapability(t *testing.T, dir, driver string, db postgresConfig, gatewayURL, agentPrivateKey, sql string) string {
	t.Helper()
	content := fmt.Sprintf(`service:
  title: Onprest Container DB Startup IT
  version: 0.1.0
database:
  driver: %s
  host: %s
  port: %s
  name: %s
  user: %s
  password: %s
gateway:
  url: %s
  agent_private_key: %s
logging:
  max_size: 10MB
  max_files: 3
capabilities:
  startup_probe:
    sql: %s
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer
`, driver, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), yamlString(sql))
	return writeFile(t, dir+"/capability."+driver+".startup.yaml", content)
}

func writeContainerWritableCapability(t *testing.T, dir, driver string, db postgresConfig, gatewayURL, agentPrivateKey, statement string) string {
	t.Helper()
	content := fmt.Sprintf(`service:
  title: Onprest Writable Protection IT
  version: 0.1.0
database:
  driver: %s
  host: %s
  port: %s
  name: %s
  user: %s
  password: %s
gateway:
  url: %s
  agent_private_key: %s
capabilities:
  attempted_multiple_mutation:
    sql: %s
    policy:
      readonly: false
      timeout: 2s
      max_bytes: 128KB
`, driver, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), yamlString(statement))
	return writeFile(t, filepath.Join(dir, "capability."+driver+".writable-protection.yaml"), content)
}

func writeContainerPermissionCapability(t *testing.T, dir, driver string, db postgresConfig, gatewayURL, agentPrivateKey, sql string) string {
	t.Helper()
	content := fmt.Sprintf(`service:
  title: Onprest Container DB Permission IT
  version: 0.1.0
database:
  driver: %s
  host: %s
  port: %s
  name: %s
  user: %s
  password: %s
gateway:
  url: %s
  agent_private_key: %s
logging:
  max_size: 10MB
  max_files: 3
capabilities:
  permission_probe:
    sql: %s
    policy:
      readonly: false
      timeout: 2s
      max_bytes: 128KB
`, driver, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), yamlString(sql))
	return writeFile(t, dir+"/capability."+driver+".permission.yaml", content)
}

func writeContainerTimeoutCapability(t *testing.T, dir, driver string, db postgresConfig, gatewayURL, agentPrivateKey, sql string, policyTimeout time.Duration) string {
	t.Helper()
	content := fmt.Sprintf(`service:
  title: Onprest Container DB Timeout IT
  version: 0.1.0
database:
  driver: %s
  host: %s
  port: %s
  name: %s
  user: %s
  password: %s
gateway:
  url: %s
  agent_private_key: %s
logging:
  max_size: 10MB
  max_files: 3
capabilities:
  slow_query:
    sql: %s
    policy:
      readonly: false
      timeout: %s
      max_rows: 1
      max_bytes: 128KB
    result:
      slept:
        type: integer
`, driver, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), yamlString(sql), policyTimeout.String())
	return writeFile(t, dir+"/capability."+driver+".timeout.yaml", content)
}
