package contract

import (
	"errors"
	"math"
	"sort"
	"strings"
)

// JSONSchema returns the standard 2020-12 schema for the public value. File
// leaves become inline wire objects; Onprest-only validation metadata uses x-*
// annotations. Formats are annotations under the standard default vocabulary;
// the server retains its existing format validators.
func (s *Schema) JSONSchema() map[string]any { return publicSchema(s, !s.output, false) }
func publicSchema(s *Schema, input, nonNull bool) map[string]any {
	if s == nil {
		return map[string]any{}
	}
	if s.File != nil {
		return inlineFileSchema(s)
	}
	out := map[string]any{}
	if d, ok := s.normalized["description"].(string); ok {
		out["description"] = d
	}
	if len(s.Types) == 1 {
		out["type"] = s.Types[0]
	} else if len(s.Types) > 1 {
		out["type"] = append([]string{}, s.Types...)
	}
	if s.PropertiesSet {
		props := map[string]any{}
		for _, k := range s.PropertyOrder {
			props[k] = publicSchema(s.Properties[k], input, false)
		}
		out["properties"] = props
	}
	if s.Items != nil {
		out["items"] = publicSchema(s.Items, input, false)
	}
	if contains(s.Types, "object") {
		required := []string{}
		for _, k := range s.Required {
			if !input || !s.Properties[k].DefaultSet {
				required = append(required, k)
			}
		}
		out["required"] = required
		if s.Additional != nil {
			out["additionalProperties"] = publicSchema(s.Additional, input, false)
		} else {
			out["additionalProperties"] = s.AdditionalAllowed
		}
	}
	for k, v := range s.Constraints {
		if k != "format" || v != "" {
			out[k] = cloneValue(v)
		}
	}
	if contains(s.Types, "integer") && !contains(s.Types, "number") {
		if n, ok := out["minimum"]; !ok || compareNumbers(n, int64(math.MinInt64)) < 0 {
			out["minimum"] = int64(math.MinInt64)
		}
		if n, ok := out["maximum"]; !ok || compareNumbers(n, int64(math.MaxInt64)) > 0 {
			out["maximum"] = int64(math.MaxInt64)
		}
	}
	if s.Pattern != nil {
		out["pattern"] = s.Pattern.ECMAExpression()
	}
	if input && s.DefaultSet {
		out["default"] = cloneValue(s.Default)
	}
	if !input && !nonNull {
		if s.Nullable {
			if len(out) == 0 {
				return out
			}
			// Value constraints must not apply to null, even enum/const/format.
			return map[string]any{"anyOf": []any{map[string]any{"type": "null"}, out}}
		}
		if len(s.Types) == 0 {
			out["not"] = map[string]any{"type": "null"}
		}
	}
	return out
}
func fileAnnotations(s *Schema) map[string]any {
	return map[string]any{"extensions": append([]string{}, s.File.Extensions...), "max_bytes": s.File.MaxBytes, "mime_types": append([]string{}, s.File.MIMETypes...), "allow_empty": s.File.AllowEmpty}
}
func inlineFileSchema(s *Schema) map[string]any {
	out := map[string]any{
		"type": "object", "properties": map[string]any{
			"name":      map[string]any{"type": "string", "minLength": int64(1), "maxLength": int64(255), "description": "UTF-8 display basename, 1–255 bytes; no path separators or control characters."},
			"mime_type": map[string]any{"type": "string", "minLength": int64(1), "description": "Lowercase MIME type without parameters."},
			"data":      map[string]any{"type": "string", "contentEncoding": "base64", "description": "Canonical RFC 4648 padded Base64. Decoded size and file constraints are checked by the server."},
		}, "required": []string{"name", "mime_type", "data"}, "additionalProperties": false, "x-onprest-file": fileAnnotations(s),
		"description": "Inline file; extensions and MIME restrictions both apply. max_bytes limits decoded bytes, not Base64 string length.",
	}
	if d, ok := s.normalized["description"].(string); ok && d != "" {
		out["description"] = d + " " + out["description"].(string)
	}
	return out
}

