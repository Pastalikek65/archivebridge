package bridge

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type testZipMember struct {
	name string
	data []byte
	mode os.FileMode
}

func writeTestZip(t *testing.T, filename string, members ...testZipMember) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, member := range members {
		h := &zip.FileHeader{Name: member.name, Method: zip.Store}
		if member.mode != 0 {
			h.SetMode(member.mode)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatalf("create ZIP member %q: %v", member.name, err)
		}
		if _, err := f.Write(member.data); err != nil {
			t.Fatalf("write ZIP member %q: %v", member.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close ZIP: %v", err)
	}
	data := b.Bytes()
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatalf("write ZIP: %v", err)
	}
	return append([]byte(nil), data...)
}

func writeTestTarGz(t *testing.T, filename string, members ...testZipMember) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, member := range members {
		mode := int64(0600)
		typeflag := byte(tar.TypeReg)
		if member.mode&os.ModeSymlink != 0 {
			typeflag = tar.TypeSymlink
		}
		h := &tar.Header{Name: member.name, Mode: mode, Size: int64(len(member.data)), Typeflag: typeflag, Linkname: string(member.data), ModTime: time.Unix(0, 0).UTC()}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("write TAR header %q: %v", member.name, err)
		}
		if typeflag == tar.TypeReg {
			if _, err := tw.Write(member.data); err != nil {
				t.Fatalf("write TAR member %q: %v", member.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close TAR: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	data := append([]byte(nil), b.Bytes()...)
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatalf("write TAR.GZ: %v", err)
	}
	return data
}

func writeTestTarGzWithTrailingData(t *testing.T, filename string, trailingBytes int) {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "empty.jpg", Mode: 0600, Size: 0, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(bytes.Repeat([]byte{0}, trailingBytes)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func inspectTestArchive(t *testing.T, filename string, limits Limits) *Plan {
	t.Helper()
	p, err := Inspect(context.Background(), []string{filename}, limits)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", filename, err)
	}
	return p
}

func issueExists(p *Plan, code string) bool {
	for _, issue := range p.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func TestIndependentRejectsUnsafeAndAmbiguousArchiveNames(t *testing.T) {
	cases := []struct {
		name    string
		members []testZipMember
	}{
		{name: "parent traversal", members: []testZipMember{{name: "../escape.jpg", data: []byte("x")}}},
		{name: "backslash traversal", members: []testZipMember{{name: `..\escape.jpg`, data: []byte("x")}}},
		{name: "duplicate member", members: []testZipMember{{name: "photo.jpg", data: []byte("a")}, {name: "photo.jpg", data: []byte("b")}}},
		{name: "case collision", members: []testZipMember{{name: "Photo.jpg", data: []byte("a")}, {name: "photo.jpg", data: []byte("b")}}},
		{name: "symbolic link member", members: []testZipMember{{name: "linked.jpg", data: []byte("target.jpg"), mode: os.ModeSymlink | 0777}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			filename := filepath.Join(root, "source.zip")
			writeTestZip(t, filename, tc.members...)
			if _, err := Inspect(context.Background(), []string{filename}, DefaultLimits()); err == nil {
				t.Fatalf("Inspect accepted %s", tc.name)
			}
		})
	}
}

func TestIndependentRejectsSourceSymlinks(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.zip")
	writeTestZip(t, source, testZipMember{name: "photo.jpg", data: []byte("photo")})
	link := filepath.Join(root, "linked.zip")
	if err := os.Symlink(source, link); err != nil {
		t.Skipf("source symlinks are unavailable on this host: %v", err)
	}
	if _, err := Inspect(context.Background(), []string{link}, DefaultLimits()); err == nil {
		t.Fatal("Inspect accepted a symbolic-link source")
	}
}

func TestIndependentRejectsZIPCRCAndTruncatedGzip(t *testing.T) {
	t.Run("ZIP CRC", func(t *testing.T) {
		root := t.TempDir()
		filename := filepath.Join(root, "source.zip")
		writeTestZip(t, filename, testZipMember{name: "photo.jpg", data: []byte("crc-protected-original")})
		f, err := os.Open(filename)
		if err != nil {
			t.Fatal(err)
		}
		st, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		zr, err := zip.NewReader(f, st.Size())
		if err != nil {
			t.Fatal(err)
		}
		offset, err := zr.File[0].DataOffset()
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		b[offset] ^= 0x40
		if err := os.WriteFile(filename, b, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(context.Background(), []string{filename}, DefaultLimits()); err == nil {
			t.Fatal("Inspect accepted a ZIP member with an invalid CRC")
		}
	})

	t.Run("truncated gzip trailer", func(t *testing.T) {
		root := t.TempDir()
		filename := filepath.Join(root, "source.tar.gz")
		data := writeTestTarGz(t, filename, testZipMember{name: "photo.jpg", data: []byte("photo")})
		if len(data) < 8 {
			t.Fatal("test gzip archive is unexpectedly short")
		}
		if err := os.WriteFile(filename, data[:len(data)-4], 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(context.Background(), []string{filename}, DefaultLimits()); err == nil {
			t.Fatal("Inspect accepted a TAR.GZ with a truncated integrity trailer")
		}
	})
}

func TestIndependentExpandedByteAndMetadataBounds(t *testing.T) {
	root := t.TempDir()

	t.Run("exact total limit is accepted", func(t *testing.T) {
		filename := filepath.Join(root, "exact.zip")
		writeTestZip(t, filename, testZipMember{name: "photo.jpg", data: []byte("12345")})
		limits := Limits{MaxJSONBytes: 8, MaxEntries: 10, MaxArchives: 2, MaxMediaBytes: 5, MaxTotalBytes: 5}
		p := inspectTestArchive(t, filename, limits)
		if p.Stats.MediaBytes != 5 {
			t.Fatalf("MediaBytes = %d, want 5", p.Stats.MediaBytes)
		}
	})

	t.Run("aggregate regular member limit includes unsupported data", func(t *testing.T) {
		filename := filepath.Join(root, "over-total.zip")
		writeTestZip(t, filename, testZipMember{name: "notes.bin", data: []byte("123456")})
		limits := Limits{MaxJSONBytes: 8, MaxEntries: 10, MaxArchives: 2, MaxMediaBytes: 5, MaxTotalBytes: 5}
		if _, err := Inspect(context.Background(), []string{filename}, limits); err == nil {
			t.Fatal("Inspect accepted unsupported payload beyond MaxTotalBytes")
		}
	})

	t.Run("media limit", func(t *testing.T) {
		filename := filepath.Join(root, "over-media.zip")
		writeTestZip(t, filename, testZipMember{name: "photo.jpg", data: []byte("123456")})
		limits := Limits{MaxJSONBytes: 8, MaxEntries: 10, MaxArchives: 2, MaxMediaBytes: 5, MaxTotalBytes: 20}
		if _, err := Inspect(context.Background(), []string{filename}, limits); err == nil {
			t.Fatal("Inspect accepted media beyond MaxMediaBytes")
		}
	})

	t.Run("limits cannot exceed built-in safety bounds", func(t *testing.T) {
		filename := filepath.Join(root, "too-large-limit.zip")
		writeTestZip(t, filename, testZipMember{name: "photo.jpg", data: []byte("photo")})
		limits := DefaultLimits()
		limits.MaxJSONBytes = 1 << 62
		if _, err := Inspect(context.Background(), []string{filename}, limits); err == nil {
			t.Fatal("Inspect accepted an extreme parser limit that could overflow allocation arithmetic")
		}
	})

	t.Run("oversized metadata is preserved without interpretation", func(t *testing.T) {
		filename := filepath.Join(root, "oversized-json.zip")
		raw := []byte(`{"title":"photo.jpg"}`)
		writeTestZip(t, filename,
			testZipMember{name: "photo.jpg", data: []byte("photo")},
			testZipMember{name: "photo.jpg.json", data: raw},
		)
		limits := Limits{MaxJSONBytes: 8, MaxEntries: 10, MaxArchives: 2, MaxMediaBytes: 100, MaxTotalBytes: 100}
		p := inspectTestArchive(t, filename, limits)
		if len(p.Sidecars) != 1 || !issueExists(p, "metadata_too_large") {
			t.Fatalf("oversized sidecar was not preserved as unresolved: sidecars=%d issues=%+v", len(p.Sidecars), p.Issues)
		}
		if p.Files[0].MetadataStatus != "malformed" || p.Files[0].MetadataID == "" || p.Files[0].Date != "" {
			t.Fatalf("oversized sidecar relationship or no-interpretation status was lost: %+v", p.Files[0])
		}
	})
}

func TestIndependentMetadataAmbiguityMalformedRawAndManifestPrivacy(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "takeout.zip")
	ambiguousRaw := []byte(`{"title":"photo.jpg","photoTakenTime":{"timestamp":"1700000000"}}`)
	secondRaw := []byte(`{"title":"photo.jpg","creationTime":{"timestamp":"1700000001"}}`)
	malformedRaw := []byte(`{"title":"broken.jpg","photoTakenTime":{"timestamp":"1700000002"}} trailing`)
	writeTestZip(t, source,
		testZipMember{name: "Google Photos/Trip/photo.jpg", data: []byte("original-photo-bytes")},
		testZipMember{name: "Google Photos/Trip/photo.jpg.json", data: ambiguousRaw},
		testZipMember{name: "Google Photos/Trip/metadata.json", data: secondRaw},
		testZipMember{name: "Google Photos/Trip/broken.jpg", data: []byte("original-broken-bytes")},
		testZipMember{name: "Google Photos/Trip/broken.jpg.json", data: malformedRaw},
		testZipMember{name: "Google Photos/Trip/notes.txt", data: []byte("unsupported member")},
	)
	p := inspectTestArchive(t, source, DefaultLimits())
	if len(p.Files) != 2 || len(p.Sidecars) != 3 {
		t.Fatalf("unexpected plan counts: files=%d sidecars=%d", len(p.Files), len(p.Sidecars))
	}
	files := map[string]MediaOccurrence{}
	for _, f := range p.Files {
		files[pathBase(f.EntryPath)] = f
	}
	if got := files["photo.jpg"]; got.MetadataStatus != "ambiguous" || got.MetadataID != "" || got.Date != "" {
		t.Fatalf("ambiguous metadata was selected: %+v", got)
	}
	if got := files["broken.jpg"]; got.MetadataStatus != "malformed" || got.MetadataID == "" || got.Date != "" {
		t.Fatalf("malformed matched sidecar was not retained without interpretation: %+v", got)
	}
	if !issueExists(p, "ambiguous_metadata") || !issueExists(p, "malformed_metadata") || !issueExists(p, "unsupported_member") {
		t.Fatalf("expected unresolved issues are missing: %+v", p.Issues)
	}

	planFile := filepath.Join(root, "review.plan.json")
	if err := WritePlan(p, planFile); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	reloaded, err := ReadPlan(planFile)
	if err != nil {
		t.Fatalf("ReadPlan: %v", err)
	}
	if reloaded.ID != p.ID {
		t.Fatalf("plan round trip changed ID: got %s want %s", reloaded.ID, p.ID)
	}

	out := filepath.Join(root, "portable")
	if _, err := Export(context.Background(), reloaded, out); err != nil {
		t.Fatalf("Export: %v", err)
	}
	for _, f := range reloaded.Files {
		var want []byte
		switch pathBase(f.EntryPath) {
		case "photo.jpg":
			want = []byte("original-photo-bytes")
		case "broken.jpg":
			want = []byte("original-broken-bytes")
		default:
			t.Fatalf("unexpected media %q", f.EntryPath)
		}
		got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(f.OutputPath)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("media %q bytes changed during export", f.EntryPath)
		}
	}
	for _, sc := range reloaded.Sidecars {
		var want []byte
		switch pathBase(sc.EntryPath) {
		case "photo.jpg.json":
			want = ambiguousRaw
		case "metadata.json":
			want = secondRaw
		case "broken.jpg.json":
			want = malformedRaw
		default:
			t.Fatalf("unexpected sidecar %q", sc.EntryPath)
		}
		got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(sc.OutputPath)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("sidecar %q bytes changed during export", sc.EntryPath)
		}
	}
	manifestPath := filepath.Join(out, manifestFilename)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{filepath.Clean(source), filepath.ToSlash(filepath.Clean(source)), filepath.Dir(source)} {
		if strings.Contains(string(manifestBytes), private) {
			t.Fatalf("portable manifest disclosed local source path %q", private)
		}
	}
	manifest, err := ReadManifest(out)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if manifest.PlanID != p.ID || len(manifest.Sidecars) != 3 || len(manifest.Files) != 2 {
		t.Fatalf("manifest lost selected scope: %+v", manifest)
	}
	verified, err := Verify(context.Background(), out)
	if err != nil || verified.Status != "ok" {
		t.Fatalf("Verify = %+v, %v", verified, err)
	}
}

func TestIndependentMatchedSidecarRetainsDateWhenItAlsoNamesAnAlbum(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "album-photo.zip")
	raw := []byte(`{"title":"photo.jpg","albumName":"Summer Trip","photoTakenTime":{"timestamp":"1700000000"}}`)
	writeTestZip(t, source,
		testZipMember{name: "photo.jpg", data: []byte("photo")},
		testZipMember{name: "photo.jpg.json", data: raw},
	)
	p := inspectTestArchive(t, source, DefaultLimits())
	if len(p.Files) != 1 || p.Files[0].MetadataStatus != "matched" || p.Files[0].MetadataID == "" || p.Files[0].Date != "2023-11-14T22:13:20Z" {
		t.Fatalf("unambiguous media/date metadata was lost when albumName was present: %+v", p.Files)
	}
}

func TestIndependentMultiArchiveOccurrencesAndFolderRelationships(t *testing.T) {
	root := t.TempDir()
	partA := filepath.Join(root, "part-a.zip")
	partB := filepath.Join(root, "part-b.zip")
	shared := []byte("same-original-bytes")
	writeTestZip(t, partA, testZipMember{name: "Google Photos/Trip/Day 1/photo.jpg", data: shared})
	writeTestZip(t, partB, testZipMember{name: "Google Photos/Trip/Day 2/photo.jpg", data: shared})

	p := inspectTestArchive(t, partA, DefaultLimits())
	combined, err := Inspect(context.Background(), []string{partB, partA}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := Inspect(context.Background(), []string{partA, partB}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if combined.ID != reversed.ID {
		t.Fatalf("source input order changed deterministic plan ID: %s != %s", combined.ID, reversed.ID)
	}
	if p.Sources[0].SHA256 != hex.EncodeToString(sha256OfFile(t, partA)) {
		t.Fatalf("source SHA-256 does not identify original archive bytes")
	}
	if len(combined.Sources) != 2 || len(combined.Files) != 2 || len(combined.Albums) != 4 {
		t.Fatalf("multi-archive scope/relationships were lost: sources=%d files=%d albums=%d", len(combined.Sources), len(combined.Files), len(combined.Albums))
	}
	if combined.Files[0].ID == combined.Files[1].ID || combined.Files[0].OutputPath != combined.Files[1].OutputPath {
		t.Fatalf("identical bytes should retain distinct occurrences but share content storage: %+v", combined.Files)
	}
	for _, f := range combined.Files {
		if len(f.AlbumIDs) != 2 {
			t.Errorf("nested album membership not preserved for %q: %v", f.EntryPath, f.AlbumIDs)
		}
	}
	for _, album := range combined.Albums {
		if len(album.MediaIDs) != 1 {
			t.Errorf("album %q should own one source occurrence, got %v", album.Folder, album.MediaIDs)
		}
	}

	out := filepath.Join(root, "portable")
	report, err := Export(context.Background(), combined, out)
	if err != nil {
		t.Fatal(err)
	}
	if report.FilesWritten != 1 || report.FilesReused != 1 {
		t.Fatalf("content deduplication report = %+v, want one write and one reuse", report)
	}
	verified, err := Verify(context.Background(), out)
	if err != nil || verified.Status != "ok" || verified.FilesChecked != 2 {
		t.Fatalf("multi-archive Verify = %+v, %v", verified, err)
	}
}

func TestIndependentRejectsIdenticalArchiveContentUnderDifferentNames(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "part-a.zip")
	second := filepath.Join(root, "part-b.zip")
	data := writeTestZip(t, first, testZipMember{name: "photo.jpg", data: []byte("same source")})
	if err := os.WriteFile(second, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), []string{first, second}, DefaultLimits()); err == nil {
		t.Fatal("Inspect accepted byte-identical source archives that collide on source-relative occurrence IDs")
	}
}

func TestIndependentPlanMutationAndUnknownSchemaAreRejected(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.zip")
	writeTestZip(t, source, testZipMember{name: "photo.jpg", data: []byte("photo")})
	p := inspectTestArchive(t, source, DefaultLimits())

	outputMutation := cloneTestPlan(t, p)
	outputMutation.Files[0].OutputPath = "../../escape.jpg"
	if _, err := Export(context.Background(), outputMutation, filepath.Join(root, "mutated-output")); err == nil {
		t.Fatal("Export accepted a mutated generated output path")
	}
	if _, err := os.Stat(filepath.Join(root, "mutated-output")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid output path created output state: stat err=%v", err)
	}

	other := filepath.Join(root, "other.zip")
	writeTestZip(t, other, testZipMember{name: "photo.jpg", data: []byte("different source bytes")})
	outOfScope := cloneTestPlan(t, p)
	outOfScope.Sources[0].Path = other
	if _, err := Export(context.Background(), outOfScope, filepath.Join(root, "out-of-scope")); err == nil {
		t.Fatal("Export accepted a source path whose bytes do not match the inspected identity")
	}

	privateIssue := cloneTestPlan(t, p)
	privateIssue.Issues = append(privateIssue.Issues, Issue{Code: "private_path", SourceIndex: 0, Details: privateIssue.Sources[0].Path})
	privateIssue.Stats.IssueCount = len(privateIssue.Issues)
	privateIssueID, err := calculatePlanID(privateIssue)
	if err != nil {
		t.Fatal(err)
	}
	privateIssue.ID = privateIssueID
	if _, err := Export(context.Background(), privateIssue, filepath.Join(root, "private-issue")); err == nil {
		t.Fatal("Export accepted an issue that would disclose the local source path in the manifest")
	}

	unknown := cloneTestPlan(t, p)
	unknown.SchemaVersion = SchemaVersion + 1
	unknownFile := filepath.Join(root, "unknown.plan.json")
	b, err := json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknownFile, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlan(unknownFile); err == nil {
		t.Fatal("ReadPlan accepted an unknown schema version")
	}

	out := filepath.Join(root, "portable")
	if _, err := Export(context.Background(), p, out); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(out, manifestFilename)
	var manifest map[string]any
	b, err = os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["schemaVersion"] = float64(SchemaVersion + 1)
	b, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(out); err == nil {
		t.Fatal("ReadManifest accepted an unknown schema version")
	}
}

func TestIndependentOutputOwnershipIdempotenceCorruptionAndSymlinks(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.zip")
	writeTestZip(t, source, testZipMember{name: "photo.jpg", data: []byte("original-photo")})
	p := inspectTestArchive(t, source, DefaultLimits())

	owned := filepath.Join(root, "owned")
	first, err := Export(context.Background(), p, owned)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Export(context.Background(), p, owned)
	if err != nil {
		t.Fatalf("same-plan re-export: %v", err)
	}
	if first.FilesWritten != 1 || second.FilesWritten != 0 || second.FilesReused != 1 {
		t.Fatalf("re-export was not idempotent: first=%+v second=%+v", first, second)
	}

	contentPath := filepath.Join(owned, filepath.FromSlash(p.Files[0].OutputPath))
	if err := os.WriteFile(contentPath, []byte("corrupted-photo"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(context.Background(), p, owned); err == nil {
		t.Fatal("same-plan re-export silently accepted/replaced a corrupted content-addressed file")
	}
	corrupted, err := os.ReadFile(contentPath)
	if err != nil || string(corrupted) != "corrupted-photo" {
		t.Fatalf("corrupt content was unexpectedly replaced: bytes=%q err=%v", corrupted, err)
	}

	unowned := filepath.Join(root, "unowned")
	if err := os.Mkdir(unowned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unowned, "keep.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(context.Background(), p, unowned); err == nil {
		t.Fatal("Export accepted a nonempty unowned output directory")
	}
	if b, err := os.ReadFile(filepath.Join(unowned, "keep.txt")); err != nil || string(b) != "keep" {
		t.Fatalf("unowned directory content changed: %q, %v", b, err)
	}

	target := filepath.Join(root, "symlink-target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "symlink-output")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("output directory symlinks are unavailable on this host: %v", err)
	}
	if _, err := Export(context.Background(), p, link); err == nil {
		t.Fatal("Export accepted a symbolic-link output root")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target was modified: entries=%v err=%v", entries, err)
	}
}

func TestIndependentCancellationResumeAndChangedSourceSHA(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.zip")
	original := writeTestZip(t, source, testZipMember{name: "photo.jpg", data: []byte("original-photo")})
	p := inspectTestArchive(t, source, DefaultLimits())
	wantSourceHash := sha256.Sum256(original)
	if p.Sources[0].SHA256 != hex.EncodeToString(wantSourceHash[:]) {
		t.Fatalf("plan source SHA-256 = %s, want original archive SHA-256 %s", p.Sources[0].SHA256, hex.EncodeToString(wantSourceHash[:]))
	}

	out := filepath.Join(root, "resumable")
	ctx := &cancelAfterErrChecks{Context: context.Background(), cancelAt: 2, done: make(chan struct{})}
	if _, err := Export(ctx, p, out); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Export error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(out, ownerFilename)); err != nil {
		t.Fatalf("canceled export did not preserve its ownership checkpoint: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, stagingName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled export left a staging directory: %v", err)
	}
	resumed, err := Export(context.Background(), p, out)
	if err != nil || resumed.FilesWritten != 1 {
		t.Fatalf("resume after cancellation = %+v, %v", resumed, err)
	}

	changed := filepath.Join(root, "changed.zip")
	writeTestZip(t, changed, testZipMember{name: "photo.jpg", data: []byte("changed-source")})
	if err := os.WriteFile(source, mustReadTestFile(t, changed), 0600); err != nil {
		t.Fatal(err)
	}
	changedOut := filepath.Join(root, "changed-output")
	if _, err := Export(context.Background(), p, changedOut); err == nil {
		t.Fatal("Export accepted an archive whose original SHA-256 changed after inspection")
	}
	if _, err := os.Stat(changedOut); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed source created output state before identity validation: %v", err)
	}
}

type cancelAfterErrChecks struct {
	context.Context
	cancelAt int
	calls    int
	done     chan struct{}
	once     sync.Once
}

func (c *cancelAfterErrChecks) Done() <-chan struct{} { return c.done }

func (c *cancelAfterErrChecks) Err() error {
	c.calls++
	if c.calls >= c.cancelAt {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}

func cloneTestPlan(t *testing.T, p *Plan) *Plan {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var clone Plan
	if err := json.Unmarshal(b, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func sha256OfFile(t *testing.T, filename string) []byte {
	t.Helper()
	b, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return h[:]
}

func mustReadTestFile(t *testing.T, filename string) []byte {
	t.Helper()
	b, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pathBase(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func TestIndependentTARLogicalPayloadLimitMatchesZIP(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "exact.tar.gz")
	writeTestTarGz(t, filename, testZipMember{name: "photo.jpg", data: []byte("12345")})
	limits := Limits{MaxJSONBytes: 8, MaxEntries: 10, MaxArchives: 2, MaxMediaBytes: 5, MaxTotalBytes: 5}
	p := inspectTestArchive(t, filename, limits)
	if p.Stats.MediaBytes != 5 {
		t.Fatalf("TAR.GZ MediaBytes = %d, want 5", p.Stats.MediaBytes)
	}
	tooLarge := filepath.Join(root, "over-total.tar.gz")
	writeTestTarGz(t, tooLarge, testZipMember{name: "photo.jpg", data: []byte("123456")})
	limits.MaxMediaBytes = 10
	if _, err := Inspect(context.Background(), []string{tooLarge}, limits); err == nil {
		t.Fatal("TAR.GZ parser accepted regular member payload beyond MaxTotalBytes")
	}
}

func TestIndependentTARFramingBudgetBoundsTrailingGzipData(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "trailing.tar.gz")
	writeTestTarGzWithTrailingData(t, filename, 17<<20)
	limits := Limits{MaxJSONBytes: 8, MaxEntries: 10, MaxArchives: 2, MaxMediaBytes: 1, MaxTotalBytes: 1}
	if _, err := Inspect(context.Background(), []string{filename}, limits); err == nil {
		t.Fatal("Inspect drained unbounded decompressed data after the TAR end marker")
	}
}

func TestIndependentZIP64PreflightBoundariesAndMalformedExtents(t *testing.T) {
	root := t.TempDir()
	basePath := filepath.Join(root, "base.zip")
	base := writeTestZip(t, basePath, testZipMember{name: "photo.jpg", data: []byte("zip64-original")})
	eocd := independentFindEOCD(t, base)
	cdSize := uint64(binary.LittleEndian.Uint32(base[eocd+12 : eocd+16]))
	cdOffset := uint64(binary.LittleEndian.Uint32(base[eocd+16 : eocd+20]))
	limits := DefaultLimits()
	validBoundaryLimits := limits
	validBoundaryLimits.MaxEntries = 1

	valid64, _ := independentWrapZIP64(t, base, 1, cdSize, cdOffset)
	validPath := filepath.Join(root, "valid64.zip")
	if err := os.WriteFile(validPath, valid64, 0600); err != nil {
		t.Fatal(err)
	}
	p := inspectTestArchive(t, validPath, validBoundaryLimits)
	if len(p.Files) != 1 || p.Files[0].Bytes != int64(len("zip64-original")) {
		t.Fatalf("valid ZIP64 boundary lost its member: %+v", p.Files)
	}

	tooMany := []struct {
		name      string
		bytes     []byte
		wantError string
	}{
		{name: "declared count exceeds global cap", wantError: "archive entry limit exceeded", bytes: func() []byte {
			b, _ := independentWrapZIP64(t, base, 100_001, cdSize, cdOffset)
			return b
		}()},
		{name: "maximum uint64 count cannot allocate", wantError: "archive entry limit exceeded", bytes: func() []byte {
			b, _ := independentWrapZIP64(t, base, ^uint64(0), cdSize, cdOffset)
			return b
		}()},
		{name: "count does not match directory", wantError: "truncated member header", bytes: func() []byte {
			b, _ := independentWrapZIP64(t, base, 2, cdSize, cdOffset)
			return b
		}()},
		{name: "directory metadata exceeds 64 MiB", wantError: "central-directory metadata exceeds", bytes: func() []byte {
			b, _ := independentWrapZIP64(t, base, 1, (64<<20)+1, cdOffset)
			return b
		}()},
		{name: "directory offset overflows file extent", bytes: func() []byte {
			b, _ := independentWrapZIP64(t, base, 1, cdSize, uint64(len(base))+1)
			return b
		}()},
	}
	for _, tc := range tooMany {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(root, strings.ReplaceAll(tc.name, " ", "-")+".zip")
			if err := os.WriteFile(filename, tc.bytes, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(context.Background(), []string{filename}, limits); err == nil {
				t.Fatalf("Inspect accepted malformed/over-limit ZIP64 fixture %q", tc.name)
			} else if tc.wantError != "" && !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Inspect error = %v, want %q", err, tc.wantError)
			}
		})
	}

	badRecordSize, recordOffset := independentWrapZIP64(t, base, 1, cdSize, cdOffset)
	binary.LittleEndian.PutUint64(badRecordSize[recordOffset+4:recordOffset+12], 43)
	badRecordPath := filepath.Join(root, "short-zip64-record.zip")
	if err := os.WriteFile(badRecordPath, badRecordSize, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), []string{badRecordPath}, limits); err == nil {
		t.Fatal("Inspect accepted a ZIP64 end record shorter than the required fixed fields")
	}

	badLocator, _ := independentWrapZIP64(t, base, 1, cdSize, cdOffset)
	locator := independentFindEOCD(t, badLocator) - zip64LocatorLength
	binary.LittleEndian.PutUint64(badLocator[locator+8:locator+16], ^uint64(0))
	badLocatorPath := filepath.Join(root, "bad-zip64-locator.zip")
	if err := os.WriteFile(badLocatorPath, badLocator, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), []string{badLocatorPath}, limits); err == nil {
		t.Fatal("Inspect accepted an out-of-bounds ZIP64 locator offset")
	}
}

