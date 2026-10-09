package bridge

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type betaReviewZipMember struct {
	name string
	data []byte
}

func writeBetaReviewZIP(t *testing.T, filename string, members ...betaReviewZipMember) {
	t.Helper()
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, member := range members {
		w, err := zw.Create(member.name)
		if err != nil {
			_ = zw.Close()
			_ = f.Close()
			t.Fatal(err)
		}
		if _, err := w.Write(member.data); err != nil {
			_ = zw.Close()
			_ = f.Close()
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func betaReviewIssueExists(p *Plan, code string) bool {
	for _, issue := range p.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func TestBetaIndependentSidecarTitleContradictionDoesNotAttachWrongDate(t *testing.T) {
	const mediaA = "Google Photos/Album/A.png"
	const mediaB = "Google Photos/Album/B.png"
	const sidecarPath = mediaA + ".json"
	dateSidecar := []byte(`{"title":"B.png","photoTakenTime":{"timestamp":"1700000000"}}`)
	mediaABytes := []byte("A image bytes")
	mediaBBytes := []byte("B image bytes")

	for _, tc := range []struct {
		name  string
		addB  bool
		wantA string
		wantB string
	}{
		{name: "explicit target absent", wantA: "unmatched"},
		{name: "explicit target conflicts with adjacent basename", addB: true, wantA: "ambiguous", wantB: "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "takeout.zip")
			members := []betaReviewZipMember{
				{name: mediaA, data: mediaABytes},
				{name: sidecarPath, data: dateSidecar},
			}
			if tc.addB {
				members = append(members, betaReviewZipMember{name: mediaB, data: mediaBBytes})
			}
			writeBetaReviewZIP(t, source, members...)

			p, err := Inspect(context.Background(), []string{source}, DefaultLimits())
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			files := make(map[string]MediaOccurrence, len(p.Files))
			for _, file := range p.Files {
				files[file.EntryPath] = file
			}
			for name, wantStatus := range map[string]string{mediaA: tc.wantA, mediaB: tc.wantB} {
				if wantStatus == "" {
					continue
				}
				got := files[name]
				if got.EntryPath == "" {
					t.Fatalf("media occurrence %q is missing", name)
				}
				if got.MetadataStatus != wantStatus || got.MetadataID != "" || got.Date != "" {
					t.Errorf("contradictory sidecar attached to %q: status=%q metadataID=%q date=%q, want unresolved %q with no attachment or date", name, got.MetadataStatus, got.MetadataID, got.Date, wantStatus)
				}
			}
			if !betaReviewIssueExists(p, "unmatched_metadata") && !betaReviewIssueExists(p, "ambiguous_metadata") {
				t.Errorf("contradictory sidecar was not surfaced as an issue: %+v", p.Issues)
			}
			if len(p.Sidecars) != 1 || p.Sidecars[0].EntryPath != sidecarPath {
				t.Fatalf("original sidecar occurrence was not preserved: %+v", p.Sidecars)
			}
			wantSidecarHash := sha256.Sum256(dateSidecar)
			if p.Sidecars[0].SHA256 != hex.EncodeToString(wantSidecarHash[:]) || p.Sidecars[0].Bytes != int64(len(dateSidecar)) {
				t.Fatalf("sidecar identity does not describe original raw bytes: %+v", p.Sidecars[0])
			}

			out := filepath.Join(root, "portable")
			if _, err := Export(context.Background(), p, out); err != nil {
				t.Fatalf("Export unresolved but preserved sidecar: %v", err)
			}
			stored, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(p.Sidecars[0].OutputPath)))
			if err != nil {
				t.Fatalf("read exported raw sidecar: %v", err)
			}
			if !bytes.Equal(stored, dateSidecar) {
				t.Fatalf("export changed unresolved sidecar bytes: got %q want %q", stored, dateSidecar)
			}
		})
	}
}

func TestBetaAlbumTitleCollisionAcrossPartsKeepsSourceProvenance(t *testing.T) {
	root := t.TempDir()
	partA := filepath.Join(root, "part-a.zip")
	partB := filepath.Join(root, "part-b.zip")
	const folder = "Google Photos/Albums/Summer Trip"
	writeBetaReviewZIP(t, partA,
		betaReviewZipMember{name: folder + "/A.jpg", data: []byte("part A media")},
		betaReviewZipMember{name: folder + "/A.jpg.json", data: []byte(`{"title":"A.jpg","albumName":"Summer Trip"}`)},
	)
	writeBetaReviewZIP(t, partB,
		betaReviewZipMember{name: folder + "/B.jpg", data: []byte("part B media")},
		betaReviewZipMember{name: folder + "/B.jpg.json", data: []byte(`{"title":"B.jpg","albumName":"Summer Trip"}`)},
	)

	plan, err := Inspect(context.Background(), []string{partA, partB}, DefaultLimits())
	if err != nil {
		t.Fatalf("Inspect multipart album: %v", err)
	}
	if len(plan.Albums) != 2 || len(plan.Files) != 2 {
		t.Fatalf("same-title album provenance was merged or lost: albums=%+v files=%+v", plan.Albums, plan.Files)
	}
	if plan.Albums[0].ID == plan.Albums[1].ID {
		t.Fatal("same-title album records from different source parts share an ID")
	}
	fileByID := make(map[string]MediaOccurrence, len(plan.Files))
	for _, file := range plan.Files {
		fileByID[file.ID] = file
	}
	for _, album := range plan.Albums {
		if album.Title != "Summer Trip" || album.Folder != folder || len(album.MediaIDs) != 1 {
			t.Errorf("album did not retain source-part folder identity: %+v", album)
			continue
		}
		media, ok := fileByID[album.MediaIDs[0]]
		if !ok || (media.EntryPath != folder+"/A.jpg" && media.EntryPath != folder+"/B.jpg") || len(media.AlbumIDs) != 1 || media.AlbumIDs[0] != album.ID {
			t.Errorf("album %q crosses source-part occurrence relationships: album=%+v media=%+v", album.ID, album, media)
		}
	}

	out := filepath.Join(root, "portable")
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatalf("Export multipart album: %v", err)
	}
	manifest, err := ReadManifest(out)
	if err != nil {
		t.Fatalf("ReadManifest after multipart export: %v", err)
	}
	if manifest.PlanID != plan.ID || len(manifest.Albums) != 2 || len(manifest.Files) != 2 {
		t.Fatalf("multipart album provenance was not preserved in manifest: %+v", manifest)
	}
	verified, err := Verify(context.Background(), out)
	if err != nil || verified.Status != "ok" {
		t.Fatalf("Verify multipart archive = %+v, %v", verified, err)
	}
	compared, err := Compare(context.Background(), plan, out)
	if err != nil || compared.Status != "matched" || compared.AlbumsChecked != 2 {
		t.Fatalf("Compare multipart album = %+v, %v", compared, err)
	}
}

