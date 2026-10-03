package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
)

func TestVersionedPrivateConfigurationIsRedactedFromAllDiagnosticLogs(t *testing.T) {
	oldExe, oldOpen := executablePath, openDatabaseForPreparation
	t.Cleanup(func() { executablePath, openDatabaseForPreparation = oldExe, oldOpen })
	for _, secretMode := range []string{"inline", "file"} {
		for _, entry := range []string{"startup", "validate", "doctor", "runtime"} {
			for _, stage := range []string{"open", "ping", "explain"} {
				if entry == "runtime" && stage != "explain" {
					continue
				}
				t.Run(secretMode+"/"+entry+"/"+stage, func(t *testing.T) {
					preflightDriver.reset()
					dir := t.TempDir()
					executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
					ca, cert, key, _ := writeDatabaseTLSFixture(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "db.internal")
					privateKey, _ := os.ReadFile(key)
					password, query := "PASSWORD_PRIVATE_SENTINEL + / @\n", "select :id as SQL_PRIVATE_SENTINEL"
					convertedQuery := "select $1 as SQL_PRIVATE_SENTINEL"
					passwordPath := filepath.Join(dir, "password")
					secret := "password: " + string(protocol.MustJSON(password))
					if secretMode == "file" {
						if err := os.WriteFile(passwordPath, []byte(password), 0600); err != nil {
							t.Fatal(err)
						}
						secret = "password_file: " + string(protocol.MustJSON(passwordPath))
					}
					data := versionedFixture("    executor: {type: db, source: primary, sql: " + string(protocol.MustJSON(query)) + "}\n    input: {properties: {id: {type: integer, default: 7}}}\n")
					data = strings.Replace(data, "user: test", "user: test, "+secret+", tls: {mode: verify-full, ca_file: "+string(protocol.MustJSON(ca))+", cert_file: "+string(protocol.MustJSON(cert))+", key_file: "+string(protocol.MustJSON(key))+"}", 1)
					path := filepath.Join(dir, "capability.yaml")
					if err := os.WriteFile(path, []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
					var privateDetail, capturedDSN string
					openDatabaseForPreparation = func(definition DatabaseDef) (*sql.DB, error) {
						if definition.Password != password || !bytes.Equal(definition.TLS.KeyBytes, privateKey) {
							return nil, errors.New("snapshot did not contain the resolved secret")
						}
						capturedDSN = definition.DSN()
						privateDetail = "retained nonsecret diagnostic " + password + " " + strconv.Quote(password) + " " + capturedDSN + " " + query + " " + convertedQuery + " " + string(privateKey) + " " + strconv.Quote(string(privateKey)) + " " + testAgentPrivateKey
						// Later diagnostics must use captured values, never reopen files.
						_ = os.WriteFile(passwordPath, []byte("REPLACEMENT_PASSWORD"), 0600)
						_ = os.WriteFile(key, []byte("REPLACEMENT_TLS_KEY"), 0600)
						if entry != "runtime" {
							switch stage {
							case "open":
								return nil, errors.New(privateDetail)
							case "ping":
								preflightDriver.pingErr = errors.New(privateDetail)
							case "explain":
								preflightDriver.queryErr = errors.New(privateDetail)
							}
						}
						return sql.Open(preflightTestDriverName, "")
					}
					var stdout, stderr bytes.Buffer
					logName := "onprest-agent.log"
					if entry == "startup" || entry == "runtime" {
						runner, err := NewRunner(context.Background(), Config{CapabilityFile: path}, &stdout)
						if entry == "startup" {
							if err == nil {
								t.Fatal("startup did not fail")
							}
							stderr.WriteString(err.Error())
						} else {
							if err != nil {
								t.Fatal(err)
							}
							preflightDriver.queryErr = errors.New(privateDetail)
							snapshot, _ := runner.versioned.Snapshot("test")
							response := runner.executeVersioned(context.Background(), protocol.Request{Type: "request", ID: "runtime-redaction", Capability: "test", DefinitionHash: snapshot.DefinitionHash(), Deadline: time.Now().Add(time.Second)})
							if response.Error == nil {
								t.Fatal("runtime query did not fail")
							}
							stdout.Write(protocol.MustJSON(response))
							_ = runner.versioned.Close()
							_ = runner.detailLogCloser.Close()
						}
					} else {
						_, code := HandleCLI(context.Background(), []string{entry, "--config", path, "--format", "json"}, &stdout, &stderr)
						if code != 1 {
							t.Fatal("diagnostic entrypoint did not fail")
						}
						logName = "onprest-agent." + entry + ".log"
					}
					logs, err := os.ReadFile(filepath.Join(dir, logName))
					if err != nil {
						t.Fatal(err)
					}
					var details strings.Builder
					for _, line := range bytes.Split(logs, []byte{'\n'}) {
						if len(line) == 0 {
							continue
						}
						var record map[string]any
						if json.Unmarshal(line, &record) != nil {
							t.Fatal("invalid diagnostic record")
						}
						if detail, ok := record["detail"].(string); ok {
							details.WriteString(detail)
						}
					}
					combined := stdout.String() + stderr.String() + details.String()
					for _, value := range []string{password, strings.Trim(strconv.Quote(password), `"`), capturedDSN, query, convertedQuery, string(privateKey), strings.Trim(strconv.Quote(string(privateKey)), `"`), testAgentPrivateKey} {
						if value == "" || strings.Contains(combined, value) {
							t.Fatal("configuration leaked to diagnostic/runtime/public output")
						}
					}
					if !strings.Contains(details.String(), "retained nonsecret diagnostic") || !strings.Contains(details.String(), "[REDACTED]") || !strings.Contains(details.String(), "[SQL REDACTED]") {
						t.Fatal("diagnostics were dropped instead of safely redacted")
					}
				})
			}
		}
	}
}
