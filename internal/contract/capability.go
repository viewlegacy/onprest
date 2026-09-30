package contract

import (
	"errors"
	"fmt"
	"github.com/viewlegacy/onprest/internal/sqlsyntax"
	"strings"
)

func compileCapability(name string, m rawMap, defaults map[string]any, sources Sources) (*Capability, error) {
	if m.has("sql") || m.has("params") || m.has("result") {
		return nil, errors.New("legacy sql/params/result is unsupported; use executor/input/output")
	}
	if e := known(m, "description executor input output policy annotations examples"); e != nil {
		return nil, e
	}
	c := &Capability{Name: name}
	var e error
	c.Description, e = stringDefault(m, "description", "")
	if e != nil {
		return nil, e
	}
	input, e := m.object("input")
	if e != nil {
		return nil, e
	}
	c.Input, e = compileSchema(input, true, true, false, 0)
	if e != nil {
		return nil, e
	}
	out, e := m.object("output")
	if e != nil {
		return nil, e
	}
	c.Output, e = compileOutput(out)
	if e != nil {
		return nil, e
	}
	ex, e := m.object("executor")
	if e != nil {
		return nil, e
	}
	c.Executor, e = compileExecutor(ex, c.Input, sources)
	if e != nil {
		return nil, e
	}
	policy, e := m.object("policy")
	if e != nil {
		return nil, e
	}
	pv, e := compilePolicyValues(policy)
	if e != nil {
		return nil, e
	}
	c.Policy, e = resolvePolicy(c, pv, defaults)
	if e != nil {
		return nil, e
	}
	if c.Executor.Type == "http" {
		fixed := []any{}
		collectFixed(c.Executor.Settings, &fixed)
		limits := c.Limits()
		requestLimit := c.Executor.Settings["limits"].(map[string]any)["max_http_request_bytes"].(int64)
		limits.MaxBytes = min(limits.MaxBytes, requestLimit)
		for _, value := range fixed {
			if e := checkJSONLimits(value, limits); e != nil {
				return nil, e
			}
		}
	}
	if c.Input.HasFiles() {
		if e := c.Input.resolveFileLimits(c.Policy["max_input_file_bytes"].(int64)); e != nil {
			return nil, e
		}
	}
	if e := lintOutput(c); e != nil {
		return nil, e
	}
	if m.has("annotations") {
		a, e := object(m.get("annotations"), "annotations")
		if e != nil {
			return nil, e
		}
		if e := known(a, "read_only destructive idempotent open_world"); e != nil {
			return nil, e
		}
		c.Annotations = map[string]any{}
		for _, k := range a.keys {
			b, ok := a.get(k).(bool)
			if !ok {
				return nil, errors.New("annotation must be boolean")
			}
			c.Annotations[k] = b
		}
	}
	if m.has("examples") {
		a, e := list(m.get("examples"), "examples")
		if e != nil {
			return nil, e
		}
		for _, v := range a {
			r, e := object(v, "example")
			if e != nil {
				return nil, e
			}
			if e := known(r, "params"); e != nil {
				return nil, e
			}
			p, e := object(r.get("params"), "example.params")
			if e != nil {
				return nil, e
			}
			rawParams := plain(p).(map[string]any)
			if e := checkJSONLimits(rawParams, exampleWireLimits(c.Limits())); e != nil {
				return nil, e
			}
			remaining := c.Limits().MaxTotalFileBytes
			prepared, err := normalizeExampleFiles(c.Input, rawParams, "", &remaining)
			if err != nil {
				return nil, err
			}

			params, _, e := c.ValidateInput(prepared.(map[string]any))
			if e != nil {
				return nil, fmt.Errorf("example: %w", e)
			}
			c.Examples = append(c.Examples, map[string]any{"params": publicExample(params, rawParams)})
		}
	}
	return c, nil
}
func compileOutput(m rawMap) (Output, error) {
	o := Output{}
	mode, e := stringDefault(m, "mode", "")
	if e != nil || mode != "" && mode != "passthrough" {
		return o, errors.New("output mode must be passthrough or omitted")
	}
	o.Mode = mode
	schema := emptyMap()
	for _, k := range m.keys {
		if k != "data" && k != "files" && k != "mode" {
			schema.keys = append(schema.keys, k)
			schema.values[k] = m.get(k)
		}
	}
	o.Schema, e = compileSchema(schema, false, false, mode == "passthrough", 0)
	if e != nil {
		return o, e
	}
	if m.has("data") {
		data, e := object(m.get("data"), "output.data")
		if e != nil || len(data.keys) != 0 {
			return o, errors.New("output.data must be an empty map")
		}
		o.Data = true
	}
	if m.has("files") {
		f, e := object(m.get("files"), "output.files")
		if e != nil {
			return o, e
		}
		o.Files, e = compileOutputFiles(f)
		if e != nil {
			return o, e
		}
	}
	if len(schema.keys) > 0 || mode != "" {
		if !o.Data && (o.Files == nil || o.Files["format"] != "csv") {
			return o, errors.New("output schema/mode requires data or generated CSV")
		}
	}
	return o, nil
}
func compileOutputFiles(m rawMap) (map[string]any, error) {
	if e := known(m, "format from columns filename formula_escape mime_type delivery"); e != nil {
		return nil, e
	}
	out := map[string]any{}
	format, e := stringDefault(m, "format", "")
	if e != nil || format != "" && format != "csv" {
		return nil, errors.New("output file format must be csv or omitted")
	}
	if m.has("format") && format == "" {
		return nil, errors.New("empty format is unsupported")
	}
	if format == "csv" {
		out["format"] = format
		name, e := requiredString(m, "filename")
		if e != nil {
			return nil, e
		}
		if e := ValidateBasename(name); e != nil {
			return nil, e
		}
		out["filename"] = name
		from, e := stringDefault(m, "from", "")
		if e != nil {
			return nil, e
		}
		if from != "" {
			if !strings.HasPrefix(from, "/") {
				return nil, errors.New("CSV from must be JSON Pointer")
			}
			for _, t := range strings.Split(from[1:], "/") {
				if _, e := unescapePointer(t); e != nil {
					return nil, e
				}
			}
		}
		out["from"] = from
		b, e := boolDefault(m, "formula_escape", true)
		if e != nil {
			return nil, e
		}
		out["formula_escape"] = b
		if m.has("columns") {
			cols, e := stringsList(m.get("columns"), "columns", true)
			if e != nil || len(cols) == 0 {
				return nil, errors.New("columns must be nonempty and unique")
			}
			out["columns"] = cols
		}
		mt, e := stringDefault(m, "mime_type", "text/csv")
		if e != nil || !validMIME(mt) {
			return nil, errors.New("invalid output MIME")
		}
		out["mime_type"] = mt
	} else {
		for _, k := range []string{"from", "columns", "filename", "formula_escape"} {
			if m.has(k) {
				return nil, errors.New("CSV-only setting on original file output")
			}
		}
		if m.has("mime_type") {
			mt, e := requiredString(m, "mime_type")
			if e != nil || !validMIME(mt) {
				return nil, errors.New("invalid output MIME")
			}
			out["mime_type"] = mt
		}
	}
	d, e := m.object("delivery")
	if e != nil {
		return nil, e
	}
	if e := known(d, "rest mcp"); e != nil {
		return nil, e
	}
	rest, e := stringDefault(d, "rest", "reference")
	if e != nil || !contains([]string{"reference", "direct"}, rest) {
		return nil, errors.New("invalid REST file delivery")
	}
	mcp, e := stringDefault(d, "mcp", "reference")
	if e != nil || !contains([]string{"reference", "inline"}, mcp) {
		return nil, errors.New("invalid MCP file delivery")
	}
	out["delivery"] = map[string]any{"rest": rest, "mcp": mcp}
	return out, nil
}
func compileExecutor(m rawMap, input *Schema, sources Sources) (Executor, error) {
	x := Executor{}
	typ, e := requiredString(m, "type")
	if e != nil {
		return x, e
	}
	x.Type = typ
	var out map[string]any
	switch typ {
	case "db":
		if e := known(m, "type source sql"); e != nil {
			return x, e
		}
		source, e := requiredString(m, "source")
		if e != nil {
			return x, e
		}
		db, ok := sources.Databases[source]
		if !ok {
			return x, errors.New("unknown database source")
		}
		if input.HasFiles() {
			return x, errors.New("DB file input is unsupported")
		}
		sql, e := requiredString(m, "sql")
		if e != nil {
			return x, e
		}
		cl := sqlsyntax.Classify(db["driver"].(string), sql)
		if !cl.Single || cl.Operation == "" || cl.ReturnsRows {
			return x, errors.New("DB executor requires supported single SELECT/INSERT/UPDATE/DELETE without row-returning DML")
		}
		for _, name := range sqlsyntax.Parameters(db["driver"].(string), sql) {
			s, ok := input.Properties[name]
			if !ok {
				return x, errors.New("SQL parameter must name a declared input property")
			}
			if e := bindingType(s, "scalar"); e != nil {
				return x, e
			}
		}
		x.Source = source
		x.Operation = string(cl.Operation)
		out = map[string]any{"type": typ, "source": source, "sql": sql}
	case "csv":
		out, e = compileCSVExecutor(m, input, sources)
	case "file":
		out, e = compileFileExecutor(m, input, sources)
	case "http":
		out, e = compileHTTPExecutor(m, input, sources)
	case "wasm":
		out, e = compileWasmExecutor(m)
	default:
		return x, errors.New("unsupported executor type")
	}
	if e != nil {
		return x, e
	}
	x.Settings = out
	if source, ok := out["source"].(string); ok {
		x.Source = source
	}
	return x, nil
}
func lintOutput(c *Capability) error {
	o := &c.Output
	s := o.Schema
	kind := c.Executor.Type
	dataProvider := !(kind == "file" || kind == "http" && c.Executor.Settings["response"].(map[string]any)["kind"] == "file")
	if !dataProvider && (o.Data || len(s.Types) > 0 || s.PropertiesSet || s.Items != nil || len(s.Constraints) > 0 || o.Mode != "") {
		return errors.New("file-only executor cannot declare data schema")
	}
	if !dataProvider && o.Files != nil && o.Files["format"] == "csv" {
		return errors.New("file-only executor cannot generate CSV")
	}
	if kind == "file" {
		read := c.Executor.Settings["read"] != nil
		if read != (o.Files != nil) {
			return errors.New("file read and output.files must both be defined or both omitted")
		}
		if o.Files != nil && o.Files["format"] == "csv" {
			return errors.New("file executor cannot generate CSV")
		}
	}
	if o.Files != nil && o.Files["format"] != "csv" && dataProvider && kind != "wasm" {
		return errors.New("data executor requires format:csv for file output")
	}
	if kind == "db" || kind == "csv" {
		if kind == "csv" || c.Executor.Operation == "select" {
			if len(s.Types) > 0 && !contains(s.Types, "array") {
				return errors.New("DB SELECT/CSV output schema must describe row array")
			}
			if s.Items != nil && len(s.Items.Types) > 0 && !contains(s.Items.Types, "object") {
				return errors.New("row output schema must describe objects")
			}
		} else {
			if len(s.Types) > 0 && !contains(s.Types, "object") {
				return errors.New("DML output schema must describe count object")
			}
			if s.PropertiesSet {
				count, ok := s.Properties["count"]
				if !ok {
					return errors.New("DML output cannot exclude count")
				}
				if len(count.Types) > 0 && !contains(count.Types, "integer") && !contains(count.Types, "number") {
					return errors.New("DML count must be integer")
				}
				for name := range s.Properties {
					if name != "count" {
						return errors.New("DML output only provides count")
					}
				}
			}
		}
	}
	if o.Files != nil && o.Files["format"] == "csv" {
		from := o.Files["from"].(string)
		rows := s
		if from != "" {
			var e error
			rows, e = s.resolveOutputPointer(from)
			if e != nil {
				return e
			}
		}
		if len(rows.Types) > 0 && !contains(rows.Types, "array") {
			return errors.New("CSV from must target object array")
		}
		if rows.Items != nil && len(rows.Items.Types) > 0 && !contains(rows.Items.Types, "object") {
			return errors.New("CSV elements must be objects")
		}
		cols, explicit := o.Files["columns"].([]string)
		closed := rows.Items != nil && rows.Items.PropertiesSet && !rows.Items.AdditionalAllowed && o.Mode != "passthrough"
		if explicit && closed {
			for _, name := range cols {
				if _, ok := rows.Items.Properties[name]; !ok {
					return errors.New("CSV column is excluded by output schema")
				}
			}
		}
		if !explicit {
			if closed && len(rows.Items.PropertyOrder) > 0 {
				o.Files["columns"] = append([]string{}, rows.Items.PropertyOrder...)
			} else if kind == "db" && c.Executor.Operation == "select" || kind == "csv" {
				o.Files["column_order"] = "provider_metadata"
			} else {
				return errors.New("CSV columns are required when fixed columns cannot be determined")
			}
		}
	}
	if o.Files != nil && o.Files["delivery"].(map[string]any)["rest"] == "direct" {
		if o.Data {
			return errors.New("REST direct requires omitted output.data")
		}
		if o.Files["format"] == "csv" || kind == "http" && c.Executor.Settings["response"].(map[string]any)["kind"] == "file" {
			return nil
		}
		if kind == "file" {
			r := c.Executor.Settings["read"].(map[string]any)
			if r["written"] == true {
				w := c.Executor.Settings["write"].([]any)
				if len(w) != 1 {
					return errors.New("REST direct requires exactly one write")
				}
				p := w[0].(map[string]any)["input"].(string)
				leaf, _, _ := c.Input.ResolvePointer(p)
				if leaf.File == nil {
					return errors.New("REST direct requires one file input")
				}
			} else if r["selector"].(map[string]any)["take"] != int64(1) {
				return errors.New("REST direct selector requires take:1")
			}
		} else if kind != "wasm" {
			return errors.New("REST direct requires a single-file contract")
		}
	}
	return nil
}
