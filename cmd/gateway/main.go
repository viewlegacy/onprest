package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/viewlegacy/onprest/internal/gateway"
)

func main() {
	if gateway.HandleCLI(os.Args[1:], os.Stdout, os.Stderr) {
		return
	}
	cfg, err := gateway.LoadConfigFromEnv()
	if err != nil {
		log.Fatalf("gateway config: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := gateway.NewServer(cfg, os.Stdout).ListenAndServeContext(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("gateway stopped: %v", err)
	}
}
