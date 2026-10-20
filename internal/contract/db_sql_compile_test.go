package contract

import (
	"fmt"
	"strings"
	"testing"
)

// dbCompile compiles one DB capability for driver through the production
// contract.Compile path (the same compiler the running agent uses).
func dbCompile(driver, sql, policy string) error {
	header := strings.Replace(configHeader, "db: {driver: postgres,", "db: {driver: "+driver+",", 1)
	cap := "executor: {type: db, source: db, sql: " + yamlQuote(sql) + "}"
	if policy != "" {
		cap += "\npolicy: {" + policy + "}"
	}
	_, e := Compile([]byte(header + "capabilities:\n  test:\n" + indent(cap, 4)))
	return e
}
func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// The readonly policy and the statement classes are decided by the production
// compiler: SELECT runs with any readonly value, mutations need readonly:false,
// and statements outside single SELECT/INSERT/UPDATE/DELETE are refused.
func TestDBStatementClassAndReadonlyThroughCompile(t *testing.T) {
	drivers := []string{"postgres", "mysql", "sqlserver", "oracle"}
	statements := map[string]string{
		"SELECT": "SELECT id FROM customers",
		"INSERT": "INSERT INTO customers (id) VALUES (1)",
		"UPDATE": "UPDATE customers SET id = 1",
		"DELETE": "DELETE FROM customers WHERE id = 1",
	}
	for _, driver := range drivers {
		for op, sql := range statements {
			for _, readonly := range []string{"", "readonly: true", "readonly: false"} {
				t.Run(fmt.Sprintf("%s/%s/%q", driver, op, readonly), func(t *testing.T) {
					e := dbCompile(driver, sql, readonly)
					wantOK := op == "SELECT" || readonly == "readonly: false"
					if (e == nil) != wantOK {
						t.Fatalf("accepted=%v want %v: %v", e == nil, wantOK, e)
					}
					if !wantOK && !strings.Contains(e.Error(), "readonly:false") {
						t.Fatalf("mutation refused for another reason: %v", e)
					}
				})
			}
		}
	}
}

func TestDBStatementShapesThroughCompile(t *testing.T) {
	refused := map[string][]string{
		"multiple statements": {"SELECT 1; SELECT 2", "UPDATE customers SET id = 1; DELETE FROM customers", "SELECT 1;DELETE FROM customers"},
		"WITH":                {"WITH x AS (SELECT 1 AS id) SELECT id FROM x", "WITH x AS (SELECT 1 AS id) DELETE FROM customers"},
		"MERGE":               {"MERGE INTO customers c USING other o ON (c.id = o.id) WHEN MATCHED THEN UPDATE SET c.id = o.id"},
		"DDL":                 {"CREATE TABLE t (id INT)", "DROP TABLE customers", "ALTER TABLE customers ADD c INT", "TRUNCATE TABLE customers"},
	}
	for _, driver := range []string{"postgres", "mysql", "sqlserver", "oracle"} {
		for class, list := range refused {
			for _, sql := range list {
				t.Run(driver+"/"+class+"/"+sql, func(t *testing.T) {
					if e := dbCompile(driver, sql, "readonly: false"); e == nil || !strings.Contains(e.Error(), "supported single SELECT/INSERT/UPDATE/DELETE") {
						t.Fatal("not refused as an unsupported statement:", e)
					}
				})
			}
		}
		// Statement separators, keywords and comment markers inside quoted
		// literals and comments do not change the single statement's class.
		for _, sql := range []string{"SELECT ';DELETE FROM customers' AS x", "SELECT 1 -- ; DELETE FROM customers", "SELECT 1 /* ; DELETE FROM customers */"} {
			t.Run(driver+"/inert/"+sql, func(t *testing.T) {
				if e := dbCompile(driver, sql, ""); e != nil {
					t.Fatal(e)
				}
			})
		}
		t.Run(driver+"/inert mutation", func(t *testing.T) {
			if e := dbCompile(driver, "SELECT 'INSERT INTO x' AS v", ""); e != nil {
				t.Fatal("quoted mutation keyword made a SELECT a mutation", e)
			}
			if e := dbCompile(driver, "UPDATE customers SET id = 1 /* SELECT */", ""); e == nil || !strings.Contains(e.Error(), "readonly:false") {
				t.Fatal("comment made a mutation read-only")
			}
		})
	}
	// Row-returning DML is outside the supported shapes.
	for driver, sql := range map[string]string{
		"postgres":  "UPDATE customers SET id = 1 RETURNING id",
		"oracle":    "UPDATE customers SET id = 1 RETURNING id INTO :id",
		"sqlserver": "UPDATE customers SET id = 1 OUTPUT inserted.id",
	} {
		t.Run(driver+"/returning", func(t *testing.T) {
			e := dbCompile(driver, sql, "readonly: false")
			if driver == "oracle" {
				e = func() error {
					header := strings.Replace(configHeader, "db: {driver: postgres,", "db: {driver: oracle,", 1)
					_, err := Compile([]byte(header + "capabilities:\n  test:\n" + indent("executor: {type: db, source: db, sql: "+yamlQuote(sql)+"}\ninput: {properties: {id: {type: integer}}, required: [id]}\npolicy: {readonly: false}", 4)))
					return err
				}()
			}
			if e == nil || !strings.Contains(e.Error(), "without row-returning DML") {
				t.Fatal("row-returning DML not refused:", e)
			}
		})
	}
}

