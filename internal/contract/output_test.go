package contract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
)

func assertOutputCode(t *testing.T, e error, want string) {
	t.Helper()
	var out *OutputError
	if !errors.As(e, &out) || out.Code != want {
		t.Fatalf("error=%v want %s", e, want)
	}
}
func TestOutputProjectionModesAndNull(t *testing.T) {
	cases := []struct {
		name, output string
		input, want  any
		fail         bool
	}{
		{"all", "data: {}", map[string]any{"A": int64(1), "B": "x"}, map[string]any{"A": int64(1), "B": "x"}, false},
		{"empty_properties", "properties: {}\ndata: {}", map[string]any{"A": int64(1)}, map[string]any{}, false},
		{"selected_nested", "properties: {A: {properties: {keep: {type: integer}}}}\ndata: {}", map[string]any{"A": map[string]any{"keep": int64(1), "secret": "hidden"}, "B": "hidden"}, map[string]any{"A": map[string]any{"keep": int64(1)}}, false},
		{"nullable_enum", "properties: {A: {type: integer, enum: [1]}}\ndata: {}", map[string]any{"A": nil}, map[string]any{"A": nil}, false},
		{"nullable_const", "properties: {A: {type: string, const: yes}}\ndata: {}", map[string]any{"A": nil}, map[string]any{"A": nil}, false},
		{"nullable_false", "properties: {A: {type: integer, nullable: false}}\ndata: {}", map[string]any{"A": nil}, nil, true},
		{"untyped_nonnull", "nullable: false\ndata: {}", nil, nil, true},
		{"required_null", "properties: {A: {type: integer}}\nrequired: [A]\ndata: {}", map[string]any{"A": nil}, map[string]any{"A": nil}, false},
		{"required_missing", "properties: {A: {type: integer}}\nrequired: [A]\ndata: {}", map[string]any{}, nil, true},
		{"optional_missing", "properties: {A: {type: integer}}\ndata: {}", map[string]any{}, map[string]any{}, false},
		{"passthrough", "mode: passthrough\nproperties: {A: {type: integer}}\ndata: {}", map[string]any{"B": map[string]any{"secret": "retained"}}, map[string]any{"B": map[string]any{"secret": "retained"}}, false},
		{"passthrough_nested_false", "mode: passthrough\nproperties: {A: {properties: {keep: {}}, additionalProperties: false}}\ndata: {}", map[string]any{"A": map[string]any{"unknown": 1}}, nil, true},
		{"additional_child_projection", "type: object\nadditionalProperties: {properties: {keep: {type: boolean}}}\ndata: {}", map[string]any{"x": map[string]any{"keep": true, "drop": "hidden"}}, map[string]any{"x": map[string]any{"keep": true}}, false},
		{"array_no_items", "type: array\ndata: {}", []any{map[string]any{"all": int64(1)}, nil}, []any{map[string]any{"all": int64(1)}, nil}, false},
		{"no_type_coercion", "properties: {A: {type: integer}}\ndata: {}", map[string]any{"A": "1"}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compile(t, wasm+"\noutput:\n"+indent(tc.output, 2), "")
			before := string(canonical(t, tc.input))
			definition := string(canonical(t, c.Normalized()))
			got, e := c.ProjectOutput(tc.input, nil)
			if tc.fail {
				assertOutputCode(t, e, "OUTPUT_INVALID")
			} else {
				if e != nil {
					t.Fatal(e)
				}
				if !equalJSON(got, tc.want) {
					t.Fatalf("got=%#v want=%#v", got, tc.want)
				}
			}
			if string(canonical(t, tc.input)) != before || string(canonical(t, c.Normalized())) != definition {
				t.Fatal("projection mutated input or definition")
			}
		})
	}
}

const selectExecutor = `executor: {type: db, source: db, sql: 'SELECT id, label FROM customers'}`

const dmlExecutor = `executor: {type: db, source: db, sql: 'UPDATE customers SET id=1'}
policy: {readonly: false}`

