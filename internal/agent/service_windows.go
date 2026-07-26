//go:build windows

package agent

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type platformServiceManager struct {
	opts ServiceOptions
}

func newServiceManager(opts ServiceOptions) serviceManager {
	return platformServiceManager{opts: opts}
}

func (m platformServiceManager) Install() error {
	binPath := fmt.Sprintf("%s --config %s", windowsQuote(m.opts.BinaryPath), windowsQuote(m.opts.ConfigPath))
	if err := runCommand("sc.exe", "create", defaultServiceName, "binPath=", binPath, "start=", "demand", "DisplayName=", defaultServiceDisplayName); err != nil {
		return err
	}
	_ = runCommand("sc.exe", "description", defaultServiceName, defaultServiceDescription)
	return nil
}

func (m platformServiceManager) Start() error {
	return runCommand("sc.exe", "start", defaultServiceName)
}

func (m platformServiceManager) Stop() error {
	return runCommand("sc.exe", "stop", defaultServiceName)
}

func (m platformServiceManager) Status() (ServiceStatus, error) {
	status := ServiceStatus{
		Service: defaultServiceName,
		Native:  "windows-service",
		State:   "not-installed",
	}
	out, err := exec.Command("sc.exe", "query", defaultServiceName).CombinedOutput()
	text := string(out)
	if err != nil {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "does not exist") || strings.Contains(lower, "marked for deletion") {
			return status, nil
		}
		return ServiceStatus{}, fmt.Errorf("sc.exe query %s: %w: %s", defaultServiceName, err, strings.TrimSpace(text))
	}
	status.Installed = true
	status.State = windowsServiceState(text)
	if cfg, err := exec.Command("sc.exe", "qc", defaultServiceName).CombinedOutput(); err == nil {
		status.Binary, status.Config = parseWindowsBinPath(string(cfg))
	}
	return status, nil
}

func (m platformServiceManager) Uninstall() error {
	_ = m.Stop()
	if err := runCommand("sc.exe", "delete", defaultServiceName); err != nil {
		return err
	}
	for i := 0; i < 40; i++ {
		out, err := exec.Command("sc.exe", "query", defaultServiceName).CombinedOutput()
		if err != nil {
			lower := strings.ToLower(string(out))
			if strings.Contains(lower, "does not exist") || strings.Contains(lower, "marked for deletion") {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("service %s was not deleted within 10s", defaultServiceName)
}

func windowsServiceState(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "STATE") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				return strings.ToLower(fields[len(fields)-1])
			}
		}
	}
	return "unknown"
}

func parseWindowsBinPath(text string) (string, string) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "BINARY_PATH_NAME") {
			continue
		}
		_, value, ok := strings.Cut(line, ":")
		if !ok {
			return "", ""
		}
		value = strings.TrimSpace(value)
		parts := strings.Split(value, " --config ")
		if len(parts) != 2 {
			return strings.Trim(parts[0], `"`), ""
		}
		return strings.Trim(parts[0], `"`), strings.Trim(parts[1], `"`)
	}
	return "", ""
}

func windowsQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func runCommand(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		if len(out) > 0 {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
