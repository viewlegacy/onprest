package wasmruntime

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// Parser decisions do not require compiling and launching a Wasm worker for
// every value. The real-worker and REST/MCP tests retain the boundary wiring.
func TestWasmTerminalEnvelopeValidation(t *testing.T) {
	public := func(code, message string) string {
		// These callers use only JSON-safe ASCII strings.
		return `{"error":{"kind":"public","code":"` + code + `","message":"` + message + `"}}`
	}
	for _, tc := range []struct{ raw, code string }{
		{`{"error":{"kind":"internal","category":"execution"}}`, "EXECUTION_FAILED"},
		{`{"error":{"kind":"internal","category":"output"}}`, "OUTPUT_INVALID"},
		{`{"error":{"kind":"internal","category":"limit"}}`, "LIMIT_EXCEEDED"},
		{`{"error":{"kind":"internal","category":"bogus"}}`, "OUTPUT_INVALID"},
		{public("APP_DOCUMENT_INVALID", "safe"), "APP_DOCUMENT_INVALID"},
		{public("APP_A", "a"), "APP_A"},
		{public("APP_A", strings.Repeat("a", 1024)), "APP_A"},
		{public("APP_A", ""), "EXECUTION_FAILED"},
		{public("APP_A", strings.Repeat("a", 1025)), "EXECUTION_FAILED"},
		{public("APP_lower", "private"), "EXECUTION_FAILED"},
		{public("EXECUTION_FAILED", "private"), "EXECUTION_FAILED"},
		{`{"error":{"kind":"public","code":"APP_A","message":"unsafe\n"}}`, "EXECUTION_FAILED"},
		{`{"data":null,"files":[],"error":{}}`, "OUTPUT_INVALID"},
		{`{"data":1,"data":2,"files":[]}`, "OUTPUT_INVALID"},
		{`{"data":"\ud800","files":[]}`, "OUTPUT_INVALID"},
	} {
		s := newSession(context.Background(), nil, DefaultLimits(), &diagnostics{})
		err := s.envelope(&handle{reader: io.NopCloser(strings.NewReader(tc.raw))})
		if err == nil {
			err = s.terminalError
		}
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != tc.code {
			t.Fatalf("%s: %v, want %s", tc.raw, err, tc.code)
		}
		if !strings.HasPrefix(tc.code, "APP_") && failure.Message != "" {
			t.Fatal("private detail became a public message", failure)
		}
	}
}