// InputMediaSchemas produces JSON and (when file input exists) multipart
// definitions. A nil Multipart means the media type cannot represent the
// configured required file array with maxItems:0. Encoding defines raw binary
// parts; format:binary alone is only a client annotation.
type InputMediaSchemas struct{ JSON, Multipart, Encoding map[string]any }

func (c *Capability) InputMediaSchemas() InputMediaSchemas {
	result := InputMediaSchemas{JSON: publicSchema(c.Input, true, false)}
	if !c.Input.HasFiles() {
		return result
	}
	multipart := publicSchema(c.Input, true, false)
	props, ok := multipart["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		multipart["properties"] = props
	}
	encoding := map[string]any{}
	for _, name := range c.Input.PropertyOrder {
		s := c.Input.Properties[name]
		file := s
		array := s.File == nil && s.Items != nil && s.Items.File != nil
		if array {
			file = s.Items
		}
		if file.File == nil {
			encoding[name] = map[string]any{"contentType": "application/json"}
			continue
		}
		if array {
			if max, ok := s.Constraints["maxItems"].(int64); ok && max == 0 {
				if contains(c.Input.Required, name) {
					return result
				}
				delete(props, name)
				continue
			}
		}
		binary := map[string]any{"type": "string", "format": "binary", "x-onprest-file": fileAnnotations(file)}
		if array {
			p := map[string]any{"type": "array", "items": binary, "minItems": max(int64(1), constraintInt(s, "minItems"))}
			if upper, ok := s.Constraints["maxItems"]; ok {
				p["maxItems"] = upper
			}
			if d, ok := s.normalized["description"]; ok {
				p["description"] = d
			}
			props[name] = p
		} else {
			props[name] = binary
		}
		mime := "*/*"
		if len(file.File.MIMETypes) > 0 {
			mime = strings.Join(file.File.MIMETypes, ",")
		}
		encoding[name] = map[string]any{"contentType": mime, "style": "form", "explode": true}
	}
	result.Multipart = multipart
	result.Encoding = encoding
	return result
}
func constraintInt(s *Schema, k string) int64 { n, _ := s.Constraints[k].(int64); return n }
func objectSchema(props map[string]any, required []string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}
func countSchema() map[string]any {
	return map[string]any{"type": "integer", "format": "int64", "minimum": int64(0), "maximum": int64(math.MaxInt64)}
}
func (c *Capability) dataSchema() map[string]any {
	if !c.Output.Data {
		return map[string]any{"type": "null"}
	}
	s := c.Output.Schema
	if rowProvider(c) {
		rows := publicSchema(s, false, true)
		rows["type"] = "array"
		item := publicSchema(s.Items, false, true)
		item["type"] = "object"
		if s.Items == nil {
			item["additionalProperties"] = true
		}
		if c.Output.Mode != "passthrough" && s.Items != nil && s.Items.PropertiesSet {
			item["required"] = append([]string{}, s.Items.PropertyOrder...)
		}
		rows["items"] = item
		return objectSchema(map[string]any{"rows": rows, "count": countSchema()}, []string{"rows", "count"})
	}
	if dmlProvider(c) {
		out := publicSchema(s, false, true)
		out["type"] = "object"
		out["additionalProperties"] = false
		out["required"] = []string{"count"}
		countDefinition := s.Properties["count"]
		if countDefinition == nil {
			countDefinition = s.Additional
		}
		count := publicSchema(countDefinition, false, true)
		count["type"] = "integer"
		if n, ok := count["minimum"]; !ok || compareNumbers(n, int64(0)) < 0 {
			count["minimum"] = int64(0)
		}
		if n, ok := count["maximum"]; !ok || compareNumbers(n, int64(math.MaxInt64)) > 0 {
			count["maximum"] = int64(math.MaxInt64)
		}
		count["format"] = "int64"
		out["properties"] = map[string]any{"count": count}
		return out
	}
	return publicSchema(s, false, false)
}
func artifactDescriptorSchema(reference bool) map[string]any {
	props := map[string]any{
		"id":         map[string]any{"type": "string", "minLength": int64(1)},
		"name":       map[string]any{"type": "string", "minLength": int64(1), "maxLength": int64(255)},
		"mime_type":  map[string]any{"type": "string", "minLength": int64(1)},
		"size_bytes": countSchema(),
		"sha256":     map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}(?![\\s\\S])"},
	}
	required := []string{"id", "name", "mime_type", "size_bytes", "sha256"}
	if reference {
		props["uri"] = map[string]any{"type": "string", "format": "uri"}
		props["expires_at"] = map[string]any{"type": "string", "format": "date-time"}
		required = append(required, "uri", "expires_at")
	}
	return objectSchema(props, required)
}