func TestBetaAlbumDescriptorMergesWithItsSameSourceFolder(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "one-part.zip")
	const folder = "Google Photos/Albums/Summer Trip"
	writeBetaReviewZIP(t, source,
		betaReviewZipMember{name: folder + "/photo.jpg", data: []byte("media")},
		betaReviewZipMember{name: folder + "/photo.jpg.json", data: []byte(`{"title":"photo.jpg","albumName":"Summer Trip"}`)},
	)

	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatalf("Inspect folder and descriptor: %v", err)
	}
	if len(plan.Albums) != 1 || len(plan.Files) != 1 {
		t.Fatalf("folder and exact sidecar descriptor should identify one album: albums=%+v files=%+v", plan.Albums, plan.Files)
	}
	album := plan.Albums[0]
	file := plan.Files[0]
	if album.Title != "Summer Trip" || album.Folder != folder || len(album.MediaIDs) != 1 || album.MediaIDs[0] != file.ID || len(file.AlbumIDs) != 1 || file.AlbumIDs[0] != album.ID {
		t.Fatalf("same-source album relationship was not merged consistently: album=%+v file=%+v", album, file)
	}
}

func TestBetaRejectedUnownedOutputDirectoryRemainsUnchanged(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	writeBetaReviewZIP(t, source, betaReviewZipMember{name: "photo.jpg", data: []byte("media")})
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	out := filepath.Join(root, "existing-user-directory")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(out, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("preserve this unowned directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(context.Background(), plan, out); err == nil {
		t.Fatal("Export accepted a nonempty unowned output directory")
	}
	contents, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 1 || contents[0].Name() != "keep.txt" {
		t.Fatalf("refused export modified an unowned output directory: entries=%v", contents)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "preserve this unowned directory" {
		t.Fatalf("refused export changed the existing sentinel: %q, %v", got, err)
	}
}

func TestBetaCompareRejectsSelfConsistentManifestMetadataTampering(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	writeBetaReviewZIP(t, source,
		betaReviewZipMember{name: "Google Photos/Trip/photo.jpg", data: []byte("original media")},
		betaReviewZipMember{name: "Google Photos/Trip/photo.jpg.json", data: []byte(`{"title":"photo.jpg","photoTakenTime":{"timestamp":"1700000000"}}`)},
	)
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	out := filepath.Join(root, "portable")
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatalf("Export: %v", err)
	}

	manifest, err := ReadManifest(out)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Date == "" {
		t.Fatalf("fixture did not produce dated media: %+v", manifest.Files)
	}
	manifest.Files[0].Date = "2024-01-01T00:00:00Z"
	tamperedPlan := &Plan{
		SchemaVersion: manifest.SchemaVersion,
		Files:         manifest.Files,
		Sidecars:      manifest.Sidecars,
		Albums:        manifest.Albums,
		Issues:        manifest.Issues,
		Stats:         manifest.Stats,
		Sources:       make([]Source, len(manifest.Sources)),
	}
	for i, source := range manifest.Sources {
		tamperedPlan.Sources[i] = Source{Name: source.Name, SHA256: source.SHA256, Bytes: source.Bytes, Format: source.Format}
	}
	manifest.PlanID, err = calculatePlanID(tamperedPlan)
	if err != nil {
		t.Fatalf("calculate self-consistent tampered manifest ID: %v", err)
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, manifestFilename), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	ownerBytes, err := json.Marshal(ownerRecord{SchemaVersion: SchemaVersion, PlanID: manifest.PlanID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, ownerFilename), ownerBytes, 0600); err != nil {
		t.Fatal(err)
	}

	verified, err := Verify(context.Background(), out)
	if err != nil || verified.Status != "ok" {
		t.Fatalf("tampered manifest should remain structurally and byte-integrity valid: %+v, %v", verified, err)
	}
	compared, err := Compare(context.Background(), plan, out)
	if err != nil || compared.Status != "mismatched" || len(compared.Mismatches) == 0 {
		t.Fatalf("Compare accepted self-consistent metadata tampering: %+v, %v", compared, err)
	}
	encodedReport, err := json.Marshal(compared)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encodedReport, []byte(root)) || bytes.Contains(encodedReport, []byte(source)) {
		t.Fatalf("Compare report disclosed a local source/output path: %s", encodedReport)
	}
}

