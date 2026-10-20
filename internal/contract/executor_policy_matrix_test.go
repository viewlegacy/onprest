package contract

import (
	"strings"
	"testing"
)

// CSV and HTTP complete the executor applicability matrix: CSV shares the
// SELECT row limit, while database write controls never reach CSV or HTTP.
func TestPolicyApplicabilityCSVAndHTTP(t *testing.T) {
	defs := "defaults: {readonly: false, max_rows: 7, max_affected_rows: 2, max_output_bytes: 2MiB}\n"
	csv := "executor: {type: csv, read: {source: read, selector: {name: sales.csv}}}\noutput: {data: {}}"
	http := "executor: {type: http, source: api, method: GET, path: /items, response: {kind: json}}\noutput: {data: {}}"
	c := compile(t, csv, defs)
	if c.Policy["max_rows"] != int64(7) || c.Policy["max_output_bytes"] != int64(2<<20) {
		t.Fatal("CSV defaults not inherited", c.Policy)
	}
	h := compile(t, http, defs)
	if h.Policy["max_output_bytes"] != int64(2<<20) {
		t.Fatal("HTTP output default not inherited", h.Policy)
	}
	for name, policy := range map[string]map[string]any{"csv": c.Policy, "http": h.Policy} {
		for _, k := range []string{"readonly", "max_affected_rows"} {
			if _, ok := policy[k]; ok {
				t.Fatalf("%s inherited non-applicable %s", name, k)
			}
		}
	}
	if _, ok := h.Policy["max_rows"]; ok {
		t.Fatal("HTTP inherited max_rows", h.Policy)
	}
	direct := compile(t, csv+"\npolicy: {max_rows: 3}", "")
	if direct.Policy["max_rows"] != int64(3) {
		t.Fatal("CSV direct max_rows", direct.Policy)
	}
	for cap, policies := range map[string][]string{csv: {"readonly: false", "max_affected_rows: 1"}, http: {"readonly: false", "max_affected_rows: 1", "max_rows: 1"}} {
		for _, p := range policies {
			_, e := Compile([]byte(fixture(cap+"\npolicy: {"+p+"}", "")))
			if e == nil || !strings.Contains(e.Error(), "does not apply") {
				t.Fatalf("%q accepted on %q: %v", p, cap, e)
			}
		}
	}
	// An unrelated default changes nothing for a capability it does not apply to.
	sameDefinition(t, compile(t, http, defs), compile(t, http, strings.Replace(defs, "max_rows: 7", "max_rows: 9", 1)))
}

// DB providers never receive file input; every file position is refused at
// startup with the specific reason instead of a generic binding error.
func TestDBFileInputRejectedForEveryFilePosition(t *testing.T) {
	for name, input := range map[string]string{
		"single":       "input: {properties: {document: {type: file}}}",
		"required":     "input: {properties: {document: {type: file}}, required: [document]}",
		"array":        "input: {properties: {documents: {items: {type: file}}}}",
		"nested":       "input: {properties: {order: {properties: {document: {type: file}}}}}",
		"nested_array": "input: {properties: {orders: {items: {properties: {document: {type: file}}}}}}",
	} {
		t.Run(name, func(t *testing.T) {
			_, e := Compile([]byte(fixture("executor: {type: db, source: db, sql: SELECT 1}\n"+input, "")))
			if e == nil || !strings.Contains(e.Error(), "DB file input is unsupported") {
				t.Fatalf("DB file input not refused explicitly: %v", e)
			}
		})
	}
	compile(t, "executor: {type: db, source: db, sql: 'SELECT :month::text AS month'}\ninput: {properties: {month: {type: string}}, required: [month]}", "")
}
