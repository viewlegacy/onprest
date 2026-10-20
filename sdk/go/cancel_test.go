package onprest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// pollHost answers every operation as ready with the host cancellation code (-5).
type pollHost struct {
	next     int64
	controls map[int64]*bytes.Reader
	canceled int
}

func (h *pollHost) call(b []byte) int64 {
	var q struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(b, &q) != nil {
		return -1
	}
	v := map[string]any{}
	switch q.Method {
	case "runtime.begin":
		v["operation_handle"] = 7
	case "runtime.poll":
		v["ready"] = true
		v["count"] = -5
	case "runtime.cancel":
		h.canceled++
	default:
		return -7
	}
	raw, _ := json.Marshal(v)
	h.next++
	h.controls[h.next] = bytes.NewReader(raw)
	return h.next
}
func (h *pollHost) read(id int64, b []byte) int32 {
	r := h.controls[id]
	if r == nil {
		return -1
	}
	n, _ := r.Read(b)
	return int32(n)
}
func (*pollHost) write(int64, []byte) int32 { return -1 }
func (h *pollHost) close(id int64) int32    { delete(h.controls, id); return 0 }

// pendingDeadline has a passed deadline whose timer has not run yet, so Err is still nil.
type pendingDeadline struct {
	context.Context
	deadline time.Time
}

func (c pendingDeadline) Deadline() (time.Time, bool) { return c.deadline, true }

func TestHostCancellationAtPassedDeadlineIsDeadlineExceeded(t *testing.T) {
	newExecution := func() (*execution, *pollHost) {
		h := &pollHost{controls: map[int64]*bytes.Reader{}}
		e := testExecution()
		e.host = h
		return e, h
	}
	t.Run("operation", func(t *testing.T) {
		e, _ := newExecution()
		ctx := pendingDeadline{e.ctx, time.Now().Add(-time.Millisecond)}
		_, err := e.businessCall(ctx, "http.open", map[string]any{})
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Fatalf("passed request deadline reported as %v", err)
		}
	})
	t.Run("standard client", func(t *testing.T) {
		e, _ := newExecution()
		ctx := pendingDeadline{e.ctx, time.Now().Add(-time.Millisecond)}
		req, err := http.NewRequestWithContext(ctx, "POST", "https://example.internal/wait", strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := NewHTTPClient().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Fatalf("passed request deadline reported as %v", err)
		}
	})
	t.Run("no deadline", func(t *testing.T) {
		e, _ := newExecution()
		_, err := e.businessCall(e.ctx, "http.open", map[string]any{})
		if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("host cancellation reported as %v", err)
		}
	})
	t.Run("future deadline", func(t *testing.T) {
		e, _ := newExecution()
		ctx := pendingDeadline{e.ctx, time.Now().Add(time.Hour)}
		_, err := e.businessCall(ctx, "http.open", map[string]any{})
		if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("host cancellation before the deadline reported as %v", err)
		}
	})
}
