package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

type csvFixture struct {
	dir        string
	store      *artifacts.Store
	prepared   *providers.Prepared
	controller *ExecutionController
	cap        *contract.Capability
}

func newCSVFixture(t *testing.T, body string, files map[string][]byte, quota int64) *csvFixture {
	t.Helper()
	dir := t.TempDir()
	folder := filepath.Join(dir, "reports")
	if e := os.Mkdir(folder, 0700); e != nil {
		t.Fatal(e)
	}
	for name, b := range files {
		if e := os.WriteFile(filepath.Join(folder, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	yaml := fmt.Sprintf("schema_version: 1\ngateway: {url: wss://gateway.example/ws/agent, agent_private_key: %s}\nsources: {folders: {reports: {root: %q, access: read}}}\ncapabilities:\n  csv:\n%s", testAgentPrivateKey, folder, body)
	cfg, e := contract.Compile([]byte(yaml))
	if e != nil {
		t.Fatal(e, yaml)
	}
	registry, _ := providers.NewRegistry(map[string]providers.Factory{"csv": prepareCSVProvider})
	prepared, e := registry.Prepare(context.Background(), cfg, filepath.Join(dir, "agent.yaml"), providers.ResolveOptions{})
	if e != nil {
		t.Fatal(e)
	}
	store, e := artifacts.Open(artifacts.Options{Directory: filepath.Join(dir, "spool"), QuotaBytes: quota, MaxObjects: 64, MaxPending: 4, TransferTimeout: time.Second})
	if e != nil {
		prepared.Close()
		t.Fatal(e)
	}
	controller, e := NewExecutionController(prepared, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := prepared.Close(); e != nil {
			t.Error(e)
		}
		if e := store.Close(); e != nil {
			t.Error(e)
		}
	})
	return &csvFixture{dir, store, prepared, controller, cfg.Capabilities["csv"]}
}
func (f *csvFixture) run(ctx context.Context, in map[string]any) (*contract.PreparedOutput, *contract.ErrorValue) {
	owner := artifacts.Owner{RequestID: "csv-request", CallerBinding: "caller", Capability: "csv", DefinitionHash: "hash", AuthGeneration: "generation"}
	var stager contract.ArtifactStager
	if f.cap.Output.Files != nil {
		stager, _ = f.store.Output(owner, artifacts.OutputLimits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20, MaxFiles: 4, TTL: time.Minute})
	}
	return f.controller.Execute(context.WithValue(ctx, artifactStoreKey{}, f.store), ExecutionRequest{ID: "csv-request", Capability: "csv", Params: in, stager: stager})
}

const csvFolder = `    executor: {type: csv, read: {source: reports, selector: {name: a.csv}}}
    output: {data: {}}
`

func TestCSVColumnsStringsPartialTypingAndCommonOutput(t *testing.T) {
	for _, config := range []string{"", ", csv: {}", ", csv: {columns: {}}", ", csv: {columns: {amount: {type: integer}}}"} {
		t.Run(config, func(t *testing.T) {
			body := strings.Replace(csvFolder, "}}}", "}}"+config+"}", 1)
			f := newCSVFixture(t, body, map[string][]byte{"a.csv": []byte("id,amount,secret,empty\r\n00123,42,=private,\r\n")}, 1<<20)
			out, e := f.run(context.Background(), map[string]any{})
			if e != nil {
				t.Fatal(e)
			}
			defer out.Close()
			row := out.Data.(map[string]any)["rows"].([]any)[0].(map[string]any)
			if row["id"] != "00123" || row["secret"] != "=private" || row["empty"] != "" {
				t.Fatal(row)
			}
			want := any("42")
			if strings.Contains(config, "integer") {
				want = int64(42)
			}
			if row["amount"] != want {
				t.Fatal(row)
			}
		})
	}
	body := `    executor: {type: csv, read: {source: reports, selector: {name: a.csv}}, csv: {columns: {amount: {type: integer}}}, where: [{column: secret, op: eq, input: /filters/key}]}
    input: {properties: {filters: {properties: {key: {type: string}}, required: [key]}}, required: [filters]}
    output:
      items: {properties: {amount: {type: integer}, id: {type: string}}}
      data: {}
      files: {format: csv, filename: result.csv}
`
	f := newCSVFixture(t, body, map[string][]byte{"a.csv": []byte("id,amount,secret\n=cmd,9223372036854775807,allowed\n00123,0,other\n")}, 1<<20)
	out, e := f.run(context.Background(), map[string]any{"filters": map[string]any{"key": "allowed"}})
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	row := out.Data.(map[string]any)["rows"].([]any)[0].(map[string]any)
	if len(row) != 2 || row["amount"] != int64(9223372036854775807) || *out.ReturnedRows != 1 {
		t.Fatal(row)
	}
	b, err := io.ReadAll(out.Files[0].Reader)
	if err != nil || string(b) != "amount,id\r\n9223372036854775807,'=cmd\r\n" {
		t.Fatal(string(b), err)
	}
}
func TestCSVParserQuotesEncodingBOMAndNewlines(t *testing.T) {
	cases := []struct {
		name, encoding string
		raw            []byte
		want           []string
		bad            bool
	}{
		{"utf8", "utf-8", []byte("\xef\xbb\xbfa,\"x\r\ny\nz\rw\",\"\"\"quote\"\"\"\r\n"), []string{"\ufeffa", "x\r\ny\nz\rw", "\"quote\""}, false},
		{"jis", "shift_jis", []byte{0x81, 0x60, ',', 0x81, 0x61, ',', 0x81, 0x7c, ',', 0x81, 0x91, ',', 0x81, 0x92, ',', 0x81, 0xca, '\n'}, []string{"〜", "‖", "−", "¢", "£", "¬"}, false},
		{"windows", "cp932", []byte{0x81, 0x60, ',', 0x81, 0x61, ',', 0x81, 0x7c, ',', 0x81, 0x91, ',', 0x81, 0x92, ',', 0x81, 0xca, '\n'}, []string{"～", "∥", "－", "￠", "￡", "￢"}, false},
		{"extensions", "cp932", []byte{0x87, 0x40, ',', 0xfa, 0x40, ',', 0xf0, 0x40, '\n'}, []string{"①", "ⅰ", "\ue000"}, false},
		{"jis extension", "shift_jis", []byte{0x87, 0x40}, nil, true},
		{"jis udc", "shift_jis", []byte{0xf0, 0x40}, nil, true},
		{"invalid utf8", "utf-8", []byte{0xff}, nil, true},
		{"valid replacement", "utf-8", []byte("�"), []string{"�"}, false},
		{"truncated", "cp932", []byte{0x81}, nil, true},
		{"invalid trail", "cp932", []byte{0x81, 0x7f}, nil, true},
		{"unassigned", "cp932", []byte{0x81, 0xad}, nil, true},
		{"quote", "utf-8", []byte("a,\"unfinished"), nil, true},
		{"quote junk", "utf-8", []byte("\"a\" x"), nil, true},
		{"unquoted quote", "utf-8", []byte("x\"a"), nil, true},
		{"bare CR", "utf-8", []byte("a\rb"), nil, true},
		{"bare CR EOF", "utf-8", []byte("a\r"), nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := csvRecords{chars: newCSVCharacters(context.Background(), bytes.NewReader(c.raw), c.encoding), delimiter: ','}
			got, e := p.read()
			if c.bad {
				if e == nil {
					t.Fatal(got)
				}
				return
			}
			if e != nil || !reflect.DeepEqual(got, c.want) {
				t.Fatal(got, e, c.want)
			}
		})
	}
}
func TestCSVTypedCellsNullAndWhereOperations(t *testing.T) {
	for _, typ := range []string{"string", "integer", "number", "boolean"} {
		column := map[string]any{"type": typ, "nullable": true, "null_values": []string{"NULL", ""}}
		for _, v := range []string{"NULL", ""} {
			got, e := csvCell(v, column)
			if e != nil || got != nil {
				t.Fatal(typ, got, e)
			}
		}
		column["nullable"] = false
		if _, e := csvCell("NULL", column); e == nil {
			t.Fatal("null accepted")
		}
	}
	for _, test := range []struct {
		typ, cell string
		want      any
	}{
		{"integer", "+00123", int64(123)}, {"integer", "-9223372036854775808", int64(-9223372036854775808)},
		{"number", ".5", float64(.5)}, {"number", "-1.e+3", float64(-1000)}, {"boolean", "true", true}, {"boolean", "false", false},
	} {
		got, err := csvCell(test.cell, map[string]any{"type": test.typ, "nullable": false, "null_values": []string{}})
		if err != nil || got != test.want {
			t.Fatal(test, got, err)
		}
	}

	bad := map[string][]string{"integer": {"", " 1", "1 ", "1.2", "9223372036854775808"}, "number": {"", "NaN", "Inf", "0x1p2", "1e999", " 1"}, "boolean": {"1", "TRUE", "True", "", "false "}}
	for typ, values := range bad {
		for _, v := range values {
			if _, e := csvCell(v, map[string]any{"type": typ, "nullable": false, "null_values": []string{}}); e == nil {
				t.Fatal(typ, v)
			}
		}
	}
	for _, pair := range [][2]any{{"a", "b"}, {int64(9007199254740992), int64(9007199254740993)}, {1.5, 2.5}, {false, true}} {
		for _, op := range []string{"eq", "ne", "lt", "lte", "gt", "gte", "in"} {
			arg := pair[1]
			want := op == "ne" || op == "lt" || op == "lte"
			if op == "in" {
				arg = []any{pair[1], pair[0]}
				want = true
			}
			if csvMatches(pair[0], op, arg) != want {
				t.Fatal(pair, op)
			}
			if csvMatches(nil, op, arg) {
				t.Fatal("null matched", op)
			}
		}
	}
}
func TestCSVHeaderlessMultipleOrderZeroAndFailures(t *testing.T) {
	cases := []struct {
		name, executor, output, policy string
		files                          map[string][]byte
		code                           string
		count                          int64
		csv                            string
	}{
		{name: "headerless", executor: `csv: {header: false, columns: {b: {position: 1, type: integer}, a: {position: 0}}}`, files: map[string][]byte{"a.csv": []byte("00123,7\n")}, count: 1},
		{name: "zero", files: map[string][]byte{"a.csv": []byte("a,b\n")}, output: `items: {properties: {b: {type: string}, a: {type: string}}}, files: {format: csv, filename: result.csv}`, count: 0, csv: "b,a\r\n"},
		{name: "explicit subset", files: map[string][]byte{"a.csv": []byte("a,b\n1,2\n")}, output: `files: {format: csv, filename: result.csv, columns: [b]}`, count: 1, csv: "b\r\n2\r\n"},
		{name: "metadata order", files: map[string][]byte{"a.csv": []byte("b,a\n2,1\n")}, output: `files: {format: csv, filename: result.csv}`, count: 1, csv: "b,a\r\n2,1\r\n"},
		{name: "missing parsed zero", executor: `csv: {columns: {missing: {type: integer}}}`, files: map[string][]byte{"a.csv": []byte("a,b\n")}, code: "EXECUTION_FAILED"},
		{name: "missing published zero", output: `items: {properties: {missing: {type: string}}}, data: {}`, files: map[string][]byte{"a.csv": []byte("a,b\n")}, code: "OUTPUT_INVALID"},
		{name: "duplicate header", files: map[string][]byte{"a.csv": []byte("a,a\n")}, code: "EXECUTION_FAILED"},
		{name: "short row", files: map[string][]byte{"a.csv": []byte("a,b\n1\n")}, code: "EXECUTION_FAILED"},
		{name: "typed late failure", executor: `csv: {columns: {a: {type: integer}}}`, files: map[string][]byte{"a.csv": []byte("a,b\n1,ok\nbad,private\n")}, code: "EXECUTION_FAILED"},
		{name: "output does not coerce", output: `items: {properties: {a: {type: integer}}}, data: {}`, files: map[string][]byte{"a.csv": []byte("a\n1\n")}, code: "OUTPUT_INVALID"},
		{name: "rows boundary", policy: "max_rows: 1", files: map[string][]byte{"a.csv": []byte("a\n1\n2\n")}, code: "LIMIT_EXCEEDED"},
		{name: "items", policy: "max_json_items: 5", files: map[string][]byte{"a.csv": []byte("a,b\n1,2\n")}, code: "LIMIT_EXCEEDED"},
		{name: "depth", policy: "max_json_depth: 2", files: map[string][]byte{"a.csv": []byte("a\n1\n")}, code: "LIMIT_EXCEEDED"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := csvFolder
			if c.executor != "" {
				body = strings.Replace(body, "}}}", "}}, "+c.executor+"}", 1)
			}
			if c.output != "" {
				body = strings.Replace(body, "data: {}", c.output, 1)
			}
			if c.policy != "" {
				body += "    policy: {" + c.policy + "}\n"
			}
			f := newCSVFixture(t, body, c.files, 1<<20)
			out, e := f.run(context.Background(), map[string]any{})
			if c.code != "" {
				if out != nil || e == nil || e.Code != c.code || e.Outcome != "no_change" {
					t.Fatal(out, e)
				}
				if f.store.Usage() != 0 {
					t.Fatal("quota leak")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if *out.ReturnedRows != c.count {
				t.Fatal(*out.ReturnedRows)
			}
			if c.csv != "" {
				b, e := io.ReadAll(out.Files[0].Reader)
				if e != nil || string(b) != c.csv {
					t.Fatal(string(b), e)
				}
			}
			out.Close()
			if f.store.Usage() != 0 {
				t.Fatal("quota leak")
			}
		})
	}
	body := strings.Replace(csvFolder, "name: a.csv", "name_regex: '.*[.]csv', take: 3, sort: {by: name, order: desc}", 1)
	f := newCSVFixture(t, body, map[string][]byte{"a.csv": []byte("a\na\n"), "b.csv": []byte("a\nb\n")}, 1<<20)
	out, e := f.run(context.Background(), map[string]any{})
	if e != nil {
		t.Fatal(e)
	}
	rows := out.Data.(map[string]any)["rows"].([]any)
	if rows[0].(map[string]any)["a"] != "b" || rows[1].(map[string]any)["a"] != "a" {
		t.Fatal(rows)
	}
	out.Close()
	os.WriteFile(filepath.Join(f.dir, "reports", "b.csv"), []byte("different\n"), 0600)
	out, e = f.run(context.Background(), map[string]any{})
	if out != nil || e == nil || e.Code != "EXECUTION_FAILED" {
		t.Fatal(out, e)
	}
}
func TestCSVQuotaCancellationImmutabilityAndConcurrentPublication(t *testing.T) {
	f := newCSVFixture(t, csvFolder, map[string][]byte{"a.csv": []byte("a\n" + strings.Repeat("x", 8192) + "\n")}, 16384)
	reservation, e := f.store.Reserve(8192)
	if e != nil {
		t.Fatal(e)
	}
	out, failure := f.run(context.Background(), map[string]any{})
	if out != nil || failure == nil || failure.Code != "LIMIT_EXCEEDED" {
		t.Fatal(out, failure)
	}
	reservation.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, failure = f.run(ctx, map[string]any{})
	if out != nil || failure == nil || failure.Code != "EXECUTION_TIMEOUT" || failure.Outcome != "not_started" {
		t.Fatal(out, failure)
	}
	snapshot, _ := f.prepared.Snapshot("csv")
	snapshot.Definition().Executor.Settings["csv"].(map[string]any)["encoding"] = "invalid"
	os.WriteFile(filepath.Join(f.dir, "reports", "a.csv"), []byte("a\nx\n"), 0600)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, e := f.run(context.Background(), map[string]any{})
			if e != nil {
				t.Error(e)
				return
			}
			if out.Data.(map[string]any)["rows"].([]any)[0].(map[string]any)["a"] != "x" {
				t.Error(out.Data)
			}
			if e := out.Close(); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if f.store.Usage() != 0 {
		t.Fatal(f.store.Usage())
	}
	// No callback observes success before the complete late-broken source is read.
	os.WriteFile(filepath.Join(f.dir, "reports", "a.csv"), []byte("a\nok\n\"broken"), 0600)
	lease, e := f.prepared.Acquire("csv")
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Release()
	calls := 0
	out, err, ok := lease.ExecutePublication(context.WithValue(context.Background(), artifactStoreKey{}, f.store), map[string]any{}, &providers.Effects{}, func(contract.ProviderResult) (*contract.PreparedOutput, error) { calls++; return nil, nil })
	if !ok || err == nil || out != nil || calls != 0 {
		t.Fatal(ok, err, calls)
	}
}
func TestCSVFilterNullRejectedBeforeFolderReadAndMissingColumnAtZero(t *testing.T) {
	body := `    executor: {type: csv, read: {source: reports, selector: {name: missing.csv}}, where: [{column: a, op: eq, input: /arg}]}
    input: {properties: {arg: {type: [string, "null"]}}, required: [arg]}
    output: {data: {}}
`
	f := newCSVFixture(t, body, nil, 1<<20)
	out, e := f.run(context.Background(), map[string]any{"arg": nil})
	if out != nil || e == nil || e.Code != "EXECUTION_FAILED" {
		t.Fatal(out, e)
	}
	os.WriteFile(filepath.Join(f.dir, "reports", "missing.csv"), []byte("b\n"), 0600)
	out, e = f.run(context.Background(), map[string]any{"arg": "x"})
	if out != nil || e == nil || e.Code != "EXECUTION_FAILED" {
		t.Fatal(out, e)
	}
}

func TestCSVWhereAllOperatorsNestedArrayPresenceAndFilterBeforeLimit(t *testing.T) {
	for _, op := range []string{"eq", "ne", "lt", "lte", "gt", "gte", "in"} {
		t.Run(op, func(t *testing.T) {
			argSchema := `type: integer`
			arg := any(int64(2))
			if op == "in" {
				argSchema = `items: {type: integer}`
				arg = []any{int64(1), int64(3)}
			}
			body := fmt.Sprintf(`    executor: {type: csv, read: {source: reports, selector: {name: a.csv}}, csv: {columns: {amount: {type: integer, nullable: true, null_values: ["NULL"]}}}, where: [{column: amount, op: %s, input: /filters/0/value}]}
    input: {properties: {filters: {items: {properties: {value: {%s}}, required: [value]}, minItems: 1}}, required: [filters]}
    output: {data: {}}
    policy: {max_rows: 2}
`, op, argSchema)
			f := newCSVFixture(t, body, map[string][]byte{"a.csv": []byte("amount,unconfigured\nNULL,ignored\n1,00123\n2,\n3,other\n")}, 1<<20)
			out, e := f.run(context.Background(), map[string]any{"filters": []any{map[string]any{"value": arg}}})
			if e != nil {
				t.Fatal(e)
			}
			defer out.Close()
			want := map[string][]int64{"eq": {2}, "ne": {1, 3}, "lt": {1}, "lte": {1, 2}, "gt": {3}, "gte": {2, 3}, "in": {1, 3}}[op]
			rows := out.Data.(map[string]any)["rows"].([]any)
			if len(rows) != len(want) {
				t.Fatal(rows, want)
			}
			for i, row := range rows {
				if row.(map[string]any)["amount"] != want[i] {
					t.Fatal(rows, want)
				}
			}
		})
	}
	body := `    executor: {type: csv, read: {source: reports, selector: {name: a.csv}}, where: [{column: secret, op: eq, input: /arg}]}
    input: {properties: {arg: {type: string}}, required: [arg]}
    output: {items: {properties: {id: {type: string}}}, data: {}}
    policy: {max_rows: 1, max_output_bytes: 128}
`
	// Raw records and excluded columns can exceed max_output_bytes; the common
	// publisher measures the selected result, while scratch consumes shared quota.
	raw := "id,secret,large\nx,deny," + strings.Repeat("A", 32768) + "\ny,allow," + strings.Repeat("B", 32768) + "\n"
	f := newCSVFixture(t, body, map[string][]byte{"a.csv": []byte(raw)}, 1<<20)
	out, e := f.run(context.Background(), map[string]any{"arg": "allow"})
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	rows := out.Data.(map[string]any)["rows"].([]any)
	if len(rows) != 1 || len(rows[0].(map[string]any)) != 1 || rows[0].(map[string]any)["id"] != "y" {
		t.Fatal(rows)
	}
}
func TestCSVFilterInNullElementsRejectedBeforeSourceIO(t *testing.T) {
	body := `    executor: {type: csv, read: {source: reports, selector: {name: missing.csv}}, where: [{column: a, op: in, input: /arg}]}
    input: {properties: {arg: {type: [array, "null"], items: {type: [string, "null"]}}}, required: [arg]}
    output: {data: {}}
`
	f := newCSVFixture(t, body, nil, 1<<20)
	for _, arg := range []any{nil, []any{nil}} {
		out, e := f.run(context.Background(), map[string]any{"arg": arg})
		if out != nil || e == nil || e.Code != "EXECUTION_FAILED" {
			t.Fatal(out, e)
		}
	}
}

// Cancel after a valid prefix enters the parser, rather than before admission.
type csvCancelReader struct {
	cancel context.CancelFunc
	read   bool
}

func (r *csvCancelReader) Read(b []byte) (int, error) {
	if r.read {
		return 0, io.ErrUnexpectedEOF
	}
	r.read = true
	n := copy(b, "a,\"private")
	r.cancel()
	return n, nil
}
func TestCSVParserCancellationDuringRecord(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := csvRecords{chars: newCSVCharacters(ctx, &csvCancelReader{cancel: cancel}, "utf-8"), delimiter: ','}
	record, err := p.read()
	if record != nil || err != context.Canceled {
		t.Fatal(record, err)
	}
}

func TestCSVPublicationHoldsScratchUntilCanceledCallbackCleanup(t *testing.T) {
	raw := []byte("a\nok\n")
	f := newCSVFixture(t, csvFolder, map[string][]byte{"a.csv": raw}, 1<<20)
	lease, err := f.prepared.Acquire("csv")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), artifactStoreKey{}, f.store))
	defer cancel()
	calls := 0
	out, err, supported := lease.ExecutePublication(ctx, map[string]any{}, &providers.Effects{}, func(result contract.ProviderResult) (*contract.PreparedOutput, error) {
		calls++
		if f.store.Usage() != int64(len(raw)) || *result.ReturnedRows != 1 {
			t.Fatal(f.store.Usage(), result)
		}
		cancel()
		return nil, context.Canceled
	})
	var failure *providers.Failure
	if !supported || out != nil || !errors.As(err, &failure) || !errors.Is(failure.Cause, context.Canceled) || calls != 1 || f.store.Usage() != 0 {
		t.Fatal(supported, out, err, calls, f.store.Usage())
	}
}
func TestCSVOutputOmittedStillParsesAndMeasuresRows(t *testing.T) {
	body := strings.Replace(csvFolder, "    output: {data: {}}\n", "", 1)
	f := newCSVFixture(t, body, map[string][]byte{"a.csv": []byte("a\none\ntwo\n")}, 1<<20)
	out, err := f.run(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if out.Data != nil || len(out.Files) != 0 || out.ReturnedRows == nil || *out.ReturnedRows != 2 {
		t.Fatal(out)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "reports", "a.csv"), []byte("a\none\n\"broken"), 0600); err != nil {
		t.Fatal(err)
	}
	out, failure := f.run(context.Background(), map[string]any{})
	if out != nil || failure == nil || failure.Code != "EXECUTION_FAILED" {
		t.Fatal(out, failure)
	}
}

func TestCSVWhereConditionsANDAndCustomDelimiter(t *testing.T) {
	body := `    executor: {type: csv, read: {source: reports, selector: {name: a.csv}}, csv: {delimiter: ';', columns: {n: {type: integer}}}, where: [{column: group, op: eq, input: /group}, {column: n, op: gte, input: /minimum}]}
    input: {properties: {group: {type: string}, minimum: {type: integer}}, required: [group, minimum]}
    output: {data: {}}
`
	f := newCSVFixture(t, body, map[string][]byte{"a.csv": []byte("\ngroup;n\r\nkeep;1\r\nother;3\r\nkeep;2\r\n")}, 1<<20)
	out, failure := f.run(context.Background(), map[string]any{"group": "keep", "minimum": int64(2)})
	if failure != nil {
		t.Fatal(failure)
	}
	defer out.Close()
	rows := out.Data.(map[string]any)["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["n"] != int64(2) {
		t.Fatal(rows)
	}
}
