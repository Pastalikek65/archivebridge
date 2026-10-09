package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pastalikek65/archivebridge/internal/bridge"
)

func TestRunVersion(t *testing.T) {
	t.Run("human", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), []string{"--version"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("Run exit code = %d, stderr = %q", code, stderr.String())
		}
		if got, want := stdout.String(), "ArchiveBridge 0.2.0 (development)\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
		if stderr.Len() != 0 {
			t.Fatalf("unexpected stderr: %q", stderr.String())
		}
	})

	t.Run("json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), []string{"version", "--json"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("Run exit code = %d, stderr = %q", code, stderr.String())
		}
		var got commandOutput
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatalf("stdout is not JSON: %v (%q)", err, stdout.String())
		}
		if got.SchemaVersion != cliSchemaVersion || got.Status != "ok" || got.Command != "version" || got.Version != "0.2.0" || got.Commit != "development" {
			t.Fatalf("unexpected version response: %+v", got)
		}
		if stderr.Len() != 0 {
			t.Fatalf("unexpected stderr: %q", stderr.String())
		}
	})
}

func TestErrorCodeOutputLockedIsStable(t *testing.T) {
	if got := errorCode(bridge.ErrOutputLocked); got != "output_locked" {
		t.Fatalf("errorCode(ErrOutputLocked) = %q, want output_locked", got)
	}
}

func TestRunRejectsUnknownCommandAndArguments(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown command", args: []string{"mystery"}, want: "UNKNOWN_COMMAND"},
		{name: "extra argument", args: []string{"verify", "--archive", "archive-dir", "extra"}, want: "INVALID_ARGUMENT"},
		{name: "unknown flag in JSON mode", args: []string{"verify", "--archive", "archive-dir", "--json", "--mystery"}, want: "INVALID_ARGUMENT"},
		{name: "compare requires plan and archive", args: []string{"compare", "--json"}, want: "INVALID_ARGUMENT"},
		{name: "compare rejects unknown flag", args: []string{"compare", "--plan", "plan.json", "--archive", "archive-dir", "--mystery"}, want: "INVALID_ARGUMENT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), tc.args, &stdout, &stderr)
			if code == 0 {
				t.Fatal("Run succeeded for invalid arguments")
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr %q does not contain error code %q", stderr.String(), tc.want)
			}
			if wantsJSON(tc.args) {
				var got errorOutput
				if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
					t.Fatalf("stdout is not a JSON error: %v (%q)", err, stdout.String())
				}
				if got.SchemaVersion != cliSchemaVersion || got.Status != "error" || got.Error.Code != tc.want {
					t.Fatalf("unexpected JSON error: %+v", got)
				}
			} else if stdout.Len() != 0 {
				t.Fatalf("unexpected stdout on failure: %q", stdout.String())
			}
		})
	}
}

func TestRunFailedPlanDoesNotCreateOutput(t *testing.T) {
	temp := t.TempDir()
	planPath := filepath.Join(temp, "plan.json")
	missingSource := filepath.Join(temp, "missing.zip")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"plan", "--source", missingSource, "--output", planPath}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("plan succeeded for a missing source")
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected stdout on failed operation: %q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatal("failed plan did not report a diagnostic")
	}
	if _, err := os.Stat(planPath); !os.IsNotExist(err) {
		t.Fatalf("failed plan output exists or could not be checked: %v", err)
	}
}

