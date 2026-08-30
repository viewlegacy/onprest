package agent

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mssql "github.com/denisenkom/go-mssqldb"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sijms/go-ora/v2/network"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

var mutationDriverSequence atomic.Uint64

type mutationDriverState struct {
	mu                                       sync.Mutex
	calls                                    []string
	opens, closes                            int
	beginCtx                                 context.Context
	execErr, rowsErr, commitErr, rollbackErr error
	beginErr                                 error
	beginStarted                             chan struct{}
	beginRelease                             chan struct{}
	beginStartOnce                           sync.Once
	beginSucceedAfterCancel                  bool
	rows                                     int64
	rollbackCtxErr                           error
	execStarted                              chan struct{}
	execRelease                              chan struct{}
	execStartOnce                            sync.Once
	rollbackStarted                          chan struct{}
	rollbackRelease                          chan struct{}
	rollbackDone                             chan struct{}
	rollbackStartOnce                        sync.Once
	rollbackDoneOnce                         sync.Once
}

type observedCancellationContext struct {
	context.Context
	once     sync.Once
	observed chan struct{}
}

func (c *observedCancellationContext) Err() error {
	err := c.Context.Err()
	if err != nil {
		c.once.Do(func() { close(c.observed) })
	}
	return err
}

func (s *mutationDriverState) call(name string) {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	s.mu.Unlock()
}

type mutationTestDriver struct{ state *mutationDriverState }

func (d mutationTestDriver) Open(string) (driver.Conn, error) {
	d.state.mu.Lock()
	d.state.opens++
	d.state.mu.Unlock()
	return &mutationTestConn{state: d.state}, nil
}

type mutationTestConn struct{ state *mutationDriverState }

func (c *mutationTestConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *mutationTestConn) Close() error {
	c.state.mu.Lock()
	c.state.closes++
	c.state.mu.Unlock()
	return nil
}
func (c *mutationTestConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *mutationTestConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	c.state.call("begin")
	c.state.beginCtx = ctx
	if c.state.beginStarted != nil {
		c.state.beginStartOnce.Do(func() { close(c.state.beginStarted) })
	}
	if c.state.beginRelease != nil {
		select {
		case <-c.state.beginRelease:
		case <-ctx.Done():
			if c.state.beginSucceedAfterCancel {
				<-c.state.beginRelease
			} else {
				return nil, ctx.Err()
			}
		}
	}
	if c.state.beginErr != nil {
		return nil, c.state.beginErr
	}
	return &mutationTestTx{state: c.state}, nil
}

