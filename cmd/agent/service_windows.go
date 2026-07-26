//go:build windows

package main

import (
	"context"
	"io"

	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "onprest-agent"

func runAsPlatformService(args []string) (bool, error) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false, err
	}
	return true, svc.Run(windowsServiceName, &windowsService{args: args})
}

type windowsService struct {
	args []string
}

func (s *windowsService) Execute(_ []string, changes <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runAgent(ctx, s.args, io.Discard) }()
	statuses <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			if err != nil && ctx.Err() == nil {
				return false, 1
			}
			return false, 0
		case change := <-changes:
			switch change.Cmd {
			case svc.Interrogate:
				statuses <- change.CurrentStatus
			case svc.Stop, svc.Shutdown:
				statuses <- svc.Status{State: svc.StopPending}
				cancel()
				if err := <-done; err != nil && ctx.Err() == nil {
					return false, 1
				}
				return false, 0
			}
		}
	}
}
