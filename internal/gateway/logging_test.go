package gateway

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentLogsAreWrittenOneRecordAtATime(t *testing.T) {
	w := &overlapDetectWriter{}
	s := NewServer(Config{}, w)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			s.log("concurrent", map[string]any{"value": value})
		}(i)
	}
	wg.Wait()
	if w.overlap.Load() {
		t.Fatal("log writer was called concurrently")
	}
	if got := w.writes.Load(); got != 32 {
		t.Fatalf("log writes = %d, want 32", got)
	}
}

type overlapDetectWriter struct {
	active  atomic.Bool
	overlap atomic.Bool
	writes  atomic.Int32
}

func (w *overlapDetectWriter) Write(p []byte) (int, error) {
	if !w.active.CompareAndSwap(false, true) {
		w.overlap.Store(true)
	}
	defer w.active.Store(false)
	time.Sleep(time.Millisecond)
	w.writes.Add(1)
	return len(p), nil
}
