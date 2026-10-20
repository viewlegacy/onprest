package contract

import (
	"reflect"
	"testing"
)

// Enum values are normalized into CanonicalJSON byte order regardless of how
// many values there are or how short their canonical forms are.
func TestEnumNormalizesToCanonicalOrder(t *testing.T) {
	for name, tc := range map[string]struct {
		schema string
		want   []any
	}{
		"three integers":       {`{type: integer, enum: [3, 1, 2]}`, []any{int64(1), int64(2), int64(3)}},
		"four integers":        {`{type: integer, enum: [10, 9, 8, 7]}`, []any{int64(10), int64(7), int64(8), int64(9)}},
		"single-char strings":  {`{type: string, enum: [d, b, c, a]}`, []any{"a", "b", "c", "d"}},
		"strings and null":     {`{type: [string, 'null'], enum: [open, null, closed]}`, []any{"closed", "open", nil}},
		"integers and null":    {`{type: [integer, 'null'], enum: [3, null, 1, 2]}`, []any{int64(1), int64(2), int64(3), nil}},
		"many short integers":  {`{type: integer, enum: [9, 8, 7, 6, 5, 4, 3, 2, 1, 0]}`, []any{int64(0), int64(1), int64(2), int64(3), int64(4), int64(5), int64(6), int64(7), int64(8), int64(9)}},
		"mixed canonical size": {`{type: [integer, string], enum: [b, 20, a, 3]}`, []any{"a", "b", int64(20), int64(3)}},
	} {
		t.Run(name, func(t *testing.T) {
			s, e := CompileSchema([]byte("properties: {v: "+tc.schema+"}"), true)
			if e != nil {
				t.Fatal(e)
			}
			v := s.Properties["v"]
			if got := v.Constraints["enum"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("enum constraint=%#v, want %#v", got, tc.want)
			}
			if got := v.Normalized()["enum"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("normalized enum=%#v, want %#v", got, tc.want)
			}
			for i := 1; i < len(tc.want); i++ {
				a, _ := CanonicalJSON(tc.want[i-1])
				b, _ := CanonicalJSON(tc.want[i])
				if string(a) >= string(b) {
					t.Fatalf("expected order is not CanonicalJSON order at %d", i)
				}
			}
		})
	}
}
