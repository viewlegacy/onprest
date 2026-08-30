package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func HandleCLI(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "validate":
		return true, handleValidateCLI(ctx, args[1:], stdout, stderr)
	case "service":
		return true, handleServiceCLI(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		printAgentUsage(stdout)
		return true, 0
	}
	if startupArguments(args) {
		return false, 0
	}
	fmt.Fprintln(stderr, "onprest-agent: invalid command or arguments")
	printAgentUsage(stderr)
	return true, 2
}

func startupArguments(args []string) bool {
	fs := flag.NewFlagSet("onprest-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("config", "", "")
	fs.String("capability-file", "", "")
	return fs.Parse(args) == nil && fs.NArg() == 0
}

type validationOutcome struct {
	report  ValidationReport
	err     *preparationError
	release func() error
}

type validationFunction func(context.Context, Config) validationOutcome

func validateConfiguration(ctx context.Context, cfg Config) validationOutcome {
	session, err := newValidationLogSession()
	if err != nil {
		stage, message := validationStageDetailLog, validationDiagnosticFailureMessage
		if err == errValidationBusy {
			stage, message = validationStageBusy, "another validation is already running"
		}
		return validationOutcome{err: &preparationError{stage: stage, publicMessage: message}, release: func() error { return nil }}
	}
	outcome := validationOutcome{release: session.Close}
	prepared, err := prepareAgent(ctx, cfg, session.NewDetailLog)
	if err != nil {
		if pe, ok := err.(*preparationError); ok {
			outcome.err = pe
		} else {
			outcome.err = &preparationError{stage: validationStageInternal, publicMessage: "validation could not complete"}
		}
		return outcome
	}
	outcome.report = ValidationReport{DatabaseDriver: prepared.cf.Database.Driver, Capabilities: len(prepared.cf.Capabilities)}
	if err := prepared.finishValidationSuccess(); err != nil {
		if pe, ok := err.(*preparationError); ok {
			outcome.err = pe
		} else {
			outcome.err = &preparationError{stage: validationStageInternal, publicMessage: "validation could not complete"}
		}
	}
	return outcome
}

func handleValidateCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return handleValidateCLIWithValidator(ctx, args, stdout, stderr, os.Getenv, validateConfiguration)
}

func handleValidateCLIWithValidator(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) string, validate validationFunction) int {
	return handleValidateCLIWithJSONEncoder(ctx, args, stdout, stderr, lookupEnv, validate, encodeValidationJSON)
}

type validationJSONEncoder func(io.Writer, validationJSONOutput) error

func encodeValidationJSON(w io.Writer, value validationJSONOutput) error {
	return json.NewEncoder(w).Encode(value)
}

func handleValidateCLIWithJSONEncoder(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) string, validate validationFunction, encodeJSON validationJSONEncoder) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configFile := fs.String("config", "", "path to capability YAML file")
	capabilityFile := fs.String("capability-file", "", "alias for --config")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		printValidateUsage(stdout)
		return 0
	} else if err != nil || fs.NArg() != 0 || (*format != "text" && *format != "json") {
		fmt.Fprintln(stderr, "validate: invalid arguments")
		printValidateUsage(stderr)
		return 2
	}
	cfg := Config{CapabilityFile: resolveCapabilityFile(*configFile, *capabilityFile, lookupEnv), ReconnectEvery: 30 * 1e9}
	outcome := validate(ctx, cfg)
	if outcome.release == nil {
		outcome.release = func() error { return nil }
	}
	code := 0
	if outcome.err != nil {
		code = 1
	}
	writeErr := writeValidationOutput(*format, outcome, stdout, stderr, encodeJSON)
	if writeErr != nil {
		_ = checkedWrite(stderr, []byte("validate: output failed\n"))
		code = 1
	}
	_ = outcome.release()
	return code
}

type validationJSONOutput struct {
	Valid          bool            `json:"valid"`
	DatabaseDriver string          `json:"database_driver,omitempty"`
	Capabilities   *int            `json:"capabilities,omitempty"`
	Stage          validationStage `json:"stage,omitempty"`
	Capability     string          `json:"capability,omitempty"`
	Message        string          `json:"message,omitempty"`
	DetailLog      string          `json:"detail_log,omitempty"`
	CleanupPath    string          `json:"cleanup_path,omitempty"`
}

