package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/Pastalikek65/archivebridge/internal/bridge"
	"github.com/Pastalikek65/archivebridge/internal/webui"
)

const cliSchemaVersion = 1

type sourceFlags []string

func (s *sourceFlags) String() string { return strings.Join(*s, ",") }

func (s *sourceFlags) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("source path cannot be empty")
	}
	*s = append(*s, value)
	return nil
}

type commandOutput struct {
	SchemaVersion       int    `json:"schemaVersion"`
	Status              string `json:"status"`
	Command             string `json:"command"`
	Version             string `json:"version,omitempty"`
	Commit              string `json:"commit,omitempty"`
	Plan                any    `json:"plan,omitempty"`
	OutputPath          string `json:"outputPath,omitempty"`
	SelectedSourceCount int    `json:"selectedSourceCount,omitempty"`
	Report              any    `json:"report,omitempty"`
}

type errorOutput struct {
	SchemaVersion int          `json:"schemaVersion"`
	Status        string       `json:"status"`
	Command       string       `json:"command,omitempty"`
	Error         errorDetails `json:"error"`
	Report        any          `json:"report,omitempty"`
}

type errorDetails struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type cliError struct {
	code    string
	message string
	report  any
}

func (e *cliError) Error() string { return e.message }

func usageError(message string) error {
	return &cliError{code: "INVALID_ARGUMENT", message: message}
}

// Run executes one CLI invocation and returns a process exit code. It is kept
// separate from main so commands can be tested without starting a process.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) (exitCode int) {
	if ctx == nil {
		ctx = context.Background()
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	command := commandName(args)
	jsonMode := wantsJSON(args)
	if command == "serve" && ctx.Err() == nil {
		if parsed, err := parseServe(args[1:]); err == nil {
			_, _ = fmt.Fprintf(stderr, "Starting read-only viewer at http://%s (Ctrl+C to stop)\n", parsed.listen)
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err := &cliError{code: "INTERNAL_ERROR", message: "command failed unexpectedly"}
			exitCode = reportFailure(stderr, stdout, jsonMode, command, err)
		}
	}()

	result, err := execute(ctx, args)
	if err != nil {
		return reportFailure(stderr, stdout, jsonMode, command, err)
	}
	if ctx.Err() != nil && command != "version" {
		return reportFailure(stderr, stdout, jsonMode, command, context.Canceled)
	}
	if jsonMode {
		if err := writeJSON(stdout, result); err != nil {
			return reportFailure(stderr, nil, false, command, &cliError{code: "OUTPUT_ERROR", message: "could not write JSON output"})
		}
		return 0
	}
	if err := writeHuman(stdout, result); err != nil {
		return reportFailure(stderr, nil, false, command, &cliError{code: "OUTPUT_ERROR", message: "could not write command output"})
	}
	return 0
}

func safeExit(ctx context.Context, args []string, stdout, stderr io.Writer) (exitCode int) {
	defer func() {
		if recover() != nil {
			exitCode = reportFailure(stderr, stdout, wantsJSON(args), commandName(args), &cliError{code: "INTERNAL_ERROR", message: "command failed unexpectedly"})
		}
	}()
	return Run(ctx, args, stdout, stderr)
}