func TestBetaCompareRejectsSelfConsistentPlanMetadataTampering(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	writeBetaReviewZIP(t, source,
		betaReviewZipMember{name: "Google Photos/Trip/photo.jpg", data: []byte("original media")},
		betaReviewZipMember{name: "Google Photos/Trip/photo.jpg.json", data: []byte(`{"title":"photo.jpg","photoTakenTime":{"timestamp":"1700000000"}}`)},
	)
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	out := filepath.Join(root, "portable")
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(plan.Files) != 1 || plan.Files[0].Date == "" {
		t.Fatalf("fixture did not produce dated media: %+v", plan.Files)
	}

	tamperedPlan := *plan
	tamperedPlan.Files = append([]MediaOccurrence(nil), plan.Files...)
	tamperedPlan.Files[0].Date = "2024-01-01T00:00:00Z"
	tamperedPlan.ID, err = calculatePlanID(&tamperedPlan)
	if err != nil {
		t.Fatalf("calculate self-consistent tampered plan ID: %v", err)
	}
	if err := validatePlan(&tamperedPlan, true); err != nil {
		t.Fatalf("self-consistent edited plan should pass structural validation before source comparison: %v", err)
	}

	compared, err := Compare(context.Background(), &tamperedPlan, out)
	if err != nil || compared.Status != "mismatched" || len(compared.Mismatches) == 0 {
		t.Fatalf("Compare accepted plan metadata that differs from the original source: %+v, %v", compared, err)
	}
	encodedReport, err := json.Marshal(compared)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encodedReport, []byte(root)) || bytes.Contains(encodedReport, []byte(source)) {
		t.Fatalf("Compare report disclosed a local source/output path: %s", encodedReport)
	}
}

