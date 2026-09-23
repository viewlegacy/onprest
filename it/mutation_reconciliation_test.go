//go:build integration

package it

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	reconciliationMainID      = "req-20260906-001"
	reconciliationPendingID   = "req-20260906-002"
	reconciliationCollisionID = "req-20260906-003"
)

type reconciliationPayload struct {
	ExternalRequestID string `json:"external_request_id"`
	ProductCode       string `json:"product_code"`
	Quantity          int    `json:"quantity"`
	CustomerCode      string `json:"customer_code"`
}

type reconciliationRow struct {
	ExternalRequestID string `json:"external_request_id"`
	ProductCode       string `json:"product_code"`
	Quantity          int    `json:"quantity"`
	CustomerCode      string `json:"customer_code"`
}

type mutationReconciliationFixtures struct {
	restCreate    []byte
	restReconcile []byte
	mcpCreate     []byte
	mcpReconcile  []byte
}

func TestContainerDBDriverMutationReconciliation(t *testing.T) {
	for _, driver := range []string{"postgres", "mysql", "sqlserver", "oracle"} {
		if !selectedDBForTest(t, driver) {
			continue
		}
		driver := driver
		t.Run(driver, func(t *testing.T) {
			dbCfg := selectedContainerDBConfig(t, driver)
			repo := repoRoot(t)
			setupMutationReconciliationFixture(t, repo, driver, dbCfg)

			tmp := t.TempDir()
			gatewayBin := filepath.Join(tmp, "onprest-gateway")
			agentBin := filepath.Join(tmp, "onprest-agent")
			buildBinary(t, repo, gatewayBin, "./cmd/gateway")
			buildBinary(t, repo, agentBin, "./cmd/agent")

			secrets := newITSecrets(t)
			addr := freeAddr(t)
			baseURL := "http://" + addr
			capabilityFile := renderMutationReconciliationCapability(t, repo, tmp, driver, dbCfg, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gateway := startProcess(t, ctx, gatewayBin, nil, []string{
				"GATEWAY_ADDR=" + addr,
				"GATEWAY_AGENT_PUBLIC_KEY=" + secrets.AgentPublicKey,
				"GATEWAY_API_KEYS_JSON=" + secrets.APIKeysJSON,
				"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=100",
				"GATEWAY_RATE_LIMIT_BURST=100",
			})
			defer stopProcess(t, gateway)
			waitForHTTP(t, baseURL+"/healthz", "", http.StatusOK)

			agent := startProcess(t, ctx, agentBin, nil, []string{
				"AGENT_CAPABILITY_FILE=" + capabilityFile,
			})
			defer stopProcess(t, agent)
			waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusOK)

			proxy := newMutationReconciliationHTTPProxy(t, baseURL)
			runReconciliationScenarios(t, driver, dbCfg, baseURL, proxy, secrets.APIKey, agentBin)

			t.Run("read-capability-unavailable", func(t *testing.T) {
				// Read unavailability is tested after all writes have completed. The
				// persisted rows remain available to the independent DB observer, while
				// the public read capability is deliberately stopped.
				stopProcess(t, agent)
				waitForHTTP(t, baseURL+"/openapi.json", secrets.APIKey, http.StatusServiceUnavailable)
				status, body := postCapability(t, baseURL, secrets.APIKey, "reconcile_order", fmt.Sprintf(`{"external_request_id":%q}`, reconciliationMainID))
				if status != http.StatusServiceUnavailable {
					t.Fatalf("read-unavailable status=%d body=%s", status, body)
				}
				requireAPIErrorCode(t, body, "GATEWAY_AGENT_OFFLINE")
				if got := reconciliationRowCount(t, driver, dbCfg); got != 2 {
					t.Fatalf("read-unavailable path changed persisted row count=%d want 2", got)
				}
			})
		})
	}
}

