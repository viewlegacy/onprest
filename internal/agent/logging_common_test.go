package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func diagnosticRecords(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(b), []byte{'\n'})
	out := []map[string]any{}
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if e := json.Unmarshal(line, &m); e != nil {
			t.Fatal("broken JSONL", string(line), e)
		}
		out = append(out, m)
	}
	return out
}

func TestCommonDiagnosticRecordLevelsContextAndControlEscaping(t *testing.T) {
	for _, threshold := range []string{"debug", "info", "warn", "error"} {
		t.Run(threshold, func(t *testing.T) {
			var b bytes.Buffer
			s, e := NewDiagnosticSink(&b, nil, threshold)
			if e != nil {
				t.Fatal(e)
			}
			for _, level := range []string{"debug", "info", "warn", "error"} {
				if e := s.Write(DiagnosticRecord{Level: level, Source: "db", Event: "agent_error", Message: "line\n\u001b\"quoted\"", ErrorCode: "EXECUTION_FAILED", Detail: "private"}); e != nil {
					t.Fatal(e)
				}
			}
			records := diagnosticRecords(t, b.Bytes())
			n, _ := diagnosticLevel(threshold)
			if len(records) != 4-n {
				t.Fatal(records)
			}
			for _, m := range records {
				if m["source"] != "db" || m["event"] != "agent_error" || m["message"] != "line\n\u001b\"quoted\"" || m["error_code"] != "EXECUTION_FAILED" || m["detail"] != "private" {
					t.Fatal(m)
				}
				if _, ok := m["capability"]; ok {
					t.Fatal("invented capability")
				}
				if _, ok := m["request_id"]; ok {
					t.Fatal("invented request ID")
				}
				ts, e := time.Parse(time.RFC3339Nano, m["ts"].(string))
				if e != nil || ts.Location() != time.UTC {
					t.Fatal("not UTC", m)
				}
			}
		})
	}
	var b bytes.Buffer
	s, _ := NewDiagnosticSink(&b, nil, "")
	if e := s.Write(DiagnosticRecord{Level: "PRIVATE", Source: "http", Event: "injected", Message: "secret invalid message", Detail: "secret detail", Capability: "read", RequestID: "request"}); e != nil {
		t.Fatal(e)
	}
	m := diagnosticRecords(t, b.Bytes())[0]
	if m["level"] != "warn" || m["event"] != "invalid_log_level" || m["message"] != "invalid diagnostic log level" || m["detail"] != nil || m["capability"] != "read" || m["request_id"] != "request" {
		t.Fatal(m)
	}
	if _, e := NewDiagnosticSink(&b, nil, "PRIVATE"); e == nil {
		t.Fatal("invalid threshold accepted")
	}
}

func TestCommonDiagnosticConcurrentBoundedJSONLinesAndClose(t *testing.T) {
	var b bytes.Buffer
	closer := &diagnosticCloser{}
	s, _ := NewDiagnosticSink(&b, closer, "info")
	var wg sync.WaitGroup
	message := strings.Repeat("日本語", 7000) + string([]byte{0xff})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := s.Write(DiagnosticRecord{Level: "info", Source: "wasm", Event: "wasm_log", Message: message, Capability: "run", RequestID: "request"}); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	rows := diagnosticRecords(t, b.Bytes())
	if len(rows) != 32 {
		t.Fatal(len(rows))
	}
	for _, m := range rows {
		v := m["message"].(string)
		if len(v) > 16<<10 || !utf8.ValidString(v) {
			t.Fatal("unbounded/invalid record")
		}
	}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := s.Close(); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if closer.calls.Load() != 1 {
		t.Fatal("closer not once")
	}
	if e := s.Write(DiagnosticRecord{Level: "error"}); !errors.Is(e, io.ErrClosedPipe) {
		t.Fatal("closed sink reopened", e)
	}
	if got := boundedDiagnostic(string([]byte{0xff})+"日本語", 16); !utf8.ValidString(got) || !strings.Contains(got, "\uFFFD") {
		t.Fatal("invalid UTF-8 not replaced", got)
	}
}

func TestCommonDiagnosticWriterFaultsAndRotation(t *testing.T) {
	for _, fault := range []struct {
		name string
		w    io.Writer
		want error
	}{{"short", diagnosticFaultWriter{}, io.ErrShortWrite}, {"error", diagnosticFaultWriter{err: io.ErrClosedPipe}, io.ErrClosedPipe}} {
		t.Run(fault.name, func(t *testing.T) {
			s, _ := NewDiagnosticSink(fault.w, nil, "info")
			if e := s.Write(DiagnosticRecord{Level: "error", Source: "agent", Event: "agent_error", Message: "error"}); !errors.Is(e, fault.want) {
				t.Fatal(e)
			}
		})
	}
	closer := &diagnosticCloser{err: io.ErrClosedPipe}
	var b bytes.Buffer
	s, _ := NewDiagnosticSink(&b, closer, "info")
	if e := s.Close(); !errors.Is(e, io.ErrClosedPipe) {
		t.Fatal(e)
	}
	if e := s.Close(); !errors.Is(e, io.ErrClosedPipe) || closer.calls.Load() != 1 {
		t.Fatal(e, closer.calls.Load())
	}
	path := filepath.Join(t.TempDir(), "agent.log")
	s, e := NewRotatingDiagnosticSink(path, 400, 2, "debug")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 12; i++ {
		if e := s.Write(DiagnosticRecord{Level: "info", Source: "csv", Event: "agent_error", Message: strings.Repeat("m", 100), Capability: "read", RequestID: "request"}); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	for _, suffix := range []string{"", ".1", ".2"} {
		b, e := os.ReadFile(path + suffix)
		if e != nil {
			t.Fatal(e)
		}
		if len(b) > 400 {
			t.Fatal("rotation size not enforced")
		}
		if len(diagnosticRecords(t, b)) != 1 {
			t.Fatal("record split across generations")
		}
	}
	if _, e := os.Stat(path + ".3"); !os.IsNotExist(e) {
		t.Fatal("rotation count exceeded", e)
	}
	if e := s.Write(DiagnosticRecord{Level: "error"}); !errors.Is(e, io.ErrClosedPipe) {
		t.Fatal("rotating sink reopened after Close", e)
	}
}

type diagnosticCloser struct {
	calls atomic.Int32
	err   error
}

func (c *diagnosticCloser) Close() error { c.calls.Add(1); return c.err }

type diagnosticFaultWriter struct{ err error }

func (w diagnosticFaultWriter) Write(b []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	return len(b) - 1, nil
}
