//go:build linux

package agent

import (
	"strings"
	"testing"
)

func TestLinuxServiceUnitUsesPathSyntaxForWorkingDirectory(t *testing.T) {
	opts := ServiceOptions{
		Description: "Onprest Agent",
		WorkDir:     `/opt/Onprest Agent/%build`,
		BinaryPath:  `/opt/Onprest Agent/%build/onprest-agent`,
		ConfigPath:  `/opt/Onprest Agent/%build/capability.yaml`,
	}
	unit := linuxServiceUnit(opts)
	for _, want := range []string{
		"WorkingDirectory=/opt/Onprest Agent/%%build\n",
		`ExecStart="/opt/Onprest Agent/%%build/onprest-agent" --config "/opt/Onprest Agent/%%build/capability.yaml"`,
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("linuxServiceUnit() missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, `WorkingDirectory="`) {
		t.Fatalf("linuxServiceUnit() quoted WorkingDirectory path:\n%s", unit)
	}
}

func TestSystemdPathPreservesAbsolutePathSyntax(t *testing.T) {
	got := systemdPath(`/opt/Onprest Agent/%instance\data`)
	want := `/opt/Onprest Agent/%%instance\data`
	if got != want {
		t.Fatalf("systemdPath() = %q, want %q", got, want)
	}
}

func TestSystemdQuoteEscapesCommandArgumentAndSpecifierSyntax(t *testing.T) {
	got := systemdQuote(`/opt/Onprest Agent/%instance\data"file`)
	want := `"/opt/Onprest Agent/%%instance\\data\"file"`
	if got != want {
		t.Fatalf("systemdQuote() = %q, want %q", got, want)
	}
}