func writeValidationOutput(format string, outcome validationOutcome, stdout, stderr io.Writer, encodeJSON validationJSONEncoder) error {
	if format == "json" {
		result := validationJSONOutput{Valid: outcome.err == nil}
		if outcome.err == nil {
			count := outcome.report.Capabilities
			result.DatabaseDriver, result.Capabilities = outcome.report.DatabaseDriver, &count
		} else {
			result.Stage, result.Capability, result.Message = outcome.err.stage, outcome.err.capability, outcome.err.publicMessage
			result.DetailLog, result.CleanupPath = outcome.err.detailLogPath, outcome.err.cleanupPath
		}
		var encoded bytes.Buffer
		if err := encodeJSON(&encoded, result); err != nil {
			return err
		}
		return checkedWrite(stdout, encoded.Bytes())
	}
	if outcome.err == nil {
		return checkedWrite(stdout, []byte(fmt.Sprintf("validation succeeded: driver=%s capabilities=%d\n", outcome.report.DatabaseDriver, outcome.report.Capabilities)))
	}
	message := outcome.err.publicMessage
	if outcome.err.stage == validationStageCapabilityExplain && outcome.err.capability != "" {
		message = fmt.Sprintf("capability %q EXPLAIN validation failed", outcome.err.capability)
	}
	if outcome.err.detailLogPath != "" {
		message += "; see validation detail log: " + outcome.err.detailLogPath
	}
	if outcome.err.cleanupPath != "" {
		message += "; remove incomplete temporary file after validation exits: " + outcome.err.cleanupPath
	}
	return checkedWrite(stderr, []byte("validate: "+message+"\n"))
}

func checkedWrite(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		return io.ErrShortWrite
	}
	return err
}

func handleServiceCLI(args []string, stdout, stderr io.Writer) int {
	return handleServiceCLIWithFactory(args, stdout, stderr, newServiceManager)
}

func handleServiceCLIWithFactory(args []string, stdout, stderr io.Writer, factory func(ServiceOptions) serviceManager) int {
	if len(args) == 0 {
		printServiceUsage(stderr)
		return 2
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("service install", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		configFile := fs.String("config", "", "path to capability YAML file")
		if err := fs.Parse(args[1:]); err != nil {
			fmt.Fprintf(stderr, "service install: %v\n", err)
			return 2
		}
		if fs.NArg() > 0 {
			fmt.Fprintf(stderr, "service install: unexpected argument %q\n", fs.Arg(0))
			return 2
		}
		opts, err := defaultServiceOptions(*configFile)
		if err != nil {
			fmt.Fprintf(stderr, "service install: %v\n", err)
			return 1
		}
		if err := factory(opts).Install(); err != nil {
			fmt.Fprintf(stderr, "service install: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "installed %s\n", opts.Name)
		return 0
	case "start":
		if rejectServiceArgs("service start", args[1:], stderr) {
			return 2
		}
		if err := factory(defaultServiceIdentity()).Start(); err != nil {
			fmt.Fprintf(stderr, "service start: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "started onprest-agent")
		return 0
	case "stop":
		if rejectServiceArgs("service stop", args[1:], stderr) {
			return 2
		}
		if err := factory(defaultServiceIdentity()).Stop(); err != nil {
			fmt.Fprintf(stderr, "service stop: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "stopped onprest-agent")
		return 0
	case "status":
		if rejectServiceArgs("service status", args[1:], stderr) {
			return 2
		}
		status, err := factory(defaultServiceIdentity()).Status()
		if err != nil {
			fmt.Fprintf(stderr, "service status: %v\n", err)
			return 1
		}
		printServiceStatus(stdout, status)
		return 0
	case "uninstall", "remove":
		if rejectServiceArgs("service uninstall", args[1:], stderr) {
			return 2
		}
		if err := factory(defaultServiceIdentity()).Uninstall(); err != nil {
			fmt.Fprintf(stderr, "service uninstall: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "uninstalled onprest-agent")
		return 0
	case "help", "--help", "-h":
		printServiceUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "service: unknown command %q\n", args[0])
		printServiceUsage(stderr)
		return 2
	}
}

func rejectServiceArgs(command string, args []string, stderr io.Writer) bool {
	if len(args) == 0 {
		return false
	}
	fmt.Fprintf(stderr, "%s: unexpected argument %q\n", command, args[0])
	return true
}

func printAgentUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  onprest-agent [--config PATH] [--capability-file PATH]  start agent")
	fmt.Fprintln(w, "  onprest-agent validate [--config PATH] [--format text|json]")
	fmt.Fprintln(w, "  onprest-agent service install [--config PATH]  install OS service")
	fmt.Fprintln(w, "  onprest-agent service start                    start OS service")
	fmt.Fprintln(w, "  onprest-agent service stop                     stop OS service")
	fmt.Fprintln(w, "  onprest-agent service status                   show OS service status")
	fmt.Fprintln(w, "  onprest-agent service uninstall                remove OS service")
}

func printValidateUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  onprest-agent validate [--config PATH] [--capability-file PATH] [--format text|json]")
}

func printServiceUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  onprest-agent service install [--config PATH]")
	fmt.Fprintln(w, "  onprest-agent service start")
	fmt.Fprintln(w, "  onprest-agent service stop")
	fmt.Fprintln(w, "  onprest-agent service status")
	fmt.Fprintln(w, "  onprest-agent service uninstall")
}

func absPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("--config is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}
