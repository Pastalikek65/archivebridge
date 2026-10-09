package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Pastalikek65/archivebridge/internal/immich"
)

const (
	defaultImmichAPIKeyEnv = "ARCHIVEBRIDGE_IMMICH_API_KEY"
	defaultImmichTimeout   = 30 * time.Minute
	maxImmichTimeout       = 24 * time.Hour
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type immichPlanArgs struct {
	archive        string
	skipUnresolved bool
}

type immichRemoteArgs struct {
	archive           string
	server            string
	apiKeyEnv         string
	reportPath        string
	outputPath        string
	resumePath        string
	skipUnresolved    bool
	allowLoopbackHTTP bool
	timeout           time.Duration
}

func executeImmich(ctx context.Context, args []string) (commandOutput, error) {
	if len(args) == 0 {
		return commandOutput{}, usageError("expected immich plan, import, or verify")
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: "immich.help", HelpText: usageForImmich()}, nil
	}
	if len(args) > 1 && hasHelpFlag(args[1:]) {
		return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: "immich." + args[0] + ".help", HelpText: helpForImmichSubcommand(args[0])}, nil
	}
	switch args[0] {
	case "plan":
		parsed, err := parseImmichPlan(args[1:])
		if err != nil {
			return commandOutput{}, err
		}
		report, err := immich.Plan(ctx, parsed.archive, parsed.skipUnresolved)
		if err != nil {
			return commandOutput{}, wrapImmichError(err, nil)
		}
		if report == nil {
			return commandOutput{}, &cliError{code: "IMMICH_INVALID_REPORT", message: "Immich planning returned no report."}
		}
		switch report.Status {
		case "ready", "ready_with_skips":
			return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: "immich.plan", Report: report}, nil
		case "blocked":
			return commandOutput{}, &cliError{code: "IMMICH_PLAN_BLOCKED", message: "the selected archive has unresolved items; review the plan or explicitly use --skip-unresolved", report: report}
		default:
			return commandOutput{}, &cliError{code: "IMMICH_UNKNOWN_STATUS", message: "Immich planning returned an unrecognized status.", report: report}
		}
	case "import":
		return runImmichImport(ctx, args[1:])
	case "verify":
		return runImmichVerify(ctx, args[1:])
	default:
		return commandOutput{}, &cliError{code: "UNKNOWN_COMMAND", message: fmt.Sprintf("unknown immich command %q; use plan, import, or verify", args[0])}
	}
}

func parseImmichPlan(args []string) (immichPlanArgs, error) {
	fs := newFlagSet("immich plan")
	var parsed immichPlanArgs
	fs.StringVar(&parsed.archive, "archive", "", "verified local portable archive directory")
	fs.BoolVar(&parsed.skipUnresolved, "skip-unresolved", false, "explicitly leave unresolved occurrences untransferred")
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "write JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return immichPlanArgs{}, usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return immichPlanArgs{}, usageError("unexpected positional argument: " + fs.Arg(0))
	}
	if strings.TrimSpace(parsed.archive) == "" {
		return immichPlanArgs{}, usageError("--archive is required")
	}
	return parsed, nil
}

