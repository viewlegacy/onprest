package agent

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type CapabilityFile struct {
	Service      ServiceDef               `json:"service" yaml:"service"`
	Gateway      GatewayDef               `json:"gateway" yaml:"gateway"`
	Database     DatabaseDef              `json:"database" yaml:"database"`
	Logging      LoggingDef               `json:"logging" yaml:"logging"`
	Defaults     PolicyDef                `json:"defaults" yaml:"defaults"`
	Capabilities map[string]CapabilityDef `json:"capabilities" yaml:"capabilities"`
}

type ServiceDef struct {
	Title       string `json:"title" yaml:"title"`
	Version     string `json:"version" yaml:"version"`
	Description string `json:"description" yaml:"description"`
}

type GatewayDef struct {
	URL             string `json:"url" yaml:"url"`
	AgentPrivateKey string `json:"agent_private_key" yaml:"agent_private_key"`
}

type DatabaseDef struct {
	Driver   string `json:"driver" yaml:"driver"`
	Host     string `json:"host" yaml:"host"`
	Port     int    `json:"port" yaml:"port"`
	Name     string `json:"name" yaml:"name"`
	User     string `json:"user" yaml:"user"`
	Password string `json:"password" yaml:"password"`
}

type LoggingDef struct {
	MaxSize  string `json:"max_size" yaml:"max_size"`
	MaxFiles int    `json:"max_files" yaml:"max_files"`
}

type CapabilityDef struct {
	Name        string              `json:"name" yaml:"-"`
	Description string              `json:"description" yaml:"description"`
	SQL         string              `json:"sql" yaml:"sql"`
	Params      map[string]ParamDef `json:"params" yaml:"params"`
	Policy      PolicyDef           `json:"policy" yaml:"policy"`
	Result      ResultDef           `json:"result,omitempty" yaml:"result,omitempty"`
}

type ParamDef struct {
	Type        string `json:"type" yaml:"type"`
	Required    bool   `json:"required" yaml:"required"`
	Default     any    `json:"default,omitempty" yaml:"default,omitempty"`
	Enum        []any  `json:"enum,omitempty" yaml:"enum,omitempty"`
	Minimum     *int64 `json:"minimum,omitempty" yaml:"minimum,omitempty"`
	Maximum     *int64 `json:"maximum,omitempty" yaml:"maximum,omitempty"`
	MinLength   *int   `json:"minLength,omitempty" yaml:"minLength,omitempty"`
	MaxLength   *int   `json:"maxLength,omitempty" yaml:"maxLength,omitempty"`
	Pattern     string `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Format      string `json:"format,omitempty" yaml:"format,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

type PolicyDef struct {
	Readonly        *bool  `json:"readonly,omitempty" yaml:"readonly,omitempty"`
	Timeout         string `json:"timeout" yaml:"timeout"`
	MaxRows         int    `json:"max_rows" yaml:"max_rows"`
	MaxBytes        string `json:"max_bytes" yaml:"max_bytes"`
	ExposeInOpenAPI *bool  `json:"expose_in_openapi,omitempty" yaml:"expose_in_openapi,omitempty"`
}

type ResultDef map[string]ResultColumnDef

type ResultColumnDef struct {
	Type        string `json:"type" yaml:"type"`
	Description string `json:"description" yaml:"description"`
}

func LoadCapabilityFile(path string) (*CapabilityFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cf CapabilityFile
	if err := yaml.Unmarshal(b, &cf); err != nil {
		return nil, fmt.Errorf("parse capability.yaml: %w", err)
	}
	if err := cf.Lint(); err != nil {
		return nil, err
	}
	return &cf, nil
}

func (cf *CapabilityFile) Lint() error {
	if cf.Service.Title == "" {
		cf.Service.Title = "Onprest Agent"
	}
	if cf.Service.Version == "" {
		cf.Service.Version = "0.1.0"
	}
	if cf.Gateway.URL == "" {
		return errors.New("gateway.url is required")
	}
	if cf.Gateway.AgentPrivateKey == "" {
		return errors.New("gateway.agent_private_key is required")
	}
	if key, err := base64.RawURLEncoding.DecodeString(cf.Gateway.AgentPrivateKey); err != nil || len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("gateway.agent_private_key must be base64url-encoded Ed25519 private key")
	}
	if err := cf.Database.lint(); err != nil {
		return err
	}
	if err := cf.Logging.lint(); err != nil {
		return err
	}
	if len(cf.Capabilities) == 0 {
		return errors.New("at least one capability is required")
	}
	if err := mergePolicy(PolicyDef{}, cf.Defaults).lint("defaults"); err != nil {
		return err
	}
	nameRe := regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)
	for name, cap := range cf.Capabilities {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("capabilities.%s name is invalid", name)
		}
		cap.Name = name
		cap.Policy = mergePolicy(cf.Defaults, cap.Policy)
		if strings.TrimSpace(cap.SQL) == "" {
			return fmt.Errorf("%s.sql is required", name)
		}
		if err := cap.Policy.lint(name + ".policy"); err != nil {
			return err
		}
		if readonly(cap.Policy) && !isReadOnlySQL(cap.SQL) {
			return fmt.Errorf("%s.sql must be read-only when policy.readonly is true", name)
		}
		for pname, p := range cap.Params {
			if err := p.lint(name + ".params." + pname); err != nil {
				return err
			}
		}
		for cname, col := range cap.Result {
			if !validType(col.Type) {
				return fmt.Errorf("%s.result.%s.type is invalid", name, cname)
			}
		}
		cf.Capabilities[name] = cap
	}
	return nil
}

