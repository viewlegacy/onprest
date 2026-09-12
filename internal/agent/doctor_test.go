package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
)

func TestDoctorCLIOutputExitAndSharedResolverContract(t *testing.T) {
	fake := func(_ context.Context, cfg Config) doctorOutcome {
		if cfg.CapabilityFile != "primary.yaml" {
			t.Fatalf("CapabilityFile=%q, want primary.yaml", cfg.CapabilityFile)
		}
		return doctorOutcome{checks: []doctorCheck{{Stage: "config", Status: "passed", Code: "OK", Message: "configuration is valid"}}}
	}
	var stdout, stderr bytes.Buffer
	if code := handleDoctorCLIWithDoctor(context.Background(), []string{"--config", "primary.yaml", "--capability-file", "alias.yaml", "--format", "json"}, &stdout, &stderr, func(key string) string {
		if key == "AGENT_CAPABILITY_FILE" {
			return "env.yaml"
		}
		return ""
	}, fake, encodeDoctorJSON); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "{\"ok\":true,\"checks\":[{\"stage\":\"config\",\"status\":\"passed\",\"code\":\"OK\",\"message\":\"configuration is valid\"}]}\n" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := handleDoctorCLIWithDoctor(context.Background(), []string{"--config", "primary.yaml"}, &stdout, &stderr, func(string) string { return "" }, fake, encodeDoctorJSON); code != 0 {
		t.Fatalf("default text code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "doctor config: passed (OK) configuration is valid\n" || stderr.Len() != 0 {
		t.Fatalf("default text stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	if code := handleDoctorCLIWithDoctor(context.Background(), nil, &stdout, &stderr, func(string) string { return "" }, func(context.Context, Config) doctorOutcome {
		return doctorOutcome{checks: []doctorCheck{{Stage: "config", Status: "failed", Code: "CONFIG_INVALID", Message: "capability configuration is invalid"}}, err: &preparationError{stage: validationStageConfig, publicMessage: "capability configuration is invalid"}}
	}, encodeDoctorJSON); code != 1 || !strings.Contains(stdout.String(), "doctor config: failed") {
		t.Fatalf("failure code=%d stdout=%q", code, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := handleDoctorCLIWithDoctor(context.Background(), []string{"--format", "yaml"}, &stdout, &stderr, nil, nil, encodeDoctorJSON); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "doctor: invalid arguments") {
		t.Fatalf("usage code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := handleDoctorCLIWithDoctor(context.Background(), []string{"--help"}, &stdout, &stderr, nil, nil, encodeDoctorJSON); code != 0 || !strings.Contains(stdout.String(), "onprest-agent doctor") || stderr.Len() != 0 {
		t.Fatalf("help code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestDoctorConfigurationRunsGatewayStagesWithoutRuntimeActivity(t *testing.T) {
	preflightDriver.reset()
	oldExe := executablePath
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	t.Cleanup(func() { executablePath = oldExe })

	var verifyRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			t.Errorf("request method/body = %s/%v", r.Method, r.Body)
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Errorf("request body=%q", body)
		}
		switch r.URL.Path {
		case "/ws/agent/challenge":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"challenge":"doctor-test-challenge"}`)
		case "/ws/agent/verify":
			verifyRequests++
			for _, header := range []string{"X-Agent-Timestamp", "X-Agent-Nonce", "X-Agent-Challenge", "X-Agent-Signature"} {
				if r.Header.Get(header) == "" {
					t.Errorf("missing %s", header)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	cf := validCapabilityFile()
	cf.Gateway.URL = "ws://" + serverURL.Host + "/ws/agent"
	withPreflightSeams(t, cf)
	outcome := doctorConfiguration(context.Background(), Config{CapabilityFile: "ignored"})
	if outcome.err != nil {
		t.Fatalf("doctor error=%v checks=%#v", outcome.err, outcome.checks)
	}
	if verifyRequests != 1 {
		t.Fatalf("verify requests=%d, want 1", verifyRequests)
	}
	wantStages := []string{"config", "database_open", "database_ping", "capability_explain", "gateway_dns", "gateway_connect", "gateway_tls", "gateway_challenge", "gateway_verify"}
	if len(outcome.checks) != len(wantStages) {
		t.Fatalf("checks=%#v", outcome.checks)
	}
	for i, want := range wantStages {
		if outcome.checks[i].Stage != want || outcome.checks[i].Status == "failed" {
			t.Fatalf("check[%d]=%#v, want passed stage %s", i, outcome.checks[i], want)
		}
	}
	if outcome.checks[6].Status != "skipped" || outcome.checks[6].Code != "NOT_APPLICABLE" {
		t.Fatalf("TLS check=%#v, want explicit skipped", outcome.checks[6])
	}
	if err := outcome.release(); err != nil {
		t.Fatal(err)
	}
	if events := strings.Join(preflightDriver.snapshot(), ","); !strings.Contains(events, "load,open,ping,explain") || strings.Contains(events, "runtime") {
		t.Fatalf("preflight events=%s", events)
	}
	path := filepath.Join(dir, "onprest-agent.doctor.log")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor fixed log after success: err=%v", err)
	}
}

func TestDoctorConfigurationFailureSkipsDependentStagesAndKeepsLatestOnlyLog(t *testing.T) {
	preflightDriver.reset()
	oldExe := executablePath
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	t.Cleanup(func() { executablePath = oldExe })
	validationFixed := filepath.Join(dir, "onprest-agent.validate.log")
	if err := os.WriteFile(validationFixed, []byte("validation-log-must-remain"), 0o600); err != nil {
		t.Fatal(err)
	}

	cf := validCapabilityFile()
	cf.Gateway.URL = "ws://gateway.test:8080/ws/agent"
	withPreflightSeams(t, cf)
	oldLookup := doctorLookupHost
	lookupErr := errors.New("DNS detail must remain local")
	doctorLookupHost = func(context.Context, string) ([]string, error) { return nil, lookupErr }
	t.Cleanup(func() { doctorLookupHost = oldLookup })

	first := doctorConfiguration(context.Background(), Config{CapabilityFile: "ignored"})
	if first.err == nil || first.err.stage != validationStageGatewayDNS {
		t.Fatalf("first outcome=%#v", first.err)
	}
	if err := first.release(); err != nil {
		t.Fatal(err)
	}
	secondErr := errors.New("second DNS detail")
	doctorLookupHost = func(context.Context, string) ([]string, error) { return nil, secondErr }
	second := doctorConfiguration(context.Background(), Config{CapabilityFile: "ignored"})
	if second.err == nil || second.err.stage != validationStageGatewayDNS {
		t.Fatalf("second outcome=%#v", second.err)
	}
	for _, check := range second.checks {
		if check.Stage == "gateway_dns" && check.Status != "failed" {
			t.Fatalf("DNS check=%#v", check)
		}
		if check.Status == "skipped" && check.Code != "SKIPPED" {
			t.Fatalf("skipped check=%#v", check)
		}
	}
	if err := second.release(); err != nil {
		t.Fatal(err)
	}
	fixed := filepath.Join(dir, "onprest-agent.doctor.log")
	content, err := os.ReadFile(fixed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "gateway_dns") || strings.Contains(string(content), "DNS detail must remain local") || !strings.Contains(string(content), "second DNS detail") {
		t.Fatalf("latest doctor log=%q", content)
	}
	if strings.Contains(string(content), "first DNS detail") {
		t.Fatalf("older doctor log was retained: %q", content)
	}
	if got, err := os.ReadFile(validationFixed); err != nil || string(got) != "validation-log-must-remain" {
		t.Fatalf("validate fixed log changed: %q err=%v", got, err)
	}
}

func TestDoctorDatabaseFailureSkipsGatewayAndKeepsDriverDetailPrivate(t *testing.T) {
	preflightDriver.reset()
	oldExe := executablePath
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	t.Cleanup(func() { executablePath = oldExe })
	cf := validCapabilityFile()
	cf.Database.Password = "DB_PASSWORD_SENTINEL"
	preflightDriver.pingErr = errors.New("database driver detail: DB_PASSWORD_SENTINEL")
	withPreflightSeams(t, cf)
	outcome := doctorConfiguration(context.Background(), Config{CapabilityFile: "ignored"})
	if outcome.err == nil || outcome.err.stage != validationStageDatabasePing {
		t.Fatalf("outcome=%#v", outcome.err)
	}
	var pingFailure, gatewayCheck doctorCheck
	for _, check := range outcome.checks {
		if check.Stage == "database_ping" {
			pingFailure = check
		}
		if check.Stage == "gateway_dns" {
			gatewayCheck = check
		}
		if strings.Contains(check.Message, "password") || strings.Contains(check.Message, "DSN") {
			t.Fatalf("driver detail leaked in check=%#v", check)
		}
	}
	if pingFailure.Status != "failed" || pingFailure.Code != "DATABASE_PING_FAILED" || gatewayCheck.Status != "skipped" {
		t.Fatalf("checks=%#v", outcome.checks)
	}
	if err := outcome.release(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "onprest-agent.doctor.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "database_ping") || !strings.Contains(string(content), "[REDACTED]") {
		t.Fatalf("doctor detail=%q", content)
	}
}

func TestDoctorLogCreationFailureIsSafeAndDoesNotOpenDatabase(t *testing.T) {
	preflightDriver.reset()
	oldExe, oldCreate := executablePath, validationCreatePrivateFile
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	validationCreatePrivateFile = func(string) (*os.File, error) { return nil, errors.New("PRIVATE_DOCTOR_LOG_FAILURE") }
	t.Cleanup(func() { executablePath, validationCreatePrivateFile = oldExe, oldCreate })

	loads, opens := 0, 0
	oldLoad, oldOpen := loadCapabilityForPreparation, openDatabaseForPreparation
	loadCapabilityForPreparation = func(string) (*CapabilityFile, error) { loads++; return validCapabilityFile(), nil }
	openDatabaseForPreparation = func(DatabaseDef) (*sql.DB, error) { opens++; return nil, errors.New("database must not open") }
	t.Cleanup(func() { loadCapabilityForPreparation, openDatabaseForPreparation = oldLoad, oldOpen })

	outcome := doctorConfiguration(context.Background(), Config{CapabilityFile: "ignored"})
	if outcome.err == nil || outcome.err.stage != validationStageDetailLog || loads != 1 || opens != 0 {
		t.Fatalf("outcome=%#v loads=%d opens=%d", outcome.err, loads, opens)
	}
	for _, check := range outcome.checks {
		if strings.Contains(check.Message, "PRIVATE_DOCTOR_LOG_FAILURE") {
			t.Fatalf("secret leaked in check=%#v", check)
		}
	}
	if err := outcome.release(); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorGatewayStageDeadlineUsesBoundedContext(t *testing.T) {
	if err := runDoctorStage(context.Background(), func(stageCtx context.Context) error {
		deadline, ok := stageCtx.Deadline()
		if !ok {
			return errors.New("doctor stage has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > doctorStageTimeout {
			return errors.New("doctor stage deadline is outside the 10 second bound")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runDoctorStage(ctx, func(context.Context) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want canceled context", err)
	}
	if doctorStageTimeout != 10*time.Second || doctorOverallTimeout != 60*time.Second {
		t.Fatalf("deadlines changed: stage=%s overall=%s", doctorStageTimeout, doctorOverallTimeout)
	}
}

func TestDoctorTimeoutUsesFailedStageAndSkipsFollowingStages(t *testing.T) {
	completed := []doctorCheck{
		{Stage: "config", Status: "passed", Code: "OK"},
		{Stage: "database_open", Status: "passed", Code: "OK"},
	}
	checks := doctorChecksForFailure(completed, &preparationError{stage: validationStageCanceled, detailErr: context.DeadlineExceeded})
	var failure doctorCheck
	for _, check := range checks {
		if check.Status == "failed" {
			failure = check
			break
		}
	}
	if failure.Stage != "database_ping" || failure.Code != "TIMEOUT" {
		t.Fatalf("failure=%#v checks=%#v", failure, checks)
	}
	for _, check := range checks {
		if check.Stage == "database_ping" {
			continue
		}
		if check.Stage == "gateway_dns" && (check.Status != "skipped" || check.Code != "SKIPPED") {
			t.Fatalf("gateway DNS check=%#v, want skipped", check)
		}
	}
}

func TestDoctorGatewayRejectsInvalidURLBeforeNetwork(t *testing.T) {
	for _, raw := range []string{
		"https://gateway.example.com/ws/agent",
		"ws://gateway.example.com/agent",
		"ws://gateway.example.com/ws/agent?token=secret",
		"ws://user:password@gateway.example.com/ws/agent",
	} {
		if _, err := parseDoctorGatewayTarget(raw); err == nil {
			t.Errorf("parseDoctorGatewayTarget(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestDoctorGatewayTLSUsesSystemTrustAndRejectsUntrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	cf := validCapabilityFile()
	cf.Gateway.URL = "wss://" + server.Listener.Addr().String() + "/ws/agent"
	checks := make([]doctorCheck, 0, len(doctorStages))
	failure := runDoctorGatewayStages(context.Background(), cf, &checks)
	if failure == nil || failure.stage != validationStageGatewayTLS {
		t.Fatalf("failure=%#v checks=%#v, want TLS failure", failure, checks)
	}
	if len(checks) != 2 || checks[0].Stage != string(validationStageGatewayDNS) || checks[1].Stage != string(validationStageGatewayConnect) {
		t.Fatalf("checks=%#v, want DNS/connect before TLS", checks)
	}
}

func TestAgentVerifyAuthHeadersUseDedicatedSignatureWithoutHandshakeKey(t *testing.T) {
	headers := make(http.Header)
	if err := setAgentVerifyAuthHeaders(headers, testAgentPrivateKey, "/ws/agent/verify", "challenge"); err != nil {
		t.Fatal(err)
	}
	if headers.Get("Sec-WebSocket-Key") != "" {
		t.Fatal("doctor verification added a WebSocket handshake key")
	}
	privateKey, err := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(headers.Get("X-Agent-Signature"))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PrivateKey(privateKey).Public().(ed25519.PublicKey), protocol.AgentVerifyAuthMessage("/ws/agent/verify", headers.Get("X-Agent-Timestamp"), headers.Get("X-Agent-Nonce"), "challenge"), signature) {
		t.Fatal("doctor verification signature did not match its dedicated domain")
	}
}