func TestRunArchiveWorkflow(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "takeout-001.zip")
	createTakeoutZIP(t, source)
	planPath := filepath.Join(temp, "plan.json")
	archiveDir := filepath.Join(temp, "portable-archive")

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"inspect", "--source", source}, &stdout, &stderr); code != 0 {
		t.Fatalf("inspect exit code = %d, stderr = %q", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "1 selected source parts") || !strings.Contains(got, "Media occurrences: 1") || !strings.Contains(got, "Unresolved issues: 1") || !strings.Contains(got, "[unsupported_member]") || !strings.Contains(got, "whole-account coverage") {
		t.Fatalf("inspect summary lacks selected-scope counts: %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected inspect stderr: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"plan", "--source", source, "--output", planPath, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("plan exit code = %d, stderr = %q", code, stderr.String())
	}
	var planResponse commandOutput
	if err := json.Unmarshal(stdout.Bytes(), &planResponse); err != nil {
		t.Fatalf("plan output is not JSON: %v (%q)", err, stdout.String())
	}
	if planResponse.Status != "ok" || planResponse.Command != "plan" || planResponse.OutputPath != planPath {
		t.Fatalf("unexpected plan response: %+v", planResponse)
	}
	plan, err := bridge.ReadPlan(planPath)
	if err != nil {
		t.Fatalf("plan file is not readable: %v", err)
	}
	if plan.Stats.SourceCount != 1 || plan.Stats.MediaCount != 1 || plan.Stats.IssueCount != 1 {
		t.Fatalf("unexpected plan counts: %+v", plan.Stats)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"export", "--plan", planPath, "--out", archiveDir, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("export exit code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var exportResponse struct {
		Status string              `json:"status"`
		Report bridge.ExportReport `json:"report"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &exportResponse); err != nil {
		t.Fatalf("export output is not JSON: %v (%q)", err, stdout.String())
	}
	if exportResponse.Status != "ok" || exportResponse.Report.Status != "complete" || exportResponse.Report.FilesWritten != 1 || len(exportResponse.Report.Issues) != 1 {
		t.Fatalf("unexpected export response: %+v", exportResponse)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"resume", "--plan", planPath, "--out", archiveDir, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("resume exit code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &exportResponse); err != nil {
		t.Fatalf("resume output is not JSON: %v (%q)", err, stdout.String())
	}
	if exportResponse.Report.Status != "complete" || exportResponse.Report.FilesReused != 1 {
		t.Fatalf("resume did not report reuse: %+v", exportResponse.Report)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"verify", "--archive", archiveDir, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("verify exit code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var verifyResponse struct {
		Status string              `json:"status"`
		Report bridge.VerifyReport `json:"report"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &verifyResponse); err != nil {
		t.Fatalf("verify output is not JSON: %v (%q)", err, stdout.String())
	}
	if verifyResponse.Status != "ok" || verifyResponse.Report.Status != "ok" || verifyResponse.Report.FilesChecked != 1 {
		t.Fatalf("unexpected verify response: %+v", verifyResponse)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"compare", "--plan", planPath, "--archive", archiveDir, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("compare exit code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var compareResponse struct {
		SchemaVersion int                  `json:"schemaVersion"`
		Status        string               `json:"status"`
		Command       string               `json:"command"`
		Report        bridge.CompareReport `json:"report"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &compareResponse); err != nil {
		t.Fatalf("compare output is not JSON: %v (%q)", err, stdout.String())
	}
	if compareResponse.SchemaVersion != cliSchemaVersion || compareResponse.Status != "ok" || compareResponse.Command != "compare" || compareResponse.Report.Status != "matched" || compareResponse.Report.PlanID != plan.ID || compareResponse.Report.ArchivePlanID != plan.ID || compareResponse.Report.MediaChecked != 1 {
		t.Fatalf("unexpected compare response: %+v", compareResponse)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected compare stderr: %q", stderr.String())
	}

	tamperedPath := filepath.Join(archiveDir, filepath.FromSlash(plan.Files[0].OutputPath))
	if err := os.WriteFile(tamperedPath, []byte("tampered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"verify", "--archive", archiveDir, "--json"}, &stdout, &stderr); code == 0 {
		t.Fatal("verify succeeded after an exported file was changed")
	}
	var failedVerify struct {
		Status string              `json:"status"`
		Error  errorDetails        `json:"error"`
		Report bridge.VerifyReport `json:"report"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &failedVerify); err != nil {
		t.Fatalf("failed verify output is not JSON: %v (%q)", err, stdout.String())
	}
	if failedVerify.Status != "error" || failedVerify.Error.Code != "REPORT_FAILED" || failedVerify.Report.Status != "failed" || len(failedVerify.Report.Issues) == 0 {
		t.Fatalf("verify failure report was not preserved: %+v", failedVerify)
	}
	if !strings.Contains(stderr.String(), "REPORT_FAILED") {
		t.Fatalf("failed verify did not write a diagnostic: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"compare", "--plan", planPath, "--archive", archiveDir, "--json"}, &stdout, &stderr); code == 0 {
		t.Fatal("compare succeeded after an exported file was changed")
	}
	var failedCompare struct {
		SchemaVersion int                  `json:"schemaVersion"`
		Status        string               `json:"status"`
		Command       string               `json:"command"`
		Error         errorDetails         `json:"error"`
		Report        bridge.CompareReport `json:"report"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &failedCompare); err != nil {
		t.Fatalf("failed compare output is not JSON: %v (%q)", err, stdout.String())
	}
	if failedCompare.SchemaVersion != cliSchemaVersion || failedCompare.Status != "error" || failedCompare.Command != "compare" || failedCompare.Error.Code != "REPORT_FAILED" || failedCompare.Report.Status != "mismatched" || len(failedCompare.Report.Mismatches) == 0 {
		t.Fatalf("compare failure report was not preserved: %+v", failedCompare)
	}
	if !strings.Contains(stderr.String(), "REPORT_FAILED") {
		t.Fatalf("failed compare did not write a diagnostic: %q", stderr.String())
	}
}

func createTakeoutZIP(t *testing.T, filename string) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	zipWriter := zip.NewWriter(file)
	member, err := zipWriter.Create("Google Photos/Album/example.jpg")
	if err != nil {
		_ = zipWriter.Close()
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(member, "sample photo bytes"); err != nil {
		_ = zipWriter.Close()
		_ = file.Close()
		t.Fatal(err)
	}
	metadata, err := zipWriter.Create("Google Photos/Album/readme.txt")
	if err != nil {
		_ = zipWriter.Close()
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(metadata, "unrecognized source content"); err != nil {
		_ = zipWriter.Close()
		_ = file.Close()
		t.Fatal(err)
	}
	if err := zipWriter.Close(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunCancellationIsVersionedAndNonzero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"inspect", "--source", "source.zip", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("cancelled command succeeded")
	}
	var got errorOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a JSON error: %v (%q)", err, stdout.String())
	}
	if got.SchemaVersion != cliSchemaVersion || got.Status != "cancelled" || got.Error.Code != "CANCELLED" {
		t.Fatalf("unexpected cancellation response: %+v", got)
	}
}

func TestServeRequiresLoopbackAddress(t *testing.T) {
	for _, address := range []string{"0.0.0.0:4175", ":4175", "example.com:4175"} {
		if isLocalListenAddress(address) {
			t.Errorf("accepted non-loopback listen address %q", address)
		}
	}
	for _, address := range []string{"127.0.0.1:4175", "localhost:4175", "[::1]:4175"} {
		if !isLocalListenAddress(address) {
			t.Errorf("rejected loopback listen address %q", address)
		}
	}
	parsed, err := parseServe([]string{"--archive", "portable-archive", "--listen", "localhost:4175"})
	if err != nil {
		t.Fatalf("parseServe rejected localhost: %v", err)
	}
	if parsed.listen != "127.0.0.1:4175" {
		t.Fatalf("localhost listen address = %q, want literal loopback 127.0.0.1:4175", parsed.listen)
	}
}

func TestReportStatusesMustMatchCommand(t *testing.T) {
	cases := []struct {
		command string
		status  string
		wantErr string
	}{
		{command: "export", status: "complete"},
		{command: "resume", status: "complete"},
		{command: "verify", status: "ok"},
		{command: "export", status: "failed", wantErr: "REPORT_FAILED"},
		{command: "verify", status: "complete", wantErr: "UNKNOWN_STATUS"},
		{command: "verify", status: "", wantErr: "UNKNOWN_STATUS"},
	}
	for _, tc := range cases {
		t.Run(tc.command+"-"+tc.status, func(t *testing.T) {
			err := checkReportStatus(tc.command, tc.status, map[string]string{"status": tc.status})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || errorCode(err) != tc.wantErr) {
				t.Fatalf("error = %v, want code %s", err, tc.wantErr)
			}
		})
	}
}
