package contract

import "testing"

// An untyped child accepts any JSON value, while an object with properties or
// an explicit object type rejects undeclared properties.
func TestInputUndeclaredPropertiesByDeclaration(t *testing.T) {
	c := compile(t, wasm+"\n"+`input: {properties: {any: {}, described: {description: Free form.}, typed: {type: object}, declared: {properties: {keep: {type: integer}}}}}`, "")
	for name, params := range map[string]map[string]any{
		"untyped object":      {"any": map[string]any{"x": int64(1), "nested": []any{"a", nil}}},
		"untyped scalar":      {"any": "text"},
		"untyped null":        {"any": nil},
		"described value":     {"described": map[string]any{"x": true}},
		"empty typed object":  {"typed": map[string]any{}},
		"declared properties": {"declared": map[string]any{"keep": int64(1)}},
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			if _, _, e := c.ValidateInput(params); e != nil {
				t.Fatal(e)
			}
		})
	}
	for name, params := range map[string]map[string]any{
		"typed object extra":    {"typed": map[string]any{"x": int64(1)}},
		"declared object extra": {"declared": map[string]any{"keep": int64(1), "x": int64(2)}},
		"root extra":            {"unknown": int64(1)},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, _, e := c.ValidateInput(params); e == nil {
				t.Fatal("undeclared property accepted")
			}
		})
	}
}