func TestOutputDMLCountDefinitionCannotExcludeProviderValue(t *testing.T) {
	for _, mode := range []string{"", "mode: passthrough\n"} {
		for _, definition := range []string{
			"type: object\nadditionalProperties: false",
			"type: object\nadditionalProperties: {type: string}",
			"type: object\nadditionalProperties: {type: 'null'}",
			"properties: {}",
		} {
			t.Run(mode+definition, func(t *testing.T) {
				if _, e := Compile([]byte(fixture(dmlExecutor+"\noutput:\n"+indent(mode+definition+"\ndata: {}", 2), ""))); e == nil {
					t.Fatal("accepted definition excluding fixed DML count")
				}
			})
		}
		for _, definition := range []string{
			"", "type: [object,string]", "type: object\nadditionalProperties: true",
			"properties: {count: {type: integer}}\nadditionalProperties: false",
			"type: object\nadditionalProperties: {type: number, minimum: 2, maximum: 4}",
		} {
			t.Run(mode+definition+"_retains_count", func(t *testing.T) {
				c := compile(t, dmlExecutor+"\noutput:\n"+indent(mode+definition+"\ndata: {}", 2), "")
				out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: map[string]any{"count": int64(3)}}, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer out.Close()
				if !equalJSON(out.Data, map[string]any{"count": int64(3)}) {
					t.Fatalf("lost fixed provider count: %#v", out.Data)
				}
			})
		}
	}
}

func TestOutputCSVClosedPassthroughColumns(t *testing.T) {
	for _, executor := range []string{wasm, selectExecutor} {
		for _, items := range []string{"properties: {b: {type: string}, a: {type: string}}", "type: object"} {
			definition := executor + "\noutput: {mode: passthrough, items: {" + items + ", additionalProperties: false}, data: {}, files: {format: csv, columns: [secret], filename: x.csv}}"
			if _, e := Compile([]byte(fixture(definition, ""))); e == nil {
				t.Fatal("accepted CSV column forbidden by closed passthrough object")
			}
		}
		c := compile(t, executor+"\noutput: {mode: passthrough, items: {properties: {b: {type: string}, a: {type: string}}, additionalProperties: false}, data: {}, files: {format: csv, filename: x.csv}}", "")
		out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: []any{map[string]any{"a": "A", "b": "B"}}, Columns: []string{"a", "b"}}, &memoryStager{})
		if e != nil {
			t.Fatal(e)
		}
		if got := string(outputArtifactBytes(t, out)); got != "b,a\r\nB,A\r\n" {
			t.Fatalf("closed passthrough declaration order lost: %q", got)
		}
		out.Close()
		sink := &memoryStager{}
		out, e = ProcessOutput(context.Background(), c, ProviderResult{Data: []any{map[string]any{"a": "A", "b": "B", "secret": "hidden"}}, Columns: []string{"a", "b", "secret"}}, sink)
		assertOutputCode(t, e, "OUTPUT_INVALID")
		if out != nil || len(sink.stages) != 0 {
			t.Fatal("unknown passthrough key reached artifact publication")
		}
	}
	// A dictionary schema still permits dynamic columns and validates their cells.
	c := compile(t, wasm+"\noutput: {mode: passthrough, items: {type: object, additionalProperties: {type: string}}, data: {}, files: {format: csv, columns: [dynamic], filename: x.csv}}", "")
	out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: []any{map[string]any{"dynamic": "value"}}}, &memoryStager{})
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	if got := string(outputArtifactBytes(t, out)); got != "dynamic\r\nvalue\r\n" {
		t.Fatal(got)
	}
	if _, e := c.ProjectOutput([]any{map[string]any{"dynamic": true}}, nil); e == nil {
		t.Fatal("dynamic column bypassed dictionary value schema")
	}
}

