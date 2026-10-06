package agent

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type dbProvider struct {
	db       *sql.DB
	database DatabaseDef
	cap      *contract.Capability
}

func prepareDBProvider(ctx context.Context, snapshot *providers.Snapshot) (providers.Provider, error) {
	return prepareDBProviderObserved(ctx, snapshot, nil)
}
func prepareDBProviderObserved(ctx context.Context, snapshot *providers.Snapshot, observe preparationObserver) (providers.Provider, error) {
	cap := snapshot.Definition()
	source, ok := snapshot.Source(providers.SourceKey{Group: "databases", Name: cap.Executor.Source})
	if !ok {
		return nil, errors.New("database source is unavailable")
	}
	database := snapshotDatabase(source)
	db, err := openDatabaseForPreparation(database)
	if err != nil {
		return nil, &dbPrepareError{validationStageDatabaseOpen, err}
	}
	if observe != nil {
		observe(validationStageDatabaseOpen)
	}
	p := &dbProvider{db: db, database: database, cap: cap}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		return p, &dbPrepareError{validationStageDatabasePing, err}
	}
	if observe != nil {
		observe(validationStageDatabasePing)
	}
	params := schemaDummyParams(cap.Input)
	query, args, err := buildSQL(database.Driver, cap.Executor.Settings["sql"].(string), params)
	if err != nil {
		return p, err
	}
	d, _ := time.ParseDuration(cap.Policy["timeout"].(string))
	explainCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	err = explainQuery(explainCtx, db, database.Driver, query, args)
	if err != nil {
		return p, &dbPrepareError{validationStageCapabilityExplain, err}
	}
	if observe != nil {
		observe(validationStageCapabilityExplain)
	}
	return p, nil
}
func (p *dbProvider) Close() error { return closeDatabaseForPreparation(p.db) }
func (p *dbProvider) Execute(ctx context.Context, input map[string]any, effects *providers.Effects) (contract.ProviderResult, error) {
	if p.cap.Executor.Operation != "select" {
		return contract.ProviderResult{}, errors.New("mutation requires public validation")
	}
	effects.NoChange()
	q, a, e := buildSQL(p.database.Driver, p.cap.Executor.Settings["sql"].(string), input)
	if e != nil {
		return contract.ProviderResult{}, e
	}
	rows, columns, e := queryDBValues(ctx, p.db, q, a, p.cap.Policy["max_rows"].(int64))
	if e != nil {
		return contract.ProviderResult{}, p.failure(e, ctx)
	}
	count := int64(len(rows))
	return contract.ProviderResult{Data: rows, Columns: columns, ReturnedRows: &count}, nil
}
func (p *dbProvider) ExecutePublication(ctx context.Context, input map[string]any, effects *providers.Effects, publish func(contract.ProviderResult) (*contract.PreparedOutput, error)) (*contract.PreparedOutput, error) {
	if p.cap.Executor.Operation == "select" {
		r, e := p.Execute(ctx, input, effects)
		if e != nil {
			return nil, e
		}
		return publish(r)
	}
	q, a, e := buildSQL(p.database.Driver, p.cap.Executor.Settings["sql"].(string), input)
	if e != nil {
		effects.NoChange()
		return nil, e
	}
	mt, state, e := beginMutationTx(ctx, ctx, p.db, p.database.Driver)
	var effect *providers.Effect
	if state != beginNotStarted {
		effect = effects.Begin()
	}
	if e != nil && state != beginCanceledWithTx {
		if state == beginOutcomeUnknown {
			return nil, &providers.Failure{Code: "OUTCOME_UNKNOWN", Reason: "TRANSACTION_START_UNKNOWN", Cause: e}
		}
		effects.NoChange()
		return nil, p.failure(e, ctx)
	}
	if effect == nil {
		effect = effects.Begin()
	}
	terminal := false
	defer func() {
		if !terminal {
			_ = mt.tx.Rollback()
		}
		mt.close()
	}()
	rollback := func(cause error) (*contract.PreparedOutput, error) {
		rb := rollbackMutation(mt.tx)
		terminal = true
		if rb.state != rollbackConfirmed {
			mt.discardConnection()
			return nil, &providers.Failure{Code: "OUTCOME_UNKNOWN", Reason: "ROLLBACK_UNKNOWN", Cause: errors.Join(cause, rb.err())}
		}
		effect.RolledBack()
		if state == beginCanceledWithTx && p.database.Driver == "oracle" {
			mt.discardConnection()
		}
		return nil, cause
	}
	if state == beginCanceledWithTx {
		return rollback(p.failure(ctx.Err(), ctx))
	}
	result, e := mt.tx.ExecContext(ctx, q, a...)
	if e != nil {
		return rollback(p.failure(e, ctx))
	}
	count, e := result.RowsAffected()
	if e != nil || count < 0 {
		return rollback(p.failure(errors.Join(e, errors.New("invalid affected count")), ctx))
	}
	if limit, ok := p.cap.Policy["max_affected_rows"].(int64); ok && count > limit {
		return rollback(&providers.Failure{Code: "LIMIT_EXCEEDED", Reason: "AFFECTED_ROWS_EXCEEDED", Cause: errors.New("affected row limit exceeded")})
	}
	out, e := publish(contract.ProviderResult{Data: map[string]any{"count": count}, AffectedRows: &count})
	if e != nil {
		return rollback(e)
	}
	if e = ctx.Err(); e != nil {
		_ = out.Close()
		return rollback(p.failure(e, ctx))
	}
	if e = mt.tx.Commit(); e != nil {
		terminal = true
		mt.discardConnection()
		_ = out.Close()
		return nil, &providers.Failure{Code: "OUTCOME_UNKNOWN", Reason: "COMMIT_UNKNOWN", Cause: e}
	}
	terminal = true
	effect.Committed()
	return out, nil
}