// SuccessSchema describes the common object envelope. delivery is rest or mcp;
// file URI construction and content rendering belong to the later transport.
func (c *Capability) SuccessSchema(delivery string) map[string]any {
	files := map[string]any{"type": "array", "maxItems": int64(0)}
	if c.Output.Files != nil {
		reference := true
		if delivery == "mcp" && c.Output.Files["delivery"].(map[string]any)["mcp"] == "inline" {
			reference = false
		}
		descriptor := artifactDescriptorSchema(reference)
		descriptor["properties"].(map[string]any)["size_bytes"].(map[string]any)["maximum"] = policyInt(c, "max_output_file_bytes")
		files["items"] = descriptor
		files["maxItems"] = policyInt(c, "max_output_files")
		if c.Output.Files["format"] == "csv" || c.Output.Files["delivery"].(map[string]any)["rest"] == "direct" {
			files["minItems"] = int64(1)
			files["maxItems"] = int64(1)
		}
	}
	return objectSchema(map[string]any{"request_id": map[string]any{"type": "string", "minLength": int64(1)}, "result": objectSchema(map[string]any{"data": c.dataSchema(), "files": files}, []string{"data", "files"})}, []string{"request_id", "result"})
}

var systemErrorCodes = []string{"INVALID_REQUEST", "VALIDATION_FAILED", "AUTH_FAILED", "CAPABILITY_DENIED", "PERMISSION_DENIED", "NOT_FOUND", "SOURCE_CHANGED", "CONFLICT", "LIMIT_EXCEEDED", "RATE_LIMITED", "BUSY", "AGENT_OFFLINE", "DEPENDENCY_UNAVAILABLE", "EXECUTION_TIMEOUT", "EXECUTION_FAILED", "OUTPUT_INVALID", "OUTCOME_UNKNOWN", "INTERNAL_ERROR"}
var outcomes = []string{"not_started", "no_change", "rolled_back", "committed", "unknown"}

const publicErrorPattern = `^APP_[A-Z][A-Z0-9_]{0,59}(?![\s\S])`

func ErrorSchema() map[string]any {
	code := map[string]any{"anyOf": []any{map[string]any{"type": "string", "enum": append([]string{}, systemErrorCodes...)}, map[string]any{"type": "string", "pattern": publicErrorPattern}}}
	safeString := map[string]any{"type": "string", "minLength": int64(1), "maxLength": int64(1024), "pattern": `^[^\u{0}-\u{1f}\u{7f}-\u{9f}]*(?![\s\S])`, "x-onprest-max-bytes": int64(1024), "description": "Public safe message; at most 1024 UTF-8 bytes, no control characters."}
	err := objectSchema(map[string]any{"code": code, "message": safeString, "reason": map[string]any{"type": "string", "pattern": `^[A-Z][A-Z0-9_]*(?![\s\S])`, "description": "Optional fixed safe reason code, never raw upstream detail."}, "outcome": map[string]any{"type": "string", "enum": append([]string{}, outcomes...)}}, []string{"code", "message", "outcome"})
	err["allOf"] = []any{map[string]any{"if": map[string]any{"properties": map[string]any{"code": map[string]any{"pattern": publicErrorPattern}}, "required": []string{"code"}}, "then": map[string]any{"not": map[string]any{"required": []string{"reason"}}}}}
	return objectSchema(map[string]any{"request_id": map[string]any{"type": "string", "minLength": int64(1)}, "error": err}, []string{"request_id", "error"})
}

