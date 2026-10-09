package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pastalikek65/archivebridge/internal/bridge"
	"github.com/Pastalikek65/archivebridge/internal/immich"
)

func TestParseImmichCommandContracts(t *testing.T) {
	plan, err := parseImmichPlan([]string{"--archive", "photos", "--skip-unresolved", "--json"})
	if err != nil || plan.archive != "photos" || !plan.skipUnresolved {
		t.Fatalf("parse plan = %#v, %v", plan, err)
	}

	parsed, err := parseImmichRemote("import", []string{
		"--archive", "photos", "--server", "https://immich.example", "--report", "new.json",
		"--resume", "old.json", "--allow-http-loopback", "--timeout", "2m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.apiKeyEnv != defaultImmichAPIKeyEnv || parsed.timeout != 2*time.Minute ||
		parsed.resumePath != "old.json" || !parsed.allowLoopbackHTTP {
		t.Fatalf("parsed import options = %#v", parsed)
	}
	verify, err := parseImmichRemote("verify", []string{
		"--archive", "photos", "--server", "https://immich.example", "--report", "import.json", "--output", "verify.json",
	})
	if err != nil || verify.reportPath != "import.json" || verify.outputPath != "verify.json" {
		t.Fatalf("parse verify = %#v, %v", verify, err)
	}

	for _, args := range [][]string{
		{"--archive", "photos", "--unknown"},
		{"--archive", "photos", "extra"},
		{"--archive", "photos", "--server", "https://immich.example", "--report", "r.json", "--timeout", "25h"},
		{"--archive", "photos", "--server", "https://immich.example", "--report", "r.json", "--api-key-env", "BAD=NAME"},
	} {
		withCommand := append([]string(nil), args...)
		if len(args) > 0 && args[0] == "--archive" && strings.Contains(strings.Join(args, " "), "--unknown") {
			if _, err := parseImmichPlan(withCommand); err == nil {
				t.Errorf("plan accepted invalid args %q", args)
			}
			continue
		}
		if _, err := parseImmichRemote("import", withCommand); err == nil {
			t.Errorf("import accepted invalid args %q", args)
		}
	}
}

func TestImmichHelpIsAvailableWithoutServerOrCredentials(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"immich", "--help"}, {"immich", "import", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("help %q exit=%d stderr=%s", args, code, stderr.String())
		}
		if stdout.Len() == 0 || stderr.Len() != 0 {
			t.Fatalf("help %q stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
		if len(args) == 1 && args[0] == "--help" &&
			(!strings.Contains(stdout.String(), "Immich 3.3.1") || strings.Contains(stdout.String(), "source-candidate")) {
			t.Fatalf("top-level help does not describe the implemented adapter clearly: %s", stdout.String())
		}
	}
}

func TestImmichFollowUpInheritsExplicitUnresolvedSkips(t *testing.T) {
	if reportContainsSkipped(nil) {
		t.Fatal("nil report unexpectedly authorized skipped-scope follow-up")
	}
	if reportContainsSkipped(&immich.Report{Status: "completed"}) {
		t.Fatal("complete report unexpectedly enabled unresolved skips")
	}
	if !reportContainsSkipped(&immich.Report{Status: "completed_with_skips"}) {
		t.Fatal("completed-with-skips report did not preserve its selected scope")
	}
	if !reportContainsSkipped(&immich.Report{Status: "interrupted", Files: []immich.OccurrenceResult{{State: "skipped"}}}) {
		t.Fatal("interrupted report did not preserve its explicit skipped occurrence")
	}
}

func TestImmichJournalExclusiveCheckpointsAndOwnership(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "operation.json")
	store, err := newImmichJournal(path, "immich.import")
	if err != nil {
		t.Fatal(err)
	}
	report := &immich.Report{
		SchemaVersion: immich.ReportSchemaVersion, Mode: "import", Status: "in_progress",
		PlanID: strings.Repeat("a", 64), ManifestSHA256: strings.Repeat("b", 64),
		ServerOrigin: "https://immich.example", ServerVersion: immich.SupportedServerVersion,
		Files: []immich.OccurrenceResult{{OccurrenceID: "occurrence-1", State: "pending"}},
	}
	if err := store.checkpoint(report, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	record, canonical, err := loadImmichJournal(path, "immich.import")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != store.path {
		t.Fatalf("loaded journal path differs from its owned path: loaded=%q stored=%q", canonical, store.path)
	}
	if record.JournalID != store.journalID || record.Report == nil || record.Report.PlanID != report.PlanID {
		t.Fatalf("loaded journal did not retain its report: loaded=%#v journalID=%q expectedPlanID=%q", record, store.journalID, report.PlanID)
	}

	secret := "private-api-key-that-must-not-appear"
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(secret)) {
		t.Fatal("journal contains a credential value")
	}
	if _, err := newImmichJournal(path, "immich.import"); err == nil {
		t.Fatal("new journal overwrote an existing path")
	}
	if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, encoded) {
		t.Fatalf("existing report changed after output collision: err=%v", err)
	}

	foreign := immichJournal{SchemaVersion: immichJournalSchema, Product: immichProduct,
		JournalID: "other-owner", Command: "immich.import", Status: "in_progress", Report: report}
	foreignBytes, err := marshalJournal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, foreignBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.checkpoint(report, "complete", nil); err == nil {
		t.Fatal("journal updated a file after its ownership marker changed")
	}
	if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, foreignBytes) {
		t.Fatalf("unowned report changed after refusal: err=%v", err)
	}
}

