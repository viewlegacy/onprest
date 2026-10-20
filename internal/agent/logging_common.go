package agent

import (
	"errors"
	"github.com/viewlegacy/onprest/internal/agent/localdiagnostics"
	"io"
)

type DiagnosticRecord = localdiagnostics.DiagnosticRecord
type DiagnosticSink = localdiagnostics.DiagnosticSink

func NewDiagnosticSink(w io.Writer, c io.Closer, l string) (*DiagnosticSink, error) {
	return localdiagnostics.NewDiagnosticSink(w, c, l)
}
func diagnosticLevel(s string) (int, bool)     { return localdiagnostics.Level(s) }
func boundedDiagnostic(s string, n int) string { return localdiagnostics.Bounded(s, n) }

// NewRotatingDiagnosticSink shares the runtime's existing rotation policy. It
// is used only when an owner explicitly constructs the new internal pipeline.
func NewRotatingDiagnosticSink(path string, maxSize int64, maxFiles int, level string) (*DiagnosticSink, error) {
	if _, ok := diagnosticLevel(level); !ok {
		return nil, errors.New("invalid log level")
	}
	w, err := newRotatingFileWriter(path, maxSize, maxFiles)
	if err != nil {
		return nil, err
	}
	s, err := NewDiagnosticSink(w, w, level)
	if err != nil {
		_ = w.Close()
	}
	return s, err
}