type dbPrepareError struct {
	stage validationStage
	cause error
}

func (e *dbPrepareError) Error() string { return e.cause.Error() }
func (e *dbPrepareError) Unwrap() error { return e.cause }
func (p *dbProvider) failure(err error, ctx context.Context) error {
	var output *contract.OutputError
	if errors.As(err, &output) {
		return err
	}
	code, reason := "EXECUTION_FAILED", "DATABASE_QUERY_FAILED"
	if ctx.Err() != nil {
		code, reason = "EXECUTION_TIMEOUT", "DATABASE_TIMEOUT"
	} else if classifyDBError(p.database.Driver, err) != "" {
		code, reason = "CONFLICT", "DATABASE_CONSTRAINT"
	} else if isDBUnreachable(err) {
		code, reason = "DEPENDENCY_UNAVAILABLE", "DATABASE_UNAVAILABLE"
	}
	return &providers.Failure{Code: code, Reason: reason, Cause: err}
}
func schemaDummyParams(s *contract.Schema) map[string]any {
	m := map[string]any{}
	for name, v := range s.Properties {
		if v.DefaultSet {
			m[name] = v.Default
			continue
		}
		if values, ok := v.Constraints["enum"].([]any); ok && len(values) > 0 {
			m[name] = values[0]
			continue
		}
		if value, ok := v.Constraints["const"]; ok {
			m[name] = value
			continue
		}
		switch {
		case schemaHasType(v, "integer"):
			m[name] = int64(1)
		case schemaHasType(v, "number"):
			m[name] = float64(1)
		case schemaHasType(v, "boolean"):
			m[name] = true
		default:
			m[name] = "example"
		}
	}
	return m
}
func schemaHasType(s *contract.Schema, kind string) bool {
	for _, t := range s.Types {
		if t == kind {
			return true
		}
	}
	return false
}

func queryDBValues(ctx context.Context, db *sql.DB, q string, args []any, maxRows int64) ([]any, []string, error) {
	rows, e := db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, nil, e
	}
	defer rows.Close()
	cols, e := rows.Columns()
	if e != nil {
		return nil, nil, e
	}
	types, e := rows.ColumnTypes()
	if e != nil {
		return nil, nil, e
	}
	seen := map[string]bool{}
	for _, name := range cols {
		if seen[name] || !utf8.ValidString(name) {
			return nil, nil, &contract.OutputError{Code: "OUTPUT_INVALID"}
		}
		seen[name] = true
	}
	result := []any{}
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if e := rows.Scan(ptrs...); e != nil {
			return nil, nil, e
		}
		row := map[string]any{}
		for i, name := range cols {
			kind := types[i].DatabaseTypeName()
			if strings.EqualFold(kind, "NUMBER") {
				_, scale, known := types[i].DecimalSize()
				if known && scale == 0 {
					kind = "ORACLE_INTEGER"
				}
			}
			v, e := normalizeDBValue(values[i], kind)
			if e != nil {
				return nil, nil, &contract.OutputError{Code: "OUTPUT_INVALID"}
			}
			row[name] = v
		}
		result = append(result, row)
		if int64(len(result)) >= maxRows {
			break
		}
	}
	if e := rows.Err(); e != nil {
		return nil, nil, e
	}
	if e := rows.Close(); e != nil {
		return nil, nil, e
	}
	return result, cols, nil
}
func normalizeDBValue(v any, dbType string) (any, error) {
	if v == nil {
		return nil, nil
	}
	kind := strings.ToUpper(dbType)
	binary := kind == "BYTEA" || kind == "BINARY" || kind == "VARBINARY" || kind == "IMAGE" || kind == "RAW" || kind == "LONG RAW" || strings.Contains(kind, "BLOB")
	if kind == "ORACLE_INTEGER" {
		switch value := v.(type) {
		case string:
			return strconv.ParseInt(value, 10, 64)
		case []byte:
			return strconv.ParseInt(string(value), 10, 64)
		case int64:
			return value, nil
		}
		return nil, errors.New("integer database value has no exact int64 representation")
	}
	decimal := strings.Contains(kind, "DECIMAL") || strings.Contains(kind, "NUMERIC") || kind == "NUMBER"
	if binary {
		switch value := v.(type) {
		case []byte:
			return base64.StdEncoding.EncodeToString(value), nil
		case string:
			return base64.StdEncoding.EncodeToString([]byte(value)), nil
		default:
			return nil, errors.New("invalid binary database value")
		}
	}
	if decimal {
		switch value := v.(type) {
		case []byte:
			if !utf8.Valid(value) {
				return nil, errors.New("invalid decimal encoding")
			}
			return string(value), nil
		case string:
			return value, nil
		case int64:
			return strconv.FormatInt(value, 10), nil
		}
		return nil, errors.New("decimal database value has no exact representation")
	}
	switch value := v.(type) {
	case []byte:
		if !utf8.Valid(value) {
			return nil, errors.New("invalid database text encoding")
		}
		return string(value), nil
	case string:
		if !utf8.ValidString(value) {
			return nil, errors.New("invalid database text encoding")
		}
		return value, nil
	case time.Time:
		return fmt.Sprint(value), nil
	case int64, bool, float64:
		return value, nil
	case int32:
		return int64(value), nil
	case int:
		return int64(value), nil
	default:
		return nil, fmt.Errorf("unsupported database value type %T", v)
	}
}
