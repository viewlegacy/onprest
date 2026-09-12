package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	doctorOverallTimeout = 60 * time.Second
	doctorStageTimeout   = 10 * time.Second
)

type doctorCheck struct {
	Stage   string `json:"stage"`
	Status  string `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type doctorJSONOutput struct {
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

type doctorOutcome struct {
	checks  []doctorCheck
	err     *preparationError
	release func() error
}

type doctorFunction func(context.Context, Config) doctorOutcome

// The stage list intentionally keeps the database preparation stages separate
// from the network stages. The normal Runner still uses prepareAgent without
// this doctor-only network extension.
var doctorStages = []validationStage{
	validationStageConfig,
	validationStageDatabaseOpen,
	validationStageDatabasePing,
	validationStageCapabilityExplain,
	validationStageGatewayDNS,
	validationStageGatewayConnect,
	validationStageGatewayTLS,
	validationStageGatewayChallenge,
	validationStageGatewayVerify,
}

var (
	doctorLookupHost = func(ctx context.Context, host string) ([]string, error) {
		return net.DefaultResolver.LookupHost(ctx, host)
	}
	doctorDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	doctorHTTPClientFactory = newDoctorHTTPClient
)

func handleDoctorCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return handleDoctorCLIWithDoctor(ctx, args, stdout, stderr, os.Getenv, doctorConfiguration, encodeDoctorJSON)
}

type doctorJSONEncoder func(io.Writer, doctorJSONOutput) error

func encodeDoctorJSON(w io.Writer, value doctorJSONOutput) error {
	return json.NewEncoder(w).Encode(value)
}

func handleDoctorCLIWithDoctor(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) string, doctor doctorFunction, encodeJSON doctorJSONEncoder) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configFile := fs.String("config", "", "path to capability YAML file")
	capabilityFile := fs.String("capability-file", "", "alias for --config")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		printDoctorUsage(stdout)
		return 0
	} else if err != nil || fs.NArg() != 0 || (*format != "text" && *format != "json") {
		fmt.Fprintln(stderr, "doctor: invalid arguments")
		printDoctorUsage(stderr)
		return 2
	}
	cfg := Config{CapabilityFile: resolveCapabilityFile(*configFile, *capabilityFile, lookupEnv), ReconnectEvery: 30 * time.Second}
	outcome := doctor(ctx, cfg)
	if outcome.release == nil {
		outcome.release = func() error { return nil }
	}
	code := 0
	if outcome.err != nil {
		code = 1
	}
	if err := writeDoctorOutput(*format, outcome, stdout, encodeJSON); err != nil {
		_ = checkedWrite(stderr, []byte("doctor: output failed\n"))
		code = 1
	}
	_ = outcome.release()
	return code
}

func writeDoctorOutput(format string, outcome doctorOutcome, stdout io.Writer, encodeJSON doctorJSONEncoder) error {
	checks := outcome.checks
	if checks == nil {
		checks = []doctorCheck{}
	}
	if format == "json" {
		var encoded strings.Builder
		if err := encodeJSON(&encoded, doctorJSONOutput{OK: outcome.err == nil, Checks: checks}); err != nil {
			return err
		}
		return checkedWrite(stdout, []byte(encoded.String()))
	}
	for _, check := range checks {
		line := fmt.Sprintf("doctor %s: %s (%s) %s\n", check.Stage, check.Status, check.Code, check.Message)
		if err := checkedWrite(stdout, []byte(line)); err != nil {
			return err
		}
	}
	return nil
}

func doctorConfiguration(ctx context.Context, cfg Config) doctorOutcome {
	session, err := newDoctorLogSession()
	if err != nil {
		stage, message := validationStageDetailLog, doctorDiagnosticFailureMessage
		if errors.Is(err, errValidationBusy) {
			stage, message = validationStageBusy, "another doctor is already running"
		}
		pe := &preparationError{stage: stage, publicMessage: message, detailErr: err}
		return doctorOutcome{checks: doctorChecksForFailure(nil, pe), err: pe, release: func() error { return nil }}
	}
	outcome := doctorOutcome{release: session.Close}
	checks := make([]doctorCheck, 0, len(doctorStages))
	observe := func(stage validationStage) {
		checks = append(checks, doctorCheck{Stage: string(stage), Status: "passed", Code: "OK", Message: doctorPassedMessage(stage)})
	}
	doctorCtx, cancel := context.WithTimeout(ctx, doctorOverallTimeout)
	defer cancel()
	prepared, err := prepareAgentForDoctor(doctorCtx, cfg, session.NewDetailLog, observe)
	if err != nil {
		pe := asPreparationError(err, "doctor could not complete")
		outcome.err = pe
		outcome.checks = doctorChecksForFailure(checks, pe)
		return outcome
	}

	if failure := runDoctorGatewayStages(doctorCtx, prepared.cf, &checks); failure != nil {
		if err := prepared.recordFailureWithMessage(failure, doctorDiagnosticFailureMessage); err != nil {
			failure = asPreparationError(err, doctorDiagnosticFailureMessage)
		}
		outcome.err = failure
		outcome.checks = doctorChecksForFailure(checks, failure)
		return outcome
	}
	if err := prepared.finishDoctorSuccess(); err != nil {
		pe := asPreparationError(err, doctorCleanupFailureMessage)
		outcome.err = pe
		outcome.checks = doctorChecksForFailure(checks, pe)
		return outcome
	}
	outcome.checks = checks
	return outcome
}

func asPreparationError(err error, fallback string) *preparationError {
	if pe, ok := err.(*preparationError); ok {
		return pe
	}
	return &preparationError{stage: validationStageInternal, publicMessage: fallback, detailErr: err}
}

func doctorChecksForFailure(passed []doctorCheck, pe *preparationError) []doctorCheck {
	checks := append([]doctorCheck(nil), passed...)
	failureStage := doctorFailureStage(pe, checks)
	if !doctorHasStage(checks, string(failureStage)) {
		checks = append(checks, doctorFailureCheck(pe, failureStage))
	}
	failureIndex := doctorStageIndex(failureStage)
	if failureIndex >= 0 {
		for _, stage := range doctorStages[failureIndex+1:] {
			if !doctorHasStage(checks, string(stage)) {
				checks = append(checks, doctorCheck{Stage: string(stage), Status: "skipped", Code: "SKIPPED", Message: "not run because a dependent stage failed"})
			}
		}
	} else {
		for _, stage := range doctorStages {
			if !doctorHasStage(checks, string(stage)) {
				checks = append(checks, doctorCheck{Stage: string(stage), Status: "skipped", Code: "SKIPPED", Message: "not run because doctor did not complete"})
			}
		}
	}
	return checks
}

func doctorFailureStage(pe *preparationError, completed []doctorCheck) validationStage {
	if pe == nil {
		return validationStageInternal
	}
	switch pe.stage {
	case validationStageConfig, validationStageDatabaseOpen, validationStageDatabasePing, validationStageCapabilityExplain,
		validationStageGatewayDNS, validationStageGatewayConnect, validationStageGatewayTLS, validationStageGatewayChallenge, validationStageGatewayVerify,
		validationStageBusy, validationStageDetailLog, validationStageInternal:
		return pe.stage
	case validationStageCanceled:
		for _, stage := range doctorStages {
			if !doctorHasStage(completed, string(stage)) {
				return stage
			}
		}
		return validationStageInternal
	default:
		return validationStageInternal
	}
}

func doctorStageIndex(stage validationStage) int {
	for i, candidate := range doctorStages {
		if candidate == stage {
			return i
		}
	}
	return -1
}

func doctorHasStage(checks []doctorCheck, stage string) bool {
	for _, check := range checks {
		if check.Stage == stage {
			return true
		}
	}
	return false
}

func doctorFailureCheck(pe *preparationError, stage validationStage) doctorCheck {
	code, message := doctorErrorContract(stage)
	if pe != nil && (pe.stage == validationStageCanceled || errors.Is(pe.detailErr, context.Canceled) || errors.Is(pe.detailErr, context.DeadlineExceeded)) {
		code, message = "TIMEOUT", "doctor timed out or was canceled"
	}
	if pe != nil && pe.detailLogPath != "" && stage != validationStageConfig && stage != validationStageBusy {
		message += "; see doctor detail log"
	}
	if pe != nil && pe.cleanupPath != "" {
		message += "; doctor diagnostic cleanup is incomplete"
	}
	return doctorCheck{Stage: string(stage), Status: "failed", Code: code, Message: message}
}

func doctorPassedMessage(stage validationStage) string {
	switch stage {
	case validationStageConfig:
		return "configuration is valid"
	case validationStageDatabaseOpen:
		return "database opened"
	case validationStageDatabasePing:
		return "database is reachable"
	case validationStageCapabilityExplain:
		return "capability EXPLAIN checks passed"
	default:
		return "stage passed"
	}
}

func doctorErrorContract(stage validationStage) (string, string) {
	switch stage {
	case validationStageConfig:
		return "CONFIG_INVALID", "capability configuration is invalid"
	case validationStageBusy:
		return "BUSY", "another doctor is already running"
	case validationStageDetailLog:
		return "DOCTOR_LOG_FAILED", doctorDiagnosticFailureMessage
	case validationStageDatabaseOpen:
		return "DATABASE_OPEN_FAILED", "database could not be opened"
	case validationStageDatabasePing:
		return "DATABASE_PING_FAILED", "database is unreachable"
	case validationStageCapabilityExplain:
		return "CAPABILITY_EXPLAIN_FAILED", "capability EXPLAIN validation failed"
	case validationStageGatewayDNS:
		return "GATEWAY_DNS_FAILED", "gateway DNS lookup failed"
	case validationStageGatewayConnect:
		return "GATEWAY_CONNECT_FAILED", "gateway TCP connection failed"
	case validationStageGatewayTLS:
		return "GATEWAY_TLS_FAILED", "gateway TLS verification failed"
	case validationStageGatewayChallenge:
		return "GATEWAY_CHALLENGE_FAILED", "gateway challenge failed"
	case validationStageGatewayVerify:
		return "GATEWAY_VERIFY_FAILED", "gateway verification failed"
	case validationStageCanceled:
		return "TIMEOUT", "doctor timed out or was canceled"
	default:
		return "DOCTOR_INTERNAL", "doctor could not complete"
	}
}

type doctorGatewayEndpoint struct {
	url        *url.URL
	host       string
	address    string
	serverName string
}

func parseDoctorGatewayTarget(raw string) (doctorGatewayEndpoint, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "ws" && u.Scheme != "wss") || u.Path != "/ws/agent" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return doctorGatewayEndpoint{}, errors.New("invalid gateway URL")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return doctorGatewayEndpoint{url: u, host: u.Hostname(), address: net.JoinHostPort(u.Hostname(), port), serverName: u.Hostname()}, nil
}

func runDoctorGatewayStages(ctx context.Context, cf *CapabilityFile, checks *[]doctorCheck) *preparationError {
	target, err := parseDoctorGatewayTarget(cf.Gateway.URL)
	if err != nil {
		return doctorGatewayFailure(validationStageGatewayDNS, err)
	}
	if err := runDoctorStage(ctx, func(stageCtx context.Context) error {
		if net.ParseIP(target.host) != nil {
			return nil
		}
		addresses, err := doctorLookupHost(stageCtx, target.host)
		if err != nil {
			return err
		}
		if len(addresses) == 0 {
			return errors.New("gateway host resolved to no addresses")
		}
		return nil
	}); err != nil {
		return doctorGatewayFailure(validationStageGatewayDNS, err)
	}
	*checks = append(*checks, doctorCheck{Stage: string(validationStageGatewayDNS), Status: "passed", Code: "OK", Message: "gateway host resolved"})

	if err := runDoctorStage(ctx, func(stageCtx context.Context) error {
		conn, err := doctorDialContext(stageCtx, "tcp", target.address)
		if err != nil {
			return err
		}
		return conn.Close()
	}); err != nil {
		return doctorGatewayFailure(validationStageGatewayConnect, err)
	}
	*checks = append(*checks, doctorCheck{Stage: string(validationStageGatewayConnect), Status: "passed", Code: "OK", Message: "gateway TCP connection succeeded"})

	if target.url.Scheme == "ws" {
		*checks = append(*checks, doctorCheck{Stage: string(validationStageGatewayTLS), Status: "skipped", Code: "NOT_APPLICABLE", Message: "TLS is not used for ws://"})
	} else {
		if err := runDoctorStage(ctx, func(stageCtx context.Context) error {
			conn, err := doctorDialContext(stageCtx, "tcp", target.address)
			if err != nil {
				return err
			}
			defer conn.Close()
			tlsConn := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: target.serverName})
			defer tlsConn.Close()
			return tlsConn.HandshakeContext(stageCtx)
		}); err != nil {
			return doctorGatewayFailure(validationStageGatewayTLS, err)
		}
		*checks = append(*checks, doctorCheck{Stage: string(validationStageGatewayTLS), Status: "passed", Code: "OK", Message: "gateway TLS certificate and hostname verified"})
	}

	if doctorHTTPClientFactory == nil {
		return doctorGatewayFailure(validationStageGatewayChallenge, errors.New("doctor HTTP client unavailable"))
	}
	client := doctorHTTPClientFactory(target.url)
	if client == nil {
		return doctorGatewayFailure(validationStageGatewayChallenge, errors.New("doctor HTTP client unavailable"))
	}
	if transport, ok := client.Transport.(*http.Transport); ok {
		defer transport.CloseIdleConnections()
	}
	var challenge string
	if err := runDoctorStage(ctx, func(stageCtx context.Context) error {
		var err error
		challenge, err = fetchAgentChallengeWithClient(stageCtx, target.url.String(), client)
		return err
	}); err != nil {
		return doctorGatewayFailure(validationStageGatewayChallenge, err)
	}
	*checks = append(*checks, doctorCheck{Stage: string(validationStageGatewayChallenge), Status: "passed", Code: "OK", Message: "gateway issued a challenge"})

	if err := runDoctorStage(ctx, func(stageCtx context.Context) error {
		return postAgentVerify(stageCtx, target.url.String(), cf.Gateway.AgentPrivateKey, challenge, client)
	}); err != nil {
		return doctorGatewayFailure(validationStageGatewayVerify, err)
	}
	*checks = append(*checks, doctorCheck{Stage: string(validationStageGatewayVerify), Status: "passed", Code: "OK", Message: "gateway signature verification succeeded"})
	return nil
}

func doctorGatewayFailure(stage validationStage, err error) *preparationError {
	_, message := doctorErrorContract(stage)
	return &preparationError{stage: stage, publicMessage: message, detailErr: err}
}

func runDoctorStage(parent context.Context, fn func(context.Context) error) error {
	if err := parent.Err(); err != nil {
		return err
	}
	stageCtx, cancel := context.WithTimeout(parent, doctorStageTimeout)
	defer cancel()
	err := fn(stageCtx)
	if stageErr := stageCtx.Err(); stageErr != nil {
		return stageErr
	}
	return err
}

func newDoctorHTTPClient(u *url.URL) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: u.Hostname(),
		},
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func postAgentVerify(ctx context.Context, gatewayURL, privateKey, challenge string, client *http.Client) error {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return fmt.Errorf("unsupported gateway URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/verify"
	u.RawQuery = ""
	u.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	if err := setAgentVerifyAuthHeaders(req.Header, privateKey, "/ws/agent/verify", challenge); err != nil {
		return err
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAgentChallengeResponseBytes))
		return fmt.Errorf("gateway verify returned status %d", resp.StatusCode)
	}
	var body struct {
		OK bool `json:"ok"`
	}
	if err := decodeBoundedAgentJSON(resp.Body, maxAgentChallengeResponseBytes, &body); err != nil {
		return errors.New("gateway verify response is invalid")
	}
	if !body.OK {
		return errors.New("gateway verify response was not successful")
	}
	return nil
}