func TestBeginMutationTxPoolAcquisitionAndDriverStartupAreBounded(t *testing.T) {
	t.Run("pool acquisition", func(t *testing.T) {
		state := &mutationDriverState{}
		runner := mutationRunner(t, state)
		runner.db.SetMaxOpenConns(1)
		held, err := runner.db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		execCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, beginState, err := beginMutationTx(t.Context(), execCtx, runner.db, "postgres")
		if !errors.Is(err, context.DeadlineExceeded) || beginState != beginNotStarted {
			t.Fatalf("state=%v error=%v", beginState, err)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("pool acquisition did not return promptly: %s", time.Since(start))
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if len(state.calls) != 0 {
			t.Fatalf("driver begin was reached: %v", state.calls)
		}
	})

	t.Run("driver startup", func(t *testing.T) {
		state := &mutationDriverState{beginStarted: make(chan struct{}), beginRelease: make(chan struct{})}
		runner := mutationRunner(t, state)
		execCtx, cancel := context.WithCancel(t.Context())
		result := make(chan *mutationFailure, 1)
		go func() {
			_, _, failure := runner.executeMutation(t.Context(), execCtx, CapabilityDef{Policy: PolicyDef{MaxBytes: "128KB"}}, "update t set v=1", nil)
			result <- failure
		}()
		select {
		case <-state.beginStarted:
		case <-time.After(time.Second):
			t.Fatal("BeginTx did not start")
		}
		cancel()
		select {
		case failure := <-result:
			if failure == nil || failure.code != errorOutcomeUnknown {
				t.Fatalf("failure=%+v", failure)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled BeginTx did not return")
		}
		conn, err := runner.db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		state.mu.Lock()
		opens := state.opens
		state.mu.Unlock()
		if opens != 2 {
			t.Fatalf("outcome-unknown BeginTx connection was reused: opens=%d", opens)
		}
	})

	t.Run("transaction start and cancellation race never commits", func(t *testing.T) {
		for iteration := range 100 {
			func() {
				state := &mutationDriverState{
					beginStarted:            make(chan struct{}),
					beginRelease:            make(chan struct{}),
					beginSucceedAfterCancel: true,
					rollbackDone:            make(chan struct{}),
				}
				runner := mutationRunnerWithoutCleanup(t, state)
				defer runner.db.Close()
				runner.db.SetMaxOpenConns(1)
				execCtx, cancel := context.WithCancel(t.Context())
				type beginResult struct {
					mtx   *mutationTx
					state beginState
					err   error
				}
				result := make(chan beginResult, 1)
				go func() {
					mtx, beginState, err := beginMutationTx(t.Context(), execCtx, runner.db, "postgres")
					result <- beginResult{mtx: mtx, state: beginState, err: err}
				}()
				select {
				case <-state.beginStarted:
				case <-time.After(time.Second):
					t.Fatalf("iteration %d: BeginTx did not start", iteration)
				}

				// Make cancellation observable inside the driver before it returns a
				// Tx. This deterministically exercises the successful BeginTx return
				// after the start gate watcher won cancellation.
				cancel()
				select {
				case <-state.beginCtx.Done():
				case <-time.After(time.Second):
					t.Fatalf("iteration %d: start gate watcher did not cancel driver context", iteration)
				}
				close(state.beginRelease)

				var got beginResult
				select {
				case got = <-result:
				case <-time.After(time.Second):
					t.Fatalf("iteration %d: transaction start race did not terminate", iteration)
				}
				if got.mtx == nil || got.state != beginCanceledWithTx || !errors.Is(got.err, context.Canceled) {
					t.Fatalf("iteration %d: result=%+v, want canceled transaction requiring rollback", iteration, got)
				}
				select {
				case <-got.mtx.startWatcherDone:
				default:
					t.Fatalf("iteration %d: transaction start watcher survived beginMutationTx return", iteration)
				}
				rb := rollbackMutation(got.mtx.tx)
				if rb.state != rollbackConfirmed && rb.state != rollbackAlreadyDone {
					t.Fatalf("iteration %d: rollback=%+v", iteration, rb)
				}
				got.mtx.close()
				select {
				case <-state.rollbackDone:
				case <-time.After(time.Second):
					t.Fatalf("iteration %d: rollback did not complete", iteration)
				}
				state.mu.Lock()
				calls := append([]string(nil), state.calls...)
				state.mu.Unlock()
				rollbackCount, commitCount := 0, 0
				for _, call := range calls {
					switch call {
					case "rollback":
						rollbackCount++
					case "commit":
						commitCount++
					}
				}
				if rollbackCount != 1 || commitCount != 0 {
					t.Fatalf("iteration %d: calls=%v, want exactly one rollback and no commit", iteration, calls)
				}
				reacquireCtx, cancelReacquire := context.WithTimeout(t.Context(), time.Second)
				reacquired, err := runner.db.Conn(reacquireCtx)
				cancelReacquire()
				if err != nil {
					t.Fatalf("iteration %d: reacquire dedicated connection: %v", iteration, err)
				}
				if stats := runner.db.Stats(); stats.InUse != 1 {
					t.Fatalf("iteration %d: reacquired connection not checked out: %+v", iteration, stats)
				}
				if err := reacquired.Close(); err != nil {
					t.Fatalf("iteration %d: return reacquired connection: %v", iteration, err)
				}
				if stats := runner.db.Stats(); stats.InUse != 0 {
					t.Fatalf("iteration %d: reacquired dedicated connection not returned: %+v", iteration, stats)
				}
				if err := runner.db.Close(); err != nil {
					t.Fatalf("iteration %d: close iteration database: %v", iteration, err)
				}
			}()
		}
	})
}

func TestOracleMutationStartCancellationKeepsTransactionContextAliveForRollback(t *testing.T) {
	for iteration := range 100 {
		state := &mutationDriverState{
			beginStarted: make(chan struct{}),
			beginRelease: make(chan struct{}),
		}
		runner := mutationRunner(t, state)
		runner.cf.Database.Driver = "oracle"
		baseExecCtx, cancel := context.WithCancel(t.Context())
		execCtx := &observedCancellationContext{Context: baseExecCtx, observed: make(chan struct{})}
		type beginResult struct {
			mtx   *mutationTx
			state beginState
			err   error
		}
		result := make(chan beginResult, 1)
		go func() {
			mtx, state, err := beginMutationTx(t.Context(), execCtx, runner.db, "oracle")
			result <- beginResult{mtx: mtx, state: state, err: err}
		}()
		select {
		case <-state.beginStarted:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: BeginTx did not start", iteration)
		}
		cancel()
		select {
		case <-execCtx.observed:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: start gate watcher did not win cancellation", iteration)
		}
		close(state.beginRelease)
		var got beginResult
		select {
		case got = <-result:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: canceled Oracle mutation did not return", iteration)
		}
		if got.mtx == nil || got.state != beginCanceledWithTx || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("iteration %d: result=%+v, want canceled Oracle transaction", iteration, got)
		}
		failure := runner.canceledMutationStartFailure(got.mtx, got.err)
		if failure == nil || failure.code != "AGENT_QUERY_TIMEOUT" {
			t.Fatalf("iteration %d: failure=%+v", iteration, failure)
		}
		got.mtx.close()
		conn, err := runner.db.Conn(t.Context())
		if err != nil {
			t.Fatalf("iteration %d: reacquire connection: %v", iteration, err)
		}
		_ = conn.Close()
		state.mu.Lock()
		beginCtxErr := state.beginCtx.Err()
		rollbackCtxErr := state.rollbackCtxErr
		calls := append([]string(nil), state.calls...)
		opens, closes := state.opens, state.closes
		state.mu.Unlock()
		if rollbackCtxErr != nil {
			t.Fatalf("iteration %d: Oracle transaction context canceled before rollback: %v", iteration, rollbackCtxErr)
		}
		if got := joinCalls(calls); got != "begin,rollback" {
			t.Fatalf("iteration %d: calls=%s beginCtxErr=%v opens=%d closes=%d, want begin,rollback", iteration, got, beginCtxErr, opens, closes)
		}
		if opens != 2 || closes < 1 {
			t.Fatalf("iteration %d: canceled Oracle transaction connection was reused: opens=%d closes=%d", iteration, opens, closes)
		}
	}
}

func (c *mutationTestConn) ExecContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	c.state.call("exec")
	if c.state.execStarted != nil {
		c.state.execStartOnce.Do(func() { close(c.state.execStarted) })
	}
	if c.state.execRelease != nil {
		select {
		case <-c.state.execRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.state.execErr != nil {
		return nil, c.state.execErr
	}
	return mutationTestResult{state: c.state}, nil
}

func TestRunnerQueuedAndRunningCancellationUsesRealServeConn(t *testing.T) {
	state := &mutationDriverState{rows: 1, execStarted: make(chan struct{}), execRelease: make(chan struct{})}
	r := mutationRunner(t, state)
	maxConcurrent := 1
	r.cf.Runtime.MaxConcurrentRequests = &maxConcurrent
	readonly := false
	cap := CapabilityDef{
		SQL:       "update t set v=1",
		Operation: sqlOperationUpdate,
		Policy:    PolicyDef{Readonly: &readonly, Timeout: "5s", MaxBytes: "128KB"},
	}
	r.cf.Capabilities = map[string]CapabilityDef{"mutate": cap}
	r.caps = r.cf.ByName()

	connCtx, connCancel := context.WithCancel(context.Background())
	defer connCancel()
	accepted := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := ws.Accept(w, req)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		close(accepted)
		r.serveConn(connCtx, conn)
	}))
	defer httpServer.Close()
	client, err := ws.Dial(time.Second, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	<-accepted

	if err := client.WriteText(protocol.MustJSON(protocol.Request{ID: "running", Capability: "mutate", Params: map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-state.execStarted:
	case <-time.After(time.Second):
		t.Fatal("running request did not reach ExecContext")
	}
	if err := client.WriteText(protocol.MustJSON(protocol.Request{ID: "queued", Capability: "mutate", Params: map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteText(protocol.MustJSON(protocol.Request{ID: "busy", Capability: "mutate", Params: map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: "queued"})); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: "running"})); err != nil {
		t.Fatal(err)
	}

	responses := map[string]protocol.Response{}
	for range 2 {
		msg, err := client.ReadText()
		if err != nil {
			t.Fatal(err)
		}
		var response protocol.Response
		if err := json.Unmarshal(msg, &response); err != nil {
			t.Fatal(err)
		}
		responses[response.ID] = response
	}
	if response := responses["running"]; response.Error == nil || response.Error.Code != "AGENT_QUERY_TIMEOUT" {
		t.Fatalf("running cancel response=%+v", response)
	}
	if response := responses["busy"]; response.Error == nil || response.Error.Code != "AGENT_BUSY" {
		t.Fatalf("busy response=%+v", response)
	}
	for _, id := range []string{"running", "busy", "unknown"} {
		if err := client.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: id})); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(time.Second)
	for {
		state.mu.Lock()
		execCalls := 0
		for _, call := range state.calls {
			if call == "exec" {
				execCalls++
			}
		}
		state.mu.Unlock()
		if execCalls == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued canceled request reached DB; calls=%v", state.calls)
		}
		time.Sleep(time.Millisecond)
	}
	close(state.execRelease)
	// Reusing canceled, completed, and queue-busy IDs proves all terminal
	// registry entries were removed rather than retained for the connection.
	for _, id := range []string{"queued", "running", "busy"} {
		if err := client.WriteText(protocol.MustJSON(protocol.Request{ID: id, Capability: "mutate", Params: map[string]any{}})); err != nil {
			t.Fatal(err)
		}
		msg, err := client.ReadText()
		if err != nil {
			t.Fatal(err)
		}
		var response protocol.Response
		if err := json.Unmarshal(msg, &response); err != nil {
			t.Fatal(err)
		}
		if response.ID != id || response.Error != nil || string(response.Result) != `{"count":1}` {
			t.Fatalf("reused %s response=%s", id, msg)
		}
	}
	state.mu.Lock()
	calls := append([]string(nil), state.calls...)
	state.mu.Unlock()
	if got := joinCalls(calls); got != "begin,exec,rollback,begin,exec,rows,commit,begin,exec,rows,commit,begin,exec,rows,commit" {
		t.Fatalf("transaction calls=%s", got)
	}
}

