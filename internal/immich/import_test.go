package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testAPIKey = "immich-test-key-must-never-leak"

func TestOptionsAndReportsOmitAPIKey(t *testing.T) {
	options, err := json.Marshal(Options{ServerURL: "https://immich.example.invalid", APIKey: testAPIKey})
	if err != nil {
		t.Fatal(err)
	}
	report, err := json.Marshal(Report{SchemaVersion: 1, Mode: "import", Status: "in_progress", ServerOrigin: "https://immich.example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{options, report} {
		if strings.Contains(string(encoded), testAPIKey) || strings.Contains(string(encoded), "apiKey") {
			t.Fatalf("serialized runtime credential: %s", encoded)
		}
	}
}

func TestImportBlocksUnresolvedArchiveBeforeAnyNetworkRequest(t *testing.T) {
	archive := makePortableArchive(t,
		takeoutMember{name: "Takeout/Google Photos/unknown.jpg", data: []byte("unknown-date")},
		takeoutMember{name: "Takeout/Google Photos/unknown.jpg.json", data: []byte(`{"title":"unknown.jpg"}`)},
	)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected network", http.StatusInternalServerError)
	}))
	defer server.Close()

	report, err := Import(context.Background(), archive, Options{ServerURL: server.URL, APIKey: testAPIKey})
	if !errors.Is(err, ErrUnresolved) || report == nil || report.Status != "blocked" {
		t.Fatalf("expected truthful blocked report before network, report=%+v err=%v", report, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("blocked local preflight contacted the server %d times", calls.Load())
	}
}

func TestImportRequiresAllPermissionsBeforeMutationAndSanitizesFailures(t *testing.T) {
	archive := datedArchive(t)
	var mutations atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != testAPIKey {
			t.Errorf("preflight request missing configured key header")
		}
		switch strings.TrimPrefix(r.URL.Path, "/api") {
		case "/server/version":
			writeJSON(t, w, http.StatusOK, map[string]any{"major": 3, "minor": 3, "patch": 1, "prerelease": nil})
		case "/api-keys/me":
			permissions := RequiredPermissions()
			permissions = permissions[:len(permissions)-1]
			writeJSON(t, w, http.StatusOK, map[string]any{"permissions": permissions})
		case "/users/me":
			writeJSON(t, w, http.StatusOK, map[string]any{"id": "11111111-1111-4111-8111-111111111111", "email": testAPIKey})
		case "/assets":
			mutations.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(testAPIKey))
		case "/albums":
			if r.Method != http.MethodGet {
				mutations.Add(1)
			}
			http.Error(w, testAPIKey, http.StatusUnauthorized)
		default:
			http.Error(w, testAPIKey, http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	var snapshots [][]byte
	report, err := Import(context.Background(), archive, Options{
		ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true,
		Progress: func(r Report) error {
			b, marshalErr := json.Marshal(r)
			if marshalErr != nil {
				return marshalErr
			}
			snapshots = append(snapshots, b)
			return nil
		},
	})
	if err == nil || report == nil || report.Status != "failed" {
		t.Fatalf("missing permission must fail before mutation: report=%+v err=%v", report, err)
	}
	if mutations.Load() != 0 {
		t.Fatalf("preflight made %d remote mutations before permission rejection", mutations.Load())
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("error exposed key: %v", err)
	}
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), testAPIKey) {
		t.Fatalf("report exposed key: %s", encoded)
	}
	for _, snapshot := range snapshots {
		if strings.Contains(string(snapshot), testAPIKey) {
			t.Fatalf("progress snapshot exposed key: %s", snapshot)
		}
	}
}

