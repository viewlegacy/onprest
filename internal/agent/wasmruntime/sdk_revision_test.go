package wasmruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/contract"
)

func TestSDKRevisionReadDirPartialError(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprint(on), func(t *testing.T) {
			r, m, c, base := sdkSetup(t, on, "read_write")
			for _, name := range []string{"a", "b"} {
				if e := os.WriteFile(filepath.Join(base, name), []byte("x"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			info, e := os.Stat(filepath.Join(base, "a"))
			if e != nil {
				t.Fatal(e)
			}
			raw, e := contract.CanonicalJSON(metadataMap(meta(info)))
			if e != nil {
				t.Fatal(e)
			}
			l := DefaultLimits()
			l.ReadBytes = int64(len(raw))
			l.TotalReadBytes = l.ReadBytes
			v := sdkData(t, r, m, c, map[string]any{"scenario": "read-dir-partial", "path": "."}, l)
			if len(v["names"].([]any)) != 1 || v["names"].([]any)[0] != "a" || v["limit"] != true {
				t.Fatal(v)
			}
			v = sdkData(t, r, m, c, map[string]any{"scenario": "read-dir-partial", "path": "."}, DefaultLimits())
			if len(v["names"].([]any)) != 2 || v["failed"] != false {
				t.Fatal(v)
			}
			if e = os.Remove(filepath.Join(base, "b")); e != nil {
				t.Fatal(e)
			}
			if e = os.Symlink("missing", filepath.Join(base, "z")); e != nil {
				t.Fatal(e)
			}
			v = sdkData(t, r, m, c, map[string]any{"scenario": "read-dir-partial", "path": "."}, DefaultLimits())
			if len(v["names"].([]any)) != 1 || v["failed"] != true || (on && v["permission"] != true) || (!on && v["notfound"] != true) {
				t.Fatal(v)
			}
		})
	}
}

func sdkRevisionRun(t *testing.T, r *Runner, m *Module, c *contract.Capability, p map[string]any, l Limits) (*contract.PreparedOutput, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.Execute(ctx, m, p, c, l)
}
func TestSDKRevisionEnvelopeBoundaryAndPriority(t *testing.T) {
	r, m, c, _ := sdkSetup(t, false, "")
	c.Policy["max_output_bytes"] = int64(40 << 20)
	l := DefaultLimits()
	l.MemoryBytes = 512 << 20
	for _, n := range []int64{EnvelopeLimit - 1, EnvelopeLimit, EnvelopeLimit + 1} {
		out, e := sdkRevisionRun(t, r, m, c, map[string]any{"scenario": "envelope-boundary", "i64": n}, l)
		if n > EnvelopeLimit {
			errorCode(t, e, "LIMIT_EXCEEDED")
			if out != nil {
				t.Fatal("oversize published")
			}
		} else {
			if e != nil {
				t.Fatal(e)
			}
			if len(out.Data.(string)) != int(n)-len(`{"data":"","files":[]}`) {
				t.Fatal("boundary size changed")
			}
			out.Close()
		}
	}
	for _, total := range []bool{false, true} {
		limited := l
		scenario := "files"
		if total {
			scenario = "files-limit-total"
			limited.FileBytes = 4
			limited.TotalFileBytes = 7
		} else {
			limited.FileBytes = 3
		}
		out, e := sdkRevisionRun(t, r, m, c, map[string]any{"scenario": scenario}, limited)
		errorCode(t, e, "LIMIT_EXCEEDED")
		if out != nil || r.Store.Usage() != 0 {
			t.Fatal("returned file limit published or leaked output")
		}
	}
	_, e := sdkRevisionRun(t, r, m, c, map[string]any{"scenario": "large-handler-error", "i64": int64(EnvelopeLimit + 1)}, l)
	errorCode(t, e, "APP_PRIMARY")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e = r.Execute(ctx, m, map[string]any{"scenario": "envelope-boundary", "i64": int64(EnvelopeLimit + 1)}, c, l)
	errorCode(t, e, "EXECUTION_TIMEOUT")
}

type sdkFaultStage struct {
	contract.ArtifactStage
	mode string
}

func (s *sdkFaultStage) Write(b []byte) (int, error) {
	if s.mode == "write" {
		n, e := s.ArtifactStage.Write(b[:min(1, len(b))])
		return n, errors.Join(e, io.ErrUnexpectedEOF)
	}
	return s.ArtifactStage.Write(b)
}
func (s *sdkFaultStage) Close() error {
	e := s.ArtifactStage.Close()
	if s.mode == "close" {
		return errors.Join(e, errors.New("private stage close failure"))
	}
	return e
}

func TestSDKRevisionFinalResultTransportFailure(t *testing.T) {
	for _, mode := range []string{"open-handles", "spool", "open-io", "write", "close"} {
		t.Run(mode, func(t *testing.T) {
			r, m, c, _ := sdkSetup(t, false, "")
			l := DefaultLimits()
			if mode == "open-handles" {
				l.Handles = 2
			}
			if mode == "spool" {
				store, e := artifacts.Open(artifacts.Options{Directory: filepath.Join(t.TempDir(), "spool"), QuotaBytes: 1, MaxPending: 16, MaxObjects: 256, TransferTimeout: time.Minute})
				if e != nil {
					t.Fatal(e)
				}
				r.Store = store
				t.Cleanup(func() {
					if store.Usage() != 0 {
						t.Error("private spool leaked")
					}
					store.Close()
				})
			}
			if mode == "open-io" || mode == "write" || mode == "close" {
				r.stageFactory = func(ctx context.Context, name, media string, size int64) (contract.ArtifactStage, error) {
					if mode == "open-io" {
						return nil, errors.New("private stage open failure")
					}
					stage, e := r.Store.NewScratch(ctx, name, media, size)
					if e != nil {
						return nil, e
					}
					return &sdkFaultStage{stage, mode}, nil
				}
			}
			_, e := sdkRevisionRun(t, r, m, c, map[string]any{"scenario": "data"}, l)
			want := "OUTPUT_INVALID"
			if mode == "open-handles" || mode == "spool" {
				want = "LIMIT_EXCEEDED"
			}
			errorCode(t, e, want)
		})
	}
	// The raw real-Wasm fixture holds ACK controls until result.commit cannot
	// allocate. It exits normally, so the host transport classification survives.
	r, m, c, _ := bridgeSetup(t, false, "")
	l := DefaultLimits()
	l.Handles = 3
	_, e := sdkRevisionRun(t, r, m, c, map[string]any{"scenario": "review_commit", "size": 2}, l)
	errorCode(t, e, "LIMIT_EXCEEDED")
}

func TestSDKRevisionTimeoutOverridesResultTransportFailure(t *testing.T) {
	r, m, c, _ := sdkSetup(t, false, "")
	store, e := artifacts.Open(artifacts.Options{Directory: filepath.Join(t.TempDir(), "spool"), QuotaBytes: 1, MaxPending: 16, MaxObjects: 256, TransferTimeout: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	r.Store = store
	t.Cleanup(func() {
		if store.Usage() != 0 {
			t.Error("timeout leaked spool")
		}
		store.Close()
	})
	var mu sync.Mutex
	var diagnostic string
	r.Diagnostic = func(level, event, message, detail string) { mu.Lock(); diagnostic += message + detail; mu.Unlock() }
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, e = r.Execute(ctx, m, map[string]any{"scenario": "result-timeout"}, c, DefaultLimits())
	errorCode(t, e, "EXECUTION_TIMEOUT")
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(diagnostic, "Wasm result: Onprest limit exceeded") {
		t.Fatal("result transport limit was not reached before timeout", diagnostic)
	}
}
