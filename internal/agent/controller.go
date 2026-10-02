package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

type ExecutionRequest struct {
	ID, Capability, DefinitionHash string
	Params                         map[string]any
	Deadline                       time.Time
}

// ExecutionController owns admission, one execution budget, public output and
// safe failures. It is deliberately disconnected from the existing Runner.
type ExecutionController struct {
	prepared    *providers.Prepared
	stager      contract.ArtifactStager
	sink        *DiagnosticSink
	mu          sync.Mutex
	active, max int64
	rates       map[string]*capabilityRateLimiter
}

func NewExecutionController(prepared *providers.Prepared, stager contract.ArtifactStager, sink *DiagnosticSink) (*ExecutionController, error) {
	if prepared == nil {
		return nil, errors.New("invalid execution controller")
	}
	maxConcurrent, ok := prepared.Runtime()["max_concurrent_requests"].(int64)
	if !ok || maxConcurrent < 1 {
		return nil, errors.New("invalid execution controller")
	}
	c := &ExecutionController{prepared: prepared, max: maxConcurrent, stager: stager, sink: sink, rates: map[string]*capabilityRateLimiter{}}
	for _, name := range prepared.Names() {
		s, ok := prepared.Snapshot(name)
		if !ok {
			return nil, errors.New("prepared execution is closed")
		}
		cap := s.Definition()
		if r, ok := cap.Policy["rate_limit"].(map[string]any); ok {
			def := RateLimitDef{Requests: r["requests"].(int64), Burst: r["burst"].(int64), Per: r["per"].(string)}
			if err := def.lint(); err != nil {
				return nil, err
			}
			c.rates[name] = newCapabilityRateLimiter(def, time.Now())
		}
	}
	return c, nil
}

func (c *ExecutionController) Execute(parent context.Context, req ExecutionRequest) (out *contract.PreparedOutput, failure *contract.ErrorValue) {
	effects := &providers.Effects{}
	fail := func(code string) *contract.ErrorValue {
		return &contract.ErrorValue{Code: code, Message: executionMessage(code), Outcome: effects.Outcome()}
	}
	if parent.Err() != nil || !req.Deadline.IsZero() && !time.Now().Before(req.Deadline) {
		return nil, fail("EXECUTION_TIMEOUT")
	}
	if req.ID == "" || len(req.ID) > 256 || !utf8.ValidString(req.ID) {
		return nil, fail("INVALID_REQUEST")
	}
	lease, err := c.prepared.Acquire(req.Capability)
	if err != nil {
		return nil, fail("NOT_FOUND")
	}
	defer lease.Release()
	snapshot := lease.Snapshot()
	cap := snapshot.Definition()
	if req.DefinitionHash != "" && req.DefinitionHash != snapshot.DefinitionHash() {
		return nil, fail("CONFLICT")
	}
	parent, cancelLease := lease.Context(parent)
	defer cancelLease()
	d, err := time.ParseDuration(cap.Policy["timeout"].(string))
	if err != nil || d <= 0 || d > 300*time.Second {
		return nil, fail("EXECUTION_FAILED")
	}
	deadline := time.Now().Add(d)
	if !req.Deadline.IsZero() && req.Deadline.Before(deadline) {
		deadline = req.Deadline
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	params, leaves, err := cap.ValidateInput(req.Params)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fail("EXECUTION_TIMEOUT")
		}
		return nil, fail("VALIDATION_FAILED")
	}
	for _, leaf := range leaves {
		if leaf.Value.RequestID != req.ID {
			return nil, fail("VALIDATION_FAILED")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fail("EXECUTION_TIMEOUT")
	}
	if rate := c.rates[req.Capability]; rate != nil {
		allowed, _ := rate.take(time.Now())
		if !allowed {
			return nil, fail("RATE_LIMITED")
		}
	}
	c.mu.Lock()
	if c.active >= c.max {
		c.mu.Unlock()
		return nil, fail("BUSY")
	}
	c.active++
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.active--; c.mu.Unlock() }()
	defer func() {
		if v := recover(); v != nil {
			if out != nil {
				_ = out.Close()
				out = nil
			}
			c.diagnostic(req, cap.Executor.Type, "EXECUTION_FAILED", fmt.Sprint(v))
			failure = fail("EXECUTION_FAILED")
			if ctx.Err() != nil {
				failure = fail("EXECUTION_TIMEOUT")
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, fail("EXECUTION_TIMEOUT")
	}
	effects.Start()
	result, err := lease.Execute(ctx, params, effects)
	if err != nil {
		// An error result still transfers all reader ownership to the controller.
		suppressed := cap.Clone()
		suppressed.Output.Data = false
		suppressed.Output.Files = nil
		_, cleanupErr := contract.ProcessOutput(ctx, suppressed, result, nil)
		c.diagnostic(req, cap.Executor.Type, "EXECUTION_FAILED", errors.Join(err, cleanupErr, errors.Unwrap(cleanupErr)).Error())
		if ctx.Err() != nil {
			return nil, fail("EXECUTION_TIMEOUT")
		}
		var pf *providers.Failure
		if errors.As(err, &pf) && !strings.HasPrefix(pf.Code, "APP_") {
			public := fail(pf.Code)
			public.Reason = pf.Reason
			if public.Validate() == nil {
				return nil, public
			}
		}
		return nil, fail("EXECUTION_FAILED")
	}
	out, err = contract.ProcessOutput(ctx, cap, result, c.stager)
	if err != nil {
		code := "OUTPUT_INVALID"
		var oe *contract.OutputError
		if errors.As(err, &oe) {
			code = oe.Code
		}
		if ctx.Err() != nil {
			code = "EXECUTION_TIMEOUT"
		}
		c.diagnostic(req, cap.Executor.Type, code, errors.Join(err, errors.Unwrap(err)).Error())
		return nil, fail(code)
	}
	if ctx.Err() != nil {
		_ = out.Close()
		return nil, fail("EXECUTION_TIMEOUT")
	}
	return out, nil
}
func (c *ExecutionController) diagnostic(req ExecutionRequest, source, code, detail string) {
	if c.sink != nil {
		_ = c.sink.Write(DiagnosticRecord{Level: "error", Source: source, Event: "agent_error", Message: executionMessage(code), Capability: req.Capability, RequestID: req.ID, ErrorCode: code, Detail: detail})
	}
}
func executionMessage(code string) string {
	switch code {
	case "INVALID_REQUEST":
		return "request is invalid"
	case "VALIDATION_FAILED":
		return "input does not satisfy the configured contract"
	case "NOT_FOUND":
		return "capability is not available"
	case "CONFLICT":
		return "capability definition has changed"
	case "RATE_LIMITED":
		return "capability rate limit exceeded"
	case "BUSY":
		return "execution capacity is busy"
	case "EXECUTION_TIMEOUT":
		return "execution exceeded the configured deadline"
	case "OUTPUT_INVALID":
		return "output does not satisfy the configured contract"
	case "LIMIT_EXCEEDED":
		return "output exceeds the configured limit"
	default:
		return "capability execution failed"
	}
}