func TestIndependentStandardZIPPreflightRejectsMalformedCentralOffsets(t *testing.T) {
	root := t.TempDir()
	basePath := filepath.Join(root, "base.zip")
	base := writeTestZip(t, basePath, testZipMember{name: "photo.jpg", data: []byte("photo")})
	eocd := independentFindEOCD(t, base)
	cases := []struct {
		name string
		edit func([]byte)
	}{
		{name: "offset beyond file", edit: func(b []byte) { binary.LittleEndian.PutUint32(b[eocd+16:eocd+20], uint32(len(b)+1)) }},
		{name: "size exceeds metadata budget", edit: func(b []byte) { binary.LittleEndian.PutUint32(b[eocd+12:eocd+16], uint32((64<<20)+1)) }},
		{name: "record count exceeds actual directory", edit: func(b []byte) { binary.LittleEndian.PutUint16(b[eocd+10:eocd+12], 2) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := append([]byte(nil), base...)
			tc.edit(data)
			filename := filepath.Join(root, strings.ReplaceAll(tc.name, " ", "-")+".zip")
			if err := os.WriteFile(filename, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(context.Background(), []string{filename}, DefaultLimits()); err == nil {
				t.Fatalf("Inspect accepted malformed central-directory case %q", tc.name)
			}
		})
	}
}

func TestIndependentAdjustedOffsetSelfExtractingZIPRejected(t *testing.T) {
	root := t.TempDir()
	base := writeTestZip(t, filepath.Join(root, "base.zip"), testZipMember{
		name: "photo.jpg",
		data: []byte("synthetic-payload"),
	})
	eocd := independentFindEOCD(t, base)
	oldDirectoryOffset := binary.LittleEndian.Uint32(base[eocd+16 : eocd+20])
	prefix := []byte("MZ synthetic self-extractor stub\x00")
	centralOffset := int(oldDirectoryOffset)
	if centralOffset+zipCentralHeaderLength > eocd {
		t.Fatal("test ZIP central-directory header is outside expected bounds")
	}
	localOffset := binary.LittleEndian.Uint32(base[centralOffset+42 : centralOffset+46])
	if uint64(localOffset)+uint64(len(prefix)) > uint64(^uint32(0)) || uint64(oldDirectoryOffset)+uint64(len(prefix)) > uint64(^uint32(0)) {
		t.Fatal("self-extractor test prefix overflows classic ZIP offsets")
	}
	binary.LittleEndian.PutUint32(base[centralOffset+42:centralOffset+46], localOffset+uint32(len(prefix)))
	binary.LittleEndian.PutUint32(base[eocd+16:eocd+20], oldDirectoryOffset+uint32(len(prefix)))
	data := append(prefix, base...)
	filename := filepath.Join(root, "adjusted-offset-self-extracting.zip")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), []string{filename}, DefaultLimits()); err == nil {
		t.Fatal("Inspect accepted a ZIP preamble with correctly adjusted central and local offsets")
	}
}

