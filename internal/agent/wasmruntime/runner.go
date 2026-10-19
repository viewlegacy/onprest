package wasmruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/buildinfo"
	"github.com/viewlegacy/onprest/internal/contract"
)

type Observation struct {
	PeakRSS         uint64
	CPUSeconds      float64
	CompileDuration time.Duration
}
type Runner struct {
	Binary       string
	Store        *artifacts.Store
	Cache        *Cache
	Diagnostic   Diagnostic
	Observe      func(Observation)
	AgentVersion string
	Bridge       *Bridge
	// Package-private probes used by real-process resource and result transport tests.
	stageFactory func(context.Context, string, string, int64) (contract.ArtifactStage, error)
	sample       func(*process.Process) (uint64, float64, error)
	rssOverride  uint64
	cpuOverride  float64
}

func (r *Runner) version() string {
	if r.AgentVersion != "" {
		return r.AgentVersion
	}
	return buildinfo.Current()
}
func (r *Runner) validate(ctx context.Context, m *Module, l Limits) (Metadata, error) {
	metadata, e := r.process(ctx, m, nil, l, nil, true)
	if e != nil {
		return Metadata{}, e
	}
	if metadata == nil {
		return Metadata{}, errors.New("worker returned no validation metadata")
	}
	return *metadata, nil
}