func TestOutputProviderShapeMetadataAndPrecision(t *testing.T) {
	selected := compile(t, selectExecutor+"\n"+`output: {items: {properties: {id: {type: integer}, label: {type: string}}}, data: {}}`, "")
	for _, tc := range []struct {
		name    string
		data    any
		columns []string
		valid   bool
	}{
		{"empty", []any{}, []string{"id", "label"}, true}, {"empty_missing", []any{}, []string{"id"}, false},
		{"duplicate_metadata", []any{}, []string{"id", "id", "label"}, false}, {"null_root", nil, nil, false}, {"object_root", map[string]any{}, nil, false}, {"null_row", []any{nil}, []string{"id", "label"}, false},
		{"row_missing", []any{map[string]any{"id": int64(1)}}, []string{"id", "label"}, false}, {"nullable_cell", []any{map[string]any{"id": nil, "label": nil}}, []string{"id", "label"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, e := selected.ProjectOutput(tc.data, tc.columns)
			if (e == nil) != tc.valid {
				t.Fatalf("result=%#v error=%v", out, e)
			}
		})
	}
	pass := compile(t, selectExecutor+"\noutput: {mode: passthrough, items: {properties: {optional: {type: integer}}}, data: {}}", "")
	if _, e := pass.ProjectOutput([]any{map[string]any{"id": int64(1)}}, []string{"id"}); e != nil {
		t.Fatal(e)
	}
	dml := compile(t, `executor: {type: db, source: db, sql: 'UPDATE customers SET id=1'}
policy: {readonly: false}
output: {properties: {count: {type: number}}, data: {}}`, "")
	out, e := ProcessOutput(context.Background(), dml, ProviderResult{Data: map[string]any{"count": int64(math.MaxInt64)}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if out.Data.(map[string]any)["count"] != int64(math.MaxInt64) {
		t.Fatal("DML count precision lost")
	}
	out.Close()
	for _, bad := range []any{nil, map[string]any{}, map[string]any{"count": int64(-1)}, map[string]any{"count": 1.5}, map[string]any{"count": int64(1), "extra": true}} {
		if _, e := dml.ProjectOutput(bad, nil); e == nil {
			t.Fatalf("accepted invalid DML %#v", bad)
		}
	}
}
func TestOutputJSONLimitsAndValidity(t *testing.T) {
	c := compile(t, wasm+"\noutput: {data: {}}\npolicy: {max_output_bytes: 3B}", "")
	for _, tc := range []struct {
		v     any
		valid bool
	}{{"", true}, {"a", true}, {"ab", false}} {
		_, e := c.ProjectOutput(tc.v, nil)
		if (e == nil) != tc.valid {
			t.Fatalf("bytes input=%#v error=%v", tc.v, e)
		}
	}
	c = compile(t, wasm+"\noutput: {data: {}}", "")
	cyclic := map[string]any{}
	cyclic["cycle"] = cyclic
	for name, bad := range map[string]any{"nan": math.NaN(), "infinity": math.Inf(1), "utf8": string([]byte{0xff}), "map_key": map[string]any{string([]byte{0xff}): true}, "cycle": cyclic, "input_file": FileValue{Name: "secret", SizeBytes: 1}, "channel": make(chan int)} {
		t.Run(name, func(t *testing.T) {
			if _, e := c.ProjectOutput(bad, nil); e == nil {
				t.Fatal("accepted invalid JSON")
			}
		})
	}
	c = compile(t, wasm+"\noutput: {data: {}}\npolicy: {max_json_depth: 2, max_json_items: 4}", "")
	if _, e := c.ProjectOutput(map[string]any{"x": int64(1)}, nil); e != nil {
		t.Fatal(e)
	}
	for _, v := range []any{map[string]any{"x": map[string]any{"y": int64(1)}}, map[string]any{"a": 1, "b": 2}} {
		_, e := c.ProjectOutput(v, nil)
		assertOutputCode(t, e, "LIMIT_EXCEEDED")
	}
	c = compile(t, selectExecutor+"\noutput: {data: {}}\npolicy: {max_output_bytes: 2B}", "")
	if _, e := c.ProjectOutput([]any{}, nil); e != nil {
		t.Fatal(e)
	}
	if out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: []any{}}, nil); e == nil || out != nil {
		t.Fatal("public rows/count wrapper exceeded limit undetected")
	}
}

// Test stager exercises the private-stage API, not an Agent spool substitute.
type memoryStager struct {
	stages                                  []*memoryStage
	beginErr, writeErr, closeErr, finishErr error
	short                                   bool
}
type memoryStage struct {
	bytes.Buffer
	owner          *memoryStager
	aborts, closes int
	maxWrite       int
	metadata       ArtifactMetadata
	reader         *trackedReader
}

func (s *memoryStager) Begin(context.Context, string, string) (ArtifactStage, error) {
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	stage := &memoryStage{owner: s}
	s.stages = append(s.stages, stage)
	return stage, nil
}
func (s *memoryStage) Write(p []byte) (int, error) {
	s.maxWrite = max(s.maxWrite, len(p))
	if s.owner.writeErr != nil {
		return 0, s.owner.writeErr
	}
	if s.owner.short && len(p) > 0 {
		return s.Buffer.Write(p[:len(p)-1])
	}
	return s.Buffer.Write(p)
}
func (s *memoryStage) Close() error { s.closes++; return s.owner.closeErr }
func (s *memoryStage) Finish(m ArtifactMetadata) (io.ReadCloser, error) {
	if s.owner.finishErr != nil {
		return nil, s.owner.finishErr
	}
	s.metadata = m
	s.reader = &trackedReader{Reader: bytes.NewReader(s.Bytes())}
	return s.reader, nil
}
func (s *memoryStage) Abort() error { s.aborts++; return nil }

