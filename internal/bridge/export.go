package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const ownerFilename = ".archivebridge-owner.json"
const manifestFilename = "manifest.json"
const stagingName = ".archivebridge-staging"
const stageOwnerFilename = ".archivebridge-stage-owner.json"
const lockFilename = ".archivebridge.lock"

type ownerRecord struct {
	SchemaVersion int    `json:"schemaVersion"`
	PlanID        string `json:"planId"`
}

type contentTarget struct {
	hash  string
	bytes int64
	out   string
	entry string
}

func Export(ctx context.Context, p *Plan, outputDir string) (*ExportReport, error) {
	if err := validatePlan(p, true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := filepath.Abs(outputDir)
	if err != nil {
		return nil, err
	}
	out = filepath.Clean(out)
	// Reject stale or substituted sources before creating an owned output directory.
	for i, src := range p.Sources {
		st, err := os.Lstat(src.Path)
		if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return nil, fmt.Errorf("source %d is not a regular non-symlink file", i)
		}
		sha, size, err := hashFile(ctx, src.Path)
		if err != nil || sha != src.SHA256 || size != src.Bytes {
			return nil, fmt.Errorf("source %d no longer matches the inspected identity", i)
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(out, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	rootInfo, err := os.Lstat(out)
	if err != nil {
		return nil, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("output root must be a real directory, not a symlink")
	}
	if err := preflightOwnedOutput(out, p.ID); err != nil {
		return nil, err
	}
	lock, err := acquireOutputLock(out)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	if err := prepareOwnedOutput(out, p.ID); err != nil {
		return nil, err
	}
	stage := filepath.Join(out, stagingName)
	if err := recoverOwnedStage(stage, p.ID); err != nil {
		return nil, err
	}
	stageCompleted := false
	defer func() {
		// A graceful context cancellation can clean its own now-inactive stage;
		// process death cannot run this cleanup and is recovered on the next
		// locked Export call.
		if !stageCompleted && ctx.Err() != nil {
			_ = finishOwnedStage(stage, p.ID)
		}
	}()
	expected, err := expectedTree(p)
	if err != nil {
		return nil, err
	}
	if err := validateOwnedTree(out, expected); err != nil {
		return nil, err
	}

	report := &ExportReport{PlanID: p.ID, OutputPath: out, Status: "complete", Issues: append([]Issue{}, p.Issues...)}
	var expandedBytes int64
	var tarExpandedBytes int64
	var entryCount int
	for si, src := range p.Sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sha, size, err := hashFile(ctx, src.Path)
		if err != nil || sha != src.SHA256 || size != src.Bytes {
			return nil, fmt.Errorf("source %d no longer matches the inspected identity", si)
		}
		targets := make(map[string]contentTarget)
		for _, f := range p.Files {
			if f.SourceIndex == si {
				if _, exists := targets[f.EntryPath]; exists {
					return nil, errors.New("multiple planned outputs refer to one archive source entry")
				}
				targets[f.EntryPath] = contentTarget{hash: f.SHA256, bytes: f.Bytes, out: f.OutputPath, entry: f.EntryPath}
			}
		}
		for _, s := range p.Sidecars {
			if s.SourceIndex == si {
				if _, exists := targets[s.EntryPath]; exists {
					return nil, errors.New("multiple planned outputs refer to one archive source entry")
				}
				targets[s.EntryPath] = contentTarget{hash: s.SHA256, bytes: s.Bytes, out: s.OutputPath, entry: s.EntryPath}
			}
		}
		seen := make(map[string]bool, len(targets))
		err = walkArchive(ctx, src.Path, src.Format, DefaultLimits(), &expandedBytes, &tarExpandedBytes, &entryCount, func(entry *archiveEntry) error {
			target, ok := targets[entry.Path]
			if !ok {
				return nil
			}
			if seen[entry.Path] {
				return errors.New("planned archive member appeared more than once")
			}
			seen[entry.Path] = true
			written, reused, n, err := exportMember(ctx, out, stage, entry.Reader, target)
			if err != nil {
				return err
			}
			if written {
				report.FilesWritten++
				report.BytesWritten += n
			}
			if reused {
				report.FilesReused++
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("cannot export source %q: %w", src.Name, err)
		}
		if len(seen) != len(targets) {
			return nil, fmt.Errorf("source %q is missing one or more planned archive members", src.Name)
		}
		sha, size, err = hashFile(ctx, src.Path)
		if err != nil || sha != src.SHA256 || size != src.Bytes {
			return nil, fmt.Errorf("source %d changed during export", si)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manifest := manifestFromPlan(p)
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(out, manifestFilename)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if existing, err := os.Lstat(manifestPath); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
			return nil, errors.New("existing manifest is not a regular file")
		}
		b, err := os.ReadFile(manifestPath)
		if err != nil {
			return nil, err
		}
		if !bytesEqual(b, manifestBytes) {
			return nil, errors.New("existing manifest does not match this plan")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		if err := writeStagedCreateOnly(stage, manifestPath, manifestBytes); err != nil {
			return nil, err
		}
	}
	if err := finishOwnedStage(stage, p.ID); err != nil {
		return nil, fmt.Errorf("exported archive but could not clean owned staging area: %w", err)
	}
	stageCompleted = true
	return report, nil
}

func manifestFromPlan(p *Plan) *Manifest {
	m := &Manifest{SchemaVersion: p.SchemaVersion, PlanID: p.ID, Sources: make([]PublicSource, len(p.Sources)), Files: append([]MediaOccurrence{}, p.Files...), Sidecars: append([]Sidecar{}, p.Sidecars...), Albums: append([]Album{}, p.Albums...), Issues: append([]Issue{}, p.Issues...), Stats: p.Stats}
	for i, s := range p.Sources {
		m.Sources[i] = PublicSource{Name: s.Name, SHA256: s.SHA256, Bytes: s.Bytes, Format: s.Format}
	}
	return m
}

func prepareOwnedOutput(out, planID string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(out, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	st, err := os.Lstat(out)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return errors.New("output root must be a real directory, not a symlink")
	}
	marker := filepath.Join(out, ownerFilename)
	if st, err := os.Lstat(marker); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return errors.New("output ownership marker is not a regular file")
		}
		b, err := readJSONFile(marker, 4096)
		if err != nil {
			return err
		}
		var owner ownerRecord
		if err := decodeStrict(b, &owner); err != nil {
			return errors.New("output ownership marker is invalid")
		}
		if owner.SchemaVersion != SchemaVersion || owner.PlanID != planID {
			return errors.New("output directory is owned by a different plan")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != lockFilename {
		return errors.New("refusing nonempty unowned output directory")
	}
	lockInfo, err := os.Lstat(filepath.Join(out, lockFilename))
	if err != nil || lockInfo.Mode()&os.ModeSymlink != 0 || !lockInfo.Mode().IsRegular() || lockInfo.Size() != 0 {
		return errors.New("unowned output contains an invalid lock file")
	}
	b, _ := json.Marshal(ownerRecord{SchemaVersion: SchemaVersion, PlanID: planID})
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(marker)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(marker)
		return closeErr
	}
	return nil
}

// preflightOwnedOutput rejects unrelated or invalid output directories before
// creating the persistent lock file. All mutable validation is repeated under
// the lock by prepareOwnedOutput and validateOwnedTree.
func preflightOwnedOutput(out, planID string) error {
	marker := filepath.Join(out, ownerFilename)
	st, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		entries, err := os.ReadDir(out)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		if len(entries) != 1 || entries[0].Name() != lockFilename {
			return errors.New("refusing nonempty unowned output directory")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return errors.New("output ownership marker is not a regular file")
	}
	b, err := readJSONFile(marker, 4096)
	if err != nil {
		return err
	}
	var owner ownerRecord
	if err := decodeStrict(b, &owner); err != nil || owner.SchemaVersion != SchemaVersion || owner.PlanID != planID {
		return errors.New("output ownership marker is invalid or belongs to another plan")
	}
	return nil
}

func recoverOwnedStage(stage, planID string) error {
	if err := os.Mkdir(stage, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("cannot create exclusive staging directory: %w", err)
	}
	info, err := os.Lstat(stage)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("staging path must be a real directory")
	}
	marker := filepath.Join(stage, stageOwnerFilename)
	markerInfo, markerErr := os.Lstat(marker)
	if errors.Is(markerErr, os.ErrNotExist) {
		entries, err := os.ReadDir(stage)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("staging directory has contents but no valid ownership marker")
		}
		if err := writeStageOwner(marker, planID); err != nil {
			return err
		}
	} else if markerErr != nil {
		return markerErr
	} else {
		if markerInfo.Mode()&os.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() {
			return errors.New("staging ownership marker is not a regular file")
		}
		b, err := readJSONFile(marker, 4096)
		if err != nil {
			return err
		}
		var owner ownerRecord
		if err := decodeStrict(b, &owner); err != nil || owner.SchemaVersion != SchemaVersion || owner.PlanID != planID {
			return errors.New("staging ownership marker does not match this plan")
		}
	}

	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	stale := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == stageOwnerFilename {
			continue
		}
		if !knownStageTempName(name) {
			return fmt.Errorf("staging directory contains an unrecognized entry %q", name)
		}
		full := filepath.Join(stage, name)
		st, err := os.Lstat(full)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return fmt.Errorf("staging entry %q is not a regular file", name)
		}
		stale = append(stale, full)
	}
	// Validate the complete directory before removing any member so an
	// unexpected sibling never causes partial cleanup of preserved state.
	for _, full := range stale {
		if err := os.Remove(full); err != nil {
			return err
		}
	}
	return nil
}