func execute(ctx context.Context, args []string) (commandOutput, error) {
	if len(args) == 0 {
		return commandOutput{}, usageError("expected a command; use inspect, plan, export, resume, verify, serve, or version")
	}

	if args[0] == "--version" {
		_, err := parseVersionFlags(args[1:])
		if err != nil {
			return commandOutput{}, err
		}
		return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: "version", Version: version, Commit: commit}, nil
	}

	command := args[0]
	if command == "version" {
		_, err := parseVersionFlags(args[1:])
		if err != nil {
			return commandOutput{}, err
		}
		return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: "version", Version: version, Commit: commit}, nil
	}

	if ctx.Err() != nil {
		return commandOutput{}, context.Canceled
	}

	switch command {
	case "inspect":
		flags, err := parseSources("inspect", args[1:], false)
		if err != nil {
			return commandOutput{}, err
		}
		plan, err := bridge.Inspect(ctx, flags.sources, bridge.DefaultLimits())
		if err != nil {
			return commandOutput{}, err
		}
		return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: command, Plan: plan}, nil
	case "plan":
		flags, err := parseSources("plan", args[1:], true)
		if err != nil {
			return commandOutput{}, err
		}
		plan, err := bridge.Inspect(ctx, flags.sources, bridge.DefaultLimits())
		if err != nil {
			return commandOutput{}, err
		}
		if err := bridge.WritePlan(plan, flags.output); err != nil {
			return commandOutput{}, err
		}
		return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: command, Plan: plan, OutputPath: flags.output}, nil
	case "export", "resume":
		flags, err := parsePlanOutput(command, args[1:])
		if err != nil {
			return commandOutput{}, err
		}
		plan, err := bridge.ReadPlan(flags.plan)
		if err != nil {
			return commandOutput{}, err
		}
		report, err := bridge.Export(ctx, plan, flags.output)
		if err != nil {
			return commandOutput{}, err
		}
		if err := checkReportStatus(command, report.Status, report); err != nil {
			return commandOutput{}, err
		}
		return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: command, Report: report, SelectedSourceCount: plan.Stats.SourceCount}, nil
	case "verify":
		flags, err := parseArchive("verify", args[1:])
		if err != nil {
			return commandOutput{}, err
		}
		report, err := bridge.Verify(ctx, flags.archive)
		if err != nil {
			return commandOutput{}, err
		}
		if err := checkReportStatus(command, report.Status, report); err != nil {
			return commandOutput{}, err
		}
		return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: command, Report: report}, nil
	case "serve":
		flags, err := parseServe(args[1:])
		if err != nil {
			return commandOutput{}, err
		}
		if err := webui.Serve(ctx, flags.archive, flags.listen); err != nil {
			return commandOutput{}, err
		}
		return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: command, OutputPath: flags.listen}, nil
	default:
		return commandOutput{}, &cliError{code: "UNKNOWN_COMMAND", message: fmt.Sprintf("unknown command %q", command)}
	}
}

type parsedSources struct {
	sources []string
	output  string
}

func parseSources(command string, args []string, requireOutput bool) (parsedSources, error) {
	fs := newFlagSet(command)
	var sources sourceFlags
	var output string
	fs.Var(&sources, "source", "source archive part (repeatable)")
	if requireOutput {
		fs.StringVar(&output, "output", "", "write inspection plan to this new file")
	}
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "write JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return parsedSources{}, usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return parsedSources{}, usageError("unexpected positional argument: " + fs.Arg(0))
	}
	if len(sources) == 0 {
		return parsedSources{}, usageError("at least one --source is required")
	}
	if requireOutput && strings.TrimSpace(output) == "" {
		return parsedSources{}, usageError("--output is required")
	}
	return parsedSources{sources: []string(sources), output: output}, nil
}

type parsedPlanOutput struct{ plan, output string }

func parsePlanOutput(command string, args []string) (parsedPlanOutput, error) {
	fs := newFlagSet(command)
	var parsed parsedPlanOutput
	fs.StringVar(&parsed.plan, "plan", "", "inspection plan file")
	fs.StringVar(&parsed.output, "out", "", "archive output directory")
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "write JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return parsedPlanOutput{}, usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return parsedPlanOutput{}, usageError("unexpected positional argument: " + fs.Arg(0))
	}
	if strings.TrimSpace(parsed.plan) == "" {
		return parsedPlanOutput{}, usageError("--plan is required")
	}
	if strings.TrimSpace(parsed.output) == "" {
		return parsedPlanOutput{}, usageError("--out is required")
	}
	return parsed, nil
}

