//go:build integration

package it

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
	"github.com/viewlegacy/onprest/internal/gateway"
)

func TestPostgresPolicyAndQueryFailures(t *testing.T) {
	db := postgresContainerConfig(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	tmp := t.TempDir()
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, `  limited_rows:
    sql: select n::int as n from generate_series(1,5) as n
    policy:
      readonly: true
      timeout: 2s
      max_rows: 2
      max_bytes: 128KB
    result:
      n:
        type: integer
  slow_query:
    sql: select 'done'::text as slept from pg_sleep(1)
    policy:
      readonly: true
      timeout: 200ms
      max_rows: 1
      max_bytes: 128KB
    result:
      slept:
        type: string
  failing_query:
    sql: select (1 / :denominator::int)::int as value
    params:
      denominator:
        type: integer
        required: true
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      value:
        type: integer
  too_large:
    sql: select repeat('x', 2048)::text as payload
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 1KB
    result:
      payload:
        type: string`)

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

	status, body := postCapability(t, baseURL, secrets.APIKey, "limited_rows", `{}`)
	if status != http.StatusOK {
		t.Fatalf("limited_rows status=%d body=%s", status, string(body))
	}
	var limited struct {
		Rows  []map[string]any `json:"rows"`
		Count int              `json:"count"`
	}
	if err := json.Unmarshal(body, &limited); err != nil {
		t.Fatal(err)
	}
	if limited.Count != 2 || len(limited.Rows) != 2 {
		t.Fatalf("limited_rows response = %s, want 2 rows", string(body))
	}

	status, body = postCapability(t, baseURL, secrets.APIKey, "slow_query", `{}`)
	if status != http.StatusGatewayTimeout {
		t.Fatalf("slow_query status=%d want %d; body=%s", status, http.StatusGatewayTimeout, string(body))
	}
	requireAPIErrorCode(t, body, "AGENT_QUERY_TIMEOUT")

	status, body = postCapability(t, baseURL, secrets.APIKey, "failing_query", `{"denominator":0}`)
	if status != http.StatusBadGateway {
		t.Fatalf("failing_query status=%d want %d; body=%s", status, http.StatusBadGateway, string(body))
	}
	requireAPIErrorCode(t, body, "AGENT_QUERY_FAILED")

	status, body = postCapability(t, baseURL, secrets.APIKey, "too_large", `{}`)
	if status != http.StatusBadGateway {
		t.Fatalf("too_large status=%d want %d; body=%s", status, http.StatusBadGateway, string(body))
	}
	requireAPIErrorCode(t, body, "AGENT_QUERY_FAILED")

	cancel()
	select {
	case <-errCh:
	case <-time.After(12 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}

func TestReadonlyMultiStatementIsRejectedBeforeRealPostgresExecution(t *testing.T) {
	dbConfig := postgresContainerConfig(t)
	db, err := sql.Open("postgres", postgresDSN(dbConfig))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `create table if not exists onprest_readonly_guard (id integer primary key, value integer not null)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into onprest_readonly_guard (id, value) values (1, 7) on conflict (id) do update set value = 7`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = db.ExecContext(cleanupCtx, `drop table if exists onprest_readonly_guard`)
	})

	secrets := newITSecrets(t)
	capabilityFile := writePostgresCapability(t, t.TempDir(), dbConfig, "ws://127.0.0.1:1/ws/agent", secrets.AgentPrivateKey, `  attempted_write:
    sql: "select value from onprest_readonly_guard where id = 1; update onprest_readonly_guard set value = 99 where id = 1"
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      value:
        type: integer`)
	if _, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile}, nil); err == nil {
		t.Fatal("NewRunner accepted readonly multi-statement SQL")
	}
	var value int
	if err := db.QueryRowContext(ctx, `select value from onprest_readonly_guard where id = 1`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 7 {
		t.Fatalf("readonly rejected statement changed real DB value to %d", value)
	}
}