func (l *LoggingDef) lint() error {
	if l.MaxSize == "" {
		l.MaxSize = "10MB"
	}
	if _, err := parseByteSize(l.MaxSize); err != nil {
		return fmt.Errorf("logging.max_size is invalid: %w", err)
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = 3
	}
	return nil
}

func (db DatabaseDef) lint() error {
	switch db.Driver {
	case "postgres", "mysql", "sqlserver", "oracle":
	default:
		return fmt.Errorf("database.driver must be one of postgres, mysql, sqlserver, oracle")
	}
	if db.Host == "" {
		return errors.New("database.host is required")
	}
	if db.Name == "" {
		return errors.New("database.name is required")
	}
	if db.User == "" {
		return errors.New("database.user is required")
	}
	if db.Port <= 0 {
		return errors.New("database.port is required")
	}
	return nil
}

func (db DatabaseDef) DSN() string {
	hostPort := net.JoinHostPort(db.Host, fmt.Sprint(db.Port))
	switch db.Driver {
	case "postgres":
		u := url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(db.User, db.Password),
			Host:   hostPort,
			Path:   "/" + db.Name,
		}
		q := u.Query()
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
		return u.String()
	case "mysql":
		return fmt.Sprintf("%s:%s@tcp(%s)/%s", db.User, db.Password, hostPort, db.Name)
	case "sqlserver":
		u := url.URL{
			Scheme: "sqlserver",
			User:   url.UserPassword(db.User, db.Password),
			Host:   hostPort,
		}
		q := u.Query()
		q.Set("database", db.Name)
		q.Set("encrypt", "disable")
		u.RawQuery = q.Encode()
		return u.String()
	case "oracle":
		u := url.URL{
			Scheme: "oracle",
			User:   url.UserPassword(db.User, db.Password),
			Host:   hostPort,
			Path:   "/" + db.Name,
		}
		return u.String()
	default:
		return ""
	}
}

func (p ParamDef) lint(path string) error {
	if !validType(p.Type) {
		return fmt.Errorf("%s.type is invalid", path)
	}
	if p.Pattern != "" {
		if _, err := regexp.Compile(p.Pattern); err != nil {
			return fmt.Errorf("%s.pattern is invalid: %w", path, err)
		}
	}
	switch p.Format {
	case "", "email", "uuid", "date", "date-time", "uri":
	default:
		return fmt.Errorf("%s.format is invalid", path)
	}
	if p.MinLength != nil && *p.MinLength < 0 {
		return fmt.Errorf("%s.minLength must be >= 0", path)
	}
	if p.MaxLength != nil && *p.MaxLength < 0 {
		return fmt.Errorf("%s.maxLength must be >= 0", path)
	}
	if p.MinLength != nil && p.MaxLength != nil && *p.MinLength > *p.MaxLength {
		return fmt.Errorf("%s.minLength must be <= maxLength", path)
	}
	return nil
}