func parseImmichRemote(command string, args []string) (immichRemoteArgs, error) {
	fs := newFlagSet("immich " + command)
	parsed := immichRemoteArgs{apiKeyEnv: defaultImmichAPIKeyEnv, timeout: defaultImmichTimeout}
	fs.StringVar(&parsed.archive, "archive", "", "verified local portable archive directory")
	fs.StringVar(&parsed.server, "server", "", "Immich server origin (HTTPS; loopback HTTP requires explicit opt-in)")
	fs.StringVar(&parsed.apiKeyEnv, "api-key-env", defaultImmichAPIKeyEnv, "environment variable containing the Immich API key")
	if command == "import" {
		fs.BoolVar(&parsed.skipUnresolved, "skip-unresolved", false, "explicitly leave unresolved occurrences untransferred")
	}
	fs.BoolVar(&parsed.allowLoopbackHTTP, "allow-http-loopback", false, "allow HTTP only for a literal loopback address")
	var timeoutText string
	fs.StringVar(&timeoutText, "timeout", defaultImmichTimeout.String(), "maximum operation duration (default 30m, maximum 24h)")
	if command == "import" {
		fs.StringVar(&parsed.reportPath, "report", "", "new operation report path (must not exist)")
		fs.StringVar(&parsed.resumePath, "resume", "", "prior import report to reconcile and resume")
	} else if command == "verify" {
		fs.StringVar(&parsed.reportPath, "report", "", "prior import report to verify")
		fs.StringVar(&parsed.outputPath, "output", "", "new verification report path (must not exist)")
	}
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "write JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return immichRemoteArgs{}, usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return immichRemoteArgs{}, usageError("unexpected positional argument: " + fs.Arg(0))
	}
	if strings.TrimSpace(parsed.archive) == "" {
		return immichRemoteArgs{}, usageError("--archive is required")
	}
	if strings.TrimSpace(parsed.server) == "" {
		return immichRemoteArgs{}, usageError("--server is required")
	}
	if !envNamePattern.MatchString(parsed.apiKeyEnv) {
		return immichRemoteArgs{}, usageError("--api-key-env must be a valid environment-variable name")
	}
	timeout, err := time.ParseDuration(timeoutText)
	if err != nil || timeout <= 0 || timeout > maxImmichTimeout {
		return immichRemoteArgs{}, usageError("--timeout must be positive and no greater than 24h")
	}
	parsed.timeout = timeout
	if command == "import" && strings.TrimSpace(parsed.reportPath) == "" {
		return immichRemoteArgs{}, usageError("--report is required and must name a new file")
	}
	if command == "verify" && (strings.TrimSpace(parsed.reportPath) == "" || strings.TrimSpace(parsed.outputPath) == "") {
		return immichRemoteArgs{}, usageError("--report and --output are required; --output must name a new file")
	}
	return parsed, nil
}

