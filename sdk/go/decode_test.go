package onprest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"strings"
	"testing"
)

type embeddedA struct {
	Value int
}
type embeddedB struct {
	Value int
}
type definedString string
type customInteger int32

func (i *customInteger) UnmarshalJSON(b []byte) error {
	var n int32
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*i = customInteger(n + 1)
	return nil
}

type textValue struct{ Text string }

func (v *textValue) UnmarshalText(b []byte) error { v.Text = string(b); return nil }

type customJSON struct{ Raw string }

func (v *customJSON) UnmarshalJSON(b []byte) error { v.Raw = string(b); return nil }
func testExecution() *execution {
	e := &execution{alive: true, owned: map[*owned]bool{}}
	e.ctx = context.WithValue(context.Background(), sessionKey{}, e)
	return e
}
func TestTypedDecodeExactFieldsPresenceAndNumbers(t *testing.T) {
	type Input struct {
		embeddedA
		Name    definedString       `json:"name,omitempty"`
		Ignore  int                 `json:"-"`
		I32     int32               `json:"i32"`
		I64     int64               `json:"i64"`
		U       uint64              `json:"u"`
		Pointer *int64              `json:"pointer"`
		Raw     json.RawMessage     `json:"raw"`
		Any     any                 `json:"any"`
		Nested  map[string][]*int64 `json:"nested"`
		Custom  customInteger       `json:"custom"`
		Text    textValue           `json:"text"`
		JSON    customJSON          `json:"json"`
	}
	v, err := decode[Input]([]byte(`{"name":"value","Value":4,"i32":-2147483648,"i64":9007199254740993,"u":18446744073709551615,"pointer":0,"raw":null,"any":[9007199254740993,null],"nested":{"a":[0,null]},"custom":4,"text":"decoded","json":{"a":1}}`), testExecution())
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "value" || v.Value != 4 || v.I64 != 9007199254740993 || v.U != 18446744073709551615 || v.Pointer == nil || *v.Pointer != 0 || string(v.Raw) != "null" || v.Any.([]any)[0] != json.Number("9007199254740993") || v.Nested["a"][0] == nil || v.Nested["a"][1] != nil || v.Custom != 5 || v.Text.Text != "decoded" || v.JSON.Raw != `{"a":1}` {
		t.Fatalf("%+v", v)
	}
	absent, err := decode[Input]([]byte(`{}`), testExecution())
	if err != nil || absent.Pointer != nil || absent.Raw != nil {
		t.Fatal(absent, err)
	}
	for _, raw := range []string{`{"Name":"bad"}`, `{"Ignore":1}`, `{"i32":2147483648}`, `{"i64":9223372036854775808}`, `{"u":-1}`, `{"u":18446744073709551616}`, `{"i32":1.5}`, `{"name":null}`, `{"i32":null}`, `{"custom":2147483648}`, `{"nested":{"a":["1"]}}`, `{"text":{}}`, `{} {}`} {
		if _, err := decode[Input]([]byte(raw), testExecution()); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestTypedDecodeAmbiguityUnsupportedAndCustomFile(t *testing.T) {
	type Ambiguous struct {
		embeddedA
		embeddedB
	}
	if _, err := decode[Ambiguous]([]byte(`{"Value":1}`), testExecution()); err == nil {
		t.Fatal("ambiguous field accepted")
	}
	type Unsupported struct {
		A [1]int  `json:"a"`
		S int     `json:"s,string"`
		F float32 `json:"f"`
	}
	for _, raw := range []string{`{"a":[1]}`, `{"s":"1"}`, `{"s":1}`, `{"f":1e100}`} {
		if _, err := decode[Unsupported]([]byte(raw), testExecution()); err == nil {
			t.Fatal(raw)
		}
	}
	type Input struct {
		Raw    json.RawMessage       `json:"raw"`
		Any    any                   `json:"any"`
		Custom customJSON            `json:"custom"`
		Files  []InputFile           `json:"files"`
		Nested map[string]*InputFile `json:"nested"`
	}
	descriptor := `{"__onprest_input_file_v1":{"file_id":"secret","name":"a.bin","mime_type":"application/octet-stream","size_bytes":0,"sha256":"` + strings.Repeat("a", 64) + `"}}`
	for _, field := range []string{"raw", "any", "custom"} {
		_, err := decode[Input]([]byte(`{"`+field+`":`+descriptor+`}`), testExecution())
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal(field, err)
		}
	}
	v, err := decode[Input]([]byte(`{"files":[`+descriptor+`],"nested":{"x":`+descriptor+`}}`), testExecution())
	if err != nil || v.Files[0].Name() != "a.bin" || v.Nested["x"].Size() != 0 {
		t.Fatal(v, err)
	}
	if _, err := json.Marshal(v.Files); err == nil {
		t.Fatal("InputFile serialized")
	}
	if _, err := decode[int]([]byte(`{}`), testExecution()); err == nil {
		t.Fatal("scalar root")
	}
	if _, err := decode[map[int]string]([]byte(`{}`), testExecution()); err == nil {
		t.Fatal("nonstring map root")
	}
}
func TestErrorsOwnershipContextAndPublicValidation(t *testing.T) {
	for _, test := range []struct {
		code int64
		want error
	}{{-1, fs.ErrInvalid}, {-2, fs.ErrPermission}, {-3, fs.ErrNotExist}, {-4, ErrLimitExceeded}, {-5, context.Canceled}, {-7, errors.ErrUnsupported}, {-8, fs.ErrExist}} {
		wrapped := &fs.PathError{Op: "open", Path: "private", Err: hostError(test.code, context.Background())}
		if !errors.Is(fmt.Errorf("outer: %w", wrapped), test.want) {
			t.Fatal(test)
		}
	}
	if errors.Is(hostError(-6, context.Background()), fs.ErrNotExist) {
		t.Fatal("I/O reclassified")
	}
	for _, test := range []struct {
		code, message string
		valid         bool
	}{{"APP_A", "x", true}, {"APP_" + strings.Repeat("A", 60), strings.Repeat("a", 1024), true}, {"APP_" + strings.Repeat("A", 61), "x", false}, {"APP_1", "x", false}, {"LIMIT_EXCEEDED", "x", false}, {"APP_A", "", false}, {"APP_A", "bad\n", false}, {"APP_A", string([]byte{255}), false}} {
		if validPublic(PublicError(test.code, test.message).(*publicError)) != test.valid {
			t.Fatal(test)
		}
	}
	var p *publicError
	if !errors.As(fmt.Errorf("wrapped: %w", PublicError("APP_A", "x")), &p) {
		t.Fatal("public wrapping")
	}
	e := testExecution()
	if e.check(context.Background()) == nil {
		t.Fatal("outside context")
	}
	e.alive = false
	if !errors.Is(e.check(e.ctx), fs.ErrClosed) {
		t.Fatal("ended context")
	}
	var f InputFile
	if _, err := f.Open(context.Background()); err == nil {
		t.Fatal("zero file opened")
	}
	var r *bytes.Reader
	if !nilReader(r) || nilReader(bytes.NewReader(nil)) {
		t.Fatal("typed nil reader")
	}
}

type memoryHost struct {
	next         int64
	controls     map[int64]*bytes.Reader
	bodies       map[int64]*bytes.Buffer
	terminal     map[string]any
	resultHandle int64
}

func newMemoryHost() *memoryHost {
	return &memoryHost{next: 1, controls: map[int64]*bytes.Reader{}, bodies: map[int64]*bytes.Buffer{}}
}
func (h *memoryHost) id() int64 { h.next++; return h.next }
func (h *memoryHost) call(b []byte) int64 {
	var q struct {
		Method string         `json:"method"`
		Args   map[string]any `json:"args"`
	}
	if json.Unmarshal(b, &q) != nil {
		return -1
	}
	v := map[string]int64{}
	switch q.Method {
	case "result.file", "result.open":
		body := h.id()
		h.bodies[body] = &bytes.Buffer{}
		marker := h.id()
		v["body_handle"] = body
		if q.Method == "result.open" {
			h.resultHandle = body
			v["result_handle"] = marker
		} else {
			v["file_handle"] = marker
		}
	case "result.commit":
		if json.Unmarshal(h.bodies[h.resultHandle].Bytes(), &h.terminal) != nil {
			return -1
		}
	case "log.write":
	default:
		return -7
	}
	raw, _ := json.Marshal(v)
	id := h.id()
	h.controls[id] = bytes.NewReader(raw)
	return id
}
func (h *memoryHost) read(id int64, b []byte) int32 {
	r := h.controls[id]
	if r == nil {
		return -1
	}
	n, _ := r.Read(b)
	return int32(n)
}
func (h *memoryHost) write(id int64, b []byte) int32 {
	r := h.bodies[id]
	if r == nil {
		return -1
	}
	n, _ := r.Write(b)
	return int32(n)
}
func (h *memoryHost) close(id int64) int32 { delete(h.controls, id); return 0 }

type closerReader struct {
	closeCount            int
	readError, closeError error
	Reader                io.Reader
}

func (r *closerReader) Read(b []byte) (int, error) {
	if r.readError != nil {
		return 0, r.readError
	}
	return r.Reader.Read(b)
}
func (r *closerReader) Close() error { r.closeCount++; return r.closeError }
func TestRunTerminalAndReturnedReaderOwnership(t *testing.T) {
	for _, test := range []struct {
		name                          string
		handlerErr, readErr, closeErr error
		want                          string
	}{{"success", nil, nil, nil, ""}, {"read", nil, errors.New("private read"), nil, "output"}, {"close", nil, nil, errors.New("private close"), "output"}, {"limit", nil, ErrLimitExceeded, nil, "limit"}, {"primary", PublicError("APP_PRIMARY", "primary"), nil, ErrLimitExceeded, "APP_PRIMARY"}, {"joined", errors.Join(PublicError("APP_PRIMARY", "primary"), ErrLimitExceeded), nil, nil, "limit"}} {
		t.Run(test.name, func(t *testing.T) {
			h := newMemoryHost()
			r := &closerReader{Reader: strings.NewReader("value"), readError: test.readErr, closeError: test.closeErr}
			var diagnostic bytes.Buffer
			run[map[string]any](strings.NewReader(`{}`), &diagnostic, h, func(context.Context, map[string]any) (Result, error) {
				return Result{Data: map[string]any{"n": 1}, Files: []File{{"a.bin", "application/octet-stream", r}}}, test.handlerErr
			})
			if r.closeCount != 1 {
				t.Fatal(r.closeCount)
			}
			err, has := h.terminal["error"].(map[string]any)
			if test.want == "" {
				if has || h.terminal["data"] == nil {
					t.Fatal(h.terminal)
				}
			} else if test.want == "APP_PRIMARY" {
				if err["code"] != test.want {
					t.Fatal(err)
				}
			} else if err["category"] != test.want {
				t.Fatal(h.terminal)
			}
			raw, _ := json.Marshal(h.terminal)
			if bytes.Contains(raw, []byte("private")) {
				t.Fatal(string(raw))
			}
		})
	}
	h := newMemoryHost()
	called := false
	run[struct {
		A int `json:"a"`
	}](strings.NewReader(`{"a":null}`), io.Discard, h, func(context.Context, struct {
		A int `json:"a"`
	}) (Result, error) {
		called = true
		return Result{}, nil
	})
	if called || !reflect.DeepEqual(h.terminal["error"], map[string]any{"kind": "internal", "category": "execution"}) {
		t.Fatal(called, h.terminal)
	}
}

func TestReadDirPositiveControlErrors(t *testing.T) {
	for _, test := range []struct {
		v    any
		want error
	}{{json.Number("0"), nil}, {json.Number("1"), fs.ErrInvalid}, {json.Number("2"), fs.ErrPermission}, {json.Number("3"), fs.ErrNotExist}, {json.Number("4"), ErrLimitExceeded}, {json.Number("5"), context.Canceled}, {json.Number("7"), errors.ErrUnsupported}, {json.Number("8"), fs.ErrExist}} {
		err := directoryControlError(test.v, context.Background())
		if test.want == nil {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, test.want) {
			t.Fatal(test, err)
		}
	}
	for _, v := range []any{json.Number("6"), json.Number("-1"), json.Number("9"), json.Number("999999999999999999999999"), "4", nil} {
		err := directoryControlError(v, context.Background())
		if err == nil || errors.Is(err, ErrLimitExceeded) || errors.Is(err, fs.ErrInvalid) {
			t.Fatal(v, err)
		}
	}
}