func TestImmichJournalCanonicalPathComparisonHandlesMixedCaseAncestors(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "reports", "operation.json")
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := newImmichJournal(path, "immich.import")
	if err != nil {
		t.Fatal(err)
	}
	report := &immich.Report{
		SchemaVersion: immich.ReportSchemaVersion, Mode: "import", Status: "in_progress",
		PlanID: strings.Repeat("a", 64), ManifestSHA256: strings.Repeat("b", 64),
		ServerOrigin: "https://immich.example", ServerVersion: immich.SupportedServerVersion,
	}
	if err := store.checkpoint(report, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	loadPath := path
	if runtime.GOOS == "windows" {
		loadPath = strings.ToUpper(path)
	}
	record, canonical, err := loadImmichJournal(loadPath, "immich.import")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != store.path {
		t.Fatalf("case-varied journal paths must identify the same file: loaded=%q stored=%q", canonical, store.path)
	}
	if record.JournalID != store.journalID || record.Report == nil {
		t.Fatalf("case-varied journal load lost owned report identity: loaded=%#v storeID=%q", record, store.journalID)
	}
}

func TestImmichPlanCommandUsesVerifiedLocalArchiveAndReturnsBlockedReport(t *testing.T) {
	archive := makeTestPortableArchive(t)

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"immich", "plan", "--archive", archive, "--skip-unresolved", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("plan exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var success struct {
		SchemaVersion int               `json:"schemaVersion"`
		Status        string            `json:"status"`
		Command       string            `json:"command"`
		Report        immich.PlanReport `json:"report"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &success); err != nil {
		t.Fatalf("decode plan output: %v; %s", err, stdout.String())
	}
	if success.SchemaVersion != cliSchemaVersion || success.Status != "ok" || success.Command != "immich.plan" ||
		(success.Report.Status != "ready" && success.Report.Status != "ready_with_skips") || len(success.Report.Files) == 0 {
		t.Fatalf("unexpected plan response: %#v", success)
	}
	if strings.Contains(stderr.String(), "API key") {
		t.Fatalf("offline plan unexpectedly referenced authentication: %s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"immich", "plan", "--archive", archive, "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("default plan should block unresolved fixture metadata: %s", stdout.String())
	}
	var blocked struct {
		Status string `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
		Report immich.PlanReport `json:"report"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &blocked); err != nil {
		t.Fatalf("decode blocked plan: %v; %s", err, stdout.String())
	}
	if blocked.Status != "error" || blocked.Error.Code != "IMMICH_PLAN_BLOCKED" || blocked.Report.Status != "blocked" {
		t.Fatalf("blocked plan was not represented truthfully: %#v", blocked)
	}
}

func TestImmichImportMissingKeyCreatesNoReportAndLeaksNoSecret(t *testing.T) {
	t.Setenv(defaultImmichAPIKeyEnv, "")
	archive := makeTestPortableArchive(t)
	path := filepath.Join(t.TempDir(), "must-not-exist.json")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{
		"immich", "import", "--archive", archive, "--server", "https://immich.example",
		"--report", path, "--json",
	}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stdout.String(), "IMMICH_API_KEY_MISSING") {
		t.Fatalf("missing key was not rejected: exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("preflight auth failure created a report: %v", err)
	}
	const key = "test-private-value-never-logged"
	t.Setenv(defaultImmichAPIKeyEnv, key)
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{
		"immich", "import", "--archive", archive, "--server", "https://immich.example",
		"--report", path, "--not-a-real-flag", "--json",
	}, &stdout, &stderr)
	if code == 0 || strings.Contains(stdout.String()+stderr.String(), key) {
		t.Fatalf("invalid invocation leaked its environment secret: exit=%d", code)
	}
}

func TestImmichOperationReportsCannotBeWrittenInsideArchive(t *testing.T) {
	archive := makeTestPortableArchive(t)
	if report, err := bridge.Verify(context.Background(), archive); err != nil || report.Status != "ok" {
		t.Fatalf("precondition archive verification = %#v, %v", report, err)
	}
	t.Setenv(defaultImmichAPIKeyEnv, "")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{
			name: "import",
			args: []string{"immich", "import", "--archive", archive, "--server", "https://immich.example",
				"--report", filepath.Join(archive, "import-report.json"), "--json"},
		},
		{
			name: "verify",
			args: []string{"immich", "verify", "--archive", archive, "--server", "https://immich.example",
				"--report", filepath.Join(archive, "missing-import-report.json"),
				"--output", filepath.Join(archive, "verify-report.json"), "--json"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), tc.args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("archive-overlapping report path was accepted: stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
			var response errorOutput
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
			}
			if response.Status != "error" || response.Error.Code != "IMMICH_REPORT_INSIDE_ARCHIVE" {
				t.Fatalf("unexpected overlap error: %#v", response)
			}
			if strings.Contains(stdout.String()+stderr.String(), ".archivebridge-report-") {
				t.Fatalf("journal temp path leaked in diagnostics: %s", stdout.String()+stderr.String())
			}
			for _, name := range []string{"import-report.json", "verify-report.json"} {
				if _, err := os.Lstat(filepath.Join(archive, name)); !os.IsNotExist(err) {
					t.Fatalf("overlap rejection created %s: %v", name, err)
				}
			}
		})
	}
	if report, err := bridge.Verify(context.Background(), archive); err != nil || report.Status != "ok" {
		t.Fatalf("archive changed after rejected output paths: %#v, %v", report, err)
	}
}

func makeTestPortableArchive(t *testing.T) string {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate CLI test file")
	}
	project := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
	fixtures := filepath.Join(project, "examples", "sample")
	plan, err := bridge.Inspect(context.Background(), []string{
		filepath.Join(fixtures, "takeout-part-1.zip"), filepath.Join(fixtures, "takeout-part-2.zip"),
	}, bridge.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	planPath := filepath.Join(directory, "plan.json")
	archivePath := filepath.Join(directory, "portable-archive")
	if err := bridge.WritePlan(plan, planPath); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Export(context.Background(), plan, archivePath); err != nil {
		t.Fatal(err)
	}
	return archivePath
}
