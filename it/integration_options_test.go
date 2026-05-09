//go:build integration

package it

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"
)

var itDBFlag = flag.String("onprest-it-db", "postgres", "integration DB: postgres, mysql, sqlserver, oracle, or all")

func TestMain(m *testing.M) {
	code := m.Run()
	cleanupIntegrationDBs()
	os.Exit(code)
}

func selectedDBForTest(t *testing.T, driver string) bool {
	t.Helper()
	selected, ok := normalizeITDB(*itDBFlag)
	if !ok {
		t.Fatalf("invalid -onprest-it-db=%q; use postgres, mysql, sqlserver, oracle, or all", *itDBFlag)
	}
	driver, ok = normalizeITDB(driver)
	if !ok || driver == "all" {
		t.Fatalf("invalid integration DB driver %q", driver)
	}
	return selected == "all" || selected == driver
}

func normalizeITDB(v string) (string, bool) {
	switch v {
	case "", "postgres", "postgresql":
		return "postgres", true
	case "mysql", "sqlserver", "oracle", "all":
		return v, true
	default:
		return "", false
	}
}

func cleanupIntegrationDBs() {
	for _, fixture := range allDBFixtures() {
		if fixture.cleanup != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = fixture.cleanup(ctx)
			cancel()
		}
	}
}