func TestImportDoesNotFollowAuthenticatedRedirect(t *testing.T) {
	archive := datedArchive(t)
	var leaked atomic.Int64
	steal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == testAPIKey {
			leaked.Add(1)
		}
	}))
	defer steal.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.URL.Path, "/api") == "/server/version" {
			http.Redirect(w, r, steal.URL+"/capture", http.StatusFound)
			return
		}
		http.Error(w, "redirect expected", http.StatusInternalServerError)
	}))
	defer server.Close()

	report, err := Import(context.Background(), archive, Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true})
	if err == nil || report == nil || report.Status != "failed" {
		t.Fatalf("redirect response should fail the preflight: report=%+v err=%v", report, err)
	}
	if leaked.Load() != 0 {
		t.Fatalf("API key followed a redirect to another server")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("redirect error exposed key: %v", err)
	}
}

func datedArchive(t *testing.T) string {
	t.Helper()
	return makePortableArchive(t,
		takeoutMember{name: "Takeout/Google Photos/Album/photo.jpg", data: []byte("dated-image")},
		takeoutMember{name: "Takeout/Google Photos/Album/photo.jpg.json", data: datedSidecar("photo.jpg", "1699999999")},
	)
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode mock response: %v", err)
	}
}

func TestImportDuplicateContentWithSkippedOccurrenceUsesReadyRepresentative(t *testing.T) {
	for _, readyFirst := range []bool{true, false} {
		name := "skipped occurrence sorts first"
		if readyFirst {
			name = "ready occurrence sorts first"
		}
		t.Run(name, func(t *testing.T) {
			archive, plan, media := mixedDateDuplicateArchiveInOccurrenceOrder(t, readyFirst)
			if len(plan.Files) != 2 || plan.UniqueContents != 1 || plan.SkippedCount != 1 {
				t.Fatalf("fixture does not contain one ready and one skipped occurrence for one content: %+v", plan)
			}
			var ready, skipped *PlannedFile
			for i := range plan.Files {
				if plan.Files[i].State == "ready" {
					ready = &plan.Files[i]
				} else if plan.Files[i].State == "skipped" {
					skipped = &plan.Files[i]
				}
			}
			if ready == nil || skipped == nil || (ready.OccurrenceID < skipped.OccurrenceID) != readyFirst {
				t.Fatalf("fixture occurrence ordering does not match case: ready=%+v skipped=%+v", ready, skipped)
			}

			fake := newFakeImmich()
			server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
			defer server.Close()
			opts := Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true, SkipUnresolved: true}

			imported, err := Import(context.Background(), archive, opts)
			if err != nil || imported == nil || imported.Status != "completed_with_skips" {
				t.Fatalf("Import should transfer the ready occurrence while preserving the unresolved skip: report=%+v err=%v", imported, err)
			}
			if len(imported.Contents) != 1 || imported.Contents[0].State != "uploaded" || imported.Contents[0].RemoteAssetID != fakeAssetID || !imported.Contents[0].OriginalSHA256Verified || imported.Contents[0].VerifiedOriginalBytes != int64(len(media)) {
				t.Fatalf("content inventory does not prove one verified original upload: %+v", imported.Contents)
			}
			if len(imported.Contents[0].OccurrenceIDs) != 2 || imported.Contents[0].OccurrenceIDs[0] != minString(ready.OccurrenceID, skipped.OccurrenceID) || imported.Contents[0].OccurrenceIDs[1] != maxString(ready.OccurrenceID, skipped.OccurrenceID) {
				t.Fatalf("content inventory lost an occurrence or changed stable order: %+v", imported.Contents[0])
			}
			readyResult := occurrenceByID(imported.Files, ready.OccurrenceID)
			skippedResult := occurrenceByID(imported.Files, skipped.OccurrenceID)
			if readyResult == nil || readyResult.State != "uploaded" || readyResult.Date != "2023-11-14T22:13:20Z" || readyResult.RemoteAssetID != fakeAssetID {
				t.Fatalf("known-date occurrence did not receive the verified asset: %+v", readyResult)
			}
			if skippedResult == nil || skippedResult.State != "skipped" || skippedResult.Date != "" || skippedResult.RemoteAssetID != "" {
				t.Fatalf("unknown-date occurrence was not kept skipped without an inferred date or asset identity: %+v", skippedResult)
			}
			if len(imported.Albums) != 1 || imported.Albums[0].State != "verified" || len(imported.Memberships) != 2 {
				t.Fatalf("source album inventory did not preserve both occurrence relationships: albums=%+v memberships=%+v", imported.Albums, imported.Memberships)
			}
			membershipStates := map[string]string{}
			for _, membership := range imported.Memberships {
				membershipStates[membership.OccurrenceID] = membership.State
			}
			if membershipStates[ready.OccurrenceID] != "added" || membershipStates[skipped.OccurrenceID] != "skipped" {
				t.Fatalf("album membership states do not match ready/skipped occurrences: %+v", imported.Memberships)
			}
			if len(fake.uploads) != 1 || fake.writes != 3 || fake.uploads[0].filename != filepath.Base(ready.SourceEntry) || fake.uploads[0].created != ready.Date || fake.uploads[0].modified != ready.Date || !bytes.Equal(fake.uploads[0].assetData, media) {
				t.Fatalf("expected one original upload using only the ready occurrence's metadata, uploads=%+v writes=%d", fake.uploads, fake.writes)
			}

			writesAfterImport := fake.writes
			verified, err := VerifyRemote(context.Background(), archive, opts, *imported)
			if err != nil || verified == nil || verified.Status != "verified_with_skips" || verified.Verification == nil || verified.Verification.ContentsChecked != 1 || verified.Verification.MembershipsChecked != 1 {
				t.Fatalf("VerifyRemote did not confirm selected content and the ready album relationship: report=%+v err=%v", verified, err)
			}
			if fake.writes != writesAfterImport {
				t.Fatalf("read-only verification performed a mutation: writes %d -> %d", writesAfterImport, fake.writes)
			}

			repeated, err := Import(context.Background(), archive, opts)
			if err != nil || repeated == nil || repeated.Status != "completed_with_skips" || len(repeated.Contents) != 1 || repeated.Contents[0].State != "reused" || len(repeated.Files) != 2 {
				t.Fatalf("repeat import did not reconcile the selected duplicate safely: report=%+v err=%v", repeated, err)
			}
			if occurrenceByID(repeated.Files, ready.OccurrenceID).State != "reused" || occurrenceByID(repeated.Files, skipped.OccurrenceID).State != "skipped" || fake.writes != writesAfterImport || len(fake.uploads) != 1 {
				t.Fatalf("repeat import changed skipped state or duplicated remote content: report=%+v writes=%d uploads=%d", repeated, fake.writes, len(fake.uploads))
			}
		})
	}
}