func knownStageTempName(name string) bool {
	var prefix string
	switch {
	case strings.HasPrefix(name, "member-"):
		prefix = "member-"
	case strings.HasPrefix(name, "manifest-"):
		prefix = "manifest-"
	default:
		return false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if len(suffix) == 0 || len(suffix) > 10 {
		return false
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(suffix, 10, 32)
	return err == nil && strconv.FormatUint(n, 10) == suffix
}

func writeStageOwner(filename, planID string) error {
	b, err := json.Marshal(ownerRecord{SchemaVersion: SchemaVersion, PlanID: planID})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(filename)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(filename)
		return closeErr
	}
	return nil
}

func finishOwnedStage(stage, planID string) error {
	if err := recoverOwnedStage(stage, planID); err != nil {
		return err
	}
	marker := filepath.Join(stage, stageOwnerFilename)
	if err := os.Remove(marker); err != nil {
		return err
	}
	return os.Remove(stage)
}

func validateStageForVerify(out, planID string) error {
	stage := filepath.Join(out, stagingName)
	info, err := os.Lstat(stage)
	if errors.Is(err, os.ErrNotExist) {
		return nil // Legacy and completed exports have no staging directory.
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("staging path is not a real directory")
	}
	marker := filepath.Join(stage, stageOwnerFilename)
	markerInfo, err := os.Lstat(marker)
	if err != nil || markerInfo.Mode()&os.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() {
		return errors.New("staging ownership marker is missing or unsafe")
	}
	b, err := readJSONFile(marker, 4096)
	if err != nil {
		return err
	}
	var owner ownerRecord
	if err := decodeStrict(b, &owner); err != nil || owner.SchemaVersion != SchemaVersion || owner.PlanID != planID {
		return errors.New("staging ownership marker does not match the archive")
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == stageOwnerFilename {
			continue
		}
		if !knownStageTempName(entry.Name()) {
			return errors.New("staging directory contains an unrecognized entry")
		}
		st, err := os.Lstat(filepath.Join(stage, entry.Name()))
		if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return errors.New("staging directory contains an unsafe temporary entry")
		}
		return errors.New("staging directory contains an unfinished temporary file")
	}
	return nil
}