func runImmichImport(parent context.Context, args []string) (commandOutput, error) {
	parsed, err := parseImmichRemote("import", args)
	if err != nil {
		return commandOutput{}, err
	}
	if err := requireImmichReportOutsideArchive(parsed.archive, parsed.reportPath); err != nil {
		return commandOutput{}, err
	}
	apiKey, err := loadImmichAPIKey(parsed.apiKeyEnv)
	if err != nil {
		return commandOutput{}, err
	}
	var prior *immich.Report
	var priorPath string
	if parsed.resumePath != "" {
		record, canonical, loadErr := loadImmichJournal(parsed.resumePath, "immich.import")
		if loadErr != nil {
			return commandOutput{}, &cliError{code: "IMMICH_RESUME_REPORT_INVALID", message: loadErr.Error()}
		}
		prior, err = validateJournalForResume(record)
		if err != nil {
			return commandOutput{}, &cliError{code: "IMMICH_RESUME_REPORT_INVALID", message: "resume input is not a supported import report"}
		}
		priorPath = canonical
	}
	if priorPath != "" && sameReportPath(parsed.reportPath, priorPath) {
		return commandOutput{}, usageError("--report must be a new path separate from the --resume input")
	}
	if err := requireImmichReportOutsideArchive(parsed.archive, parsed.reportPath); err != nil {
		return commandOutput{}, err
	}
	store, err := newImmichJournal(parsed.reportPath, "immich.import")
	if err != nil {
		return commandOutput{}, &cliError{code: "IMMICH_REPORT_CREATE_FAILED", message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(parent, parsed.timeout)
	defer cancel()
	opts := immich.Options{
		ServerURL: parsed.server, APIKey: apiKey, AllowLoopbackHTTP: parsed.allowLoopbackHTTP,
		SkipUnresolved: parsed.skipUnresolved || reportContainsSkipped(prior), Timeout: parsed.timeout, Resume: prior,
		Progress: func(snapshot immich.Report) error {
			return store.checkpoint(&snapshot, snapshot.Status, nil)
		},
	}
	report, operationErr := immich.Import(ctx, parsed.archive, opts)
	if report == nil {
		if operationErr == nil {
			operationErr = errors.New("Immich import returned no report")
		}
		storeErr := store.checkpoint(nil, terminalJournalStatus(operationErr), reportError(operationErr))
		return commandOutput{}, combineImmichFailure(operationErr, storeErr, nil)
	}
	status := report.Status
	if status == "" {
		status = terminalJournalStatus(operationErr)
	}
	if operationErr != nil {
		status = terminalJournalStatus(operationErr)
	}
	storeErr := store.checkpoint(report, status, reportError(operationErr))
	if operationErr != nil || storeErr != nil {
		return commandOutput{}, combineImmichFailure(operationErr, storeErr, report)
	}
	if report.Status != "completed" && report.Status != "completed_with_skips" {
		return commandOutput{}, &cliError{code: "IMMICH_IMPORT_INCOMPLETE", message: "Immich import did not reach a completed status; inspect its report before resuming", report: report}
	}
	return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: "immich.import", Report: report, OutputPath: store.path}, nil
}

func runImmichVerify(parent context.Context, args []string) (commandOutput, error) {
	parsed, err := parseImmichRemote("verify", args)
	if err != nil {
		return commandOutput{}, err
	}
	if err := requireImmichReportOutsideArchive(parsed.archive, parsed.outputPath); err != nil {
		return commandOutput{}, err
	}
	apiKey, err := loadImmichAPIKey(parsed.apiKeyEnv)
	if err != nil {
		return commandOutput{}, err
	}
	priorRecord, priorPath, err := loadImmichJournal(parsed.reportPath, "immich.import")
	if err != nil {
		return commandOutput{}, &cliError{code: "IMMICH_IMPORT_REPORT_INVALID", message: err.Error()}
	}
	prior, err := validateJournalForResume(priorRecord)
	if err != nil {
		return commandOutput{}, &cliError{code: "IMMICH_IMPORT_REPORT_INVALID", message: "--report is not a supported import report"}
	}
	if sameReportPath(parsed.outputPath, priorPath) {
		return commandOutput{}, usageError("--output must be a new path separate from the --report input")
	}
	if err := requireImmichReportOutsideArchive(parsed.archive, parsed.outputPath); err != nil {
		return commandOutput{}, err
	}
	store, err := newImmichJournal(parsed.outputPath, "immich.verify")
	if err != nil {
		return commandOutput{}, &cliError{code: "IMMICH_REPORT_CREATE_FAILED", message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(parent, parsed.timeout)
	defer cancel()
	opts := immich.Options{
		ServerURL: parsed.server, APIKey: apiKey, AllowLoopbackHTTP: parsed.allowLoopbackHTTP,
		SkipUnresolved: reportContainsSkipped(prior), Timeout: parsed.timeout,
		Progress: func(snapshot immich.Report) error {
			return store.checkpoint(&snapshot, snapshot.Status, nil)
		},
	}
	report, operationErr := immich.VerifyRemote(ctx, parsed.archive, opts, *prior)
	if report == nil {
		if operationErr == nil {
			operationErr = errors.New("Immich verification returned no report")
		}
		storeErr := store.checkpoint(nil, terminalJournalStatus(operationErr), reportError(operationErr))
		return commandOutput{}, combineImmichFailure(operationErr, storeErr, nil)
	}
	status := report.Status
	if status == "" || operationErr != nil {
		status = terminalJournalStatus(operationErr)
	}
	storeErr := store.checkpoint(report, status, reportError(operationErr))
	if operationErr != nil || storeErr != nil {
		return commandOutput{}, combineImmichFailure(operationErr, storeErr, report)
	}
	if report.Status != "verified" && report.Status != "verified_with_skips" {
		return commandOutput{}, &cliError{code: "IMMICH_VERIFY_FAILED", message: "remote verification did not pass; inspect its report", report: report}
	}
	return commandOutput{SchemaVersion: cliSchemaVersion, Status: "ok", Command: "immich.verify", Report: report, OutputPath: store.path}, nil
}

// A resume or verification inherits unresolved-item skips from the prior
// report. The initial import still requires the explicit --skip-unresolved
// choice; the report then binds that selected scope for follow-up operations.
func reportContainsSkipped(report *immich.Report) bool {
	if report == nil {
		return false
	}
	if report.Status == "completed_with_skips" {
		return true
	}
	for _, file := range report.Files {
		if file.State == "skipped" {
			return true
		}
	}
	return false
}

func loadImmichAPIKey(envName string) (string, error) {
	value, found := os.LookupEnv(envName)
	if !found || strings.TrimSpace(value) == "" {
		return "", &cliError{code: "IMMICH_API_KEY_MISSING", message: "the configured Immich API-key environment variable is not set"}
	}
	return value, nil
}

func terminalJournalStatus(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return "failed"
}

func combineImmichFailure(operationErr, reportErr error, report *immich.Report) error {
	if reportErr != nil {
		return &cliError{code: "IMMICH_REPORT_WRITE_FAILED", message: "the operation report could not be safely checkpointed; no further remote operation will be attempted", report: report}
	}
	if operationErr == nil {
		operationErr = errors.New("Immich operation did not complete")
	}
	return wrapImmichError(operationErr, report)
}

func wrapImmichError(err error, report any) error {
	if err == nil {
		return nil
	}
	code := errorCode(err)
	if code == "OPERATION_FAILED" {
		code = "IMMICH_OPERATION_FAILED"
	}
	message := safeImmichErrorMessage(err)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &cliError{code: "CANCELLED", message: "operation cancelled", report: report, cause: err}
	}
	return &cliError{code: code, message: message, report: report}
}

func writeImmichPlan(w io.Writer, plan *immich.PlanReport) error {
	if _, err := fmt.Fprintf(w, "Immich plan status: %s\nPlan ID: %s\nManifest SHA-256: %s\nMedia occurrences: %d\nUnique contents: %d\nSource albums: %d\nUnresolved: %d\nExplicitly skipped: %d\nRaw sidecars transferred: 0 (kept in the local archive)\n",
		plan.Status, plan.PlanID, plan.ManifestSHA256, plan.MediaOccurrences, plan.UniqueContents, plan.SourceAlbums, plan.UnresolvedCount, plan.SkippedCount); err != nil {
		return err
	}
	if err := writeImmichIssues(w, plan.Issues); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "This preview covers only the selected portable archive, not a complete online account.")
	return err
}

func writeImmichReport(w io.Writer, report *immich.Report, journalPath string) error {
	if _, err := fmt.Fprintf(w, "Immich %s status: %s\nMedia occurrences: %d\nUnique contents: %d\nSidecars transferred: 0\nSource albums: %d\nAlbum memberships: %d\nIssues: %d\n",
		report.Mode, report.Status, len(report.Files), len(report.Contents), len(report.Albums), len(report.Memberships), len(report.Issues)); err != nil {
		return err
	}
	if report.Verification != nil {
		if _, err := fmt.Fprintf(w, "Remote verification status: %s\nContents checked: %d\nAlbums checked: %d\nMemberships checked: %d\nBytes checked: %d\n",
			report.Verification.Status, report.Verification.ContentsChecked, report.Verification.AlbumsChecked,
			report.Verification.MembershipsChecked, report.Verification.BytesChecked); err != nil {
			return err
		}
	}
	if err := writeImmichIssues(w, report.Issues); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "Private operation report: %s\nScope is limited to this selected archive.\n", journalPath)
	return err
}

