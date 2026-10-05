package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

const (
	agentResponseWriteTimeout  = 5 * time.Second
	agentChallengeFetchTimeout = 10 * time.Second
	agentMinResponseQueueSize  = 256
	// Gateway request IDs are 26-byte ULIDs. Keep generous compatibility room
	// for internal peers while bounding every echoed response and detail-log ID;
	// otherwise a non-reading peer could retain 256 near-16MiB IDs in the
	// bounded response queue.
	agentMaxWireIDBytes = 256
)

type Runner struct {
	dial            func(time.Duration, string, http.Header) (*ws.Conn, error)
	store           *artifacts.Store
	controls        *controlAdmission
	versioned       *providers.Prepared
	controller      *ExecutionController
	cfg             Config
	cf              *CapabilityFile
	caps            map[string]CapabilityDef
	rateLimits      map[string]*capabilityRateLimiter
	clock           agentClock
	db              *sql.DB
	logOut          io.Writer
	detailLog       io.Writer
	detailLogCloser io.Closer
}

type requestTask struct {
	prepared *preparedExecution
	response *protocol.Response
	id       string
	ctx      context.Context
	cancel   context.CancelFunc
}

type inflightRegistry struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func (i *inflightRegistry) add(id string, cancel context.CancelFunc) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if id == "" {
		return false
	}
	if _, exists := i.cancels[id]; exists {
		return false
	}
	i.cancels[id] = cancel
	return true
}
func (i *inflightRegistry) contains(id string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, ok := i.cancels[id]
	return ok
}
func (i *inflightRegistry) cancel(id string) {
	i.mu.Lock()
	cancel := i.cancels[id]
	i.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (i *inflightRegistry) finish(id string) {
	i.mu.Lock()
	cancel := i.cancels[id]
	delete(i.cancels, id)
	i.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (i *inflightRegistry) cancelAll() {
	i.mu.Lock()
	cancels := i.cancels
	i.cancels = map[string]context.CancelFunc{}
	i.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func NewRunner(ctx context.Context, cfg Config, logOut io.Writer) (*Runner, error) {
	prepared, err := prepareAgent(ctx, cfg, newRuntimeDetailLogFactory())
	if err != nil {
		if pe, ok := err.(*preparationError); ok && pe.stage == validationStageConfig {
			return nil, pe
		}
		return nil, startupFailure()
	}
	clock := agentClock(time.Now)
	r := &Runner{versioned: prepared.versioned, controller: prepared.controller, cfg: cfg, cf: prepared.cf, caps: prepared.caps, rateLimits: newCapabilityRateLimiters(prepared.cf, clock), clock: clock, db: prepared.db, logOut: logOut, detailLog: prepared.detailLog.Writer, detailLogCloser: preparationLogCloser{prepared.detailLog}}
	if err := r.initializeTransport(); err != nil {
		prepared.versioned.Close()
		prepared.detailLog.Close()
		return nil, startupFailure()
	}
	r.log("agent_ready", map[string]any{"capabilities": len(prepared.cf.Capabilities), "driver": prepared.cf.Database.Driver, "max_concurrent_requests": *prepared.cf.Runtime.MaxConcurrentRequests})
	return r, nil
}

func postgresSessionLocation(ctx context.Context, conn *pgx.Conn) *time.Location {
	if name := conn.PgConn().ParameterStatus("TimeZone"); name != "" {
		if location, err := time.LoadLocation(name); err == nil {
			return location
		}
	}
	// PostgreSQL also accepts fixed-offset/POSIX TimeZone values that Go may
	// not recognize by name. Preserve the session's current offset in that
	// case, as lib/pq did when it could not load the named location.
	var secondsEast int32
	if err := conn.QueryRow(ctx, "select extract(timezone from current_timestamp)::integer").Scan(&secondsEast); err == nil {
		return time.FixedZone("", int(secondsEast))
	}
	return time.UTC
}

func startupFailure() error {
	return fmt.Errorf("agent startup failed; see agent detail log")
}

type preparationLogCloser struct{ log *preparationDetailLog }

func (c preparationLogCloser) Close() error { return c.log.Close() }

func (r *Runner) Run(ctx context.Context) error {
	if r.store != nil {
		defer r.store.Close()
	}
	if r.versioned != nil {
		defer r.versioned.Close()
	} else if r.db != nil {
		defer r.db.Close()
	}
	if r.detailLogCloser != nil {
		defer r.detailLogCloser.Close()
	}
	for ctx.Err() == nil {
		challenge, err := fetchRunnerChallenge(ctx, r.cf.Gateway.URL, fetchAgentChallenge)
		if err != nil {
			r.log("gateway_connect_failed", map[string]any{"error": err.Error()})
			sleep(ctx, r.cfg.ReconnectEvery)
			continue
		}
		headers := http.Header{}
		headers.Set(protocol.MajorHeader, protocol.Major)
		if err := setAgentAuthHeaders(headers, r.cf.Gateway.AgentPrivateKey, "/ws/agent", challenge); err != nil {
			r.log("agent_auth_failed", map[string]any{"error": err.Error()})
			return err
		}
		dial := r.dial
		if dial == nil {
			dial = ws.Dial
		}
		conn, err := dial(10*time.Second, r.cf.Gateway.URL, headers)
		if err != nil {
			r.log("gateway_connect_failed", map[string]any{"error": err.Error()})
			sleep(ctx, r.cfg.ReconnectEvery)
			continue
		}
		if conn.ProtocolMajor() != protocol.Major {
			_ = conn.Close()
			r.log("gateway_connect_failed", map[string]any{"error": "unsupported gateway protocol major"})
			sleep(ctx, r.cfg.ReconnectEvery)
			continue
		}
		r.log("gateway_connected", nil)
		r.serveConn(ctx, conn)
		sleep(ctx, r.cfg.ReconnectEvery)
	}
	return ctx.Err()
}

func fetchRunnerChallenge(parent context.Context, gatewayURL string, fetch func(context.Context, string) (string, error)) (string, error) {
	ctx, cancel := context.WithTimeout(parent, agentChallengeFetchTimeout)
	defer cancel()
	return fetch(ctx, gatewayURL)
}

func (r *Runner) serveConn(ctx context.Context, conn *ws.Conn) {
	if r.controller != nil {
		r.serveTransport(ctx, conn)
		return
	}
	connCtx, cancel := context.WithCancel(ctx)
	maxConcurrent := *r.cf.Runtime.MaxConcurrentRequests
	requests := make(chan requestTask, maxConcurrent)
	responses := make(chan protocol.Response, agentResponseQueueSize(maxConcurrent))
	inflight := &inflightRegistry{cancels: map[string]context.CancelFunc{}}
	done := make(chan struct{})
	go func() {
		select {
		case <-connCtx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	var workers sync.WaitGroup
	workers.Add(maxConcurrent + 1)
	for range maxConcurrent {
		go func() {
			defer workers.Done()
			for {
				select {
				case task := <-requests:
					var resp protocol.Response
					if task.ctx.Err() != nil {
						inflight.finish(task.id)
						continue
					}
					if task.response != nil {
						resp = *task.response
					} else {
						resp = r.executePrepared(task.ctx, task.prepared)
					}
					inflight.finish(task.id)
					select {
					case responses <- resp:
					case <-connCtx.Done():
						return
					}
				case <-connCtx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer workers.Done()
		for {
			select {
			case resp := <-responses:
				if err := conn.WriteTextWithDeadline(protocol.MustJSON(resp), agentResponseWriteTimeout); err != nil {
					r.log("gateway_write_failed", map[string]any{"error": err.Error()})
					cancel()
					_ = conn.Close()
					return
				}
			case <-connCtx.Done():
				return
			}
		}
	}()
	defer func() {
		cancel()
		inflight.cancelAll()
		_ = conn.Close()
		workers.Wait()
		close(done)
	}()
	for connCtx.Err() == nil {
		msg, err := conn.ReadText()
		if err != nil {
			r.log("gateway_disconnected", map[string]any{"error": err.Error()})
			return
		}
		// Inspect the bounded ID before allocating the full JSON value tree.
		// Unmarshal skips other fields without constructing their values.
		var envelope struct {
			ID boundedWireID `json:"id"`
		}
		if json.Unmarshal(msg, &envelope) != nil || !validAgentWireID(string(envelope.ID)) {
			return
		}
		control, decodeErr := contract.DecodeJSON(msg, contract.Limits{MaxBytes: 16 << 20, MaxDepth: 32, MaxItems: 100000})
		if decodeErr != nil {
			return
		}
		controlFields, validObject := control.(map[string]any)
		if !validObject {
			return
		}
		requestType, _ := controlFields["type"].(string)
		if requestType != "request" {
			if requestType == "cancel" && len(controlFields) == 2 {
				inflight.cancel(string(envelope.ID))
				continue
			}
			r.detailError("", "INTERNAL_ERROR", "invalid gateway request", "unknown or invalid control request", string(envelope.ID))
			continue
		}
		var req protocol.Request
		dec := json.NewDecoder(bytes.NewReader(msg))
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil || req.ID == "" {
			select {
			case responses <- protocol.Response{ID: req.ID, Error: &protocol.Error{Code: "INVALID_REQUEST", Message: "invalid gateway request", Outcome: "not_started"}}:
			case <-connCtx.Done():
				return
			}
			continue
		}
		prepared, prepareRejected := r.prepare(req)
		if prepareRejected == nil {
			if rateRejected := r.rateLimitRejection(prepared); rateRejected != nil {
				select {
				case responses <- *rateRejected:
				case <-connCtx.Done():
					return
				default:
					// A malicious peer that does not read rejections must not
					// block the WebSocket reader or grow a response backlog.
					cancel()
					_ = conn.Close()
					return
				}
				continue
			}
		}
		taskCtx, taskCancel := context.WithCancel(connCtx)
		if !inflight.add(req.ID, taskCancel) {
			taskCancel()
			select {
			case responses <- protocol.Response{ID: req.ID, Error: &protocol.Error{Code: "INVALID_REQUEST", Message: "invalid gateway request", Outcome: "not_started"}}:
			case <-connCtx.Done():
				return
			}
			continue
		}
		task := requestTask{id: req.ID, prepared: prepared, response: prepareRejected, ctx: taskCtx, cancel: taskCancel}
		select {
		case requests <- task:
		case <-connCtx.Done():
			inflight.finish(req.ID)
			return
		default:
			inflight.finish(req.ID)
			select {
			case responses <- protocol.Response{ID: req.ID, Error: &protocol.Error{Code: "BUSY", Message: "agent request queue is full", Outcome: "not_started"}}:
			case <-connCtx.Done():
				return
			default:
				cancel()
				_ = conn.Close()
				return
			}
		}
	}
}

type boundedWireID string

func (id *boundedWireID) UnmarshalJSON(raw []byte) error {
	// Bound raw size before strict string decoding so malformed Unicode cannot
	// be replaced by encoding/json before identity admission.
	if len(raw) > 6*agentMaxWireIDBytes+2 {
		return errors.New("invalid gateway request ID")
	}
	decoded, e := contract.DecodeJSON(raw, contract.Limits{MaxBytes: 6*agentMaxWireIDBytes + 2, MaxDepth: 1, MaxItems: 1})
	value, ok := decoded.(string)
	if e != nil || !ok || !validAgentWireID(value) {
		return errors.New("invalid gateway request ID")
	}
	*id = boundedWireID(value)
	return nil
}

func validAgentWireID(id string) bool {
	return protocol.ValidID(id) && len(id) <= agentMaxWireIDBytes
}

func agentResponseQueueSize(maxConcurrent int) int {
	size := maxConcurrent*2 + 1
	if size < agentMinResponseQueueSize {
		return agentMinResponseQueueSize
	}
	return size
}

func (r *Runner) log(event string, fields map[string]any) {
	if r.logOut == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["event"] = event
	_ = json.NewEncoder(r.logOut).Encode(fields)
}

func (r *Runner) detailError(capability, code, message, detail, requestID string) {
	if r.detailLog == nil {
		return
	}
	fields := map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339Nano),
		"event":      "agent_error",
		"capability": capability,
		"error_code": code,
		"message":    message,
		"detail":     detail,
	}
	if requestID != "" {
		fields["request_id"] = requestID
	}
	_ = json.NewEncoder(r.detailLog).Encode(fields)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
