package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Pastalikek65/archivebridge/internal/immich"
)

const (
	immichJournalSchema = 1
	immichProduct       = "ArchiveBridge"
	maxImmichJournal    = 128 << 20
)

type immichJournal struct {
	SchemaVersion int            `json:"schemaVersion"`
	Product       string         `json:"product"`
	JournalID     string         `json:"journalId"`
	Command       string         `json:"command"`
	Status        string         `json:"status"`
	UpdatedAt     string         `json:"updatedAt"`
	Report        *immich.Report `json:"report,omitempty"`
	Error         *errorDetails  `json:"error,omitempty"`
}

type immichJournalStore struct {
	path      string
	journalID string
	command   string
}

func newImmichJournal(path, command string) (*immichJournalStore, error) {
	canonical, err := canonicalNewFilePath(path)
	if err != nil {
		return nil, err
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, errors.New("could not initialize the private operation report")
	}
	store := &immichJournalStore{path: canonical, journalID: hex.EncodeToString(idBytes), command: command}
	initial := immichJournal{
		SchemaVersion: immichJournalSchema, Product: immichProduct, JournalID: store.journalID,
		Command: command, Status: "starting", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := marshalJournal(initial)
	if err != nil {
		return nil, errors.New("could not initialize the private operation report")
	}
	tmp, err := os.CreateTemp(filepath.Dir(canonical), ".archivebridge-report-"+store.journalID+"-*.tmp")
	if err != nil {
		return nil, errors.New("could not create the private operation report")
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, errors.New("could not initialize the private operation report")
	}
	if n, writeErr := tmp.Write(data); writeErr != nil || n != len(data) {
		_ = tmp.Close()
		return nil, errors.New("could not initialize the private operation report")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, errors.New("could not durably initialize the private operation report")
	}
	if err := tmp.Close(); err != nil {
		return nil, errors.New("could not durably initialize the private operation report")
	}
	if err := installNewReport(tmpPath, canonical); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errors.New("report output already exists; choose a new path")
		}
		return nil, errors.New("could not install the private operation report exclusively")
	}
	if err := syncReportDirectory(filepath.Dir(canonical)); err != nil {
		return nil, errors.New("could not durably initialize the private operation report")
	}
	return store, nil
}

func canonicalNewFilePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("report path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("report path is invalid")
	}
	parent := filepath.Dir(filepath.Clean(absolute))
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("report parent must be an existing ordinary directory")
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", errors.New("report parent could not be resolved safely")
	}
	canonical := filepath.Join(resolvedParent, filepath.Base(absolute))
	if _, err := os.Lstat(canonical); err == nil {
		return "", errors.New("report output already exists; choose a new path")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("report output could not be checked safely")
	}
	return canonical, nil
}

