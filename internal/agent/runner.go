package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
	_ "time/tzdata"

	_ "github.com/denisenkom/go-mssqldb"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	_ "github.com/sijms/go-ora/v2"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

const (
	agentResponseWriteTimeout  = 5 * time.Second
	agentChallengeFetchTimeout = 10 * time.Second
)

type Runner struct {
	cfg             Config
	cf              *CapabilityFile
	caps            map[string]CapabilityDef
	db              *sql.DB
	logOut          io.Writer
	detailLog       io.Writer
	detailLogCloser io.Closer
}

type requestTask struct {
	req    protocol.Request
	ctx    context.Context
	cancel context.CancelFunc
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
	r := &Runner{cfg: cfg, cf: prepared.cf, caps: prepared.caps, db: prepared.db, logOut: logOut, detailLog: prepared.detailLog.Writer, detailLogCloser: preparationLogCloser{prepared.detailLog}}
	r.log("agent_ready", map[string]any{"capabilities": len(prepared.cf.Capabilities), "driver": prepared.cf.Database.Driver, "max_concurrent_requests": *prepared.cf.Runtime.MaxConcurrentRequests})
	return r, nil
}

func openDatabase(database DatabaseDef) (*sql.DB, error) {
	if database.Driver != "postgres" {
		return sql.Open(driverName(database.Driver), database.DSN())
	}
	config, err := pgx.ParseConfig(database.DSN())
	if err != nil {
		return nil, err
	}
	afterConnect := func(ctx context.Context, conn *pgx.Conn) error {
		// lib/pq represented timestamp without time zone in an unnamed UTC
		// location, while timestamptz used PostgreSQL's session TimeZone. Set
		// both codecs explicitly so the pgx migration preserves those public
		// string coercions and does not inherit the agent process timezone.
		conn.TypeMap().RegisterType(&pgtype.Type{
			Name: "timestamp", OID: pgtype.TimestampOID,
			Codec: &pgtype.TimestampCodec{ScanLocation: time.FixedZone("", 0)},
		})
		conn.TypeMap().RegisterType(&pgtype.Type{
			Name: "timestamptz", OID: pgtype.TimestamptzOID,
			Codec: &pgtype.TimestamptzCodec{ScanLocation: postgresSessionLocation(ctx, conn)},
		})
		return nil
	}
	return stdlib.OpenDB(*config, stdlib.OptionAfterConnect(afterConnect)), nil
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
	defer r.db.Close()
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
		if err := setAgentAuthHeaders(headers, r.cf.Gateway.AgentPrivateKey, "/ws/agent", challenge); err != nil {
			r.log("agent_auth_failed", map[string]any{"error": err.Error()})
			return err
		}
		conn, err := ws.Dial(10*time.Second, r.cf.Gateway.URL, headers)
		if err != nil {
			r.log("gateway_connect_failed", map[string]any{"error": err.Error()})
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
	connCtx, cancel := context.WithCancel(ctx)
	maxConcurrent := *r.cf.Runtime.MaxConcurrentRequests
	requests := make(chan requestTask, maxConcurrent)
	responses := make(chan protocol.Response, maxConcurrent*2+1)
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
						inflight.finish(task.req.ID)
						continue
					}
					resp = r.handle(task.ctx, task.req)
					inflight.finish(task.req.ID)
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
		var envelope struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		dec := json.NewDecoder(bytes.NewReader(msg))
		dec.UseNumber()
		if err := dec.Decode(&envelope); err != nil {
			r.detailError("", "AGENT_INTERNAL_ERROR", "invalid gateway request", "invalid gateway request: "+err.Error(), "")
			select {
			case responses <- protocol.Response{Error: &protocol.Error{Code: "AGENT_INTERNAL_ERROR", Message: "invalid gateway request"}}:
			case <-connCtx.Done():
				return
			}
			continue
		}
		if envelope.Type != "" {
			if envelope.Type == "cancel" && envelope.ID != "" {
				inflight.cancel(envelope.ID)
				continue
			}
			r.detailError("", "AGENT_INTERNAL_ERROR", "invalid gateway request", "unknown or invalid control request", envelope.ID)
			continue
		}
		var req protocol.Request
		dec = json.NewDecoder(bytes.NewReader(msg))
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil || req.ID == "" {
			select {
			case responses <- protocol.Response{ID: req.ID, Error: &protocol.Error{Code: "AGENT_INTERNAL_ERROR", Message: "invalid gateway request"}}:
			case <-connCtx.Done():
				return
			}
			continue
		}
		taskCtx, taskCancel := context.WithCancel(connCtx)
		if !inflight.add(req.ID, taskCancel) {
			taskCancel()
			select {
			case responses <- protocol.Response{ID: req.ID, Error: &protocol.Error{Code: "AGENT_INTERNAL_ERROR", Message: "invalid gateway request"}}:
			case <-connCtx.Done():
				return
			}
			continue
		}
		select {
		case requests <- requestTask{req: req, ctx: taskCtx, cancel: taskCancel}:
		case <-connCtx.Done():
			inflight.finish(req.ID)
			return
		default:
			inflight.finish(req.ID)
			select {
			case responses <- protocol.Response{ID: req.ID, Error: &protocol.Error{Code: "AGENT_BUSY", Message: "agent request queue is full"}}:
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