// BuildOpenAPI produces metadata from compiled definitions only. It does not
// advertise disabled capabilities or disclose executor/source/secret settings.
// Runtime serving and Gateway-owned security/server metadata are connected later.
func BuildOpenAPI(config *Config) map[string]any {
	paths := map[string]any{}
	for _, name := range sortedCapabilityNames(config) {
		c := config.Capabilities[name]
		if c.Policy["expose_in_openapi"] != true {
			continue
		}
		input := c.InputMediaSchemas()
		content := map[string]any{"application/json": map[string]any{"schema": input.JSON}}
		if input.Multipart != nil {
			content["multipart/form-data"] = map[string]any{"schema": input.Multipart, "encoding": input.Encoding}
		}
		responseContent := map[string]any{"application/json": map[string]any{"schema": c.SuccessSchema("rest")}}
		if c.Output.Files != nil && c.Output.Files["delivery"].(map[string]any)["rest"] == "direct" {
			mime, _ := c.Output.Files["mime_type"].(string)
			if mime == "" {
				mime = "application/octet-stream"
			}
			responseContent = map[string]any{mime: map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}
		}
		responses := map[string]any{"200": map[string]any{"description": "Capability result", "content": responseContent}}
		for _, status := range []string{"400", "401", "403", "404", "409", "413", "415", "422", "429", "500", "502", "503", "504"} {
			responses[status] = map[string]any{"description": "Safe capability error", "content": map[string]any{"application/json": map[string]any{"schema": ErrorSchema()}}}
		}
		op := map[string]any{"operationId": name, "summary": name, "description": c.Description, "x-onprest-capability": name, "requestBody": map[string]any{"required": true, "content": content}, "responses": responses}
		if c.Annotations != nil {
			op["x-onprest-annotations"] = cloneMap(c.Annotations)
		}
		if len(c.Examples) > 0 {
			a := []any{}
			for _, e := range c.Examples {
				a = append(a, cloneMap(e))
			}
			op["x-onprest-examples"] = a
		}
		paths["/api/v1/capabilities/"+name] = map[string]any{"post": op}
	}
	info := map[string]any{"title": config.Service["title"], "version": config.Service["version"]}
	if d, ok := config.Service["description"]; ok {
		info["description"] = d
	}
	return map[string]any{"openapi": "3.1.0", "info": info, "paths": paths}
}
func sortedCapabilityNames(c *Config) []string {
	names := []string{}
	for name := range c.Capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func BuildMCPTools(config *Config, version string) ([]map[string]any, error) {
	if !contains([]string{"", "2025-03-26", "2025-06-18", "2025-11-25"}, version) {
		return nil, errors.New("unsupported MCP protocol version")
	}
	tools := []map[string]any{}
	for _, name := range sortedCapabilityNames(config) {
		c := config.Capabilities[name]
		if c.Policy["expose_in_openapi"] != true {
			continue
		}
		tool := map[string]any{"name": name, "description": c.Description, "inputSchema": publicSchema(c.Input, true, false)}
		if version == "2025-06-18" || version == "2025-11-25" {
			tool["outputSchema"] = c.SuccessSchema("mcp")
		}
		annotations := map[string]any{}
		for from, to := range map[string]string{"read_only": "readOnlyHint", "destructive": "destructiveHint", "idempotent": "idempotentHint", "open_world": "openWorldHint"} {
			if value, ok := c.Annotations[from]; ok {
				annotations[to] = value
			}
		}
		if len(annotations) > 0 {
			tool["annotations"] = annotations
		}
		tools = append(tools, tool)
	}
	return tools, nil
}