func (p PolicyDef) lint(path string) error {
	if _, err := timeout(p); err != nil {
		return fmt.Errorf("%s.timeout is invalid: %w", path, err)
	}
	if p.MaxRows <= 0 {
		return fmt.Errorf("%s.max_rows must be > 0", path)
	}
	if _, err := maxBytes(p); err != nil {
		return fmt.Errorf("%s.max_bytes is invalid: %w", path, err)
	}
	return nil
}

func (cf *CapabilityFile) ByName() map[string]CapabilityDef {
	out := map[string]CapabilityDef{}
	for name, cap := range cf.Capabilities {
		cap.Name = name
		out[name] = cap
	}
	return out
}

func (cf *CapabilityFile) CapabilityList() []CapabilityDef {
	names := make([]string, 0, len(cf.Capabilities))
	for name := range cf.Capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]CapabilityDef, 0, len(names))
	for _, name := range names {
		cap := cf.Capabilities[name]
		cap.Name = name
		out = append(out, cap)
	}
	return out
}

func mergePolicy(defaults, cap PolicyDef) PolicyDef {
	out := defaults
	if cap.Readonly != nil {
		out.Readonly = cap.Readonly
	}
	if cap.Timeout != "" {
		out.Timeout = cap.Timeout
	}
	if cap.MaxRows != 0 {
		out.MaxRows = cap.MaxRows
	}
	if cap.MaxBytes != "" {
		out.MaxBytes = cap.MaxBytes
	}
	if cap.ExposeInOpenAPI != nil {
		out.ExposeInOpenAPI = cap.ExposeInOpenAPI
	}
	if out.Readonly == nil {
		v := true
		out.Readonly = &v
	}
	if out.Timeout == "" {
		out.Timeout = "5s"
	}
	if out.MaxRows == 0 {
		out.MaxRows = 100
	}
	if out.MaxBytes == "" {
		out.MaxBytes = "1MB"
	}
	if out.ExposeInOpenAPI == nil {
		v := true
		out.ExposeInOpenAPI = &v
	}
	return out
}

func validType(t string) bool {
	switch t {
	case "string", "integer", "number", "boolean":
		return true
	default:
		return false
	}
}

func timeout(p PolicyDef) (time.Duration, error) {
	if p.Timeout == "" {
		return 5 * time.Second, nil
	}
	return time.ParseDuration(p.Timeout)
}

func readonly(p PolicyDef) bool {
	return p.Readonly == nil || *p.Readonly
}

func exposeInOpenAPI(p PolicyDef) bool {
	return p.ExposeInOpenAPI == nil || *p.ExposeInOpenAPI
}

func maxBytes(p PolicyDef) (int64, error) {
	if p.MaxBytes == "" {
		return 1 << 20, nil
	}
	return parseByteSize(p.MaxBytes)
}

func parseByteSize(raw string) (int64, error) {
	s := strings.TrimSpace(strings.ToUpper(raw))
	multiplier := int64(1)
	for _, suffix := range []struct {
		text string
		mul  int64
	}{
		{"KB", 1 << 10},
		{"MB", 1 << 20},
		{"GB", 1 << 30},
		{"B", 1},
	} {
		if strings.HasSuffix(s, suffix.text) {
			multiplier = suffix.mul
			s = strings.TrimSpace(strings.TrimSuffix(s, suffix.text))
			break
		}
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, errors.New("must be > 0")
	}
	return n * multiplier, nil
}

func isReadOnlySQL(query string) bool {
	keyword := firstSQLKeyword(query)
	return keyword == "select"
}

func firstSQLKeyword(query string) string {
	s := strings.TrimSpace(query)
	for {
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = strings.TrimSpace(s[i+1:])
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = strings.TrimSpace(s[i+2:])
				continue
			}
			return ""
		}
		break
	}
	for i, r := range s {
		if r < 'a' || r > 'z' {
			if r < 'A' || r > 'Z' {
				return strings.ToLower(s[:i])
			}
		}
	}
	return strings.ToLower(s)
}
