package immich_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Pastalikek65/archivebridge/internal/bridge"
	"github.com/Pastalikek65/archivebridge/internal/immich"
)

const (
	reviewAccountID = "11111111-2222-4333-8444-555555555555"
	reviewAssetID   = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	reviewAlbumID   = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
)

func reviewArchive(t *testing.T) (string, *immich.PlanReport, []byte) {
	return reviewArchiveIn(t, "Takeout/Google Photos", []byte(`{"title":"photo.jpg","photoTakenTime":{"timestamp":"1700000000"}}`), false)
}

func reviewAlbumArchive(t *testing.T) (string, *immich.PlanReport, []byte) {
	return reviewArchiveIn(t, "Takeout/Google Photos/Synthetic Album", []byte(`{"title":"photo.jpg","photoTakenTime":{"timestamp":"1700000000"}}`), true)
}

func reviewUnresolvedAlbumArchive(t *testing.T) (string, *immich.PlanReport, []byte) {
	return reviewArchiveIn(t, "Takeout/Google Photos/Synthetic Album", []byte(`{"title":"photo.jpg"}`), true)
}

func reviewMixedDateDuplicateArchive(t *testing.T) (string, *immich.PlanReport, []byte) {
	t.Helper()
	root := t.TempDir()
	media := []byte("same original bytes with one known and one unresolved occurrence")
	dateJSON := []byte(`{"title":"known.jpg","photoTakenTime":{"timestamp":"1700000000"}}`)
	var source, archive string
	var plan *immich.PlanReport
	for attempt := 0; attempt < 100; attempt++ {
		source = filepath.Join(root, "mixed-date-duplicates.zip")
		if err := os.Remove(source); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		f, err := os.Create(source)
		if err != nil {
			t.Fatal(err)
		}
		z := zip.NewWriter(f)
		knownName := "Takeout/Google Photos/known.jpg"
		missingName := fmt.Sprintf("Takeout/Google Photos/missing-%03d.jpg", attempt)
		for _, entry := range []struct {
			name string
			data []byte
		}{{knownName, media}, {missingName, media}, {knownName + ".json", dateJSON}} {
			member, createErr := z.Create(entry.name)
			if createErr != nil {
				_ = f.Close()
				t.Fatal(createErr)
			}
			if _, writeErr := member.Write(entry.data); writeErr != nil {
				_ = f.Close()
				t.Fatal(writeErr)
			}
		}
		if err := z.Close(); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		inspection, err := bridge.Inspect(context.Background(), []string{source}, bridge.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		archive = filepath.Join(root, fmt.Sprintf("portable-%03d", attempt))
		if _, err := bridge.Export(context.Background(), inspection, archive); err != nil {
			t.Fatal(err)
		}
		plan, err = immich.Plan(context.Background(), archive, true)
		if err != nil {
			t.Fatal(err)
		}
		var knownID, unresolvedID string
		for _, file := range plan.Files {
			switch file.SourceEntry {
			case knownName:
				knownID = file.OccurrenceID
			case missingName:
				unresolvedID = file.OccurrenceID
			}
		}
		if knownID == "" || unresolvedID == "" {
			t.Fatalf("mixed-date fixture did not preserve both occurrences: %+v", plan.Files)
		}
		if unresolvedID < knownID {
			break
		}
		plan = nil
	}
	if plan == nil || len(plan.Files) != 2 || plan.UniqueContents != 1 {
		t.Fatal("could not construct a same-content fixture whose skipped occurrence sorts before the ready occurrence")
	}
	return archive, plan, media
}

func reviewArchiveIn(t *testing.T, folder string, sidecar []byte, skipUnresolved bool) (string, *immich.PlanReport, []byte) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "synthetic.zip")
	media := []byte("synthetic original bytes for resume-origin binding")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	entries := map[string][]byte{
		folder + "/photo.jpg":      media,
		folder + "/photo.jpg.json": sidecar,
	}
	for name, contents := range entries {
		member, createErr := z.Create(name)
		if createErr != nil {
			_ = f.Close()
			t.Fatal(createErr)
		}
		if _, writeErr := member.Write(contents); writeErr != nil {
			_ = f.Close()
			t.Fatal(writeErr)
		}
	}
	if err := z.Close(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	plan, err := bridge.Inspect(context.Background(), []string{source}, bridge.Limits{})
	if err != nil {
		t.Fatalf("inspect synthetic source: %v", err)
	}
	archive := filepath.Join(root, "portable")
	if _, err := bridge.Export(context.Background(), plan, archive); err != nil {
		t.Fatalf("export synthetic source: %v", err)
	}
	preview, err := immich.Plan(context.Background(), archive, skipUnresolved)
	if err != nil {
		t.Fatalf("plan synthetic archive: %v", err)
	}
	expectedAlbums := 0
	if folder == "Takeout/Google Photos/Synthetic Album" {
		expectedAlbums = 1
	}
	if len(preview.Files) != 1 || len(preview.Albums) != expectedAlbums {
		t.Fatalf("unexpected synthetic archive shape: status=%q files=%d albums=%d", preview.Status, len(preview.Files), len(preview.Albums))
	}
	return archive, preview, media
}

