package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
	"path/filepath"
)

// The runtime view holds operational configuration only. Executable definitions
// and their validation remain owned by the versioned compiler and controller.
func capabilityRuntimeView(cfg *contract.Config, path string) (*CapabilityFile, error) {
	cf := &CapabilityFile{Compiled: cfg, ConfigPath: path, Capabilities: map[string]CapabilityDef{}}
	decode := func(v any, dest any) { b, _ := json.Marshal(v); _ = json.Unmarshal(b, dest) }
	decode(cfg.Service, &cf.Service)
	decode(cfg.Gateway, &cf.Gateway)
	n := int(cfg.Runtime["max_concurrent_requests"].(int64))
	cf.Runtime.MaxConcurrentRequests = &n
	cf.Logging = LoggingDef{Level: cfg.Logging["level"].(string), MaxSize: fmt.Sprint(cfg.Logging["max_size"]), MaxFiles: int(cfg.Logging["max_files"].(int64))}
	if filename, ok := cfg.Gateway["agent_private_key_file"].(string); ok {
		if !filepath.IsAbs(filename) {
			filename = filepath.Join(filepath.Dir(path), filename)
		}
		key, err := providers.ReadProtectedConfiguration(context.Background(), filename)
		if err != nil {
			return nil, errors.New("agent private key could not be read")
		}
		cf.Gateway.AgentPrivateKey = string(key)
		clear(key)
	}
	rawKey, err := base64.RawURLEncoding.DecodeString(cf.Gateway.AgentPrivateKey)
	if err != nil || len(rawKey) != ed25519.PrivateKeySize || base64.RawURLEncoding.EncodeToString(rawKey) != cf.Gateway.AgentPrivateKey {
		clear(rawKey)
		return nil, errors.New("agent private key is invalid")
	}
	clear(rawKey)
	for name, cap := range cfg.Capabilities {
		cf.Capabilities[name] = CapabilityDef{Name: name, Operation: sqlOperation(cap.Executor.Operation)}
	}
	return cf, nil
}

func snapshotDatabase(source map[string]any) DatabaseDef {
	text := func(k string) string { s, _ := source[k].(string); return s }
	db := DatabaseDef{Driver: text("driver"), Host: text("host"), Name: text("name"), User: text("user"), Password: text("password"), Port: int(source["port"].(int64))}
	t := source["tls"].(map[string]any)
	db.TLS.Mode, _ = t["mode"].(string)
	db.TLS.ServerName, _ = t["server_name"].(string)
	db.TLS.CABytes, _ = t["ca_bytes"].([]byte)
	db.TLS.CertBytes, _ = t["cert_bytes"].([]byte)
	db.TLS.KeyBytes, _ = t["key_bytes"].([]byte)
	db.TLS.Snapshot = true
	return db
}