func TestRunnerConnectionCloseCancelsAndRollsBackInflightMutation(t *testing.T) {
	state := &mutationDriverState{rows: 1, execStarted: make(chan struct{}), execRelease: make(chan struct{})}
	r := mutationRunner(t, state)
	maxConcurrent := 1
	r.cf.Runtime.MaxConcurrentRequests = &maxConcurrent
	readonly := false
	cap := CapabilityDef{SQL: "update t set v=1", Operation: sqlOperationUpdate, Policy: PolicyDef{Readonly: &readonly, Timeout: "5s", MaxBytes: "128KB"}}
	r.cf.Capabilities = map[string]CapabilityDef{"mutate": cap}
	r.caps = r.cf.ByName()

	done := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := ws.Accept(w, req)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		r.serveConn(context.Background(), conn)
		close(done)
	}))
	defer httpServer.Close()
	client, err := ws.Dial(time.Second, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WriteText(protocol.MustJSON(protocol.Request{ID: "closing", Capability: "mutate", Params: map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-state.execStarted:
	case <-time.After(time.Second):
		t.Fatal("request did not reach ExecContext")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveConn did not stop after connection close")
	}
	state.mu.Lock()
	calls := append([]string(nil), state.calls...)
	state.mu.Unlock()
	if got := joinCalls(calls); got != "begin,exec,rollback" {
		t.Fatalf("connection-close calls=%s", got)
	}
}

type mutationTestTx struct{ state *mutationDriverState }

func (t *mutationTestTx) Commit() error { t.state.call("commit"); return t.state.commitErr }
func (t *mutationTestTx) Rollback() error {
	t.state.call("rollback")
	t.state.rollbackCtxErr = t.state.beginCtx.Err()
	if t.state.rollbackStarted != nil {
		t.state.rollbackStartOnce.Do(func() { close(t.state.rollbackStarted) })
	}
	if t.state.rollbackRelease != nil {
		<-t.state.rollbackRelease
	}
	if t.state.rollbackDone != nil {
		t.state.rollbackDoneOnce.Do(func() { close(t.state.rollbackDone) })
	}
	return t.state.rollbackErr
}

type mutationTestResult struct{ state *mutationDriverState }

func (r mutationTestResult) LastInsertId() (int64, error) { return 0, nil }
func (r mutationTestResult) RowsAffected() (int64, error) {
	r.state.call("rows")
	return r.state.rows, r.state.rowsErr
}

func mutationRunner(t *testing.T, state *mutationDriverState) *Runner {
	t.Helper()
	runner := mutationRunnerWithoutCleanup(t, state)
	t.Cleanup(func() { _ = runner.db.Close() })
	return runner
}

func mutationRunnerWithoutCleanup(t *testing.T, state *mutationDriverState) *Runner {
	t.Helper()
	name := fmt.Sprintf("onprest-mutation-test-%d", mutationDriverSequence.Add(1))
	sql.Register(name, mutationTestDriver{state: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{db: db, cf: &CapabilityFile{Database: DatabaseDef{Driver: "postgres"}}, detailLog: io.Discard}
}

func TestExecuteMutationTransactionOrderingAndFailures(t *testing.T) {
	tests := []struct {
		name      string
		state     mutationDriverState
		cancel    bool
		maxBytes  string
		wantCode  string
		wantCalls string
		discard   bool
	}{
		{name: "success zero", state: mutationDriverState{rows: 0}, wantCalls: "begin,exec,rows,commit"},
		{name: "rows error", state: mutationDriverState{rowsErr: errors.New("rows")}, wantCode: "AGENT_QUERY_FAILED", wantCalls: "begin,exec,rows,rollback"},
		{name: "negative rows", state: mutationDriverState{rows: -1}, wantCode: "AGENT_QUERY_FAILED", wantCalls: "begin,exec,rows,rollback"},
		{name: "max bytes before commit", state: mutationDriverState{rows: 1}, maxBytes: "1B", wantCode: "AGENT_QUERY_FAILED", wantCalls: "begin,exec,rows,rollback"},
		{name: "exec connection lost", state: mutationDriverState{execErr: errors.New("connection reset by peer")}, wantCode: "AGENT_DB_UNREACHABLE", wantCalls: "begin,exec,rollback"},
		{name: "exec connection lost rollback unknown", state: mutationDriverState{execErr: errors.New("connection reset by peer"), rollbackErr: errors.New("rollback")}, wantCode: errorOutcomeUnknown, wantCalls: "begin,exec,rollback", discard: true},
		{name: "rollback unknown", state: mutationDriverState{execErr: errors.New("exec"), rollbackErr: errors.New("rollback")}, wantCode: errorOutcomeUnknown, wantCalls: "begin,exec,rollback", discard: true},
		{name: "rollback tx done unknown", state: mutationDriverState{execErr: errors.New("exec"), rollbackErr: sql.ErrTxDone}, wantCode: errorOutcomeUnknown, wantCalls: "begin,exec,rollback", discard: true},
		{name: "commit unknown", state: mutationDriverState{rows: 2, commitErr: errors.New("commit")}, wantCode: errorOutcomeUnknown, wantCalls: "begin,exec,rows,commit", discard: true},
		{name: "cancel before pool acquisition", state: mutationDriverState{}, cancel: true, wantCode: "AGENT_QUERY_TIMEOUT", wantCalls: ""},
	}
	for i := range tests {
		tc := &tests[i]
		t.Run(tc.name, func(t *testing.T) {
			r := mutationRunner(t, &tc.state)
			requestCtx := context.Background()
			execCtx, cancel := context.WithCancel(requestCtx)
			if tc.cancel {
				cancel()
			} else {
				defer cancel()
			}
			maxBytes := tc.maxBytes
			if maxBytes == "" {
				maxBytes = "128KB"
			}
			payload, count, failure := r.executeMutation(requestCtx, execCtx, CapabilityDef{Policy: PolicyDef{MaxBytes: maxBytes}}, "update t set v=1", nil)
			if tc.wantCode == "" {
				if failure != nil || string(payload) != "{\"count\":0}" || count != 0 {
					t.Fatalf("payload=%s count=%d failure=%v", payload, count, failure)
				}
			} else if failure == nil || failure.code != tc.wantCode {
				t.Fatalf("failure=%v want=%s", failure, tc.wantCode)
			}
			tc.state.mu.Lock()
			calls := append([]string(nil), tc.state.calls...)
			rollbackCtxErr := tc.state.rollbackCtxErr
			tc.state.mu.Unlock()
			if got := joinCalls(calls); got != tc.wantCalls {
				t.Fatalf("calls=%s want=%s", got, tc.wantCalls)
			}
			if rollbackCtxErr != nil {
				t.Fatalf("transaction lifetime context was canceled before rollback completed: %v", rollbackCtxErr)
			}
			if tc.discard {
				conn, err := r.db.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.Close()
				tc.state.mu.Lock()
				opens := tc.state.opens
				closes := tc.state.closes
				tc.state.mu.Unlock()
				if opens != 2 || closes < 1 {
					t.Fatalf("outcome-unknown connection was reused: opens=%d closes=%d", opens, closes)
				}
			}
		})
	}
}

func TestExecuteMutationDBDisconnectClassification(t *testing.T) {
	tests := []struct {
		name        string
		state       mutationDriverState
		wantCode    string
		wantMessage string
		wantCalls   string
	}{
		{
			name:        "begin disconnect",
			state:       mutationDriverState{beginErr: errors.New("connection reset by peer")},
			wantCode:    "AGENT_DB_UNREACHABLE",
			wantMessage: "database is unreachable",
			wantCalls:   "begin",
		},
		{
			name:        "exec disconnect rollback confirmed",
			state:       mutationDriverState{execErr: driver.ErrBadConn},
			wantCode:    "AGENT_DB_UNREACHABLE",
			wantMessage: "database is unreachable",
			wantCalls:   "begin,exec,rollback",
		},
		{
			name:        "exec disconnect rollback unknown",
			state:       mutationDriverState{execErr: errors.New("connection is closed"), rollbackErr: errors.New("rollback connection lost")},
			wantCode:    errorOutcomeUnknown,
			wantMessage: "transaction outcome is unknown",
			wantCalls:   "begin,exec,rollback",
		},
	}
	for i := range tests {
		tc := &tests[i]
		t.Run(tc.name, func(t *testing.T) {
			r := mutationRunner(t, &tc.state)
			_, _, failure := r.executeMutation(t.Context(), t.Context(), CapabilityDef{Policy: PolicyDef{MaxBytes: "128KB"}}, "update t set v=1", nil)
			if failure == nil || failure.code != tc.wantCode || failure.message != tc.wantMessage {
				t.Fatalf("failure=%+v want code=%s message=%q", failure, tc.wantCode, tc.wantMessage)
			}
			tc.state.mu.Lock()
			calls := append([]string(nil), tc.state.calls...)
			tc.state.mu.Unlock()
			if got := joinCalls(calls); got != tc.wantCalls {
				t.Fatalf("calls=%s want=%s", got, tc.wantCalls)
			}
		})
	}
}

func TestDatabaseSQLAutomaticRollbackRaceIsOutcomeUnknownAndWaitsForDriver(t *testing.T) {
	for iteration := range 50 {
		t.Run(fmt.Sprintf("iteration-%d", iteration), func(t *testing.T) {
			state := &mutationDriverState{
				rollbackStarted: make(chan struct{}),
				rollbackRelease: make(chan struct{}),
				rollbackDone:    make(chan struct{}),
			}
			runner := mutationRunner(t, state)
			runner.db.SetMaxOpenConns(1)
			beginCtx, cancelBegin := context.WithCancel(t.Context())
			tx, err := runner.db.BeginTx(beginCtx, &sql.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}

			cancelBegin()
			select {
			case <-state.rollbackStarted:
			case <-time.After(time.Second):
				t.Fatal("database/sql automatic rollback did not reach driver.Rollback")
			}

			rbResult := make(chan rollbackResult, 1)
			go func() { rbResult <- rollbackMutation(tx) }()
			var rb rollbackResult
			select {
			case rb = <-rbResult:
			case <-time.After(time.Second):
				t.Fatal("explicit Rollback did not return sql.ErrTxDone during automatic rollback")
			}
			if rb.state != rollbackAlreadyDone || !errors.Is(rb.err(), sql.ErrTxDone) {
				t.Fatalf("rollback result=%+v, want unconfirmed sql.ErrTxDone", rb)
			}
			failure := mutationFailureForRollback(context.Canceled, rb, "AGENT_QUERY_TIMEOUT", queryTimeoutDetail)
			if failure.code != errorOutcomeUnknown {
				t.Fatalf("failure=%+v, want %s", failure, errorOutcomeUnknown)
			}
			select {
			case <-state.rollbackDone:
				t.Fatal("driver.Rollback completed before its release channel")
			default:
			}

			connResult := make(chan error, 1)
			go func() {
				conn, err := runner.db.Conn(t.Context())
				if err == nil {
					err = conn.Close()
				}
				connResult <- err
			}()
			select {
			case err := <-connResult:
				t.Fatalf("database/sql reused connection before driver.Rollback completed: %v", err)
			case <-time.After(5 * time.Millisecond):
			}
			close(state.rollbackRelease)
			select {
			case <-state.rollbackDone:
			case <-time.After(time.Second):
				t.Fatal("driver.Rollback did not complete after release")
			}
			select {
			case err := <-connResult:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("database/sql did not release connection after driver.Rollback")
			}

			state.mu.Lock()
			calls := append([]string(nil), state.calls...)
			state.mu.Unlock()
			if got := joinCalls(calls); got != "begin,rollback" {
				t.Fatalf("transaction calls=%s, want exactly one Rollback and no Commit", got)
			}
		})
	}
}

func joinCalls(calls []string) string {
	var out string
	for i, c := range calls {
		if i > 0 {
			out += ","
		}
		out += c
	}
	return out
}

func TestClassifyDBConstraintErrors(t *testing.T) {
	for _, tc := range []struct {
		driver string
		err    error
	}{
		{"postgres", &pgconn.PgError{Code: "23505"}},
		{"mysql", &mysql.MySQLError{Number: 1062}},
		{"sqlserver", mssql.Error{Number: 2627}},
		{"oracle", network.NewOracleError(1)},
	} {
		if got := classifyDBError(tc.driver, tc.err); got != errorConstraintViolation {
			t.Fatalf("%s constraint=%q", tc.driver, got)
		}
	}
	if got := classifyDBError("postgres", &pgconn.PgError{Code: "42000"}); got != "" {
		t.Fatalf("non-constraint=%q", got)
	}
}
