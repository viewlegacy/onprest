package agent

import (
	"encoding/json"
	"math"
	"testing"
)

func TestBuildSQLSkipsQuotedTextAndCasts(t *testing.T) {
	got, args, err := buildSQL("postgres", "select ':skip' as s, now()::date as d, id from customers where id = :id and note <> $$:skip$$", map[string]any{"id": int64(42)})
	if err != nil {
		t.Fatal(err)
	}
	want := "select ':skip' as s, now()::date as d, id from customers where id = $1 and note <> $$:skip$$"
	if got != want {
		t.Fatalf("sql mismatch\nwant: %s\n got: %s", want, got)
	}
	if len(args) != 1 || args[0] != int64(42) {
		t.Fatalf("args mismatch: %#v", args)
	}
}

func TestBuildSQLServerPlaceholders(t *testing.T) {
	got, args, err := buildSQL("sqlserver", "select * from customers where id = :id and status = :status", map[string]any{"id": int64(1), "status": "active"})
	if err != nil {
		t.Fatal(err)
	}
	want := "select * from customers where id = @p1 and status = @p2"
	if got != want {
		t.Fatalf("sql mismatch\nwant: %s\n got: %s", want, got)
	}
	if len(args) != 2 {
		t.Fatalf("args mismatch: %#v", args)
	}
}

func TestBuildSQLQuestionMarkPlaceholders(t *testing.T) {
	got, args, err := buildSQL("mysql", "select * from customers where id = :id and status = :status", map[string]any{"id": int64(1), "status": "active"})
	if err != nil {
		t.Fatal(err)
	}
	want := "select * from customers where id = ? and status = ?"
	if got != want {
		t.Fatalf("sql mismatch\nwant: %s\n got: %s", want, got)
	}
	if len(args) != 2 || args[0] != int64(1) || args[1] != "active" {
		t.Fatalf("args mismatch: %#v", args)
	}
}

func TestBuildSQLOraclePlaceholders(t *testing.T) {
	got, args, err := buildSQL("oracle", "select * from customers where id = :id and status = :status", map[string]any{"id": int64(1), "status": "active"})
	if err != nil {
		t.Fatal(err)
	}
	want := "select * from customers where id = :1 and status = :2"
	if got != want {
		t.Fatalf("sql mismatch\nwant: %s\n got: %s", want, got)
	}
	if len(args) != 2 || args[0] != int64(1) || args[1] != "active" {
		t.Fatalf("args mismatch: %#v", args)
	}
}

func TestBuildSQLUsesDialectAwareProtectedRegions(t *testing.T) {
	tests := []struct {
		name, driver, query, want string
	}{
		{
			name:   "postgres escape dollar and nested comment",
			driver: "postgres",
			query:  `select E'escaped \':id', $$:id$$, /* outer /* :id */ :id */ :id, :second::text`,
			want:   `select E'escaped \':id', $$:id$$, /* outer /* :id */ :id */ $1, $2::text`,
		},
		{
			name:   "mysql backtick",
			driver: "mysql",
			query:  "select `column:name`, ':id', /* :id */ :id, :second",
			want:   "select `column:name`, ':id', /* :id */ ?, ?",
		},
		{
			name:   "sqlserver bracket and nested comment",
			driver: "sqlserver",
			query:  "select [column:name], /* outer /* :id */ :id */ :id, :second",
			want:   "select [column:name], /* outer /* :id */ :id */ @p1, @p2",
		},
		{
			name:   "oracle alternative quotes",
			driver: "oracle",
			query:  "select q'[content :id]', q'{more :id}', :id, :second from dual",
			want:   "select q'[content :id]', q'{more :id}', :1, :2 from dual",
		},
	}
	params := map[string]any{"id": int64(7), "second": "next"}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, args, err := buildSQL(tc.driver, tc.query, params)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("sql mismatch\nwant: %s\n got: %s", tc.want, got)
			}
			if len(args) != 2 || args[0] != int64(7) || args[1] != "next" {
				t.Fatalf("args=%#v", args)
			}
		})
	}
}

func TestBuildSQLRejectsMissingParam(t *testing.T) {
	if _, _, err := buildSQL("postgres", "select * from customers where id = :id", map[string]any{}); err == nil {
		t.Fatal("buildSQL() error = nil, want missing param error")
	}
}

func TestBuildExplainSQL(t *testing.T) {
	tests := []struct {
		driver string
		want   string
	}{
		{driver: "postgres", want: "EXPLAIN select 1"},
		{driver: "mysql", want: "EXPLAIN select 1"},
		{driver: "oracle", want: "EXPLAIN PLAN FOR select 1"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			got, ok := buildExplainSQL(tc.driver, "select 1")
			if !ok {
				t.Fatal("buildExplainSQL() ok = false, want true")
			}
			if got != tc.want {
				t.Fatalf("buildExplainSQL() = %q, want %q", got, tc.want)
			}
		})
	}
	if _, ok := buildExplainSQL("sqlserver", "select 1"); ok {
		t.Fatal("buildExplainSQL(sqlserver) unexpectedly returned unreachable SHOWPLAN SQL")
	}
}

func TestMaxBytesParsesAndRejectsInvalidValues(t *testing.T) {
	got, err := maxBytes(PolicyDef{MaxBytes: "256KB"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 256<<10 {
		t.Fatalf("maxBytes() = %d, want %d", got, 256<<10)
	}
	for _, invalid := range []string{"0B", "-1KB", "bad", "10xyz", "1.5MB", "9223372036854775807GB", "MB"} {
		if _, err := maxBytes(PolicyDef{MaxBytes: invalid}); err == nil {
			t.Fatalf("maxBytes(%q) error = nil, want error", invalid)
		}
	}
}

func TestResultNumberAndIntegerConversionsRejectNonFiniteAndOverflow(t *testing.T) {
	for _, value := range []any{math.NaN(), math.Inf(1), math.Inf(-1), float64(9223372036854775808.0), uint64(math.MaxInt64) + 1} {
		if _, err := coerceInteger(value); err == nil {
			t.Fatalf("coerceInteger(%v) error = nil", value)
		}
	}
	if got, err := coerceInteger(float64(-9223372036854775808.0)); err != nil || got != int64(math.MinInt64) {
		t.Fatalf("MinInt64 conversion = %#v, %v", got, err)
	}
	for _, value := range []any{math.NaN(), math.Inf(1), math.Inf(-1), json.Number("1e10000")} {
		if _, err := coerceNumber(value); err == nil {
			t.Fatalf("coerceNumber(%v) error = nil", value)
		}
	}
}
