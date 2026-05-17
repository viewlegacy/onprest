package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/viewlegacy/onprest/internal/agent"
)

func main() {
	if handled, code := agent.HandleCLI(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	cfg, err := agent.LoadConfig(os.Args[1:])
	if err != nil {
		log.Fatalf("agent config: %v", err)
	}
	runner, err := agent.NewRunner(cfg, os.Stdout)
	if err != nil {
		log.Fatalf("agent init: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runner.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("agent stopped: %v", err)
	}
}
