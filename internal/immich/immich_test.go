package immich

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pastalikek65/archivebridge/internal/bridge"
)

type takeoutMember struct {
	name string
	data []byte
}

func makePortableArchive(t *testing.T, members ...takeoutMember) string {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, member := range members {
		w, err := zw.Create(member.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(member.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	plan, err := bridge.Inspect(context.Background(), []string{source}, bridge.DefaultLimits())
	if err != nil {
		t.Fatalf("Inspect fixture: %v", err)
	}
	archive := filepath.Join(root, "portable")
	if _, err := bridge.Export(context.Background(), plan, archive); err != nil {
		t.Fatalf("Export fixture: %v", err)
	}
	return archive
}

func datedSidecar(filename, timestamp string) []byte {
	return []byte(fmt.Sprintf(`{"title":%q,"photoTakenTime":{"timestamp":%q}}`, filename, timestamp))
}

func TestPlanReportsVerifiedManifestAndPreservesSourceRelationships(t *testing.T) {
	media := []byte("original-image-bytes")
	archive := makePortableArchive(t,
		takeoutMember{name: "Takeout/Google Photos/Family/photo.jpg", data: media},
		takeoutMember{name: "Takeout/Google Photos/Family/photo.jpg.json", data: datedSidecar("photo.jpg", "1699999999")},
	)

	got, err := Plan(context.Background(), archive, false)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got.Status != "ready" || got.PlanID == "" || got.ManifestSHA256 == "" {
		t.Fatalf("Plan identity/status not populated: %+v", got)
	}
	if got.MediaOccurrences != 1 || got.UniqueContents != 1 || got.SourceAlbums != 1 || got.UnresolvedCount != 0 {
		t.Fatalf("unexpected local summary: %+v", got)
	}
	if len(got.Files) != 1 || got.Files[0].State != "ready" || got.Files[0].SHA256 != fmt.Sprintf("%x", sha256.Sum256(media)) {
		t.Fatalf("unexpected file plan: %+v", got.Files)
	}
	if len(got.Files[0].SourceAlbumIDs) != 1 || got.Files[0].Date == "" {
		t.Fatalf("source date or album relationship was lost: %+v", got.Files[0])
	}
	if len(got.Albums) != 1 || got.Albums[0].SourceAlbumID != got.Files[0].SourceAlbumIDs[0] || len(got.Albums[0].OccurrenceIDs) != 1 {
		t.Fatalf("source album identity was not retained: %+v", got.Albums)
	}
	if len(got.Sidecars) != 1 || got.Sidecars[0].Transferred || got.Sidecars[0].State != "not_transferred" {
		t.Fatalf("Takeout sidecar must remain local and visible: %+v", got.Sidecars)
	}
	if got.ManifestSHA256 != fileSHA256(t, filepath.Join(archive, "manifest.json")) {
		t.Fatalf("manifest digest does not cover the complete manifest file: %q", got.ManifestSHA256)
	}
}

func TestPlanBlocksMissingDatesUnlessExplicitlySkipped(t *testing.T) {
	archive := makePortableArchive(t,
		takeoutMember{name: "Takeout/Google Photos/Family/undated.jpg", data: []byte("undated-media")},
		takeoutMember{name: "Takeout/Google Photos/Family/undated.jpg.json", data: []byte(`{"title":"undated.jpg"}`)},
	)

	blocked, err := Plan(context.Background(), archive, false)
	if err != nil {
		t.Fatalf("Plan should return a blocked preview rather than hide unresolved metadata: %v", err)
	}
	if blocked.Status != "blocked" || blocked.UnresolvedCount != 1 || blocked.SkippedCount != 0 || len(blocked.Files) != 1 || blocked.Files[0].State != "blocked" {
		t.Fatalf("missing date was not blocked: %+v", blocked)
	}

	skipped, err := Plan(context.Background(), archive, true)
	if err != nil {
		t.Fatalf("Plan with explicit skip: %v", err)
	}
	if skipped.Status != "ready_with_skips" || skipped.UnresolvedCount != 1 || skipped.SkippedCount != 1 || skipped.Files[0].State != "skipped" {
		t.Fatalf("explicit unresolved skip is not truthful: %+v", skipped)
	}
}

func TestPlanBlocksIdenticalBytesWithConflictingDates(t *testing.T) {
	media := []byte("same-bytes-different-dates")
	archive := makePortableArchive(t,
		takeoutMember{name: "Takeout/Google Photos/One/a.jpg", data: media},
		takeoutMember{name: "Takeout/Google Photos/One/a.jpg.json", data: datedSidecar("a.jpg", "1699999999")},
		takeoutMember{name: "Takeout/Google Photos/Two/b.jpg", data: media},
		takeoutMember{name: "Takeout/Google Photos/Two/b.jpg.json", data: datedSidecar("b.jpg", "1700000000")},
	)

	got, err := Plan(context.Background(), archive, false)
	if err != nil {
		t.Fatalf("Plan should surface a preflight blocker: %v", err)
	}
	if got.Status != "blocked" || got.UnresolvedCount != 2 || got.UniqueContents != 1 {
		t.Fatalf("conflicting dates for identical content were not blocked as one content conflict: %+v", got)
	}
	for _, file := range got.Files {
		if file.State != "blocked" || file.Reason != "conflicting_content_dates" {
			t.Errorf("occurrence was not marked with the conflict: %+v", file)
		}
	}
}

func TestPlanRejectsTamperedPortableArchive(t *testing.T) {
	archive := makePortableArchive(t,
		takeoutMember{name: "Takeout/Google Photos/photo.jpg", data: []byte("original")},
		takeoutMember{name: "Takeout/Google Photos/photo.jpg.json", data: datedSidecar("photo.jpg", "1699999999")},
	)
	manifestPath := filepath.Join(archive, "manifest.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.Replace(string(manifest), `"schemaVersion": 1`, `"schemaVersion": 999`, 1))
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(context.Background(), archive, false); err == nil {
		t.Fatal("Plan accepted an unknown manifest schema instead of requiring bridge.Verify")
	}
}

func TestReportSerializationNeverContainsCredentials(t *testing.T) {
	const secret = "test-secret-do-not-serialize"
	r := Report{SchemaVersion: 1, Mode: "import", Status: "in_progress", ServerOrigin: "https://immich.example.invalid", Contents: []ContentResult{{SHA256: strings.Repeat("a", 64), Bytes: 1, State: "pending"}}}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "apiKey") {
		t.Fatalf("report exposed a credential field: %s", encoded)
	}
}

func TestPlanUsesNoCurrentTimeFallback(t *testing.T) {
	archive := makePortableArchive(t,
		takeoutMember{name: "Takeout/Google Photos/unknown.jpg", data: []byte("unknown")},
		takeoutMember{name: "Takeout/Google Photos/unknown.jpg.json", data: []byte(`{"title":"unknown.jpg"}`)},
	)
	got, err := Plan(context.Background(), archive, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0].Date != "" || got.Files[0].State != "skipped" {
		t.Fatalf("unknown date was replaced with a timestamp: %+v", got.Files)
	}
}

func fileSHA256(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