// requireImmichReportOutsideArchive prevents the operation journal (and its
// atomic replacement files) from changing the verified portable archive.
// Resolve both paths before creating the journal, then compare filesystem
// identity along the report parent's ancestor chain as well as canonical paths
// so short Windows names and directory links cannot evade the boundary.
func requireImmichReportOutsideArchive(archivePath, reportPath string) error {
	archiveAbsolute, err := filepath.Abs(filepath.Clean(strings.TrimSpace(archivePath)))
	if err != nil {
		return &cliError{code: "IMMICH_ARCHIVE_PATH_INVALID", message: "the selected archive path could not be resolved safely"}
	}
	archiveResolved, err := filepath.EvalSymlinks(archiveAbsolute)
	if err != nil {
		return &cliError{code: "IMMICH_ARCHIVE_PATH_INVALID", message: "the selected archive path could not be resolved safely"}
	}
	archiveInfo, err := os.Stat(archiveResolved)
	if err != nil || !archiveInfo.IsDir() {
		return &cliError{code: "IMMICH_ARCHIVE_PATH_INVALID", message: "the selected archive must be an existing directory"}
	}

	reportAbsolute, err := filepath.Abs(filepath.Clean(strings.TrimSpace(reportPath)))
	if err != nil {
		return &cliError{code: "IMMICH_REPORT_PATH_INVALID", message: "the report output path could not be resolved safely"}
	}
	parent := filepath.Dir(reportAbsolute)
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() {
		return &cliError{code: "IMMICH_REPORT_PATH_INVALID", message: "the report output parent must be an existing directory"}
	}
	parentResolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return &cliError{code: "IMMICH_REPORT_PATH_INVALID", message: "the report output parent could not be resolved safely"}
	}
	target := filepath.Join(parentResolved, filepath.Base(reportAbsolute))
	if pathContains(archiveResolved, target) {
		return &cliError{code: "IMMICH_REPORT_INSIDE_ARCHIVE", message: "the report output must be outside the selected archive directory"}
	}

	for current := parentResolved; ; current = filepath.Dir(current) {
		info, statErr := os.Stat(current)
		if statErr != nil || !info.IsDir() {
			return &cliError{code: "IMMICH_REPORT_PATH_INVALID", message: "the report output parent could not be checked safely"}
		}
		if os.SameFile(archiveInfo, info) {
			return &cliError{code: "IMMICH_REPORT_INSIDE_ARCHIVE", message: "the report output must be outside the selected archive directory"}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

func pathContains(parent, candidate string) bool {
	parent = filepath.Clean(parent)
	candidate = filepath.Clean(candidate)
	if runtime.GOOS == "windows" {
		parent = strings.ToLower(parent)
		candidate = strings.ToLower(candidate)
	}
	relative, err := filepath.Rel(parent, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func sameReportPath(a, b string) bool {
	left, err1 := filepath.Abs(a)
	right, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	left, right = filepath.Clean(left), filepath.Clean(right)
	if os.PathSeparator == '\\' {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func loadImmichJournal(path string, expectedCommand string) (immichJournal, string, error) {
	canonical, err := filepath.Abs(path)
	if err != nil {
		return immichJournal{}, "", errors.New("report path is invalid")
	}
	canonical = filepath.Clean(canonical)
	info, err := os.Lstat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxImmichJournal {
		return immichJournal{}, "", errors.New("operation report is missing, linked, non-regular, or too large")
	}
	f, err := os.Open(canonical)
	if err != nil {
		return immichJournal{}, "", errors.New("operation report could not be opened safely")
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxImmichJournal+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) > maxImmichJournal {
		return immichJournal{}, "", errors.New("operation report could not be read safely")
	}
	var record immichJournal
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return immichJournal{}, "", errors.New("operation report is malformed or has an unsupported schema")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return immichJournal{}, "", errors.New("operation report has trailing data")
	}
	if record.SchemaVersion != immichJournalSchema || record.Product != immichProduct || record.Command != expectedCommand ||
		record.JournalID == "" || record.Status == "" || record.Report == nil || record.Report.SchemaVersion != immich.ReportSchemaVersion {
		return immichJournal{}, "", errors.New("operation report does not match the expected ArchiveBridge operation")
	}
	return record, canonical, nil
}

func (s *immichJournalStore) checkpoint(report *immich.Report, status string, diagnostic *errorDetails) error {
	if s == nil || s.path == "" || s.journalID == "" {
		return errors.New("report journal is unavailable")
	}
	if err := s.checkOwnership(); err != nil {
		return err
	}
	entry := immichJournal{
		SchemaVersion: immichJournalSchema, Product: immichProduct, JournalID: s.journalID,
		Command: s.command, Status: status, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Report: report, Error: diagnostic,
	}
	data, err := marshalJournal(entry)
	if err != nil {
		return errors.New("operation report exceeded its safe size or could not be encoded")
	}
	if int64(len(data)) > maxImmichJournal {
		return errors.New("operation report exceeds the safe size limit")
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".archivebridge-report-"+s.journalID+"-*.tmp")
	if err != nil {
		return errors.New("could not create a temporary operation report")
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		return errors.New("could not durably write the operation report")
	}
	if err := replaceOwnedReport(tmpPath, s.path); err != nil {
		return errors.New("could not atomically update the operation report")
	}
	removeTemp = false
	if err := syncReportDirectory(filepath.Dir(s.path)); err != nil {
		return errors.New("could not durably update the operation report")
	}
	return nil
}

func (s *immichJournalStore) checkOwnership() error {
	info, err := os.Lstat(s.path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxImmichJournal {
		return errors.New("operation report is no longer an owned regular file")
	}
	f, err := os.Open(s.path)
	if err != nil {
		return errors.New("operation report is no longer readable")
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxImmichJournal+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) > maxImmichJournal {
		return errors.New("operation report is no longer readable")
	}
	var owner immichJournal
	if json.Unmarshal(data, &owner) != nil || owner.SchemaVersion != immichJournalSchema || owner.Product != immichProduct ||
		owner.JournalID != s.journalID || owner.Command != s.command {
		return errors.New("operation report ownership changed; refusing to overwrite it")
	}
	return nil
}

func marshalJournal(record immichJournal) ([]byte, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func reportError(err error) *errorDetails {
	if err == nil {
		return nil
	}
	code := errorCode(err)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = "CANCELLED"
	}
	return &errorDetails{Code: code, Message: safeImmichErrorMessage(err)}
}

func safeImmichErrorMessage(err error) string {
	var typed *immich.Error
	if errors.As(err, &typed) && typed.Message != "" {
		return typed.Message
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "operation cancelled"
	}
	var cliErr *cliError
	if errors.As(err, &cliErr) && cliErr.message != "" {
		return cliErr.message
	}
	return "Immich operation did not complete; inspect the sanitized report before resuming."
}

func validateJournalForResume(record immichJournal) (*immich.Report, error) {
	if record.Report == nil || record.Report.Mode != "import" || record.Report.SchemaVersion != immich.ReportSchemaVersion {
		return nil, fmt.Errorf("report is not a supported Immich import journal")
	}
	return record.Report, nil
}
