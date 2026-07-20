package agent

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingServiceManager struct {
	calls  *[]string
	status ServiceStatus
	err    error
}

func (m recordingServiceManager) Install() error {
	*m.calls = append(*m.calls, "install")
	return m.err
}
func (m recordingServiceManager) Start() error { *m.calls = append(*m.calls, "start"); return m.err }
func (m recordingServiceManager) Stop() error  { *m.calls = append(*m.calls, "stop"); return m.err }
func (m recordingServiceManager) Status() (ServiceStatus, error) {
	*m.calls = append(*m.calls, "status")
	return m.status, m.err
}
func (m recordingServiceManager) Uninstall() error {
	*m.calls = append(*m.calls, "uninstall")
	return m.err
}

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

func TestServiceCLIInvokesManagerLifecycleAndPrintsStatusContract(t *testing.T) {
	var calls []string
	var installedOpts ServiceOptions
	factory := func(opts ServiceOptions) serviceManager {
		if opts.ConfigPath != "" {
			installedOpts = opts
		}
		return recordingServiceManager{calls: &calls, status: ServiceStatus{
			Service: "onprest-agent", Native: "test-manager", Installed: true, State: "running", PID: "42",
		}}
	}
	config := filepath.Join(t.TempDir(), "capability.yaml")
	commands := [][]string{
		{"install", "--config", config},
		{"start"},
		{"status"},
		{"stop"},
		{"uninstall"},
	}
	var stdout, stderr bytes.Buffer
	for _, args := range commands {
		if code := handleServiceCLIWithFactory(args, &stdout, &stderr, factory); code != 0 {
			t.Fatalf("service %v code=%d stderr=%s", args, code, stderr.String())
		}
	}
	if strings.Join(calls, ",") != "install,start,status,stop,uninstall" {
		t.Fatalf("manager calls = %v", calls)
	}
	if installedOpts.ConfigPath != config || installedOpts.BinaryPath == "" || installedOpts.WorkDir != filepath.Dir(installedOpts.BinaryPath) {
		t.Fatalf("install options = %#v", installedOpts)
	}
	for _, want := range []string{"service: onprest-agent", "native: test-manager", "installed: true", "state: running", "pid: 42"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestServiceCLIReturnsManagerErrors(t *testing.T) {
	var calls []string
	factory := func(ServiceOptions) serviceManager {
		return recordingServiceManager{calls: &calls, err: errors.New("manager unavailable")}
	}
	for _, args := range [][]string{{"install", "--config", "capability.yaml"}, {"start"}, {"stop"}, {"status"}, {"uninstall"}} {
		var stdout, stderr bytes.Buffer
		if code := handleServiceCLIWithFactory(args, &stdout, &stderr, factory); code != 1 {
			t.Fatalf("service %v code=%d, want 1", args, code)
		}
		if !strings.Contains(stderr.String(), "manager unavailable") {
			t.Fatalf("service %v stderr=%q", args, stderr.String())
		}
	}
}