func expectedTree(p *Plan) (map[string]bool, error) {
	expected := map[string]bool{ownerFilename: true, manifestFilename: true, lockFilename: true, stagingName: true}
	expected[filepath.Join(stagingName, stageOwnerFilename)] = true
	for _, f := range p.Files {
		if err := addOutputPath(expected, f.OutputPath); err != nil {
			return nil, err
		}
	}
	for _, s := range p.Sidecars {
		if err := addOutputPath(expected, s.OutputPath); err != nil {
			return nil, err
		}
	}
	return expected, nil
}

func addOutputPath(expected map[string]bool, rel string) error {
	if strings.ContainsAny(rel, "\\:") || path.IsAbs(rel) || path.Clean(rel) != rel {
		return errors.New("unsafe generated output path")
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return errors.New("invalid generated output path")
	}
	for i := 1; i < len(parts); i++ {
		expected[filepath.Join(parts[:i]...)] = true
	}
	expected[filepath.FromSlash(rel)] = true
	return nil
}

func validateOwnedTree(out string, expected map[string]bool) error {
	directories := make(map[string]bool, len(expected))
	for key := range expected {
		for parent := filepath.Dir(key); parent != "." && parent != string(os.PathSeparator); parent = filepath.Dir(parent) {
			directories[parent] = true
		}
	}
	return filepath.WalkDir(out, func(full string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if full == out {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("output tree contains a symbolic link")
		}
		rel, err := filepath.Rel(out, full)
		if err != nil {
			return err
		}
		rel = filepath.Clean(rel)
		if !expected[rel] {
			return fmt.Errorf("output tree contains an unowned entry %q", rel)
		}
		wantDir := directories[rel]
		if wantDir && !info.IsDir() {
			return fmt.Errorf("output path component %q is not a directory", rel)
		}
		if !wantDir && info.IsDir() {
			return fmt.Errorf("unexpected output directory %q", rel)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("output entry %q is not a regular file", rel)
		}
		return nil
	})
}

