// Package contract compiles the versioned capability definition independently of
// runtime preparation, source connections, transports, and public rendering.
package contract

import "regexp"

type Config struct {
	SchemaVersion                      int64
	Service, Gateway, Runtime, Logging map[string]any
	Sources                            Sources
	Capabilities                       map[string]*Capability
}
type Sources struct{ Databases, Folders, HTTP map[string]map[string]any }
type Capability struct {
	Name, Description string
	Input             *Schema
	Output            Output
	Executor          Executor
	Policy            map[string]any
	Annotations       map[string]any
	Examples          []map[string]any
}
type Executor struct {
	Type, Source, Operation string
	Settings                map[string]any
}
type Output struct {
	Schema *Schema
	Data   bool
	Files  map[string]any
	Mode   string
}

// Schema keeps property declaration order separately from its canonical map.
// Presence matters: properties:{} selects an empty object, unlike no properties.
type Schema struct {
	Types             []string
	Properties        map[string]*Schema
	PropertyOrder     []string
	PropertiesSet     bool
	Items             *Schema
	AdditionalAllowed bool
	Additional        *Schema
	Required          []string
	Default           any
	DefaultSet        bool
	Nullable          bool
	Constraints       map[string]any
	File              *FileConstraints
	Pattern           *Pattern
	normalized        map[string]any
}
type FileConstraints struct {
	Extensions, MIMETypes []string
	MaxBytes              int64
	AllowEmpty            bool
}

// FileValue represents validated request-owned metadata. It contains neither
// payload bytes nor a host path. Receiving code must compute size and digest.
type FileValue struct {
	Pointer, ID, Name, MIMEType, SHA256, RequestID string
	SizeBytes                                      int64
}
type FileLeaf struct {
	Pointer string
	Value   FileValue
	Schema  *Schema
}
type Limits struct{ MaxBytes, MaxDepth, MaxItems, MaxFileBytes, MaxTotalFileBytes, MaxFiles int64 }

// Pattern is the shared syntax tree; its compiled Go expression is never used
// as the source for another engine's expression.
type Pattern struct {
	AST          *PatternNode
	GoExpression string
	re           *regexp.Regexp
}
type PatternNode struct {
	Kind, Text string
	Ranges     []rune
	Children   []*PatternNode
	Min, Max   int
	Lazy       bool
}

func (p *Pattern) MatchString(s string) bool { return p.re.MatchString(s) }
func (s *Schema) Normalized() map[string]any { return cloneMap(s.normalized) }
func (c *Capability) Limits() Limits {
	i := func(k string) int64 {
		if v, ok := c.Policy[k].(int64); ok {
			return v
		}
		return 0
	}
	return Limits{i("max_input_bytes"), i("max_json_depth"), i("max_json_items"), i("max_input_file_bytes"), i("max_total_input_file_bytes"), i("max_input_files")}
}

// Normalized returns only effective configuration. Non-applicable defaults do
// not enter a capability's canonical definition. Secret resolution is later.
func (c *Capability) Normalized() map[string]any {
	out := map[string]any{"input": c.Input.Normalized(), "executor": cloneMap(c.Executor.Settings), "output": c.Output.normalized(), "policy": cloneMap(c.Policy)}
	if c.Description != "" {
		out["description"] = c.Description
	}
	if c.Annotations != nil {
		out["annotations"] = cloneMap(c.Annotations)
	}
	if len(c.Examples) > 0 {
		a := make([]any, len(c.Examples))
		for i, v := range c.Examples {
			a[i] = v
		}
		out["examples"] = a
	}
	return out
}
func (o Output) normalized() map[string]any {
	m := o.Schema.Normalized()
	if o.Data {
		m["data"] = map[string]any{}
	}
	if o.Files != nil {
		m["files"] = cloneMap(o.Files)
	}
	if o.Mode != "" {
		m["mode"] = o.Mode
	}
	return m
}
func cloneMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		o := make([]any, len(x))
		for i, v := range x {
			o[i] = cloneValue(v)
		}
		return o
	case []string:
		return append([]string{}, x...)
	default:
		return v
	}
}
