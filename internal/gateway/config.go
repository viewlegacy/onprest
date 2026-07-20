package gateway

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultMaxRequestBodyBytes int64 = 1 << 20

type Config struct {
	Addr                string
	PublicURL           string
	AgentPublicKey      string
	APIKeys             []APIKey
	CORSAllowedOrigins  []string
	IPAllowList         []*net.IPNet
	TrustedProxies      []*net.IPNet
	RateLimit           RateLimitConfig
	AgentTimeout        time.Duration
	AgentWriteTimeout   time.Duration
	AgentPingInterval   time.Duration
	AgentPongTimeout    time.Duration
	BodyReadTimeout     time.Duration
	MaxRequestBodyBytes int64
	EmitOpenAPISnapshot bool
}

type APIKey struct {
	Name         string       `json:"name"`
	KeyHash      string       `json:"key_hash"`
	Capabilities capabilities `json:"capabilities"`
}

type RateLimitConfig struct {
	RequestsPerSecond float64
	Burst             int
}

type capabilities []string

func (c *capabilities) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if one == "" {
			*c = nil
			return nil
		}
		*c = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*c = many
	return nil
}

func LoadConfigFromEnv() (Config, error) {
	maxRequestBodyBytes, err := envInt64("GATEWAY_MAX_REQUEST_BODY_BYTES", defaultMaxRequestBodyBytes)
	if err != nil {
		return Config{}, err
	}
	if maxRequestBodyBytes <= 0 {
		return Config{}, errors.New("GATEWAY_MAX_REQUEST_BODY_BYTES must be > 0")
	}

	rps, err := envFloat("GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND", 10)
	if err != nil {
		return Config{}, err
	}
	if rps <= 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
		return Config{}, errors.New("GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND must be > 0")
	}
	burst, err := envInt("GATEWAY_RATE_LIMIT_BURST", 20)
	if err != nil {
		return Config{}, err
	}
	if burst <= 0 {
		return Config{}, errors.New("GATEWAY_RATE_LIMIT_BURST must be > 0")
	}
	emitOpenAPISnapshot, err := envBool("GATEWAY_EMIT_OPENAPI_SNAPSHOT", false)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Addr:                env("GATEWAY_ADDR", ":8080"),
		AgentPublicKey:      os.Getenv("GATEWAY_AGENT_PUBLIC_KEY"),
		EmitOpenAPISnapshot: emitOpenAPISnapshot,
		RateLimit: RateLimitConfig{
			RequestsPerSecond: rps,
			Burst:             burst,
		},
		AgentTimeout:        30 * time.Second,
		AgentWriteTimeout:   5 * time.Second,
		AgentPingInterval:   15 * time.Second,
		AgentPongTimeout:    10 * time.Second,
		BodyReadTimeout:     15 * time.Second,
		MaxRequestBodyBytes: maxRequestBodyBytes,
	}
	publicURL, err := normalizePublicURL(os.Getenv("GATEWAY_PUBLIC_URL"))
	if err != nil {
		return cfg, err
	}
	cfg.PublicURL = publicURL
	corsAllowedOrigins, err := parseCORSAllowedOrigins(os.Getenv("GATEWAY_CORS_ALLOWED_ORIGINS"))
	if err != nil {
		return cfg, err
	}
	cfg.CORSAllowedOrigins = corsAllowedOrigins
	if cfg.AgentPublicKey == "" {
		return cfg, errors.New("GATEWAY_AGENT_PUBLIC_KEY is required")
	}
	if key, err := base64.RawURLEncoding.DecodeString(cfg.AgentPublicKey); err != nil || len(key) != ed25519.PublicKeySize {
		return cfg, errors.New("GATEWAY_AGENT_PUBLIC_KEY must be base64url-encoded Ed25519 public key")
	}
	rawKeys := stripEnvQuotes(strings.TrimSpace(os.Getenv("GATEWAY_API_KEYS_JSON")))
	if rawKeys == "" {
		return cfg, errors.New("GATEWAY_API_KEYS_JSON is required")
	}
	if err := json.Unmarshal([]byte(rawKeys), &cfg.APIKeys); err != nil {
		return cfg, fmt.Errorf("parse GATEWAY_API_KEYS_JSON: %w", err)
	}
	if len(cfg.APIKeys) == 0 {
		return cfg, errors.New("at least one API key is required")
	}
	if len(cfg.APIKeys) > maxAPIKeys {
		return cfg, fmt.Errorf("GATEWAY_API_KEYS_JSON supports at most %d keys", maxAPIKeys)
	}
	for _, key := range cfg.APIKeys {
		if key.Name == "" {
			return cfg, errors.New("GATEWAY_API_KEYS_JSON[].name is required")
		}
		if key.KeyHash == "" {
			return cfg, fmt.Errorf("api key %q key_hash is required", key.Name)
		}
	}
	blocks, err := parseIPBlocks(os.Getenv("GATEWAY_IP_ALLOW_LIST"))
	if err != nil {
		return cfg, err
	}
	cfg.IPAllowList = blocks
	blocks, err = parseIPBlocks(os.Getenv("GATEWAY_TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return cfg, err
	}
	cfg.TrustedProxies = blocks
	return cfg, nil
}

func parseCORSAllowedOrigins(raw string) ([]string, error) {
	var origins []string
	seen := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(part)
		if origin == "" {
			continue
		}
		normalized, err := normalizeOrigin(origin)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		origins = append(origins, normalized)
	}
	return origins, nil
}

func parseIPBlocks(raw string) ([]*net.IPNet, error) {
	var blocks []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, block, err := net.ParseCIDR(part)
		if err != nil {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, err
			}
			if ip.To4() != nil {
				part += "/32"
			} else {
				part += "/128"
			}
			_, block, err = net.ParseCIDR(part)
			if err != nil {
				return nil, err
			}
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func stripEnvQuotes(v string) string {
	if len(v) < 2 {
		return v
	}
	if (v[0] == '\'' && v[len(v)-1] == '\'') || (v[0] == '"' && v[len(v)-1] == '"') {
		return v[1 : len(v)-1]
	}
	return v
}

func envInt(key string, fallback int) (int, error) {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer: %w", key, err)
		}
		return n, nil
	}
	return fallback, nil
}

func envInt64(key string, fallback int64) (int64, error) {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer: %w", key, err)
		}
		return n, nil
	}
	return fallback, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a number: %w", key, err)
		}
		return n, nil
	}
	return fallback, nil
}

func envBool(key string, fallback bool) (bool, error) {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return false, fmt.Errorf("%s must be a boolean: %w", key, err)
		}
		return b, nil
	}
	return fallback, nil
}
