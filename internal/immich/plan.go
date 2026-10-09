package immich

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Pastalikek65/archivebridge/internal/bridge"
)

const maxManifestBytes = int64(256 << 20)

const sourceMTimeNotice = "Takeout does not provide the original filesystem modification time; Immich requires fileModifiedAt, so v1 sets it to the known supported source date."

// Plan reads and verifies the portable archive without contacting Immich.
// When skipUnresolved is false, missing, ambiguous, malformed, or conflicting
// dates produce a blocked preview. With an explicit skip, affected occurrences
// remain visible as skipped and are never assigned invented dates.
func Plan(ctx context.Context, archive string, skipUnresolved bool) (*PlanReport, error) {
	root, err := filepath.Abs(archive)
	if err != nil {
		return nil, localError("ARCHIVE_INVALID", "Portable archive could not be resolved safely.", err)
	}
	root = filepath.Clean(root)
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, localError("ARCHIVE_INVALID", "Portable archive must be a real directory.", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, localError("CANCELED", "Local archive inspection was canceled.", err)
	}

	manifest, err := bridge.ReadManifest(root)
	if err != nil {
		return nil, localError("MANIFEST_INVALID", "Portable archive manifest is invalid or unsupported.", err)
	}
	manifestSHA, err := digestManifest(ctx, root)
	if err != nil {
		return nil, localError("MANIFEST_UNREADABLE", "Portable archive manifest could not be read safely.", err)
	}
	verified, err := bridge.Verify(ctx, root)
	if err != nil {
		return nil, localError("ARCHIVE_VERIFY_FAILED", "Portable archive verification did not complete.", err)
	}
	if verified == nil || verified.Status != "ok" {
		return nil, &Error{Code: "ARCHIVE_VERIFY_FAILED", Message: "Portable archive failed integrity verification.", cause: ErrPreflight}
	}
	if err := ctx.Err(); err != nil {
		return nil, localError("CANCELED", "Local archive inspection was canceled.", err)
	}
	manifestAfter, err := bridge.ReadManifest(root)
	if err != nil {
		return nil, localError("MANIFEST_CHANGED", "Portable archive manifest changed during verification.", err)
	}
	manifestSHAFinal, err := digestManifest(ctx, root)
	if err != nil || manifestSHAFinal != manifestSHA || !reflect.DeepEqual(manifest, manifestAfter) {
		return nil, localError("MANIFEST_CHANGED", "Portable archive manifest changed during verification.", err)
	}

	report := &PlanReport{
		SchemaVersion:        ReportSchemaVersion,
		Status:               "ready",
		PlanID:               manifest.PlanID,
		ManifestSHA256:       manifestSHA,
		DateTransferPolicy:   dateTransferPolicy,
		FileModifiedAtNotice: sourceMTimeNotice,
		MediaOccurrences:     len(manifest.Files),
		Files:                make([]PlannedFile, 0, len(manifest.Files)),
		Sidecars:             make([]PlannedSidecar, 0, len(manifest.Sidecars)),
		Albums:               make([]PlannedAlbum, 0, len(manifest.Albums)),
		Issues:               []Issue{},
	}
	contents := make(map[string][]int, len(manifest.Files))
	dateByOccurrence := make([]string, len(manifest.Files))
	blockedReason := make([]string, len(manifest.Files))
	for i, media := range manifest.Files {
		contents[media.SHA256] = append(contents[media.SHA256], i)
		parsed, parseErr := time.Parse(time.RFC3339, media.Date)
		if media.MetadataStatus == "ambiguous" || media.MetadataStatus == "malformed" {
			blockedReason[i] = "unresolved_metadata"
		} else if media.Date == "" {
			blockedReason[i] = "missing_date"
		} else if parseErr != nil {
			blockedReason[i] = "invalid_date"
		} else {
			dateByOccurrence[i] = parsed.UTC().Format(time.RFC3339Nano)
		}
	}
	for _, indexes := range contents {
		knownDates := map[string]struct{}{}
		for _, i := range indexes {
			if dateByOccurrence[i] != "" {
				knownDates[dateByOccurrence[i]] = struct{}{}
			}
		}
		if len(knownDates) > 1 {
			for _, i := range indexes {
				blockedReason[i] = "conflicting_content_dates"
			}
		}
	}

	for i, media := range manifest.Files {
		state := "ready"
		reason := blockedReason[i]
		if reason != "" {
			if skipUnresolved {
				state = "skipped"
				report.SkippedCount++
			} else {
				state = "blocked"
			}
			report.UnresolvedCount++
			report.Issues = append(report.Issues, Issue{
				Code:         reason,
				Details:      issueDetails(reason),
				OccurrenceID: media.ID,
				SourceEntry:  media.EntryPath,
			})
		}
		report.Files = append(report.Files, PlannedFile{
			OccurrenceID:   media.ID,
			SourceEntry:    media.EntryPath,
			OutputPath:     media.OutputPath,
			SHA256:         media.SHA256,
			Bytes:          media.Bytes,
			Date:           dateByOccurrence[i],
			SourceAlbumIDs: append([]string{}, media.AlbumIDs...),
			State:          state,
			Reason:         reason,
		})
	}
	report.UniqueContents = len(contents)
	for _, sidecar := range manifest.Sidecars {
		report.Sidecars = append(report.Sidecars, PlannedSidecar{
			SidecarID:   sidecar.ID,
			SourceEntry: sidecar.EntryPath,
			SHA256:      sidecar.SHA256,
			Bytes:       sidecar.Bytes,
			State:       "not_transferred",
			Transferred: false,
		})
	}
	fileIndex := make(map[string]int, len(report.Files))
	for i := range report.Files {
		fileIndex[report.Files[i].OccurrenceID] = i
	}
	for _, album := range manifest.Albums {
		state := "ready"
		readyMembers := 0
		for _, occurrenceID := range album.MediaIDs {
			if i, ok := fileIndex[occurrenceID]; ok && report.Files[i].State == "ready" {
				readyMembers++
			}
		}
		if len(album.MediaIDs) > 0 && readyMembers == 0 {
			state = "skipped"
		}
		report.Albums = append(report.Albums, PlannedAlbum{
			SourceAlbumID: album.ID,
			Title:         album.Title,
			Folder:        album.Folder,
			OccurrenceIDs: append([]string{}, album.MediaIDs...),
			State:         state,
		})
	}
	report.SourceAlbums = len(report.Albums)
	for _, issue := range manifest.Issues {
		report.Issues = append(report.Issues, Issue{
			Code:        issue.Code,
			Details:     issueDetails(issue.Code),
			SourceEntry: issue.EntryPath,
		})
	}
	sort.Slice(report.Issues, func(i, j int) bool {
		if report.Issues[i].Code != report.Issues[j].Code {
			return report.Issues[i].Code < report.Issues[j].Code
		}
		if report.Issues[i].SourceEntry != report.Issues[j].SourceEntry {
			return report.Issues[i].SourceEntry < report.Issues[j].SourceEntry
		}
		return report.Issues[i].OccurrenceID < report.Issues[j].OccurrenceID
	})
	if report.UnresolvedCount > 0 {
		if skipUnresolved {
			report.Status = "ready_with_skips"
		} else {
			report.Status = "blocked"
		}
	}
	return report, nil
}

