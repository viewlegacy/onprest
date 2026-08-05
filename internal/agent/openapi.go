package agent

func BuildOpenAPI(cf *CapabilityFile) map[string]any {
	paths := map[string]any{}
	for _, cap := range cf.CapabilityList() {
		if !exposeInOpenAPI(cap.Policy) {
			continue
		}
		props := map[string]any{}
		required := []string{}
		for name, p := range cap.Params {
			props[name] = schemaForParam(p)
			if p.Required && p.Default == nil {
				required = append(required, name)
			}
		}
		requestSchema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			requestSchema["required"] = required
		}
		paths["/api/v1/capabilities/"+cap.Name] = map[string]any{
			"post": map[string]any{
				"summary":              cap.Name,
				"description":          cap.Description,
				"x-onprest-capability": cap.Name,
				"requestBody":          map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": requestSchema}}},
				"responses":            map[string]any{"200": map[string]any{"description": "Capability result", "content": map[string]any{"application/json": map[string]any{"schema": responseSchema(cap)}}}},
			},
		}
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       cf.Service.Title,
			"version":     cf.Service.Version,
			"description": cf.Service.Description,
		},
		"paths": paths,
	}
}

func schemaForParam(p ParamDef) map[string]any {
	s := map[string]any{"type": p.Type}
	if p.Type == "integer" {
		s["format"] = "int64"
	}
	if p.Default != nil {
		s["default"] = p.Default
	}
	if len(p.Enum) > 0 {
		s["enum"] = p.Enum
	}
	if p.Minimum != nil {
		s["minimum"] = *p.Minimum
	}
	if p.Maximum != nil {
		s["maximum"] = *p.Maximum
	}
	if p.MinLength != nil {
		s["minLength"] = *p.MinLength
	}
	if p.MaxLength != nil {
		s["maxLength"] = *p.MaxLength
	}
	if p.Pattern != "" {
		s["pattern"] = p.Pattern
	}
	if p.Format != "" {
		s["format"] = p.Format
	}
	if p.Description != "" {
		s["description"] = p.Description
	}
	return s
}

func responseSchema(cap CapabilityDef) map[string]any {
	count := map[string]any{"type": "integer", "format": "int64", "minimum": int64(0)}
	if cap.Operation.mutation() {
		return map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"count": count},
			"required":             []string{"count"},
			"additionalProperties": false,
		}
	}
	item := map[string]any{"type": "object", "additionalProperties": false}
	if len(cap.Result) > 0 {
		props := map[string]any{}
		required := []string{}
		for name, col := range cap.Result {
			prop := map[string]any{"type": col.Type}
			if col.Description != "" {
				prop["description"] = col.Description
			}
			props[name] = prop
			required = append(required, name)
		}
		item = map[string]any{"type": "object", "properties": props, "additionalProperties": false, "required": required}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"rows":  map[string]any{"type": "array", "items": item},
			"count": count,
		},
		"required":             []string{"rows", "count"},
		"additionalProperties": false,
	}
}
