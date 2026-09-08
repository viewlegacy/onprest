package buildinfo

import "testing"

func TestCurrentDefaultsToDev(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })
	for _, value := range []string{"", "  ", "\t\n"} {
		Version = value
		if got := Current(); got != "dev" {
			t.Fatalf("Current(%q)=%q, want dev", value, got)
		}
	}
}

func TestCurrentReturnsTrimmedInjectedVersion(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })
	Version = " 1.2.4 "
	if got := Current(); got != "1.2.4" {
		t.Fatalf("Current()=%q, want 1.2.4", got)
	}
}
