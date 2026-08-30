package agent

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
)

const queryTimeoutDetail = "query exceeded policy.timeout"

func (r *Runner) handle(parent context.Context, req protocol.Request) protocol.Response {
	if req.Capability == "meta" {
		return protocol.ResultResponse(req.ID, map[string]any{"data": BuildOpenAPI(r.cf), "response_kinds": responseKinds(r.cf)})
	}
	cap, ok := r.caps[req.Capability]
	if !ok {
		return r.errorResponse(req, "GATEWAY_CAPABILITY_NOT_FOUND", "capability is not defined", "capability is not defined")
	}
	params, err := validateParams(cap, req.Params)
	if err != nil {
		return r.errorResponse(req, "AGENT_VALIDATION_FAILED", err.Error(), err.Error())
	}
	query, args, err := buildSQL(r.cf.Database.Driver, cap.SQL, params)
	if err != nil {
		return r.errorResponse(req, "AGENT_VALIDATION_FAILED", err.Error(), err.Error())
	}
	d, err := timeout(cap.Policy)
	if err != nil {
		return r.errorResponse(req, "AGENT_INTERNAL_ERROR", "agent internal error", err.Error())
	}
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	if cap.Operation.mutation() {
		payload, _, failure := r.executeMutation(parent, ctx, cap, query, args)
		if failure != nil {
			return r.errorResponse(req, failure.code, failure.message, failure.detail)
		}
		return protocol.Response{ID: req.ID, Result: payload}
	}
	return r.executeSelect(ctx, parent, req, cap, query, args)
}

func responseKinds(cf *CapabilityFile) map[string]string {
	kinds := make(map[string]string, len(cf.Capabilities))
	for name, cap := range cf.Capabilities {
		if cap.Operation.mutation() {
			kinds[name] = "mutation"
		} else {
			kinds[name] = "select"
		}
	}
	return kinds
}

func (r *Runner) executeSelect(ctx, parent context.Context, req protocol.Request, cap CapabilityDef, query string, args []any) protocol.Response {
	rows, cols, err := queryRows(ctx, r.db, query, args, resolvedMaxRows(cap.Policy))
	if err != nil {
		if ctx.Err() != nil {
			return r.errorResponse(req, "AGENT_QUERY_TIMEOUT", queryTimeoutDetail, queryTimeoutDetail)
		}
		if classifyDBError(r.cf.Database.Driver, err) == errorConstraintViolation {
			return r.errorResponse(req, errorConstraintViolation, "database constraint violation", err.Error())
		}
		if isDBUnreachable(err) || !r.dbReachable(parent) {
			return r.errorResponse(req, "AGENT_DB_UNREACHABLE", "database is unreachable", err.Error())
		}
		return r.errorResponse(req, "AGENT_QUERY_FAILED", "database query failed", err.Error())
	}
	rows, err = applyResultContract(rows, cols, cap.Result)
	if err != nil {
		return r.errorResponse(req, "AGENT_QUERY_FAILED", "database query failed", err.Error())
	}
	result := map[string]any{"rows": rows, "count": int64(len(rows))}
	limit, err := maxBytes(cap.Policy)
	if err != nil {
		return r.errorResponse(req, "AGENT_INTERNAL_ERROR", "agent internal error", err.Error())
	}
	if b, err := json.Marshal(result); err != nil {
		return r.errorResponse(req, "AGENT_INTERNAL_ERROR", "agent internal error", err.Error())
	} else if int64(len(b)) > limit {
		return r.errorResponse(req, "AGENT_QUERY_FAILED", "response exceeds policy.max_bytes", "response exceeds policy.max_bytes")
	}
	return protocol.ResultResponse(req.ID, result)
}

type mutationFailure struct{ code, message, detail string }

type beginState int

const (
	beginNotStarted beginState = iota
	beginStarted
	beginCanceledWithTx
	beginOutcomeUnknown
)

type mutationTx struct {
	conn             *sql.Conn
	tx               *sql.Tx
	startWatcherDone <-chan struct{}
	releaseOnce      sync.Once
	release          context.CancelFunc
	discard          bool
}

func (m *mutationTx) discardConnection() {
	if m != nil {
		m.discard = true
	}
}

func (m *mutationTx) close() {
	if m == nil {
		return
	}
	m.releaseOnce.Do(func() {
		m.release()
		if m.discard {
			discardSQLConn(m.conn)
		}
		_ = m.conn.Close()
	})
}