func reportForReviewPlan(plan *immich.PlanReport, serverOrigin string) immich.Report {
	report := immich.Report{
		SchemaVersion:      immich.ReportSchemaVersion,
		Mode:               "import",
		Status:             "interrupted",
		DateTransferPolicy: "takeout-date-authoritative-generated-xmp-v1",
		PlanID:             plan.PlanID,
		ManifestSHA256:     plan.ManifestSHA256,
		ServerOrigin:       serverOrigin,
		ServerVersion:      immich.SupportedServerVersion,
		AccountID:          reviewAccountID,
		Files:              make([]immich.OccurrenceResult, 0, len(plan.Files)),
		Sidecars:           append([]immich.PlannedSidecar{}, plan.Sidecars...),
		Contents:           []immich.ContentResult{},
		Albums:             []immich.AlbumResult{},
		Memberships:        []immich.MembershipResult{},
		Issues:             []immich.Issue{},
	}
	for _, file := range plan.Files {
		report.Files = append(report.Files, immich.OccurrenceResult{
			OccurrenceID: file.OccurrenceID, SourceEntry: file.SourceEntry,
			OutputPath: file.OutputPath, SHA256: file.SHA256, Bytes: file.Bytes,
			Date: file.Date, SourceAlbumIDs: append([]string{}, file.SourceAlbumIDs...),
			State: "reused", RemoteAssetID: reviewAssetID,
		})
	}
	for _, album := range plan.Albums {
		report.Albums = append(report.Albums, immich.AlbumResult{
			SourceAlbumID: album.SourceAlbumID, Title: album.Title, Folder: album.Folder,
			Marker:        "ArchiveBridge:sourceAlbum:v1:" + album.SourceAlbumID,
			OccurrenceIDs: append([]string{}, album.OccurrenceIDs...), State: album.State,
		})
	}
	file := plan.Files[0]
	report.Contents = append(report.Contents, immich.ContentResult{
		SHA256: file.SHA256, Bytes: file.Bytes,
		OccurrenceIDs: []string{file.OccurrenceID}, RemoteAssetID: reviewAssetID,
		State: "reused", OriginalSHA256Verified: true, VerifiedOriginalBytes: file.Bytes,
	})
	return report
}