func digestManifest(ctx context.Context, root string) (string, error) {
	name := filepath.Join(root, "manifest.json")
	before, err := os.Lstat(name)
	if err != nil {
		return "", err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maxManifestBytes {
		return "", errors.New("manifest is not a bounded regular file")
	}
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) || opened.Size() != before.Size() || !opened.Mode().IsRegular() {
		return "", errors.New("manifest identity changed while opening")
	}
	h := sha256.New()
	limited := &contextReader{ctx: ctx, r: io.LimitReader(f, maxManifestBytes+1)}
	n, err := io.Copy(h, limited)
	if err != nil {
		return "", err
	}
	if n != before.Size() || n > maxManifestBytes {
		return "", errors.New("manifest size changed or exceeds the bound")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func issueDetails(code string) string {
	switch code {
	case "missing_date":
		return "No unambiguous supported source date is available for this media occurrence."
	case "invalid_date":
		return "The preserved source date could not be interpreted as a valid timestamp."
	case "unresolved_metadata":
		return "The media occurrence has ambiguous or malformed date metadata."
	case "conflicting_content_dates":
		return "Identical file bytes have different source dates; one remote asset cannot represent both dates."
	default:
		return strings.TrimSpace("The source metadata is unresolved: " + code + ".")
	}
}

func localError(code, message string, cause error) error {
	return &Error{Code: code, Message: message, cause: cause}
}