func TestIndependentSourceCountBoundaryAcrossSixteenParts(t *testing.T) {
	root := t.TempDir()
	paths := make([]string, 17)
	for i := range paths {
		name := "part-" + strings.Repeat("0", 2-len(strconv.Itoa(i))) + strconv.Itoa(i) + ".zip"
		paths[i] = filepath.Join(root, name)
		member := "photo-" + strconv.Itoa(i) + ".jpg"
		writeTestZip(t, paths[i], testZipMember{name: member, data: []byte{byte(i + 1)}})
	}
	p, err := Inspect(context.Background(), paths[:16], DefaultLimits())
	if err != nil {
		t.Fatalf("Inspect exactly 16 selected source parts: %v", err)
	}
	if len(p.Sources) != 16 || p.Stats.SourceCount != 16 || len(p.Files) != 16 {
		t.Fatalf("16-part scope was not retained: sources=%d stats=%+v files=%d", len(p.Sources), p.Stats, len(p.Files))
	}
	if _, err := Inspect(context.Background(), paths, DefaultLimits()); err == nil {
		t.Fatal("Inspect accepted 17 source parts beyond the configured bound")
	}
}

func independentFindEOCD(t *testing.T, data []byte) int {
	t.Helper()
	for i := len(data) - zipEOCDLength; i >= 0 && i >= len(data)-zipEOCDLength-(1<<16-1); i-- {
		if binary.LittleEndian.Uint32(data[i:i+4]) != zipEOCDSignature {
			continue
		}
		comment := int(binary.LittleEndian.Uint16(data[i+20 : i+22]))
		if i+zipEOCDLength+comment == len(data) {
			return i
		}
	}
	t.Fatal("ZIP EOCD not found")
	return 0
}