type trackedReader struct {
	io.Reader
	closes   int
	closeErr error
}

func (r *trackedReader) Close() error { r.closes++; return r.closeErr }

type readFailure struct{}

func (readFailure) Read(p []byte) (int, error) {
	copy(p, "secret-host-path")
	return len("secret-host-path"), errors.New("secret-reader-detail")
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, nil }
func outputArtifactBytes(t *testing.T, o *PreparedOutput) []byte {
	t.Helper()
	if len(o.Files) != 1 {
		t.Fatalf("files=%d", len(o.Files))
	}
	b, e := io.ReadAll(o.Files[0].Reader)
	if e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(b)
	if o.Files[0].SizeBytes != int64(len(b)) || o.Files[0].SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("artifact metadata is not computed from actual bytes")
	}
	return b
}
func TestOutputCSVPublicationMatchesJSONColumns(t *testing.T) {
	c := compile(t, selectExecutor+"\n"+`output:
  items:
    properties:
      label: {type: string}
      id: {type: integer}
  data: {}
  files: {format: csv, filename: report.csv}`, "")
	source := ProviderResult{Data: []any{map[string]any{"id": int64(math.MaxInt64), "label": "  =SUM(1,2)", "secret": "never-publish"}}, Columns: []string{"secret", "id", "label"}}
	sink := &memoryStager{}
	out, e := ProcessOutput(context.Background(), c, source, sink)
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	if got := string(outputArtifactBytes(t, out)); got != "label,id\r\n\"'  =SUM(1,2)\",9223372036854775807\r\n" {
		t.Fatalf("CSV=%q", got)
	}
	data := out.Data.(map[string]any)
	if data["count"] != int64(1) {
		t.Fatal(data)
	}
	row := data["rows"].([]any)[0].(map[string]any)
	if row["id"] != int64(math.MaxInt64) || len(row) != 2 {
		t.Fatal(row)
	}
	if strings.Contains(string(canonical(t, out.Data)), "secret") {
		t.Fatal("hidden column leaked through JSON")
	}
}
func TestOutputCSVFromColumnsFormulaAndZeroRows(t *testing.T) {
	c := compile(t, wasm+"\n"+`output:
  properties:
    a/b:
      items: {properties: {label: {type: string}, n: {type: integer}, optional: {type: boolean}}}
  files: {format: csv, from: /a~1b, columns: [label, n, optional], filename: nested.csv}`, "")
	data := map[string]any{"a/b": []any{map[string]any{"label": "a,\"b\"\nc", "n": int64(-1)}, map[string]any{"label": nil, "n": nil, "optional": false}}}
	out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: data}, &memoryStager{})
	if e != nil {
		t.Fatal(e)
	}
	if out.Data != nil {
		t.Fatal("CSV-only data was published")
	}
	if got := string(outputArtifactBytes(t, out)); got != "label,n,optional\r\n\"a,\"\"b\"\"\nc\",-1,\r\n,,false\r\n" {
		t.Fatalf("CSV=%q", got)
	}
	out.Close()
	out, e = ProcessOutput(context.Background(), c, ProviderResult{Data: map[string]any{"a/b": []any{}}}, &memoryStager{})
	if e != nil {
		t.Fatal(e)
	}
	if got := string(outputArtifactBytes(t, out)); got != "label,n,optional\r\n" {
		t.Fatal(got)
	}
	out.Close()
	for _, data := range []any{map[string]any{}, map[string]any{"a/b": nil}, map[string]any{"a/b": []any{nil}}} {
		if out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: data}, &memoryStager{}); e == nil || out != nil {
			t.Fatalf("accepted CSV target %#v", data)
		}
	}
	for _, value := range []string{"=x", "+x", "-x", "@x", "\tx", "\rx", " \tx", "  =x", "\u2003@x"} {
		if got := escapeFormula(value); got != "'"+value {
			t.Fatalf("formula=%q", got)
		}
	}
	for _, value := range []string{"plain", "", " \nnormal"} {
		if got := escapeFormula(value); got != value {
			t.Fatalf("escaped safe string=%q", got)
		}
	}
	off := compile(t, wasm+"\noutput: {items: {properties: {s: {type: string}}}, files: {format: csv, filename: x.csv, formula_escape: false}}", "")
	out, e = ProcessOutput(context.Background(), off, ProviderResult{Data: []any{map[string]any{"s": "=x"}}}, &memoryStager{})
	if e != nil {
		t.Fatal(e)
	}
	if got := string(outputArtifactBytes(t, out)); got != "s\r\n=x\r\n" {
		t.Fatal(got)
	}
	out.Close()
}
func TestOutputOriginalArtifactsOwnershipAndIntegrity(t *testing.T) {
	c := compile(t, wasm+"\noutput: {data: {}, files: {}}", "")
	b := bytes.Repeat([]byte{0, 0xff, 0x5a}, 30000)
	size := int64(len(b))
	hash := sha256.Sum256(b)
	r := &trackedReader{Reader: bytes.NewReader(b)}
	sink := &memoryStager{}
	out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: "scalar", Files: []ProviderFile{{Name: "原本.bin", MIMEType: "application/octet-stream", Reader: r, SizeBytes: &size, SHA256: hex.EncodeToString(hash[:])}}}, sink)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(outputArtifactBytes(t, out), b) || out.Data != "scalar" || r.closes != 1 {
		t.Fatal("original bytes/data/ownership changed")
	}
	if e := out.Close(); e != nil {
		t.Fatal(e)
	}
	out.Close()
	if sink.stages[0].aborts != 1 || sink.stages[0].reader.closes != 1 {
		t.Fatal("cleanup was not once")
	}
	hidden := compile(t, wasm+"\noutput: {}", "")
	r = &trackedReader{Reader: readFailure{}}
	out, e = ProcessOutput(context.Background(), hidden, ProviderResult{Files: []ProviderFile{{Reader: r}}}, nil)
	if e != nil || out.Data != nil || len(out.Files) != 0 || r.closes != 1 {
		t.Fatalf("omitted output contract: %#v %v", out, e)
	}
	out.Close()
	for _, tc := range []struct {
		name   string
		reader io.Reader
		size   *int64
		digest string
	}{
		{"zero", bytes.NewReader(nil), nil, ""}, {"wrong_size", bytes.NewReader([]byte("x")), &size, ""}, {"wrong_digest", bytes.NewReader([]byte("x")), nil, strings.Repeat("0", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, e := ProcessOutput(context.Background(), c, ProviderResult{Files: []ProviderFile{{Name: "x.bin", MIMEType: "application/octet-stream", Reader: tc.reader, SizeBytes: tc.size, SHA256: tc.digest}}}, &memoryStager{})
			if tc.name == "zero" {
				if e != nil {
					t.Fatal(e)
				}
				if len(outputArtifactBytes(t, out)) != 0 {
					t.Fatal("zero file changed")
				}
				out.Close()
			} else {
				assertOutputCode(t, e, "OUTPUT_INVALID")
				if out != nil {
					t.Fatal("integrity failure published")
				}
			}
		})
	}
}
func TestOutputLateFailuresAbortAllStagesAndCloseReaders(t *testing.T) {
	c := compile(t, wasm+"\noutput: {files: {}}", "")
	for _, tc := range []struct {
		name     string
		reader   io.Reader
		closeErr error
		sink     *memoryStager
	}{
		{"read", readFailure{}, nil, &memoryStager{}}, {"no_progress", emptyReader{}, nil, &memoryStager{}}, {"close", strings.NewReader("b"), errors.New("secret-close"), &memoryStager{}},
		{"write", strings.NewReader("b"), nil, &memoryStager{writeErr: errors.New("secret-write")}}, {"short_write", strings.NewReader("b"), nil, &memoryStager{short: true}},
		{"flush", strings.NewReader("b"), nil, &memoryStager{closeErr: errors.New("secret-flush")}}, {"finish", strings.NewReader("b"), nil, &memoryStager{finishErr: errors.New("secret-finish")}},
		{"begin", strings.NewReader("b"), nil, &memoryStager{beginErr: errors.New("secret-begin")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &trackedReader{Reader: strings.NewReader("a")}
			second := &trackedReader{Reader: tc.reader, closeErr: tc.closeErr}
			out, e := ProcessOutput(context.Background(), c, ProviderResult{Files: []ProviderFile{{Name: "a", MIMEType: "text/plain", Reader: first}, {Name: "b", MIMEType: "text/plain", Reader: second}}}, tc.sink)
			assertOutputCode(t, e, "OUTPUT_INVALID")
			if out != nil || strings.Contains(e.Error(), "secret") {
				t.Fatal("partial output/detail published")
			}
			if first.closes != 1 || second.closes != 1 {
				t.Fatal("all source readers must close")
			}
			for _, s := range tc.sink.stages {
				if s.aborts != 1 {
					t.Fatal("stage leaked")
				}
				if s.reader != nil && s.reader.closes != 1 {
					t.Fatal("sealed reader leaked")
				}
			}
		})
	}
}
func TestOutputArtifactLimitsDirectAndCancellation(t *testing.T) {
	c := compile(t, wasm+"\noutput: {files: {}}\npolicy: {max_output_file_bytes: 3B, max_total_output_file_bytes: 4B, max_output_files: 2}", "")
	for _, tc := range []struct {
		name   string
		bodies []string
		valid  bool
	}{
		{"below", []string{"ab"}, true}, {"exact_single", []string{"abc"}, true}, {"over_single", []string{"abcd"}, false}, {"exact_total", []string{"ab", "cd"}, true}, {"over_total", []string{"abc", "de"}, false}, {"over_count", []string{"", "", ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []ProviderFile{}
			readers := []*trackedReader{}
			for _, b := range tc.bodies {
				r := &trackedReader{Reader: strings.NewReader(b)}
				readers = append(readers, r)
				files = append(files, ProviderFile{Name: "x", MIMEType: "text/plain", Reader: r})
			}
			sink := &memoryStager{}
			out, e := ProcessOutput(context.Background(), c, ProviderResult{Files: files}, sink)
			if tc.valid {
				if e != nil {
					t.Fatal(e)
				}
				out.Close()
			} else {
				assertOutputCode(t, e, "LIMIT_EXCEEDED")
				if out != nil {
					t.Fatal("over limit published")
				}
			}
			for _, r := range readers {
				if r.closes != 1 {
					t.Fatal("source reader leaked")
				}
			}
		})
	}
	direct := compile(t, wasm+"\noutput: {files: {delivery: {rest: direct}}}", "")
	if out, e := ProcessOutput(context.Background(), direct, ProviderResult{}, &memoryStager{}); e == nil || out != nil {
		t.Fatal("direct allowed zero files")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &trackedReader{Reader: strings.NewReader("x")}
	if out, e := ProcessOutput(ctx, c, ProviderResult{Files: []ProviderFile{{Reader: r}}}, &memoryStager{}); !errors.Is(e, context.Canceled) || out != nil || r.closes != 1 {
		t.Fatal("cancel did not release owned reader")
	}
}
func TestOutputCSVLateInvalidAndByteLimit(t *testing.T) {
	c := compile(t, wasm+"\noutput: {files: {format: csv, columns: [x], filename: x.csv}}", "")
	sink := &memoryStager{}
	out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: []any{map[string]any{"x": "valid"}, map[string]any{"x": map[string]any{"invalid": true}}}}, sink)
	assertOutputCode(t, e, "OUTPUT_INVALID")
	if out != nil || len(sink.stages) != 1 || sink.stages[0].aborts != 1 || sink.stages[0].reader != nil {
		t.Fatal("late invalid CSV published or leaked")
	}
	c = compile(t, wasm+"\noutput: {files: {format: csv, columns: [x], filename: x.csv}}\npolicy: {max_output_file_bytes: 3B}", "")
	out, e = ProcessOutput(context.Background(), c, ProviderResult{Data: []any{map[string]any{"x": "x"}}}, &memoryStager{})
	assertOutputCode(t, e, "LIMIT_EXCEEDED")
	if out != nil {
		t.Fatal("CSV file limit published")
	}
}
func TestOutputConcurrentDefinitionIsImmutable(t *testing.T) {
	c := compile(t, wasm+"\noutput: {properties: {x: {type: integer}}, data: {}}", "")
	before := string(canonical(t, c.Normalized()))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: map[string]any{"x": int64(math.MaxInt64), "private": true}}, nil)
				if e != nil {
					t.Error(e)
					return
				}
				if out.Data.(map[string]any)["x"] != int64(math.MaxInt64) {
					t.Error("precision loss")
				}
				out.Close()
				_ = c.SuccessSchema("rest")
				_ = c.InputMediaSchemas()
			}
		}()
	}
	wg.Wait()
	if before != string(canonical(t, c.Normalized())) {
		t.Fatal("shared definition changed")
	}
}
func TestEnvelopeAlwaysCarriesResultAndSafeError(t *testing.T) {
	body, e := json.Marshal(NewSuccessEnvelope("request", nil, nil))
	if e != nil {
		t.Fatal(e)
	}
	if string(body) != `{"request_id":"request","result":{"data":null,"files":[]}}` {
		t.Fatal(string(body))
	}
	for _, code := range systemErrorCodes {
		if e := (ErrorValue{Code: code, Message: "safe", Outcome: "unknown"}).Validate(); e != nil {
			t.Fatal(e)
		}
	}
	for _, tc := range []struct {
		name  string
		e     ErrorValue
		valid bool
	}{
		{"app_min", ErrorValue{Code: "APP_A", Message: "x", Outcome: "no_change"}, true}, {"app_max", ErrorValue{Code: "APP_" + strings.Repeat("A", 60), Message: strings.Repeat("x", 1024), Outcome: "committed"}, true},
		{"app_code_over", ErrorValue{Code: "APP_" + strings.Repeat("A", 61), Message: "x", Outcome: "unknown"}, false}, {"reserved", ErrorValue{Code: "GATEWAY_RAW_ERROR", Message: "x", Outcome: "unknown"}, false},
		{"empty", ErrorValue{Code: "APP_A", Outcome: "unknown"}, false}, {"over", ErrorValue{Code: "APP_A", Message: strings.Repeat("x", 1025), Outcome: "unknown"}, false}, {"utf8_bytes", ErrorValue{Code: "APP_A", Message: strings.Repeat("あ", 342), Outcome: "unknown"}, false},
		{"control", ErrorValue{Code: "APP_A", Message: "x\nx", Outcome: "unknown"}, false}, {"invalid_utf8", ErrorValue{Code: "APP_A", Message: string([]byte{0xff}), Outcome: "unknown"}, false},
		{"reason", ErrorValue{Code: "EXECUTION_TIMEOUT", Message: "safe", Reason: "UPSTREAM_RESPONSE_TIMEOUT", Outcome: "unknown"}, true}, {"raw_reason", ErrorValue{Code: "EXECUTION_TIMEOUT", Message: "safe", Reason: "https://secret", Outcome: "unknown"}, false},
		{"app_reason", ErrorValue{Code: "APP_A", Message: "safe", Reason: "CODE", Outcome: "unknown"}, false}, {"outcome", ErrorValue{Code: "APP_A", Message: "safe", Outcome: "retry"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if e := tc.e.Validate(); (e == nil) != tc.valid {
				t.Fatalf("validation=%v", e)
			}
		})
	}
}

