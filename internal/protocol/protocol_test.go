package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentVerifyAuthMessageUsesDedicatedDomainAndFields(t *testing.T) {
	got := string(AgentVerifyAuthMessage("/ws/agent/verify", "2026-09-12T00:00:00Z", "nonce", "challenge"))
	want := "onprest-agent-verify-v1\n/ws/agent/verify\n2026-09-12T00:00:00Z\nnonce\nchallenge"
	if got != want {
		t.Fatalf("message=%q, want %q", got, want)
	}
	if string(AgentAuthMessage("/ws/agent", "2026-09-12T00:00:00Z", "nonce", "challenge", "handshake")) == got {
		t.Fatal("verify and WebSocket authentication domains were reused")
	}
}

func TestVersionedRequestRejectsLegacyAndInvalidJSON(t *testing.T) {
	valid := `{"type":"request","id":"r","capability":"c","params":{"id":9007199254740993},"deadline":"2026-10-03T00:00:00Z","definition_hash":"hash","caller_binding":"caller"}`
	var req Request
	if err := json.Unmarshal([]byte(valid), &req); err != nil || req.Params["id"] != int64(9007199254740993) {
		t.Fatalf("request=%+v err=%v", req, err)
	}
	for _, raw := range []string{
		`{"id":"r","capability":"c","params":{}}`,
		strings.Replace(valid, `"type":"request",`, "", 1),
		strings.Replace(valid, `"id":"r"`, `"id":"r","id":"s"`, 1),
		strings.Replace(valid, `9007199254740993`, `9223372036854775808`, 1),
		strings.Replace(valid, `"params":{"id":9007199254740993}`, `"params":null`, 1),
		strings.Replace(valid, `"caller_binding":"caller"`, `"unexpected":true`, 1),
	} {
		if err := json.Unmarshal([]byte(raw), &req); err == nil {
			t.Fatalf("accepted invalid request %s", raw)
		}
	}
}

func TestVersionedResponseRejectsInvalidEnvelopeAndPreservesPrecision(t *testing.T) {
	count := int64(9007199254740993)
	out := ResultResponse("r", map[string]any{"data": map[string]any{"count": count}, "files": []any{}})
	out.Metrics = &Metrics{ExecutorKind: "db", AffectedRows: &count, ResponseBytes: 26}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err = json.Unmarshal(raw, &resp); err != nil || *resp.Metrics.AffectedRows != count {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	for _, raw := range []string{
		`{"id":"r","result":{"count":1}}`,
		`{"type":"result","id":"r","data":null,"files":[],"metrics":{},"error":{}}`,
		`{"type":"result","id":"r","data":null,"files":[],"metrics":{},"id":"s"}`,
		`{"type":"error","id":"r","error":{"code":"OLD_ERROR","message":"unsafe","outcome":"unknown"}}`,
		`{"type":"error","id":"r","error":{"code":"OUTPUT_INVALID","message":"safe"}}`,
	} {
		if err := json.Unmarshal([]byte(raw), &resp); err == nil {
			t.Fatalf("accepted invalid response %s", raw)
		}
	}
}

func TestVersionedResponseRejectsUnknownNestedFixedFields(t *testing.T) {
	result := `{"type":"result","id":"r","data":null,"files":[],"metrics":{"executor_kind":"db","affected_rows":1,"file_count":0,"response_bytes":4,"input_file_count":0,"input_file_bytes":0}}`
	failure := `{"type":"error","id":"r","error":{"code":"VALIDATION_FAILED","message":"safe","outcome":"not_started"}}`
	metadata := `{"type":"metadata","id":"r","metadata":{"data":{"openapi":"3.1.0"},"capabilities":{"c":{"definition_hash":"hash","executor_kind":"db","execution_timeout":"1s","effect_class":"none","input_schema":{"type":"object"},"input_limits":{}}},"response_kinds":{"c":"select"}}}`
	for _, raw := range []string{result, failure, metadata} {
		var response Response
		if err := json.Unmarshal([]byte(raw), &response); err != nil {
			t.Fatal(err)
		}
	}
	for name, raw := range map[string]string{
		"metrics_unknown":    strings.Replace(result, `"executor_kind":"db"`, `"executor_kind":"db","future_metric":1`, 1),
		"error_unknown":      strings.Replace(failure, `"message":"safe"`, `"message":"safe","detail":"private"`, 1),
		"metadata_unknown":   strings.Replace(metadata, `"data":{`, `"unknown":1,"data":{`, 1),
		"capability_unknown": strings.Replace(metadata, `"effect_class":"none"`, `"effect_class":"none","unknown":1`, 1),
		"metrics_duplicate":  strings.Replace(result, `"affected_rows":1`, `"affected_rows":1,"affected_rows":2`, 1),
		"error_duplicate":    strings.Replace(failure, `"message":"safe"`, `"message":"safe","message":"other"`, 1),
		"metrics_overflow":   strings.Replace(result, `"affected_rows":1`, `"affected_rows":9223372036854775808`, 1),
		"metrics_fraction":   strings.Replace(result, `"affected_rows":1`, `"affected_rows":1.5`, 1),
		"metrics_malformed":  strings.Replace(result, `"file_count":0`, `"file_count":{}`, 1),
		"error_malformed":    strings.Replace(failure, `"message":"safe"`, `"message":{}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			var response Response
			if err := json.Unmarshal([]byte(raw), &response); err == nil {
				t.Fatal("accepted malformed fixed frame")
			}
		})
	}
}