func discardSQLConn(conn *sql.Conn) {
	if conn == nil {
		return
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

// beginMutationTx bounds both pool acquisition and driver transaction startup
// with execCtx without attaching the execution deadline to the established
// transaction. The gate is disarmed atomically when BeginTx succeeds, leaving
// the transaction alive until explicit commit/rollback completes.
func beginMutationTx(requestCtx, execCtx context.Context, db *sql.DB, driverName string) (*mutationTx, beginState, error) {
	conn, err := db.Conn(execCtx)
	if err != nil {
		return nil, beginNotStarted, err
	}

	lifetimeCtx, releaseLifetime := context.WithCancel(context.WithoutCancel(requestCtx))
	startCtx, cancelStart := context.WithCancelCause(lifetimeCtx)
	var gate atomic.Int32 // 0=pending, 1=started, 2=execution canceled
	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	var stopOnce sync.Once
	stopWatcher := func() {
		stopOnce.Do(func() { close(stop) })
		<-watcherDone
	}
	go func() {
		defer close(watcherDone)
		select {
		case <-execCtx.Done():
			if gate.CompareAndSwap(0, 2) {
				cancelStart(execCtx.Err())
			}
		case <-stop:
		}
	}()
	if err := execCtx.Err(); err != nil && gate.CompareAndSwap(0, 2) {
		cancelStart(err)
	}

	beginCtx := context.Context(startCtx)
	if driverName == "oracle" {
		// go-ora starts a transaction without network I/O. Keeping its
		// database/sql transaction context detached from the execution deadline
		// prevents a late cancellation break from poisoning the next operation.
		beginCtx = lifetimeCtx
	}
	tx, err := conn.BeginTx(beginCtx, &sql.TxOptions{Isolation: sql.LevelDefault})
	if err == nil {
		startedFirst := gate.CompareAndSwap(0, 1)
		stopWatcher()
		m := &mutationTx{
			conn:             conn,
			tx:               tx,
			startWatcherDone: watcherDone,
			release: func() {
				cancelStart(context.Canceled)
				releaseLifetime()
			},
		}
		if startedFirst {
			return m, beginStarted, nil
		}
		return m, beginCanceledWithTx, execCtx.Err()
	}

	canceledFirst := gate.Load() == 2
	if !canceledFirst {
		gate.CompareAndSwap(0, 1)
	}
	stopWatcher()
	cancelStart(context.Canceled)
	releaseLifetime()
	if canceledFirst {
		discardSQLConn(conn)
	}
	_ = conn.Close()
	if canceledFirst {
		return nil, beginOutcomeUnknown, err
	}
	return nil, beginNotStarted, err
}

func (r *Runner) executeMutation(requestCtx, execCtx context.Context, cap CapabilityDef, query string, args []any) (json.RawMessage, int64, *mutationFailure) {
	mtx, state, err := beginMutationTx(requestCtx, execCtx, r.db, r.cf.Database.Driver)
	if err != nil {
		if state == beginOutcomeUnknown {
			return nil, 0, &mutationFailure{errorOutcomeUnknown, "transaction outcome is unknown", err.Error()}
		}
		if execCtx.Err() != nil {
			return nil, 0, &mutationFailure{"AGENT_QUERY_TIMEOUT", queryTimeoutDetail, execCtx.Err().Error()}
		}
		if isDBUnreachable(err) {
			return nil, 0, &mutationFailure{"AGENT_DB_UNREACHABLE", "database is unreachable", err.Error()}
		}
		return nil, 0, &mutationFailure{"AGENT_QUERY_FAILED", "database query failed", err.Error()}
	}
	tx := mtx.tx
	terminal := false
	defer func() {
		if !terminal {
			_ = tx.Rollback()
		}
		mtx.close()
	}()
	failAfterRollback := func(original error, code, message string) (json.RawMessage, int64, *mutationFailure) {
		failure := rollbackMutationFailure(mtx, original, code, message, false)
		terminal = true
		return nil, 0, failure
	}
	if state == beginCanceledWithTx {
		failure := r.canceledMutationStartFailure(mtx, execCtx.Err())
		terminal = true
		return nil, 0, failure
	}
	result, err := tx.ExecContext(execCtx, query, args...)
	if err != nil {
		if execCtx.Err() != nil {
			return failAfterRollback(execCtx.Err(), "AGENT_QUERY_TIMEOUT", queryTimeoutDetail)
		}
		if classifyDBError(r.cf.Database.Driver, err) == errorConstraintViolation {
			return failAfterRollback(err, errorConstraintViolation, "database constraint violation")
		}
		if isDBUnreachable(err) {
			return failAfterRollback(err, "AGENT_DB_UNREACHABLE", "database is unreachable")
		}
		return failAfterRollback(err, "AGENT_QUERY_FAILED", "database query failed")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return failAfterRollback(err, "AGENT_QUERY_FAILED", "database query failed")
	}
	if count < 0 {
		return failAfterRollback(errors.New("RowsAffected returned a negative count"), "AGENT_QUERY_FAILED", "database query failed")
	}
	limit, err := maxBytes(cap.Policy)
	if err != nil {
		return failAfterRollback(err, "AGENT_INTERNAL_ERROR", "agent internal error")
	}
	payload, err := buildMutationPayload(count, limit)
	if err != nil {
		message := "database query failed"
		if errors.Is(err, errMutationResponseTooLarge) {
			message = "response exceeds policy.max_bytes"
		}
		return failAfterRollback(err, "AGENT_QUERY_FAILED", message)
	}
	if err := execCtx.Err(); err != nil {
		return failAfterRollback(err, "AGENT_QUERY_TIMEOUT", queryTimeoutDetail)
	}
	if err := tx.Commit(); err != nil {
		terminal = true
		mtx.discardConnection()
		return nil, 0, &mutationFailure{errorOutcomeUnknown, "transaction outcome is unknown", err.Error()}
	}
	terminal = true
	return payload, count, nil
}

func (r *Runner) canceledMutationStartFailure(mtx *mutationTx, cause error) *mutationFailure {
	return rollbackMutationFailure(mtx, cause, "AGENT_QUERY_TIMEOUT", queryTimeoutDetail, r.cf.Database.Driver == "oracle")
}

func rollbackMutationFailure(mtx *mutationTx, original error, code, message string, discard bool) *mutationFailure {
	if discard {
		mtx.discardConnection()
	}
	rb := rollbackMutation(mtx.tx)
	if rb.state != rollbackConfirmed {
		mtx.discardConnection()
	}
	return mutationFailureForRollback(original, rb, code, message)
}

func mutationFailureForRollback(original error, rb rollbackResult, code, message string) *mutationFailure {
	if rb.state != rollbackConfirmed {
		return &mutationFailure{errorOutcomeUnknown, "transaction outcome is unknown", errors.Join(original, rb.err()).Error()}
	}
	return &mutationFailure{code, message, original.Error()}
}

var errMutationResponseTooLarge = errors.New("mutation response exceeds policy.max_bytes")

func buildMutationPayload(count, maxBytes int64) (json.RawMessage, error) {
	payload := json.RawMessage(strconv.AppendInt([]byte(`{"count":`), count, 10))
	payload = append(payload, '}')
	if int64(len(payload)) > maxBytes {
		return nil, errMutationResponseTooLarge
	}
	return payload, nil
}

type rollbackState int

const (
	rollbackConfirmed rollbackState = iota
	rollbackAlreadyDone
	rollbackFailed
)

type rollbackResult struct {
	state rollbackState
	cause error
}

func (r rollbackResult) err() error {
	if r.cause != nil {
		return r.cause
	}
	return errors.New("transaction rollback outcome is unknown")
}
func rollbackMutation(tx *sql.Tx) rollbackResult {
	err := tx.Rollback()
	if err == nil {
		return rollbackResult{state: rollbackConfirmed}
	}
	if errors.Is(err, sql.ErrTxDone) {
		return rollbackResult{state: rollbackAlreadyDone, cause: err}
	}
	return rollbackResult{state: rollbackFailed, cause: err}
}

func applyResultContract(rows []map[string]any, cols []string, result ResultDef) ([]map[string]any, error) {
	if len(result) == 0 {
		filtered := make([]map[string]any, len(rows))
		for i := range filtered {
			filtered[i] = map[string]any{}
		}
		return filtered, nil
	}
	available := map[string]struct{}{}
	for _, col := range cols {
		available[col] = struct{}{}
	}
	for name := range result {
		if _, ok := available[name]; !ok {
			return nil, fmt.Errorf("result column %q was not returned by SQL", name)
		}
	}
	filtered := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out := map[string]any{}
		for name, def := range result {
			v, err := coerceResultValue(row[name], def.Type)
			if err != nil {
				return nil, fmt.Errorf("result column %q: %w", name, err)
			}
			out[name] = v
		}
		filtered = append(filtered, out)
	}
	return filtered, nil
}

func coerceResultValue(v any, typ string) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch typ {
	case "integer":
		return coerceInteger(v)
	case "number":
		return coerceNumber(v)
	case "boolean":
		return coerceBoolean(v)
	case "string":
		return coerceString(v), nil
	default:
		return v, nil
	}
}

