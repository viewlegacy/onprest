package localdiagnostics

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// DiagnosticRecord is exclusively local. Provider errors, code logs and worker
// stdio must never be reused as public errors or Gateway/Managed events.
type DiagnosticRecord struct {
	Level      string
	Source     string
	Event      string
	Message    string
	Capability string
	RequestID  string
	ErrorCode  string
	Detail     string
}

// DiagnosticSink serializes complete JSON Lines, including level filtering and
// Close, under one lock. It owns only a supplied closer, never ambient stdout.
type DiagnosticSink struct {
	mu       sync.Mutex
	writer   io.Writer
	closer   io.Closer
	level    int
	closed   bool
	closeErr error
}

func Level(s string) (int, bool) {
	switch s {
	case "debug":
		return 0, true
	case "info":
		return 1, true
	case "warn":
		return 2, true
	case "error":
		return 3, true
	}
	return 0, false
}
func NewDiagnosticSink(writer io.Writer, closer io.Closer, level string) (*DiagnosticSink, error) {
	if level == "" {
		level = "info"
	}
	n, ok := Level(level)
	if !ok || writer == nil {
		return nil, errors.New("invalid local diagnostic sink")
	}
	return &DiagnosticSink{writer: writer, closer: closer, level: n}, nil
}

func (s *DiagnosticSink) Write(record DiagnosticRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	level, ok := Level(record.Level)
	if !ok {
		record.Level = "warn"
		record.Event = "invalid_log_level"
		record.Message = "invalid diagnostic log level"
		record.Detail = ""
		level = 2
	}
	if level < s.level {
		return nil
	}
	switch record.Source {
	case "db", "csv", "http", "file", "wasm", "agent":
	default:
		record.Source = "agent"
	}
	m := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "level": record.Level, "source": record.Source, "event": Bounded(record.Event, 256), "message": Bounded(record.Message, 16<<10)}
	if record.Capability != "" {
		m["capability"] = Bounded(record.Capability, 128)
	}
	if record.RequestID != "" {
		m["request_id"] = Bounded(record.RequestID, 256)
	}
	if record.ErrorCode != "" {
		m["error_code"] = Bounded(record.ErrorCode, 128)
	}
	if record.Detail != "" {
		m["detail"] = Bounded(record.Detail, 16<<10)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	n, err := s.writer.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}
func Bounded(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}
func (s *DiagnosticSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		if s.closer != nil {
			s.closeErr = s.closer.Close()
		}
	}
	return s.closeErr
}
