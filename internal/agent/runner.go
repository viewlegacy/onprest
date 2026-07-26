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

	_ "github.com/denisenkom/go-mssqldb"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/sijms/go-ora/v2"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

const agentResponseWriteTimeout = 5 * time.Second

type Runner struct {
	cfg             Config
	cf              *CapabilityFile
	caps            map[string]CapabilityDef
	db              *sql.DB
	logOut          io.Writer
	detailLog       io.Writer
	detailLogCloser io.Closer
}

func NewRunner(cfg Config, logOut io.Writer) (*Runner, error) {
	cf, err := LoadCapabilityFile(cfg.CapabilityFile)
	if err != nil {
		return nil, err
	}
	detailLog, err := newAgentDetailLog(cf.Logging)
	if err != nil {
		return nil, fmt.Errorf("agent detail log: %w", err)
	}
	closeDetailLog := true
	defer func() {
		if closeDetailLog {
			_ = detailLog.Close()
		}
	}()
	db, err := sql.Open(driverName(cf.Database.Driver), cf.Database.DSN())
	if err != nil {
		writeStartupDetail(detailLog, "AGENT_STARTUP_FAILED", err.Error())
		return nil, startupFailure()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		writeStartupDetail(detailLog, "AGENT_DB_UNREACHABLE", err.Error())
		_ = db.Close()
		return nil, startupFailure()
	}
	r := &Runner{cfg: cfg, cf: cf, caps: cf.ByName(), db: db, logOut: logOut, detailLog: detailLog, detailLogCloser: detailLog}
	if err := r.explainAll(ctx); err != nil {
		r.detailError("", "AGENT_STARTUP_FAILED", "agent startup failed", err.Error(), "")
		_ = db.Close()
		return nil, startupFailure()
	}
	closeDetailLog = false
	r.log("agent_ready", map[string]any{"capabilities": len(cf.Capabilities), "driver": cf.Database.Driver, "max_concurrent_requests": *cf.Runtime.MaxConcurrentRequests})
	return r, nil
}

func startupFailure() error {
	return fmt.Errorf("agent startup failed; see agent detail log")
}

func writeStartupDetail(w io.Writer, code, detail string) {
	if w == nil {
		return
	}
	fields := map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339Nano),
		"event":      "agent_error",
		"capability": "",
		"error_code": code,
		"message":    "agent startup failed",
		"detail":     detail,
	}
	_ = json.NewEncoder(w).Encode(fields)
}

func (r *Runner) Run(ctx context.Context) error {
	defer r.db.Close()
	if r.detailLogCloser != nil {
		defer r.detailLogCloser.Close()
	}
	for ctx.Err() == nil {
		challengeCtx, challengeCancel := context.WithTimeout(ctx, 10*time.Second)
		challenge, err := fetchAgentChallenge(challengeCtx, r.cf.Gateway.URL)
		challengeCancel()
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

func (r *Runner) serveConn(ctx context.Context, conn *ws.Conn) {
	connCtx, cancel := context.WithCancel(ctx)
	maxConcurrent := *r.cf.Runtime.MaxConcurrentRequests
	requests := make(chan protocol.Request, maxConcurrent)
	responses := make(chan protocol.Response, maxConcurrent*2+1)
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
				case req := <-requests:
					resp := r.handle(connCtx, req)
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
		var req protocol.Request
		dec := json.NewDecoder(bytes.NewReader(msg))
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil {
			r.detailError("", "AGENT_INTERNAL_ERROR", "invalid gateway request", "invalid gateway request: "+err.Error(), "")
			select {
			case responses <- protocol.Response{Error: &protocol.Error{Code: "AGENT_INTERNAL_ERROR", Message: "invalid gateway request"}}:
			case <-connCtx.Done():
				return
			}
			continue
		}
		select {
		case requests <- req:
		case <-connCtx.Done():
			return
		default:
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
