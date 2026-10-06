// Package testschema validates public OpenAPI with the pinned document validator
// and public response JSON Schemas with the existing pinned ECMAScript Ajv gate.
package testschema

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"testing"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
)

type TestingT interface {
	Helper()
	Fatal(...any)
	Fatalf(string, ...any)
}
type Document struct {
	validator validator.Validator
	raw       map[string]any
}

func OpenAPI(t *testing.T, body []byte) *Document {
	t.Helper()
	doc, e := libopenapi.NewDocument(body)
	if e != nil {
		t.Fatal(e)
	}
	v, es := validator.NewValidator(doc)
	if len(es) > 0 {
		t.Fatal(es)
	}
	if ok, es := v.ValidateDocument(); !ok {
		t.Fatal("published OpenAPI invalid", es)
	}
	var raw map[string]any
	if e = json.Unmarshal(body, &raw); e != nil {
		t.Fatal(e)
	}
	return &Document{v, raw}
}
func HTTPResponse(t *testing.T, v *Document, url string, header http.Header, body []byte) {
	t.Helper()
	request, e := http.NewRequest("POST", url, bytes.NewReader([]byte("{}")))
	if e != nil {
		t.Fatal(e)
	}
	request.Header.Set("Content-Type", "application/json")
	response := &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(bytes.NewReader(body))}
	media, _, e := mime.ParseMediaType(header.Get("Content-Type"))
	if e != nil {
		t.Fatal(e)
	}
	operation := v.raw["paths"].(map[string]any)[request.URL.Path].(map[string]any)["post"].(map[string]any)
	schema := operation["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)[media].(map[string]any)["schema"]
	if media == "application/json" {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if e = decoder.Decode(&value); e != nil {
			t.Fatal(e)
		}
		JSON(t, schema, value, true)
	} else {
		if ok, es := v.validator.ValidateHttpResponse(request, response); !ok {
			t.Fatalf("published direct HTTP response does not match OpenAPI: %v", es)
		}
		JSON(t, schema, string(body), true)
	}
}
func JSON(t *testing.T, schema, value any, want bool) {
	t.Helper()
	validateJSON(t, schema, value, want)
}