// Dialect quoting and comments protect their contents from statement and
// parameter recognition: a quoted ';' or ':name' is text, not a separator or a
// placeholder, while the outer ':id' is still a bound parameter.
func TestDBDialectProtectedRegionsThroughCompile(t *testing.T) {
	cases := map[string][]string{
		"postgres":  {"SELECT E'\\';DELETE :other' AS x, :id AS id", "SELECT $$;DELETE :other$$ AS x, :id AS id", "SELECT $q$;DELETE :other$q$ AS x, :id AS id", "SELECT 1 AS x /* outer /* ; :other */ ; DELETE */, :id AS id"},
		"mysql":     {"SELECT `;DELETE :other` FROM t WHERE id = :id", "SELECT ';DELETE :other' AS x, :id AS id"},
		"sqlserver": {"SELECT [;DELETE :other] FROM t WHERE id = :id", "SELECT 1 AS x /* outer /* ; :other */ ; DELETE */, :id AS id"},
		"oracle":    {"SELECT q'[;DELETE :other]' AS x, :id AS id FROM dual", "SELECT ';DELETE :other' AS x, :id AS id FROM dual"},
	}
	for driver, list := range cases {
		for _, sql := range list {
			t.Run(driver+"/"+sql, func(t *testing.T) {
				header := strings.Replace(configHeader, "db: {driver: postgres,", "db: {driver: "+driver+",", 1)
				cap := "executor: {type: db, source: db, sql: " + yamlQuote(sql) + "}\ninput: {properties: {id: {type: integer}}, required: [id]}"
				if _, e := Compile([]byte(header + "capabilities:\n  test:\n" + indent(cap, 4))); e != nil {
					t.Fatal(e)
				}
			})
		}
	}
	for _, driver := range []string{"postgres", "mysql", "sqlserver", "oracle"} {
		t.Run(driver+"/CALL", func(t *testing.T) {
			if e := dbCompile(driver, "CALL refresh_customers()", "readonly: false"); e == nil || !strings.Contains(e.Error(), "supported single SELECT/INSERT/UPDATE/DELETE") {
				t.Fatal("CALL not refused:", e)
			}
		})
	}
	// Nested block comments are trivia in PostgreSQL and SQL Server; a statement
	// separator after the outer comment still splits the text.
	for _, driver := range []string{"postgres", "sqlserver"} {
		t.Run(driver+"/nested comment cannot hide a second statement", func(t *testing.T) {
			if e := dbCompile(driver, "SELECT 1 /* outer /* inner */ still comment */; DELETE FROM customers", "readonly: false"); e == nil {
				t.Fatal("second statement after a nested comment accepted")
			}
		})
	}
}
