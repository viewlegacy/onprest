package gateway

import (
	"strings"
	"testing"
)

func TestLoadConfigFromEnvAcceptsQuotedAPIKeysJSON(t *testing.T) {
	t.Setenv("GATEWAY_AGENT_PUBLIC_KEY", "TrMm87V3aET3MmGUzHf3_XKZRPEHe1bDM-POH1mrjr8")
	t.Setenv("GATEWAY_API_KEYS_JSON", `'[{"name":"dev","key_hash":"$2a$10$INgs32pPDl8EQAOTcQ1NN.eZUpNkDtyTKXh2luqxE32vNBmaLpy7m","capabilities":["*"]}]'`)
	t.Setenv("GATEWAY_IP_ALLOW_LIST", "203.0.113.0/24,198.51.100.10")
	t.Setenv("GATEWAY_TRUSTED_PROXY_CIDRS", "172.16.0.0/12")
	t.Setenv("GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND", "5")
	t.Setenv("GATEWAY_RATE_LIMIT_BURST", "7")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Name != "dev" || cfg.APIKeys[0].Capabilities[0] != "*" {
		t.Fatalf("APIKeys = %#v", cfg.APIKeys)
	}
	if len(cfg.IPAllowList) != 2 {
		t.Fatalf("len(IPAllowList) = %d, want 2", len(cfg.IPAllowList))
	}
	if len(cfg.TrustedProxies) != 1 {
		t.Fatalf("len(TrustedProxies) = %d, want 1", len(cfg.TrustedProxies))
	}
	if cfg.RateLimit.RequestsPerSecond != 5 || cfg.RateLimit.Burst != 7 {
		t.Fatalf("RateLimit = %#v", cfg.RateLimit)
	}
}

func TestLoadConfigFromEnvRejectsMissingAndInvalidRequiredValues(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		errMsg string
	}{
		{name: "missing public key", env: map[string]string{
			"GATEWAY_API_KEYS_JSON": `[{"name":"dev","key_hash":"hash","capabilities":["*"]}]`,
		}, errMsg: "GATEWAY_AGENT_PUBLIC_KEY is required"},
		{name: "invalid public key", env: map[string]string{
			"GATEWAY_AGENT_PUBLIC_KEY": "bad",
			"GATEWAY_API_KEYS_JSON":    `[{"name":"dev","key_hash":"hash","capabilities":["*"]}]`,
		}, errMsg: "GATEWAY_AGENT_PUBLIC_KEY must be base64url-encoded Ed25519 public key"},
		{name: "missing api keys", env: map[string]string{
			"GATEWAY_AGENT_PUBLIC_KEY": "TrMm87V3aET3MmGUzHf3_XKZRPEHe1bDM-POH1mrjr8",
		}, errMsg: "GATEWAY_API_KEYS_JSON is required"},
		{name: "invalid api keys json", env: map[string]string{
			"GATEWAY_AGENT_PUBLIC_KEY": "TrMm87V3aET3MmGUzHf3_XKZRPEHe1bDM-POH1mrjr8",
			"GATEWAY_API_KEYS_JSON":    `{`,
		}, errMsg: "parse GATEWAY_API_KEYS_JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := LoadConfigFromEnv(); err == nil || !strings.Contains(err.Error(), tc.errMsg) {
				t.Fatalf("LoadConfigFromEnv() error = %v, want containing %q", err, tc.errMsg)
			}
		})
	}
}

func TestLoadConfigFromEnvCapabilitiesFormsAndDefaults(t *testing.T) {
	t.Setenv("GATEWAY_AGENT_PUBLIC_KEY", "TrMm87V3aET3MmGUzHf3_XKZRPEHe1bDM-POH1mrjr8")
	t.Setenv("GATEWAY_API_KEYS_JSON", `[{"name":"admin","key_hash":"hash","capabilities":"*"},{"name":"dev","key_hash":"hash","capabilities":["get_customer"]}]`)

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimit.RequestsPerSecond != 10 || cfg.RateLimit.Burst != 20 {
		t.Fatalf("RateLimit = %#v, want defaults 10/20", cfg.RateLimit)
	}
	if len(cfg.APIKeys) != 2 {
		t.Fatalf("len(APIKeys) = %d, want 2", len(cfg.APIKeys))
	}
	if len(cfg.APIKeys[0].Capabilities) != 1 || cfg.APIKeys[0].Capabilities[0] != "*" {
		t.Fatalf("admin capabilities = %#v, want wildcard", cfg.APIKeys[0].Capabilities)
	}
	if len(cfg.APIKeys[1].Capabilities) != 1 || cfg.APIKeys[1].Capabilities[0] != "get_customer" {
		t.Fatalf("dev capabilities = %#v, want get_customer", cfg.APIKeys[1].Capabilities)
	}
}

func TestLoadConfigFromEnvRejectsNonPositiveRateLimit(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		value  string
		errMsg string
	}{
		{
			name:   "requests per second zero",
			key:    "GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND",
			value:  "0",
			errMsg: "GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND must be > 0",
		},
		{
			name:   "requests per second negative",
			key:    "GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND",
			value:  "-1",
			errMsg: "GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND must be > 0",
		},
		{
			name:   "burst zero",
			key:    "GATEWAY_RATE_LIMIT_BURST",
			value:  "0",
			errMsg: "GATEWAY_RATE_LIMIT_BURST must be > 0",
		},
		{
			name:   "burst negative",
			key:    "GATEWAY_RATE_LIMIT_BURST",
			value:  "-1",
			errMsg: "GATEWAY_RATE_LIMIT_BURST must be > 0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GATEWAY_AGENT_PUBLIC_KEY", "TrMm87V3aET3MmGUzHf3_XKZRPEHe1bDM-POH1mrjr8")
			t.Setenv("GATEWAY_API_KEYS_JSON", `[{"name":"dev","key_hash":"hash","capabilities":["*"]}]`)
			t.Setenv(tc.key, tc.value)

			if _, err := LoadConfigFromEnv(); err == nil || !strings.Contains(err.Error(), tc.errMsg) {
				t.Fatalf("LoadConfigFromEnv() error = %v, want containing %q", err, tc.errMsg)
			}
		})
	}
}

func TestStripEnvQuotes(t *testing.T) {
	tests := map[string]string{
		`[{"name":"dev"}]`:       `[{"name":"dev"}]`,
		`'[{"name":"dev"}]'`:     `[{"name":"dev"}]`,
		`"[{\"name\":\"dev\"}]"`: `[{\"name\":\"dev\"}]`,
	}
	for in, want := range tests {
		if got := stripEnvQuotes(in); got != want {
			t.Fatalf("stripEnvQuotes(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseIPBlocksAcceptsCIDRAndSingleIP(t *testing.T) {
	blocks, err := parseIPBlocks("192.0.2.0/24, 2001:db8::1, 198.51.100.10")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 {
		t.Fatalf("len(blocks) = %d, want 3", len(blocks))
	}
}

func TestParseIPBlocksRejectsInvalidValue(t *testing.T) {
	if _, err := parseIPBlocks("not-an-ip"); err == nil {
		t.Fatal("parseIPBlocks() error = nil, want error")
	}
}