func coerceInteger(v any) (any, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int8:
		return int64(n), nil
	case int16:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case uint:
		if uint64(n) > math.MaxInt64 {
			return nil, fmt.Errorf("integer overflows int64")
		}
		return int64(n), nil
	case uint8:
		return int64(n), nil
	case uint16:
		return int64(n), nil
	case uint32:
		return int64(n), nil
	case uint64:
		if n > math.MaxInt64 {
			return nil, fmt.Errorf("integer overflows int64")
		}
		return int64(n), nil
	case float32:
		return floatToInteger(float64(n))
	case float64:
		return floatToInteger(n)
	case json.Number:
		return n.Int64()
	case string:
		return strconv.ParseInt(n, 10, 64)
	default:
		return nil, fmt.Errorf("cannot coerce %T to integer", v)
	}
}

func floatToInteger(n float64) (int64, error) {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("number must be finite")
	}
	if math.Trunc(n) != n {
		return 0, fmt.Errorf("number is not an integer")
	}
	const int64UpperExclusive = 9223372036854775808.0
	const int64LowerInclusive = -9223372036854775808.0
	if n >= int64UpperExclusive || n < int64LowerInclusive {
		return 0, fmt.Errorf("integer overflows int64")
	}
	return int64(n), nil
}

func coerceNumber(v any) (any, error) {
	var value float64
	var err error
	switch n := v.(type) {
	case int:
		value = float64(n)
	case int8:
		value = float64(n)
	case int16:
		value = float64(n)
	case int32:
		value = float64(n)
	case int64:
		value = float64(n)
	case uint:
		value = float64(n)
	case uint8:
		value = float64(n)
	case uint16:
		value = float64(n)
	case uint32:
		value = float64(n)
	case uint64:
		value = float64(n)
	case float32:
		value = float64(n)
	case float64:
		value = n
	case json.Number:
		value, err = n.Float64()
	case string:
		value, err = strconv.ParseFloat(n, 64)
	default:
		return nil, fmt.Errorf("cannot coerce %T to number", v)
	}
	if err != nil {
		return nil, err
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("number must be finite")
	}
	return value, nil
}

