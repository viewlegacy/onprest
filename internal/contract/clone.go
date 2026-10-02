package contract

// Clone returns a separately owned definition, including presence, declaration
// order and compiler-private normalized values. Prepared runtimes must never
// retain a caller-owned mutable compiler AST.
func (c *Capability) Clone() *Capability {
	if c == nil {
		return nil
	}
	v := *c
	v.Input = cloneSchema(c.Input)
	v.Output.Schema = cloneSchema(c.Output.Schema)
	if c.Output.Files != nil {
		v.Output.Files = cloneMap(c.Output.Files)
	}
	v.Executor.Settings = cloneMap(c.Executor.Settings)
	v.Policy = cloneMap(c.Policy)
	if c.Annotations != nil {
		v.Annotations = cloneMap(c.Annotations)
	}
	v.Examples = make([]map[string]any, len(c.Examples))
	for i, e := range c.Examples {
		v.Examples[i] = cloneMap(e)
	}
	return &v
}

func cloneSchema(s *Schema) *Schema {
	if s == nil {
		return nil
	}
	v := *s
	v.Types = append([]string(nil), s.Types...)
	v.PropertyOrder = append([]string(nil), s.PropertyOrder...)
	v.Required = append([]string(nil), s.Required...)
	if s.Properties != nil {
		v.Properties = make(map[string]*Schema, len(s.Properties))
		for k, p := range s.Properties {
			v.Properties[k] = cloneSchema(p)
		}
	}
	v.Items = cloneSchema(s.Items)
	v.Additional = cloneSchema(s.Additional)
	v.Default = cloneValue(s.Default)
	v.Constraints = cloneMap(s.Constraints)
	v.normalized = cloneMap(s.normalized)
	if s.File != nil {
		f := *s.File
		f.Extensions = append([]string(nil), f.Extensions...)
		f.MIMETypes = append([]string(nil), f.MIMETypes...)
		v.File = &f
	}
	if s.Pattern != nil {
		p := *s.Pattern
		p.AST = clonePatternNode(p.AST)
		v.Pattern = &p // regexp.Regexp is safe for concurrent matching.
	}
	return &v
}

func clonePatternNode(p *PatternNode) *PatternNode {
	if p == nil {
		return nil
	}
	v := *p
	v.Ranges = append([]rune(nil), p.Ranges...)
	v.Children = make([]*PatternNode, len(p.Children))
	for i, child := range p.Children {
		v.Children[i] = clonePatternNode(child)
	}
	return &v
}
