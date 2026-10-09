package bridge

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectExportVerifyRoundTrip(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	photoPath := "Takeout/Google Photos/Photos from 2020/Trip/IMG_1.JPG"
	photo := []byte("original-photo-bytes\x00\x01")
	metadata := []byte(`{"title":"IMG_1.JPG","photoTakenTime":{"timestamp":"1700000000"}}`)
	writeTestZIP(t, source, map[string][]byte{
		photoPath:           photo,
		photoPath + ".json": metadata,
		"Takeout/Google Photos/Photos from 2020/Trip/readme.txt": []byte("visible unsupported member"),
	})

	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || len(plan.Sidecars) != 1 {
		t.Fatalf("counts: files=%d sidecars=%d", len(plan.Files), len(plan.Sidecars))
	}
	media := plan.Files[0]
	if media.MetadataStatus != "matched" || media.MetadataID != plan.Sidecars[0].ID {
		t.Fatalf("metadata association: %#v", media)
	}
	if media.Date != "2023-11-14T22:13:20Z" {
		t.Fatalf("date = %q", media.Date)
	}
	if len(plan.Albums) != 1 || plan.Albums[0].Title != "Trip" || len(plan.Albums[0].MediaIDs) != 1 {
		t.Fatalf("albums = %#v", plan.Albums)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != "unsupported_member" {
		t.Fatalf("issues = %#v", plan.Issues)
	}

	planPath := filepath.Join(root, "plan.json")
	if err := WritePlan(plan, planPath); err != nil {
		t.Fatal(err)
	}
	if err := WritePlan(plan, planPath); err == nil {
		t.Fatal("WritePlan overwrote an existing plan")
	}
	loaded, err := ReadPlan(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != plan.ID {
		t.Fatalf("plan id changed across round trip: %q != %q", loaded.ID, plan.ID)
	}

	out := filepath.Join(root, "portable")
	first, err := Export(context.Background(), loaded, out)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "complete" || first.FilesWritten != 2 || first.FilesReused != 0 {
		t.Fatalf("first export = %#v", first)
	}
	manifest, err := ReadManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(root)) || bytes.Contains(encoded, []byte(source)) {
		t.Fatal("manifest disclosed a local source path")
	}
	mediaBytes, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(media.OutputPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mediaBytes, photo) {
		t.Fatal("export changed original media bytes")
	}
	sidecarBytes, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(plan.Sidecars[0].OutputPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sidecarBytes, metadata) {
		t.Fatal("export changed raw sidecar bytes")
	}

	second, err := Export(context.Background(), loaded, out)
	if err != nil {
		t.Fatal(err)
	}
	if second.FilesWritten != 0 || second.FilesReused != 2 {
		t.Fatalf("resume report = %#v", second)
	}
	verified, err := Verify(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Status != "ok" || verified.FilesChecked != 1 || verified.SidecarsChecked != 1 {
		t.Fatalf("verify = %#v", verified)
	}

	if err := os.WriteFile(filepath.Join(out, filepath.FromSlash(media.OutputPath)), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	verified, err = Verify(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Status != "failed" || len(verified.Issues) == 0 || verified.Issues[0].Code != "content_mismatch" {
		t.Fatalf("tamper report = %#v", verified)
	}
	if _, err := Export(context.Background(), loaded, out); err == nil {
		t.Fatal("resume overwrote corrupt content-addressed media")
	}
}

func TestPayloadLimitConsistentAcrossZIPAndTARGZ(t *testing.T) {
	root := t.TempDir()
	contents := []byte("12345678")
	limits := DefaultLimits()
	limits.MaxTotalBytes = int64(len(contents))
	limits.MaxMediaBytes = int64(len(contents))

	zipPath := filepath.Join(root, "one.zip")
	writeTestZIP(t, zipPath, map[string][]byte{"folder/a.jpg": contents})
	if _, err := Inspect(context.Background(), []string{zipPath}, limits); err != nil {
		t.Fatalf("ZIP at exact payload limit: %v", err)
	}

	tarPath := filepath.Join(root, "one.tar.gz")
	writeTestTARGZ(t, tarPath, map[string][]byte{"folder/a.jpg": contents})
	if _, err := Inspect(context.Background(), []string{tarPath}, limits); err != nil {
		t.Fatalf("TAR.GZ at exact payload limit: %v", err)
	}
}

func TestStrictMetadataAndAmbiguousCandidatesRemainUnattached(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	writeTestZIP(t, source, map[string][]byte{
		"a/one.jpg":      []byte("one"),
		"a/two.jpg":      []byte("two"),
		"a/one.jpg.json": []byte(`{"title":"two.jpg","photoTakenTime":{"timestamp":"1700000000"}}`),
		"a/two.jpg.json": []byte(`{"title":"two.jpg"} trailing`),
	})
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]MediaOccurrence{}
	for _, f := range plan.Files {
		byName[filepath.Base(f.EntryPath)] = f
	}
	if byName["one.jpg"].MetadataStatus != "ambiguous" || byName["one.jpg"].MetadataID != "" || byName["one.jpg"].Date != "" {
		t.Fatalf("ambiguous metadata attached: %#v", byName["one.jpg"])
	}
	if byName["two.jpg"].MetadataStatus != "ambiguous" || byName["two.jpg"].MetadataID != "" || byName["two.jpg"].Date != "" {
		t.Fatalf("ambiguous metadata attached: %#v", byName["two.jpg"])
	}
	foundMalformed := false
	foundAmbiguous := false
	for _, issue := range plan.Issues {
		if issue.Code == "malformed_metadata" {
			foundMalformed = true
		}
		if issue.Code == "ambiguous_metadata" {
			foundAmbiguous = true
		}
	}
	if !foundMalformed || !foundAmbiguous {
		t.Fatalf("issues did not preserve ambiguity/malformed evidence: %#v", plan.Issues)
	}
}

func writeTestZIP(t *testing.T, filename string, members map[string][]byte) {
	t.Helper()
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, contents := range members {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTestTARGZ(t *testing.T, filename string, members map[string][]byte) {
	t.Helper()
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, contents := range members {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStrictJSONNoTrailingData(t *testing.T) {
	if got := parseMetadata([]byte(`{"title":"a.jpg"} trailing`)); !got.malformed {
		t.Fatalf("trailing JSON accepted: %#v", got)
	}
}
