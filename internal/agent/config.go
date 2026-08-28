package agent

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	CapabilityFile string
	ReconnectEvery time.Duration
}

func LoadConfigFromEnv() (Config, error) {
	return loadConfig(nil, os.Getenv)
}

func LoadConfig(args []string) (Config, error) {
	return loadConfig(args, os.Getenv)
}

func loadConfig(args []string, lookupEnv func(string) string) (Config, error) {
	fs := flag.NewFlagSet("onprest-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configFile := fs.String("config", "", "path to capability YAML file")
	capabilityFile := fs.String("capability-file", "", "path to capability YAML file")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parse agent args: %w", err)
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected agent argument %q", fs.Arg(0))
	}
	path := resolveCapabilityFile(*configFile, *capabilityFile, lookupEnv)
	return Config{
		CapabilityFile: path,
		ReconnectEvery: 30 * time.Second,
	}, nil
}

func resolveCapabilityFile(configFile, capabilityFile string, lookupEnv func(string) string) string {
	if configFile != "" {
		return configFile
	}
	if capabilityFile != "" {
		return capabilityFile
	}
	if path := envFrom(lookupEnv, "AGENT_CAPABILITY_FILE", ""); path != "" {
		return path
	}
	return defaultCapabilityFile()
}

func envFrom(lookupEnv func(string) string, key, fallback string) string {
	if v := lookupEnv(key); v != "" {
		return v
	}
	return fallback
}

func defaultCapabilityFile() string {
	exe, err := os.Executable()
	if err != nil {
		return "capability.yaml"
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "capability.yaml"
	}
	return filepath.Join(filepath.Dir(abs), "capability.yaml")
}
