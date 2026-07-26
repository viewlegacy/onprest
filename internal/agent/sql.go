package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		if ch == '\'' {
			next := copySingleQuoted(out, query[p:])
			out = next.buf
			p += next.n
			continue
		}
		if ch == '"' {
			next := copyDoubleQuoted(out, query[p:])
			out = next.buf
			p += next.n
			continue
		}
		if ch == '-' && p+1 < len(query) && query[p+1] == '-' {
			next := copyUntilNewline(out, query[p:])
			out = next.buf
			p += next.n
			continue
		}
		if ch == '/' && p+1 < len(query) && query[p+1] == '*' {
			next := copyBlockComment(out, query[p:])
			out = next.buf
			p += next.n
			continue
		}
		if ch == '$' {
			next, ok := copyDollarQuoted(out, query[p:])
			if ok {
				out = next.buf
				p += next.n
				continue
			}
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

type copyResult struct {
	buf []byte
	n   int
}

func copySingleQuoted(out []byte, s string) copyResult {
	out = append(out, s[0])
	for i := 1; i < len(s); i++ {
		out = append(out, s[i])
		if s[i] == '\'' {
			if i+1 < len(s) && s[i+1] == '\'' {
				i++
				out = append(out, s[i])
				continue
			}
			return copyResult{buf: out, n: i + 1}
		}
	}
	return copyResult{buf: out, n: len(s)}
}

func copyDoubleQuoted(out []byte, s string) copyResult {
	out = append(out, s[0])
	for i := 1; i < len(s); i++ {
		out = append(out, s[i])
		if s[i] == '"' {
			if i+1 < len(s) && s[i+1] == '"' {
				i++
				out = append(out, s[i])
				continue
			}
			return copyResult{buf: out, n: i + 1}
		}
	}
	return copyResult{buf: out, n: len(s)}
}

func copyUntilNewline(out []byte, s string) copyResult {
	for i := 0; i < len(s); i++ {
		out = append(out, s[i])
		if s[i] == '\n' {
			return copyResult{buf: out, n: i + 1}
		}
	}
	return copyResult{buf: out, n: len(s)}
}

func copyBlockComment(out []byte, s string) copyResult {
	for i := 0; i < len(s); i++ {
		out = append(out, s[i])
		if i > 0 && s[i-1] == '*' && s[i] == '/' {
			return copyResult{buf: out, n: i + 1}
		}
	}
	return copyResult{buf: out, n: len(s)}
}

func copyDollarQuoted(out []byte, s string) (copyResult, bool) {
	endTagStart := 1
	for endTagStart < len(s) && (isIdent(s[endTagStart]) || isDigit(s[endTagStart])) {
		endTagStart++
	}
	if endTagStart >= len(s) || s[endTagStart] != '$' {
		return copyResult{}, false
	}
	tag := s[:endTagStart+1]
	out = append(out, tag...)
	i := len(tag)
	for i < len(s) {
		if hasPrefixAt(s, i, tag) {
			out = append(out, tag...)
			return copyResult{buf: out, n: i + len(tag)}, true
		}
		out = append(out, s[i])
		i++
	}
	return copyResult{buf: out, n: len(s)}, true
}

func readParamName(s string) (string, int) {
	if len(s) == 0 || !isIdent(s[0]) {
		return "", 0
	}
	i := 1
	for i < len(s) && (isIdent(s[i]) || isDigit(s[i])) {
		i++
	}
	return s[:i], i
}

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

func isIdent(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func hasPrefixAt(s string, idx int, prefix string) bool {
	return idx+len(prefix) <= len(s) && s[idx:idx+len(prefix)] == prefix
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