// Execute waits for normal worker exit, then performs common publication checks.
// Returned output owns independent stages and must be closed by its consumer.
func (r *Runner) Execute(ctx context.Context, m *Module, params map[string]any, c *contract.Capability, l Limits) (out *contract.PreparedOutput, err error) {
	return r.ExecutePublication(ctx, m, params, c, l, func(v contract.ProviderResult) (*contract.PreparedOutput, error) {
		return contract.ProcessOutput(ctx, c, v, r.Store.Scratch())
	})
}
func (r *Runner) ExecutePublication(ctx context.Context, m *Module, params map[string]any, c *contract.Capability, l Limits, publish func(contract.ProviderResult) (*contract.PreparedOutput, error)) (out *contract.PreparedOutput, err error) {
	if m == nil || r.Store == nil || c == nil {
		return nil, failure("EXECUTION_FAILED", "invalid execution material")
	}
	b, e := contract.CanonicalJSON(params)
	if e != nil || len(b) > 1<<20 {
		return nil, failure("EXECUTION_FAILED", "invalid normalized params")
	}
	d := &diagnostics{emit: r.Diagnostic}
	var cleanupErr error
	defer func() {
		reason := ""
		if err != nil {
			reason = diagnosticReason(err)
		}
		if cleanupErr != nil {
			reason += "; cleanup: " + cleanupErr.Error()
		}
		d.finish(reason)
	}()
	s := newSession(ctx, r.Store, l, d)
	if r.stageFactory != nil {
		s.createStage = r.stageFactory
	}
	s.bridge = r.Bridge
	s.roots = map[string]*filesystem.Root{}
	defer func() {
		cleanupErr = s.cleanup()
		if cleanupErr != nil {
			if out != nil {
				_ = out.Close()
				out = nil
			}
			if err == nil {
				err = failure("EXECUTION_FAILED", cleanupErr.Error())
			}
		}
	}()
	_, e = r.process(ctx, m, b, l, s, false)
	if e != nil {
		return nil, e
	}
	if s.resultFailure != nil {
		return nil, s.resultFailure
	}
	// A refused terminal envelope records why it was refused; that decision
	// wins over the generic invalid-state result even if the worker exits 0.
	// A committed terminal followed by a protocol violation stays OUTPUT_INVALID.
	if s.rejectedEnvelope != nil && !s.committed {
		return nil, s.rejectedEnvelope
	}
	if s.failed || s.asyncFailure.Load() {
		return nil, failure("OUTPUT_INVALID", "invalid result state")
	}
	if !s.committed {
		return nil, failure("OUTPUT_INVALID", "worker exited without result commit")
	}
	if s.terminalError != nil {
		return nil, s.terminalError
	}
	return publish(s.value)
}
func (r *Runner) process(ctx context.Context, m *Module, params []byte, l Limits, s *session, validate bool) (metadata *Metadata, resultErr error) {
	defer func() {
		if resultErr != nil && ctx.Err() != nil {
			resultErr = failure("EXECUTION_TIMEOUT", ctx.Err().Error())
		}
	}()
	productRSSLimit, e := l.rssLimit()
	if e != nil {
		return nil, e
	}
	// The test probe lowers only the sampled RSS threshold; OS restrictions keep the product ceiling.
	rssLimit := productRSSLimit
	if r.rssOverride > 0 {
		rssLimit = r.rssOverride
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("worker requires a fixed execution deadline")
	}
	if e = ctx.Err(); e != nil {
		return nil, failure("EXECUTION_TIMEOUT", e.Error())
	}
	if m == nil || len(m.bytes) == 0 || int64(len(m.bytes)) > l.ModuleBytes {
		return nil, failure("EXECUTION_FAILED", "invalid module bytes")
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	childIn, parentOut, e := os.Pipe()
	if e != nil {
		return nil, e
	}
	defer childIn.Close()
	defer parentOut.Close()
	parentIn, childOut, e := os.Pipe()
	if e != nil {
		return nil, e
	}
	defer parentIn.Close()
	defer childOut.Close()
	binary := r.Binary
	if binary == "" {
		binary, e = os.Executable()
		if e != nil {
			return nil, e
		}
	}
	cmd := exec.CommandContext(childCtx, binary)
	if e = attachPipes(cmd, childIn, childOut); e != nil {
		return nil, failure("DEPENDENCY_UNAVAILABLE", e.Error())
	}
	cmd.Env = []string{}
	cmd.Stdin = nil
	d := &diagnostics{emit: r.Diagnostic}
	if s != nil {
		d = s.logs
	}
	if s == nil {
		defer func() {
			reason := ""
			if resultErr != nil {
				reason = diagnosticReason(resultErr)
			}
			d.finish(reason)
		}()
	}
	cmd.Stdout = streamWriter{d, false}
	cmd.Stderr = streamWriter{d, true}
	cpuBudget := time.Until(deadline).Seconds()
	if r.cpuOverride > 0 {
		cpuBudget = r.cpuOverride
	}
	if e = cmd.Start(); e != nil {
		return nil, failure("EXECUTION_FAILED", e.Error())
	}
	childIn.Close()
	childOut.Close()
	finished := make(chan error, 1)
	exited := make(chan struct{})
	go func() { err := cmd.Wait(); close(exited); finished <- err }()
	// All blocked pipe operations stop when cancellation kills the worker. Parent
	// descriptors are closed too, including while bootstrap bytes are being sent.
	stopIO := context.AfterFunc(childCtx, func() { parentIn.Close(); parentOut.Close() })
	defer stopIO()
	waited := false
	defer func() {
		if !waited {
			cancel()
			<-finished
		}
	}()
	closeRestriction, e := restrictProcess(cmd, productRSSLimit, time.Until(deadline))
	if e != nil {
		return nil, failure("DEPENDENCY_UNAVAILABLE", e.Error())
	}
	defer closeRestriction()
	tokenBytes := make([]byte, 32)
	if _, e = rand.Read(tokenBytes); e != nil {
		return nil, e
	}
	token := hex.EncodeToString(tokenBytes)
	boot := bootstrap{Token: token, Limits: l, Deadline: deadline.UnixNano(), ModuleSize: len(m.bytes), ParamsSize: len(params), Validate: validate, Version: r.version()}
	if r.Bridge != nil && !validate {
		boot.Environment = r.Bridge.Environment
		boot.Business = true
		boot.Base = r.Bridge.Base
	}
	if e = send(parentOut, boot); e != nil {
		return nil, failure("EXECUTION_FAILED", e.Error())
	}
	var ready packet
	if e = receive(parentIn, &ready); e != nil || ready.Op != "ready" || ready.Token != token {
		return nil, failure("EXECUTION_FAILED", "worker bootstrap failed")
	}
	// At most the worker's CPU rlimit, which it set from the remaining time before ready.
	cpuLimit := math.Min(cpuBudget, math.Max(1, math.Ceil(time.Until(deadline).Seconds())))
	p, e := process.NewProcess(int32(cmd.Process.Pid))
	if e != nil {
		return nil, failure("DEPENDENCY_UNAVAILABLE", e.Error())
	}
	readCounters := func() (uint64, float64, error) {
		if r.sample != nil {
			return r.sample(p)
		}
		return sampleCounters(childCtx, p)
	}
	observation := Observation{}
	rss, cpu, e := readCounters()
	if e != nil {
		return nil, failure("DEPENDENCY_UNAVAILABLE", "initial worker counter: "+e.Error())
	}
	observation.PeakRSS = rss
	observation.CPUSeconds = cpu
	var observationMu sync.Mutex
	monitorError := make(chan error, 1)
	monitorDone := make(chan struct{})
	var monitorWG sync.WaitGroup
	monitorWG.Add(1)
	go func() {
		defer monitorWG.Done()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitorDone:
				return
			case <-childCtx.Done():
				return
			case <-ticker.C:
				rss, cpu, e := readCounters()
				if e != nil {
					if childCtx.Err() != nil {
						return
					}
					select {
					case <-exited:
						return
					case <-childCtx.Done():
						return
					case <-time.After(10 * time.Millisecond):
					}
					select {
					case monitorError <- failure("DEPENDENCY_UNAVAILABLE", e.Error()):
					default:
					}
					cancel()
					return
				}
				observationMu.Lock()
				if rss > observation.PeakRSS {
					observation.PeakRSS = rss
				}
				observation.CPUSeconds = cpu
				observationMu.Unlock()
				if rss > rssLimit || cpu > cpuBudget {
					select {
					case monitorError <- failure("LIMIT_EXCEEDED", "worker OS resource budget exceeded"):
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	var monitorStop sync.Once
	stopMonitor := func() { monitorStop.Do(func() { close(monitorDone); monitorWG.Wait() }) }
	defer func() {
		stopMonitor()
		if r.Observe != nil {
			observationMu.Lock()
			r.Observe(observation)
			observationMu.Unlock()
		}
	}()
	if e = send(parentOut, packet{Op: "go", Token: token}); e != nil {
		return nil, e
	}
	started := time.Now()
	if e = writeAll(parentOut, m.bytes); e == nil {
		e = writeAll(parentOut, params)
	}
	if e != nil {
		return nil, failure("EXECUTION_FAILED", e.Error())
	}
	done := false
	for {
		var packet packet
		e = receive(parentIn, &packet)
		if e != nil {
			break
		}
		if packet.Op == "compiled" {
			observationMu.Lock()
			observation.CompileDuration = time.Since(started)
			observationMu.Unlock()
			continue
		}
		if packet.Op == "done" {
			done = true
			metadata = packet.Metadata
			stopMonitor()
			if e = send(parentOut, struct {
				Op string `json:"op"`
			}{Op: "finish"}); e != nil {
				done = false
			}
			break
		}
		if validate || s == nil {
			e = errors.New("invalid validation response")
			break
		}
		if e = send(parentOut, s.dispatch(packet)); e != nil {
			break
		}
	}
	if !done {
		cancel()
	}
	waitError := <-finished
	waited = true
	var resultError error
	select {
	case resultError = <-monitorError:
	default:
	}
	if resultError == nil && ctx.Err() != nil {
		resultError = failure("EXECUTION_TIMEOUT", ctx.Err().Error())
	}
	if resultError == nil && waitError != nil && cpuLimitKilled(cmd.ProcessState, cpuLimit) {
		resultError = failure("LIMIT_EXCEEDED", "worker OS resource budget exceeded")
	}
	if resultError == nil && waitError != nil {
		resultError = failure("EXECUTION_FAILED", waitError.Error())
	}
	if resultError == nil && !done {
		resultError = failure("EXECUTION_FAILED", fmt.Sprint(e))
	}
	if resultError != nil {
		return nil, resultError
	}
	return metadata, nil
}

var _ io.Writer = streamWriter{}

func diagnosticReason(err error) string {
	var f *Error
	if errors.As(err, &f) && f.Detail != "" {
		return f.Code + ": " + f.Detail
	}
	return err.Error()
}
