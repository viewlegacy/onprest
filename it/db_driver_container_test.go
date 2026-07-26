//go:build integration

package it

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
)

func TestContainerDBDriverSmoke(t *testing.T) {
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

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			baseURL := startInternalGateway(t, ctx, addr, secrets, 2*time.Second)
			runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
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
			runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
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
	runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
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
			if _, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: path, ReconnectEvery: time.Second}, nil); err == nil {
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
			runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
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
		name   string
		driver string
		sql    string
	}{
		{name: "postgres", driver: "postgres", sql: "select 'done'::text as slept from pg_sleep(1)"},
		{name: "mysql", driver: "mysql", sql: "select sleep(1) as slept"},
		{name: "sqlserver", driver: "sqlserver", sql: "WAITFOR DELAY '00:00:01'; SELECT cast(1 as int) as slept"},
		{name: "oracle", driver: "oracle", sql: `select count(*) as "slept" from dual connect by level <= 100000000`},
	} {
		if !selectedDBForTest(t, tc.driver) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			db := selectedContainerDBConfig(t, tc.driver)
			secrets := newITSecrets(t)
			addr := freeAddr(t)
			tmp := t.TempDir()
			capabilityFile := writeContainerTimeoutCapability(t, tmp, tc.driver, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, tc.sql)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			baseURL := startInternalGateway(t, ctx, addr, secrets, 1500*time.Millisecond)
			runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
			if err != nil {
				t.Fatal(err)
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runner.Run(ctx) }()
			waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

			status, body := postCapability(t, baseURL, secrets.APIKey, "slow_query", `{}`)
			if status != http.StatusGatewayTimeout {
				t.Fatalf("slow_query status=%d want %d; body=%s", status, http.StatusGatewayTimeout, string(body))
			}
			requireAPIErrorCode(t, body, "AGENT_QUERY_TIMEOUT")
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
			if status != http.StatusOK || !strings.Contains(string(body), `"id":1`) {
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
			runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
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
			if _, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil); err == nil {
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
			"create table public.onprest_it_permission (id integer primary key)",
			"insert into public.onprest_it_permission (id) values (1)",
			"create user " + user + " with password '" + password + "'",
			"grant connect on database " + itDBName + " to " + user,
			"grant usage on schema public to " + user,
			"grant select on table public.onprest_it_permission to " + user,
		})
		restricted := admin
		restricted.User = user
		restricted.Password = password
		return restricted, "select id from public.onprest_it_permission", func() {
			execDBStatements(t, driver, admin, []string{"revoke select on table public.onprest_it_permission from " + user})
		}
	case "mysql":
		user := "opden" + suffix
		root := admin
		root.User = "root"
		execDBStatements(t, driver, root, []string{
			"drop table if exists onprest_it_permission",
			"create table onprest_it_permission (id int primary key)",
			"insert into onprest_it_permission (id) values (1)",
			"drop user if exists '" + user + "'@'%'",
			"create user '" + user + "'@'%' identified by '" + password + "'",
			"grant select on " + itDBName + ".onprest_it_permission to '" + user + "'@'%'",
		})
		restricted := admin
		restricted.User = user
		restricted.Password = password
		return restricted, "select id from onprest_it_permission", func() {
			execDBStatements(t, driver, root, []string{"revoke select on " + itDBName + ".onprest_it_permission from '" + user + "'@'%'"})
		}
	case "sqlserver":
		user := "opden" + suffix
		execDBStatements(t, driver, admin, []string{
			"if object_id('dbo.onprest_it_permission', 'U') is not null drop table dbo.onprest_it_permission",
			"create table dbo.onprest_it_permission (id int primary key)",
			"insert into dbo.onprest_it_permission (id) values (1)",
			"if exists (select 1 from sys.database_principals where name = N'" + user + "') drop user [" + user + "]",
			"if exists (select 1 from sys.server_principals where name = N'" + user + "') drop login [" + user + "]",
			"create login [" + user + "] with password = N'" + password + "', check_policy = off",
			"create user [" + user + "] for login [" + user + "]",
			"grant showplan to [" + user + "]",
			"grant select on object::dbo.onprest_it_permission to [" + user + "]",
		})
		restricted := admin
		restricted.User = user
		restricted.Password = password
		return restricted, "select id from dbo.onprest_it_permission", func() {
			execDBStatements(t, driver, admin, []string{"revoke select on object::dbo.onprest_it_permission from [" + user + "]"})
		}
	case "oracle":
		user := "OPDEN" + suffix
		system := admin
		system.User = "system"
		system.Password = itDBPassword
		execDBStatements(t, driver, system, []string{
			"begin execute immediate 'drop user " + user + " cascade'; exception when others then if sqlcode != -1918 then raise; end if; end;",
			"begin execute immediate 'drop table onprest_it_permission purge'; exception when others then if sqlcode != -942 then raise; end if; end;",
			"create table onprest_it_permission (id number(10) primary key)",
			"insert into onprest_it_permission (id) values (1)",
			"create user " + user + " identified by \"" + password + "\"",
			"grant create session to " + user,
			"grant select on system.onprest_it_permission to " + user,
		})
		restricted := admin
		restricted.User = user
		restricted.Password = password
		return restricted, `select id as "id" from system.onprest_it_permission`, func() {
			execDBStatements(t, driver, system, []string{"revoke select on system.onprest_it_permission from " + user})
		}
	default:
		t.Fatalf("unsupported permission scenario driver %q", driver)
		return postgresConfig{}, "", nil
	}
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
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer
`, driver, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), yamlString(sql))
	return writeFile(t, dir+"/capability."+driver+".permission.yaml", content)
}

func writeContainerTimeoutCapability(t *testing.T, dir, driver string, db postgresConfig, gatewayURL, agentPrivateKey, sql string) string {
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
      timeout: 100ms
      max_rows: 1
      max_bytes: 128KB
    result:
      slept:
        type: integer
`, driver, yamlString(db.Host), db.Port, yamlString(db.Name), yamlString(db.User), yamlString(db.Password), yamlString(gatewayURL), yamlString(agentPrivateKey), yamlString(sql))
	return writeFile(t, dir+"/capability."+driver+".timeout.yaml", content)
}
