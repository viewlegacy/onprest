package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
)

func TestCSVResultMetricsRequireMeasuredRowsAndPreserveOtherExecutors(t *testing.T) {
	count := int64(1)
	negative := int64(-1)
	zero := int64(0)
	row := map[string]any{"rows": []any{map[string]any{"id": "00123"}}, "count": int64(1)}
	cases := []struct {
		name, kind         string
		data               any
		returned, affected *int64
		bad                bool
	}{
		{"rows", "csv", row, &count, nil, false},
		{"files only", "csv", nil, &count, nil, false},
		{"zero", "csv", map[string]any{"rows": []any{}, "count": int64(0)}, &zero, nil, false},
		{"missing", "csv", row, nil, nil, true},
		{"affected", "csv", nil, nil, &count, true},
		{"both", "csv", row, &count, &count, true},
		{"negative", "csv", nil, &negative, nil, true},
		{"mismatch count", "csv", row, &zero, nil, true},
		{"mismatch payload count", "csv", map[string]any{"rows": []any{map[string]any{}}, "count": int64(0)}, &count, nil, true},
		{"malformed shape", "csv", []any{}, &zero, nil, true},
		{"file rows", "file", nil, &zero, nil, true},
		{"HTTP affected", "http", nil, nil, &count, true},
		{"Wasm rows", "wasm", nil, &zero, nil, true},
		{"other metrics absent", "http", row, nil, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, e := contract.CanonicalJSON(c.data)
			if e != nil {
				t.Fatal(e)
			}
			resp := protocol.ResultResponse("id", contract.ResultValue{Data: c.data, Files: []contract.PublicFileDescriptor{}})
			resp.Metrics = &protocol.Metrics{ExecutorKind: c.kind, ReturnedRows: c.returned, AffectedRows: c.affected, ResponseBytes: int64(len(body))}
			meta := protocol.CapabilityMetadata{ExecutorKind: c.kind, EffectClass: "none"}
			b, m, e := validateAgentResult("id", resp, meta, true)
			if (e != nil) != c.bad {
				t.Fatal(string(b), m, e)
			}
			if !c.bad && m.ReturnedRows != c.returned {
				t.Fatal(m)
			}
		})
	}
}

func TestInputResponseCountAndExecutionLog(t *testing.T) {
	for _, tc := range []struct {
		name, kind, effect string
		returned, affected *int64
		failure            bool
	}{
		{"CSV rows", "csv", "none", int64Ptr(1), nil, false},
		{"CSV zero", "csv", "none", int64Ptr(0), nil, false},
		{"SELECT rows", "db", "none", int64Ptr(1), nil, false},
		{"SELECT zero", "db", "none", int64Ptr(0), nil, false},
		{"DML affected", "db", "possible", nil, int64Ptr(3), false},
		{"DML zero", "db", "possible", nil, int64Ptr(0), false},
		{"DML int64", "db", "possible", nil, int64Ptr(9223372036854775807), false},
		{"HTTP", "http", "none", nil, nil, false},
		{"file", "file", "none", nil, nil, false},
		{"Wasm", "wasm", "none", nil, nil, false},
		{"CSV failure", "csv", "none", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			ac := &agentConn{}
			s := &Server{agent: ac, logOut: &logs}
			ctx := context.WithValue(context.Background(), callIdentityKey{}, callIdentity{ID: "id"})
			meta := protocol.CapabilityMetadata{ExecutorKind: tc.kind, EffectClass: tc.effect}
			// An omitted public data output must retain the measured counter too.
			response := protocol.ResultResponse("id", contract.ResultValue{Files: []contract.PublicFileDescriptor{}})
			response.Metrics = &protocol.Metrics{ExecutorKind: tc.kind, ReturnedRows: tc.returned, AffectedRows: tc.affected, ResponseBytes: 4}
			status := http.StatusOK
			if tc.failure {
				response = protocol.Response{ID: "id", Error: &contract.ErrorValue{Code: "EXECUTION_FAILED", Message: "failed", Outcome: "no_change"}}
				status = http.StatusBadGateway
			}
			result := s.inputResponse(ctx, ac, "cap", meta, response)
			if result.Status != status {
				t.Fatalf("result: %+v", result)
			}
			s.accessExecutionLog("rest", "id", "key", "cap", result, time.Now())
			var event map[string]any
			decoder := json.NewDecoder(&logs)
			decoder.UseNumber()
			if err := decoder.Decode(&event); err != nil {
				t.Fatal(err)
			}
			want := tc.returned
			metric := "returned_rows"
			if want == nil {
				want, metric = tc.affected, "affected_rows"
			}
			if _, present := event["count"]; !present {
				t.Fatal("missing count", event)
			}
			if want == nil {
				if result.Count != nil || event["count"] != nil {
					t.Fatal("unmeasured count must stay null", result.Count, event)
				}
			} else if result.Count == nil || *result.Count != *want || event["count"] != json.Number(fmt.Sprint(*want)) || event[metric] != event["count"] {
				t.Fatal("count must match measured rows", result.Count, event)
			}
		})
	}
}
