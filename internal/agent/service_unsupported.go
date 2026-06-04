//go:build !linux && !darwin && !windows

package agent

import "fmt"

type platformServiceManager struct {
	opts ServiceOptions
}

func newServiceManager(opts ServiceOptions) serviceManager {
	return platformServiceManager{opts: opts}
}

func (m platformServiceManager) Install() error {
	return unsupportedServicePlatform()
}

func (m platformServiceManager) Start() error {
	return unsupportedServicePlatform()
}

func (m platformServiceManager) Stop() error {
	return unsupportedServicePlatform()
}

func (m platformServiceManager) Status() (ServiceStatus, error) {
	return ServiceStatus{}, unsupportedServicePlatform()
}

func (m platformServiceManager) Uninstall() error {
	return unsupportedServicePlatform()
}

func unsupportedServicePlatform() error {
	return fmt.Errorf("agent service management is supported on Linux, macOS, and Windows")
}
