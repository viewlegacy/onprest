package gateway

import (
	"testing"

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