func TestOutputArtifactMetadataAndSuppressedCleanup(t *testing.T) {
	c := compile(t, wasm+"\noutput: {files: {}}", "")
	for _, tc := range []struct {
		name, mime string
		reader     io.Reader
	}{
		{strings.Repeat("a", 255), "text/plain", strings.NewReader("")},
		{strings.Repeat("a", 256), "text/plain", strings.NewReader("")}, {"../x", "text/plain", strings.NewReader("x")}, {"x\nname", "text/plain", strings.NewReader("x")}, {string([]byte{0xff}), "text/plain", strings.NewReader("x")},
		{"x", "Text/Plain", strings.NewReader("x")}, {"x", "text/plain; charset=utf-8", strings.NewReader("x")}, {"x", "text/plain", nil}, {"x", "text/plain", (*trackedReader)(nil)},
	} {
		t.Run(tc.name+tc.mime, func(t *testing.T) {
			out, e := ProcessOutput(context.Background(), c, ProviderResult{Files: []ProviderFile{{Name: tc.name, MIMEType: tc.mime, Reader: tc.reader}}}, &memoryStager{})
			if len(tc.name) == 255 {
				if e != nil {
					t.Fatal(e)
				}
				out.Close()
			} else {
				assertOutputCode(t, e, "OUTPUT_INVALID")
				if out != nil {
					t.Fatal("invalid metadata published")
				}
			}
		})
	}
	csv := compile(t, wasm+"\noutput: {files: {format: csv, filename: x.csv, columns: [x]}}", "")
	r := &trackedReader{Reader: readFailure{}}
	if out, e := ProcessOutput(context.Background(), csv, ProviderResult{Data: []any{}, Files: []ProviderFile{{Reader: r}}}, &memoryStager{}); e == nil || out != nil || r.closes != 1 {
		t.Fatal("mixed original and generated files accepted or leaked")
	}
	hidden := compile(t, wasm, "")
	r = &trackedReader{Reader: readFailure{}, closeErr: errors.New("secret")}
	if out, e := ProcessOutput(context.Background(), hidden, ProviderResult{Files: []ProviderFile{{Reader: r}}}, nil); e == nil || out != nil || r.closes != 1 {
		t.Fatal("suppressed reader Close failure was ignored")
	}
}

