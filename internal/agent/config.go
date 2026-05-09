package agent

import (
	"os"
	"time"
)

type Config struct {
	CapabilityFile string
	ReconnectEvery time.Duration
}

func LoadConfigFromEnv() (Config, error) {
	return Config{
		CapabilityFile: env("AGENT_CAPABILITY_FILE", "capability.yaml"),
		ReconnectEvery: 30 * time.Second,
	}, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
