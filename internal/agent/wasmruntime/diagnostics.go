package wasmruntime

import (
	"bytes"
	"strings"
	"sync"
	"unicode/utf8"
)

// Diagnostic receives bounded local records; the owner supplies the existing
// level-filtered sink. User text is never interpreted as a result or event.
type Diagnostic func(level, event, message, detail string)
type diagnostics struct {
	mu             sync.Mutex
	emit           Diagnostic
	raw            int
	truncated      bool
	stdout, stderr []byte
	tail           []byte
}

func (d *diagnostics) record(level, event, message, detail string) {
	if d.emit != nil {
		d.emit(level, event, message, detail)
	}
}
func utf8Cut(b []byte, n int) int {
	if n >= len(b) {
		return len(b)
	}
	for n > 0 && !utf8.RuneStart(b[n]) {
		n--
	}
	if n == 0 {
		return 1
	}
	return n
}
func (d *diagnostics) reserve(n int) int {
	left := (1 << 20) - d.raw
	if left < 0 {
		left = 0
	}
	accepted := n
	if accepted > left {
		accepted = left
	}
	d.raw += accepted
	if accepted < n && !d.truncated {
		d.truncated = true
		d.record("warn", "log_truncated", "Wasm diagnostics exceeded the raw byte budget", "")
	}
	return accepted
}
func (d *diagnostics) stream(stderr bool, b []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if stderr {
		d.tail = append(d.tail, b...)
		if len(d.tail) > Chunk {
			d.tail = append([]byte(nil), d.tail[len(d.tail)-Chunk:]...)
		}
	}
	n := d.reserve(len(b))
	b = b[:n]
	buf := &d.stdout
	level, event := "info", "wasm_stdout"
	if stderr {
		buf = &d.stderr
		level, event = "warn", "wasm_stderr"
	}
	*buf = append(*buf, b...)
	d.flush(buf, level, event, false)
}
func (d *diagnostics) flush(buf *[]byte, level, event string, final bool) {
	for len(*buf) > 0 {
		n := bytes.IndexByte(*buf, '\n')
		consume := n + 1
		if n < 0 {
			if len(*buf) < 16<<10 && !final {
				return
			}
			n = len(*buf)
			consume = n
			if !final {
				n = completeUTF8Prefix(*buf, n)
				consume = n
				if n == 0 {
					return
				}
			}
		}
		if n > 16<<10 {
			n = utf8Cut(*buf, 16<<10)
			consume = n
		}
		message := strings.ToValidUTF8(string((*buf)[:n]), "\uFFFD")
		for len(message) > 16<<10 {
			cut := utf8Cut([]byte(message), 16<<10)
			d.record(level, event, message[:cut], "")
			message = message[cut:]
		}
		d.record(level, event, message, "")
		*buf = append((*buf)[:0], (*buf)[consume:]...)
	}
}
func (d *diagnostics) log(level, message string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.reserve(len(message))
	if n == 0 && len(message) > 0 {
		return
	}
	if level != "debug" && level != "info" && level != "warn" && level != "error" {
		d.record("warn", "invalid_log_level", "invalid diagnostic log level", "")
		return
	}
	b := []byte(message[:n])
	d.flush(&b, level, "wasm_log", true)
}
func (d *diagnostics) finish(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flush(&d.stdout, "info", "wasm_stdout", true)
	d.flush(&d.stderr, "warn", "wasm_stderr", true)
	if reason == "" {
		return
	}
	reason = strings.ToValidUTF8(reason, "\uFFFD")
	if len(reason) > 16<<10 {
		reason = reason[:utf8Cut([]byte(reason), 16<<10)]
	}
	d.record("error", "wasm_failed", "Wasm worker failed", reason)
	tail := strings.ToValidUTF8(string(d.tail), "\uFFFD")
	for len(tail) > 0 {
		n := utf8Cut([]byte(tail), 16<<10)
		d.record("error", "wasm_stderr_tail", "Wasm worker stderr tail", tail[:n])
		tail = tail[n:]
	}
}

type streamWriter struct {
	diagnostics *diagnostics
	stderr      bool
}

func (w streamWriter) Write(b []byte) (int, error) {
	w.diagnostics.stream(w.stderr, b)
	return len(b), nil
}

// Hold a valid but incomplete trailing rune until the next pipe read. Invalid
// bytes are complete RuneError tokens and can still be drained immediately.
func completeUTF8Prefix(b []byte, n int) int {
	if n == 0 {
		return 0
	}
	start := n - 1
	for start > 0 && !utf8.RuneStart(b[start]) {
		start--
	}
	if !utf8.FullRune(b[start:n]) {
		return start
	}
	return n
}
