package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/viewlegacy/onprest/internal/sqlsyntax"
)

func buildSQL(driver, query string, params map[string]any) (string, []any, error) {
	if err := validateSQLShape(query); err != nil {
		return "", nil, err
	}
	args := []any{}
	out := make([]byte, 0, len(query)+16)
	i := 0
	for p := 0; p < len(query); {
		ch := query[p]
		if next, ok := protectedSQLRegionEnd(driver, query, p); ok {
			out = append(out, query[p:next]...)
			p = next
			continue
		}
		if ch == ':' {
			if p+1 < len(query) && query[p+1] == ':' {
				out = append(out, "::"...)
				p += 2
				continue
			}
			name, n := readParamName(query[p+1:])
			if n > 0 {
				v, ok := params[name]
				if !ok {
					return "", nil, fmt.Errorf("missing sql param: %s", name)
				}
				args = append(args, v)
				i++
				out = append(out, placeholder(driver, i)...)
				p += 1 + n
				continue
			}
		}
		out = append(out, ch)
		p++
	}
	return string(out), args, nil
}

func readParamName(s string) (string, int) { return sqlsyntax.ParameterName(s) }

func placeholder(driver string, idx int) string {
	switch driver {
	case "postgres":
		return fmt.Sprintf("$%d", idx)
	case "sqlserver":
		return fmt.Sprintf("@p%d", idx)
	case "oracle":
		return fmt.Sprintf(":%d", idx)
	default:
		return "?"
	}
}

func validateSQLShape(query string) error {
	if query == "" {
		return errors.New("sql is empty")
	}
	return nil
}

func buildExplainSQL(driver, query string) (string, bool) {
	switch driver {
	case "postgres", "mysql":
		return "EXPLAIN " + query, true
	case "oracle":
		return "EXPLAIN PLAN FOR " + query, true
	default:
		return "", false
	}
}

func queryRows(ctx context.Context, db *sql.DB, q string, args []any, maxRows int) ([]map[string]any, []string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	result := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := map[string]any{}
		for i, col := range cols {
			if b, ok := values[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = values[i]
			}
		}
		result = append(result, row)
		if maxRows > 0 && len(result) >= maxRows {
			break
		}
	}
	return result, cols, rows.Err()
}