func TestAgentExecutesIndependentRequestsConcurrentlyAgainstPostgres(t *testing.T) {
	db := postgresContainerConfig(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	capabilityFile := writePostgresCapability(t, t.TempDir(), db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, `  blocking_query:
    sql: /* onprest_it_parallel_block */ select 1::int as id from pg_sleep(1)
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer
  fast_query:
    sql: select 2::int as id
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer`)
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
	type result struct {
		status int
		body   []byte
	}
	blocking := make(chan result, 1)
	go func() {
		status, body := postCapability(t, baseURL, secrets.APIKey, "blocking_query", `{}`)
		blocking <- result{status: status, body: body}
	}()
	waitForPostgresActiveQuery(t, db, "onprest_it_parallel_block")
	start := time.Now()
	status, body := postCapability(t, baseURL, secrets.APIKey, "fast_query", `{}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"id":2`) {
		t.Fatalf("fast query status=%d body=%s", status, body)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("fast query was head-of-line blocked for %s", elapsed)
	}
	select {
	case slow := <-blocking:
		if slow.status != http.StatusOK || !strings.Contains(string(slow.body), `"id":1`) {
			t.Fatalf("blocking query status=%d body=%s", slow.status, slow.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocking query did not finish")
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}

func TestAgentYAMLMaxConcurrentRequestsSerializesRealPostgresExecutions(t *testing.T) {
	db := postgresContainerConfig(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	capabilityFile := writePostgresCapabilityWithRuntime(t, t.TempDir(), db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, 1, `  first_blocking_query:
    sql: /* onprest_it_concurrency_first */ select 1::int as id from pg_sleep(2)
    policy:
      readonly: true
      timeout: 4s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer
  second_blocking_query:
    sql: /* onprest_it_concurrency_second */ select 2::int as id from pg_sleep(1)
    policy:
      readonly: true
      timeout: 4s
      max_rows: 1
      max_bytes: 128KB
    result:
      id:
        type: integer`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGatewayWithConfig(t, ctx, addr, secrets, 5*time.Second, io.Discard, func(cfg *gateway.Config) {
		cfg.AgentPingInterval = 50 * time.Millisecond
		cfg.AgentPongTimeout = 50 * time.Millisecond
		cfg.AgentWriteTimeout = 100 * time.Millisecond
	})
	var agentLogs bytes.Buffer
	runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, &agentLogs)
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

	type result struct {
		status int
		body   []byte
		err    error
	}
	first := make(chan result, 1)
	second := make(chan result, 1)
	go func() {
		status, body, err := postCapabilityRequest(baseURL, secrets.APIKey, "first_blocking_query", `{}`)
		first <- result{status: status, body: body, err: err}
	}()
	waitForPostgresActiveQuery(t, db, "onprest_it_concurrency_first")
	go func() {
		status, body, err := postCapabilityRequest(baseURL, secrets.APIKey, "second_blocking_query", `{}`)
		second <- result{status: status, body: body, err: err}
	}()

	assertPostgresQueryRemainsInactive(t, db, "onprest_it_concurrency_second", 500*time.Millisecond)
	waitForHealthAgentState(t, baseURL, true)
	select {
	case got := <-second:
		t.Fatalf("second request completed before the configured single worker was released: status=%d body=%s err=%v", got.status, got.body, got.err)
	default:
	}

	busyStart := time.Now()
	busyStatus, busyBody := postCapability(t, baseURL, secrets.APIKey, "second_blocking_query", `{}`)
	if busyStatus != http.StatusServiceUnavailable {
		t.Fatalf("REST request beyond bounded queue status=%d body=%s", busyStatus, busyBody)
	}
	requireAPIErrorCode(t, busyBody, "AGENT_BUSY")
	if elapsed := time.Since(busyStart); elapsed > 750*time.Millisecond {
		t.Fatalf("REST bounded-queue rejection waited %s instead of failing promptly", elapsed)
	}

	mcpBusyStart := time.Now()
	mcpBusy := postMCPPayload(t, baseURL, secrets.APIKey, `{"jsonrpc":"2.0","id":"busy","method":"tools/call","params":{"name":"second_blocking_query","arguments":{}}}`)
	if !bytes.Contains(mcpBusy, []byte(`"isError":true`)) || !bytes.Contains(mcpBusy, []byte(`"code":"AGENT_BUSY"`)) {
		t.Fatalf("MCP request beyond bounded queue did not return an AGENT_BUSY tool result: %s", mcpBusy)
	}
	if elapsed := time.Since(mcpBusyStart); elapsed > 750*time.Millisecond {
		t.Fatalf("MCP bounded-queue rejection waited %s instead of failing promptly", elapsed)
	}

	for _, request := range []struct {
		name string
		ch   <-chan result
	}{{name: "first", ch: first}, {name: "second", ch: second}} {
		select {
		case got := <-request.ch:
			if got.err != nil || got.status != http.StatusOK {
				t.Fatalf("%s request status=%d body=%s err=%v", request.name, got.status, got.body, got.err)
			}
		case <-time.After(6 * time.Second):
			t.Fatalf("%s request did not finish", request.name)
		}
	}
	if connections := strings.Count(agentLogs.String(), `"event":"gateway_connected"`); connections != 1 {
		t.Fatalf("agent connection generation changed while workers were saturated: connections=%d logs=%s", connections, agentLogs.String())
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}

func TestPostgresDBUnreachableDuringQuery(t *testing.T) {
	db, stopDB := dedicatedPostgresContainer(t)
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	tmp := t.TempDir()
	capabilityFile := writePostgresCapability(t, tmp, db, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, `  interrupted_sleep:
    sql: /* onprest_it_interrupted_sleep */ select 'done'::text as slept from pg_sleep(10)
    policy:
      readonly: true
      timeout: 15s
      max_rows: 1
      max_bytes: 128KB
    result:
      slept:
        type: string`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := startInternalGateway(t, ctx, addr, secrets, 8*time.Second)
	runner, err := agentpkg.NewRunner(agentpkg.Config{CapabilityFile: capabilityFile, ReconnectEvery: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()
	waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

	type capabilityResult struct {
		status int
		body   []byte
	}
	resultCh := make(chan capabilityResult, 1)
	go func() {
		status, body := postCapability(t, baseURL, secrets.APIKey, "interrupted_sleep", `{}`)
		resultCh <- capabilityResult{status: status, body: body}
	}()
	waitForPostgresActiveQuery(t, db, "onprest_it_interrupted_sleep")
	stopDB()

	var result capabilityResult
	select {
	case result = <-resultCh:
	case <-time.After(12 * time.Second):
		t.Fatal("interrupted_sleep request did not return after stopping DB container")
	}
	status, body := result.status, result.body
	if status != http.StatusBadGateway {
		t.Fatalf("interrupted_sleep status=%d want %d; body=%s", status, http.StatusBadGateway, string(body))
	}
	requireAPIErrorCode(t, body, "AGENT_DB_UNREACHABLE")

	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}

func dedicatedPostgresContainer(t *testing.T) (postgresConfig, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout("postgres"))
	defer cancel()
	cfg, _, cleanup, err := startPostgresContainer(ctx)
	if err != nil {
		if os.Getenv("ONPREST_IT_REQUIRE_CONTAINERS") == "1" {
			t.Fatalf("start dedicated postgres testcontainer: %v", err)
		}
		t.Skipf("skip dedicated postgres testcontainer integration: %v", err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = cleanup(ctx)
		})
	}
	t.Cleanup(stop)
	return cfg, stop
}

func waitForPostgresActiveQuery(t *testing.T, cfg postgresConfig, marker string) {
	t.Helper()
	db, err := sql.Open("postgres", postgresDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		var count int
		err := db.QueryRowContext(ctx, `
select count(*)
from pg_stat_activity
where pid <> pg_backend_pid()
  and state = 'active'
  and query like '%' || $1 || '%'`, marker).Scan(&count)
		cancel()
		if err == nil && count > 0 {
			return
		}
		if err != nil && isPostgresConnectionError(err) {
			t.Fatalf("postgres became unreachable before interrupted query was active: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for active postgres query marker %q", marker)
}

func assertPostgresQueryRemainsInactive(t *testing.T, cfg postgresConfig, marker string, duration time.Duration) {
	t.Helper()
	db, err := sql.Open("postgres", postgresDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var count int
		err := db.QueryRowContext(ctx, `select count(*) from pg_stat_activity where state = 'active' and query like '%' || $1 || '%' and pid <> pg_backend_pid()`, marker).Scan(&count)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("query %q reached PostgreSQL while configured max_concurrent_requests worker was occupied", marker)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func isPostgresConnectionError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"connection refused",
		"connection reset",
		"connection timed out",
		"bad connection",
		"server is not accepting",
		"database system is shutting down",
		"terminating connection",
		"connection is closed",
		"unexpected eof",
	} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}