func mixedDateDuplicateArchiveInOccurrenceOrder(t *testing.T, readyFirst bool) (string, *PlanReport, []byte) {
	t.Helper()
	media := []byte("one original shared by a known-date and an unresolved occurrence")
	knownName := "Takeout/Google Photos/Shared/known.jpg"
	for attempt := 0; attempt < 100; attempt++ {
		missingName := fmt.Sprintf("Takeout/Google Photos/Shared/missing-%03d.jpg", attempt)
		archive := makePortableArchive(t,
			takeoutMember{name: knownName, data: media},
			takeoutMember{name: missingName, data: media},
			takeoutMember{name: knownName + ".json", data: datedSidecar("known.jpg", "1700000000")},
		)
		plan, err := Plan(context.Background(), archive, true)
		if err != nil {
			t.Fatalf("Plan mixed-date duplicate fixture: %v", err)
		}
		var readyID, skippedID string
		for _, file := range plan.Files {
			switch file.SourceEntry {
			case knownName:
				readyID = file.OccurrenceID
			case missingName:
				skippedID = file.OccurrenceID
			}
		}
		if readyID == "" || skippedID == "" {
			t.Fatalf("fixture did not retain both occurrences: %+v", plan.Files)
		}
		if (readyID < skippedID) == readyFirst {
			return archive, plan, media
		}
	}
	t.Fatal("could not create deterministic fixture with requested occurrence-ID order")
	return "", nil, nil
}

func minString(a, b string) string {
	if a < b {
		return a
	}
	return b
}

func maxString(a, b string) string {
	if a > b {
		return a
	}
	return b
}