func TestBetaRecoveryAcceptsActualCreateTempStageName(t *testing.T) {
	for _, pattern := range []string{"member-*", "manifest-*"} {
		t.Run(pattern, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "takeout.zip")
			writeBetaReviewZIP(t, source, betaReviewZipMember{name: "photo.jpg", data: []byte("media")})
			plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			out := filepath.Join(root, "portable")
			if _, err := Export(context.Background(), plan, out); err != nil {
				t.Fatalf("initial Export: %v", err)
			}
			stage := filepath.Join(out, stagingName)
			if err := os.Mkdir(stage, 0700); err != nil {
				t.Fatal(err)
			}
			ownerBytes, err := json.Marshal(ownerRecord{SchemaVersion: SchemaVersion, PlanID: plan.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stage, stageOwnerFilename), ownerBytes, 0600); err != nil {
				t.Fatal(err)
			}
			partial, err := os.CreateTemp(stage, pattern)
			if err != nil {
				t.Fatal(err)
			}
			partialName := filepath.Base(partial.Name())
			if _, err := partial.Write([]byte("partial bytes left by a terminated writer")); err != nil {
				_ = partial.Close()
				t.Fatal(err)
			}
			if err := partial.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Export(context.Background(), plan, out); err != nil {
				t.Fatalf("recovery rejected os.CreateTemp-generated stage file %q: %v", partialName, err)
			}
			if _, err := os.Lstat(stage); !os.IsNotExist(err) {
				t.Fatalf("successful recovery left staging path: %v", err)
			}
		})
	}
}

func TestBetaRecoveryRejectsDifferentPlanStageWithoutChangingIt(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	writeBetaReviewZIP(t, source, betaReviewZipMember{name: "photo.jpg", data: []byte("media")})
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	out := filepath.Join(root, "portable")
	if _, err := Export(context.Background(), plan, out); err != nil {
		t.Fatalf("initial Export: %v", err)
	}
	stage := filepath.Join(out, stagingName)
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	wrongPlanID := stableID("another plan")
	ownerBytes, err := json.Marshal(ownerRecord{SchemaVersion: SchemaVersion, PlanID: wrongPlanID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, stageOwnerFilename), ownerBytes, 0600); err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(stage, "member-1729908219")
	partialBytes := []byte("preserve this foreign staged file")
	if err := os.WriteFile(partialPath, partialBytes, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := Export(context.Background(), plan, out); err == nil {
		t.Fatal("recovery accepted a staging marker bound to another plan")
	}
	gotOwner, err := os.ReadFile(filepath.Join(stage, stageOwnerFilename))
	if err != nil || !bytes.Equal(gotOwner, ownerBytes) {
		t.Fatalf("recovery changed foreign staging ownership marker: got %q, err %v", gotOwner, err)
	}
	gotPartial, err := os.ReadFile(partialPath)
	if err != nil || !bytes.Equal(gotPartial, partialBytes) {
		t.Fatalf("recovery changed foreign staged payload: got %q, err %v", gotPartial, err)
	}
}

func TestBetaPlanCannotReuseOneArchiveEntryAsMediaAndSidecar(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	mediaBytes := []byte("original media bytes")
	writeBetaReviewZIP(t, source, betaReviewZipMember{name: "photo.jpg", data: mediaBytes})
	plan, err := Inspect(context.Background(), []string{source}, DefaultLimits())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(plan.Files) != 1 || len(plan.Sidecars) != 0 {
		t.Fatalf("unexpected fixture plan: files=%d sidecars=%d", len(plan.Files), len(plan.Sidecars))
	}
	media := plan.Files[0]
	plan.Sidecars = append(plan.Sidecars, Sidecar{
		ID:          stableID(plan.Sources[media.SourceIndex].SHA256, media.EntryPath, media.SHA256),
		SourceIndex: media.SourceIndex,
		EntryPath:   media.EntryPath,
		SHA256:      media.SHA256,
		Bytes:       media.Bytes,
		OutputPath:  "sidecars/sha256/" + media.SHA256 + ".json",
	})
	plan.Stats.SidecarCount = 1
	plan.Stats.SidecarBytes = int64(len(mediaBytes))
	plan.ID, err = calculatePlanID(plan)
	if err != nil {
		t.Fatalf("calculate crafted plan ID: %v", err)
	}
	if err := validatePlan(plan, true); err == nil {
		t.Fatal("plan validation accepted one source member as both media and sidecar")
	}

	out := filepath.Join(root, "portable")
	if _, err := Export(context.Background(), plan, out); err == nil {
		t.Fatal("Export accepted a plan that assigns one source member to two output records")
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatalf("rejected colliding plan created output state: %v", err)
	}
}
