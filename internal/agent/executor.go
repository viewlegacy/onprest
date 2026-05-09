package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
)

func (r *Runner) handle(parent context.Context, req protocol.Request) protocol.Response {
	if req.Capability == "meta" {
		return protocol.ResultResponse(req.ID, map[string]any{"data": BuildOpenAPI(r.cf)})
	}
	cap, ok := r.caps[req.Capability]
	if !ok {
		return r.errorResponse(req, "GATEWAY_CAPABILITY_NOT_FOUND", "capability is not defined")
	}
	params, err := validateParams(cap, req.Params)
	if err != nil {
		return r.errorResponse(req, "AGENT_VALIDATION_FAILED", err.Error())
	}
	query, args, err := buildSQL(r.cf.Database.Driver, cap.SQL, params)
	if err != nil {
		return r.errorResponse(req, "AGENT_VALIDATION_FAILED", err.Error())
	}
	d, err := timeout(cap.Policy)
	if err != nil {
		return r.errorResponse(req, "AGENT_INTERNAL_ERROR", err.Error())
	}
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	rows, cols, err := queryRows(ctx, r.db, query, args, cap.Policy.MaxRows)
	if err != nil {
		if ctx.Err() != nil {
			return r.errorResponse(req, "AGENT_QUERY_TIMEOUT", err.Error())
		}
		if isDBUnreachable(err) || !r.dbReachable(parent) {
			return r.errorResponse(req, "AGENT_DB_UNREACHABLE", err.Error())
		}
		return r.errorResponse(req, "AGENT_QUERY_FAILED", err.Error())
	}
	rows, err = applyResultContract(rows, cols, cap.Result)
	if err != nil {
		return r.errorResponse(req, "AGENT_QUERY_FAILED", err.Error())
	}
	result := map[string]any{"rows": rows, "count": len(rows)}
	limit, err := maxBytes(cap.Policy)
	if err != nil {
		return r.errorResponse(req, "AGENT_INTERNAL_ERROR", err.Error())
	}
	if b, err := json.Marshal(result); err != nil {
		return r.errorResponse(req, "AGENT_INTERNAL_ERROR", err.Error())
	} else if int64(len(b)) > limit {
		return r.errorResponse(req, "AGENT_QUERY_FAILED", "response exceeds policy.max_bytes")
	}
	return protocol.ResultResponse(req.ID, result)
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
	if math.Trunc(n) != n {
		return 0, fmt.Errorf("number is not an integer")
	}
	if n > math.MaxInt64 || n < math.MinInt64 {
		return 0, fmt.Errorf("integer overflows int64")
	}
	return int64(n), nil
}

func coerceNumber(v any) (any, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int8:
		return float64(n), nil
	case int16:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case uint:
		return float64(n), nil
	case uint8:
		return float64(n), nil
	case uint16:
		return float64(n), nil
	case uint32:
		return float64(n), nil
	case uint64:
		return float64(n), nil
	case float32:
		return float64(n), nil
	case float64:
		return n, nil
	case json.Number:
		return n.Float64()
	case string:
		return strconv.ParseFloat(n, 64)
	default:
		return nil, fmt.Errorf("cannot coerce %T to number", v)
	}
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

func (r *Runner) errorResponse(req protocol.Request, code, detail string) protocol.Response {
	r.detailError(req.Capability, code, detail, req.ID)
	return protocol.Response{ID: req.ID, Error: &protocol.Error{Code: code, Detail: detail}}
}

func (r *Runner) explainAll(parent context.Context) error {
	for _, cap := range r.cf.CapabilityList() {
		params := dummyParams(cap)
		query, args, err := buildSQL(r.cf.Database.Driver, cap.SQL, params)
		if err != nil {
			return fmt.Errorf("%s explain build: %w", cap.Name, err)
		}
		d, err := timeout(cap.Policy)
		if err != nil {
			return fmt.Errorf("%s timeout: %w", cap.Name, err)
		}
		ctx, cancel := context.WithTimeout(parent, d)
		err = r.explainQuery(ctx, query, args)
		cancel()
		if err != nil {
			return fmt.Errorf("%s explain failed: %w", cap.Name, err)
		}
	}
	return nil
}

func (r *Runner) explainQuery(ctx context.Context, query string, args []any) error {
	if r.cf.Database.Driver == "sqlserver" {
		conn, err := r.db.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "SET SHOWPLAN_TEXT ON"); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), "SET SHOWPLAN_TEXT OFF")
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		return rows.Close()
	}
	explain, ok := buildExplainSQL(r.cf.Database.Driver, query)
	if !ok {
		return fmt.Errorf("unsupported driver: %s", r.cf.Database.Driver)
	}
	rows, err := r.db.QueryContext(ctx, explain, args...)
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
	return driver
}
