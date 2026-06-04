//go:build darwin

package agent

import (
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"strings"
)

const (
	darwinServiceLabel = "com.onprest.agent"
	darwinServiceDir   = "/Library/Application Support/Onprest"
	darwinServicePath  = "/Library/Application Support/Onprest/com.onprest.agent.plist"
)

type platformServiceManager struct {
	opts ServiceOptions
}

func newServiceManager(opts ServiceOptions) serviceManager {
	return platformServiceManager{opts: opts}
}

func (m platformServiceManager) Install() error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>--config</string>
    <string>%s</string>
  </array>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>KeepAlive</key>
  <true/>
  <key>RunAtLoad</key>
  <false/>
  <key>StandardOutPath</key>
  <string>/var/log/onprest-agent.stdout.log</string>
  <key>StandardErrorPath</key>
  <string>/var/log/onprest-agent.stderr.log</string>
  <key>OnprestConfig</key>
  <string>%s</string>
  <key>OnprestBinary</key>
  <string>%s</string>
</dict>
</plist>
`, darwinServiceLabel, xmlEscape(m.opts.BinaryPath), xmlEscape(m.opts.ConfigPath), xmlEscape(m.opts.WorkDir), xmlEscape(m.opts.ConfigPath), xmlEscape(m.opts.BinaryPath))
	if err := os.MkdirAll(darwinServiceDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(darwinServicePath, []byte(plist), 0o644)
}

func (m platformServiceManager) Start() error {
	if err := runCommand("launchctl", "bootstrap", "system", darwinServicePath); err != nil && !strings.Contains(err.Error(), "service already loaded") {
		return err
	}
	return runCommand("launchctl", "kickstart", "-k", "system/"+darwinServiceLabel)
}

func (m platformServiceManager) Stop() error {
	return runCommand("launchctl", "bootout", "system/"+darwinServiceLabel)
}

func (m platformServiceManager) Status() (ServiceStatus, error) {
	status := ServiceStatus{
		Service:  defaultServiceName,
		Native:   "launchd",
		UnitPath: darwinServicePath,
		State:    "not-installed",
	}
	b, err := os.ReadFile(darwinServicePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status, nil
		}
		return ServiceStatus{}, err
	}
	status.Installed = true
	content := string(b)
	status.Config = plistStringValue(content, "OnprestConfig")
	status.Binary = plistStringValue(content, "OnprestBinary")
	out, err := exec.Command("launchctl", "print", "system/"+darwinServiceLabel).CombinedOutput()
	if err != nil {
		status.State = "stopped"
		return status, nil
	}
	status.State = "loaded"
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid = ") {
			status.PID = strings.TrimSpace(strings.TrimPrefix(line, "pid = "))
			status.State = "running"
			break
		}
	}
	return status, nil
}

func (m platformServiceManager) Uninstall() error {
	_ = m.Stop()
	if err := os.Remove(darwinServicePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func xmlEscape(s string) string {
	return html.EscapeString(s)
}

func plistStringValue(content, key string) string {
	marker := "<key>" + key + "</key>"
	idx := strings.Index(content, marker)
	if idx < 0 {
		return ""
	}
	rest := content[idx+len(marker):]
	start := strings.Index(rest, "<string>")
	end := strings.Index(rest, "</string>")
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return html.UnescapeString(rest[start+len("<string>") : end])
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