type cancelReader struct{ cancel context.CancelFunc }

func (r cancelReader) Read(p []byte) (int, error) { r.cancel(); copy(p, "x"); return 1, io.EOF }
func TestOutputCancellationDuringPrivateStage(t *testing.T) {
	c := compile(t, wasm+"\noutput: {files: {}}", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &trackedReader{Reader: cancelReader{cancel}}
	sink := &memoryStager{}
	if out, e := ProcessOutput(ctx, c, ProviderResult{Files: []ProviderFile{{Name: "x", MIMEType: "text/plain", Reader: r}}}, sink); !errors.Is(e, context.Canceled) || out != nil || r.closes != 1 || len(sink.stages) != 1 || sink.stages[0].aborts != 1 {
		t.Fatal("cancel published or leaked")
	}
}

func TestOutputCSVProviderMetadataColumns(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		columns      []string
		csv          string
		valid        bool
	}{
		{"metadata_order", "files: {format: csv, filename: x.csv}", []string{"z", "a"}, "z,a\r\n", true},
		{"explicit_subset", "files: {format: csv, filename: x.csv, columns: [a]}", []string{"z", "a"}, "a\r\n", true},
		{"explicit_missing", "files: {format: csv, filename: x.csv, columns: [missing]}", []string{"a"}, "", false},
		{"empty_public_columns", "items: {properties: {}}\nfiles: {format: csv, filename: x.csv}", []string{"a"}, "", false},
		{"declared_order", "items: {properties: {z: {}, a: {}}}\nfiles: {format: csv, filename: x.csv}", []string{"a", "z"}, "z,a\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compile(t, selectExecutor+"\noutput:\n"+indent(tc.output, 2), "")
			out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: []any{}, Columns: tc.columns}, &memoryStager{})
			if tc.valid {
				if e != nil {
					t.Fatal(e)
				}
				if got := string(outputArtifactBytes(t, out)); got != tc.csv {
					t.Fatal(got)
				}
				out.Close()
			} else {
				assertOutputCode(t, e, "OUTPUT_INVALID")
				if out != nil {
					t.Fatal("invalid zero-row columns published")
				}
			}
		})
	}
}