type parsedArchive struct{ archive string }

func parseArchive(command string, args []string) (parsedArchive, error) {
	fs := newFlagSet(command)
	var parsed parsedArchive
	fs.StringVar(&parsed.archive, "archive", "", "portable archive directory")
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "write JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return parsedArchive{}, usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return parsedArchive{}, usageError("unexpected positional argument: " + fs.Arg(0))
	}
	if strings.TrimSpace(parsed.archive) == "" {
		return parsedArchive{}, usageError("--archive is required")
	}
	return parsed, nil
}

type parsedServe struct{ archive, listen string }

func parseServe(args []string) (parsedServe, error) {
	fs := newFlagSet("serve")
	var parsed parsedServe
	fs.StringVar(&parsed.archive, "archive", "", "portable archive directory")
	fs.StringVar(&parsed.listen, "listen", "127.0.0.1:4175", "localhost listen address")
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "write JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return parsedServe{}, usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return parsedServe{}, usageError("unexpected positional argument: " + fs.Arg(0))
	}
	if strings.TrimSpace(parsed.archive) == "" {
		return parsedServe{}, usageError("--archive is required")
	}
	if strings.TrimSpace(parsed.listen) == "" {
		return parsedServe{}, usageError("--listen cannot be empty")
	}
	if normalized, ok := normalizeLocalListenAddress(parsed.listen); !ok {
		return parsedServe{}, usageError("--listen must use localhost, 127.0.0.1, or [::1]")
	} else {
		parsed.listen = normalized
	}
	return parsed, nil
}

func isLocalListenAddress(address string) bool {
	_, ok := normalizeLocalListenAddress(address)
	return ok
}

func normalizeLocalListenAddress(address string) (string, bool) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return "", false
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", false
	}
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return "", false
		}
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	} else {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", false
		}
	}
	return net.JoinHostPort(host, port), true
}

func parseVersionFlags(args []string) (bool, error) {
	fs := newFlagSet("version")
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "write JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return false, usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return false, usageError("unexpected positional argument: " + fs.Arg(0))
	}
	return asJSON, nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("archivebridge "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func checkReportStatus(command, status string, report any) error {
	expected := ""
	switch command {
	case "export", "resume":
		expected = "complete"
	case "verify":
		expected = "ok"
	}
	if status == expected {
		return nil
	}
	if status == "failed" || status == "incomplete" || status == "error" {
		return &cliError{code: "REPORT_FAILED", message: fmt.Sprintf("%s reported status %q", command, status), report: report}
	}
	return &cliError{code: "UNKNOWN_STATUS", message: fmt.Sprintf("%s returned unrecognized status %q", command, status), report: report}
}

func commandName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if args[0] == "--version" {
		return "version"
	}
	return args[0]
}

func wantsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || arg == "--json=true" {
			return true
		}
	}
	return false
}

func reportFailure(stderr, stdout io.Writer, jsonMode bool, command string, err error) int {
	code, message := errorCode(err), err.Error()
	status := "error"
	exitCode := 1
	if code == "INVALID_ARGUMENT" || code == "UNKNOWN_COMMAND" {
		exitCode = 2
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code, message, status, exitCode = "CANCELLED", "operation cancelled", "cancelled", 130
	}
	if stderr != nil {
		_, _ = fmt.Fprintf(stderr, "archivebridge: %s: %s\n", code, message)
		if !jsonMode {
			var cliErr *cliError
			if errors.As(err, &cliErr) && cliErr.report != nil {
				_ = writeHuman(stderr, commandOutput{Command: command, Report: cliErr.report})
			}
		}
	}
	if jsonMode && stdout != nil {
		var report any
		var cliErr *cliError
		if errors.As(err, &cliErr) {
			report = cliErr.report
		}
		payload := errorOutput{SchemaVersion: cliSchemaVersion, Status: status, Command: command, Error: errorDetails{Code: code, Message: message}, Report: report}
		_ = writeJSON(stdout, payload)
	}
	return exitCode
}

