//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const linuxServicePath = "/etc/systemd/system/onprest-agent.service"

type platformServiceManager struct {
	opts ServiceOptions
}

func newServiceManager(opts ServiceOptions) serviceManager {
	return platformServiceManager{opts: opts}
}

func (m platformServiceManager) Install() error {
	if err := os.WriteFile(linuxServicePath, []byte(linuxServiceUnit(m.opts)), 0o644); err != nil {
		return err
	}
	return runCommand("systemctl", "daemon-reload")
}

func linuxServiceUnit(opts ServiceOptions) string {
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s --config %s
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target

# OnprestConfig=%s
# OnprestBinary=%s
`, opts.Description, systemdPath(opts.WorkDir), systemdQuote(opts.BinaryPath), systemdQuote(opts.ConfigPath), opts.ConfigPath, opts.BinaryPath)
}

func (m platformServiceManager) Start() error {
	return runCommand("systemctl", "start", m.serviceUnit())
}

func (m platformServiceManager) Stop() error {
	return runCommand("systemctl", "stop", m.serviceUnit())
}

func (m platformServiceManager) Status() (ServiceStatus, error) {
	status := ServiceStatus{
		Service:  defaultServiceName,
		Native:   "systemd",
		UnitPath: linuxServicePath,
		State:    "not-installed",
	}
	b, err := os.ReadFile(linuxServicePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status, nil
		}
		return ServiceStatus{}, err
	}
	status.Installed = true
	status.State = "unknown"
	status.Config = metadataValue(string(b), "OnprestConfig")
	status.Binary = metadataValue(string(b), "OnprestBinary")
	if out, err := exec.Command("systemctl", "is-active", m.serviceUnit()).CombinedOutput(); err == nil {
		status.State = strings.TrimSpace(string(out))
	} else if s := strings.TrimSpace(string(out)); s != "" {
		status.State = s
	}
	if out, err := exec.Command("systemctl", "show", m.serviceUnit(), "-p", "MainPID", "--value").CombinedOutput(); err == nil {
		pid := strings.TrimSpace(string(out))
		if pid != "" && pid != "0" {
			status.PID = pid
		}
	}
	return status, nil
}

func (m platformServiceManager) Uninstall() error {
	_ = m.Stop()
	if err := os.Remove(linuxServicePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return runCommand("systemctl", "daemon-reload")
}

func (m platformServiceManager) serviceUnit() string {
	if m.opts.Name != "" {
		return m.opts.Name + ".service"
	}
	return defaultServiceName + ".service"
}

func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, `%`, `%%`)
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func systemdPath(s string) string {
	return strings.ReplaceAll(s, `%`, `%%`)
}

func metadataValue(content, key string) string {
	prefix := "# " + key + "="
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
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