func independentWrapZIP64(t *testing.T, base []byte, records, directorySize, directoryOffset uint64) ([]byte, int) {
	t.Helper()
	eocd := independentFindEOCD(t, base)
	if binary.LittleEndian.Uint16(base[eocd+20:eocd+22]) != 0 {
		t.Fatal("test ZIP unexpectedly has a comment")
	}
	record := make([]byte, zip64EOCDLength)
	binary.LittleEndian.PutUint32(record[0:4], zip64EOCDSignature)
	binary.LittleEndian.PutUint64(record[4:12], 44)
	binary.LittleEndian.PutUint16(record[12:14], 45)
	binary.LittleEndian.PutUint16(record[14:16], 45)
	binary.LittleEndian.PutUint64(record[24:32], records)
	binary.LittleEndian.PutUint64(record[32:40], records)
	binary.LittleEndian.PutUint64(record[40:48], directorySize)
	binary.LittleEndian.PutUint64(record[48:56], directoryOffset)

	zip64Offset := len(base[:eocd])
	locator := make([]byte, zip64LocatorLength)
	binary.LittleEndian.PutUint32(locator[0:4], zip64LocatorSignature)
	binary.LittleEndian.PutUint64(locator[8:16], uint64(zip64Offset))
	binary.LittleEndian.PutUint32(locator[16:20], 1)
	data := make([]byte, 0, len(base)+len(record)+len(locator))
	data = append(data, base[:eocd]...)
	data = append(data, record...)
	data = append(data, locator...)
	data = append(data, base[eocd:]...)
	classic := zip64Offset + zip64EOCDLength + zip64LocatorLength
	binary.LittleEndian.PutUint16(data[classic+8:classic+10], 0xffff)
	binary.LittleEndian.PutUint16(data[classic+10:classic+12], 0xffff)
	binary.LittleEndian.PutUint32(data[classic+12:classic+16], 0xffffffff)
	binary.LittleEndian.PutUint32(data[classic+16:classic+20], 0xffffffff)
	return data, zip64Offset
}
