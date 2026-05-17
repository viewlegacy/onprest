package agent

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigUsesCLIConfigBeforeEnv(t *testing.T) {
	cfg, err := loadConfig([]string{"--config", "cli.yaml"}, func(key string) string {
		if key == "AGENT_CAPABILITY_FILE" {
			return "env.yaml"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CapabilityFile != "cli.yaml" {
		t.Fatalf("CapabilityFile = %q, want cli.yaml", cfg.CapabilityFile)
	}
	if cfg.ReconnectEvery != 30*time.Second {
		t.Fatalf("ReconnectEvery = %s, want 30s", cfg.ReconnectEvery)
	}
}

func TestLoadConfigFallsBackToEnvThenDefault(t *testing.T) {
	cfg, err := loadConfig(nil, func(key string) string {
		if key == "AGENT_CAPABILITY_FILE" {
			return "env.yaml"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CapabilityFile != "env.yaml" {
		t.Fatalf("CapabilityFile = %q, want env.yaml", cfg.CapabilityFile)
	}

	cfg, err = loadConfig(nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cfg.CapabilityFile) != "capability.yaml" {
		t.Fatalf("CapabilityFile = %q, want basename capability.yaml", cfg.CapabilityFile)
	}
	if !filepath.IsAbs(cfg.CapabilityFile) {
		t.Fatalf("CapabilityFile = %q, want absolute executable-adjacent path", cfg.CapabilityFile)
	}
}

func TestLoadConfigRejectsUnexpectedArgs(t *testing.T) {
	if _, err := loadConfig([]string{"unexpected"}, func(string) string { return "" }); err == nil {
		t.Fatal("loadConfig accepted unexpected arg")
	}
}

func TestAgentUsageMentionsServiceCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code := HandleCLI([]string{"--help"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("HandleCLI handled=%t code=%d stderr=%q", handled, code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"onprest-agent [--config PATH]", "service install [--config PATH]", "service uninstall"} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage missing %q:\n%s", want, out)
		}
	}
}

func TestDefaultServiceOptionsUsesExecutableAdjacentCapabilityByDefault(t *testing.T) {
	opts, err := defaultServiceOptions("")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(opts.ConfigPath) != "capability.yaml" {
		t.Fatalf("ConfigPath = %q, want capability.yaml basename", opts.ConfigPath)
	}
	if !filepath.IsAbs(opts.ConfigPath) {
		t.Fatalf("ConfigPath = %q, want absolute path", opts.ConfigPath)
	}
	if opts.WorkDir != filepath.Dir(opts.BinaryPath) {
		t.Fatalf("WorkDir = %q, want binary dir %q", opts.WorkDir, filepath.Dir(opts.BinaryPath))
	}
}