func writeImmichIssues(w io.Writer, issues []immich.Issue) error {
	if len(issues) == 0 {
		_, err := fmt.Fprintln(w, "No unresolved issues were recorded.")
		return err
	}
	if _, err := fmt.Fprintln(w, "Review these reported items:"); err != nil {
		return err
	}
	for _, issue := range issues {
		location := issue.SourceEntry
		if issue.OccurrenceID != "" {
			location = issue.OccurrenceID
		}
		if issue.SourceAlbumID != "" {
			if location != "" {
				location += " "
			}
			location += "album " + issue.SourceAlbumID
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

func usageForImmich() string {
	return "Usage: archivebridge immich <plan|import|verify> [options]\n" +
		"  immich plan --archive DIR [--skip-unresolved] [--json]\n" +
		"  immich import --archive DIR --server ORIGIN --report NEW [--resume OLD] [--json]\n" +
		"  immich verify --archive DIR --server ORIGIN --report INPUT --output NEW [--json]\n"
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func helpForImmichSubcommand(name string) string {
	switch name {
	case "plan":
		return "Usage: archivebridge immich plan --archive DIR [--skip-unresolved] [--json]\n" +
			"Build an offline preview from a verified local archive. No network access or report file is used.\n"
	case "import":
		return "Usage: archivebridge immich import --archive DIR --server ORIGIN --report NEW [--resume OLD] [--skip-unresolved] [--api-key-env NAME] [--allow-http-loopback] [--timeout DURATION] [--json]\n" +
			"Explicitly transfer selected originals to Immich 3.3.1. The API key is read only from the configured environment variable.\n"
	case "verify":
		return "Usage: archivebridge immich verify --archive DIR --server ORIGIN --report INPUT --output NEW [--api-key-env NAME] [--allow-http-loopback] [--timeout DURATION] [--json]\n" +
			"Read-only verification of a prior import against Immich 3.3.1. The output report path must be new.\n"
	default:
		return usageForImmich()
	}
}

const topLevelHelp = "ArchiveBridge preserves selected Takeout archives and includes an adapter for Immich 3.3.1.\n\n" +
	"Commands: inspect, plan, export, resume, verify, compare, serve, version, immich.\n" +
	"Use archivebridge immich --help for selected-archive planning, import, verification, and credential options.\n"