func coerceBoolean(v any) (any, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case string:
		return strconv.ParseBool(b)
	default:
		return nil, fmt.Errorf("cannot coerce %T to boolean", v)
	}
}

func coerceString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	default:
		return fmt.Sprint(v)
	}
}

func (r *Runner) errorResponse(req protocol.Request, code, message, detail string) protocol.Response {
	r.detailError(req.Capability, code, message, detail, req.ID)
	return protocol.Response{ID: req.ID, Error: &protocol.Error{Code: code, Message: message}}
}

func explainQuery(ctx context.Context, db *sql.DB, driver, query string, args []any) error {
	if driver == "sqlserver" {
		conn, err := db.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "SET SHOWPLAN_TEXT ON"); err != nil {
			return err
		}
		cleanup := func() error {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := conn.ExecContext(cleanupCtx, "SET SHOWPLAN_TEXT OFF")
			return err
		}
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			_ = cleanup()
			return err
		}
		if err := rows.Close(); err != nil {
			_ = cleanup()
			return err
		}
		return cleanup()
	}
	if driver == "oracle" {
		conn, err := db.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		txLifetime, release := context.WithCancel(context.WithoutCancel(ctx))
		defer release()
		tx, err := conn.BeginTx(txLifetime, &sql.TxOptions{Isolation: sql.LevelDefault})
		if err != nil {
			return err
		}
		statementID := fmt.Sprintf("onprest_%x", time.Now().UnixNano())
		_, execErr := tx.ExecContext(ctx, "EXPLAIN PLAN SET STATEMENT_ID = '"+statementID+"' FOR "+query, args...)
		rb := rollbackMutation(tx)
		if rb.state != rollbackConfirmed {
			return fmt.Errorf("oracle explain rollback: %w", rb.err())
		}
		return execErr
	}
	explain, ok := buildExplainSQL(driver, query)
	if !ok {
		return fmt.Errorf("unsupported driver: %s", driver)
	}
	rows, err := db.QueryContext(ctx, explain, args...)
	if err != nil {
		return err
	}
	return rows.Close()
}

func isDBUnreachable(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"connection refused",
		"connection reset",
		"connection timed out",
		"no such host",
		"bad connection",
		"server is not accepting",
		"database system is shutting down",
		"terminating connection",
		"connection is closed",
		"unexpected eof",
	} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}

func (r *Runner) dbReachable(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
	defer cancel()
	return r.db.PingContext(ctx) == nil
}

func driverName(driver string) string {
	if driver == "postgres" {
		return "pgx"
	}
	return driver
}
