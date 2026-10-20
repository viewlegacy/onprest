//go:build linux

package wasmruntime

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

func TestWasmRealWorkerKernelCPULimitIsLimitExceeded(t *testing.T) {
	r, m, c := setup(t)
	for _, kind := range []string{"rlimit", "external"} {
		t.Run(kind, func(t *testing.T) {
			copy := *r
			var once sync.Once
			var pid atomic.Int64
			var setupError error
			copy.sample = func(p *process.Process) (uint64, float64, error) {
				once.Do(func() {
					pid.Store(int64(p.Pid))
					if kind == "rlimit" {
						setupError = unix.Prlimit(int(p.Pid), unix.RLIMIT_CPU, &unix.Rlimit{Cur: 1, Max: 1}, nil)
					}
				})
				return 1, 0, nil
			}
			copy.cpuOverride = 1
			if kind == "external" {
				copy.cpuOverride = 30
				copy.Diagnostic = func(_, event, message, _ string) {
					if event == "wasm_stderr" && message == "worker-entered" {
						unix.Kill(int(pid.Load()), unix.SIGKILL)
					}
				}
			}
			started := time.Now()
			out, e := run(t, &copy, m, c, map[string]any{"scenario": "loop"}, 30*time.Second)
			if out != nil {
				out.Close()
				t.Fatal("killed worker published output")
			}
			if setupError != nil {
				t.Fatal(setupError)
			}
			if kind == "rlimit" {
				errorCode(t, e, "LIMIT_EXCEEDED")
			} else {
				errorCode(t, e, "EXECUTION_FAILED")
			}
			if time.Since(started) > 20*time.Second || r.Store.Usage() != 0 {
				t.Fatal("worker was not stopped before the wall deadline or leaked spool")
			}
		})
	}
}
