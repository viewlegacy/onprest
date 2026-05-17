package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	defaultServiceName        = "onprest-agent"
	defaultServiceDisplayName = "Onprest Agent"
	defaultServiceDescription = "Onprest on-prem capability agent"
)

type ServiceOptions struct {
	Name        string
	DisplayName string
	Description string
	ConfigPath  string
	BinaryPath  string
	WorkDir     string
}

type ServiceStatus struct {
	Service   string
	Native    string
	Installed bool
	State     string
	PID       string
	Config    string
	Binary    string
	UnitPath  string
}

type serviceManager interface {
	Install() error
	Start() error
	Stop() error
	Status() (ServiceStatus, error)
	Uninstall() error
}

func defaultServiceIdentity() ServiceOptions {
	return ServiceOptions{
		Name:        defaultServiceName,
		DisplayName: defaultServiceDisplayName,
		Description: defaultServiceDescription,
	}
}

func defaultServiceOptions(configFile string) (ServiceOptions, error) {
	configPath := defaultCapabilityFile()
	if configFile != "" {
		abs, err := absPath(configFile)
		if err != nil {
			return ServiceOptions{}, err
		}
		configPath = abs
	}
	binaryPath, err := os.Executable()
	if err != nil {
		return ServiceOptions{}, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryPath, err = filepath.Abs(binaryPath)
	if err != nil {
		return ServiceOptions{}, fmt.Errorf("resolve executable path: %w", err)
	}
	return ServiceOptions{
		Name:        defaultServiceName,
		DisplayName: defaultServiceDisplayName,
		Description: defaultServiceDescription,
		ConfigPath:  configPath,
		BinaryPath:  binaryPath,
		WorkDir:     filepath.Dir(binaryPath),
	}, nil
}

func printServiceStatus(w io.Writer, status ServiceStatus) {
	fmt.Fprintf(w, "service: %s\n", status.Service)
	fmt.Fprintf(w, "native: %s\n", status.Native)
	fmt.Fprintf(w, "installed: %t\n", status.Installed)
	fmt.Fprintf(w, "state: %s\n", status.State)
	if status.PID != "" {
		fmt.Fprintf(w, "pid: %s\n", status.PID)
	}
	if status.Config != "" {
		fmt.Fprintf(w, "config: %s\n", status.Config)
	}
	if status.Binary != "" {
		fmt.Fprintf(w, "binary: %s\n", status.Binary)
	}
	if status.UnitPath != "" {
		fmt.Fprintf(w, "unit: %s\n", status.UnitPath)
	}
}
