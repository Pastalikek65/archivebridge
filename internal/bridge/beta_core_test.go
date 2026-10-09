package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBetaCompareChecksSourcePlanManifestAndExport(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	writeTestZIP(t, source, map[string][]byte{
		"Takeout/Google Photos/Photos from 2022/Trip/IMG.JPG":      []byte("photo bytes"),
		"Takeout/Google Photos/Photos from 2022/Trip/IMG.JPG.json": []byte(`{"title":"IMG.JPG","photoTakenTime":{"timestamp":"1660000000"}}`),
	})
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "export")
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatal(err)
	}
	got, err := Compare(context.Background(), plan, out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "matched" || got.PlanID != plan.ID || got.ArchivePlanID != plan.ID || got.SourcesChecked != 1 || got.MediaChecked != 1 || got.SidecarsChecked != 1 || got.AlbumsChecked != 1 || len(got.Mismatches) != 0 {
		t.Fatalf("matched compare = %#v", got)
	}
	if err := os.WriteFile(filepath.Join(out, filepath.FromSlash(plan.Files[0].OutputPath)), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = Compare(context.Background(), plan, out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "mismatched" || !hasCompareMismatch(got, "archive_verify_failed") || !hasCompareMismatch(got, "verify_content_mismatch") {
		t.Fatalf("tampered export compare = %#v", got)
	}
	for _, mismatch := range got.Mismatches {
		if mismatch.Code == "verify_content_mismatch" && mismatch.EntryPath != plan.Files[0].EntryPath {
			t.Fatalf("content mismatch lost its archive-relative entry path: %#v", mismatch)
		}
	}
}

func TestBetaCompareDetectsChangedSourceAndManifest(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	writeTestZIP(t, source, map[string][]byte{"a/photo.jpg": []byte("original")})
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "export")
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Compare(context.Background(), plan, out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "mismatched" || !hasCompareMismatch(got, "source_identity") {
		t.Fatalf("changed source compare = %#v", got)
	}
	if err := os.WriteFile(source, []byte("restore source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, manifestFilename), []byte(`{"schemaVersion":1,"planId":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = Compare(context.Background(), plan, out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "mismatched" || !hasCompareMismatch(got, "manifest_invalid") {
		t.Fatalf("tampered manifest compare = %#v", got)
	}
}

func TestBetaExportRecoversOnlyKnownOwnedStageFiles(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	writeTestZIP(t, source, map[string][]byte{"a/photo.jpg": []byte("original")})
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "export")
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(out, stagingName)
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	writeBetaStageOwner(t, stage, plan.ID)
	stale := filepath.Join(stage, "member-1729908219")
	if err := os.WriteFile(stale, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(context.Background(), out)
	if err != nil || verified.Status != "failed" {
		t.Fatalf("Verify accepted an unfinished staging file: %+v, %v", verified, err)
	}
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatalf("owned stale stage was not recovered: %v", err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful recovery left stage behind: %v", err)
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	writeBetaStageOwner(t, stage, plan.ID)
	unknown := filepath.Join(stage, "not-an-archivebridge-temp")
	if err := os.WriteFile(unknown, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(context.Background(), plan, out); err == nil {
		t.Fatal("recovery accepted an unknown staging entry")
	}
	if _, err := os.Lstat(unknown); err != nil {
		t.Fatalf("recovery modified unknown stage entry: %v", err)
	}
}

func TestBetaExportActiveLockDoesNotTouchStaging(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	writeTestZIP(t, source, map[string][]byte{"a/photo.jpg": []byte("original")})
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "export")
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(out, stagingName)
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	writeBetaStageOwner(t, stage, plan.ID)
	stale := filepath.Join(stage, "member-1729908219")
	if err := os.WriteFile(stale, []byte("do not touch"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireOutputLock(out)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := Export(context.Background(), plan, out); err == nil {
		t.Fatal("export proceeded while another writer held the lock")
	} else if !errors.Is(err, ErrOutputLocked) {
		t.Fatalf("active writer error = %v, want ErrOutputLocked", err)
	}
	b, err := os.ReadFile(stale)
	if err != nil || string(b) != "do not touch" {
		t.Fatalf("active lock path modified staging file: %q, %v", b, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("release output lock: %v", err)
	}
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatalf("export did not resume after lock release: %v", err)
	}
}

func TestBetaLockFileMustBeZeroByteSingleLinkRegularFile(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	writeTestZIP(t, source, map[string][]byte{"a/photo.jpg": []byte("original")})
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "export")
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(out, lockFilename)
	link := filepath.Join(root, "lock-hardlink")
	if err := os.Link(lockPath, link); err != nil {
		t.Skipf("filesystem does not support hard links: %v", err)
	}
	verified, err := Verify(context.Background(), out)
	if err != nil || verified.Status != "failed" {
		t.Fatalf("verify accepted a hard-linked output lock: %+v, %v", verified, err)
	}
	if _, err := Export(context.Background(), plan, out); err == nil {
		t.Fatal("export accepted a hard-linked lock file")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("not empty"), 0600); err != nil {
		t.Fatal(err)
	}
	verified, err = Verify(context.Background(), out)
	if err != nil || verified.Status != "failed" {
		t.Fatalf("verify accepted a nonempty output lock: %+v, %v", verified, err)
	}
	if _, err := Export(context.Background(), plan, out); err == nil {
		t.Fatal("export accepted a nonempty lock file")
	} else if errors.Is(err, ErrOutputLocked) {
		t.Fatalf("non-contention lock-file error misclassified as output contention: %v", err)
	}
}

func writeBetaStageOwner(t *testing.T, stage, planID string) {
	t.Helper()
	b, err := json.Marshal(ownerRecord{SchemaVersion: SchemaVersion, PlanID: planID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, stageOwnerFilename), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func hasCompareMismatch(r *CompareReport, code string) bool {
	for _, m := range r.Mismatches {
		if m.Code == code {
			return true
		}
	}
	return false
}