func exportMember(ctx context.Context, out, stage string, source io.Reader, target contentTarget) (bool, bool, int64, error) {
	final, err := outputPath(out, target.out)
	if err != nil {
		return false, false, 0, err
	}
	if err := ensureOutputParent(out, final); err != nil {
		return false, false, 0, err
	}
	if st, err := os.Lstat(final); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return false, false, 0, errors.New("existing content-addressed path is not a regular file")
		}
		copyHash, n, err := hashReader(ctx, source, io.Discard)
		if err != nil {
			return false, false, n, err
		}
		if copyHash != target.hash || n != target.bytes {
			return false, false, n, errors.New("source member no longer matches its planned hash")
		}
		existingHash, existingN, err := hashFile(ctx, final)
		if err != nil || existingHash != target.hash || existingN != target.bytes {
			return false, false, n, errors.New("existing content-addressed file is corrupt")
		}
		return false, true, n, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, false, 0, err
	}
	tmp, err := os.CreateTemp(stage, "member-*")
	if err != nil {
		return false, false, 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), &contextReader{ctx: ctx, r: source})
	if copyErr == nil {
		copyErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if copyErr != nil {
		return false, false, n, copyErr
	}
	if closeErr != nil {
		return false, false, n, closeErr
	}
	if n != target.bytes || hex.EncodeToString(h.Sum(nil)) != target.hash {
		return false, false, n, errors.New("source member no longer matches its planned hash")
	}
	if err := os.Link(tmpName, final); err != nil {
		if errors.Is(err, os.ErrExist) {
			existingHash, existingN, herr := hashFile(ctx, final)
			if herr == nil && existingHash == target.hash && existingN == target.bytes {
				return false, true, n, nil
			}
			return false, false, n, errors.New("existing content-addressed file is corrupt")
		}
		return false, false, n, fmt.Errorf("cannot publish staged content: %w", err)
	}
	return true, false, n, nil
}

func ensureOutputParent(root, final string) error {
	rel, err := filepath.Rel(root, filepath.Dir(final))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("output path escaped its root")
	}
	cur := root
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(cur, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			st, err = os.Lstat(cur)
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return errors.New("output path contains a non-directory component")
		}
	}
	return nil
}

func outputPath(root, rel string) (string, error) {
	if strings.ContainsAny(rel, "\\:") || path.IsAbs(rel) || path.Clean(rel) != rel {
		return "", errors.New("unsafe output path")
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	r, err := filepath.Rel(root, full)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", errors.New("output path escaped its root")
	}
	return full, nil
}

func writeStagedCreateOnly(stage, final string, b []byte) error {
	tmp, err := os.CreateTemp(stage, "manifest-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(name, final); err != nil {
		return err
	}
	return nil
}

func hashReader(ctx context.Context, r io.Reader, dst io.Writer) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(h, dst), &contextReader{ctx: ctx, r: r})
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
