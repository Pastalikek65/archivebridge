package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func Verify(ctx context.Context, archiveDir string) (*VerifyReport, error) {
	out, err := filepath.Abs(archiveDir)
	if err != nil {
		return nil, err
	}
	out = filepath.Clean(out)
	st, err := os.Lstat(out)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return nil, errors.New("archive root must be a real directory")
	}
	m, err := ReadManifest(out)
	if err != nil {
		return nil, err
	}
	if err := verifyOwner(out, m.PlanID); err != nil {
		return nil, err
	}
	p := &Plan{Files: m.Files, Sidecars: m.Sidecars}
	expected, err := expectedTree(p)
	if err != nil {
		return nil, err
	}
	report := &VerifyReport{Status: "ok", PlanID: m.PlanID, Issues: []Issue{}}
	if err := validateOwnedTree(out, expected); err != nil {
		report.Status = "failed"
		report.Issues = append(report.Issues, Issue{Code: "unexpected_output", Details: "Output contains an entry outside the manifest-owned archive."})
	}
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ok, n, code := verifyContent(ctx, out, f.OutputPath, f.SHA256, f.Bytes)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		report.FilesChecked++
		report.BytesChecked += n
		if !ok {
			report.Status = "failed"
			report.Issues = append(report.Issues, Issue{Code: code, SourceIndex: f.SourceIndex, EntryPath: f.EntryPath, Details: "Exported media bytes are missing or do not match the manifest."})
		}
	}
	for _, s := range m.Sidecars {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ok, n, code := verifyContent(ctx, out, s.OutputPath, s.SHA256, s.Bytes)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		report.SidecarsChecked++
		report.BytesChecked += n
		if !ok {
			report.Status = "failed"
			report.Issues = append(report.Issues, Issue{Code: code, SourceIndex: s.SourceIndex, EntryPath: s.EntryPath, Details: "Exported sidecar bytes are missing or do not match the manifest."})
		}
	}
	return report, nil
}

func verifyOwner(out, planID string) error {
	marker := filepath.Join(out, ownerFilename)
	b, err := readJSONFile(marker, 4096)
	if err != nil {
		return err
	}
	var owner ownerRecord
	if err := decodeStrict(b, &owner); err != nil {
		return errors.New("invalid output ownership marker")
	}
	if owner.SchemaVersion != SchemaVersion || owner.PlanID != planID {
		return errors.New("output ownership marker does not match manifest")
	}
	return nil
}

func verifyContent(ctx context.Context, root, rel, wantHash string, wantBytes int64) (bool, int64, string) {
	full, err := outputPath(root, rel)
	if err != nil {
		return false, 0, "unsafe_output_path"
	}
	if err := checkOutputParent(root, full); err != nil {
		return false, 0, "unsafe_output_path"
	}
	st, err := os.Lstat(full)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, "missing_file"
	}
	if err != nil {
		return false, 0, "unreadable_file"
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return false, 0, "unsafe_file"
	}
	sha, n, err := hashFile(ctx, full)
	if err != nil {
		return false, n, "unreadable_file"
	}
	if n != wantBytes || sha != wantHash {
		return false, n, "content_mismatch"
	}
	return true, n, ""
}

func checkOutputParent(root, final string) error {
	rel, err := filepath.Rel(root, filepath.Dir(final))
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("output path escaped its root")
	}
	if rel == "." {
		return nil
	}
	cur := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return errors.New("output parent is not a real directory")
		}
	}
	return nil
}