func runReconciliationScenarios(t *testing.T, driver string, cfg postgresConfig, baseURL string, proxy *mutationReconciliationHTTPProxy, apiKey, agentBin string) {
	t.Helper()

	fixtures := mutationReconciliationRequests()
	want := decodeReconciliationRESTPayload(t, fixtures.restCreate)
	assertMutationReconciliationFixtures(t, fixtures, want)

	t.Run("rest-mcp-requests", func(t *testing.T) {
		status, body, err := postMutationRequest(proxy.client(), proxy.URL(), apiKey, "create_order", fixtures.restCreate, "normal")
		if err != nil {
			t.Fatalf("REST create request: %v", err)
		}
		assertMutationCount(t, status, body, 1)
		status, body, err = postMutationRequest(proxy.client(), proxy.URL(), apiKey, "reconcile_order", fixtures.restReconcile, "normal")
		if err != nil {
			t.Fatalf("REST reconcile request: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("REST reconcile status=%d body=%s", status, body)
		}
		assertReconciliationRows(t, body, &want)
		deleteReconciliationOrder(t, driver, cfg, want.ExternalRequestID)
		waitForReconciliationRows(t, driver, cfg, want.ExternalRequestID, nil)
	})

	t.Run("precommit-response-cut-unfinished-row", func(t *testing.T) {
		// The gate lock and active SQL observer prove that the INSERT is still
		// waiting before the response is cut. The read is performed while the
		// earlier transaction is unfinished, so an empty result is not treated as
		// proof that the INSERT rolled back.
		pendingPayload := reconciliationPayload{
			ExternalRequestID: reconciliationPendingID,
			ProductCode:       "SKU-PENDING",
			Quantity:          3,
			CustomerCode:      "CUST-PENDING",
		}
		blocker := lockReconciliationGate(t, driver, cfg)
		pendingDone := make(chan mutationHTTPResult, 1)
		go func() {
			status, body, err := postMutationCapability(proxy.client(), proxy.URL(), apiKey, "create_order", pendingPayload, "normal")
			pendingDone <- mutationHTTPResult{status: status, body: body, err: err}
		}()
		waitForMutationReconciliationQuery(t, driver, cfg, reconciliationMutationMarker)
		if driver == "sqlserver" {
			// SQL Server's normal READ COMMITTED read can wait on the trigger's
			// uncommitted row. The test-only observer uses READPAST to record an
			// actual empty DB result without changing the published capability.
			if rows := readUnfinishedReconciliationRows(t, driver, cfg, reconciliationPendingID); len(rows) != 0 {
				t.Fatalf("unfinished SQL Server reconciliation rows=%#v, want empty result", rows)
			}
		} else {
			status, body := postCapability(t, baseURL, apiKey, "reconcile_order", fmt.Sprintf(`{"external_request_id":%q}`, reconciliationPendingID))
			if status != http.StatusOK {
				t.Fatalf("unfinished transaction read status=%d body=%s", status, body)
			}
			assertReconciliationRows(t, body, nil)
		}
		proxy.dropActiveClient(t)
		pending := <-pendingDone
		if pending.err == nil {
			t.Fatalf("pre-commit response cut unexpectedly returned status=%d body=%s", pending.status, pending.body)
		}
		if err := blocker.Rollback(); err != nil {
			t.Fatalf("release pre-commit gate: %v", err)
		}
		// A Gateway timeout does not establish whether the Agent committed.
		// Wait for either a visible full row or the Agent's terminal error,
		// then remove a committed trial row before the next scenario.
		if waitForResponseCutReconciliationOutcome(t, driver, cfg, agentBin, pendingPayload) {
			deleteReconciliationOrder(t, driver, cfg, reconciliationPendingID)
			waitForReconciliationRows(t, driver, cfg, reconciliationPendingID, nil)
		}
	})

	t.Run("postcommit-response-cut-all-fields-match", func(t *testing.T) {
		// The MCP response is held at the HTTP proxy. The Agent can only send a
		// successful mutation response after commit; the independent DB read is
		// performed while the response bytes are still held, before the proxy
		// closes the client connection.
		attemptsBefore := reconciliationAttemptCount(t, driver, cfg)
		postCommitDone := make(chan mutationHTTPResult, 1)
		go func() {
			status, body, err := postMCPMutationRequest(proxy.client(), proxy.URL(), apiKey, fixtures.mcpCreate, "hold-response")
			postCommitDone <- mutationHTTPResult{status: status, body: body, err: err}
		}()
		proxy.waitResponseReady(t)
		waitForReconciliationRows(t, driver, cfg, want.ExternalRequestID, &want)
		proxy.dropActiveClient(t)
		postCommit := <-postCommitDone
		if postCommit.err == nil {
			t.Fatalf("post-commit response cut unexpectedly returned status=%d body=%s", postCommit.status, postCommit.body)
		}
		assertReconciliationAttemptDelta(t, driver, cfg, attemptsBefore, 1)
		status, body, err := postMCPMutationRequest(proxy.client(), proxy.URL(), apiKey, fixtures.mcpReconcile, "normal")
		if err != nil {
			t.Fatalf("MCP reconcile request: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("post-commit MCP reconciliation status=%d body=%s", status, body)
		}
		assertReconciliationMCPRows(t, body, &want)
	})

	t.Run("other-actor-update-payload-mismatch", func(t *testing.T) {
		// A different actor changes business fields after the commit. The read
		// must expose the mismatch; the request ID alone is insufficient evidence.
		changed := want
		changed.ProductCode = "SKU-OTHER"
		changed.Quantity = 7
		changed.CustomerCode = "CUST-OTHER"
		updateReconciliationOrder(t, driver, cfg, changed)
		status, body := postCapability(t, baseURL, apiKey, "reconcile_order", fmt.Sprintf(`{"external_request_id":%q}`, want.ExternalRequestID))
		if status != http.StatusOK {
			t.Fatalf("other-actor read status=%d body=%s", status, body)
		}
		assertReconciliationRows(t, body, &changed)
		if got := reconciliationPayloadFromRows(t, body); got == want {
			t.Fatal("other-actor payload unexpectedly still matched the original request")
		}
	})

	t.Run("concurrent-other-actor-update-snapshot", func(t *testing.T) {
		// Hold the gate while a second actor has already updated the order but is
		// waiting to commit. The read request is admitted by the proxy before the
		// gate is released, so each database must return one complete snapshot
		// (the old payload or the new payload), never a field-by-field hybrid.
		before := reconciliationPayload{
			ExternalRequestID: want.ExternalRequestID,
			ProductCode:       "SKU-OTHER",
			Quantity:          7,
			CustomerCode:      "CUST-OTHER",
		}
		after := reconciliationPayload{
			ExternalRequestID: want.ExternalRequestID,
			ProductCode:       "SKU-CONCURRENT",
			Quantity:          9,
			CustomerCode:      "CUST-CONCURRENT",
		}
		blocker := lockReconciliationGate(t, driver, cfg)
		updateDone := startConcurrentReconciliationUpdate(t, driver, cfg, after)
		waitForMutationReconciliationQuery(t, driver, cfg, reconciliationConcurrentUpdateMarker)

		readDone := make(chan mutationHTTPResult, 1)
		go func() {
			status, body, err := postMutationRequest(proxy.client(), proxy.URL(), apiKey, "reconcile_order", fixtures.restReconcile, "hold-upstream-hold-response")
			readDone <- mutationHTTPResult{status: status, body: body, err: err}
		}()
		proxy.waitActive(t)
		proxy.releaseActiveUpstream(t)
		waitForConcurrentReconciliationRead(t, driver, cfg, proxy)
		if err := blocker.Rollback(); err != nil {
			t.Fatalf("release concurrent update gate: %v", err)
		}
		if err := <-updateDone; err != nil {
			t.Fatalf("concurrent other-actor update: %v", err)
		}
		if driver == "sqlserver" {
			// SQL Server's read was proved to be waiting on the uncommitted
			// order update above. Once that update commits, the held response
			// must become available before the client is released.
			proxy.waitResponseReady(t)
		}
		proxy.releaseActiveResponse(t)
		read := <-readDone
		if read.err != nil {
			t.Fatalf("concurrent reconciliation read: %v", read.err)
		}
		if read.status != http.StatusOK {
			t.Fatalf("concurrent reconciliation read status=%d body=%s", read.status, read.body)
		}
		got := reconciliationPayloadFromRows(t, read.body)
		if got != before && got != after {
			t.Fatalf("concurrent reconciliation returned hybrid snapshot=%#v; want %#v or %#v", got, before, after)
		}
	})

	t.Run("unique-request-id-collision", func(t *testing.T) {
		// A pre-existing row with the same unique business key is a collision.
		// The existing payload remains observable and the failed INSERT is not
		// retried.
		collision := reconciliationPayload{
			ExternalRequestID: reconciliationCollisionID,
			ProductCode:       "SKU-EXISTING",
			Quantity:          4,
			CustomerCode:      "CUST-EXISTING",
		}
		insertReconciliationOrder(t, driver, cfg, collision)
		attemptsBefore := reconciliationAttemptCount(t, driver, cfg)
		status, body := postCapability(t, baseURL, apiKey, "create_order", fmt.Sprintf(`{"external_request_id":%q,"product_code":%q,"quantity":%d,"customer_code":%q}`, collision.ExternalRequestID, want.ProductCode, want.Quantity, want.CustomerCode))
		if status != http.StatusConflict {
			t.Fatalf("unique collision status=%d body=%s", status, body)
		}
		requireAPIErrorCode(t, body, "AGENT_CONSTRAINT_VIOLATION")
		assertReconciliationAttemptDelta(t, driver, cfg, attemptsBefore, 1)
		status, body = postCapability(t, baseURL, apiKey, "reconcile_order", fmt.Sprintf(`{"external_request_id":%q}`, collision.ExternalRequestID))
		if status != http.StatusOK {
			t.Fatalf("unique collision read status=%d body=%s", status, body)
		}
		assertReconciliationRows(t, body, &collision)
	})
}

func waitForConcurrentReconciliationRead(t *testing.T, driver string, cfg postgresConfig, proxy *mutationReconciliationHTTPProxy) {
	t.Helper()
	if driver == "sqlserver" {
		// The normal READ COMMITTED SELECT is blocked by the concurrent
		// uncommitted order update. Observe that exact read request in SQL
		// Server before releasing the gate; a generic active-request count or
		// the observer's own query would not prove that the public read ran.
		waitForSQLServerReconciliationRead(t, cfg)
		t.Log("observed the published reconciliation SELECT suspended on an order-row S-lock before releasing the update gate")
		return
	}
	// For the MVCC databases the public SELECT can complete while the
	// updater is waiting on the gate. The proxy response is marked only
	// after the real Gateway/Agent request has completed its DB query, so
	// waiting for it is the execution barrier before releasing the gate.
	proxy.waitResponseReady(t)
	t.Log("observed the real reconciliation response after its DB read completed before releasing the update gate")
}

func mutationReconciliationRequests() mutationReconciliationFixtures {
	return mutationReconciliationFixtures{
		restCreate: []byte(`{
  "external_request_id": "req-20260906-001",
  "product_code": "SKU-001",
  "quantity": 2,
  "customer_code": "CUST-001"
}`),
		restReconcile: []byte(`{
  "external_request_id": "req-20260906-001"
}`),
		mcpCreate: []byte(`{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "create_order",
    "arguments": {
      "external_request_id": "req-20260906-001",
      "product_code": "SKU-001",
      "quantity": 2,
      "customer_code": "CUST-001"
    }
  }
}`),
		mcpReconcile: []byte(`{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "reconcile_order",
    "arguments": {
      "external_request_id": "req-20260906-001"
    }
  }
}`),
	}
}

func decodeReconciliationRESTPayload(t *testing.T, body []byte) reconciliationPayload {
	t.Helper()
	var payload reconciliationPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode REST create request: %v", err)
	}
	if payload.ExternalRequestID == "" || payload.ProductCode == "" || payload.Quantity <= 0 || payload.CustomerCode == "" {
		t.Fatalf("REST create request omitted business fields: %#v", payload)
	}
	return payload
}

func assertMutationReconciliationFixtures(t *testing.T, fixtures mutationReconciliationFixtures, want reconciliationPayload) {
	t.Helper()
	var restReconcile struct {
		ExternalRequestID string `json:"external_request_id"`
	}
	if err := json.Unmarshal(fixtures.restReconcile, &restReconcile); err != nil {
		t.Fatalf("decode REST reconcile request: %v", err)
	}
	if restReconcile.ExternalRequestID != want.ExternalRequestID {
		t.Fatalf("REST reconcile ID=%q want %q", restReconcile.ExternalRequestID, want.ExternalRequestID)
	}
	var mcpCreate struct {
		Params struct {
			Name      string                `json:"name"`
			Arguments reconciliationPayload `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(fixtures.mcpCreate, &mcpCreate); err != nil {
		t.Fatalf("decode MCP create fixture: %v", err)
	}
	if mcpCreate.Params.Name != "create_order" || mcpCreate.Params.Arguments != want {
		t.Fatalf("MCP create fixture=%#v want create_order payload=%#v", mcpCreate, want)
	}
	var mcpReconcile struct {
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				ExternalRequestID string `json:"external_request_id"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(fixtures.mcpReconcile, &mcpReconcile); err != nil {
		t.Fatalf("decode MCP reconcile fixture: %v", err)
	}
	if mcpReconcile.Params.Name != "reconcile_order" || mcpReconcile.Params.Arguments.ExternalRequestID != want.ExternalRequestID {
		t.Fatalf("MCP reconcile fixture=%#v want ID=%q", mcpReconcile, want.ExternalRequestID)
	}
}

func assertMutationCount(t *testing.T, status int, body []byte, want int) {
	t.Helper()
	var response struct {
		Count int `json:"count"`
	}
	if status != http.StatusOK {
		t.Fatalf("mutation status=%d body=%s", status, body)
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode mutation count: %v; body=%s", err, body)
	}
	if response.Count != want {
		t.Fatalf("mutation count=%d want %d; body=%s", response.Count, want, body)
	}
}

type mutationHTTPResult struct {
	status int
	body   []byte
	err    error
}

func postMutationCapability(client *http.Client, baseURL, apiKey, name string, payload reconciliationPayload, mode string) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	return postMutationRequest(client, baseURL, apiKey, name, body, mode)
}

func postMutationRequest(client *http.Client, baseURL, apiKey, name string, body []byte, mode string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/capabilities/"+name, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Onprest-Reconciliation-Proxy", mode)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, responseBody, err
}

func postMCPMutationRequest(client *http.Client, baseURL, apiKey string, body []byte, mode string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", modernMCPProtocolVersion)
	req.Header.Set("X-Onprest-Reconciliation-Proxy", mode)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, responseBody, err
}

func assertReconciliationMCPRows(t *testing.T, body []byte, want *reconciliationPayload) {
	t.Helper()
	var response struct {
		Result struct {
			IsError           bool                          `json:"isError"`
			Content           []struct{ Type, Text string } `json:"content"`
			StructuredContent json.RawMessage               `json:"structuredContent"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode reconciliation MCP body: %v; body=%s", err, body)
	}
	if len(response.Error) != 0 && string(response.Error) != "null" {
		t.Fatalf("reconciliation MCP JSON-RPC error=%s", response.Error)
	}
	if response.Result.IsError || len(response.Result.Content) != 1 || response.Result.Content[0].Type != "text" {
		t.Fatalf("reconciliation MCP result=%#v", response.Result)
	}
	assertReconciliationRows(t, []byte(response.Result.Content[0].Text), want)
	if len(response.Result.StructuredContent) == 0 || string(response.Result.StructuredContent) == "null" {
		t.Fatal("reconciliation MCP response omitted structuredContent")
	}
	assertReconciliationRows(t, response.Result.StructuredContent, want)
}

func assertReconciliationRows(t *testing.T, body []byte, want *reconciliationPayload) {
	t.Helper()
	var response struct {
		Rows  []reconciliationRow `json:"rows"`
		Count int                 `json:"count"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&response); err != nil {
		t.Fatalf("decode reconciliation rows: %v; body=%s", err, body)
	}
	if want == nil {
		if response.Count != 0 || len(response.Rows) != 0 {
			t.Fatalf("unfinished transaction reconciliation=%#v, want empty rows", response)
		}
		return
	}
	if response.Count != len(response.Rows) || len(response.Rows) != 1 {
		t.Fatalf("reconciliation result=%#v, want one row", response)
	}
	got := response.Rows[0]
	if got.ExternalRequestID != want.ExternalRequestID || got.ProductCode != want.ProductCode || got.Quantity != want.Quantity || got.CustomerCode != want.CustomerCode {
		t.Fatalf("reconciliation row=%#v, want all business fields=%#v", got, want)
	}
}

func reconciliationPayloadFromRows(t *testing.T, body []byte) reconciliationPayload {
	t.Helper()
	var response struct {
		Rows []reconciliationRow `json:"rows"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode reconciliation payload: %v; body=%s", err, body)
	}
	if len(response.Rows) != 1 {
		t.Fatalf("reconciliation rows=%d, want 1", len(response.Rows))
	}
	row := response.Rows[0]
	return reconciliationPayload{ExternalRequestID: row.ExternalRequestID, ProductCode: row.ProductCode, Quantity: row.Quantity, CustomerCode: row.CustomerCode}
}

func renderMutationReconciliationCapability(t *testing.T, repo, dir, driver string, db postgresConfig, gatewayURL, agentPrivateKey string) string {
	t.Helper()
	examplePath := filepath.Join(repo, "examples", "mutation-reconciliation", "capability."+driver+".yaml")
	content, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read mutation reconciliation capability example: %v", err)
	}
	rendered := string(content)
	start := strings.Index(rendered, "database:\n")
	if start < 0 {
		t.Fatal("example missing database section")
	}
	end := strings.Index(rendered[start:], "\ngateway:") + start
	if end <= start {
		t.Fatal("example missing database section")
	}
	database := "database:\n  driver: " + driver + "\n  host: " + yamlString(db.Host) + "\n  port: " + db.Port + "\n  name: " + yamlString(db.Name) + "\n  user: " + yamlString(db.User) + "\n  password: " + yamlString(db.Password) + "\n"
	rendered = rendered[:start] + database + rendered[end:]
	gatewayStart := strings.Index(rendered, "gateway:\n")
	if gatewayStart < 0 {
		t.Fatal("example missing gateway section")
	}
	gatewayEnd := strings.Index(rendered[gatewayStart:], "\ndefaults:") + gatewayStart
	if gatewayEnd <= gatewayStart {
		t.Fatal("example missing gateway section")
	}
	gateway := "gateway:\n  url: " + yamlString(gatewayURL) + "\n  agent_private_key: " + yamlString(agentPrivateKey) + "\n"
	rendered = rendered[:gatewayStart] + gateway + rendered[gatewayEnd:]
	return writeFile(t, filepath.Join(dir, "capability.reconciliation."+driver+".yaml"), rendered)
}

func setupMutationReconciliationFixture(t *testing.T, repo, driver string, cfg postgresConfig) {
	t.Helper()
	db := openIntegrationDB(t, driver, cfg)
	defer db.Close()
	adminDB := db
	if driver == "mysql" {
		adminCfg := cfg
		adminCfg.User = "root"
		adminDB = openIntegrationDB(t, driver, adminCfg)
		defer adminDB.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range reconciliationDropStatements(driver) {
		if _, err := adminDB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s drop reconciliation fixture: %v\n%s", driver, err, statement)
		}
	}
	path := reconciliationFixturePath(repo, driver, "schema")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s reconciliation schema: %v", driver, err)
	}
	for _, statement := range splitReconciliationSQL(string(content)) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s apply reconciliation schema: %v\n%s", driver, err, statement)
		}
	}
	for _, statement := range reconciliationAttemptStatements(driver) {
		if _, err := adminDB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s apply reconciliation attempt observer: %v\n%s", driver, err, statement)
		}
	}
	for _, statement := range reconciliationBarrierStatements(driver) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s apply reconciliation barrier: %v\n%s", driver, err, statement)
		}
	}
	t.Cleanup(func() {
		cleanupCfg := cfg
		if driver == "mysql" {
			cleanupCfg.User = "root"
		}
		cleanupDB, err := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, cleanupCfg))
		if err != nil {
			t.Logf("open %s cleanup DB: %v", driver, err)
			return
		}
		defer cleanupDB.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, statement := range reconciliationDropStatements(driver) {
			if _, err := cleanupDB.ExecContext(cleanupCtx, statement); err != nil {
				t.Logf("%s cleanup reconciliation fixture: %v", driver, err)
			}
		}
	})
}

func openIntegrationDB(t *testing.T, driver string, cfg postgresConfig) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, cfg))
	if err != nil {
		t.Fatalf("open %s integration DB: %v", driver, err)
	}
	return db
}

func reconciliationFixturePath(repo, driver, kind string) string {
	return filepath.Join(repo, "examples", "mutation-reconciliation", kind+"."+driver+".sql")
}

func splitReconciliationSQL(content string) []string {
	statements := make([]string, 0, 4)
	for _, raw := range strings.Split(content, ";") {
		if statement := strings.TrimSpace(raw); statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}

func reconciliationDropStatements(driver string) []string {
	switch driver {
	case "postgres":
		return []string{
			"drop table if exists mutation_reconciliation_orders",
			"drop table if exists mutation_reconciliation_gate",
			"drop function if exists mutation_reconciliation_record_attempt()",
			"drop sequence if exists mutation_reconciliation_attempt_seq",
		}
	case "mysql":
		return []string{
			"drop table if exists mutation_reconciliation_orders",
			"drop table if exists mutation_reconciliation_gate",
			"drop table if exists mutation_reconciliation_attempts",
		}
	case "sqlserver":
		return []string{
			"if object_id('dbo.mutation_reconciliation_lock_gate', 'TR') is not null drop trigger dbo.mutation_reconciliation_lock_gate",
			"if object_id('dbo.mutation_reconciliation_attempt_observer', 'TR') is not null drop trigger dbo.mutation_reconciliation_attempt_observer",
			"if object_id('dbo.mutation_reconciliation_orders', 'U') is not null and object_id('dbo.df_mutation_reconciliation_attempt_id', 'D') is not null alter table dbo.mutation_reconciliation_orders drop constraint df_mutation_reconciliation_attempt_id",
			"if object_id('dbo.mutation_reconciliation_orders', 'U') is not null drop table dbo.mutation_reconciliation_orders",
			"if object_id('dbo.mutation_reconciliation_gate', 'U') is not null drop table dbo.mutation_reconciliation_gate",
			"if object_id('dbo.mutation_reconciliation_attempt_seq', 'SO') is not null drop sequence dbo.mutation_reconciliation_attempt_seq",
		}
	case "oracle":
		return []string{
			"begin execute immediate 'drop table mutation_reconciliation_orders purge'; exception when others then if sqlcode != -942 then raise; end if; end;",
			"begin execute immediate 'drop table mutation_reconciliation_gate purge'; exception when others then if sqlcode != -942 then raise; end if; end;",
			"begin execute immediate 'drop table mutation_reconciliation_attempts purge'; exception when others then if sqlcode != -942 then raise; end if; end;",
			"begin execute immediate 'drop sequence mutation_reconciliation_attempt_seq'; exception when others then if sqlcode != -2289 then raise; end if; end;",
		}
	default:
		return nil
	}
}

func reconciliationAttemptStatements(driver string) []string {
	switch driver {
	case "postgres":
		return []string{
			"create sequence mutation_reconciliation_attempt_seq",
			"create or replace function mutation_reconciliation_record_attempt() returns trigger language plpgsql as $$ begin perform nextval('mutation_reconciliation_attempt_seq'); return new; end $$",
			"create trigger mutation_reconciliation_attempt_observer before insert on mutation_reconciliation_orders for each row execute function mutation_reconciliation_record_attempt()",
		}
	case "mysql":
		return []string{
			"create table mutation_reconciliation_attempts (attempt_id bigint unsigned not null auto_increment primary key) engine=MyISAM",
			"create trigger mutation_reconciliation_attempt_observer before insert on mutation_reconciliation_orders for each row insert into mutation_reconciliation_attempts () values ()",
		}
	case "sqlserver":
		return []string{
			"create sequence dbo.mutation_reconciliation_attempt_seq as bigint start with 1 increment by 1",
			"alter table dbo.mutation_reconciliation_orders add mutation_reconciliation_attempt_id bigint not null constraint df_mutation_reconciliation_attempt_id default (next value for dbo.mutation_reconciliation_attempt_seq)",
		}
	case "oracle":
		return []string{
			"create table mutation_reconciliation_attempts (attempt_id number(19) primary key)",
			"create sequence mutation_reconciliation_attempt_seq",
			"create or replace trigger mutation_reconciliation_attempt_observer before insert on mutation_reconciliation_orders for each row declare pragma autonomous_transaction; begin insert into mutation_reconciliation_attempts (attempt_id) values (mutation_reconciliation_attempt_seq.nextval); commit; end;",
		}
	default:
		return nil
	}
}

func reconciliationAttemptCount(t *testing.T, driver string, cfg postgresConfig) int64 {
	t.Helper()
	observerCfg := cfg
	if driver == "mysql" {
		observerCfg.User = "root"
	}
	db := openIntegrationDB(t, driver, observerCfg)
	defer db.Close()
	var count int64
	switch driver {
	case "postgres":
		var last int64
		var called bool
		if err := db.QueryRow("select last_value, is_called from mutation_reconciliation_attempt_seq").Scan(&last, &called); err != nil {
			t.Fatalf("%s read reconciliation attempt sequence: %v", driver, err)
		}
		if called {
			count = last
		}
	case "mysql":
		if err := db.QueryRow("select count(*) from mutation_reconciliation_attempts").Scan(&count); err != nil {
			t.Fatalf("%s read reconciliation attempt audit: %v", driver, err)
		}
	case "sqlserver":
		var current sql.NullInt64
		if err := db.QueryRow("select current_value from sys.sequences where object_id = object_id('dbo.mutation_reconciliation_attempt_seq')").Scan(&current); err != nil {
			t.Fatalf("%s read reconciliation attempt sequence: %v", driver, err)
		}
		if current.Valid {
			count = current.Int64
		}
	case "oracle":
		if err := db.QueryRow("select count(*) from mutation_reconciliation_attempts").Scan(&count); err != nil {
			t.Fatalf("%s read reconciliation attempt audit: %v", driver, err)
		}
	default:
		t.Fatalf("unsupported reconciliation attempt driver %s", driver)
	}
	return count
}

func assertReconciliationAttemptDelta(t *testing.T, driver string, cfg postgresConfig, before, wantDelta int64) {
	t.Helper()
	after := reconciliationAttemptCount(t, driver, cfg)
	if got := after - before; got != wantDelta {
		t.Fatalf("%s reconciliation INSERT attempts=%d, want exactly %d (before=%d after=%d)", driver, got, wantDelta, before, after)
	}
}

func reconciliationBarrierStatements(driver string) []string {
	switch driver {
	case "postgres":
		return []string{
			"create table mutation_reconciliation_gate (id integer primary key)",
			"insert into mutation_reconciliation_gate (id) values (1)",
			"alter table mutation_reconciliation_orders add column mutation_reconciliation_gate_id integer not null default 1",
			"alter table mutation_reconciliation_orders add constraint fk_mutation_reconciliation_gate foreign key (mutation_reconciliation_gate_id) references mutation_reconciliation_gate(id)",
		}
	case "mysql":
		return []string{
			"create table mutation_reconciliation_gate (id int primary key) engine=InnoDB",
			"insert into mutation_reconciliation_gate (id) values (1)",
			"alter table mutation_reconciliation_orders add column mutation_reconciliation_gate_id int not null default 1",
			"alter table mutation_reconciliation_orders add constraint fk_mutation_reconciliation_gate foreign key (mutation_reconciliation_gate_id) references mutation_reconciliation_gate(id)",
		}
	case "sqlserver":
		return []string{
			"create table dbo.mutation_reconciliation_gate (id int primary key)",
			"insert into dbo.mutation_reconciliation_gate (id) values (1)",
			"create trigger dbo.mutation_reconciliation_lock_gate on dbo.mutation_reconciliation_orders after insert as begin set nocount on; update g with (updlock, holdlock) set id = 1 from dbo.mutation_reconciliation_gate as g where g.id = 1 and exists (select 1 from inserted); end",
		}
	case "oracle":
		return []string{
			"create table mutation_reconciliation_gate (id number(10) primary key)",
			"insert into mutation_reconciliation_gate (id) values (1)",
			"create or replace trigger mutation_reconciliation_lock_gate after insert on mutation_reconciliation_orders for each row begin update mutation_reconciliation_gate set id = 1 where id = 1; end;",
		}
	default:
		return nil
	}
}

func reconciliationGateTable(driver string) string {
	if driver == "sqlserver" {
		return "dbo.mutation_reconciliation_gate"
	}
	return "mutation_reconciliation_gate"
}

func reconciliationOrdersTable(driver string) string {
	if driver == "sqlserver" {
		return "dbo.mutation_reconciliation_orders"
	}
	return "mutation_reconciliation_orders"
}

type lockedReconciliationGate struct {
	db *sql.DB
	tx *sql.Tx
}

func lockReconciliationGate(t *testing.T, driver string, cfg postgresConfig) *lockedReconciliationGate {
	t.Helper()
	db := openIntegrationDB(t, driver, cfg)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		db.Close()
		t.Fatalf("%s begin reconciliation gate transaction: %v", driver, err)
	}
	query := fmt.Sprintf("select id from %s where id = 1 for update", reconciliationGateTable(driver))
	if driver == "sqlserver" {
		query = fmt.Sprintf("select id from %s with (updlock, holdlock) where id = 1", reconciliationGateTable(driver))
	}
	var id int
	if err := tx.QueryRowContext(context.Background(), query).Scan(&id); err != nil {
		_ = tx.Rollback()
		db.Close()
		t.Fatalf("%s lock reconciliation gate: %v", driver, err)
	}
	if id != 1 {
		_ = tx.Rollback()
		db.Close()
		t.Fatalf("%s reconciliation gate id=%d want 1", driver, id)
	}
	locked := &lockedReconciliationGate{db: db, tx: tx}
	t.Cleanup(func() {
		_ = locked.tx.Rollback()
		_ = locked.db.Close()
	})
	return locked
}

func (g *lockedReconciliationGate) Rollback() error {
	err := g.tx.Rollback()
	closeErr := g.db.Close()
	if err != nil {
		return err
	}
	return closeErr
}

const reconciliationMutationMarker = "onprest_reconciliation_insert"
const reconciliationConcurrentUpdateMarker = "onprest_reconciliation_concurrent_update"

func waitForMutationReconciliationQuery(t *testing.T, driver string, cfg postgresConfig, marker string) {
	t.Helper()
	admin := cfg
	query := ""
	args := []any{}
	switch driver {
	case "postgres":
		query = `select count(*) from pg_stat_activity where pid <> pg_backend_pid() and state = 'active' and query like '%' || $1 || '%'`
		args = append(args, marker)
	case "mysql":
		admin.User = "root"
		query = `select count(*) from information_schema.processlist where id <> connection_id() and command in ('Query', 'Execute') and info like concat('%', ?, '%')`
		args = append(args, marker)
	case "sqlserver":
		// SQL Server may hide the prepared SQL comment from dm_exec_sql_text.
		// A suspended request with a blocker in this database is the observable
		// lock barrier for the trigger's gate update.
		query = `select count(*) from sys.dm_exec_requests where session_id <> @@spid and database_id = db_id() and blocking_session_id <> 0 and status = 'suspended'`
	case "oracle":
		admin.User = "system"
		// Oracle can omit the prepared SQL comment from v$sql while the trigger
		// waits on the gate. A valid blocking session is the barrier evidence.
		query = `select count(*) from v$session where sid <> sys_context('USERENV', 'SID') and status = 'ACTIVE' and blocking_session is not null`
	default:
		t.Fatalf("unsupported reconciliation query driver %s", driver)
	}
	observer := openIntegrationDB(t, driver, admin)
	defer observer.Close()
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	var lastCount int
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		var count int
		err := observer.QueryRowContext(ctx, query, args...).Scan(&count)
		cancel()
		lastErr = err
		lastCount = count
		if err == nil && count > 0 {
			return
		}
		if err != nil && driver == "oracle" && strings.Contains(strings.ToLower(err.Error()), "table or view does not exist") {
			t.Fatalf("Oracle reconciliation observer cannot inspect active SQL: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s reconciliation query marker %q (last count=%d err=%v)", driver, marker, lastCount, lastErr)
}

func waitForSQLServerReconciliationRead(t *testing.T, cfg postgresConfig) {
	t.Helper()
	observer := openIntegrationDB(t, "sqlserver", cfg)
	defer observer.Close()
	// The request must be the published reconciliation SELECT itself, not the
	// observer query or the concurrent UPDATE's gate wait. The S-lock wait on
	// this table is the SQL Server proof that the public read reached the DB
	// while the other actor still owns the uncommitted order update.
	query := `
select count(*)
from sys.dm_exec_requests as r
cross apply sys.dm_exec_sql_text(r.sql_handle) as sql_text
where r.session_id <> @@spid
  and r.database_id = db_id()
  and r.command = 'SELECT'
  and r.status = 'suspended'
  and r.blocking_session_id <> 0
  and r.wait_type in ('LCK_M_S', 'LCK_M_SCH_S')
  and lower(convert(nvarchar(max), sql_text.text)) like '%from dbo.mutation_reconciliation_orders%'`
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	var lastCount int
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		var count int
		err := observer.QueryRowContext(ctx, query).Scan(&count)
		cancel()
		lastErr = err
		lastCount = count
		if err == nil && count > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the published SQL Server reconciliation SELECT to block (last count=%d err=%v)", lastCount, lastErr)
}

func insertReconciliationOrder(t *testing.T, driver string, cfg postgresConfig, payload reconciliationPayload) {
	t.Helper()
	db := openIntegrationDB(t, driver, cfg)
	defer db.Close()
	query := fmt.Sprintf("insert into %s (external_request_id, product_code, quantity, customer_code) values (%s, %s, %d, %s)", reconciliationOrdersTable(driver), sqlLiteral(payload.ExternalRequestID), sqlLiteral(payload.ProductCode), payload.Quantity, sqlLiteral(payload.CustomerCode))
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("%s insert reconciliation order: %v", driver, err)
	}
}

func deleteReconciliationOrder(t *testing.T, driver string, cfg postgresConfig, requestID string) {
	t.Helper()
	db := openIntegrationDB(t, driver, cfg)
	defer db.Close()
	query := fmt.Sprintf("delete from %s where external_request_id = %s", reconciliationOrdersTable(driver), sqlLiteral(requestID))
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("%s delete reconciliation order: %v", driver, err)
	}
}

func updateReconciliationOrder(t *testing.T, driver string, cfg postgresConfig, payload reconciliationPayload) {
	t.Helper()
	db := openIntegrationDB(t, driver, cfg)
	defer db.Close()
	query := fmt.Sprintf("update %s set product_code = %s, quantity = %d, customer_code = %s where external_request_id = %s", reconciliationOrdersTable(driver), sqlLiteral(payload.ProductCode), payload.Quantity, sqlLiteral(payload.CustomerCode), sqlLiteral(payload.ExternalRequestID))
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("%s update reconciliation order: %v", driver, err)
	}
}

func startConcurrentReconciliationUpdate(t *testing.T, driver string, cfg postgresConfig, payload reconciliationPayload) <-chan error {
	t.Helper()
	db := openIntegrationDB(t, driver, cfg)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		db.Close()
		t.Fatalf("%s begin concurrent reconciliation update: %v", driver, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := make(chan error, 1)
	go func() {
		update := fmt.Sprintf("update %s set product_code = %s, quantity = %d, customer_code = %s where external_request_id = %s", reconciliationOrdersTable(driver), sqlLiteral(payload.ProductCode), payload.Quantity, sqlLiteral(payload.CustomerCode), sqlLiteral(payload.ExternalRequestID))
		if _, err := tx.ExecContext(ctx, update); err != nil {
			done <- err
			return
		}
		gateUpdate := fmt.Sprintf("/* %s */ update %s set id = 1 where id = 1", reconciliationConcurrentUpdateMarker, reconciliationGateTable(driver))
		if _, err := tx.ExecContext(ctx, gateUpdate); err != nil {
			done <- err
			return
		}
		done <- tx.Commit()
	}()
	t.Cleanup(func() {
		cancel()
		_ = tx.Rollback()
		_ = db.Close()
	})
	return done
}

func reconciliationRowCount(t *testing.T, driver string, cfg postgresConfig) int {
	t.Helper()
	db := openIntegrationDB(t, driver, cfg)
	defer db.Close()
	var count int
	if err := db.QueryRow("select count(*) from " + reconciliationOrdersTable(driver)).Scan(&count); err != nil {
		t.Fatalf("%s count reconciliation rows: %v", driver, err)
	}
	return count
}

func waitForReconciliationRows(t *testing.T, driver string, cfg postgresConfig, requestID string, want *reconciliationPayload) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows := readReconciliationRows(t, driver, cfg, requestID)
		if want == nil && len(rows) == 0 {
			return
		}
		if want != nil && len(rows) == 1 && rows[0] == (reconciliationRow{ExternalRequestID: want.ExternalRequestID, ProductCode: want.ProductCode, Quantity: want.Quantity, CustomerCode: want.CustomerCode}) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for reconciliation row %q, want=%#v", requestID, want)
}

func waitForResponseCutReconciliationOutcome(t *testing.T, driver string, cfg postgresConfig, agentBin string, payload reconciliationPayload) bool {
	t.Helper()
	logPath := agentBin + ".log"
	deadline := time.Now().Add(20 * time.Second)
	want := reconciliationRow{
		ExternalRequestID: payload.ExternalRequestID,
		ProductCode:       payload.ProductCode,
		Quantity:          payload.Quantity,
		CustomerCode:      payload.CustomerCode,
	}
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(logPath)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read Agent reconciliation detail log: %v", err)
		}
		errorCode := ""
		for _, line := range bytes.Split(content, []byte{'\n'}) {
			var entry struct {
				Capability string `json:"capability"`
				ErrorCode  string `json:"error_code"`
			}
			if json.Unmarshal(line, &entry) != nil || entry.Capability != "create_order" {
				continue
			}
			errorCode = entry.ErrorCode
			if errorCode != "AGENT_QUERY_TIMEOUT" && errorCode != "AGENT_TRANSACTION_OUTCOME_UNKNOWN" {
				t.Fatalf("response-cut reconciliation mutation ended with %s", errorCode)
			}
			break
		}
		var rows []reconciliationRow
		if driver == "sqlserver" {
			rows = readUnfinishedReconciliationRows(t, driver, cfg, payload.ExternalRequestID)
		} else {
			rows = readReconciliationRows(t, driver, cfg, payload.ExternalRequestID)
		}
		if len(rows) > 1 {
			t.Fatalf("response-cut reconciliation returned multiple rows: %#v", rows)
		}
		if len(rows) == 1 {
			if rows[0] != want {
				t.Fatalf("response-cut reconciliation payload=%#v, want %#v", rows[0], want)
			}
			if errorCode == "AGENT_QUERY_TIMEOUT" {
				t.Fatal("mutation committed after Agent reported confirmed rollback")
			}
			t.Log("response-cut mutation committed; independent DB read matched the full payload")
			return true
		}
		if errorCode != "" {
			t.Logf("response-cut mutation ended with %s and no committed row", errorCode)
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting to reconcile response-cut mutation outcome")
	return false
}

func readReconciliationRows(t *testing.T, driver string, cfg postgresConfig, requestID string) []reconciliationRow {
	t.Helper()
	return readReconciliationRowsWithQuery(t, driver, cfg, requestID, "")
}

func readUnfinishedReconciliationRows(t *testing.T, driver string, cfg postgresConfig, requestID string) []reconciliationRow {
	t.Helper()
	if driver != "sqlserver" {
		t.Fatalf("unfinished reconciliation read hint requested for %s", driver)
	}
	return readReconciliationRowsWithQuery(t, driver, cfg, requestID, " with (readpast, readcommittedlock)")
}

func readReconciliationRowsWithQuery(t *testing.T, driver string, cfg postgresConfig, requestID, tableHint string) []reconciliationRow {
	t.Helper()
	db := openIntegrationDB(t, driver, cfg)
	defer db.Close()
	query := fmt.Sprintf("select external_request_id, product_code, quantity, customer_code from %s%s where external_request_id = %s", reconciliationOrdersTable(driver), tableHint, sqlLiteral(requestID))
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s read reconciliation rows: %v", driver, err)
	}
	defer rows.Close()
	var result []reconciliationRow
	for rows.Next() {
		var row reconciliationRow
		if err := rows.Scan(&row.ExternalRequestID, &row.ProductCode, &row.Quantity, &row.CustomerCode); err != nil {
			t.Fatalf("%s scan reconciliation row: %v", driver, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s iterate reconciliation rows: %v", driver, err)
	}
	return result
}

func sqlLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

type mutationReconciliationHTTPProxy struct {
	server *httptest.Server
	target string
	mu     sync.Mutex
	active *mutationReconciliationProxyRequest
}

type mutationReconciliationProxyRequest struct {
	writer          http.ResponseWriter
	cancel          context.CancelFunc
	mode            string
	started         chan struct{}
	responseReady   chan struct{}
	upstreamRelease chan struct{}
	responseRelease chan struct{}
	done            chan struct{}
	closed          atomic.Bool
	closeOnce       sync.Once
}

func newMutationReconciliationHTTPProxy(t *testing.T, target string) *mutationReconciliationHTTPProxy {
	t.Helper()
	proxy := &mutationReconciliationHTTPProxy{target: strings.TrimRight(target, "/")}
	proxy.server = httptest.NewServer(http.HandlerFunc(proxy.handle))
	t.Cleanup(proxy.server.Close)
	return proxy
}

func (p *mutationReconciliationHTTPProxy) URL() string {
	return p.server.URL
}

func (p *mutationReconciliationHTTPProxy) client() *http.Client {
	return &http.Client{Transport: http.DefaultTransport}
}

func (p *mutationReconciliationHTTPProxy) handle(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	request := &mutationReconciliationProxyRequest{
		writer:          w,
		cancel:          cancel,
		mode:            r.Header.Get("X-Onprest-Reconciliation-Proxy"),
		started:         make(chan struct{}),
		responseReady:   make(chan struct{}),
		upstreamRelease: make(chan struct{}),
		responseRelease: make(chan struct{}),
		done:            make(chan struct{}),
	}
	r.Header.Del("X-Onprest-Reconciliation-Proxy")
	p.mu.Lock()
	p.active = request
	p.mu.Unlock()
	close(request.started)
	defer func() {
		cancel()
		p.mu.Lock()
		if p.active == request {
			p.active = nil
		}
		p.mu.Unlock()
		close(request.done)
	}()

	upstreamRequest, err := http.NewRequestWithContext(ctx, r.Method, p.target+r.URL.RequestURI(), r.Body)
	if err != nil {
		return
	}
	upstreamRequest.Header = r.Header.Clone()
	if strings.Contains(request.mode, "hold-upstream") {
		<-request.upstreamRelease
	}
	response, err := http.DefaultTransport.RoundTrip(upstreamRequest)
	if err != nil {
		return
	}
	defer response.Body.Close()
	close(request.responseReady)
	if strings.Contains(request.mode, "hold-response") {
		<-request.responseRelease
	}
	if request.closed.Load() {
		return
	}
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (p *mutationReconciliationHTTPProxy) waitResponseReady(t *testing.T) {
	t.Helper()
	request := p.waitActive(t)
	select {
	case <-request.responseReady:
		return
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for held reconciliation response")
	}
}

func (p *mutationReconciliationHTTPProxy) waitActive(t *testing.T) *mutationReconciliationProxyRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		request := p.active
		p.mu.Unlock()
		if request != nil {
			return request
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for active reconciliation proxy request")
	return nil
}

func (p *mutationReconciliationHTTPProxy) dropActiveClient(t *testing.T) {
	t.Helper()
	request := p.waitActive(t)
	request.closeClient()
	select {
	case <-request.done:
	case <-time.After(15 * time.Second):
		t.Fatal("reconciliation proxy request did not close")
	}
}

func (p *mutationReconciliationHTTPProxy) releaseActiveUpstream(t *testing.T) {
	t.Helper()
	request := p.waitActive(t)
	close(request.upstreamRelease)
}

func (p *mutationReconciliationHTTPProxy) releaseActiveResponse(t *testing.T) {
	t.Helper()
	request := p.waitActive(t)
	close(request.responseRelease)
}

func (r *mutationReconciliationProxyRequest) closeClient() {
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		r.cancel()
		close(r.upstreamRelease)
		close(r.responseRelease)
		if hijacker, ok := r.writer.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	})
}