func TestOutputCSVPreservesCarriageReturnInStringCell(t *testing.T) {
	c := compile(t, wasm+"\noutput: {items: {properties: {s: {type: string}}}, files: {format: csv, filename: x.csv}}", "")
	out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: []any{map[string]any{"s": "a\rb"}}}, &memoryStager{})
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	if got := string(outputArtifactBytes(t, out)); got != "s\r\n\"a\rb\"\r\n" {
		t.Fatalf("CSV string cell changed: %q", got)
	}
}

func TestOutputCSVLargeRecordUsesBoundedStageWrites(t *testing.T) {
	c := compile(t, wasm+"\noutput: {items: {properties: {s: {type: string}}}, files: {format: csv, filename: x.csv}}", "")
	sink := &memoryStager{}
	value := strings.Repeat("large", 20000) + "\rdata"
	out, e := ProcessOutput(context.Background(), c, ProviderResult{Data: []any{map[string]any{"s": value}}}, sink)
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	got := string(outputArtifactBytes(t, out))
	if got != "s\r\n\""+value+"\"\r\n" {
		t.Fatal("large CSV cell changed")
	}
	if sink.stages[0].maxWrite > 64*1024 {
		t.Fatalf("unbounded stage write=%d", sink.stages[0].maxWrite)
	}
}
