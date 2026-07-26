package agent

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
)

func HandleCLI(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "service":
		return true, handleServiceCLI(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		printAgentUsage(stdout)
		return true, 0
	}
	return false, 0
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
	fmt.Fprintln(w, "  onprest-agent [--config PATH]                  start agent")
	fmt.Fprintln(w, "  onprest-agent service install [--config PATH]  install OS service")
	fmt.Fprintln(w, "  onprest-agent service start                    start OS service")
	fmt.Fprintln(w, "  onprest-agent service stop                     stop OS service")
	fmt.Fprintln(w, "  onprest-agent service status                   show OS service status")
	fmt.Fprintln(w, "  onprest-agent service uninstall                remove OS service")
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
