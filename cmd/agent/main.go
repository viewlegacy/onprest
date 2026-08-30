package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/viewlegacy/onprest/internal/agent"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if handled, code := agent.HandleCLI(ctx, os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	if handled, err := runAsPlatformService(os.Args[1:]); handled {
		if err != nil {
			log.Printf("agent service stopped: %v", err)
			os.Exit(1)
		}
		return
	}

	if err := runAgent(ctx, os.Args[1:], os.Stdout); err != nil && ctx.Err() == nil {
		log.Fatalf("agent stopped: %v", err)
	}
}

func runAgent(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := agent.LoadConfig(args)
	if err != nil {
		return fmt.Errorf("agent config: %w", err)
	}
	runner, err := agent.NewRunner(ctx, cfg, stdout)
	if err != nil {
		return fmt.Errorf("agent init: %w", err)
	}
	return runner.Run(ctx)
}
