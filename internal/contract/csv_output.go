package contract

import (
	"encoding/csv"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode"
)

// WriteCSV accepts already projected business data, not the DB rows/count
// envelope. The writer must be a private bounded stage: late I/O failure never
// authorizes partial publication. ProcessOutput enforces that lifecycle.
func (c *Capability) WriteCSV(w io.Writer, projected any, metadata []string) error {
	value, e := outputPointer(projected, c.Output.Files["from"].(string))
	if e != nil {
		return invalidOutput(e)
	}
	rows, ok := value.([]any)
	if !ok {
		return invalidOutput(errors.New("CSV from must select a nonnull object array"))
	}
	columns, ok := c.Output.Files["columns"].([]string)
	if !ok {
		columns = append([]string{}, metadata...)
		schema := c.Output.Schema
		if from := c.Output.Files["from"].(string); from != "" {
			schema, e = schema.resolveOutputPointer(from)
			if e != nil {
				return invalidOutput(e)
			}
		}
		if schema.Items != nil && contains(schema.Items.Types, "object") && !schema.Items.AdditionalAllowed {
			columns = []string{}
			for _, name := range metadata {
				if _, ok := schema.Items.Properties[name]; ok {
					columns = append(columns, name)
				}
			}
		}
	}
	if len(columns) == 0 {
		return invalidOutput(errors.New("CSV requires at least one published column"))
	}
	if e := validateColumns(columns); e != nil {
		return invalidOutput(e)
	}
	if rowProvider(c) {
		for _, name := range columns {
			if !contains(metadata, name) {
				return invalidOutput(errors.New("CSV column is absent from provider metadata"))
			}
		}
	}
	escape, _ := c.Output.Files["formula_escape"].(bool)
	header := append([]string{}, columns...)
	if escape {
		for i, v := range header {
			header[i] = escapeFormula(v)
		}
	}
	writeRecord := func(record []string) error {
		tail := &csvTerminatorWriter{dest: w}
		writer := csv.NewWriter(tail)
		if e := writer.Write(record); e != nil {
			return invalidOutput(e)
		}
		writer.Flush()
		if e := writer.Error(); e != nil {
			return invalidOutput(e)
		}
		return invalidOutputIfError(tail.finish())
	}
	if e := writeRecord(header); e != nil {
		return e
	}
	for _, value := range rows {
		row, ok := value.(map[string]any)
		if !ok || row == nil {
			return invalidOutput(errors.New("CSV elements must be nonnull objects"))
		}
		record := make([]string, len(columns))
		for i, name := range columns {
			record[i], e = csvCell(row[name], escape)
			if e != nil {
				return invalidOutput(e)
			}
		}
		if e := writeRecord(record); e != nil {
			return e
		}
	}
	return nil
}
func csvCell(v any, escape bool) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		if escape {
			return escapeFormula(x), nil
		}
		return x, nil
	case bool, int64, int, float64:
		b, e := CanonicalJSON(x)
		return string(b), e
	default:
		return "", errors.New("CSV cell must be a JSON scalar")
	}
}
func escapeFormula(v string) string {
	trimmed := strings.TrimLeftFunc(v, unicode.IsSpace)
	leading := v[:len(v)-len(trimmed)]
	if strings.ContainsAny(leading, "\t\r") || len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + v
	}
	return v
}
func outputPointer(v any, pointer string) (any, error) {
	if pointer == "" {
		return v, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("invalid CSV pointer")
	}
	for _, raw := range strings.Split(pointer[1:], "/") {
		token, e := unescapePointer(raw)
		if e != nil {
			return nil, e
		}
		switch cur := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = cur[token]
			if !ok {
				return nil, errors.New("CSV pointer is missing")
			}
		case []any:
			if token == "" || len(token) > 1 && token[0] == '0' {
				return nil, errors.New("invalid CSV array index")
			}
			for _, r := range token {
				if r < '0' || r > '9' {
					return nil, errors.New("invalid CSV array index")
				}
			}
			index, e := strconv.ParseUint(token, 10, 63)
			if e != nil || index >= uint64(len(cur)) {
				return nil, errors.New("CSV array index is missing")
			}
			v = cur[index]
		default:
			return nil, errors.New("CSV pointer traverses a scalar")
		}
	}
	return v, nil
}

// Keep record separators CRLF without csv.Writer.UseCRLF rewriting CR/LF
// inside business string cells. Only the last byte of each record is held.
type csvTerminatorWriter struct {
	dest    io.Writer
	last    byte
	pending bool
}

func (w *csvTerminatorWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if w.pending {
		if n, e := w.dest.Write([]byte{w.last}); e != nil {
			return 0, e
		} else if n != 1 {
			return 0, io.ErrShortWrite
		}
	}
	n, e := w.dest.Write(p[:len(p)-1])
	if e != nil {
		return n, e
	}
	if n != len(p)-1 {
		return n, io.ErrShortWrite
	}
	w.last = p[len(p)-1]
	w.pending = true
	return len(p), nil
}
func (w *csvTerminatorWriter) finish() error {
	if !w.pending || w.last != '\n' {
		return errors.New("CSV record termination is invalid")
	}
	n, e := w.dest.Write([]byte("\r\n"))
	if e != nil {
		return e
	}
	if n != 2 {
		return io.ErrShortWrite
	}
	return nil
}
func invalidOutputIfError(e error) error {
	if e == nil {
		return nil
	}
	return invalidOutput(e)
}