func TestVerifyRemoteRejectsReportThatSuppressesAPlannedAlbum(t *testing.T) {
	archive, plan, media := reviewAlbumArchive(t)
	prior := reportForReviewPlan(plan, "")
	prior.Status = "completed"
	prior.Albums[0].State = "skipped"
	prior.Albums[0].RemoteAlbumID = ""
	sha1sum := sha1.Sum(media)
	date := "2023-11-14T22:13:20Z"
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/server/version":
			_, _ = w.Write([]byte(`{"major":3,"minor":3,"patch":1,"prerelease":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/api-keys/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"permissions": immich.RequiredPermissions()})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": reviewAccountID})
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": reviewAssetID, "checksum": base64.StdEncoding.EncodeToString(sha1sum[:]),
				"fileCreatedAt": date, "fileModifiedAt": date,
				"ownerId": reviewAccountID, "isTrashed": false,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID+"/original":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(media)
		case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
			_, _ = w.Write([]byte("[]"))
		default:
			http.Error(w, "unexpected synthetic request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	prior.ServerOrigin = server.URL

	result, err := immich.VerifyRemote(context.Background(), archive, immich.Options{
		ServerURL: server.URL, APIKey: "synthetic-review-key", AllowLoopbackHTTP: true,
		Timeout: 2 * time.Second,
	}, prior)
	if err == nil || !errors.Is(err, immich.ErrResumeMismatch) {
		t.Fatalf("remote verification must reject a completed report that suppresses a source album; report=%+v err=%v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range seen {
		if request == "POST /search/metadata" || request == "PUT /albums/"+reviewAssetID+"/assets" {
			t.Errorf("VerifyRemote should reject the inconsistent report before treating album membership as checked: %s", request)
		}
	}
}

func TestImportReportsEverySkippedAlbumRelationship(t *testing.T) {
	archive, plan, _ := reviewUnresolvedAlbumArchive(t)
	if plan.Files[0].State != "skipped" || plan.Albums[0].State != "skipped" {
		t.Fatalf("fixture must produce one explicitly skipped occurrence and album: file=%q album=%q", plan.Files[0].State, plan.Albums[0].State)
	}
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/server/version":
			_, _ = w.Write([]byte(`{"major":3,"minor":3,"patch":1,"prerelease":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/api-keys/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"permissions": immich.RequiredPermissions()})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": reviewAccountID})
		case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
			_, _ = w.Write([]byte("[]"))
		default:
			http.Error(w, "unexpected synthetic request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	report, err := immich.Import(context.Background(), archive, immich.Options{
		ServerURL: server.URL, APIKey: "synthetic-review-key", AllowLoopbackHTTP: true,
		SkipUnresolved: true, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("explicit skip import should complete without remote mutations: %v", err)
	}
	if report.Status != "completed_with_skips" {
		t.Fatalf("expected explicit skipped completion, got %q", report.Status)
	}
	found := false
	for _, membership := range report.Memberships {
		if membership.SourceAlbumID == plan.Albums[0].SourceAlbumID && membership.OccurrenceID == plan.Files[0].OccurrenceID {
			found = membership.State == "skipped"
		}
	}
	if !found {
		t.Fatalf("report must retain an explicit skipped state for the source album relationship; memberships=%+v", report.Memberships)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range seen {
		if len(request) >= 5 && (request[:5] == "POST " || request[:4] == "PUT ") {
			t.Errorf("skip-only import must not mutate Immich: %s", request)
		}
	}
}

func TestVerifyRemoteRejectsTamperedMembershipResults(t *testing.T) {
	archive, plan, media := reviewAlbumArchive(t)
	prior := reportForReviewPlan(plan, "")
	prior.Status = "completed"
	prior.Albums[0].State = "verified"
	prior.Albums[0].RemoteAlbumID = reviewAlbumID
	prior.Albums[0].OwnerID = reviewAccountID
	prior.Memberships = []immich.MembershipResult{{
		SourceAlbumID: plan.Albums[0].SourceAlbumID,
		OccurrenceID:  plan.Files[0].OccurrenceID,
		RemoteAlbumID: "cccccccc-dddd-4eee-8fff-000000000000",
		RemoteAssetID: "dddddddd-eeee-4fff-8000-111111111111",
		State:         "skipped",
	}}
	sha1sum := sha1.Sum(media)
	date := "2023-11-14T22:13:20Z"
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/server/version":
			_, _ = w.Write([]byte(`{"major":3,"minor":3,"patch":1,"prerelease":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/api-keys/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"permissions": immich.RequiredPermissions()})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": reviewAccountID})
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": reviewAssetID, "checksum": base64.StdEncoding.EncodeToString(sha1sum[:]),
				"fileCreatedAt": date, "fileModifiedAt": date,
				"ownerId": reviewAccountID, "isTrashed": false,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID+"/original":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(media)
		case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": reviewAlbumID, "albumName": plan.Albums[0].Title,
				"description": "ArchiveBridge:sourceAlbum:v1:" + plan.Albums[0].SourceAlbumID,
				"albumUsers":  []map[string]any{{"role": "owner", "user": map[string]any{"id": reviewAccountID}}},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/search/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{"assets": map[string]any{
				"items": []map[string]any{{"id": reviewAssetID}}, "nextCursor": nil,
			}})
		default:
			http.Error(w, "unexpected synthetic request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	prior.ServerOrigin = server.URL

	result, err := immich.VerifyRemote(context.Background(), archive, immich.Options{
		ServerURL: server.URL, APIKey: "synthetic-review-key", AllowLoopbackHTTP: true,
		Timeout: 2 * time.Second,
	}, prior)
	if err == nil || !errors.Is(err, immich.ErrResumeMismatch) {
		t.Fatalf("VerifyRemote must reject a prior report with unrelated membership result IDs/status; report=%+v err=%v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range seen {
		if len(request) >= 5 && (request[:5] == "POST " || request[:4] == "PUT ") && request != "POST /api/search/metadata" {
			t.Errorf("VerifyRemote must remain read-only: %s", request)
		}
	}
}

func TestResumeReportFromDifferentOriginIsRejectedBeforeRemoteAssetReuse(t *testing.T) {
	archive, plan, media := reviewArchive(t)
	sha1sum := sha1.Sum(media)
	date := "2023-11-14T22:13:20Z"
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/server/version":
			_, _ = w.Write([]byte(`{"major":3,"minor":3,"patch":1,"prerelease":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/api-keys/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"permissions": immich.RequiredPermissions()})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": reviewAccountID, "email": "synthetic@example.invalid"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
			_, _ = w.Write([]byte("[]"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": reviewAssetID, "checksum": base64.StdEncoding.EncodeToString(sha1sum[:]),
				"fileCreatedAt": date, "fileModifiedAt": date,
				"ownerId": reviewAccountID, "isTrashed": false,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID+"/original":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(media)
		default:
			http.Error(w, "unexpected synthetic request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	prior := reportForReviewPlan(plan, "https://different.example")
	result, err := immich.Import(context.Background(), archive, immich.Options{
		ServerURL: server.URL, APIKey: "synthetic-review-key", AllowLoopbackHTTP: true,
		Timeout: 2 * time.Second, Resume: &prior,
	})
	if err == nil || !errors.Is(err, immich.ErrResumeMismatch) {
		t.Fatalf("cross-origin resume must fail with ErrResumeMismatch before reusing a remote identity; report=%+v err=%v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range seen {
		if request == "GET /api/assets/"+reviewAssetID || request == "GET /api/assets/"+reviewAssetID+"/original" {
			t.Errorf("cross-origin resume used a prior server asset identity: %s; requests=%v", request, seen)
		}
		if len(request) >= 5 && (request[:5] == "POST " || request[:4] == "PUT ") {
			t.Errorf("cross-origin resume attempted a remote mutation: %s", request)
		}
	}
}

func TestImportDoesNotCompleteWhenPostUploadWorkerChangesSourceDate(t *testing.T) {
	archive, plan, media := reviewAlbumArchive(t)
	if len(plan.Files) != 1 || plan.Files[0].Date != "2023-11-14T22:13:20Z" {
		t.Fatalf("test fixture must have one known Takeout date, got %+v", plan.Files)
	}
	sha1sum := sha1.Sum(media)
	const workerDate = "2020-01-02T03:04:05Z"
	var mu sync.Mutex
	assetCreated, workerCompleted, albumCreated, membershipAdded := false, false, false, false
	assetInfoCalls := 0
	assetDate := ""
	assetBytes := []byte(nil)
	var albumMarker, albumName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON := func(status int, value any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/server/version":
			writeJSON(http.StatusOK, map[string]any{"major": 3, "minor": 3, "patch": 1, "prerelease": nil})
		case r.Method == http.MethodGet && r.URL.Path == "/api/api-keys/me":
			writeJSON(http.StatusOK, map[string]any{"permissions": immich.RequiredPermissions()})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/me":
			writeJSON(http.StatusOK, map[string]any{"id": reviewAccountID})
		case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
			mu.Lock()
			created, name, marker := albumCreated, albumName, albumMarker
			mu.Unlock()
			if !created {
				writeJSON(http.StatusOK, []any{})
				return
			}
			writeJSON(http.StatusOK, []any{map[string]any{
				"id": reviewAlbumID, "albumName": name, "description": marker,
				"albumUsers": []any{map[string]any{"role": "owner", "user": map[string]any{"id": reviewAccountID}}},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets/bulk-upload-check":
			var input struct {
				Assets []struct {
					ID string `json:"id"`
				} `json:"assets"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Assets) != 1 {
				http.Error(w, "invalid synthetic upload check", http.StatusBadRequest)
				return
			}
			writeJSON(http.StatusOK, map[string]any{"results": []any{map[string]any{"id": input.Assets[0].ID, "action": "accept"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets":
			if err := r.ParseMultipartForm(int64(len(media) + 4096)); err != nil {
				http.Error(w, "invalid synthetic multipart upload", http.StatusBadRequest)
				return
			}
			file, _, err := r.FormFile("assetData")
			if err != nil {
				http.Error(w, "missing synthetic media", http.StatusBadRequest)
				return
			}
			uploaded, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil || string(uploaded) != string(media) || r.FormValue("fileCreatedAt") != plan.Files[0].Date {
				http.Error(w, "synthetic upload did not preserve source input", http.StatusBadRequest)
				return
			}
			mu.Lock()
			assetCreated, assetDate, assetBytes = true, plan.Files[0].Date, append([]byte(nil), uploaded...)
			mu.Unlock()
			writeJSON(http.StatusCreated, map[string]any{"id": reviewAssetID, "status": "created"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID+"/original":
			mu.Lock()
			if !assetCreated {
				mu.Unlock()
				http.NotFound(w, r)
				return
			}
			data := append([]byte(nil), assetBytes...)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(data)
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID:
			mu.Lock()
			created := assetCreated
			assetInfoCalls++
			firstPendingRead := assetInfoCalls == 1
			date, processed := assetDate, workerCompleted
			mu.Unlock()
			if !created {
				http.NotFound(w, r)
				return
			}
			exifInfo := any(nil)
			if processed {
				exifInfo = map[string]any{"dateTimeOriginal": workerDate}
			}
			writeJSON(http.StatusOK, map[string]any{
				"id": reviewAssetID, "checksum": base64.StdEncoding.EncodeToString(sha1sum[:]),
				"fileCreatedAt": date, "fileModifiedAt": plan.Files[0].Date,
				"ownerId": reviewAccountID, "isTrashed": false,
				"hasMetadata": processed, "exifInfo": exifInfo,
			})
			if firstPendingRead {
				// The asset endpoint first reports the upload fields with metadata
				// processing pending. The worker completes immediately afterward and
				// replaces fileCreatedAt with its embedded EXIF date.
				mu.Lock()
				workerCompleted, assetDate = true, workerDate
				mu.Unlock()
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/albums":
			var input struct {
				AlbumName   string `json:"albumName"`
				Description string `json:"description"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				http.Error(w, "invalid synthetic album", http.StatusBadRequest)
				return
			}
			mu.Lock()
			albumCreated, albumName, albumMarker = true, input.AlbumName, input.Description
			mu.Unlock()
			writeJSON(http.StatusCreated, map[string]any{"id": reviewAlbumID, "albumName": input.AlbumName, "description": input.Description})
		case r.Method == http.MethodPut && r.URL.Path == "/api/albums/"+reviewAlbumID+"/assets":
			mu.Lock()
			membershipAdded = true
			mu.Unlock()
			writeJSON(http.StatusOK, []any{map[string]any{"id": reviewAssetID, "success": true}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/search/metadata":
			mu.Lock()
			member := membershipAdded
			mu.Unlock()
			items := []any{}
			if member {
				items = append(items, map[string]any{"id": reviewAssetID})
			}
			writeJSON(http.StatusOK, map[string]any{"assets": map[string]any{"items": items, "nextCursor": nil}})
		default:
			http.Error(w, "unexpected synthetic request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	sawDurableUploadID := false
	report, err := immich.Import(context.Background(), archive, immich.Options{
		ServerURL: server.URL, APIKey: "synthetic-review-key", AllowLoopbackHTTP: true, Timeout: 2 * time.Second,
		Progress: func(checkpoint immich.Report) error {
			for _, content := range checkpoint.Contents {
				if content.RemoteAssetID == reviewAssetID && content.State == "in_flight" {
					sawDurableUploadID = true
				}
			}
			return nil
		},
	})
	mu.Lock()
	defer mu.Unlock()
	if !workerCompleted || assetDate != workerDate {
		t.Fatalf("test server did not complete the post-upload metadata worker: completed=%v date=%q", workerCompleted, assetDate)
	}
	if err == nil && report != nil && report.Status == "completed" {
		t.Errorf("import must not report completion after Immich post-processing changed the known Takeout date: date=%q report=%+v", assetDate, report)
	}
	if !sawDurableUploadID {
		t.Errorf("import must checkpoint the accepted remote asset ID before waiting for post-upload metadata processing: report=%+v", report)
	}
	if albumCreated || membershipAdded {
		t.Fatalf("source albums must not be created after remote date verification becomes invalid: albumCreated=%v membershipAdded=%v report=%+v err=%v", albumCreated, membershipAdded, report, err)
	}
	if report == nil || len(report.Contents) != 1 || report.Status == "completed" || report.Contents[0].State == "uploaded" {
		t.Fatalf("post-processing date conflict must remain visible and non-complete: report=%+v err=%v", report, err)
	}
	if len(report.Files) != 1 || report.Files[0].Date != plan.Files[0].Date {
		t.Fatalf("test archive did not carry the source metadata date: %+v", report.Files[0])
	}
}

func TestImportDoesNotCompleteWhileDuplicateAssetMetadataIsPending(t *testing.T) {
	archive, plan, media := reviewAlbumArchive(t)
	sha1sum := sha1.Sum(media)
	const workerDate = "2020-01-02T03:04:05Z"
	var mu sync.Mutex
	assetDate := plan.Files[0].Date
	assetReadCount := 0
	workerCompleted := false
	albumCreated, membershipAdded := false, false
	var albumMarker, albumName string
	uploads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON := func(status int, value any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/server/version":
			writeJSON(http.StatusOK, map[string]any{"major": 3, "minor": 3, "patch": 1, "prerelease": nil})
		case r.Method == http.MethodGet && r.URL.Path == "/api/api-keys/me":
			writeJSON(http.StatusOK, map[string]any{"permissions": immich.RequiredPermissions()})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/me":
			writeJSON(http.StatusOK, map[string]any{"id": reviewAccountID})
		case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
			mu.Lock()
			created, name, marker := albumCreated, albumName, albumMarker
			mu.Unlock()
			if !created {
				writeJSON(http.StatusOK, []any{})
				return
			}
			writeJSON(http.StatusOK, []any{map[string]any{
				"id": reviewAlbumID, "albumName": name, "description": marker,
				"albumUsers": []any{map[string]any{"role": "owner", "user": map[string]any{"id": reviewAccountID}}},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets/bulk-upload-check":
			var input struct {
				Assets []struct {
					ID string `json:"id"`
				} `json:"assets"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Assets) != 1 {
				http.Error(w, "invalid synthetic duplicate check", http.StatusBadRequest)
				return
			}
			writeJSON(http.StatusOK, map[string]any{"results": []any{map[string]any{
				"id": input.Assets[0].ID, "action": "reject", "assetId": reviewAssetID, "reason": "duplicate",
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets":
			mu.Lock()
			uploads++
			mu.Unlock()
			http.Error(w, "unexpected media upload for duplicate content", http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID+"/original":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(media)
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID:
			mu.Lock()
			assetReadCount++
			readNumber, date, processed := assetReadCount, assetDate, workerCompleted
			mu.Unlock()
			writeJSON(http.StatusOK, map[string]any{
				"id": reviewAssetID, "checksum": base64.StdEncoding.EncodeToString(sha1sum[:]),
				"fileCreatedAt": date, "fileModifiedAt": plan.Files[0].Date,
				"ownerId": reviewAccountID, "isTrashed": false,
				"hasMetadata": processed, "exifInfo": nil,
			})
			if readNumber == 2 {
				// Both initial and post-original reads see a pending asset. The real
				// worker then completes just after the second response; an importer
				// must not claim reuse from those transient pre-worker fields.
				mu.Lock()
				workerCompleted, assetDate = true, workerDate
				mu.Unlock()
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/albums":
			var input struct {
				AlbumName   string `json:"albumName"`
				Description string `json:"description"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				http.Error(w, "invalid synthetic album", http.StatusBadRequest)
				return
			}
			mu.Lock()
			albumCreated, albumName, albumMarker = true, input.AlbumName, input.Description
			mu.Unlock()
			writeJSON(http.StatusCreated, map[string]any{"id": reviewAlbumID, "albumName": input.AlbumName, "description": input.Description})
		case r.Method == http.MethodPut && r.URL.Path == "/api/albums/"+reviewAlbumID+"/assets":
			mu.Lock()
			membershipAdded = true
			mu.Unlock()
			writeJSON(http.StatusOK, []any{map[string]any{"id": reviewAssetID, "success": true}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/search/metadata":
			mu.Lock()
			member := membershipAdded
			mu.Unlock()
			items := []any{}
			if member {
				items = append(items, map[string]any{"id": reviewAssetID})
			}
			writeJSON(http.StatusOK, map[string]any{"assets": map[string]any{"items": items, "nextCursor": nil}})
		default:
			http.Error(w, "unexpected synthetic request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	report, err := immich.Import(context.Background(), archive, immich.Options{
		ServerURL: server.URL, APIKey: "synthetic-review-key", AllowLoopbackHTTP: true, Timeout: 2 * time.Second,
	})
	mu.Lock()
	defer mu.Unlock()
	if !workerCompleted || assetDate != workerDate {
		t.Fatalf("test server did not complete the worker after transient duplicate reads: completed=%v date=%q reads=%d", workerCompleted, assetDate, assetReadCount)
	}
	if err == nil && report != nil && report.Status == "completed" {
		t.Errorf("import must not report completion by reusing an asset before its metadata worker settles: report=%+v", report)
	}
	if uploads != 0 {
		t.Errorf("duplicate reconciliation must not blindly upload again: uploads=%d", uploads)
	}
	if albumCreated || membershipAdded {
		t.Errorf("no albums or memberships may be written before duplicate metadata is confirmed: album=%v membership=%v report=%+v err=%v", albumCreated, membershipAdded, report, err)
	}
	if report == nil || report.Status == "completed" || len(report.Contents) != 1 {
		t.Fatalf("pending-then-conflicting duplicate metadata must remain visible and non-complete: report=%+v err=%v", report, err)
	}
}

func TestResumeDoesNotCompleteBeforeRetainedAssetMetadataIsStable(t *testing.T) {
	archive, plan, media := reviewAlbumArchive(t)
	sha1sum := sha1.Sum(media)
	const workerDate = "2020-01-02T03:04:05Z"
	var mu sync.Mutex
	assetDate := plan.Files[0].Date
	assetReadCount := 0
	workerCompleted := false
	albumCreated, membershipAdded := false, false
	var albumMarker, albumName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON := func(status int, value any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/server/version":
			writeJSON(http.StatusOK, map[string]any{"major": 3, "minor": 3, "patch": 1, "prerelease": nil})
		case r.Method == http.MethodGet && r.URL.Path == "/api/api-keys/me":
			writeJSON(http.StatusOK, map[string]any{"permissions": immich.RequiredPermissions()})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/me":
			writeJSON(http.StatusOK, map[string]any{"id": reviewAccountID})
		case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
			mu.Lock()
			created, name, marker := albumCreated, albumName, albumMarker
			mu.Unlock()
			if !created {
				writeJSON(http.StatusOK, []any{})
				return
			}
			writeJSON(http.StatusOK, []any{map[string]any{
				"id": reviewAlbumID, "albumName": name, "description": marker,
				"albumUsers": []any{map[string]any{"role": "owner", "user": map[string]any{"id": reviewAccountID}}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID:
			mu.Lock()
			assetReadCount++
			readNumber, date, processed := assetReadCount, assetDate, workerCompleted
			mu.Unlock()
			exifInfo := any(nil)
			if processed {
				exifInfo = map[string]any{"dateTimeOriginal": workerDate}
			}
			writeJSON(http.StatusOK, map[string]any{
				"id": reviewAssetID, "checksum": base64.StdEncoding.EncodeToString(sha1sum[:]),
				"fileCreatedAt": date, "fileModifiedAt": plan.Files[0].Date,
				"ownerId": reviewAccountID, "isTrashed": false,
				"hasMetadata": processed, "exifInfo": exifInfo,
			})
			if readNumber == 1 {
				// The resumed ID already exists with transient upload dates. Complete
				// the worker after that response to ensure resume waits for its witness.
				mu.Lock()
				workerCompleted, assetDate = true, workerDate
				mu.Unlock()
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID+"/original":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(media)
		case r.Method == http.MethodPost && r.URL.Path == "/api/albums":
			var input struct {
				AlbumName   string `json:"albumName"`
				Description string `json:"description"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				http.Error(w, "invalid synthetic album", http.StatusBadRequest)
				return
			}
			mu.Lock()
			albumCreated, albumName, albumMarker = true, input.AlbumName, input.Description
			mu.Unlock()
			writeJSON(http.StatusCreated, map[string]any{"id": reviewAlbumID, "albumName": input.AlbumName, "description": input.Description})
		case r.Method == http.MethodPut && r.URL.Path == "/api/albums/"+reviewAlbumID+"/assets":
			mu.Lock()
			membershipAdded = true
			mu.Unlock()
			writeJSON(http.StatusOK, []any{map[string]any{"id": reviewAssetID, "success": true}})
		default:
			http.Error(w, "unexpected synthetic request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	prior := reportForReviewPlan(plan, server.URL)
	prior.Status = "interrupted"
	prior.Contents[0].State = "in_flight"
	prior.Contents[0].RemoteAssetID = reviewAssetID
	prior.Contents[0].OriginalSHA256Verified = false
	prior.Contents[0].VerifiedOriginalBytes = 0
	prior.Files[0].State = "in_flight"
	prior.Files[0].RemoteAssetID = reviewAssetID
	prior.Albums[0].State = "pending"
	prior.Albums[0].RemoteAlbumID = ""
	prior.Albums[0].OwnerID = ""
	prior.Memberships = []immich.MembershipResult{{
		SourceAlbumID: plan.Albums[0].SourceAlbumID,
		OccurrenceID:  plan.Files[0].OccurrenceID,
		State:         "pending",
	}}

	durableIDCheckpoint := false
	result, err := immich.Import(context.Background(), archive, immich.Options{
		ServerURL: server.URL, APIKey: "synthetic-review-key", AllowLoopbackHTTP: true,
		Timeout: 2 * time.Second, Resume: &prior,
		Progress: func(checkpoint immich.Report) error {
			for _, content := range checkpoint.Contents {
				if content.RemoteAssetID == reviewAssetID && content.State == "in_flight" {
					durableIDCheckpoint = true
				}
			}
			return nil
		},
	})
	mu.Lock()
	defer mu.Unlock()
	if !durableIDCheckpoint {
		t.Fatal("resume must persist the retained remote asset ID in an in-flight checkpoint before polling metadata")
	}
	if !workerCompleted || assetDate != workerDate || assetReadCount < 2 {
		t.Fatalf("test server did not complete the metadata worker after the retained-ID read: completed=%v date=%q reads=%d", workerCompleted, assetDate, assetReadCount)
	}
	if err == nil || result == nil || result.Status == "completed" || len(result.Contents) != 1 || result.Contents[0].RemoteAssetID != reviewAssetID {
		t.Fatalf("resume must preserve the accepted remote ID but refuse completion when the worker changes its source date: report=%+v err=%v", result, err)
	}
	if result.Contents[0].State == "uploaded" || result.Contents[0].State == "reused" || result.Files[0].RemoteAssetID != reviewAssetID {
		t.Fatalf("conflicting worker result must not be marked verified or lose the retained identity: report=%+v", result)
	}
	if albumCreated || membershipAdded {
		t.Fatalf("resume must not write albums or memberships before the retained asset's processed date is verified: album=%v membership=%v report=%+v err=%v", albumCreated, membershipAdded, result, err)
	}
}

func TestExplicitSkipCanStillUploadReadyOccurrenceWithSameContent(t *testing.T) {
	archive, plan, media := reviewMixedDateDuplicateArchive(t)
	if len(plan.Files) != 2 || plan.UniqueContents != 1 || plan.SkippedCount != 1 {
		t.Fatalf("fixture must contain one ready and one explicitly skipped occurrence of identical content: %+v", plan)
	}
	var ready *immich.PlannedFile
	var skipped *immich.PlannedFile
	for i := range plan.Files {
		if plan.Files[i].State == "ready" {
			ready = &plan.Files[i]
		} else if plan.Files[i].State == "skipped" {
			skipped = &plan.Files[i]
		}
	}
	if ready == nil || skipped == nil || skipped.OccurrenceID >= ready.OccurrenceID {
		t.Fatalf("fixture must sort the skipped occurrence ahead of the ready one in the grouped content IDs: %+v", plan.Files)
	}
	sha1sum := sha1.Sum(media)
	var uploads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON := func(status int, value any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/server/version":
			writeJSON(http.StatusOK, map[string]any{"major": 3, "minor": 3, "patch": 1, "prerelease": nil})
		case r.Method == http.MethodGet && r.URL.Path == "/api/api-keys/me":
			writeJSON(http.StatusOK, map[string]any{"permissions": immich.RequiredPermissions()})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/me":
			writeJSON(http.StatusOK, map[string]any{"id": reviewAccountID})
		case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
			writeJSON(http.StatusOK, []any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets/bulk-upload-check":
			var input struct {
				Assets []struct {
					ID string `json:"id"`
				} `json:"assets"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Assets) != 1 {
				http.Error(w, "invalid synthetic upload check", http.StatusBadRequest)
				return
			}
			writeJSON(http.StatusOK, map[string]any{"results": []any{map[string]any{"id": input.Assets[0].ID, "action": "accept"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets":
			if err := r.ParseMultipartForm(int64(len(media) + 4096)); err != nil {
				http.Error(w, "invalid synthetic upload", http.StatusBadRequest)
				return
			}
			if r.FormValue("fileCreatedAt") != ready.Date || r.FormValue("fileModifiedAt") != ready.Date || r.FormValue("filename") != filepath.Base(ready.SourceEntry) {
				http.Error(w, "upload did not use ready occurrence metadata", http.StatusBadRequest)
				return
			}
			file, _, err := r.FormFile("assetData")
			if err != nil {
				http.Error(w, "missing synthetic original", http.StatusBadRequest)
				return
			}
			uploaded, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil || !bytes.Equal(uploaded, media) {
				http.Error(w, "synthetic original mismatch", http.StatusBadRequest)
				return
			}
			uploads++
			writeJSON(http.StatusCreated, map[string]any{"id": reviewAssetID, "status": "created"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID:
			date := ready.Date
			writeJSON(http.StatusOK, map[string]any{
				"id": reviewAssetID, "checksum": base64.StdEncoding.EncodeToString(sha1sum[:]),
				"fileCreatedAt": date, "fileModifiedAt": date, "ownerId": reviewAccountID,
				"isTrashed": false, "hasMetadata": true,
				"exifInfo": map[string]any{"dateTimeOriginal": date},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/"+reviewAssetID+"/original":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(media)
		default:
			http.Error(w, "unexpected synthetic request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	report, err := immich.Import(context.Background(), archive, immich.Options{
		ServerURL: server.URL, APIKey: "synthetic-review-key", AllowLoopbackHTTP: true,
		SkipUnresolved: true, Timeout: 2 * time.Second,
	})
	if err != nil || report == nil || report.Status != "completed_with_skips" {
		t.Fatalf("explicitly skipping only the unresolved occurrence must still transfer its known-date duplicate: report=%+v err=%v", report, err)
	}
	if uploads != 1 || len(report.Contents) != 1 || report.Contents[0].State != "uploaded" || !report.Contents[0].OriginalSHA256Verified {
		t.Fatalf("known-date content was not uploaded and verified exactly once: uploads=%d contents=%+v", uploads, report.Contents)
	}
	if len(report.Files) != 2 || report.Files[0].State == report.Files[1].State {
		t.Fatalf("report did not preserve distinct ready/skipped occurrence states: %+v", report.Files)
	}
}