func errorCode(err error) string {
	var cliErr *cliError
	if errors.As(err, &cliErr) {
		return cliErr.code
	}
	return "OPERATION_FAILED"
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeHuman(w io.Writer, result commandOutput) error {
	switch result.Command {
	case "version":
		_, err := fmt.Fprintf(w, "ArchiveBridge %s (%s)\n", result.Version, result.Commit)
		return err
	case "inspect":
		return writePlanSummary(w, "Inspection", result.Plan.(*bridge.Plan), "")
	case "plan":
		return writePlanSummary(w, "Inspection", result.Plan.(*bridge.Plan), "Plan saved to "+result.OutputPath)
	case "export", "resume":
		report := result.Report.(*bridge.ExportReport)
		if _, err := fmt.Fprintf(w, "Export status: %s\nSelected source parts: %d\nFiles written: %d\nFiles reused: %d\nBytes written: %d\nUnresolved issues: %d\n", report.Status, result.SelectedSourceCount, report.FilesWritten, report.FilesReused, report.BytesWritten, len(report.Issues)); err != nil {
			return err
		}
		if err := writeIssueDetails(w, report.Issues); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w, "Scope is limited to the selected source parts in the plan; this does not establish whole-account coverage.")
		return err
	case "verify":
		report := result.Report.(*bridge.VerifyReport)
		if _, err := fmt.Fprintf(w, "Verification status: %s\nMedia files checked: %d\nSidecars checked: %d\nBytes checked: %d\nUnresolved issues: %d\n", report.Status, report.FilesChecked, report.SidecarsChecked, report.BytesChecked, len(report.Issues)); err != nil {
			return err
		}
		if err := writeIssueDetails(w, report.Issues); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w, "Verification covers the files represented by this archive manifest; it does not establish whole-account coverage.")
		return err
	case "serve":
		_, err := fmt.Fprintf(w, "ArchiveBridge read-only viewer stopped at %s\n", result.OutputPath)
		return err
	default:
		return errors.New("no human renderer for command")
	}
}

func writePlanSummary(w io.Writer, heading string, plan *bridge.Plan, saved string) error {
	if _, err := fmt.Fprintf(w, "%s scope: %d selected source parts\nMedia occurrences: %d (%d bytes)\nSidecars preserved: %d (%d bytes)\nAlbums: %d\nUnresolved issues: %d\n", heading, plan.Stats.SourceCount, plan.Stats.MediaCount, plan.Stats.MediaBytes, plan.Stats.SidecarCount, plan.Stats.SidecarBytes, plan.Stats.AlbumCount, plan.Stats.IssueCount); err != nil {
		return err
	}
	if saved != "" {
		if _, err := fmt.Fprintln(w, saved); err != nil {
			return err
		}
	}
	if err := writeIssueDetails(w, plan.Issues); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "This result covers only the selected source parts; it does not establish whole-account coverage.")
	return err
}

func writeIssueDetails(w io.Writer, issues []bridge.Issue) error {
	if len(issues) == 0 {
		_, err := fmt.Fprintln(w, "No unresolved issues were recorded for the selected sources.")
		return err
	}
	if _, err := fmt.Fprintln(w, "Review these unresolved items:"); err != nil {
		return err
	}
	for _, issue := range issues {
		location := ""
		if issue.SourceIndex >= 0 {
			location = fmt.Sprintf("source %d", issue.SourceIndex+1)
		}
		if issue.EntryPath != "" {
			if location != "" {
				location += " "
			}
			location += issue.EntryPath
		}
		if location != "" {
			location += ": "
		}
		if _, err := fmt.Fprintf(w, "- [%s] %s%s\n", issue.Code, location, issue.Details); err != nil {
			return err
		}
	}
	return nil
}
