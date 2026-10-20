package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func metadataRoundTrip(t *testing.T, s *Schema) *Schema {
	t.Helper()
	raw, e := CanonicalJSON(s.InputMetadata())
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := CompileSchema(raw, true)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(canonical(t, s.Normalized()), canonical(t, decoded.Normalized())) {
		t.Fatal("metadata changed canonical schema")
	}
	return decoded
}

func TestInputMetadataPatternRoundTripAndCanonicalHash(t *testing.T) {
	cases := commonPatternCases()
	cases = append(cases, struct {
		pattern string
		yes, no []string
	}{"^[A-Za-z0-9 ]{1,64}$", []string{"Alice", "A1 B"}, []string{"=SUM(A1)", "Alice!", "", string(bytes.Repeat([]byte{'a'}, 65))}})
	for i, fixture := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			source, _ := json.Marshal(map[string]any{"properties": map[string]any{"text": map[string]any{"type": "string", "pattern": fixture.pattern}}})
			s, e := CompileSchema(source, true)
			if e != nil {
				t.Fatal(e)
			}
			before := canonical(t, s.Normalized())
			for _, original := range []*Schema{s, cloneSchema(s)} {
				wire := original.InputMetadata()
				if wire["properties"].(map[string]any)["text"].(map[string]any)["pattern"] != fixture.pattern {
					t.Fatal("pattern source not preserved")
				}
				restored := metadataRoundTrip(t, original)
				for _, value := range fixture.yes {
					if !restored.Properties["text"].Pattern.MatchString(value) {
						t.Fatalf("accepted source value rejected: %q", value)
					}
				}
				for _, value := range fixture.no {
					if restored.Properties["text"].Pattern.MatchString(value) {
						t.Fatalf("rejected source value accepted: %q", value)
					}
				}
				if original.Properties["text"].Pattern.ECMAExpression() != restored.Properties["text"].Pattern.ECMAExpression() {
					t.Fatal("public ECMA changed")
				}
				wire["properties"].(map[string]any)["text"].(map[string]any)["pattern"] = "corrupt"
			}
			if !bytes.Equal(before, canonical(t, s.Normalized())) {
				t.Fatal("metadata/clone mutated original")
			}
		})
	}
	a := compile(t, wasm+"\ninput: {properties: {text: {type: string, pattern: '^(a|b)\\d$'}}}\n", "")
	b := compile(t, wasm+"\ninput: {properties: {text: {type: string, pattern: '^(?:a|b)[0-9]$'}}}\n", "")
	if !bytes.Equal(canonical(t, a.Normalized()), canonical(t, b.Normalized())) {
		t.Fatal("equivalent pattern hash input changed")
	}
	sameDefinition(t, a, a.Clone())
	if !bytes.Equal(canonical(t, a.Normalized()), canonical(t, a.Clone().Normalized())) {
		t.Fatal("clone hash input changed")
	}
}

func TestInputMetadataTreeValuesNumbersFilesAndOwnership(t *testing.T) {
	source := []byte(`properties:
  nested: {properties: {text: {type: string, pattern: '^a$'}}}
  array: {items: {type: string, pattern: '^b$'}}
  dictionary: {type: object, additionalProperties: {type: string, pattern: '^c$'}}
  nullable: {type: [string, "null"], pattern: '^n$', default: null}
  number: {type: integer, minimum: 9007199254740993, maximum: 9223372036854775807, default: 9007199254740993}
  opaque:
    type: object
    additionalProperties: true
    default: &value {pattern: literal, properties: {pattern: {kind: ordinary}}, items: [{pattern: ordinary}], additionalProperties: {pattern: ordinary}, large: 9223372036854775807}
    const: *value
    enum: [*value]
  f: {type: file, extensions: [txt], mime_types: [text/plain], max_bytes: 5B, allow_empty: false}
required: [nested, array, dictionary, f]
`)
	s, e := CompileSchema(source, true)
	if e != nil {
		t.Fatal(e)
	}
	before := canonical(t, s.Normalized())
	restored := metadataRoundTrip(t, cloneSchema(s))
	for _, key := range []string{"default", "const", "enum"} {
		if !reflect.DeepEqual(s.InputMetadata()["properties"].(map[string]any)["opaque"].(map[string]any)[key], s.Properties["opaque"].Normalized()[key]) {
			t.Fatalf("ordinary %s JSON interpreted as schema", key)
		}
	}
	if !reflect.DeepEqual(restored.Properties["f"].File, s.Properties["f"].File) {
		t.Fatal("file constraints changed")
	}
	limits := Limits{MaxBytes: 4096, MaxDepth: 12, MaxItems: 100, MaxFileBytes: 4, MaxTotalFileBytes: 4, MaxFiles: 1}
	input := map[string]any{"nested": map[string]any{"text": "a"}, "array": []any{"b"}, "dictionary": map[string]any{"key": "c"}, "f": file("/f", "a.txt", "text/plain", 4)}
	for _, schema := range []*Schema{s, restored} {
		got, leaves, e := ValidateInput(schema, input, limits)
		if e != nil || len(leaves) != 1 || leaves[0].Schema.File.MaxBytes != 4 || got["number"] != int64(9007199254740993) || got["nullable"] != nil {
			t.Fatal(got, leaves, e)
		}
		if got["opaque"].(map[string]any)["large"] != int64(9223372036854775807) {
			t.Fatal("ordinary int64 default rounded")
		}
		for _, invalid := range []map[string]any{
			{"nested": map[string]any{"text": "x"}}, {"array": []any{"x"}}, {"dictionary": map[string]any{"key": "x"}}, {"number": int64(9007199254740992)}, {"f": file("/f", "a.txt", "text/plain", 5)}, {"f": file("/f", "a.bin", "text/plain", 4)}, {"f": file("/f", "a.txt", "application/json", 4)}, {"f": file("/f", "a.txt", "text/plain", 0)},
		} {
			bad := cloneMap(input)
			for k, v := range invalid {
				bad[k] = v
			}
			if _, _, e := ValidateInput(schema, bad, limits); e == nil {
				t.Fatal("invalid round-trip input accepted", bad)
			}
		}
		missing := cloneMap(input)
		delete(missing, "nested")
		if _, _, e := ValidateInput(schema, missing, limits); e == nil {
			t.Fatal("required lost")
		}
	}
	wire := s.InputMetadata()
	properties := wire["properties"].(map[string]any)
	properties["opaque"].(map[string]any)["default"].(map[string]any)["pattern"] = "changed"
	properties["f"].(map[string]any)["max_bytes"] = int64(1)
	properties["nested"].(map[string]any)["properties"].(map[string]any)["text"].(map[string]any)["pattern"] = "changed"
	if !bytes.Equal(before, canonical(t, s.Normalized())) || s.Properties["f"].File.MaxBytes != 5 {
		t.Fatal("metadata changed owned schema")
	}
	metadataRoundTrip(t, s)
}
