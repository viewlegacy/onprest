package contract

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

func relativePath(s string) error {
	if s == "" || strings.ContainsAny(s, "\x00\\") || strings.HasPrefix(s, "/") || regexp.MustCompile(`^[A-Za-z]:`).MatchString(s) {
		return errors.New("path must be a fixed relative directory")
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return errors.New("path must not traverse parent")
		}
	}
	return nil
}
func folderSource(sources Sources, name string, write bool) error {
	s, ok := sources.Folders[name]
	if !ok {
		return errors.New("unknown folder source")
	}
	if write && s["access"] != "read_write" {
		return errors.New("file write requires read_write source")
	}
	return nil
}
func compileSelector(m rawMap, input *Schema) (map[string]any, error) {
	if e := known(m, "path name name_regex recursive sort take scan_limit"); e != nil {
		return nil, e
	}
	if m.has("name") == m.has("name_regex") {
		return nil, errors.New("selector requires exactly one name or name_regex")
	}
	out := map[string]any{}
	p, e := stringDefault(m, "path", ".")
	if e != nil {
		return nil, e
	}
	if e := relativePath(p); e != nil {
		return nil, e
	}
	out["path"] = path.Clean(p)
	if m.has("name") {
		switch v := m.get("name").(type) {
		case string:
			if e := ValidateBasename(v); e != nil {
				return nil, e
			}
			out["name"] = v
		case rawMap:
			if e := known(v, "input"); e != nil {
				return nil, e
			}
			p, s, e := requirePointer(input, v.get("input"), "string", true, true)
			if e != nil {
				return nil, e
			}
			if len(s.Types) != 1 {
				return nil, errors.New("selector name must be string only")
			}
			out["name"] = map[string]any{"input": p}
		default:
			return nil, errors.New("invalid selector name")
		}
	} else {
		r, e := requiredString(m, "name_regex")
		if e != nil || len(r) > 4096 {
			return nil, errors.New("selector regex must be 1..4096 bytes")
		}
		if _, e := regexp.Compile(r); e != nil {
			return nil, errors.New("invalid selector Go regexp")
		}
		out["name_regex"] = r
	}
	b, e := boolDefault(m, "recursive", false)
	if e != nil {
		return nil, e
	}
	out["recursive"] = b
	sort, e := m.object("sort")
	if e != nil {
		return nil, e
	}
	if e := known(sort, "by order"); e != nil {
		return nil, e
	}
	by, e := stringDefault(sort, "by", "name")
	if e != nil || !contains([]string{"name", "modified_at", "size"}, by) {
		return nil, errors.New("invalid selector sort")
	}
	order, e := stringDefault(sort, "order", "asc")
	if e != nil || !contains([]string{"asc", "desc"}, order) {
		return nil, errors.New("invalid selector order")
	}
	out["sort"] = map[string]any{"by": by, "order": order}
	for k, d := range map[string]int64{"take": 1, "scan_limit": 10000} {
		n, e := intDefault(m, k, d, 1)
		if e != nil {
			return nil, e
		}
		out[k] = n
	}
	return out, nil
}
func compileRead(m rawMap, input *Schema, sources Sources, allowInput, allowWritten bool) (map[string]any, error) {
	keys := "source selector"
	if allowInput {
		keys += " input"
	}
	if allowWritten {
		keys += " written"
	}
	if e := known(m, keys); e != nil {
		return nil, e
	}
	if m.has("input") {
		if m.has("source") || m.has("selector") || m.has("written") {
			return nil, errors.New("input read and folder read are exclusive")
		}
		p, _, e := requirePointer(input, m.get("input"), "files", true, true)
		if e != nil {
			return nil, e
		}
		return map[string]any{"input": p}, nil
	}
	if m.has("written") {
		if len(m.keys) != 1 || m.get("written") != true {
			return nil, errors.New("read.written must be true and exclusive")
		}
		return map[string]any{"written": true}, nil
	}
	source, e := requiredString(m, "source")
	if e != nil {
		return nil, e
	}
	if e := folderSource(sources, source, false); e != nil {
		return nil, e
	}
	sel, e := object(m.get("selector"), "selector")
	if e != nil {
		return nil, e
	}
	selector, e := compileSelector(sel, input)
	if e != nil {
		return nil, e
	}
	return map[string]any{"source": source, "selector": selector}, nil
}
func compileCSVExecutor(m rawMap, input *Schema, sources Sources) (map[string]any, error) {
	if e := known(m, "type read csv where"); e != nil {
		return nil, e
	}
	r, e := object(m.get("read"), "read")
	if e != nil {
		return nil, e
	}
	read, e := compileRead(r, input, sources, true, false)
	if e != nil {
		return nil, e
	}
	csv, e := m.object("csv")
	if e != nil {
		return nil, e
	}
	settings, e := compileCSV(csv)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"type": "csv", "read": read, "csv": settings, "where": []any{}}
	if m.has("where") {
		conditions, e := list(m.get("where"), "where")
		if e != nil {
			return nil, e
		}
		w := []any{}
		for _, v := range conditions {
			r, e := object(v, "where condition")
			if e != nil {
				return nil, e
			}
			if e := known(r, "column op input"); e != nil {
				return nil, e
			}
			name, e := requiredString(r, "column")
			if e != nil {
				return nil, e
			}
			op, e := requiredString(r, "op")
			if e != nil || !contains([]string{"eq", "ne", "lt", "lte", "gt", "gte", "in"}, op) {
				return nil, errors.New("invalid CSV where operation")
			}
			colType := "string"
			cols := settings["columns"].(map[string]any)
			if col, ok := cols[name]; ok {
				colType = col.(map[string]any)["type"].(string)
			} else if settings["header"] == false {
				return nil, errors.New("where column is undefined")
			}
			p, s, e := requirePointer(input, r.get("input"), "", true, false)
			if e != nil {
				return nil, e
			}
			if op == "in" {
				if !contains(s.Types, "array") || s.Items == nil {
					return nil, errors.New("in binding must reference typed array")
				}
				s = s.Items
			}
			ts := without(s.Types, "null")
			if len(ts) != 1 || ts[0] != colType {
				return nil, errors.New("CSV where binding must match parsed column type")
			}
			w = append(w, map[string]any{"column": name, "op": op, "input": p})
		}
		out["where"] = w
	}
	return out, nil
}
func compileCSV(m rawMap) (map[string]any, error) {
	if e := known(m, "header encoding delimiter columns"); e != nil {
		return nil, e
	}
	header, e := boolDefault(m, "header", true)
	if e != nil {
		return nil, e
	}
	encoding, e := stringDefault(m, "encoding", "utf-8")
	if e != nil || !contains([]string{"utf-8", "shift_jis", "cp932"}, encoding) {
		return nil, errors.New("unsupported CSV encoding")
	}
	delim, e := stringDefault(m, "delimiter", ",")
	if delim == "comma" {
		delim = ","
	}
	if e != nil || utf8.RuneCountInString(delim) != 1 || strings.ContainsAny(delim, "\x00\r\n\"") || delim == string(utf8.RuneError) {
		return nil, errors.New("delimiter must be one valid CSV character")
	}
	cols, e := m.object("columns")
	if e != nil {
		return nil, e
	}
	outCols := map[string]any{}
	positions := map[int64]bool{}
	for _, name := range cols.keys {
		if name == "" {
			return nil, errors.New("CSV column name must be nonempty")
		}
		c, e := object(cols.get(name), "CSV column")
		if e != nil {
			return nil, e
		}
		if e := known(c, "type nullable null_values position"); e != nil {
			return nil, e
		}
		typ, e := stringDefault(c, "type", "string")
		if e != nil || !contains([]string{"string", "integer", "number", "boolean"}, typ) {
			return nil, errors.New("unsupported CSV column type")
		}
		nullable, e := boolDefault(c, "nullable", false)
		if e != nil {
			return nil, e
		}
		nulls := []string{}
		if c.has("null_values") {
			nulls, e = stringsList(c.get("null_values"), "null_values", false)
			if e != nil {
				return nil, e
			}
			nulls = sortedUnique(nulls)
		}
		entry := map[string]any{"type": typ, "nullable": nullable, "null_values": nulls}
		if !header {
			if !c.has("position") {
				return nil, errors.New("headerless CSV requires every position")
			}
			p, e := intDefault(c, "position", 0, 0)
			if e != nil || positions[p] {
				return nil, errors.New("invalid or duplicate CSV position")
			}
			positions[p] = true
			entry["position"] = p
		} else if c.has("position") {
			return nil, errors.New("position requires header:false")
		}
		outCols[name] = entry
	}
	if !header {
		if len(positions) == 0 {
			return nil, errors.New("headerless CSV requires columns")
		}
		for i := 0; i < len(positions); i++ {
			if !positions[int64(i)] {
				return nil, errors.New("CSV positions must be contiguous from zero")
			}
		}
	}
	return map[string]any{"header": header, "encoding": encoding, "delimiter": delim, "columns": outCols}, nil
}
func compileFileExecutor(m rawMap, input *Schema, sources Sources) (map[string]any, error) {
	if e := known(m, "type write read effects"); e != nil {
		return nil, e
	}
	if !m.has("read") && !m.has("write") {
		return nil, errors.New("file executor requires read or write")
	}
	out := map[string]any{"type": "file"}
	if m.has("write") {
		a, e := list(m.get("write"), "write")
		if e != nil || len(a) == 0 {
			return nil, errors.New("write must be nonempty list")
		}
		writes := []any{}
		for _, v := range a {
			r, e := object(v, "write")
			if e != nil {
				return nil, e
			}
			if e := known(r, "input source path name name_from overwrite"); e != nil {
				return nil, e
			}
			p, s, e := requirePointer(input, r.get("input"), "files", true, true)
			if e != nil {
				return nil, e
			}
			source, e := requiredString(r, "source")
			if e != nil {
				return nil, e
			}
			if e := folderSource(sources, source, true); e != nil {
				return nil, e
			}
			directory, e := stringDefault(r, "path", ".")
			if e != nil {
				return nil, e
			}
			if e := relativePath(directory); e != nil {
				return nil, e
			}
			if r.has("name") == r.has("name_from") {
				return nil, errors.New("write requires exactly one name or name_from")
			}
			w := map[string]any{"input": p, "source": source, "path": path.Clean(directory)}
			if r.has("name") {
				if s.File == nil {
					return nil, errors.New("fixed output name requires one file")
				}
				name, e := requiredString(r, "name")
				if e != nil {
					return nil, e
				}
				if e := validateSaveName(name); e != nil {
					return nil, e
				}
				w["name"] = name
			} else {
				n, e := requiredString(r, "name_from")
				if e != nil || n != "input" {
					return nil, errors.New("name_from must be input")
				}
				w["name_from"] = "input"
			}
			b, e := boolDefault(r, "overwrite", false)
			if e != nil {
				return nil, e
			}
			w["overwrite"] = b
			writes = append(writes, w)
		}
		out["write"] = writes
		out["effects"] = "possible"
	} else {
		out["effects"] = "none"
	}
	if m.has("effects") {
		v, e := str(m.get("effects"), "effects")
		if e != nil || v != out["effects"] {
			return nil, errors.New("file effects must match write presence")
		}
	}
	if m.has("read") {
		r, e := object(m.get("read"), "read")
		if e != nil {
			return nil, e
		}
		read, e := compileRead(r, input, sources, false, true)
		if e != nil {
			return nil, e
		}
		if read["written"] == true && !m.has("write") {
			return nil, errors.New("read.written requires write")
		}
		out["read"] = read
	}
	return out, nil
}
func validateSaveName(s string) error {
	if e := ValidateBasename(s); e != nil {
		return e
	}
	if strings.HasPrefix(strings.ToLower(s), ".onprest-") || strings.ContainsAny(s, ":<>\"|?*") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
		return errors.New("invalid or reserved saved filename")
	}
	base := strings.ToUpper(strings.SplitN(s, ".", 2)[0])
	if regexp.MustCompile(`^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])$`).MatchString(base) {
		return errors.New("reserved saved filename")
	}
	return nil
}
