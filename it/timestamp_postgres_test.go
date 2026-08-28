//go:build integration

package it

import (
	"context"
	"net/http"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
)

func TestContainerDBDriverPostgresTimestampResultPreservesJSONContract(t *testing.T) {
	if !selectedDBForTest(t, "postgres") {
		return
	}
	cfg := selectedContainerDBConfig(t, "postgres")
	secrets := newITSecrets(t)
	addr := freeAddr(t)
	capabilityFile := writePostgresCapability(t, t.TempDir(), cfg, "ws://"+addr+"/ws/agent", secrets.AgentPrivateKey, `  timestamp_result:
    sql: |
      select
        timestamp '2026-03-15 17:45:47.123456' as timestamp_without_time_zone,
        timestamp with time zone '2026-03-15 17:45:47.123456+09:00' as timestamp_with_time_zone,
        null::timestamp as nullable_timestamp_without_time_zone,
        cast(null as timestamp with time zone) as nullable_timestamp_with_time_zone
    policy:
      readonly: true
      timeout: 2s
      max_rows: 1
      max_bytes: 128KB
    result:
      timestamp_without_time_zone: {type: string}
      timestamp_with_time_zone: {type: string}
      nullable_timestamp_without_time_zone: {type: string}
      nullable_timestamp_with_time_zone: {type: string}`)

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

	status, body := postCapability(t, baseURL, secrets.APIKey, "timestamp_result", `{}`)
	const want = `{"count":1,"rows":[{"nullable_timestamp_with_time_zone":null,"nullable_timestamp_without_time_zone":null,"timestamp_with_time_zone":"2026-03-15 08:45:47.123456 +0000 UTC","timestamp_without_time_zone":"2026-03-15 17:45:47.123456 +0000 +0000"}]}`
	if status != http.StatusOK || string(body) != want {
		t.Fatalf("timestamp_result status=%d body=%s, want exact body=%s", status, body, want)
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(12 * time.Second):
		t.Fatal("agent runner did not stop")
	}
}
