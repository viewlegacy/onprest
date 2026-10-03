package agent

import (
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/viewlegacy/onprest/internal/agent/providers"
)

// configurationRedactor owns only private copies of captured configuration.
// It never reads a file, resolves a source again, or contributes to metadata.
type configurationRedactor struct {
	secrets []string
	sql     []string
	seen    map[string]bool
}

func newConfigurationRedactor(cf *CapabilityFile) *configurationRedactor {
	r := &configurationRedactor{seen: map[string]bool{}}
	if cf != nil {
		r.secret(cf.Gateway.AgentPrivateKey)
		if cf.Compiled != nil {
			for _, cap := range cf.Compiled.Capabilities {
				if sql, ok := cap.Executor.Settings["sql"].(string); ok && sql != "" {
					r.sql = append(r.sql, sql)
				}
			}
		}
	}
	return r
}

func (r *configurationRedactor) secret(value string) {
	if value == "" || r.seen[value] {
		return
	}
	quoted := strconv.Quote(value)
	for _, encoded := range []string{value, quoted[1 : len(quoted)-1], url.QueryEscape(value), url.PathEscape(value), strings.TrimPrefix(url.UserPassword("", value).String(), ":")} {
		if r.seen[encoded] {
			continue
		}
		r.seen[encoded] = true
		r.secrets = append(r.secrets, encoded)
	}
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

func (r *configurationRedactor) capture(snapshot *providers.Snapshot) {
	cap := snapshot.Definition()
	if sql, ok := cap.Executor.Settings["sql"].(string); ok && sql != "" {
		r.sql = append(r.sql, sql)
	}
	for _, key := range snapshot.SourceKeys() {
		if key.Group != "databases" {
			continue
		}
		source, _ := snapshot.Source(key)
		db := snapshotDatabase(source)
		if query, ok := cap.Executor.Settings["sql"].(string); ok {
			if converted, _, err := buildSQL(db.Driver, query, schemaDummyParams(cap.Input)); err == nil {
				r.sql = append(r.sql, converted)
			}
		}
		r.secret(db.DSN())
		r.secret(db.Password)
		for _, material := range [][]byte{db.TLS.CABytes, db.TLS.CertBytes, db.TLS.KeyBytes} {
			r.secret(string(material))
		}
	}
}

func (r *configurationRedactor) redact(detail string) string {
	for _, query := range r.sql {
		detail = strings.ReplaceAll(detail, query, "[SQL REDACTED]")
	}
	for _, secret := range r.secrets {
		detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
	}
	return detail
}
