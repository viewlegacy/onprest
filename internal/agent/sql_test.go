package agent

import "testing"

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
		{driver: "sqlserver", want: "SET SHOWPLAN_TEXT ON; select 1; SET SHOWPLAN_TEXT OFF;"},
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
}

func TestMaxBytesParsesAndRejectsInvalidValues(t *testing.T) {
	got, err := maxBytes(PolicyDef{MaxBytes: "256KB"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 256<<10 {
		t.Fatalf("maxBytes() = %d, want %d", got, 256<<10)
	}
	if _, err := maxBytes(PolicyDef{MaxBytes: "0B"}); err == nil {
		t.Fatal("maxBytes() error = nil, want error")
	}
}
